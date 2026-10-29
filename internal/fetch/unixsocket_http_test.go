//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An http collector's request.unix_socket (unixsocket.go).

// socketRequest is what a server on a Unix socket was asked.
type socketRequest struct {
	method, host, uri, auth, tenant, sni string
}

// socketServer serves handle on a Unix socket in a directory of its own,
// over TLS with config when it is not nil, and returns the socket's path
// and what it was asked. The directory is under the system's temporary
// directory rather than the test's, whose path can be longer than a socket
// path may be.
func socketServer(t *testing.T, config *tls.Config, handle http.HandlerFunc) (socket string, seen func() []socketRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "us")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket = filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []socketRequest
	server := &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := socketRequest{method: r.Method, host: r.Host, uri: r.RequestURI, auth: r.Header.Get("Authorization"), tenant: r.Header.Get("X-Tenant")}
		if r.TLS != nil {
			request.sni = r.TLS.ServerName
		}
		mu.Lock()
		asked = append(asked, request)
		mu.Unlock()
		if handle != nil {
			handle(w, r)
			return
		}
		_, _ = io.WriteString(w, "value=7\n")
	})}
	if config != nil {
		listener = tls.NewListener(listener, config)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket, func() []socketRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]socketRequest(nil), asked...)
	}
}

