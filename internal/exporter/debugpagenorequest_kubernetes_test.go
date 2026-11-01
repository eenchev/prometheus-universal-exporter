//go:build !select_request_types || request_type_kubernetes

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A kubernetes playground reads the pasted objects and lists none from its
// request.api_server; it is written without the credential a request
// needs, a file it may not name.
func TestAKubernetesPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, addr string, source bool) string {
			token := ""
			if source {
				token = ", bearer_token_file: " + testutil.WriteFile(t, "token", "t\n")
			}
			return "name: " + name + "\nrequest: {type: kubernetes, resource: pods, all_namespaces: true, api_server: 'https://" + addr + "'" + token + "}\ntransform: {type: jq}\nmetrics: [{name: kube_pods, expression: '.items | length'}]\n"
		},
		target: func(string) string { return "" },
		body:   `{"items": [{}, {}]}`, series: "kube_pods 2\n",
	})
	requirePlaygroundRefusesFiles(t, "kubernetes")
}
