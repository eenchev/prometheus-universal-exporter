//go:build !request_type_tcp

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the opt-in tcp type, the default build among them,
// refuses a configuration with a tcp collector when it loads, saying the
// type exists and is opt-in, and how to build with it: the variable of the
// Makefile and the Dockerfile, and the tag.
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
	for _, want := range []string{`collector "memcached" uses request.type "tcp", which this build of the exporter does not include`, "tcp is an opt-in type, which the default build and the published image leave out", "OPT_IN_REQUEST_TYPES=tcp", "request_type_tcp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
