package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The self-metrics endpoint is read as /probe and the static targets
// endpoint are: GET and HEAD are answered 200 with the metrics, and POST,
// PUT, PATCH, DELETE and OPTIONS 405 with Allow: GET, HEAD and a line saying
// what to use, as /probe and the static targets endpoint answer them. It
// answered every method 200.
func TestTheSelfMetricsAreReadWithGETOrHEADOnly(t *testing.T) {
	server := NewServer(config.NewManager(&model.Config{}, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	handler := server.Handler()
	serve := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := serve(method, DefaultSelfMetricsPath)
		// The recorder keeps what a HEAD writes, which net/http leaves out.
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "http_exporter_build_info") {
			t.Errorf("%s %s: %d\n%s", method, DefaultSelfMetricsPath, rec.Code, rec.Body)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		rec := serve(method, DefaultSelfMetricsPath)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" || rec.Body.String() != "use GET or HEAD to read the self-metrics\n" {
			t.Errorf("%s %s: %d, Allow %q\n%s", method, DefaultSelfMetricsPath, rec.Code, rec.Header().Get("Allow"), rec.Body)
		}
		for _, path := range []string{"/probe", DefaultStaticTargetsPath} {
			other := serve(method, path)
			if other.Code != rec.Code || other.Header().Get("Allow") != rec.Header().Get("Allow") || other.Header().Get("Content-Type") != rec.Header().Get("Content-Type") || !strings.HasPrefix(other.Body.String(), "use GET or HEAD to ") {
				t.Errorf("%s %s: %d, Allow %q, %q, %q; the self-metrics: %d, Allow %q, %q", method, path, other.Code, other.Header().Get("Allow"), other.Header().Get("Content-Type"), other.Body, rec.Code, rec.Header().Get("Allow"), rec.Header().Get("Content-Type"))
			}
		}
	}
}
