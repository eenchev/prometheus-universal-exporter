//go:build !select_request_types || (request_type_http && request_type_graphite)

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// request.max_response_bytes alone raises the limit past the default: the
// configuration no longer fills limits.max_response_bytes in, and a negative
// value is refused for every type.
func TestRequestMaxResponseBytesCanRaiseTheLimit(t *testing.T) {
	c := model.Collector{Name: "big", Request: model.RequestConfig{Type: RequestTypeHTTP, MaxResponseBytes: 50 << 20}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if got := responseLimit(&c); got != 50<<20 {
		t.Fatalf("limit %d, want 50 MiB", got)
	}
	c.Limits.MaxResponseBytes = 20 << 20
	if got := responseLimit(&c); got != 20<<20 {
		t.Fatalf("limit %d, want the smaller, 20 MiB", got)
	}
	for _, requestType := range []string{RequestTypeHTTP, RequestTypeGraphite} {
		negative := model.Collector{Name: "n", Request: model.RequestConfig{Type: requestType, MaxResponseBytes: -1}}
		if requestType == RequestTypeGraphite {
			negative.Request.Targets = []string{"a.b"}
		}
		if err := ValidateRequest(&negative); err == nil || !strings.Contains(err.Error(), "max_response_bytes must not be negative") {
			t.Errorf("%s: %v", requestType, err)
		}
	}
}
