package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end, exactly as a chart-generated monitor probes: target is the bare
// host:port of the discovered endpoint.
func TestAProbeWithTheAddressServiceDiscoveryProduces(t *testing.T) {
	testutil.CaptureLogs(t)
	var recorder pathRecorder
	target := recorder.serve(t)
	server := pathServer(t, pathCollector("/status"))
	address := strings.TrimPrefix(target.URL, "http://")

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/probe?collector=tenants&target="+address, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "demo_value 7") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if got := recorder.last(t); got != "/status" {
		t.Fatalf("target received %q", got)
	}
}
