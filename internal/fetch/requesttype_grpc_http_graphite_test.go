//go:build !select_request_types || (request_type_http && request_type_graphite && request_type_grpc)

package fetch

import (
	"net/url"
	"strings"
	"testing"
)

// The probe parameters a grpc collector refuses: those of http and of
// graphite, named together. TestGRPCProbeParameters has those it takes.
func TestGRPCProbeParametersOfOtherTypesAreRefused(t *testing.T) {
	c := validGRPC(t, grpcCollector())
	err := CheckOverrideParams(c, url.Values{"method": {"GET"}, "path": {"/x"}, "body": {"b"}, "from": {"-1h"}})
	if err == nil || !strings.Contains(err.Error(), "body, from, method, path do not apply") {
		t.Fatalf("%v", err)
	}
}
