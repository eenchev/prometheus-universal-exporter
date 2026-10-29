//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// saidInOrder checks that err says each of order, after what comes before
// it, and nothing more.
func saidInOrder(t *testing.T, name string, err error, order []string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: loaded", name)
		return
	}
	said, at := err.Error(), -1
	for _, part := range order {
		next := strings.Index(said, part)
		if next <= at {
			t.Errorf("%s: %q is not said after what comes before it in the file:\n%s", name, part, said)
			return
		}
		at = next
	}
	if got, want := strings.Count(said, "; line "), len(order)-1; got != want {
		t.Errorf("%s: %d problems are said, want %d:\n%s", name, got+1, want+1, said)
	}
}

// A cache, tls or failure_backoff block, a collector's or the defaults',
// and a static target's request block say every problem they have, unknown
// keys and values of the wrong kind, in the order of the file, and the
// rest of the file is read on: the collector after them says its unknown
// key too. Each stopped at its first unknown key, saying nothing of the
// values or the keys after it, and the cache and the backoff, saying it
// as an error of their own, ended the reading of the whole file there, as
// a cache or a backoff of one value did.
func TestEveryProblemOfABlockThatChecksItsOwnKeysIsSaid(t *testing.T) {
	rule := "transform: {type: regex}, metrics: [{name: v, expression: '(1)'}]"
	next := "  - {name: e, bogus9: 1}\n"
	for name, test := range map[string]struct {
		document string
		order    []string
	}{
		"a cache": {
			document: "collectors:\n  - {name: d, request: {type: http}, " + rule + ", cache: {bogus1: 1, ttl: d1, bogus2: 1, stale_if_error: [x], revalidate: maybe}}\n" + next,
			order:    []string{`cache has the unknown key "bogus1"`, `"d1" is not a duration`, `cache has the unknown key "bogus2"`, `expected a duration such as 30s, not a list`, `expected true or false`, `unknown key "bogus9" in a collector`},
		},
		"a cache over lines": {
			document: "collectors:\n  - name: d\n    request: {type: http}\n    transform: {type: regex}\n    metrics: [{name: v, expression: '(1)'}]\n    cache:\n      ttl: [x]\n      bogus1: 1\n      revalidate: maybe\n      bogus2: 1\n" + next,
			order:    []string{"line 7: expected a duration such as 30s, not a list", `line 8: cache has the unknown key "bogus1"`, "line 9: expected true or false", `line 10: cache has the unknown key "bogus2"`, `line 11: unknown key "bogus9" in a collector`},
		},
		"a cache of one value": {
			document: "collectors:\n  - {name: d, request: {type: http}, " + rule + ", cache: 5m}\n" + next,
			order:    []string{"cache is a mapping: write cache: {ttl: 5m}", `unknown key "bogus9" in a collector`},
		},
		"a tls block": {
			document: "collectors:\n  - {name: d, request: {type: http, tls: {bogus1: 1, insecure_skip_verify: maybe, bogus2: 1, ca_file: [x]}}, " + rule + "}\n" + next,
			order:    []string{`unknown key "bogus1" in tls`, `expected true or false`, `unknown key "bogus2" in tls`, `expected a single value, not a list`, `unknown key "bogus9" in a collector`},
		},
		"a failure_backoff": {
			document: "collectors:\n  - {name: d, request: {type: http}, " + rule + ", failure_backoff: {bogus1: 1, after: x, initial: d1, bogus2: 1, max: [1]}}\n" + next,
			order:    []string{`failure_backoff has the unknown key "bogus1"`, `expected a whole number`, `"d1" is not a duration`, `failure_backoff has the unknown key "bogus2"`, `expected a duration such as 30s, not a list`, `unknown key "bogus9" in a collector`},
		},
		"a failure_backoff of one value": {
			document: "collectors:\n  - {name: d, request: {type: http}, " + rule + ", failure_backoff: true}\n" + next,
			order:    []string{"failure_backoff is a mapping: write failure_backoff: {}", `unknown key "bogus9" in a collector`},
		},
		"the defaults' blocks": {
			document: "defaults: {cache: {bogus1: 1, ttl: d1, bogus2: 1}, failure_backoff: {bogus3: 1, initial: d2, bogus4: 1}, request: {tls: {bogus5: 1, ca_file: [x], bogus6: 1}}}\ncollectors:\n  - {name: d, request: {type: http}, " + rule + "}\n" + next,
			order: []string{`cache has the unknown key "bogus1"`, `"d1" is not a duration`, `cache has the unknown key "bogus2"`, `failure_backoff has the unknown key "bogus3"`, `"d2" is not a duration`, `failure_backoff has the unknown key "bogus4"`,
				`unknown key "bogus5" in tls`, `expected a single value, not a list`, `unknown key "bogus6" in tls`, `unknown key "bogus9" in a collector`},
		},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", test.document))
		saidInOrder(t, name, err, test.order)
	}
	_, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - {name: a, collector: d, target: http://a.example, request: {bogus1: 1, path: [x], bogus2: 1, headers: x}}\n  - {name: b, bogus9: 1}\n"))
	saidInOrder(t, "a static target's request block", err, []string{`unknown key "bogus1"`, "not a list", `unknown key "bogus2"`, "a mapping", `unknown key "bogus9"`})
}
