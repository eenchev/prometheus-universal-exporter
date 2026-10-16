//go:build !select_request_types || request_type_sshfile

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// --dry-run reads the files an sshfile collector and a static target of it
// name, which loading leaves to the connection, and warns of each it would
// fail on, the check still passing and logging the warnings: a missing
// private key, a known_hosts file without a line, a target's empty password;
// a target with the collector's files adds none.
func TestDryRunWarnsOfTheFilesOfAnSSHFileCollector(t *testing.T) {
	dir := t.TempDir()
	known := testutil.WriteIn(t, dir, "known_hosts", "# no line yet\n")
	empty := testutil.WriteIn(t, dir, "password", "\n")
	missing := filepath.Join(dir, "id_ed25519")
	config := testutil.WriteIn(t, dir, "config.yaml", `collectors:
  - name: remote
    request:
      type: sshfile
      root: /srv/metrics
      path: app.prom
      username: metrics
      private_key_file: `+missing+`
      known_hosts_file: `+known+`
    transform:
      type: prometheus
`)
	targets := testutil.WriteIn(t, dir, "targets.yaml", `interval: 1m
targets:
  - name: own
    collector: remote
    target: db1.example.com
    request:
      password_file: `+empty+`
  - name: shared
    collector: remote
    target: db2.example.com
`)
	out := runCheckCLI(t, "--config.file="+config, "--static-targets-file="+targets)
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stderr)
	}
	for check, wants := range map[string][]string{
		"config":         {`collector "remote": reading request.private_key_file: open ` + missing, `collector "remote": request.known_hosts_file ` + known + " holds no host key"},
		"static_targets": {`target "own": request.password_file ` + empty + " is empty"},
	} {
		result := out.result(t, check)
		warnings, _ := result.Details["warnings"].([]any)
		if result.Status != checkOK || len(warnings) != len(wants) {
			t.Errorf("%s: %+v, want %d warnings", check, result, len(wants))
			continue
		}
		for i, want := range wants {
			if !strings.HasPrefix(warnings[i].(string), want) {
				t.Errorf("%s warning %d: %q, want %q", check, i, warnings[i], want)
			}
		}
	}
	logged := 0
	for _, record := range out.logs {
		if record["msg"] == "configuration warning" && strings.Contains(record["warning"].(string), "a scrape would fail on it now") {
			logged++
		}
	}
	if logged != 3 {
		t.Errorf("%d warnings logged, want 3:\n%s", logged, out.stderr)
	}
}
