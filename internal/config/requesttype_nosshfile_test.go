//go:build select_request_types && !request_type_sshfile

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the sshfile type refuses a configuration with an sshfile
// collector when it loads — at startup, at a reload and in --dry-run, which
// all load it so — saying the type exists and the build left it out, and
// which tag would include it.
func TestABuildWithoutSSHFileRefusesAnSSHFileCollector(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: remote
    request:
      type: sshfile
      root: /var/lib/app
      path: app.prom
      username: metrics
      private_key_file: /etc/exporter/id_ed25519
      known_hosts_file: /etc/exporter/known_hosts
    transform:
      type: prometheus
`))
	if err == nil {
		t.Fatal("an sshfile collector loaded in a build without the type")
	}
	for _, want := range []string{`collector "remote" uses request.type "sshfile", which this build of the exporter does not include`, "request_type_sshfile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
