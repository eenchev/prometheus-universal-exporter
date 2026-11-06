//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An http collector may speak FastCGI to its target in place of HTTP,
// request.fastcgi: PHP-FPM's status and ping pages, which PHP-FPM serves
// on its FastCGI port or socket alone, and any other FastCGI application.
// The request is still written as an http collector's is, its URL, method,
// headers and body, and is sent as a FastCGI responder's request: the URL's
// host and port, or request.unix_socket, are where it is sent, its path the
// script's name, SCRIPT_NAME, and with document_root before it its file,
// SCRIPT_FILENAME, its query QUERY_STRING, its headers HTTP_ parameters,
// and its body FCGI_STDIN (fcgiParams). The application's answer is a CGI
// response, read as an HTTP answer is: its Status header the status, 200
// without one, and its other headers and body the answer's, so every
// decoder, rule, retry and redirect reads it as an HTTP answer.
//
// The client is the http.Transport's own for the http scheme, registered
// on the transports of FastCGI collectors (fastcgiTransport), so a
// request goes through the transport's dialer, the target policy's
// included, and every part of the http type above it is unchanged. Each
// request is a connection of its own, which the application closes once
// it has answered, as FCGI_KEEP_CONN left unset asks: a kept connection
// would need its own pool, and a status page is asked for once a scrape.
// A request's stderr, what PHP-FPM says of a script it could not find, is
// handed to the rules as the answer's X-Fastcgi-Stderr header, its first
// 4 KiB.

// FastCGIConfig is request.fastcgi: the directory the script's file is
// named in, SCRIPT_FILENAME being it and the URL's path, and parameters
// sent beside those made of the request, which replace one of theirs of the
// same name.
type FastCGIConfig struct {
	DocumentRoot string            `yaml:"document_root,omitempty"`
	Params       map[string]string `yaml:"params,omitempty"`
}

// fastcgiParamName is what a parameter of request.fastcgi.params is
// named: a CGI meta-variable's name, as RFC 3875 writes them.
var fastcgiParamName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FastCGIParamPattern is fastcgiParamName, for the schema.
const FastCGIParamPattern = `^[A-Za-z_][A-Za-z0-9_]*$`

// validateFastCGI checks request.fastcgi when the configuration loads. The
// request is sent to a FastCGI application, so a proxy, which speaks HTTP,
// a login, which is HTTP's, and TLS, which FastCGI is not carried in, are
// refused beside it; document_root is an absolute path, or left out; a
// parameter has a CGI meta-variable's name and no control character in its
// value, which would end the record a parameter is read from no sooner but
// say what the configuration did not. No key of it holds a placeholder: the
// script is the configuration's, never a probe's.
func validateFastCGI(c *model.Collector, f *FastCGIConfig) error {
	if f == nil {
		return nil
	}
	switch {
	case c.Request.ProxyURL != "" || c.Request.ProxyBasicAuthFile != nil:
		return fmt.Errorf("collector %q sets request.fastcgi and request.proxy_url or proxy_basic_auth_file; a FastCGI request goes to the application, never through an HTTP proxy, so leave the proxy keys out", c.Name)
	case c.Request.Login != nil:
		return fmt.Errorf("collector %q sets request.fastcgi and request.login; a login is an HTTP request of its own, which a FastCGI application does not answer, so leave login out", c.Name)
	case c.Request.TLS != model.TLSConfig{}:
		return fmt.Errorf("collector %q sets request.fastcgi and request.tls; FastCGI is spoken in plaintext, on a port or a Unix socket of the application's, so leave tls out", c.Name)
	case strings.Contains(f.DocumentRoot, "{{"):
		return fmt.Errorf("collector %q request.fastcgi.document_root has a placeholder; the script is the configuration's alone", c.Name)
	case f.DocumentRoot != "" && !strings.HasPrefix(f.DocumentRoot, "/"):
		return fmt.Errorf("collector %q request.fastcgi.document_root %q is not an absolute path; write the directory the application's scripts are in, such as /var/www/html, or leave it out", c.Name, f.DocumentRoot)
	}
	for name, value := range f.Params {
		switch {
		case !fastcgiParamName.MatchString(name):
			return fmt.Errorf("collector %q request.fastcgi.params %q is not a CGI variable's name; write letters, digits and _, not starting with a digit, such as SERVER_NAME", c.Name, name)
		case strings.Contains(value, "{{"):
			return fmt.Errorf("collector %q request.fastcgi.params.%s has a placeholder; the parameters are the configuration's alone", c.Name, name)
		case strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
			return fmt.Errorf("collector %q request.fastcgi.params.%s has a control character in it, almost always the line break an environment variable ended with", c.Name, name)
		}
	}
	return nil
}

