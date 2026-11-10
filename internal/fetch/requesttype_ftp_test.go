//go:build request_type_ftp

package fetch

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// ftpServer is an FTP server as the tests need one: it serves files by
// their path, to the account user and pass or to anonymous, begins TLS on
// AUTH TLS, opens a passive data connection on EPSV, or on PASV alone when
// noEPSV is set, naming another host's address there, and answers SIZE and
// MDTM unless plain is set. It keeps the commands it was sent.
type ftpServer struct {
	*testutil.TCPStub
	mu         sync.Mutex
	config     *tls.Config
	files      map[string]string
	modified   time.Time
	user, pass string
	noEPSV     bool
	plain      bool
	commands   []string
	// dataResumed is whether the last data connection over TLS resumed
	// the control connection's session; dataConns counts them all.
	dataResumed bool
	dataConns   int
}

func newFTPServer(t *testing.T) *ftpServer {
	t.Helper()
	s := &ftpServer{files: map[string]string{}, modified: time.Now().Add(-time.Minute)}
	var caFile string
	s.config, caFile = testutil.StubTLS(t)
	s.TCPStub = testutil.NewTCPStub(t, s.serve)
	s.CAFile = caFile
	return s
}

// set changes what the server serves, under its lock.
func (s *ftpServer) set(change func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change()
}

// transfers is how many data connections the server took, and whether the
// last one over TLS resumed the control connection's session.
func (s *ftpServer) transfers() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dataConns, s.dataResumed
}

func (s *ftpServer) sent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, "|")
}

