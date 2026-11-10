//go:build request_type_ftp

package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The ftp request type reads one file from an FTP server, as sshfile reads
// one over SFTP and localfile one from the exporter's own disk, for the
// systems that still drop their files there.
//
//	request:
//	  type: ftp
//	  root: /exports                 # the directory the collector may read under
//	  path: "{{param_site}}/meters.csv"
//	  max_age: 15m                   # optional: refuse a file older than this
//	  username: exporter             # optional: anonymous when left out
//	  password_file: /etc/exporter/ftp-pass
//	  tls: {ca_file: /etc/exporter/ftp-ca.pem}
//
// A scrape logs in, asks for binary transfer, the file's size and its
// modification time, opens a passive data connection, retrieves the file
// with RETR and quits: one control connection and one data connection,
// both the scrape's own. The file is handed to the decoder its extension
// names, as a local file is.
//
// The type only reads: no command that stores, renames or deletes is in
// the code. The data connection is always passive, made by the exporter,
// and always to the server the control connection reached, whatever
// address the server's answer names.
//
// The target is host, host:port or ftp://host:port (port 21), in plaintext;
// ftpes://host:port for explicit TLS, begun with AUTH TLS, which a target
// without a scheme has too when the collector has a tls block; and
// ftps://host:port (port 990) for TLS from the first byte.
//
// The type imports nothing outside the standard library.

const (
	// ftpTimeout bounds a scrape with no deadline of its own.
	ftpTimeout = 30 * time.Second
	// ftpReplyLimit is the most one reply may be.
	ftpReplyLimit = 64 << 10
	ftpNoFile     = "no file to read: set request.path, or give the probe a path parameter naming a file under request.root"
)

// ftpPorts are the schemes of an ftp target and their default ports.
var ftpPorts = map[string]int{"": 21, "ftp": 21, "ftpes": 21, "ftps": 990}

func init() {
	registerRequestType(&RequestType{
		Name:           RequestTypeFTP,
		Fields:         []string{"root", "path", "max_age", "max_response_bytes", "username", "password_file", "tls", "allowed_targets", "denied_targets"},
		Overrides:      []string{"path", "timeout", PathParamPrefix},
		TargetFields:   []string{"path", "timeout", "username", "password_file", "tls"},
		Requester:      ftpRequester{},
		FileExtensions: true,
		Stage:          "ftp",
		TargetHint: func(*model.Collector) (string, string) {
			return "Server", "host, host:port, ftp://host:21 or ftpes://host:21"
		},
		AnswerHint: "Paste the file's content",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, fmt.Sprintf("its request.path (%q) or its label values", c.Request.Path))
		},
		SchemaRule:         func() map[string]any { return map[string]any{"required": []any{"root"}} },
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error { return checkNetTargetAccount(t) },
		CheckPath:          checkFTPPath,
		CheckFiles:         accountFileWarnings,
	})
}

// ftpRequester is the ftp request type's Requester.
type ftpRequester struct{}

// Validate checks an ftp collector's request.
func (ftpRequester) Validate(x *model.Collector) error {
	if err := checkNetAccount(x); err != nil {
		return err
	}
	if err := validateFileRequest(x, RequestTypeFTP, false); err != nil {
		return err
	}
	if err := checkFTPPath("request.root", x.Request.Root); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkFTPPath("request.path", x.Request.Path); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if x.Request.PasswordFile != "" && x.Request.Username == "" {
		return fmt.Errorf("collector %q sets request.password_file without request.username; without an account the scrape logs in as anonymous", x.Name)
	}
	return nil
}

// checkFTPPath refuses a path a command cannot carry: a line break would
// end the command and begin another.
func checkFTPPath(what, name string) error {
	if strings.ContainsAny(name, "\r\n\x00") {
		return fmt.Errorf("%s holds a line break or a NUL, which a path sent to an FTP server may not", what)
	}
	return nil
}

