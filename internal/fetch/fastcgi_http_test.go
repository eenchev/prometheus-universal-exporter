//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/fcgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// fastcgiCollector is a validated http collector of request.fastcgi f.
func fastcgiCollector(t *testing.T, f *FastCGIConfig, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := testutil.Collector("fpm", "text")
	c.Request.Own = HTTPConfig{FastCGI: f}
	for _, e := range edit {
		e(&c)
	}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// fcgiApplication serves handler over FastCGI, as Go's net/http/fcgi
// serves it, on ln, and answers each request's CGI parameters on params.
func fcgiApplication(t *testing.T, ln net.Listener, params chan<- map[string]string, handler http.HandlerFunc) {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		_ = fcgi.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if params != nil {
				params <- fcgi.ProcessEnv(r)
			}
			handler(w, r)
		}))
	}()
}

// An http collector of request.fastcgi sends its request to the URL's host
// and port as a FastCGI responder's request: the path is SCRIPT_NAME and,
// with no document_root, SCRIPT_FILENAME, the query QUERY_STRING, the
// method REQUEST_METHOD, a header an HTTP_ parameter and the body the
// application's stdin; the CGI answer is read as an HTTP answer, its body,
// Content-Type and status, a Status header of the application's included.
// document_root names the script's file, and params replace parameters of
// their name, a value of 128 bytes or more written with a length of four.
func TestAnHTTPCollectorSpeaksFastCGI(t *testing.T) {
	proxyEnvironment(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	params := make(chan map[string]string, 4)
	type seen struct{ Path, Query, Method, Header, Body string }
	fcgiApplication(t, ln, params, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
		}
		_ = json.NewEncoder(w).Encode(seen{r.URL.Path, r.URL.RawQuery, r.Method, r.Header.Get("X-Pool"), string(body)})
	})
	target := "http://" + ln.Addr().String()
	c := fastcgiCollector(t, &FastCGIConfig{}, func(c *model.Collector) {
		c.Request.Own = HTTPConfig{Method: "POST", Body: "hello", FastCGI: &FastCGIConfig{}}
		c.Request.Headers = map[string]string{"X-Pool": "www"}
		c.Request.Path = "/status"
		c.Request.Query = map[string]string{"full": ""}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got seen
	if err := json.Unmarshal(resp.Body, &got); err != nil || got != (seen{"/status", "full=", "POST", "www", "hello"}) || resp.StatusCode != http.StatusOK || resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("answered %d %v %q: %+v, %v", resp.StatusCode, resp.Headers, resp.Body, got, err)
	}
	env := <-params
	// What net/http/fcgi made no part of the request, which is the rest.
	for name, want := range map[string]string{"SCRIPT_FILENAME": "/status", "SERVER_NAME": "127.0.0.1", "DOCUMENT_URI": "/status"} {
		if env[name] != want {
			t.Errorf("%s is %q, want %q", name, env[name], want)
		}
	}
	missing := fastcgiCollector(t, &FastCGIConfig{DocumentRoot: "/srv/www", Params: map[string]string{"SERVER_NAME": "fpm.example", "APP_ENV": "prod", "LONG": strings.Repeat("x", 200)}}, func(c *model.Collector) {
		c.Request.Path, c.Request.AcceptStatus = "/missing", []string{"404"}
	})
	resp, err = FetchCollector(ctx, target, missing, RequestOverrides{}, nil)
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the application's 404: %+v, %v", resp, err)
	}
	env = <-params
	if env["SCRIPT_FILENAME"] != "/srv/www/missing" || env["DOCUMENT_ROOT"] != "/srv/www" || env["SERVER_NAME"] != "fpm.example" || env["APP_ENV"] != "prod" || env["LONG"] != strings.Repeat("x", 200) {
		t.Errorf("document_root and params: %v", env)
	}
}

