//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A merge key (<<) works in every mapping of the files, the two that check
// their own keys included: a collector's cache and a static target's request.
// What the merge brings in is read as if written out, a key beside it wins,
// and a key the block does not take is refused wherever it came from.

const mergeCollector = `  - name: a
    request: {type: http}
    transform: {type: regex}
    metrics:
      - name: value
        expression: 'v=(\d+)'
`

// cache takes a merge key: one mapping or a list of them.
func TestAMergeKeyWorksInACache(t *testing.T) {
	for name, cache := range map[string]string{
		"one mapping":    "x-cache: &cache {ttl: 5m, stale_if_error: 1h}\ncollectors:\n" + mergeCollector + "    cache:\n      <<: *cache\n      ttl: 1m\n",
		"a list of them": "x-ttl: &ttl {ttl: 1m}\nx-stale: &stale {stale_if_error: 1h}\ncollectors:\n" + mergeCollector + "    cache: {<<: [*ttl, *stale]}\n",
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", cache))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c := cfg.Collectors[0].Cache; time.Duration(c.TTL) != time.Minute || time.Duration(c.StaleIfError) != time.Hour {
			t.Errorf("%s: cache read as %+v", name, c)
		}
	}
	// A key the cache does not take is refused when a merge brings it in.
	bad := "x-cache: &cache {ttl: 5m, stale: 1h}\ncollectors:\n" + mergeCollector + "    cache: {<<: *cache}\n"
	if _, err := Load(testutil.WriteFile(t, "config.yaml", bad)); err == nil || !strings.Contains(err.Error(), `line 1: cache has the unknown key "stale"`) {
		t.Fatalf("a merged unknown key: %v", err)
	}
	// A quoted "<<" is a key like any other, which the cache does not take.
	quoted := "collectors:\n" + mergeCollector + "    cache: {\"<<\": {ttl: 5m}}\n"
	if _, err := Load(testutil.WriteFile(t, "config.yaml", quoted)); err == nil || !strings.Contains(err.Error(), `cache has the unknown key "<<"`) {
		t.Fatalf("a quoted <<: %v", err)
	}
}

// A static target's request takes a merge key, and a path or a body the
// merge supplies replaces the collector's, as one written out does.
func TestAMergeKeyWorksInAStaticTargetsRequest(t *testing.T) {
	document := `x-request: &request {timeout: 5s, headers: {X-Tenant: a}, path: /status, body: ""}
x-retry: &retry {attempts: 2}
interval: 1m
targets:
  - name: merged
    collector: a
    target: http://one.example
    request:
      <<: *request
      timeout: 2s
      retry: *retry
  - name: listed
    collector: a
    target: http://two.example
    request: {<<: [*request]}
  - name: plain
    collector: a
    target: http://three.example
    request: {timeout: 1s}
`
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	merged, listed, plain := file.Targets[0].Request, file.Targets[1].Request, file.Targets[2].Request
	if time.Duration(merged.Timeout) != 2*time.Second || merged.Headers["X-Tenant"] != "a" || merged.Path != "/status" || merged.Retry == nil || *merged.Retry.Attempts != 2 {
		t.Errorf("merged request read as %+v", merged)
	}
	if !merged.PathSet || !merged.BodySet || !listed.PathSet || !listed.BodySet {
		t.Errorf("a merged path and body count as set: merged %v %v, listed %v %v", merged.PathSet, merged.BodySet, listed.PathSet, listed.BodySet)
	}
	if plain.PathSet || plain.BodySet {
		t.Errorf("a request without path and body has them set: %+v", plain)
	}
	for name, test := range map[string]struct{ document, want string }{
		"a merged unknown key":  {"x-request: &request {pth: /status}\ninterval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {<<: *request}\n", `line 1: unknown key "pth" in a static target's request`},
		"an aliased nested key": {"x-retry: &retry {atempts: 2}\ninterval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {retry: *retry}\n", `line 1: unknown key "atempts" in retry`},
	} {
		if _, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", test.document)); err == nil || err.Error() != test.want {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

// A collector file may bring its collectors in with a merge key at its top
// level; what else the merge brings in is refused as written there.
func TestAMergeKeyWorksAtTheTopOfACollectorFile(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "more.yaml", "x-all: &all\n  collectors:\n    - {name: a, request: {type: http}, transform: {type: regex}, metrics: [{name: value, expression: 'v=(\\d+)'}]}\n<<: *all\n")
	cfg, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [more.yaml]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "a" {
		t.Fatalf("collectors %v", testutil.CollectorNames(cfg))
	}
	testutil.WriteIn(t, dir, "more.yaml", "x-all: &all {web: {}}\n<<: *all\ncollectors:\n"+mergeCollector)
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [more.yaml]\n")); err == nil || !strings.Contains(err.Error(), `line 1: "web" is not allowed`) {
		t.Fatalf("a merged key a collector file does not take: %v", err)
	}
}
