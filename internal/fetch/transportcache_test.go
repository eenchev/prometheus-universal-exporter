package fetch

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Requests made with the same TLS and HTTP/2 settings share one connection
// pool (transport.go).

// countingServer counts the connections made to it.
func countingServer(t *testing.T, tlsServer bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	if tlsServer {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server, &conns
}

func TestTransportsAreSharedBySettings(t *testing.T) {
	cache := NewTransportCache()
	now := time.Now()
	get := func(settings TransportSettings) *http.Transport {
		t.Helper()
		transport, err := cache.get(settings, now)
		if err != nil {
			t.Fatal(err)
		}
		return transport
	}
	plain := get(TransportSettings{})
	if get(TransportSettings{}) != plain {
		t.Fatal("the same settings got two pools")
	}
	if get(TransportSettings{TLS: model.TLSConfig{InsecureSkipVerify: true}}) == plain {
		t.Error("insecure_skip_verify shares a pool with verified requests")
	}
	if get(TransportSettings{EnableHTTP2: true}) == plain {
		t.Error("HTTP/2 shares a pool with HTTP/1.1")
	}
	if plain.IdleConnTimeout <= 0 {
		t.Error("idle connections never time out")
	}
}

// A certificate rotated on disk gets a new pool at the next request.
func TestARotatedCertificateGetsANewTransport(t *testing.T) {
	target, _ := countingServer(t, true)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	write := func(at time.Time) {
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(ca, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(time.Now().Add(-time.Hour))
	cache := NewTransportCache()
	settings := TransportSettings{TLS: model.TLSConfig{CAFile: ca}}
	first, err := cache.get(settings, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write(time.Now())
	second, err := cache.get(settings, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first == second || cache.size() != 1 {
		t.Fatalf("a rotated CA kept its pool (same=%v, cached=%d)", first == second, cache.size())
	}
	// A missing file is still an error, not a cached pool.
	if _, err := cache.get(TransportSettings{TLS: model.TLSConfig{CAFile: ca + ".missing"}}, time.Now()); err == nil {
		t.Fatal("a missing CA file built a transport")
	}
}

// A pool nothing uses is closed and forgotten.
func TestUnusedTransportsAreForgotten(t *testing.T) {
	cache := NewTransportCache()
	start := time.Now()
	if _, err := cache.get(TransportSettings{}, start); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(TransportSettings{EnableHTTP2: true}, start.Add(transportIdleTTL/2)); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(TransportSettings{EnableHTTP2: true}, start.Add(transportIdleTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := cache.size(); got != 1 {
		t.Fatalf("cached=%d after the first pool went unused, want 1", got)
	}
}
