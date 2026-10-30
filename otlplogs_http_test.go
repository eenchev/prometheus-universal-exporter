//go:build !select_request_types || request_type_http

package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The running exporter forwards its own log with otlp.logs: the lines it
// logs from its start are exported in the background, at the logs path of
// the collector the metrics go to, and the lines of its shutdown in the last
// export before it exits. stderr has every line as before.
func TestTheExporterForwardsItsOwnLogOverOTLP(t *testing.T) {
	logs := &syncBuffer{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		if r.URL.Path == "/v1/logs" {
			_, _ = logs.Write(append(body, '\n'))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	p := startHeldExporterWith(t, "otlp:\n  enabled: true\n  endpoint: "+collector.URL+"/v1/metrics\n  logs:\n    enabled: true\n    interval: 1s\n", "--web.shutdown-timeout=1s")
	testutil.WaitFor(t, "the start's line exported", func() bool { return strings.Contains(logs.String(), `"stringValue":"starting exporter"`) })
	p.signal(t, syscall.SIGTERM)
	select {
	case <-p.ended:
	case <-time.After(30 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	exported := logs.String()
	for _, line := range []string{"shutting down: finishing the probes in progress and sending the last OTLP export", "sending the last OTLP export before exiting"} {
		if !strings.Contains(exported, `"stringValue":"`+line) {
			t.Errorf("the line %q of the shutdown was not exported:\n%s", line, exported)
		}
		if !strings.Contains(p.logs.String(), line) {
			t.Errorf("the line %q is not on stderr:\n%s", line, p.logs.String())
		}
	}
}