func (s *ftpServer) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	say := func(text string) { _, _ = fmt.Fprint(conn, text+"\r\n") }
	say("220-Welcome to the test server\r\n  no anonymous uploads\r\n220 ready")
	var data net.Listener
	defer func() {
		if data != nil {
			_ = data.Close()
		}
	}()
	secure, protect, user := false, false, ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		s.mu.Lock()
		s.commands = append(s.commands, line)
		plain, noEPSV, account, pass, modified := s.plain, s.noEPSV, s.user, s.pass, s.modified
		content, ok := s.files[arg]
		s.mu.Unlock()
		switch verb {
		case "AUTH":
			say("234 begin TLS")
			server := tls.Server(conn, s.config)
			if err := server.Handshake(); err != nil {
				return
			}
			conn, reader, secure = server, bufio.NewReader(server), true
		case "USER":
			user = arg
			say("331 password please")
		case "PASS":
			if user == "anonymous" && account == "" || user == account && arg == pass {
				say("230 logged in")
			} else {
				say("530 Login incorrect.")
			}
		case "PBSZ":
			say("200 PBSZ=0")
		case "PROT":
			protect = secure
			say("200 private")
		case "TYPE":
			say("200 binary")
		case "SIZE", "MDTM":
			switch {
			case plain:
				say("502 not implemented")
			case !ok:
				say("550 no such file")
			case verb == "SIZE":
				say("213 " + strconv.Itoa(len(content)))
			default:
				say("213 " + modified.UTC().Format("20060102150405") + ".123")
			}
		case "EPSV", "PASV":
			if verb == "EPSV" && noEPSV {
				say("500 what?")
				continue
			}
			var listen net.ListenConfig
			if data, err = listen.Listen(context.Background(), "tcp", "127.0.0.1:0"); err != nil {
				return
			}
			port := data.Addr().(*net.TCPAddr).Port
			if verb == "EPSV" {
				say(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", port))
			} else {
				// Another host's address, as a server behind NAT, or one
				// that would have the client call elsewhere, gives.
				say(fmt.Sprintf("227 Entering Passive Mode (10,9,8,7,%d,%d).", port>>8, port&0xff))
			}
		case "RETR":
			if !ok || data == nil {
				say("550 Failed to open file.")
				continue
			}
			say("150 here it comes")
			transfer, err := data.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.dataConns++
			s.mu.Unlock()
			if protect {
				server := tls.Server(transfer, s.config)
				if err := server.Handshake(); err != nil {
					_ = transfer.Close()
					say("425 no TLS on the data connection")
					continue
				}
				s.mu.Lock()
				s.dataResumed = server.ConnectionState().DidResume
				s.mu.Unlock()
				transfer = server
			}
			_, _ = transfer.Write([]byte(content))
			_ = transfer.Close()
			say("226 Transfer complete.")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

func ftpCollector(t *testing.T, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "drop", Request: model.RequestConfig{Type: RequestTypeFTP, Root: "/exports", Path: "{{param_site}}/meters.csv"}}
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

var berlin = RequestOverrides{Params: map[string]string{"param_site": "berlin"}}

// A scrape logs in as anonymous when the collector has no account, asks
// for binary transfer, the file's size and time, a passive data
// connection, and the file under request.root, its placeholder filled
// from the probe: the answer is the file, with the Content-Type its
// extension names and its modification time, and nothing is asked that
// writes.
func TestAnFTPScrapeReadsAFile(t *testing.T) {
	s := newFTPServer(t)
	s.files["/exports/berlin/meters.csv"] = "meter,kwh\na,1\n"
	c := ftpCollector(t)
	resp, err := FetchCollector(context.Background(), s.Addr, c, berlin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "meter,kwh\na,1\n" || resp.Headers.Get("Content-Type") != "text/csv" || resp.Headers.Get("Last-Modified") != s.modified.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") {
		t.Errorf("the answer is %q, %v", resp.Body, resp.Headers)
	}
	want := "USER anonymous|PASS exporter@|TYPE I|SIZE /exports/berlin/meters.csv|MDTM /exports/berlin/meters.csv|EPSV|RETR /exports/berlin/meters.csv|QUIT"
	if sent := s.sent(); sent != want {
		t.Errorf("the server was sent %s\nwant %s", sent, want)
	}
	if label, err := RequestLabelFor("ftp://"+s.Addr, c, berlin); err != nil || label != "ftp://"+s.Addr+"/exports/{{param_site}}/meters.csv" {
		t.Errorf("the label is %q, %v", label, err)
	}
	// A value that would lead to another directory, and a path parameter
	// that leads outside request.root, are refused before the server is
	// connected to.
	before := s.Accepted()
	for name, overrides := range map[string]RequestOverrides{
		"a value with a /": {Params: map[string]string{"param_site": "../etc"}},
		"a path outside":   {Path: "../etc/passwd", PathSet: true},
		"a line break":     {Path: "a\r\nDELE x", PathSet: true},
	} {
		if _, err := FetchCollector(context.Background(), s.Addr, c, overrides, nil); err == nil || s.Accepted() != before {
			t.Errorf("%s: err=%v, %d connections more", name, err, s.Accepted()-before)
		}
	}
}

// An ftpes:// target, and one without a scheme of a collector with a tls
// block, begins TLS with AUTH TLS before the account is sent, asks for a
// protected data connection, and makes that one's handshake resuming the
// control connection's session, as servers require; the password is in
// neither the error nor the trace of a login refused.
func TestAnFTPScrapeOverExplicitTLS(t *testing.T) {
	s := newFTPServer(t)
	s.set(func() { s.user, s.pass = "exporter", "s3cret" })
	s.files["/exports/berlin/meters.csv"] = "a,1\n"
	password := testutil.WriteFile(t, "password", "s3cret\n")
	c := ftpCollector(t, withAccount("exporter", password), func(r *model.RequestConfig) { r.TLS = model.TLSConfig{CAFile: s.CAFile} })
	for _, target := range []string{"ftpes://" + s.Addr, s.Addr} {
		s.mu.Lock()
		s.commands, s.dataResumed = nil, false
		s.mu.Unlock()
		resp, err := FetchCollector(context.Background(), target, c, berlin, nil)
		if err != nil || string(resp.Body) != "a,1\n" || resp.Trip == nil || resp.Trip.TLSConnection == nil {
			t.Fatalf("%s: %v, %+v", target, err, resp)
		}
		if sent := s.sent(); !strings.HasPrefix(sent, "AUTH TLS|USER exporter|PASS s3cret|PBSZ 0|PROT P|TYPE I|") {
			t.Errorf("%s: the server was sent %s", target, sent)
		}
		if _, resumed := s.transfers(); !resumed {
			t.Errorf("%s: the data connection did not resume the control connection's TLS session", target)
		}
	}
	wrong := ftpCollector(t, withAccount("exporter", testutil.WriteFile(t, "wrong", "hunter2\n")), func(r *model.RequestConfig) { r.TLS = model.TLSConfig{CAFile: s.CAFile} })
	ctx, trace := WithRequestTrace(context.Background())
	_, err := FetchCollector(ctx, s.Addr, wrong, berlin, nil)
	if err == nil || !strings.Contains(err.Error(), "the server answered PASS *** with 530 Login incorrect.") {
		t.Errorf("err=%v, want the refusal", err)
	}
	shown := fmt.Sprint(err, trace.Requests())
	if strings.Contains(shown, "hunter2") || !strings.Contains(shown, "PASS ***") {
		t.Errorf("the error and the trace are %s", shown)
	}
	// A plaintext ftp:// target of the same collector sends no AUTH TLS.
	s.mu.Lock()
	s.commands = nil
	s.mu.Unlock()
	if _, err := FetchCollector(context.Background(), "ftp://"+s.Addr, c, berlin, nil); err != nil || strings.Contains(s.sent(), "AUTH") {
		t.Errorf("ftp://: %v, sent %s", err, s.sent())
	}
}

// A server without EPSV is asked with PASV, and the data connection is
// made to the server the control connection reached, whatever address the
// answer names; one without SIZE and MDTM is read all the same, but for a
// collector with max_age, which cannot be held.
func TestAnFTPScrapeOfAnOldServer(t *testing.T) {
	s := newFTPServer(t)
	s.set(func() { s.noEPSV, s.plain = true, true })
	s.files["/exports/berlin/meters.csv"] = "a,1\n"
	resp, err := FetchCollector(context.Background(), s.Addr, ftpCollector(t), berlin, nil)
	if err != nil || string(resp.Body) != "a,1\n" || resp.Headers.Get("Last-Modified") != "" {
		t.Fatalf("%v, %+v", err, resp)
	}
	if sent := s.sent(); !strings.Contains(sent, "|EPSV|PASV|RETR ") {
		t.Errorf("the server was sent %s", sent)
	}
	aged := ftpCollector(t, func(r *model.RequestConfig) { r.MaxAge = model.Duration(time.Hour) })
	if _, err := FetchCollector(context.Background(), s.Addr, aged, berlin, nil); err == nil || !strings.Contains(err.Error(), "so request.max_age cannot be held") {
		t.Errorf("max_age without MDTM: err=%v", err)
	}
	s.set(func() { s.plain, s.modified = false, time.Now().Add(-2*time.Hour) })
	if _, err := FetchCollector(context.Background(), s.Addr, aged, berlin, nil); err == nil || !strings.Contains(err.Error(), "longer than request.max_age 1h") {
		t.Errorf("an old file: err=%v", err)
	}
}

// A file that is not there fails the scrape with the server's words; one
// larger than the response limit fails it as a limit exceeded, by its SIZE
// before a data connection is made, and by what was read of a server
// without SIZE.
func TestAnFTPScrapeFailsOnAMissingOrALargeFile(t *testing.T) {
	s := newFTPServer(t)
	_, err := FetchCollector(context.Background(), s.Addr, ftpCollector(t), berlin, nil)
	if err == nil || !strings.Contains(err.Error(), "the server answered RETR /exports/berlin/meters.csv with 550 Failed to open file.") {
		t.Errorf("err=%v, want the server's refusal", err)
	}
	s.files["/exports/berlin/meters.csv"] = strings.Repeat("x", 5000)
	small := ftpCollector(t, func(r *model.RequestConfig) { r.MaxResponseBytes = 1024 })
	if _, err := FetchCollector(context.Background(), s.Addr, small, berlin, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("err=%v; want the limit by SIZE", err)
	}
	if made, _ := s.transfers(); made != 0 {
		t.Errorf("%d data connections were made for a file SIZE says is too large", made)
	}
	s.set(func() { s.plain = true })
	if _, err := FetchCollector(context.Background(), s.Addr, small, berlin, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("err=%v; want the limit by what was read", err)
	}
	stub := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = fmt.Fprint(conn, "SSH-2.0-OpenSSH_9.6\r\n") })
	if _, err := FetchCollector(context.Background(), stub.Addr, ftpCollector(t), berlin, nil); err == nil || !strings.Contains(err.Error(), "is the target an FTP server?") {
		t.Errorf("err=%v, want what is no FTP server named", err)
	}
}

