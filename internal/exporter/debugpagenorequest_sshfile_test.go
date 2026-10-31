//go:build !select_request_types || request_type_sshfile

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An sshfile playground reads the pasted content and reaches no host; it is
// written without the files a request needs, its key and its known_hosts,
// which it may not name.
func TestAnSSHFilePlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			files := ""
			if source {
				files = ", private_key_file: " + testutil.WriteFile(t, "id", "not a key\n") + ", known_hosts_file: " + testutil.WriteFile(t, "known_hosts", "\n")
			}
			return "name: " + name + "\nrequest: {type: sshfile, root: /srv, path: app.prom, username: metrics" + files + "}\ntransform: {type: prometheus}\n"
		},
		target: func(addr string) string { return addr },
		body:   "pasted_series 2\n", series: "pasted_series 2\n",
	})
	requirePlaygroundRefusesFiles(t, "sshfile")
}
