//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A prometheus rule without an expression passes on the metric of its name,
// and one whose expression matches that name passes it on too: with the
// same labels each makes every series of the metric, and every scrape of a
// target that has it failed with `duplicate metric series "up"`
// (TestAPatternThatMatchesANamedRulesMetricMakesItsSeriesTwice), the
// configuration's mistake told for ever as a scrape's. The load took the
// two rules; it refuses them now, naming both by their places among the
// collector's, counted from 1, the metric and the expression, and saying
// what to change. It is so whichever of the two is written first, whether
// or not the rule with the expression has the name too, with a key written
// "" for a key left out, and whatever else the rules say. Every name a
// pattern matches is told of; a copy of a rule is told of as the same rule
// and of nothing else; and in a collector file it reads the same.
//
// A rule with an expression that sets a scale, a type or a required label
// and carries on without the series it fails on, under error_mode log or
// ignore, is refused beside the rule of the name as any other is, though a
// scrape of a target those do not apply to passed
// (TestOnlyAPatternOfHistogramsOrSummariesIsTakenBesideANameItMatches).
// The load names what the rule sets and says that the series it does not
// fail on are made by each rule; under error_mode fail, and of every pair
// whose rule sets none of the three, it says that each rule makes every
// series, word for word as it did.
//
// One such rule loads beside the name: a rule whose `type` is `histogram`
// or `summary` and whose error_mode is not fail, which is about the metrics
// of that type alone. `name: up` beside `expression: '.*'` with `type:
// histogram` and `error_mode: ignore` is the metric up and every histogram.
// It loads under the default error_mode, as a summary, written first, with
// a scale or a required label besides, and with another type in the rule
// of the name. With that same type in the rule of the name, and under
// error_mode fail, the pair is refused, in the words either was.
//
// What else is loadable is what the configuration does not decide: a label
// that tells the series apart, a required label among them that the rule of
// the name has not, an expression that does not match the name, a rule that
// exports what it matches under another name, two expressions, and another
// metric exported under the name. A python collector's rules are untouched.
func TestANamedMetricAndAPatternThatMatchesItAreRefusedAtLoad(t *testing.T) {
	said := func(earlier, later, named int, rules ...model.MetricRule) string {
		return namedPatternSaid("node", earlier, later, named, rules)
	}
	up, down := model.MetricRule{Name: "up"}, model.MetricRule{Name: "down"}
	milli := 0.001
	pattern := func(expression string) model.MetricRule { return model.MetricRule{Expression: expression} }
	for name, tc := range map[string]struct {
		document string
		want     []string
	}{
		"a name, and the name with its pattern": {collectorRules("prometheus", "", "      - name: up\n      - name: up\n        expression: '^up$'\n"), []string{said(0, 1, 0, up, pattern("^up$"))}},
		"a name, and a pattern alone":           {collectorRules("prometheus", "", "      - name: up\n      - expression: '^u'\n"), []string{said(0, 1, 0, up, pattern("^u"))}},
		"the pattern first":                     {collectorRules("prometheus", "", "      - expression: up\n      - name: up\n"), []string{said(0, 1, 1, pattern("up"), up)}},
		"a pattern with a backslash":            {collectorRules("prometheus", "", "      - name: up\n      - expression: '^\\w+$'\n"), []string{said(0, 1, 0, up, pattern(`^\w+$`))}},
		`expression: "" and name: ""`:           {collectorRules("prometheus", "", "      - name: up\n        expression: \"\"\n      - name: \"\"\n        expression: '.*'\n"), []string{said(0, 1, 0, up, pattern(".*"))}},
		"what the rules say besides":            {collectorRules("prometheus", "", "      - name: up\n        description: Whether it is up.\n        required: false\n        error_mode: ignore\n        scale: 2\n        type: gauge\n      - expression: '.*'\n        error_mode: fail\n"), []string{said(0, 1, 0, up, pattern(".*"))}},
		"the pattern's scale, under fail":       {collectorRules("prometheus", "", "      - name: up\n      - expression: '.*'\n        scale: 0.001\n        error_mode: fail\n"), []string{said(0, 1, 0, up, pattern(".*"))}},
		"the same labels in another order":      {collectorRules("prometheus", "", "      - name: up\n        labels:\n          - name: service\n            expression: job\n          - name: site\n            value: rack1\n      - expression: '^up$'\n        labels:\n          - name: site\n            value: rack1\n          - name: service\n            expression: job\n            truncate: true\n"), []string{said(0, 1, 0, up, pattern("^up$"))}},
		"every name a pattern matches":          {collectorRules("prometheus", "", "      - expression: '.*'\n      - name: up\n      - name: load\n        expression: '^node_load1$'\n      - name: down\n"), []string{said(0, 1, 1, pattern(".*"), up), said(0, 3, 3, pattern(".*"), up, up, down)}},
		// A rule with an expression that can fail on a series and carry on.
		"a pattern's scale":                   {collectorRules("prometheus", "", "      - name: request_duration_seconds\n      - expression: '.*'\n        scale: 0.001\n"), []string{said(0, 1, 0, model.MetricRule{Name: "request_duration_seconds"}, model.MetricRule{Expression: ".*", Scale: &milli})}},
		"a pattern's type, under ignore":      {collectorRules("prometheus", "", "      - name: up\n      - expression: '^up$'\n        type: gauge\n        error_mode: ignore\n"), []string{said(0, 1, 0, up, model.MetricRule{Expression: "^up$", Type: model.GaugeMetricType, ErrorMode: "ignore"})}},
		"a label the pattern's rule requires": {collectorRules("prometheus", "", "      - name: up\n        labels: [{name: service, expression: job}]\n      - expression: '^up$'\n        labels: [{name: service, expression: job, required: true}]\n"), []string{said(0, 1, 0, up, model.MetricRule{Expression: "^up$", Labels: []model.LabelRule{{Name: "service", Expression: "job", Required: true}}})}},
		"such a rule, written first":          {collectorRules("prometheus", "", "      - expression: '^u'\n        type: counter\n        scale: 2\n        error_mode: LOG\n      - name: load\n        expression: '^node_load1$'\n      - name: up\n        type: counter\n"), []string{said(0, 2, 2, model.MetricRule{Expression: "^u", Type: model.CounterMetricType, Scale: &milli}, up, model.MetricRule{Name: "up", Type: model.CounterMetricType})}},
		// A rule of the histograms, or the summaries, that does not keep to
		// them: with that type in the rule of the name, and under fail.
		"a pattern of histograms, and a name of that type": {collectorRules("prometheus", "", "      - name: request_duration_seconds\n        type: histogram\n      - expression: '.*'\n        type: histogram\n        error_mode: ignore\n"), []string{said(0, 1, 0, model.MetricRule{Name: "request_duration_seconds", Type: model.HistogramMetricType}, model.MetricRule{Expression: ".*", Type: model.HistogramMetricType, ErrorMode: "ignore"})}},
		"a pattern of summaries, under fail":               {collectorRules("prometheus", "", "      - expression: '.*'\n        type: summary\n        error_mode: Fail\n      - name: up\n"), []string{said(0, 1, 1, model.MetricRule{Expression: ".*", Type: model.SummaryMetricType, ErrorMode: "Fail"}, up)}},
		"one such rule beside one that is taken":           {collectorRules("prometheus", "", "      - name: up\n      - expression: '.*'\n        type: histogram\n      - expression: '^u'\n        type: gauge\n"), []string{said(0, 2, 0, up, pattern(".*"), model.MetricRule{Expression: "^u", Type: model.GaugeMetricType})}},
		// A copy is told of as the same rule, and then the pair of the
		// rules it is a copy of.
		"copies of both": {collectorRules("prometheus", "", "      - name: up\n      - expression: '^up'\n      - name: up\n      - expression: '^up'\n"), []string{sameRuleSaid("node", 0, 2, up), sameRuleSaid("node", 1, 3, pattern("^up")), said(0, 1, 0, up, pattern("^up"))}},
		// A rule refused for something else is told that alone.
		"a rule that is refused": {collectorRules("prometheus", "", "      - name: up\n        type: meter\n      - expression: '^up$'\n"), []string{`collector "node" metric "up" has invalid type "meter"`}},
	} {
		if got := problemsOf(t, tc.document); !slices.Equal(got, tc.want) {
			t.Errorf("%s: the load says\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}
	// The message, in full.
	const full = `collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression "^up$" of rule 2 matches that name, and their labels are alike, so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "up", or tell their series apart by a label, as with a static label that has another value in each`
	if got := problemsOf(t, collectorRules("prometheus", "", "      - name: up\n      - name: up\n        expression: '^up$'\n")); len(got) != 1 || got[0] != full {
		t.Errorf("the load says %q\nwant %s", got, full)
	}
	const fullOfThePatternFirst = `collector "node" metrics rule 1 and rule 3 both pass on metric "up": rule 3 passes on the metric of that name, the expression ".*" of rule 1 matches that name, and their labels are alike, so each makes every series of the metric,`
	if got := problemsOf(t, collectorRules("prometheus", "", "      - expression: '.*'\n      - name: load\n        expression: '^node_load1$'\n      - name: up\n")); len(got) != 1 || !strings.HasPrefix(got[0], fullOfThePatternFirst) {
		t.Errorf("the load says %q\nwant %s...", got, fullOfThePatternFirst)
	}
	// And of a rule with an expression that can fail on a series and carry
	// on: its type, with the two types the rules may give the metric; its
	// scale alone; and its type, its scale and the label it requires, with
	// that type in both rules.
	for rules, want := range map[string]string{
		"      - name: up\n      - expression: '^up$'\n        type: gauge\n":                                                  `collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression "^up$" of rule 2 matches that name, and their labels are alike; the type of rule 2 does not keep it from the metric, since every series of the metric that rule 2 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series or, where the two rules give the metric different types, as a metric of inconsistent types; take one of the two out, write the expression so that it does not match "up", or tell their series apart by a label, as with a static label that has another value in each`,
		"      - expression: '.*'\n        scale: 0.001\n        error_mode: ignore\n      - name: request_duration_seconds\n": `collector "node" metrics rule 1 and rule 2 both pass on metric "request_duration_seconds": rule 2 passes on the metric of that name, the expression ".*" of rule 1 matches that name, and their labels are alike; the scale of rule 1 does not keep it from the metric, since every series of the metric that rule 1 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "request_duration_seconds", or tell their series apart by a label, as with a static label that has another value in each`,
		"      - name: up\n        type: counter\n        labels: [{name: zone, expression: zone}]\n      - name: up\n        expression: '^u'\n        type: counter\n        scale: 2\n        labels: [{name: zone, expression: zone, required: true}]\n": `collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression "^u" of rule 2 matches that name, and their labels are alike; the type, the scale and the required label "zone" of rule 2 do not keep it from the metric, since every series of the metric that rule 2 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "up", or tell their series apart by a label, as with a static label that has another value in each`,
		// A rule of the histograms beside a name of that type, in the words
		// of a rule that can fail on a series; and one under error_mode
		// fail, in the words of a rule that makes every series.
		"      - name: latency\n        type: histogram\n      - expression: '.*'\n        type: histogram\n": `collector "node" metrics rule 1 and rule 2 both pass on metric "latency": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike; the type of rule 2 does not keep it from the metric, since every series of the metric that rule 2 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "latency", or tell their series apart by a label, as with a static label that has another value in each`,
		"      - name: up\n      - expression: '.*'\n        type: histogram\n        error_mode: fail\n":     `collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike, so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "up", or tell their series apart by a label, as with a static label that has another value in each`,
	} {
		if got := problemsOf(t, collectorRules("prometheus", "", rules)); len(got) != 1 || got[0] != want {
			t.Errorf("the load says %q\nwant %s", got, want)
		}
	}
	// In a collector file it is told the same way.
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "collectors.yaml", collectorRules("prometheus", "", "      - name: up\n      - expression: '^up$'\n"))
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")); err == nil || !strings.HasSuffix(err.Error(), said(0, 1, 0, up, pattern("^up$"))) {
		t.Errorf("in a collector file: %v", err)
	}

	for name, document := range map[string]string{
		"the metric up, and every histogram":      collectorRules("prometheus", "", "      - name: up\n      - expression: '.*'\n        type: histogram\n        error_mode: ignore\n"),
		"that, under the default error_mode":      collectorRules("prometheus", "", "      - name: up\n      - expression: '.*'\n        type: histogram\n"),
		"every summary, written first":            collectorRules("prometheus", "", "      - expression: '^u'\n        type: summary\n        error_mode: LOG\n      - name: load\n        expression: '^node_load1$'\n      - name: up\n        description: Whether it is up.\n"),
		"the histograms a pattern matches, as up": collectorRules("prometheus", "", "      - name: up\n      - name: up\n        expression: '^u'\n        type: histogram\n"),
		"every histogram, with a scale":           collectorRules("prometheus", "", "      - name: up\n      - expression: '.*'\n        type: histogram\n        scale: 0.001\n"),
		"every histogram that has a label":        collectorRules("prometheus", "", "      - name: up\n        labels: [{name: service, expression: job}]\n      - expression: '.*'\n        type: histogram\n        labels: [{name: service, expression: job, required: true}]\n"),
		"every histogram, and a gauge of a name":  collectorRules("prometheus", "", "      - name: up\n        type: gauge\n        scale: 2\n      - expression: '.*'\n        type: histogram\n"),
		"every histogram, and a summary's name":   collectorRules("prometheus", "", "      - name: rpc_duration_seconds\n        type: summary\n      - expression: '.*'\n        type: histogram\n"),
		"a label of another value in each":        collectorRules("prometheus", "", "      - name: up\n        labels: [{name: copy, value: \"1\"}]\n      - expression: '^up$'\n        labels: [{name: copy, value: \"2\"}]\n"),
		"a label more in the pattern's rule":      collectorRules("prometheus", "", "      - name: up\n      - expression: '.*'\n        labels: [{name: site, value: rack1}]\n"),
		"a label read from another in each":       collectorRules("prometheus", "", "      - name: up\n        labels: [{name: service, expression: job}]\n      - expression: '^up$'\n        labels: [{name: service, expression: instance}]\n"),
		"a required label the name has not":       collectorRules("prometheus", "", "      - name: up\n      - expression: '^up$'\n        labels: [{name: service, expression: job, required: true}]\n"),
		"a pattern that does not match the name":  collectorRules("prometheus", "", "      - name: up\n      - expression: '^node_'\n      - expression: '^UP$'\n      - expression: '^up_'\n"),
		"the pattern's series under another":      collectorRules("prometheus", "", "      - name: up\n      - name: alive\n        expression: '^up$'\n"),
		"another metric under the name":           collectorRules("prometheus", "", "      - name: up\n      - name: up\n        expression: '^node_load1$'\n"),
		"two patterns that may match one metric":  collectorRules("prometheus", "", "      - expression: '^node_'\n      - expression: '^node_cpu'\n"),
		"two patterns of one name":                collectorRules("prometheus", "", "      - expression: '^up$'\n      - name: up\n        expression: '^(up)$'\n"),
		"the rules in two collectors":             collectorRules("prometheus", "", "      - name: up\n") + strings.Replace(strings.TrimPrefix(collectorRules("prometheus", "", "      - expression: '^up$'\n"), "collectors:\n"), "name: node", "name: other", 1),
		"python rules of a name":                  collectorRules("python", "", "      - name: v\n      - name: up\n"),
	} {
		if got := problemsOf(t, document); got != nil {
			t.Errorf("%s: the load says\n%s", name, strings.Join(got, "\n"))
		}
	}
}