// ftpTarget reads a target and how its connections are secured: "" in
// plaintext, "explicit" with AUTH TLS, "implicit" from the first byte.
func ftpTarget(c *model.Collector, target string) (netAddress, string, error) {
	a, err := parseNetTarget("ftp", target, ftpPorts, false)
	if err != nil {
		return a, "", err
	}
	switch {
	case a.scheme == "ftps":
		return a, "implicit", nil
	case a.scheme == "ftpes" || a.scheme == "" && c.Request.TLS != model.TLSConfig{}:
		return a, "explicit", nil
	}
	return a, "", nil
}

// CheckTarget checks an ftp collector's target.
func (ftpRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, _, err := ftpTarget(c, target)
	return err
}

// Label is the url label of an ftp request: the file's URL.
func (ftpRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	a, secured, err := ftpTarget(withTargetAccount(c, overrides), target)
	if err != nil {
		return "", err
	}
	file, err := fileUnderRoot("", c, overrides, false, ftpNoFile)
	if err != nil {
		return "", err
	}
	return pathURL(ftpScheme(secured), a.hostPort, path.Join(c.Request.Root, file)), nil
}

func ftpScheme(secured string) string {
	return map[string]string{"": "ftp", "explicit": "ftpes", "implicit": "ftps"}[secured]
}

// Method is the http_method label of an ftp request.
func (ftpRequester) Method(*model.Collector, RequestOverrides) string { return "RETR" }

// Display renders an ftp target for logs and labels.
func (ftpRequester) Display(target string) string {
	return netDisplay("ftp", target, ftpPorts, false)
}

// ftpReply is a server's answer to one command.
type ftpReply struct {
	code int
	// text is the reply's lines, without the code of the first and the
	// last, joined.
	text string
}

// ftpSession is one control connection.
type ftpSession struct {
	ctx   context.Context
	conn  *netConn
	label string
}

