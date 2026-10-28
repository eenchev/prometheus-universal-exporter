//go:build !select_request_types || request_type_http

package exporter

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// stubProxy is a forward proxy that answers a plain-HTTP request itself,
// with value=7, and a CONNECT with refusal, recording what it was asked.
type stubProxy struct {
	refusal string
	mu      sync.Mutex
	asked   []string
}

func (p *stubProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	asked := r.URL.String()
	if r.Method == http.MethodConnect {
		asked = r.Host
	}
	p.mu.Lock()
	p.asked = append(p.asked, r.Method+" "+asked)
	p.mu.Unlock()
	if r.Method != http.MethodConnect {
		_, _ = io.WriteString(w, "value=7\n")
		return
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	writer := bufio.NewWriter(conn)
	_, _ = writer.WriteString("HTTP/1.1 " + p.refusal + "\r\nContent-Length: 0\r\n\r\n")
	_ = writer.Flush()
}

func (p *stubProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// A probe of a collector with request.proxy_url is answered with what its
// proxy fetched, the environment's proxy asked nothing; and when the proxy
// refuses the tunnel, the caller is told so without the proxy's words or
// address, which the log has.
func TestAProbeGoesThroughTheCollectorsProxy(t *testing.T) {
	environment := &stubProxy{}
	environmentServer := httptest.NewServer(environment)
	t.Cleanup(environmentServer.Close)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, environmentServer.URL)
	}
	proxy := &stubProxy{refusal: "403 Blocked by rule 12 at proxy-7.corp"}
	proxyServer := httptest.NewServer(proxy)
	t.Cleanup(proxyServer.Close)

	c := testutil.Collector("partner", "text")
	c.Request.ProxyURL = proxyServer.URL
	logs := testutil.CaptureLogs(t)
	server := modeServer(t, c)

	answer := probe(t, server, "http://metrics.partner.example/status", "partner")
	if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), "demo_value 7") {
		t.Fatalf("probe: %d %s", answer.Code, answer.Body)
	}
	if asked := proxy.seen(); len(asked) != 1 || asked[0] != "GET http://metrics.partner.example/status" {
		t.Fatalf("the collector's proxy was asked %q", asked)
	}

	answer = probe(t, server, "https://metrics.partner.example/status", "partner")
	body := answer.Body.String()
	if answer.Code == http.StatusOK || strings.Contains(body, "proxy-7.corp") || strings.Contains(body, strings.TrimPrefix(proxyServer.URL, "http://")) || !strings.Contains(body, "the proxy refused the tunnel to metrics.partner.example:443: "+model.WithheldMark) {
		t.Fatalf("probe: %d %s", answer.Code, body)
	}
	if !strings.Contains(logs.String(), "proxy-7.corp") {
		t.Fatalf("the log does not have the proxy's words: %s", logs)
	}
	if asked := proxy.seen(); len(asked) != 2 || asked[1] != "CONNECT metrics.partner.example:443" {
		t.Fatalf("the collector's proxy was asked %q", asked)
	}
	if asked := environment.seen(); len(asked) != 0 {
		t.Fatalf("the environment's proxy was asked %q", asked)
	}
}
