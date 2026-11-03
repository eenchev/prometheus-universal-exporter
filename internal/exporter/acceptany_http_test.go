//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector of request.accept_status: [any] decodes the answer whatever
// its status, a 404, a 503 and a redirect it does not follow among them, and
// its rules read the status as $status: the uptime check that listed
// [1xx, 2xx, 3xx, 4xx, 5xx] for it. A 2xx-only collector fails on the same
// answers in the http_status stage.
func TestAProbeOfAcceptStatusAnyDecodesEveryStatus(t *testing.T) {
	testutil.CaptureLogs(t)
	status := http.StatusOK
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"x": 1}`)
	}))
	defer target.Close()
	collector := func(accepted string) string {
		return "collectors:\n  - name: up\n    request:\n      type: http\n      follow_redirects: false\n" + accepted +
			"    decoder: {type: json}\n    transform: {type: jq}\n    metrics:\n      - name: uptime_status_code\n        expression: $status\n"
	}
	anything, err := config.Load(testutil.WriteFile(t, "config.yaml", collector("      accept_status: [any]\n")))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := config.Load(testutil.WriteFile(t, "config.yaml", collector("")))
	if err != nil {
		t.Fatal(err)
	}
	for _, status = range []int{http.StatusOK, http.StatusFound, http.StatusNotFound, http.StatusServiceUnavailable} {
		got := probeOnce(t, flightServer(t, anything.Collectors...), probePath("up", target.URL, ""), nil)
		if want := fmt.Sprintf("uptime_status_code %d\n", status); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("a %d answer to [any]: %d %s; want %q", status, got.Code, got.Body, want)
		}
		got = probeOnce(t, flightServer(t, plain.Collectors...), probePath("up", target.URL, ""), nil)
		if failed := got.Code == http.StatusBadGateway && strings.Contains(got.Body.String(), "http_status"); failed != (status >= 300) {
			t.Errorf("a %d answer to the 2xx default: %d %s", status, got.Code, got.Body)
		}
	}
}
