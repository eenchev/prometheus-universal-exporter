package fetch

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// retry.statuses is refused for every request type built that is not
// retried by HTTP status, in a collector and in a static target: grpc,
// which retries by code, in the words retry.codes is refused in for the
// other types, and the types without retries at all, as any retry key is.
func TestRetryStatusesAreRefusedForTypesNotRetriedByHTTPStatus(t *testing.T) {
	for name, rt := range RequestTypes {
		if slices.Contains(rt.Fields, "retry") && !rt.StatusCodes {
			continue
		}
		want := `sets request.retry, which does not apply to request.type "` + name + `"`
		if rt.StatusCodes {
			want = `sets request.retry.statuses, which does not apply to request.type "` + name + `"; retry.codes says which gRPC status codes are retried`
		}
		for _, statuses := range [][]string{{"503"}, {}} {
			c := model.Collector{Name: "c", Request: model.RequestConfig{Type: name, Retry: model.RetryConfig{Statuses: statuses}}}
			if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s, statuses %q: err=%v, want %q", name, statuses, err, want)
			}
			// A static target's is refused alike, whatever its collector.
			collector := model.Collector{Name: "c", Request: model.RequestConfig{Type: name}}
			target := &model.StaticTarget{Name: "t", Collector: "c", Request: model.TargetRequestConfig{Retry: &model.TargetRetryConfig{Statuses: statuses}}}
			wantTarget := `target "t" sets request.retry`
			if err := CheckTargetRequest(target, &collector); err == nil || !strings.Contains(err.Error(), wantTarget) {
				t.Errorf("a static target of %s, statuses %q: err=%v, want %q", name, statuses, err, wantTarget)
			}
		}
	}
}
