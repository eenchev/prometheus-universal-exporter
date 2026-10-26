//go:build !select_request_types || request_type_grpc

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A grpc collector's request.message is JSON, so of the body's filters it
// takes only those that write JSON. The rule was the shared template code's,
// keyed by the field's place; it is grpc's now, registered for the kind of
// its message field (grpcMessageKind), and holds wherever a message is
// parsed: the request's templated fields, the message a call sends and the
// message the configuration checks. A probe's message replaces the
// collector's, whose placeholders are then not checked.
func TestAMessageTakesOnlyTheFiltersThatWriteJSON(t *testing.T) {
	if templateFilterRules[grpcMessageKind] == nil || templateReplacers[grpcMessageKind] == nil {
		t.Fatal("the grpc request type registers no rule for its message's kind")
	}
	collector := func(message string) *model.Collector {
		return &model.Collector{Name: "queue", Request: model.RequestConfig{Type: RequestTypeGRPC, Own: GRPCConfig{RPC: "acme.queue.v1.QueueService/GetStats", Message: message}}}
	}
	params := map[string]string{"param_q": "7"}
	for filter, refused := range map[string]bool{"": false, "|json": false, "|number": false, "|raw": false, "|form": true, "|xml": true} {
		message := `{"queue": {{param_q` + filter + `}}}`
		c := collector(message)
		var fields []templateField
		for _, f := range grpcTemplates(c, RequestOverrides{}) {
			if f.where == "request.message" {
				fields = append(fields, f)
			}
		}
		if len(fields) != 1 || fields[0].kind != grpcMessageKind {
			t.Fatalf("grpcTemplates gives the message as %v, not one field of the kind %q", fields, grpcMessageKind)
		}
		_, parseErr := fields[0].parse()
		_, sendErr := grpcMessage(c, RequestOverrides{Params: params})
		_, _, checkErr := messageForCheck(message)
		for what, err := range map[string]error{"its templated field": parseErr, "the message sent": sendErr, "the message checked": checkErr} {
			if got := err != nil && strings.Contains(err.Error(), "which does not write JSON; a message takes |json for a string, |number for a number, or |raw"); got != refused {
				t.Errorf("%s of a message with {{param_q%s}}: %v, refused for its filter: %v, want %v", what, filter, err, got, refused)
			}
		}
		// A probe's message replaces the collector's, filter and all.
		replacement := `{"queue": 1}`
		var check RequestParamsCheck
		if _, err := check.CheckRequestParams(c, RequestOverrides{Message: &replacement, Params: map[string]string{}}); err != nil {
			t.Errorf("a probe's message did not replace a message with {{param_q%s}}: %v", filter, err)
		}
		if !fields[0].replacedBy(RequestOverrides{Message: &replacement}) || fields[0].replacedBy(RequestOverrides{}) {
			t.Errorf("the message field is replaced by a probe's message: %v, and by no message: %v", fields[0].replacedBy(RequestOverrides{Message: &replacement}), fields[0].replacedBy(RequestOverrides{}))
		}
	}
}