// A collector of request.fastcgi and request.unix_socket speaks FastCGI on
// the socket, as PHP-FPM listens on one.
func TestFastCGIGoesOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "fpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "php-fpm.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fcgiApplication(t, ln, nil, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })
	c := fastcgiCollector(t, &FastCGIConfig{}, func(c *model.Collector) { c.Request.UnixSocket = socket })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, "http://php-fpm/ping", c, RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != "pong" {
		t.Fatalf("over the socket: %+v, %v", resp, err)
	}
}

// writeTestRecord writes one FastCGI record of request 1.
func writeTestRecord(w io.Writer, kind byte, content string) {
	header := []byte{1, kind, 0, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(header[4:], uint16(len(content)))
	_, _ = w.Write(append(header, content...))
}

// What an application writes to its stderr before its headers reaches the
// rules as the X-Fastcgi-Stderr header, as PHP-FPM's "Primary script
// unknown" does before its 404; a target that answers no FastCGI record, as
// an HTTP server's, fails, saying so; an https target is refused.
func TestFastCGIStderrAndWhatIsNoFastCGI(t *testing.T) {
	params := make(chan map[string]string, 1)
	fpm := testutil.NewTCPStub(t, func(conn net.Conn) {
		pairs, ok := readTestRequest(conn)
		if !ok {
			return
		}
		params <- readTestPairs(pairs)
		writeTestRecord(conn, fcgiStderr, "Primary script unknown\n")
		writeTestRecord(conn, fcgiStdout, "Status: 404 Not Found\r\nContent-Type: text/plain\r\n\r\nFile not found.\n")
		writeTestRecord(conn, fcgiStdout, "")
		writeTestRecord(conn, fcgiEndRequest, "\x00\x00\x00\x00\x00\x00\x00\x00")
	})
	c := fastcgiCollector(t, &FastCGIConfig{}, func(c *model.Collector) { c.Request.AcceptStatus = []string{"404"} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, "http://"+fpm.Addr+"/nope.php", c, RequestOverrides{}, nil)
	if err != nil || resp.StatusCode != http.StatusNotFound || string(resp.Body) != "File not found.\n" || resp.Headers.Get("X-Fastcgi-Stderr") != "Primary script unknown" {
		t.Fatalf("answered %+v, %v", resp, err)
	}
	env := <-params
	for name, want := range map[string]string{"SCRIPT_NAME": "/nope.php", "SCRIPT_FILENAME": "/nope.php", "REQUEST_METHOD": "GET", "QUERY_STRING": "", "REQUEST_URI": "/nope.php", "GATEWAY_INTERFACE": "CGI/1.1", "SERVER_PORT": strings.Split(fpm.Addr, ":")[1]} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	if _, ok := env["CONTENT_LENGTH"]; ok {
		t.Errorf("a request without a body sent CONTENT_LENGTH %q", env["CONTENT_LENGTH"])
	}
	// It reads the request first: closing with it unread would make the
	// system reset the connection, which the client may read in place of
	// the answer.
	plain := testutil.NewTCPStub(t, func(conn net.Conn) {
		if _, ok := readTestRequest(conn); ok {
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		}
	})
	if _, err := FetchCollector(ctx, "http://"+plain.Addr+"/status", c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "is no FastCGI record") {
		t.Errorf("an HTTP server: %v", err)
	}
	if err := CheckTarget(c, "https://fpm:9000/status", false); err == nil || !strings.Contains(err.Error(), "is not an http:// URL") {
		t.Errorf("an https target: %v", err)
	}
}

// readTestRequest reads a FastCGI request's records up to the empty
// FCGI_STDIN that ends it, and answers the content of its FCGI_PARAMS.
func readTestRequest(conn net.Conn) ([]byte, bool) {
	r := bufio.NewReader(conn)
	var pairs []byte
	for {
		var header [8]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, false
		}
		length, padding := int(binary.BigEndian.Uint16(header[4:])), int(header[6])
		content := make([]byte, length+padding)
		if _, err := io.ReadFull(r, content); err != nil {
			return nil, false
		}
		if header[1] == fcgiParams {
			pairs = append(pairs, content[:length]...)
		}
		if header[1] == fcgiStdin && length == 0 {
			return pairs, true
		}
	}
}

// readTestPairs reads the name-value pairs of FCGI_PARAMS.
func readTestPairs(b []byte) map[string]string {
	out := map[string]string{}
	length := func() int {
		if b[0] < 128 {
			n := int(b[0])
			b = b[1:]
			return n
		}
		n := int(binary.BigEndian.Uint32(b) &^ (1 << 31))
		b = b[4:]
		return n
	}
	for len(b) > 0 {
		n, v := length(), length()
		out[string(b[:n])] = string(b[n : n+v])
		b = b[n+v:]
	}
	return out
}

// request.fastcgi is checked when the configuration loads.
func TestFastCGIKeysAreChecked(t *testing.T) {
	for _, c := range []struct {
		edit func(*model.Collector)
		want string
	}{
		{func(c *model.Collector) { c.Request.ProxyURL = "http://proxy:3128" }, "sets request.fastcgi and request.proxy_url"},
		{func(c *model.Collector) { c.Request.TLS = model.TLSConfig{InsecureSkipVerify: true} }, "sets request.fastcgi and request.tls"},
		{func(c *model.Collector) { c.Request.Login = &model.LoginConfig{} }, "sets request.fastcgi and request.login"},
		{func(c *model.Collector) { c.Request.Own = HTTPConfig{FastCGI: &FastCGIConfig{DocumentRoot: "www"}} }, `document_root "www" is not an absolute path`},
		{func(c *model.Collector) {
			c.Request.Own = HTTPConfig{FastCGI: &FastCGIConfig{Params: map[string]string{"9X": "y"}}}
		}, `params "9X" is not a CGI variable's name`},
		{func(c *model.Collector) {
			c.Request.Own = HTTPConfig{FastCGI: &FastCGIConfig{Params: map[string]string{"X": "{{param_x}}"}}}
		}, "params.X has a placeholder"},
		{func(c *model.Collector) {
			c.Request.Own = HTTPConfig{FastCGI: &FastCGIConfig{Params: map[string]string{"X": "a\n"}}}
		}, "params.X has a control character"},
	} {
		x := testutil.Collector("fpm", "text")
		x.Request.Own = HTTPConfig{FastCGI: &FastCGIConfig{}}
		c.edit(&x)
		if err := ValidateRequest(&x); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("got %v, want %q", err, c.want)
		}
	}
}

