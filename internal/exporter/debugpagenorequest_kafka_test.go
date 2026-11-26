//go:build request_type_kafka

package exporter

import "testing"

// A kafka playground reads a pasted answer, the JSON document the exporter
// makes of one, and connects to no broker.
func TestAKafkaPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: kafka}\ntransform: {type: jq}\nmetrics: [{name: kafka_brokers, expression: '.brokers | length'}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"brokers": [{"id": 1}, {"id": 2}]}`, series: "kafka_brokers 2\n",
	})
	requirePlaygroundRefusesFiles(t, "kafka")
}
