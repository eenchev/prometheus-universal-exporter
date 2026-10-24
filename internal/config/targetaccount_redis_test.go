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

// A static target's own tls block written empty, tls: {}, makes the scrape
// of a redis collector that speaks plaintext speak TLS, verified with the
// system roots, as the collector's own block would; the target's own
// account and block are checked when the file loads, a redis:// target
// with its own block and a username without a password file refused
// naming the target.
func TestAStaticTargetsOwnEmptyTLSBlockMakesARedisScrapeSpeakTLS(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: cache
    request:
      type: redis
    transform: {type: jq}
    metrics: [{name: v, expression: .info}]
`))
	if err != nil {
		t.Fatal(err)
	}
	server := redistest.NewTLS(t)
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", `interval: 1m
targets:
  - name: secure
    collector: cache
    target: `+server.Addr+`
    request:
      tls: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatal(err)
	}
	target := &file.Targets[0]
	if target.Request.TLS == nil || !target.Request.TLS.Set {
		t.Fatalf("tls: {} read as %+v", target.Request.TLS)
	}
	c := &cfg.Collectors[0]
	if label, err := fetch.RequestLabelFor(target.Target, c, fetch.TargetOverrides(target)); err != nil || !strings.HasPrefix(label, "rediss://") {
		t.Errorf("label %q, %v", label, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The stand-in's certificate is not among the system roots.
	if _, err := fetch.FetchCollector(ctx, target.Target, c, fetch.TargetOverrides(target), nil); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+server.Addr) {
		t.Errorf("a TLS server: %v", err)
	}
	for request, want := range map[string]string{
		"{tls: {}}":         `target "plain": target asks for plaintext with redis://, but sets request.tls of its own`,
		"{username: alice}": `target "plain": sets request.username without request.password_file`,
		"{bind_dn: cn=a}":   `target "plain" sets request.bind_dn, which does not apply to collector "cache"`,
	} {
		file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", `interval: 1m
targets:
  - name: plain
    collector: cache
    target: redis://`+server.Addr+`
    request: `+request+`
`))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaticTargetsAgainst(file, cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", request, err, want)
		}
	}
}
