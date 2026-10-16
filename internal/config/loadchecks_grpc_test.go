//go:build !select_request_types || request_type_grpc

package config

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// healthCollectorWith is a configuration of one grpc collector of the health
// service, h, with request settings put in.
func healthCollectorWith(request string) string {
	return "collectors:\n  - name: h\n    request:\n      type: grpc\n      rpc: grpc.health.v1.Health/Check\n" + request +
		"    transform:\n      type: jq\n    metrics:\n      - name: up\n        expression: .status\n"
}

// A grpc collector is held to the checks of an http one where it sends what
// an http one does: an inline credential with a control character, which
// gRPC sends as the authorization metadata and refuses to, is refused when
// the configuration loads, a static target's as a collector's; and so is a
// forward_headers entry a probe never forwards, before the check of it as a
// metadata key, which Authorization passed. A target's port out of range
// was refused already, as it was not host:port.
func TestAGRPCCollectorsCredentialsAndForwardedHeadersAreCheckedAtLoad(t *testing.T) {
	for name, test := range map[string]struct{ request, want string }{
		"a bearer token with a line feed":   {"      bearer_token: \"secret\\n\"\n", `collector "h" request.bearer_token has the control character '\n' in its value, which a header may not hold` + credentialAdvice},
		"a basic password with a line feed": {"      basic_auth: {username: u, password: \"secret\\n\"}\n", `collector "h" request.basic_auth.password has the control character '\n' in its value, which a Basic credential may not hold`},
		"a bearer token":                    {"      bearer_token: secret\n", ""},
		"Authorization forwarded":           {"      forward_headers: [Authorization]\n", `collector "h" request.forward_headers has "Authorization", which is never forwarded: the probe's Authorization is forwarded by request.forward_authorization: true alone`},
		"Host forwarded":                    {"      forward_headers: [host]\n", `collector "h" request.forward_headers has "host", which is never forwarded`},
		"a header gRPC reserves":            {"      forward_headers: [grpc-timeout]\n", `collector "h" request.forward_headers "grpc-timeout": is reserved by gRPC and HTTP/2`},
		"a header forwarded":                {"      forward_headers: [X-Tenant]\n", ""},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", healthCollectorWith(test.request)))
		checkRefusal(t, name, err, test.want)
	}
	configuration := healthCollectorWith("")
	for target, want := range map[string]string{
		"queue.internal:99999": `target "t": target "queue.internal:99999" has the port "99999"; want a number from 1 to 65535`,
		"grpc://q:0":           `has the port "0"`,
		"queue.internal:9090":  "",
	} {
		err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: h\n    target: '"+target+"'\n")
		checkRefusal(t, target, err, want)
	}
	err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: h\n    target: q:9090\n    request: {bearer_token: \"a\\nb\"}\n")
	checkRefusal(t, "a target's bearer token with a line feed", err, `target "t" request.bearer_token has the control character '\n' in its value`)
}
