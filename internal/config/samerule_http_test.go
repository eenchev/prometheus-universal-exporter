//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// collectorRules is a configuration of one collector, node, of the
// transform, with what else the collector sets, each a line of its own, and
// the rules as they are written, each a line of a list.
func collectorRules(transformType, collector, rules string) string {
	script := ""
	if transformType == "python" {
		script = "      script: metric('v', 'gauge', 1)\n"
	}
	return "collectors:\n  - name: node\n    request:\n      type: http\n" + collector + "    transform:\n      type: " + transformType + "\n" + script + "    metrics:\n" + rules
}

// problemsOf is what loading a configuration says, problem by problem,
// without the file's name before each.
func problemsOf(t *testing.T, document string) []string {
	t.Helper()
	_, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if err == nil {
		return nil
	}
	problems := model.Problems{err}
	var several model.Problems
	if errors.As(err, &several) {
		problems = several
	}
	texts := make([]string, len(problems))
	for i, problem := range problems {
		_, said, _ := strings.Cut(problem.Error(), `collector "`)
		texts[i] = `collector "` + said
	}
	return texts
}

// Two rules of a collector that are the same rule give every series twice,
// and every scrape then fails with `duplicate metric series "up"`
// (TestTheSameRuleTwiceMakesEverySeriesTwice): the configuration's mistake,
// told for ever as a scrape's. The load refuses them, naming both rules by
// their places among the collector's, counted from 1, and the metric, and
// saying to take one out or tell them apart by a label. A key written "" is
// the key left out, and what a rule says besides — scale, description,
// required, error_mode — does not make it another rule. Each copy is told of
// once, against the first; a rule refused for something else is refused for
// that alone; and in a collector file it reads the same.
//
// What stays loadable: rules of one name that differ in expression, items
// or a label, which export one family; rules that read their value from
// other texts; a python rule twice; and the same rule in two collectors.
func TestTwoRulesThatAreTheSameRuleAreRefusedAtLoad(t *testing.T) {
	const jobs = "      - name: jobs\n        items: .queues[]\n        expression: .jobs\n        labels:\n          - name: queue\n            expression: .name\n          - name: site\n            value: rack1\n"
	up, load, every := model.MetricRule{Name: "up"}, model.MetricRule{Name: "load", Expression: "^node_load1$"}, model.MetricRule{Expression: "^node_"}
	for name, tc := range map[string]struct {
		document string
		want     []string
	}{
		"prometheus, a name twice":             {collectorRules("prometheus", "", "      - name: up\n      - name: up\n"), []string{sameRuleSaid("node", 0, 1, up)}},
		"prometheus, rules 1 and 3":            {collectorRules("prometheus", "", "      - name: up\n      - expression: '^node_'\n      - name: up\n"), []string{sameRuleSaid("node", 0, 2, up)}},
		"prometheus, an expression twice":      {collectorRules("prometheus", "", "      - expression: '^node_'\n      - name: up\n      - expression: '^node_'\n"), []string{sameRuleSaid("node", 0, 2, every)}},
		"prometheus, a name and expression":    {collectorRules("prometheus", "", "      - name: load\n        expression: '^node_load1$'\n      - name: load\n        expression: '^node_load1$'\n        scale: 0.001\n"), []string{sameRuleSaid("node", 0, 1, load)}},
		`prometheus, expression: ""`:           {collectorRules("prometheus", "", "      - name: up\n        expression: \"\"\n      - name: up\n"), []string{sameRuleSaid("node", 0, 1, up)}},
		`prometheus, name: "" and items: ""`:   {collectorRules("prometheus", "", "      - name: \"\"\n        items: \"\"\n        expression: '^node_'\n      - expression: '^node_'\n"), []string{sameRuleSaid("node", 0, 1, every)}},
		"prometheus, three copies":             {collectorRules("prometheus", "", "      - name: up\n      - name: up\n      - name: up\n"), []string{sameRuleSaid("node", 0, 1, up), sameRuleSaid("node", 0, 2, up)}},
		"prometheus, what a rule says besides": {collectorRules("prometheus", "", "      - name: up\n      - name: up\n        description: Whether it is up.\n        required: false\n        error_mode: ignore\n        scale: 2\n"), []string{sameRuleSaid("node", 0, 1, up)}},
		"jq, a rule pasted twice":              {collectorRules("jq", "", jobs+"      - name: up\n        expression: .up\n"+jobs), []string{sameRuleSaid("node", 0, 2, model.MetricRule{Name: "jobs"})}},
		"jq, the labels in another order":      {collectorRules("jq", "", jobs+"      - name: jobs\n        items: .queues[]\n        expression: .jobs\n        labels:\n          - name: site\n            value: rack1\n            expression: \"\"\n          - name: queue\n            value: \"\"\n            expression: .name\n            truncate: true\n"), []string{sameRuleSaid("node", 0, 1, model.MetricRule{Name: "jobs"})}},
		"csv, another error_mode":              {collectorRules("csv", "", "      - name: used\n        expression: used\n      - name: used\n        expression: used\n        error_mode: \" FAIL \"\n"), []string{sameRuleSaid("node", 0, 1, model.MetricRule{Name: "used"})}},
		// The rules together are told of in the order they were: two
		// value_maps of one label first.
		"jq, and two value_maps of a label": {collectorRules("jq", "", "      - name: up\n        expression: .v\n        labels:\n          - name: state\n            expression: .s\n            value_map: {a: b}\n      - name: up\n        expression: .v\n        labels:\n          - name: state\n            expression: .s\n            value_map: {a: c}\n"),
			[]string{`collector "node" metric "up" label "state" has one value_map in rule 1 and another in rule 2; rules of one name share their series, so give them one value_map, or different names`, sameRuleSaid("node", 0, 1, model.MetricRule{Name: "up"})}},
		// A rule refused for something else is told that alone, and its
		// copy too; a sound rule's copy is told beside them.
		"prometheus, copies that are refused": {collectorRules("prometheus", "", "      - name: bad-name\n      - name: bad-name\n      - name: up\n      - {}\n      - {}\n      - name: up\n"), []string{
			`collector "node" metric "bad-name": "bad-name" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`,
			`collector "node" metric "bad-name": "bad-name" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`,
			ruleOfNeither("node", 3), ruleOfNeither("node", 4), sameRuleSaid("node", 2, 5, up),
		}},
	} {
		if got := problemsOf(t, tc.document); !slices.Equal(got, tc.want) {
			t.Errorf("%s: the load says\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}
	// The message, in full.
	const said = `collector "node" metrics rule 1 and rule 3 are the same rule of metric "up": alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each`
	if got := problemsOf(t, collectorRules("prometheus", "", "      - name: up\n      - expression: '^node_'\n      - name: up\n")); len(got) != 1 || got[0] != said {
		t.Errorf("the load says %q\nwant %s", got, said)
	}
	const saidOfAPattern = `collector "node" metrics rule 1 and rule 2 are the same rule of the metrics that match "^node_": alike in name,`
	if got := problemsOf(t, collectorRules("prometheus", "", "      - expression: '^node_'\n      - expression: '^node_'\n")); len(got) != 1 || !strings.HasPrefix(got[0], saidOfAPattern) {
		t.Errorf("the load says %q\nwant %s...", got, saidOfAPattern)
	}
	// In a collector file it is told the same way.
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "collectors.yaml", collectorRules("prometheus", "", "      - name: up\n      - name: up\n"))
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")); err == nil || !strings.HasSuffix(err.Error(), sameRuleSaid("node", 0, 1, up)) {
		t.Errorf("in a collector file: %v", err)
	}

	for name, document := range map[string]string{
		"one name, other expressions":             collectorRules("jq", "", "      - name: bytes\n        expression: .used\n        labels: [{name: kind, value: used}]\n      - name: bytes\n        expression: .free\n        labels: [{name: kind, value: free}]\n"),
		"one name and expression, other items":    collectorRules("jq", "", "      - name: jobs\n        items: .fast[]\n        expression: .jobs\n      - name: jobs\n        items: .slow[]\n        expression: .jobs\n"),
		"one name and expression, other labels":   collectorRules("jq", "", "      - name: up\n        expression: .v\n        labels: [{name: copy, value: \"1\"}]\n      - name: up\n        expression: .v\n        labels: [{name: copy, value: \"2\"}]\n"),
		"one name and expression, a label more":   collectorRules("prometheus", "", "      - name: up\n      - name: up\n        labels: [{name: site, value: x}]\n"),
		"a label that is a constant and one read": collectorRules("jq", "", "      - name: up\n        expression: .v\n        labels: [{name: site, value: .s}]\n      - name: up\n        expression: .v\n        labels: [{name: site, expression: .s}]\n"),
		"value_maps of other texts":               collectorRules("jq", "", "      - name: state\n        expression: .s\n        error_mode: ignore\n        value_map: {up: 1}\n      - name: state\n        expression: .s\n        error_mode: ignore\n        value_map: {down: 0}\n"),
		"time_formats of other texts":             collectorRules("csv", "", "      - name: at\n        expression: at\n        error_mode: ignore\n        time_format: rfc3339\n      - name: at\n        expression: at\n        error_mode: ignore\n        time_format: rfc1123\n"),
		"a python rule twice":                     collectorRules("python", "", "      - name: v\n      - name: v\n"),
		"a python rule twice, with a label":       collectorRules("python", "", "      - name: v\n        labels: [{name: note, expression: note, truncate: true}]\n      - name: v\n        labels: [{name: note, expression: note, truncate: true}]\n"),
		"the same rule in two collectors":         collectorRules("prometheus", "", "      - name: up\n") + strings.Replace(strings.TrimPrefix(collectorRules("prometheus", "", "      - name: up\n"), "collectors:\n"), "name: node", "name: other", 1),
	} {
		if got := problemsOf(t, document); got != nil {
			t.Errorf("%s: the load says\n%s", name, strings.Join(got, "\n"))
		}
	}
}

// A pattern written under name of a prometheus rule is refused as no metric
// name, as it was, and told as well that a pattern belongs in expression,
// and what name is beside one
// (transform.TestAPatternUnderAPrometheusRulesNameIsToldItBelongsInExpression).
// The same name under another transform, and a name that is none for
// another reason, read word for word as they did. name_escaping, which is
// about the names a response gives, changes nothing of it: a rule's name
// with a dot is refused under each of its values.
func TestAPatternUnderAPrometheusRulesNameIsToldSoAtLoad(t *testing.T) {
	const useLetters = ` is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`
	const belongs = `; a pattern to match the target's metric names by is a prometheus rule's expression, not its name, so if this is one, write it as expression, in single quotes, and leave name out, or set name to the one name the series it matches are to be exported under`
	for written, name := range map[string]string{`'node_.*'`: "node_.*", `'^up$'`: "^up$", `'up|node_load1'`: "up|node_load1", `http.server.duration`: "http.server.duration", `'node_\d+'`: `node_\d+`} {
		quoted := `"` + strings.ReplaceAll(name, `\`, `\\`) + `"`
		for _, escaping := range []string{"", "    name_escaping: fail\n", "    name_escaping: underscores\n", "    name_escaping: values\n"} {
			want := []string{`collector "node" metric ` + quoted + `: ` + quoted + useLetters + belongs}
			if got := problemsOf(t, collectorRules("prometheus", escaping, "      - name: "+written+"\n")); !slices.Equal(got, want) {
				t.Errorf("prometheus, %sname: %s: the load says %q\nwant %q", escaping, written, got, want)
			}
			if got := problemsOf(t, collectorRules("prometheus", escaping, "      - name: up\n      - name: "+written+"\n        expression: '^node_'\n")); !slices.Equal(got, want) {
				t.Errorf("prometheus, %sname: %s beside an expression: the load says %q\nwant %q", escaping, written, got, want)
			}
			want = []string{`collector "node" metric ` + quoted + `: ` + quoted + useLetters}
			if got := problemsOf(t, collectorRules("jq", escaping, "      - name: "+written+"\n        expression: .v\n")); !slices.Equal(got, want) {
				t.Errorf("jq, %sname: %s: the load says %q\nwant %q", escaping, written, got, want)
			}
			if got := problemsOf(t, collectorRules("python", escaping, "      - name: "+written+"\n")); !slices.Equal(got, want) {
				t.Errorf("python, %sname: %s: the load says %q\nwant %q", escaping, written, got, want)
			}
		}
	}
	for written, want := range map[string]string{
		"bad-name": `collector "node" metric "bad-name": "bad-name"` + useLetters,
		"1up":      `collector "node" metric "1up": "1up"` + useLetters,
		"__up":     `collector "node" metric "__up": "__up" starts with "__", which Prometheus reserves`,
	} {
		if got := problemsOf(t, collectorRules("prometheus", "", "      - name: "+written+"\n")); !slices.Equal(got, []string{want}) {
			t.Errorf("prometheus, name: %s: the load says %q\nwant %s", written, got, want)
		}
	}
	// The pattern where it belongs loads, with a name beside it and without.
	for _, rules := range []string{"      - expression: 'node_.*'\n", "      - name: load\n        expression: '^node_load1$'\n", "      - expression: '^http\\.server\\.duration$'\n"} {
		if got := problemsOf(t, collectorRules("prometheus", "    name_escaping: underscores\n", rules)); got != nil {
			t.Errorf("%q: the load says %q", rules, got)
		}
	}
}
