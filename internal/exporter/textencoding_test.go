package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Output that still is not valid UTF-8 is repaired, counted and logged, and
// the scrape still parses.
func TestInvalidUTF8IsReplacedNotFatal(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("v=1 caf\xe9\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, regexCollector("broken"))
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	recorder := probeOnce(t, server, "/probe?collector=broken&target="+target.URL, nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `v{who="caf`+"�"+`"} 1`) {
		t.Fatalf("%d %q", recorder.Code, recorder.Body.String())
	}
	if _, err := decode.ParsePrometheusText(recorder.Body.Bytes()); err != nil {
		t.Fatalf("the answer does not parse: %v", err)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_invalid_utf8_total{collector="broken"}`); got != 1 {
		t.Fatalf("counted %v", got)
	}
	if !strings.Contains(logs.String(), "were not valid UTF-8") || !strings.Contains(logs.String(), `"first_metric":"v"`) {
		t.Fatalf("not logged:\n%s", logs.String())
	}
}
