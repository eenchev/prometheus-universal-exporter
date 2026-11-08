//go:build !select_request_types || request_type_redis

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A redis playground reads the pasted answers, the JSON document the
// exporter makes of them, and logs in to no server; it is written without
// the password file its account would need, which it may not name.
func TestARedisPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			password := ""
			if source {
				password = "password_file: " + testutil.WriteFile(t, "password", "pw\n") + ", "
			}
			return "name: " + name + "\nrequest: {type: redis, username: exporter, " + password + "info_sections: [memory]}\ntransform: {type: jq}\nmetrics: [{name: used_bytes, expression: .info.memory.used_memory}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"info": {"memory": {"used_memory": 512}}}`, series: "used_bytes 512\n",
	})
	requirePlaygroundRefusesFiles(t, "redis")
}
