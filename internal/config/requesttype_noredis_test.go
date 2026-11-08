//go:build select_request_types && !request_type_redis

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build of select_request_types without the redis type refuses a
// configuration with a redis collector when it loads, saying the type exists
// and that the default build includes it, and the tag that adds it.
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
	for _, want := range []string{`collector "cache" uses request.type "redis", which this build of the exporter does not include`, "the default build includes every type", "request_type_redis"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
