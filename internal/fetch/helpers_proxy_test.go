//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The proxies a collector's request.proxy_url names in the tests (proxy.go).

// testProxy is a forward proxy: it answers a plain-HTTP request itself, with
// value=7 and the URL it was asked for, and tunnels a CONNECT to the address
// asked for, unless refusal is set, which it answers the CONNECT with
// instead. It records what it was asked and the Proxy-Authorization of each.
type testProxy struct {
	*httptest.Server
	refusal string
	forward http.HandlerFunc
	mu      sync.Mutex
	asked   []string
	auth    []string
}

func (p *testProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.URL.String()
	if r.Method == http.MethodConnect {
		target = r.Host
	}
	p.mu.Lock()
	p.asked = append(p.asked, r.Method+" "+target)
	p.auth = append(p.auth, r.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	if r.Method != http.MethodConnect {
		if p.forward != nil {
			p.forward(w, r)
			return
		}
		_, _ = io.WriteString(w, "value=7\nurl="+target+"\n")
		return
	}
	conn, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	if p.refusal != "" {
		_, _ = io.WriteString(conn, "HTTP/1.1 "+p.refusal+"\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := net.Dial("tcp", r.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = upstream.Close() }()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	pipe(conn, buffered.Reader, upstream)
}

// pipe copies between a client, read through its buffered reader, and
// upstream until either ends.
func pipe(client net.Conn, reader io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (p *testProxy) seen() (asked, auth []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...), append([]string(nil), p.auth...)
}

// newTestProxy starts a testProxy in plain HTTP.
func newTestProxy(t *testing.T) *testProxy {
	t.Helper()
	p := &testProxy{}
	p.Server = httptest.NewServer(p)
	t.Cleanup(p.Close)
	return p
}

// proxyEnvironment points the environment's proxy at a proxy of its own,
// which every test checks was asked nothing, with a pool cache built for the
// test so that its transports read it.
func proxyEnvironment(t *testing.T) *testProxy {
	t.Helper()
	environment := newTestProxy(t)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, environment.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	return environment
}

// unresolvable makes every name but localhost fail to resolve here, as on a
// host whose proxy alone resolves names, and the names in addrs resolve to
// those addresses.
func unresolvable(t *testing.T, addrs map[string]string) {
	t.Helper()
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if addr, ok := addrs[host]; ok {
			return []netip.Addr{netip.MustParseAddr(addr)}, nil
		}
		if host == "localhost" || strings.HasPrefix(host, "127.") {
			return restore(ctx, host)
		}
		return nil, &net.DNSError{Name: host, Err: "no such host", IsNotFound: true}
	}
}

// proxyCredentials writes a username and a password to files and returns
// the proxy_basic_auth_file naming them.
func proxyCredentials(t *testing.T, username, password string) *model.ProxyBasicAuthFile {
	t.Helper()
	dir := t.TempDir()
	files := &model.ProxyBasicAuthFile{Username: filepath.Join(dir, "username"), Password: filepath.Join(dir, "password")}
	for file, value := range map[string]string{files.Username: username, files.Password: password} {
		if err := os.WriteFile(file, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}
