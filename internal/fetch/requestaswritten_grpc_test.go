//go:build !select_request_types || request_type_grpc

package fetch

import (
	"net/http"
	"testing"
)

// A grpc collector's credential is not read for a call that carries an
// authorization of its own, a static target's or a forwarded one, which is
// sent instead: the collector's missing file fails only the calls that
// would have sent it.
func TestGRPCATargetsOwnCredentialDoesNotNeedTheCollectors(t *testing.T) {
	c := grpcCollector()
	c.Request.BearerTokenFile = "/nonexistent/default-token"
	checked := validGRPC(t, c)
	md, err := grpcOutgoing(checked, RequestOverrides{}, http.Header{"Authorization": {"Bearer target-token"}})
	if err != nil {
		t.Fatalf("a call with its own credential: %v", err)
	}
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer target-token" {
		t.Fatalf("the call carries authorization %v", got)
	}
	if _, err := grpcOutgoing(checked, RequestOverrides{}, http.Header{"X-Tenant": {"acme"}}); err == nil {
		t.Fatal("a call without a credential of its own did not need the collector's file")
	}
}

// A default in request.message that its filter refuses, or in a metadata
// value that a header cannot hold, is refused when the configuration loads.
func TestGRPCAnUnusableDefaultIsRefusedAtLoad(t *testing.T) {
	c := grpcCollector()
	c.Request.Message = `{"queue": {{param_queue|json}}, "limit": {{param_limit:ten|number}}}`
	wantRefused(t, c, `request.message: param_limit must be a number for its |number filter, not "ten"; that value is the placeholder's default`)
	c = grpcCollector()
	c.Request.Metadata = map[string]string{"x-tenant": "{{param_tenant:a\nb}}"}
	wantRefused(t, c, "request.metadata.x-tenant")
	c = grpcCollector()
	c.Request.Message = `{"queue": {{param_queue:orders|json}}}`
	validGRPC(t, c)
}