// A collector is refused at load without a root, with a path outside it,
// and with a password and no user.
func TestFTPConfigurationRules(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"no root":        {func(r *model.RequestConfig) { r.Root = "" }, "request.root is required for request.type ftp"},
		"relative root":  {func(r *model.RequestConfig) { r.Root = "exports" }, "must be an absolute path"},
		"path outside":   {func(r *model.RequestConfig) { r.Path = "../x" }, "leads outside request.root"},
		"line break":     {func(r *model.RequestConfig) { r.Path = "a\nb" }, "holds a line break"},
		"password alone": {func(r *model.RequestConfig) { r.PasswordFile = "/p" }, "request.password_file without request.username"},
	} {
		c := &model.Collector{Name: "drop", Request: model.RequestConfig{Type: RequestTypeFTP, Root: "/exports", Path: "a.csv"}}
		tc.change(&c.Request)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	for _, target := range []string{"sftp://host.example", "ftp://host.example/exports/a.csv", "ftp://user:pass@host.example"} {
		if _, err := FetchCollector(context.Background(), target, ftpCollector(t), berlin, nil); err == nil {
			t.Errorf("the target %q is taken", target)
		}
	}
}

// A server among the collector's denied targets is refused before it is
// connected to.
func TestAnFTPCollectorHoldsTheTargetPolicy(t *testing.T) {
	s := newFTPServer(t)
	c := ftpCollector(t, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), s.Addr, c, berlin, nil); !errors.Is(err, ErrTargetRefused) || s.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, s.Accepted())
	}
}

// The keys the opt-in ftp type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheFTPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeFTP)
}
