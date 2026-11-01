//go:build !select_request_types || request_type_grpc

package exporter

import "testing"

// A grpc collector's message holding a credential is withheld whole.
func TestTheDebugPageWithholdsAGRPCMessageHoldingACredential(t *testing.T) {
	requireFreeTextWithheld(t, `  - name: message
    request:
      type: grpc
      rpc: rooms.Rooms/List
      descriptors: reflection
      message: '{"api_key": "s3cret-message"}'
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
`)
}
