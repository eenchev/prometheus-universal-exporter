package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A mapping key that is a sequence or a mapping, or an alias of one, is
// refused in words that say so, at its line, in the configuration, a
// collector file (named, as the loader names it in each of its errors) and
// the static target file alike: alone, two of one mapping (which the decoder
// took for one key written twice), in a block merged in, in an x- block
// nobody merges, and under --config.expand-env. Two of a kind on one line
// are said once; keys on several lines each at its own.
func TestAMappingKeyThatIsNoScalarIsRefusedInItsOwnWords(t *testing.T) {
	const words = "only scalar keys (text, numbers, booleans, null) can be read"
	collector := "collectors:\n  - name: a\n    request: {type: http}\n    transform: {type: regex}\n    metrics: [{name: x, expression: 'v=(\\d+)'}]\n"
	withHeaders := func(headers string) string {
		return strings.Replace(collector, "{type: http}", "{type: http, headers: "+headers+"}", 1)
	}
	targets := "interval: 1m\ntargets:\n  - name: a\n    collector: c\n    target: http://x\n"
	for name, test := range map[string]struct {
		file, document, want string
	}{
		"one sequence":          {"config", "? [a]\n: 1\n" + collector, "line 1: a mapping key is a sequence; " + words},
		"two sequences":         {"config", "? [a]\n: 1\n? [b]\n: 2\n" + collector, "line 1: a mapping key is a sequence; " + words + "; line 3: a mapping key is a sequence; " + words},
		"a mapping":             {"config", "? {a: 1}\n: 1\n" + collector, "line 1: a mapping key is a mapping; " + words},
		"an alias of a list":    {"config", "x-s: &s [a]\n*s : 1\n" + collector, "line 2: a mapping key is an alias of a sequence; " + words},
		"an alias of a map":     {"config", "x-s: &s {a: 1}\n" + withHeaders("{*s : v}"), "line 4: a mapping key is an alias of a mapping; " + words},
		"in a header":           {"config", withHeaders("{? [k] : v}"), "line 3: a mapping key is a sequence; " + words},
		"two on one line":       {"config", withHeaders("{? [k] : v, ? [j] : w}"), "line 3: a mapping key is a sequence; " + words},
		"one of each on line":   {"config", withHeaders("{? [k] : v, ? {j: 1} : w}"), "line 3: a mapping key is a sequence; " + words + "; line 3: a mapping key is a mapping; " + words},
		"in a merged block":     {"config", "x-m: &m {? [k] : v}\n" + withHeaders("{<<: *m, A: b}"), "line 1: a mapping key is a sequence; " + words},
		"in a merged collector": {"config", "x-m: &m {? [k] : v}\n" + strings.Replace(collector, "  - name: a\n", "  - <<: *m\n    name: a\n", 1), "line 1: a mapping key is a sequence; " + words},
		"in an unused x- block": {"config", "x-m: {? [k] : v}\n" + collector, "line 1: a mapping key is a sequence; " + words},
		"collector file":        {"collectors", collector + "? [a]\n: 1\n", "collector file %s: line 6: a mapping key is a sequence; " + words},
		"collector file label":  {"collectors", strings.Replace(collector, "{name: x,", "{name: x, labels: [{? [n] : v}],", 1), "collector file %s: line 5: a mapping key is a sequence; " + words},
		"static targets":        {"targets", targets + "    labels: {? [a] : 1, ? [b] : 2}\n", "line 6: a mapping key is a sequence; " + words},
		"static targets top":    {"targets", "? {a: 1}\n: 2\n" + targets, "line 1: a mapping key is a mapping; " + words},
	} {
		t.Run(name, func(t *testing.T) {
			for _, opts := range [][]LoadOption{nil, {WithEnvExpansion(), WithStaticTargetsEnvExpansion()}} {
				var err error
				want := test.want
				switch test.file {
				case "config":
					_, err = Load(testutil.WriteFile(t, "config.yaml", test.document), opts...)
				case "collectors":
					// Read on its own: the configuration listing it could be
					// refused for its own collector in a build without http.
					path := testutil.WriteFile(t, "more.yaml", test.document)
					want = strings.Replace(want, "%s", path, 1)
					_, err = loadCollectorFile(path, opts)
				case "targets":
					_, err = LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", test.document), opts...)
				}
				if err == nil || err.Error() != want {
					t.Fatalf("error %v, want %q", err, want)
				}
			}
		})
	}
}
