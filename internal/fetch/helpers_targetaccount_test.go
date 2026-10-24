//go:build !select_request_types || request_type_grpc || request_type_sql || request_type_redis || request_type_ldap || request_type_tcp || request_type_http || request_type_graphite

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkTargetRequests checks each static target's request of cases against
// c, as the static target file is checked at startup: "" wants it taken,
// anything else an error holding it.
func checkTargetRequests(t *testing.T, c *model.Collector, cases map[string]struct {
	target  string
	request model.TargetRequestConfig
	want    string
}) {
	t.Helper()
	for name, tc := range cases {
		target := &model.StaticTarget{Name: "x", Collector: c.Name, Target: tc.target, Request: tc.request}
		err := CheckTargetRequest(target, c)
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
