package exporter

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestProbesReuseTheirConnection(t *testing.T) {
	target, conns := countingServer(t, false)
	server := verboseServer(t, false, testutil.Collector("reused", "text"))
	for i := 0; i < 5; i++ {
		if recorder := probeOnce(t, server, "/probe?collector=reused&target="+url.QueryEscape(target.URL), nil); recorder.Code != http.StatusOK {
			t.Fatalf("probe %d: %d %s", i, recorder.Code, recorder.Body)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("five probes opened %d connections, want 1", got)
	}
}

// Over HTTPS, one handshake serves every probe.
func TestHTTPSProbesReuseTheirConnection(t *testing.T) {
	target, conns := countingServer(t, true)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testutil.Collector("secure", "text")
	c.Request.TLS.CAFile = ca
	c.Request.AllowedSchemes = []string{"https"}
	server := verboseServer(t, false, c)
	for i := 0; i < 5; i++ {
		if recorder := probeOnce(t, server, "/probe?collector=secure&target="+url.QueryEscape(target.URL), nil); recorder.Code != http.StatusOK {
			t.Fatalf("probe %d: %d %s", i, recorder.Code, recorder.Body)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("five HTTPS probes opened %d connections, want 1", got)
	}
}

// OTLP exports reuse their connection to the collector too.
func TestOTLPExportsReuseTheirConnection(t *testing.T) {
	endpoint, conns := countingServer(t, false)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint.URL + "/v1/metrics")}
	server := newStaticServer(t, cfg, nil)
	for i := 0; i < 3; i++ {
		server.exportOTLP(context.Background(), 5*time.Second)
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("three exports opened %d connections, want 1", got)
	}
}
