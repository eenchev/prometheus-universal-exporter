//go:build !select_request_types || request_type_redis

package exporter

import "testing"

// A redis command's argument holding a credential is withheld.
func TestTheDebugPageWithholdsARedisArgumentHoldingACredential(t *testing.T) {
	requireFreeTextWithheld(t, `  - name: argument
    request:
      type: redis
      commands:
        - name: session
          command: [GET, "session:token=s3cret-argument"]
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
`)
}
