//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The log of an error status shows the start of the body, tidied to one line,
// and the probe's answer does not.
func TestAnErrorStatusLogsTheStartOfTheBody(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html>\n  <body>maintenance until 10:00</body>\n</html>"))
	}))
	t.Cleanup(target.Close)
	server, _ := newCacheTestServer(t, testutil.Collector("down", "text"))
	server.logger = slog.Default()
	response := probeOnce(t, server, "/probe?collector=down&target="+url.QueryEscape(target.URL), nil)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "maintenance") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if !strings.Contains(logs.String(), `"response_body":"<html> <body>maintenance until 10:00</body> </html>"`) {
		t.Fatalf("the body is not in the log:\n%s", logs)
	}
}
