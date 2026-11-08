//go:build select_request_types && !request_type_tcp

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build of select_request_types without the tcp type refuses a
// configuration with a tcp collector when it loads, saying the type exists
// and that the default build includes it, and the tag that adds it.
func TestABuildWithoutTCPRefusesATCPCollector(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: memcached
    request:
      type: tcp
      send: "stats\r\n"
      terminator: "END\r\n"
    transform:
      type: regex
    metrics:
      - name: memcached_curr_connections
        expression: 'STAT curr_connections (\d+)'
`))
	if err == nil {
		t.Fatal("a tcp collector loaded in a build without the type")
	}
	for _, want := range []string{`collector "memcached" uses request.type "tcp", which this build of the exporter does not include`, "the default build includes every type", "request_type_tcp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