// read reads one reply: a line begun by three digits and a blank, or the
// lines from one begun by three digits and a - to one begun by the same
// digits and a blank (RFC 959, 4.2).
func (s *ftpSession) read() (ftpReply, error) {
	var reply ftpReply
	var lines []string
	size := 0
	for {
		line, err := s.conn.r.ReadString('\n')
		if size += len(line); size > ftpReplyLimit {
			return reply, fmt.Errorf("a reply is longer than %d bytes, which no FTP reply is", ftpReplyLimit)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the server closed the connection")
			}
			return reply, netFailure(s.ctx, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if reply.code == 0 {
			code, err := strconv.Atoi(line[:min(3, len(line))])
			if err != nil || code < 100 || code > 599 || len(line) > 3 && line[3] != ' ' && line[3] != '-' {
				return reply, fmt.Errorf("the server answered %q, which is no FTP reply; is the target an FTP server?", netShown(line))
			}
			reply.code = code
			lines = append(lines, strings.TrimSpace(line[min(4, len(line)):]))
			if len(line) < 4 || line[3] == ' ' {
				break
			}
			continue
		}
		if strings.HasPrefix(line, strconv.Itoa(reply.code)+" ") {
			lines = append(lines, strings.TrimSpace(line[4:]))
			break
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	reply.text = netShown(strings.Join(lines, " "))
	return reply, nil
}

// command sends one line and reads its reply. shown is the line as a debug
// report shows it, the line itself unless it holds the password.
func (s *ftpSession) command(line, shown string) (ftpReply, error) {
	if shown == "" {
		shown = line
	}
	verb, _, _ := strings.Cut(shown, " ")
	traceRequest(s.ctx, verb, s.label, http.Header{"Ftp-Command": {shown}}, "", false)
	if _, err := io.WriteString(s.conn, line+"\r\n"); err != nil {
		err = netFailure(s.ctx, fmt.Errorf("%s: %w", shown, err))
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return ftpReply{}, err
	}
	reply, err := s.read()
	if err != nil {
		err = fmt.Errorf("%s: %w", shown, err)
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return reply, err
	}
	traceOutcome(s.ctx, strconv.Itoa(reply.code)+" "+reply.text)
	return reply, nil
}

// must is command for a step the scrape cannot go on without: a reply
// whose code is not one of want fails it.
func (s *ftpSession) must(line, shown string, want ...int) (ftpReply, error) {
	reply, err := s.command(line, shown)
	if err != nil {
		return reply, err
	}
	for _, code := range want {
		if reply.code == code {
			return reply, nil
		}
	}
	if shown == "" {
		shown = line
	}
	return reply, model.Errorf("the server answered %s with %d %s", shown, reply.code, reply.text)
}

// Fetch reads a scrape's file from the server.
func (ftpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	address, secured, err := ftpTarget(c, target)
	if err != nil {
		return nil, err
	}
	file, err := fileUnderRoot("", c, overrides, true, ftpNoFile)
	if err != nil {
		return nil, err
	}
	full := path.Join(c.Request.Root, file)
	if err := checkFTPPath("the file's path", full); err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, ftpTimeout)
	defer cancel()
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	cfg, err := netTLSConfig(c, secured != "")
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		// The data connection resumes the control connection's session,
		// which servers ask for so that no one else can take the transfer
		// (vsftpd's require_ssl_reuse, ProFTPD's default).
		cfg = cfg.Clone()
		cfg.ClientSessionCache = tls.NewLRUClientSessionCache(1)
		if cfg.ServerName == "" {
			cfg.ServerName = address.host
		}
	}
	if ctx, err = netGuard(ctx, c, "ftp", address.hostPort); err != nil {
		return nil, err
	}
	label := pathURL(ftpScheme(secured), address.hostPort, full)
	traceRequest(ctx, "CONNECT", label, nil, "", false)
	fail := func(err error) (*HTTPResponse, error) {
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	var first *tls.Config
	if secured == "implicit" {
		first = cfg
	}
	conn, err := netDial(ctx, address, first)
	if err != nil {
		return fail(err)
	}
	defer conn.close()
	s := &ftpSession{ctx: ctx, conn: conn, label: label}
	greeting, err := s.read()
	if err != nil {
		return fail(fmt.Errorf("reading the greeting of %s: %w", address.hostPort, err))
	}
	if greeting.code != 220 {
		return fail(fmt.Errorf("the server %s greeted with %d %s, not 220: it does not serve this client", address.hostPort, greeting.code, greeting.text))
	}
	traceOutcome(ctx, "220 "+greeting.text)
	if secured == "explicit" {
		if _, err := s.must("AUTH TLS", "", 234); err != nil {
			return nil, fmt.Errorf("%w; the server has no explicit TLS, which the target, or the collector's tls block, asks for", err)
		}
		if err := conn.secure(ctx, cfg, address.host, address.hostPort); err != nil {
			return nil, err
		}
	}
	user := c.Request.Username
	if user == "" {
		user, password = "anonymous", "exporter@"
	}
	if strings.ContainsAny(user+password, "\r\n") {
		return nil, errors.New("the account's name or password holds a line break, which a command cannot carry")
	}
	reply, err := s.must("USER "+user, "", 230, 331)
	if err != nil {
		return nil, err
	}
	if reply.code == 331 {
		if password == "" {
			return nil, fmt.Errorf("the server asks for the password of %s, and the collector has no request.password_file", user)
		}
		if _, err := s.must("PASS "+password, "PASS ***", 230, 202); err != nil {
			return nil, err
		}
	}
	if secured != "" {
		// The data connection is protected as the control connection is.
		if _, err := s.must("PBSZ 0", "", 200); err != nil {
			return nil, err
		}
		if _, err := s.must("PROT P", "", 200); err != nil {
			return nil, err
		}
	}
	if _, err := s.must("TYPE I", "", 200); err != nil {
		return nil, err
	}
	limit := responseLimit(c)
	tooLarge := func(size int64) error {
		return model.MarkError(model.Errorf("file %s is %d bytes, larger than the response limit; raise request.max_response_bytes or limits.max_response_bytes", full, model.Size(size)), model.ErrLimitExceeded)
	}
	// SIZE and MDTM (RFC 3659) are asked of a server that has them; one
	// that has not answers 500 or 502, and the file is read all the same.
	if reply, err = s.command("SIZE "+full, ""); err != nil {
		return nil, err
	} else if size, err := strconv.ParseInt(reply.text, 10, 64); reply.code == 213 && err == nil && size > limit {
		return nil, tooLarge(size)
	}
	var modified time.Time
	if reply, err = s.command("MDTM "+full, ""); err != nil {
		return nil, err
	} else if reply.code == 213 {
		stamp, _, _ := strings.Cut(reply.text, ".")
		modified, _ = time.Parse("20060102150405", stamp)
	}
	if c.Request.MaxAge > 0 {
		if modified.IsZero() {
			return nil, fmt.Errorf("the server did not say when %s was last modified (MDTM answered %d %s), so request.max_age cannot be held; leave max_age out for this server", full, reply.code, reply.text)
		}
		if err := checkMaxAge(c, full, modified); err != nil {
			return nil, err
		}
	}
	data, err := s.passive(address)
	if err != nil {
		return nil, err
	}
	defer data.close()
	if _, err := s.must("RETR "+full, "", 125, 150); err != nil {
		return nil, err
	}
	if cfg != nil {
		if err := data.secure(ctx, cfg, address.host, data.RemoteAddr().String()); err != nil {
			return nil, fmt.Errorf("the data connection: %w", err)
		}
	}
	body, err := io.ReadAll(io.LimitReader(data.r, limit+1))
	if err != nil {
		return fail(netFailure(ctx, fmt.Errorf("reading %s: %w", label, err)))
	}
	if int64(len(body)) > limit {
		return nil, tooLarge(int64(len(body)))
	}
	data.close()
	// The transfer's end: 226, or a failure the data connection's end did
	// not show, as a transfer cut short has.
	done, err := s.read()
	if err != nil {
		return fail(fmt.Errorf("the end of the transfer of %s: %w", label, err))
	}
	if done.code != 226 && done.code != 250 {
		return fail(model.Errorf("the server ended the transfer of %s with %d %s", full, done.code, done.text))
	}
	_, _ = s.command("QUIT", "")
	traceFacts(ctx, intFact("ftp.bytes", len(body)))
	headers := http.Header{}
	if contentType := localFileContentType(file); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	headers.Set("Content-Length", strconv.Itoa(len(body)))
	if !modified.IsZero() {
		headers.Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
	resp := &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Headers: headers, Body: body, Target: target, Collector: c.Name, Duration: time.Since(start)}
	if state := conn.tlsState(); state != nil {
		resp.Trip = &HTTPTrip{Total: resp.Duration, TLSConnection: tlsConnectionOf(state)}
	}
	return resp, nil
}

// passive asks the server for a data connection and makes it: EPSV, which
// names a port alone, and PASV for a server without it, whose answer's
// address is not used. Either way the connection is made to the address
// the control connection reached, so a server cannot point the exporter at
// another host.
func (s *ftpSession) passive(control netAddress) (*netConn, error) {
	host, _, err := net.SplitHostPort(s.conn.RemoteAddr().String())
	if err != nil {
		return nil, err
	}
	port := 0
	reply, err := s.command("EPSV", "")
	if err != nil {
		return nil, err
	}
	if reply.code == 229 {
		// "Entering Extended Passive Mode (|||6446|)"
		if open := strings.LastIndex(reply.text, "(|||"); open >= 0 {
			digits, _, _ := strings.Cut(reply.text[open+4:], "|")
			port, _ = strconv.Atoi(digits)
		}
	} else {
		if reply, err = s.must("PASV", "", 227); err != nil {
			return nil, fmt.Errorf("%w; the server gives no passive data connection, the only kind the exporter makes", err)
		}
		// "Entering Passive Mode (h1,h2,h3,h4,p1,p2)"
		numbers := strings.FieldsFunc(reply.text, func(r rune) bool { return r < '0' || r > '9' })
		if n := len(numbers); n >= 6 {
			high, _ := strconv.Atoi(numbers[n-2])
			low, _ := strconv.Atoi(numbers[n-1])
			if high < 256 && low < 256 {
				port = high<<8 | low
			}
		}
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("the server's answer to a passive data connection names no port: %d %s", reply.code, reply.text)
	}
	address := netAddress{host: control.host, hostPort: net.JoinHostPort(host, strconv.Itoa(port))}
	traceRequest(s.ctx, "CONNECT", "ftp-data://"+address.hostPort, nil, "", false)
	data, err := netDial(s.ctx, address, nil)
	if err != nil {
		err = fmt.Errorf("the data connection: %w", err)
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(s.ctx, "connected")
	return data, nil
}
