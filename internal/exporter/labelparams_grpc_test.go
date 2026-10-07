//go:build !select_request_types || request_type_grpc

package exporter

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
)

// A grpc collector's label values take the probe's parameters as its
// message does, by the same rules: one parameter fills the message and the
// labels alike, and the message probe parameter, which replaces the
// message, does not take away a label's use of a parameter.
func TestAGRPCCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: queueAnswer})
	server := labelServer(t, `collectors:
  - name: queue_stats
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": {{param_queue|json}}, "include_shards": true}'
      descriptors: reflection
    transform:
      type: jq
      labels:
        queue: "{{param_queue}}"
        tenant: "{{param_tenant:default}}"
    metrics:
      - name: queue_empty
        expression: .empty_count
        labels:
          - {name: source, value: "grpc-{{param_queue}}"}
`)
	probe := "/probe?collector=queue_stats&target=" + url.QueryEscape(upstream.Addr)
	message := "&message=" + url.QueryEscape(`{"queue": "other", "include_shards": true}`)
	for name, tc := range map[string]struct{ query, want string }{
		"the message and the labels alike": {"&param_queue=orders", `queue_empty{queue="orders",source="grpc-orders",tenant="default"} 0`},
		"a parameter of a label alone":     {"&param_queue=orders&param_tenant=acme", `queue_empty{queue="orders",source="grpc-orders",tenant="acme"} 0`},
		"a message that replaces it":       {message + "&param_queue=orders", `queue_empty{queue="orders",source="grpc-orders",tenant="default"} 0`},
	} {
		got := probeOnce(t, server, probe+tc.query, nil)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{tc.want}) {
			t.Errorf("%s: %d\n%s", name, got.Code, got.Body.String())
		}
	}
	for name, tc := range map[string]struct{ query, want string }{
		"no value beside a message": {message, "transform.labels.queue needs param_queue, which the probe did not supply and which has no default"},
		"a parameter nothing uses":  {"&param_queue=orders&param_tenat=acme", `probe parameters param_tenat are not used by collector "queue_stats": no placeholder in its request.message, its metadata values or its label values names them`},
		"nothing uses, a message":   {message + "&param_queue=orders&param_tenat=acme", "probe parameters param_tenat are not used: the message probe parameter replaces request.message, and nothing else in the request and no label value of the collector names them"},
	} {
		got := probeOnce(t, server, probe+tc.query, nil)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), tc.want) {
			t.Errorf("%s: %d %s\nwant 400 with %s", name, got.Code, got.Body.String(), tc.want)
		}
	}
}
