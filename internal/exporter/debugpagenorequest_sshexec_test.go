//go:build !select_request_types || request_type_sshexec

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An sshexec playground reads a pasted standard output and runs no command;
// it is written without the files a request needs, which it may not name.
func TestAnSSHExecPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			files := ""
			if source {
				files = ", private_key_file: " + testutil.WriteFile(t, "id", "not a key\n") + ", known_hosts_file: " + testutil.WriteFile(t, "known_hosts", "\n")
			}
			return "name: " + name + "\nrequest: {type: sshexec, command: [uptime], username: metrics" + files + "}\ntransform: {type: regex}\nmetrics: [{name: users, type: gauge, expression: 'users (\\d+)'}]\n"
		},
		target: func(addr string) string { return addr },
		body:   "users 3\n", series: "users 3\n",
	})
	requirePlaygroundRefusesFiles(t, "sshexec")
}