// A bare host:port target, as Prometheus's discovery hands one over, is an
// http:// one, and a redirect to https:// is not followed, since it would
// leave FastCGI for an HTTPS request to the host it names; SCRIPT_NAME is
// the path decoded, as CGI has it.
func TestFastCGITakesABareTargetAndFollowsNoRedirectToHTTPS(t *testing.T) {
	c := fastcgiCollector(t, &FastCGIConfig{})
	if err := CheckTarget(c, "php-fpm:9000", false); err != nil {
		t.Errorf("a bare host:port: %v", err)
	}
	hits := 0
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, "over https")
	}))
	t.Cleanup(secure.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	params := make(chan map[string]string, 1)
	fcgiApplication(t, ln, params, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secure.URL+"/x", http.StatusFound)
	})
	follows := fastcgiCollector(t, &FastCGIConfig{}, func(c *model.Collector) { c.Request.FollowRedirects = true })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = FetchCollector(ctx, ln.Addr().String()+"/my%20status", follows, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "redirected to an https:// URL") || hits != 0 {
		t.Errorf("a redirect to https: %v, %d requests over HTTPS", err, hits)
	}
	if env := <-params; env["SCRIPT_FILENAME"] != "/my status" {
		t.Errorf("SCRIPT_FILENAME is %q, want the path decoded", env["SCRIPT_FILENAME"])
	}
}
