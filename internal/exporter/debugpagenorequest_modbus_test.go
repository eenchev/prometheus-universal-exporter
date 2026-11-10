//go:build request_type_modbus

package exporter

import "testing"

// A modbus playground reads a pasted document of values and asks no device.
func TestAModbusPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: modbus, read: [{name: volts, address: 0}]}\ntransform: {type: jq}\nmetrics: [{name: modbus_volts, expression: .volts}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"volts": 230}`, series: "modbus_volts 230\n",
	})
	requirePlaygroundRefusesFiles(t, "modbus")
}