// fastcgiKey is request.fastcgi written as one comparable value, the part
// of the transport's settings it is (TransportSettings.fastcgi): "" for
// none, and otherwise the document root and the parameters in the order of
// their names, each after a NUL, which neither may hold.
func fastcgiKey(f *FastCGIConfig) string {
	if f == nil {
		return ""
	}
	key := "fcgi\x00" + f.DocumentRoot
	names := make([]string, 0, len(f.Params))
	for name := range f.Params {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		key += "\x00" + name + "=" + f.Params[name]
	}
	return key
}

// parseFastCGIKey is fastcgiKey read back.
func parseFastCGIKey(key string) *FastCGIConfig {
	parts := strings.Split(key, "\x00")
	f := &FastCGIConfig{DocumentRoot: parts[1], Params: map[string]string{}}
	for _, part := range parts[2:] {
		name, value, _ := strings.Cut(part, "=")
		f.Params[name] = value
	}
	return f
}

func init() {
	collectorFastCGI = func(c *model.Collector) string { return fastcgiKey(httpRequest(c).FastCGI) }
	registerFastCGI = func(t *http.Transport, key string, dial func(ctx context.Context, network, address string) (net.Conn, error)) {
		t.RegisterProtocol("http", &fastcgiTransport{config: parseFastCGIKey(key), dial: dial})
	}
}

// fastcgiTransport sends a transport's http requests as FastCGI
// requests, dialing as the transport does.
type fastcgiTransport struct {
	config *FastCGIConfig
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
}

// The FastCGI records the client writes and reads (the FastCGI
// Specification, 1.0).
const (
	fcgiVersion         = 1
	fcgiBeginRequest    = 1
	fcgiEndRequest      = 3
	fcgiParams          = 4
	fcgiStdin           = 5
	fcgiStdout          = 6
	fcgiStderr          = 7
	fcgiResponder       = 1
	fcgiRequestID       = 1
	fcgiMaxContent      = 65535
	fcgiStderrKept      = 4 << 10
	fcgiHeaderStderr    = "X-Fastcgi-Stderr"
	fcgiDefaultPort     = "80"
	fcgiMaxCGIHeaders   = 1 << 20
	fcgiRequestComplete = 0
)

// RoundTrip sends req as a FastCGI request and answers the application's
// CGI response as an HTTP response, whose body reads the rest of its
// output as it comes; closing the body closes the connection.
func (f *fastcgiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	address := req.URL.Host
	if req.URL.Port() == "" {
		address = net.JoinHostPort(req.URL.Hostname(), fcgiDefaultPort)
	}
	conn, err := f.dial(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	closeAll := func() {
		stop()
		_ = conn.Close()
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var body []byte
	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			closeAll()
			return nil, err
		}
	}
	w := bufio.NewWriter(conn)
	writeRecord(w, fcgiBeginRequest, []byte{0, fcgiResponder, 0, 0, 0, 0, 0, 0})
	var params bytes.Buffer
	for _, p := range fcgiParamsOf(req, f.config, len(body), conn.LocalAddr(), conn.RemoteAddr()) {
		writePair(&params, p[0], p[1])
	}
	writeStream(w, fcgiParams, params.Bytes())
	writeStream(w, fcgiStdin, body)
	if err := w.Flush(); err != nil {
		closeAll()
		return nil, fastcgiError(ctx, fmt.Errorf("sending the FastCGI request to %s: %w", address, err))
	}
	out := &fcgiStdoutReader{r: bufio.NewReader(conn)}
	reader := bufio.NewReader(io.LimitReader(out, fcgiMaxCGIHeaders))
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		closeAll()
		if out.err != nil {
			err = out.err
		}
		return nil, fastcgiError(ctx, fmt.Errorf("reading the FastCGI application's answer from %s: %w", address, err))
	}
	resp := &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header(header), Request: req, ContentLength: -1,
	}
	if status := header.Get("Status"); status != "" {
		code, _, _ := strings.Cut(strings.TrimSpace(status), " ")
		n, err := strconv.Atoi(code)
		if err != nil || n < 100 || n > 599 {
			closeAll()
			return nil, fmt.Errorf("the FastCGI application at %s answered the Status %q, which is no HTTP status", address, status)
		}
		resp.StatusCode, resp.Status = n, strings.TrimSpace(status)
		resp.Header.Del("Status")
	}
	// The rest of the headers' reader, then whatever else comes, as one
	// body that the headers' bound no longer holds.
	// What the application wrote to its stderr before its headers, as
	// PHP-FPM writes "Primary script unknown" before its 404.
	if len(out.stderr) > 0 {
		resp.Header.Set(fcgiHeaderStderr, strings.TrimSpace(strings.ToValidUTF8(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, string(out.stderr)), "\uFFFD")))
	}
	resp.Body = &fcgiBody{Reader: io.MultiReader(reader, out), close: closeAll}
	return resp, nil
}

