//go:build !select_request_types || request_type_dns || request_type_grpc || request_type_ldap || request_type_redis || request_type_s3file || request_type_sql || request_type_sshexec || request_type_sshfile || request_type_tcp || request_type_tlscert || request_type_smtp || request_type_modbus || request_type_mqtt || request_type_websocket || request_type_ftp

package fetch

import "context"

// The request types that tell facts of their requests for their spans
// (TracedRequest.Facts) record them with traceFacts; a build of none of
// them has no use for it.

// traceFacts adds facts to those of the last request recorded
// (TracedRequest.Facts). A debug run keeps them too, for the spans its
// debug page shows.
func traceFacts(ctx context.Context, facts ...TraceFact) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		t.requests[n-1].Facts = append(t.requests[n-1].Facts, facts...)
	}
}
