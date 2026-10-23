//go:build !request_type_redis

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the opt-in redis type, the default build among them,
// refuses a configuration with a redis collector when it loads, saying the
// type exists and is opt-in, and how to build with it: the variable of the
// Makefile and the Dockerfile, and the tag.
func TestABuildWithoutRedisRefusesARedisCollector(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: cache
    request:
      type: redis
      info_sections: [memory]
    transform:
      type: jq
    metrics:
      - name: used_memory_bytes
        expression: .info.memory.used_memory
`))
	if err == nil {
		t.Fatal("a redis collector loaded in a build without the type")
	}
	for _, want := range []string{`collector "cache" uses request.type "redis", which this build of the exporter does not include`, "redis is an opt-in type, which the default build and the published image leave out", "OPT_IN_REQUEST_TYPES=redis", "request_type_redis"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
