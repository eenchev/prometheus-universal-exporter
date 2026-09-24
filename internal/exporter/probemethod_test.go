package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// /probe only reads: a method other than GET or HEAD is refused with 405 and
// an Allow header, before the target is contacted or anything counted.
func TestProbeRefusesMethodsOtherThanGetAndHead(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("value=7\n"))
	}))
	defer target.Close()
	server, _ := newCacheTestServer(t, testutil.Collector("demo", "text"))
	path := "/probe?collector=demo&target=" + target.URL

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader("value=1")))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /probe status=%d, want 405", method, recorder.Code)
		}
		if allow := recorder.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Fatalf("%s /probe Allow=%q", method, allow)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("a refused method must not reach the target: %d requests", n)
	}
	if stats := selfMetrics(t, server); !strings.Contains(stats, `http_exporter_scrapes_total{collector="demo"} 0`) {
		t.Fatalf("a refused method must not count as a probe:\n%s", stats)
	}

	head := httptest.NewRecorder()
	server.Handler().ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
	if head.Code != http.StatusOK || requests.Load() != 1 {
		t.Fatalf("HEAD should run the probe: status=%d requests=%d", head.Code, requests.Load())
	}
	if get := probeOnce(t, server, path, nil); get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "demo_value 7") {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
}
