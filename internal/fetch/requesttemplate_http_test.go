//go:build !select_request_types || request_type_http

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// RequestParams lists each parameter a request takes once, from its path and
// its templated body, headers and query, required when any placeholder naming
// it has no default.
func TestRequestParamsListsWhatAProbeSupplies(t *testing.T) {
	c := &model.Collector{Request: model.RequestConfig{
		Type:    RequestTypeHTTP,
		Path:    "/api/{{param_tenant}}/v{{param_version:2}}",
		Headers: map[string]string{"X-Tenant": "{{param_tenant}}"},
		Query:   map[string]string{"region": "{{param_region:eu}}", "fixed": "yes"}, Own: HTTPConfig{Body: `{"service": {{param_service|json}}, "version": "{{param_version:3}}"}`},
	}}
	got := RequestParams(c)
	want := []RequestParam{
		{Name: "param_region", Default: "eu"},
		{Name: "param_service", Required: true},
		{Name: "param_tenant", Required: true},
		{Name: "param_version", Default: "2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	if params := RequestParams(&model.Collector{Request: model.RequestConfig{Type: RequestTypeHTTP, Path: "/status"}}); len(params) != 0 {
		t.Fatalf("a request without placeholders takes %+v", params)
	}
}