// fcgiParamsOf are the parameters of req's FastCGI request, in the order
// they are sent: the CGI meta-variables RFC 3875 names, made of the
// request; the request's headers as HTTP_ ones, but Content-Type and
// Content-Length, which are CONTENT_TYPE and CONTENT_LENGTH; and
// request.fastcgi.params, each in place of one of the same name.
func fcgiParamsOf(req *http.Request, f *FastCGIConfig, length int, local, remote net.Addr) [][2]string {
	script := req.URL.EscapedPath()
	if script == "" {
		script = "/"
	}
	port := req.URL.Port()
	if port == "" {
		port = fcgiDefaultPort
	}
	host, _, _ := net.SplitHostPort(local.String())
	if host == "" {
		host = "127.0.0.1"
	}
	serverAddr, _, _ := net.SplitHostPort(remote.String())
	values := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_SOFTWARE":   "prometheus-universal-exporter",
		"SERVER_PROTOCOL":   "HTTP/1.1",
		"REQUEST_METHOD":    req.Method,
		"REQUEST_SCHEME":    "http",
		"REQUEST_URI":       req.URL.RequestURI(),
		"SCRIPT_NAME":       script,
		"SCRIPT_FILENAME":   path.Join(f.DocumentRoot, script),
		"DOCUMENT_URI":      script,
		"DOCUMENT_ROOT":     f.DocumentRoot,
		"QUERY_STRING":      req.URL.RawQuery,
		"SERVER_NAME":       req.URL.Hostname(),
		"SERVER_PORT":       port,
		"SERVER_ADDR":       serverAddr,
		"REMOTE_ADDR":       host,
	}
	if f.DocumentRoot == "" {
		values["SCRIPT_FILENAME"] = script
	}
	if length > 0 {
		values["CONTENT_LENGTH"] = strconv.Itoa(length)
	}
	if t := req.Header.Get("Content-Type"); t != "" {
		values["CONTENT_TYPE"] = t
	}
	if req.Host != "" {
		values["HTTP_HOST"] = req.Host
	} else {
		values["HTTP_HOST"] = req.URL.Host
	}
	for name, vals := range req.Header {
		switch textproto.CanonicalMIMEHeaderKey(name) {
		case "Content-Type", "Content-Length", "Proxy":
			// Proxy is no header for an application to read as one: an
			// HTTP_PROXY parameter is a proxy setting to many (httpoxy).
			continue
		}
		values["HTTP_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))] = strings.Join(vals, ", ")
	}
	for name, value := range f.Params {
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, values[name]})
	}
	return out
}

// writeRecord writes one record of the request.
func writeRecord(w *bufio.Writer, kind byte, content []byte) {
	padding := (8 - len(content)%8) % 8
	header := []byte{fcgiVersion, kind, 0, fcgiRequestID, byte(len(content) >> 8), byte(len(content)), byte(padding), 0}
	_, _ = w.Write(header)
	_, _ = w.Write(content)
	_, _ = w.Write(make([]byte, padding))
}

