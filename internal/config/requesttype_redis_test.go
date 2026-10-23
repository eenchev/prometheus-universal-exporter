//go:build request_type_redis

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A redis collector is answered with a JSON document, so its decoder is
// json without a word, and a decoder or a transform that cannot read JSON
// is refused. Its commands may be written in any case and with arguments
// YAML reads as numbers, each sent as the text it is written as.
func TestARedisCollectorDecodesJSON(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: cache
    request:
      type: redis
      commands:
        - name: recent
          command: [zcount, events, 0, 100]
    transform:
      type: jq
    metrics:
      - name: recent_events
        expression: .commands.recent
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Collectors[0]
	if c.Decoder.Type != "json" || strings.Join(c.Request.Commands[0].Command, " ") != "ZCOUNT events 0 100" || len(cfg.Warnings) != 0 {
		t.Errorf("decoder %q, command %q, warnings %q", c.Decoder.Type, c.Request.Commands[0].Command, cfg.Warnings)
	}
	for transform, want := range map[string]string{"regex": "transforms with regex, which cannot read the JSON a redis collector is answered with", "jq\n    decoder:\n      type: yaml": "decodes with yaml, but a redis collector's answer is JSON"} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: cache
    request:
      type: redis
    transform:
      type: `+transform+`
    metrics:
      - name: v
        expression: .info
`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", transform, err)
		}
	}
}
