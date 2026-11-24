//go:build request_type_amqp

package exporter

import "testing"

// An amqp playground reads a pasted answer, the JSON document the exporter
// makes of one, and connects to no broker.
func TestAnAMQPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: amqp, queues: [orders]}\ntransform: {type: jq}\nmetrics: [{name: amqp_queue_messages, expression: '.queues[0].messages'}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"queues": [{"name": "orders", "exists": true, "messages": 42}]}`, series: "amqp_queue_messages 42\n",
	})
	requirePlaygroundRefusesFiles(t, "amqp")
}