// writeStream writes a stream's content in records of at most
// fcgiMaxContent bytes, and the empty record that ends it.
func writeStream(w *bufio.Writer, kind byte, content []byte) {
	for len(content) > 0 {
		n := min(len(content), fcgiMaxContent)
		writeRecord(w, kind, content[:n])
		content = content[n:]
	}
	writeRecord(w, kind, nil)
}

// writePair writes a name-value pair of FCGI_PARAMS: each length in one
// byte below 128 and in four, its high bit set, from 128.
func writePair(b *bytes.Buffer, name, value string) {
	for _, n := range []int{len(name), len(value)} {
		if n < 128 {
			b.WriteByte(byte(n))
		} else {
			_ = binary.Write(b, binary.BigEndian, uint32(n)|1<<31)
		}
	}
	b.WriteString(name)
	b.WriteString(value)
}

// fcgiStdoutReader reads the content of the application's FCGI_STDOUT
// records, keeping the first fcgiStderrKept bytes of its FCGI_STDERR, until
// its FCGI_END_REQUEST, after which it reads io.EOF.
type fcgiStdoutReader struct {
	r       *bufio.Reader
	left    int
	padding int
	done    bool
	err     error
	stderr  []byte
}

// Read reads the output's next bytes.
func (o *fcgiStdoutReader) Read(p []byte) (int, error) {
	for o.left == 0 {
		if o.done {
			return 0, io.EOF
		}
		if o.err != nil {
			return 0, o.err
		}
		if _, err := o.r.Discard(o.padding); err != nil {
			o.err = o.ended(err)
			return 0, o.err
		}
		var header [8]byte
		if _, err := io.ReadFull(o.r, header[:]); err != nil {
			o.err = o.ended(err)
			return 0, o.err
		}
		if header[0] != fcgiVersion {
			o.err = fmt.Errorf("the answer is no FastCGI record, version %d; is the target a FastCGI application's port?", header[0])
			return 0, o.err
		}
		length := int(binary.BigEndian.Uint16(header[4:6]))
		o.padding = int(header[6])
		switch header[1] {
		case fcgiStdout:
			o.left = length
		case fcgiStderr:
			content := make([]byte, length)
			if _, err := io.ReadFull(o.r, content); err != nil {
				o.err = o.ended(err)
				return 0, o.err
			}
			o.stderr = append(o.stderr, content[:min(len(content), fcgiStderrKept-len(o.stderr))]...)
		case fcgiEndRequest:
			content := make([]byte, length)
			if _, err := io.ReadFull(o.r, content); err != nil {
				o.err = o.ended(err)
				return 0, o.err
			}
			if len(content) >= 5 && content[4] != fcgiRequestComplete {
				o.err = fmt.Errorf("the FastCGI application did not complete the request: its status is %d (1 cannot multiplex, 2 overloaded, 3 unknown role)", content[4])
				return 0, o.err
			}
			o.done = true
		default:
			if _, err := o.r.Discard(length); err != nil {
				o.err = o.ended(err)
				return 0, o.err
			}
		}
	}
	n := min(len(p), o.left)
	n, err := o.r.Read(p[:n])
	o.left -= n
	if err != nil {
		o.err = o.ended(err)
		if n == 0 {
			return 0, o.err
		}
	}
	return n, nil
}

// ended is what a read that failed says: an end of the connection before
// the end of the request is the application closing it with no answer.
func (o *fcgiStdoutReader) ended(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the FastCGI application closed the connection before it ended the request")
	}
	return err
}

// fcgiBody is a FastCGI answer's body, which closes the connection when it
// is closed.
type fcgiBody struct {
	io.Reader
	close func()
	once  sync.Once
}

// Close closes the connection.
func (b *fcgiBody) Close() error {
	b.once.Do(b.close)
	return nil
}

// fastcgiError is err, or the context's error with err when the request
// was cancelled or its deadline passed, which is what the read saw.
func fastcgiError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	return err
}