// socketCollector is an http collector, validated, whose requests go to
// socket.
func socketCollector(t *testing.T, name, socket string) *model.Collector {
	t.Helper()
	c := testutil.Collector(name, "text")
	c.Request.Own = HTTPConfig{UnixSocket: socket}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// A collector's request goes over its Unix socket, with the URL's host as
// the Host header and its path and query, the collector's own query, its
// headers and its credentials, as it would go to the host; the URL's host
// is never looked up or dialed, and the environment's proxy is asked
// nothing.
func TestARequestGoesOverTheCollectorsUnixSocket(t *testing.T) {
	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	socket, seen := socketServer(t, nil, nil)
	c := testutil.Collector("docker", "text")
	c.Request.Own = HTTPConfig{UnixSocket: socket}
	c.Request.Path = "/v1.43/info"
	c.Request.Query = map[string]string{"all": "1"}
	c.Request.Headers = map[string]string{"X-Tenant": "blue"}
	c.Request.BasicAuth = &model.BasicAuth{Username: "scraper", Password: "s3cret"}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	response, err := FetchCollector(context.Background(), "http://docker.example:2375?since=5", &c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Body) != "value=7\n" {
		t.Fatalf("the answer is %q, want the socket's", response.Body)
	}
	asked := seen()
	if len(asked) != 1 {
		t.Fatalf("the socket was asked %+v, want one request", asked)
	}
	got := asked[0]
	if got.method != http.MethodGet || got.host != "docker.example:2375" || got.tenant != "blue" || got.auth == "" {
		t.Errorf("the socket was asked %+v", got)
	}
	u, err := url.Parse(got.uri)
	if err != nil || u.Path != "/v1.43/info" || u.Query().Get("since") != "5" || u.Query().Get("all") != "1" {
		t.Errorf("the request URI is %q, want the URL's path and both queries", got.uri)
	}
	if asked, _ := environment.seen(); len(asked) != 0 {
		t.Errorf("the environment's proxy was asked %q", asked)
	}
}

// An https URL is asked over the socket with TLS, the URL's host being the
// server name the certificate is verified for.
func TestAnHTTPSRequestGoesOverTheUnixSocketWithTLS(t *testing.T) {
	proxyEnvironment(t)
	certFile, keyFile := selfSigned(t, "socket", "daemon.local")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	socket, seen := socketServer(t, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil)
	c := socketCollector(t, "daemon", socket)
	c.Request.TLS = model.TLSConfig{CAFile: certFile}
	if _, err := FetchCollector(context.Background(), "https://daemon.local/status", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if asked := seen(); len(asked) != 1 || asked[0].sni != "daemon.local" || asked[0].host != "daemon.local" || asked[0].uri != "/status" {
		t.Fatalf("the socket was asked %+v, want /status over TLS for daemon.local", asked)
	}
	// A certificate for another name is refused, as it is over TCP.
	if _, err := FetchCollector(context.Background(), "https://other.local/status", c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v, want the certificate refused for other.local", err)
	}
}

// Collectors with different sockets never share a connection pool, nor
// does one with a socket with one that dials the URL's host: each request
// reaches the socket its own collector names.
func TestCollectorsWithDifferentUnixSocketsShareNoConnection(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, nil)
	first, firstSeen := socketServer(t, nil, nil)
	second, secondSeen := socketServer(t, nil, nil)
	a, b := socketCollector(t, "a", first), socketCollector(t, "b", second)
	for range 2 {
		for _, c := range []*model.Collector{a, b} {
			if _, err := FetchCollector(context.Background(), "http://localhost/status", c, RequestOverrides{}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(firstSeen()) != 2 || len(secondSeen()) != 2 {
		t.Fatalf("the first socket was asked %d times and the second %d, want 2 each", len(firstSeen()), len(secondSeen()))
	}
	tcp := testutil.Collector("tcp", "text")
	if _, err := FetchCollector(context.Background(), "http://localhost:1/status", &tcp, RequestOverrides{}, nil); err == nil {
		t.Fatal("a collector without a socket reached one")
	}
	if got := transports.size(); got != 3 {
		t.Fatalf("%d connection pools, want one for each socket and one for TCP", got)
	}
}

// A login goes over the collector's socket, as its requests do.
func TestALoginGoesOverTheCollectorsUnixSocket(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, nil)
	freshLogins(t)
	socket, seen := socketServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = io.WriteString(w, `{"access_token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "value=5\n")
	})
	c := testutil.Collector("api", "text")
	c.Request.Own = HTTPConfig{UnixSocket: socket}
	c.Request.Login = &model.LoginConfig{Type: "json", URL: "http://auth.example/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), "http://api.example/status", &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	asked := seen()
	if len(asked) != 2 || asked[0].host != "auth.example" || asked[0].uri != "/login" || asked[1].host != "api.example" {
		t.Fatalf("the socket was asked %+v, want the login and the request", asked)
	}
}

// allowed_targets and denied_targets hold the URL's host over a socket to
// its name and to an address written as the host; a name allowed_targets
// does not list is refused, since the socket gives no address to hold to
// the list, and the socket is asked nothing for a refused host.
func TestTheTargetPolicyAppliesToTheHostOverAUnixSocket(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, map[string]string{"inside.example": "10.1.2.3"})
	socket, seen := socketServer(t, nil, nil)
	for name, c := range map[string]struct {
		allowed, denied []string
		target          string
		refused         bool
	}{
		"a denied name":                     {denied: []string{"*.blocked.example"}, target: "http://api.blocked.example/", refused: true},
		"an address in a denied network":    {denied: []string{"10.0.0.0/8"}, target: "http://10.9.9.9/", refused: true},
		"the metadata address":              {target: "http://169.254.169.254/", refused: true},
		"a name not allowed":                {allowed: []string{"api.example"}, target: "http://other.example/", refused: true},
		"a name, with networks allowed":     {allowed: []string{"10.0.0.0/8"}, target: "http://inside.example/", refused: true},
		"an allowed name":                   {allowed: []string{"api.example"}, target: "http://api.example/"},
		"an allowed address":                {allowed: []string{"10.0.0.0/8"}, target: "http://10.9.9.9/"},
		"a name not looked up for a deny":   {denied: []string{"10.0.0.0/8"}, target: "http://inside.example/"},
		"no lists":                          {target: "http://localhost/"},
		"a denied loopback, not the socket": {denied: []string{"127.0.0.0/8"}, target: "http://docker/"},
	} {
		collector := testutil.Collector("api", "text")
		collector.Request.Own = HTTPConfig{UnixSocket: socket}
		collector.Request.AllowedTargets, collector.Request.DeniedTargets = c.allowed, c.denied
		if err := ValidateRequest(&collector); err != nil {
			t.Fatal(err)
		}
		before := len(seen())
		_, err := FetchCollector(context.Background(), c.target, &collector, RequestOverrides{}, nil)
		if refused := errors.Is(err, ErrTargetRefused); refused != c.refused {
			t.Errorf("%s: err = %v, want refused %v", name, err, c.refused)
		}
		if asked := len(seen()) - before; asked != map[bool]int{true: 0, false: 1}[c.refused] {
			t.Errorf("%s: the socket was asked %d times", name, asked)
		}
	}
}

// A failure to reach the socket names its path in the error the log has,
// and the probe's caller is told it withheld, with the host it named.
func TestTheCallerIsNotToldTheUnixSocketsPath(t *testing.T) {
	proxyEnvironment(t)
	dir, err := os.MkdirTemp("", "us")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "missing.sock")
	c := socketCollector(t, "docker", socket)
	_, err = FetchCollector(context.Background(), "http://docker.example/info", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), socket) {
		t.Fatalf("err = %v, want it to name the socket for the log", err)
	}
	told := model.CallerText(err)
	if strings.Contains(told, socket) || strings.Contains(told, dir) || !strings.Contains(told, model.WithheldMark) || !strings.Contains(told, "docker.example") {
		t.Fatalf("the caller is told %q", told)
	}
}

// request.unix_socket is checked when the configuration loads: an absolute
// path, with nothing a probe could fill in, and no proxy beside it.
func TestAUnixSocketIsCheckedWhenItLoads(t *testing.T) {
	for name, c := range map[string]struct {
		socket string
		proxy  string
		files  *model.ProxyBasicAuthFile
		want   string
	}{
		"blanks":            {socket: "  ", want: "request.unix_socket is nothing but blanks"},
		"relative":          {socket: "run/docker.sock", want: `request.unix_socket "run/docker.sock" is not an absolute path`},
		"dot relative":      {socket: "./docker.sock", want: "is not an absolute path"},
		"a placeholder":     {socket: "/run/{{param_name}}.sock", want: "has a placeholder"},
		"a line break":      {socket: "/run/docker.sock\n", want: "has a control character"},
		"a proxy":           {socket: "/run/docker.sock", proxy: "http://proxy.example:3128", want: "sets request.unix_socket and request.proxy_url"},
		"direct":            {socket: "/run/docker.sock", proxy: model.ProxyDirect, want: "sets request.unix_socket and request.proxy_url"},
		"proxy credentials": {socket: "/run/docker.sock", proxy: "http://proxy.example:3128", files: &model.ProxyBasicAuthFile{Username: "/u", Password: "/p"}, want: "sets request.unix_socket and request.proxy_url"},
	} {
		collector := testutil.Collector("api", "text")
		collector.Request.Own = HTTPConfig{UnixSocket: c.socket}
		collector.Request.ProxyURL, collector.Request.ProxyBasicAuthFile = c.proxy, c.files
		if err := ValidateRequest(&collector); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	for _, accepted := range []string{"", "/var/run/docker.sock", "/run/php/php-fpm.sock"} {
		collector := testutil.Collector("api", "text")
		collector.Request.Own = HTTPConfig{UnixSocket: accepted}
		if err := ValidateRequest(&collector); err != nil {
			t.Errorf("%q: %v", accepted, err)
		}
	}
}

// No probe parameter chooses the socket: a unix_socket parameter is none
// of an http collector's, and the request goes to the socket the
// configuration names.
func TestAProbeCannotChooseTheUnixSocket(t *testing.T) {
	proxyEnvironment(t)
	socket, seen := socketServer(t, nil, nil)
	other, otherSeen := socketServer(t, nil, nil)
	c := socketCollector(t, "docker", socket)
	values := url.Values{"unix_socket": {other}}
	if err := CheckOverrideParams(c, values); err != nil {
		t.Fatal(err)
	}
	overrides, err := ParseRequestOverrides(values)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), "http://docker.example/info", c, overrides, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen()) != 1 || len(otherSeen()) != 0 {
		t.Fatalf("the configured socket was asked %d times and the probe's %d, want 1 and 0", len(seen()), len(otherSeen()))
	}
}
