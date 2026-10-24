//go:build request_type_redis || request_type_ldap || request_type_tcp

package exporter

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// staticServerFromFiles is an exporter of the configuration and the static
// target file written as config and targets, both checked as at startup.
func staticServerFromFiles(t *testing.T, dir, configYAML, targetsYAML string) *Server {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, dir, "config.yaml", configYAML))
	if err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteIn(t, dir, "targets.yaml", targetsYAML)
	file, err := config.ParseStaticTargets(path, []byte(targetsYAML))
	if err != nil {
		t.Fatal(err)
	}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	return server
}

// checkTargetsUp checks the http_exporter_target_up of each static target
// named in want on the static targets endpoint's body.
func checkTargetsUp(t *testing.T, body string, want map[string]string) {
	t.Helper()
	for name, up := range want {
		found := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "http_exporter_target_up{") && strings.Contains(line, `static_target="`+name+`"`) {
				found = true
				if !strings.HasSuffix(line, " "+up) {
					t.Errorf("%s, want target_up %s", line, up)
				}
			}
		}
		if !found {
			t.Errorf("no http_exporter_target_up of %s in\n%s", name, body)
		}
	}
}
