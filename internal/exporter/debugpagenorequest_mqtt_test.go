//go:build request_type_mqtt

package exporter

import "testing"

// An mqtt playground reads a pasted document of messages and connects to
// no broker.
func TestAnMQTTPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: mqtt, topics: [sensors/a]}\ntransform: {type: jq}\nmetrics: [{name: mqtt_messages, expression: .messages | length}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"messages": [{"topic": "sensors/a", "payload": "1"}]}`, series: "mqtt_messages 1\n",
	})
	requirePlaygroundRefusesFiles(t, "mqtt")
}
