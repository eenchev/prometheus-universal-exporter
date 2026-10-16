//go:build !select_request_types || request_type_graphite

package config

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// graphiteCollectorWith is a configuration of one graphite collector, g,
// with request settings put in.
func graphiteCollectorWith(request string) string {
	return "collectors:\n  - name: g\n    request:\n      type: graphite\n      targets: [app.requests]\n" + request +
		"    transform:\n      type: jq\n    metrics:\n      - name: requests\n        items: .series[]\n        expression: .value\n"
}

// A graphite collector's request is an http request but for its URL, and
// is held to the same checks at load: an inline credential with a control
// character, a forward_headers entry never forwarded and a query name of
// nothing are refused, and so is a static target whose port is 0 or past
// 65535.
func TestAGraphiteCollectorIsHeldToTheChecksOfAnHTTPRequest(t *testing.T) {
	for name, test := range map[string]struct{ request, want string }{
		"a bearer token with a line feed": {"      bearer_token: \"secret\\n\"\n", `collector "g" request.bearer_token has the control character '\n' in its value, which a header may not hold` + credentialAdvice},
		"Host forwarded":                  {"      forward_headers: [Host]\n", `collector "g" request.forward_headers has "Host", which is never forwarded`},
		"a query name of nothing":         {"      query: {\"\": v}\n", `collector "g" request.query has a parameter without a name`},
		"what loads":                      {"      bearer_token: secret\n      forward_headers: [X-Tenant]\n      query: {a: v}\n", ""},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", graphiteCollectorWith(test.request)))
		checkRefusal(t, name, err, test.want)
	}
	configuration := graphiteCollectorWith("")
	for target, want := range map[string]string{
		"http://graphite.internal:99999": `target "t": has the port "99999" in its target URL; want a number from 1 to 65535`,
		"http://graphite.internal:8080":  "",
	} {
		err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: g\n    target: '"+target+"'\n")
		checkRefusal(t, target, err, want)
	}
}
