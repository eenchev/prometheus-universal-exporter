//go:build !select_request_types || request_type_snmp

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// An snmp playground reads a pasted document, as the exporter makes one of
// an agent's answers, and sends no request: it is written as a v3 user of
// noAuthNoPriv, which names no file, since a playground may not name the
// community file a v2c collector needs.
func TestAnSNMPPlaygroundMakesNoRequest(t *testing.T) {
	tier.Slow(t)
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			account := "snmp_version: 3, username: exporter, security_level: noAuthNoPriv"
			if source {
				account = "community_file: " + testutil.WriteFile(t, "community", "public\n")
			}
			return "name: " + name + "\nrequest: {type: snmp, " + account + ", get: {sysUpTime: 1.3.6.1.2.1.1.3.0}}\ntransform: {type: jq}\nmetrics: [{name: snmp_uptime_seconds, expression: .get.sysUpTime.value / 100}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"get": {"sysUpTime": {"type": "TimeTicks", "value": 12300}}}`, series: "snmp_uptime_seconds 123\n",
	})
	requirePlaygroundRefusesFiles(t, "snmp")
}
