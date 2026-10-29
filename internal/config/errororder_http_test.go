//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The problems of a block written on one line are said in the order of the
// file. The decoder says a value of the wrong kind with its line alone, and
// a message was placed where the mapping around it began, so that on one
// line an unknown key after it, or before it in another block, came out of
// order: the defaults block, a collector's request block and the otlp block
// each said a value's mistake before the unknown keys written before it, or
// the other way round. Each value is now placed at its own key. Each
// document is loaded many times, so that an order that depends on anything
// but the file shows.
func TestTheProblemsOfOneLineAreSaidInTheOrderOfTheFile(t *testing.T) {
	collector := "collectors:\n  - {name: d, request: {type: http%s}, transform: {type: regex}, metrics: [{name: v, expression: '(1)'}]}\n"
	for name, test := range map[string]struct {
		document string
		order    []string
	}{
		"a defaults block": {
			document: "defaults: {bogus1: 1, request: {retry: {backoff: d1, bogus2: 1}, max_response_bytes: s1, bogus3: 1}, limits: {bogus4: 1, script_timeout: d2, max_script_memory: s2, bogus5: 1}, error_handling: {on_fetch_error: [log]}, bogus6: 1}\n" + strings.Replace(collector, "%s", "", 1),
			order: []string{`unknown key "bogus1" in defaults`, `"d1" is not a duration`, `unknown key "bogus2" in retry`, `size "s1" is not`, `unknown key "request.bogus3" in defaults`,
				`unknown key "bogus4" in limits`, `"d2" is not a duration`, `size "s2" is not`, `unknown key "bogus5" in limits`, `expected a single value, not a list`, `unknown key "bogus6" in defaults`},
		},
		"a collector's request block": {
			document: strings.Replace(collector, "%s", ", bogus1: 1, max_response_bytes: s1, retry: {bogus2: 1, backoff: d1}, bogus3: 1, follow_redirects: maybe", 1),
			order:    []string{`unknown key "bogus1" in request`, `size "s1" is not`, `unknown key "bogus2" in retry`, `"d1" is not a duration`, `unknown key "bogus3" in request`, `expected true or false`},
		},
		"the otlp block": {
			document: "otlp: {enabled: true, endpoint: 'http://x/v1/metrics', timeout: d1, bogus1: 1, interval: d2, batch_max_bytes: s1, bogus2: 1}\n" + strings.Replace(collector, "%s", "", 1),
			order:    []string{`"d1" is not a duration`, `unknown key "bogus1" in otlp`, `"d2" is not a duration`, `size "s1" is not`, `unknown key "bogus2" in otlp`},
		},
	} {
		path := testutil.WriteFile(t, "config.yaml", test.document)
		var first string
		for i := range 200 {
			_, err := Load(path)
			if err == nil {
				t.Fatalf("%s: loaded", name)
			}
			said := err.Error()
			if i == 0 {
				first = said
				at := -1
				for _, part := range test.order {
					next := strings.Index(said, part)
					if next <= at {
						t.Errorf("%s: %q is not said after what comes before it in the file:\n%s", name, part, said)
						break
					}
					at = next
				}
				if got, want := strings.Count(said, "; line "), len(test.order)-1; got != want {
					t.Errorf("%s: %d problems are said, want %d:\n%s", name, got+1, want+1, said)
				}
				continue
			}
			if said != first {
				t.Fatalf("%s: loaded again, the problems are said otherwise:\n%s\nfirst:\n%s", name, said, first)
			}
		}
	}
}
