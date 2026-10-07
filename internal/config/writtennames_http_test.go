//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector's name_escaping says what a scrape does with a metric or a
// label name that is not classic: fail, the default, fails the scrape, and
// underscores and values export the name escaped. A name the configuration
// writes itself was held to the classic names under all three, so a rule
// named http.server.duration could not be written at all, where the same
// name from a target, a script or a pre-script was exported as
// http_server_duration. Each name a collector writes is held to what a
// scrape holds it to: a rule's name and its labels' names, under every
// transform, the keys of transform.labels, and what transform.rename and
// transform.rename_labels rename to. Under fail each is refused at the load,
// as it was, and told what the scrape's refusal tells; under the other two
// each loads.
func TestANameTheConfigurationWritesFollowsNameEscaping(t *testing.T) {
	const orEscape, escape = ", or set the collector's name_escaping to underscores or values to export it escaped", "; set the collector's name_escaping to underscores or values to export it escaped"
	const notAName = " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit"
	const belongs = "; a pattern to match the target's metric names by is a prometheus rule's expression, not its name, so if this is one, write it as expression, in single quotes, and leave name out, or set name to the one name the series it matches are to be exported under"
	label := "        labels:\n          - name: service.name\n            expression: .service\n"
	for name, tc := range map[string]struct {
		transform, collector, rules string
		refused                     string
	}{
		"a jq rule's name":                              {"jq", "", "      - name: http.server.duration\n        expression: .duration\n", `collector "node" metric "http.server.duration": "http.server.duration"` + notAName + orEscape},
		"a jq rule's label":                             {"jq", "", "      - name: duration\n        expression: .duration\n" + label, `collector "node" metric "duration" has invalid label name "service.name"` + escape},
		"a csv rule's name":                             {"csv", "", "      - name: disk.used\n        expression: used\n", `collector "node" metric "disk.used": "disk.used"` + notAName + orEscape},
		"a regex rule's name":                           {"regex", "", "      - name: jobs.queued\n        expression: 'queued=(\\d+)'\n", `collector "node" metric "jobs.queued": "jobs.queued"` + notAName + orEscape},
		"a prometheus rule's name":                      {"prometheus", "", "      - name: http.server.duration\n", `collector "node" metric "http.server.duration": "http.server.duration"` + notAName + orEscape + belongs},
		"a prometheus rule's name beside an expression": {"prometheus", "", "      - name: rpc.duration\n        expression: '^http\\.'\n        labels:\n          - name: svc.name\n            expression: service.name\n", `collector "node" metric "rpc.duration" has invalid label name "svc.name"` + escape},
		"a python rule's name and label":                {"python", "", "      - name: http.server.duration\n        labels:\n          - name: service.name\n            expression: x\n            truncate: true\n", `collector "node" metric "http.server.duration" has invalid label name "service.name"` + escape},
		"a key of transform.labels":                     {"jq", "", "      - name: up\n        expression: .up\n    transform_labels: {deployment.env: prod}\n", `collector "node" transform.labels has invalid label name "deployment.env"` + escape},
	} {
		for _, escaping := range []string{"", "fail", "underscores", "values"} {
			keys := tc.collector
			if escaping != "" {
				keys += "    name_escaping: " + escaping + "\n"
			}
			document := collectorRules(tc.transform, keys, tc.rules)
			// The settings of the transform stand in its block.
			if before, setting, moved := strings.Cut(document, "    transform_labels: "); moved {
				document = strings.Replace(before, "    transform:\n", "    transform:\n      labels: "+setting, 1)
			}
			got := problemsOf(t, document)
			switch {
			case escaping == "underscores" || escaping == "values":
				if got != nil {
					t.Errorf("%s under name_escaping %s: the load says %q\n%s", name, escaping, got, document)
				}
			case !slices.Equal(got, []string{tc.refused}):
				t.Errorf("%s under name_escaping %q: the load says %q\nwant %s\n%s", name, escaping, got, tc.refused, document)
			}
		}
	}
	// What a pass-through renames to.
	passthrough := "collectors:\n  - name: node\n%s    request:\n      type: http\n    transform:\n      type: prometheus\n      rename: {http.server.duration: rpc.duration}\n      rename_labels: {service.name: svc.name}\n"
	want := []string{`collector "node" transform.rename "http.server.duration" to "rpc.duration": "rpc.duration"` + notAName + orEscape, `collector "node" transform.rename_labels "service.name" to invalid label name "svc.name"` + escape}
	for escaping, refused := range map[string][]string{"": want, "    name_escaping: fail\n": want, "    name_escaping: underscores\n": nil, "    name_escaping: values\n": nil} {
		if got := problemsOf(t, strings.Replace(passthrough, "%s", escaping, 1)); !slices.Equal(got, refused) {
			t.Errorf("a pass-through's renames, %q: the load says %q\nwant %q", escaping, got, refused)
		}
	}

	// A name of nothing but blanks is no name under any of the three, and
	// one underscores would export beginning with "__" is refused as a name
	// Prometheus reserves, under underscores alone: values writes U__ before
	// every name it escapes.
	for _, tc := range []struct{ transform, escaping, rules, refused string }{
		{"prometheus", "underscores", "      - name: \" \"\n        expression: '^up$'\n", `collector "node" metrics rule 1: " "` + notAName},
		{"prometheus", "values", "      - name: \"  \"\n        expression: '^up$'\n", `collector "node" metrics rule 1: "  "` + notAName},
		{"jq", "underscores", "      - name: \" \"\n        expression: .v\n", `collector "node" metrics rule 1 has no name`},
		{"jq", "values", "      - name: v\n        expression: .v\n        labels:\n          - name: \" \"\n            value: x\n", `collector "node" metric "v" has a label without a name`},
		{"jq", "underscores", "      - name: 1_min.load\n        expression: .v\n", `collector "node" metric "1_min.load": "1_min.load" is exported as "__min_load" under name_escaping underscores, which starts with "__", which Prometheus reserves`},
		{"jq", "values", "      - name: 1_min.load\n        expression: .v\n", ""},
		{"jq", "underscores", "      - name: __up\n        expression: .v\n", `collector "node" metric "__up": "__up" starts with "__", which Prometheus reserves`},
		{"jq", "values", "      - name: __up\n        expression: .v\n", `collector "node" metric "__up": "__up" starts with "__", which Prometheus reserves`},
		{"jq", "underscores", "      - name: v\n        expression: .v\n        labels:\n          - name: ..site\n            value: x\n", `collector "node" metric "v": label name "..site" is exported as "__site" under name_escaping underscores, which starts with __, which Prometheus reserves for its own labels`},
		{"jq", "values", "      - name: v\n        expression: .v\n        labels:\n          - name: ..site\n            value: x\n", ""},
		{"jq", "values", "      - name: v\n        expression: .v\n        labels:\n          - name: __site\n            value: x\n", `collector "node" metric "v": label name "__site" starts with __, which Prometheus reserves for its own labels`},
	} {
		var want []string
		if tc.refused != "" {
			want = []string{tc.refused}
		}
		if got := problemsOf(t, collectorRules(tc.transform, "    name_escaping: "+tc.escaping+"\n", tc.rules)); !slices.Equal(got, want) {
			t.Errorf("%s rules %q under name_escaping %s: the load says %q\nwant %q", tc.transform, tc.rules, tc.escaping, got, want)
		}
	}

	// Two rules whose names differ as they are written and are one name
	// once escaped are not held against each other at the load, which
	// compares what is written: they load, and each scrape that has both
	// series fails as one of duplicate series, or of a metric of two types,
	// as it does of two such names of a target.
	for _, rules := range []string{
		"      - name: a.b\n        expression: .v\n      - name: a_b\n        expression: .v\n",
		"      - name: a.b\n        expression: .v\n      - name: a_b\n        expression: .w\n        type: counter\n",
		"      - name: v\n        expression: .v\n        labels:\n          - name: a.b\n            value: x\n          - name: a_b\n            value: y\n",
	} {
		if got := problemsOf(t, collectorRules("jq", "    name_escaping: underscores\n", rules)); got != nil {
			t.Errorf("rules %q: the load says %q", rules, got)
		}
	}

	// A static target's labels are added to what its collector exported,
	// after the names were escaped, so they are classic names whatever the
	// collector's name_escaping says.
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", collectorRules("jq", "    name_escaping: underscores\n", "      - name: http.server.duration\n        expression: .duration\n")))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - collector: node\n    target: http://a.example\n    labels: {deployment.env: prod}\n"))
	if err == nil {
		err = ValidateStaticTargets(targets)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(targets, cfg)
	}
	if want := `target "node_0" has invalid label name "deployment.env"`; err == nil || err.Error() != want {
		t.Errorf("a static target's label of a collector that escapes names: %v\nwant %s", err, want)
	}
}
