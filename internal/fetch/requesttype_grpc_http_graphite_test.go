//go:build !select_request_types || (request_type_http && request_type_graphite && request_type_grpc)

package fetch

import (
	"net/url"
	"strings"
	"testing"
)

// The probe parameters a grpc collector takes, and those it refuses.
func TestGRPCProbeParameters(t *testing.T) {
	c := validGRPC(t, grpcCollector())
	if err := CheckOverrideParams(c, url.Values{"message": {"{}"}, "timeout": {"1s"}, "retry_attempts": {"1"}, "param_q": {"x"}, "header_x": {"y"}}); err != nil {
		t.Fatal(err)
	}
	err := CheckOverrideParams(c, url.Values{"method": {"GET"}, "path": {"/x"}, "body": {"b"}, "from": {"-1h"}})
	if err == nil || !strings.Contains(err.Error(), "body, from, method, path do not apply") {
		t.Fatalf("%v", err)
	}
	overrides, err := ParseRequestOverrides(url.Values{"message": {`{"queue": "a"}`}})
	if err != nil || overrides.Message == nil || *overrides.Message != `{"queue": "a"}` {
		t.Fatalf("%+v %v", overrides, err)
	}
}
