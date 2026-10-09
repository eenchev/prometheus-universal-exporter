//go:build !select_request_types || request_type_http

package exporter

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// What the failure log holds for probes of failing targets a caller chose,
// each 8 KiB long, is under 1.5 KiB a probe: neither the target nor the
// line of the request that named it stays with the failure for the hour it
// is remembered. It held some 16 KiB a probe: the key held the target, and
// the collector's name, read from the query, the whole request line. Over
// 1,000 probes, 200 under the race detector.
func TestTheFailureLogHoldsLittleOfProbesOfLongFailingTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("app", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(config.NewManager(cfg, "", logger), "python3", logger)
	handler := server.Handler()
	pad := strings.Repeat("a", 8000)
	probes := alloctest.UnlessRaced(1000, 200)
	for i := range probes {
		query := url.Values{"collector": {"app"}, "target": {target.URL + "/" + strconv.Itoa(i) + pad}}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe?"+query.Encode(), nil))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("probe %d answered %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	var remembered, forgotten runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&remembered)
	server.failures.mu.Lock()
	entries := len(server.failures.entries)
	server.failures.entries = map[string]*failureState{}
	server.failures.mu.Unlock()
	runtime.GC()
	runtime.ReadMemStats(&forgotten)
	if entries != probes {
		t.Fatalf("%d failures are remembered of %d probes", entries, probes)
	}
	held := int64(remembered.HeapAlloc) - int64(forgotten.HeapAlloc)
	if perProbe := held / int64(probes); perProbe > 1536 {
		t.Errorf("the failure log holds %d bytes for each of %d probes of 8 KiB failing targets, want at most 1536", perProbe, probes)
	}
}
