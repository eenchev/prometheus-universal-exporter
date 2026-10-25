//go:build request_type_redis

package config

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
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
	command := c.Request.Own.(fetch.RedisConfig).Commands[0].Command
	if c.Decoder.Type != "json" || strings.Join(command, " ") != "ZCOUNT events 0 100" || len(cfg.Warnings) != 0 {
		t.Errorf("decoder %q, command %q, warnings %q", c.Decoder.Type, command, cfg.Warnings)
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

// A tls block written empty, tls: {}, makes a redis scrape of a bare
// host:port speak TLS, verified with the system roots, and its label
// rediss://; it used to go out in plaintext. redis:// with the block is
// refused.
func TestAnEmptyTLSBlockMakesARedisScrapeSpeakTLS(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: cache
    request:
      type: redis
      tls: {}
    transform: {type: jq}
    metrics: [{name: v, expression: .info}]
`))
	if err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	server := redistest.NewTLS(t)
	if label, err := fetch.RequestLabelFor(server.Addr, c, fetch.RequestOverrides{}); err != nil || !strings.HasPrefix(label, "rediss://"+server.Addr) {
		t.Errorf("label %q, %v", label, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The stand-in's certificate is not among the system roots.
	if _, err := fetch.FetchCollector(ctx, server.Addr, c, fetch.RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+server.Addr) {
		t.Errorf("a TLS server: %v", err)
	}
	if _, err := fetch.RequestLabelFor("redis://"+server.Addr, c, fetch.RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "target asks for plaintext with redis://, but the collector sets request.tls") {
		t.Errorf("redis:// with tls: {}: %v", err)
	}
}
