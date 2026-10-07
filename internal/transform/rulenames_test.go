package transform

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkMetricRuleBeforePatternNames is CheckMetricRule as it was before a
// prometheus rule's name that reads as a pattern was told where a pattern
// belongs, and before the loader named a rule without a name by its place,
// kept as an oracle: CheckMetricRule must say of every rule what this says,
// word for word and in order, but for the one sentence added to what it
// says of such a name, and CheckMetricRuleAt the same of a rule that has a
// name.
func checkMetricRuleBeforePatternNames(x *model.Collector, r *model.MetricRule) error {
	where := fmt.Sprintf("collector %q metric %q", x.Name, r.Name)
	var errs []error
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if r.Name != "" {
		if err := checkMetricName(x, r.Name); err != nil {
			fail(fmt.Errorf("%s: %w", where, err))
		}
	}
	if r.Items != "" && !jqFamily(x.Transform.Type) && x.Transform.Type != "css" {
		fail(fmt.Errorf("%s sets items, which only the jq, yq and css transforms support", where))
	}
	fail(checkValueRules(x, r, where))
	fail(checkTimeRules(x, r, where))
	fail(checkLabelValueMaps(x, r, where))
	switch {
	case jqFamily(x.Transform.Type):
		if r.Items != "" {
			if _, err := expr.CompileJQ(r.Items); err != nil {
				fail(fmt.Errorf("%s items %q: %w", where, r.Items, err))
			}
		}
		if _, err := expr.CompileJQ(r.Expression); err != nil {
			fail(fmt.Errorf("%s expression %q: %w", where, r.Expression, err))
		}
		for _, label := range expressionLabels(r) {
			if _, err := expr.CompileJQ(label.Expression); err != nil {
				fail(fmt.Errorf("%s label %q expression %q: %w", where, label.Name, label.Expression, err))
			}
		}
	case x.Transform.Type == "regex":
		re, err := expr.CompileRegex(r.Expression)
		if err != nil {
			fail(fmt.Errorf("%s regex %q: %w", where, r.Expression, err))
			break
		}
		// The capture group named value is the value, or else the first.
		// Without one there is nothing to say which part of the match is
		// the number, and with two named value, which of them.
		if re.NumSubexp() == 0 {
			fail(fmt.Errorf("%s regex %q has no capture group; the value is the capture group named value, as in '(?P<value>\\d+)', or else the first, so wrap the number in one, such as 'requests=(\\d+)'", where, r.Expression))
		}
		names := re.SubexpNames()
		if named := countOf(names, regexValueName); named > 1 {
			fail(fmt.Errorf("%s regex %q has %d capture groups named %s; the group of that name is the value, so the regex can have one: name the others something else, or leave them unnamed", where, r.Expression, named, regexValueName))
		}
		for _, label := range expressionLabels(r) {
			if index := captureIndex(label.Expression, names); index < 0 || index >= len(names) {
				fail(fmt.Errorf("%s label %q refers to capture group %q, which the regex does not have", where, label.Name, label.Expression))
			}
		}
	case x.Transform.Type == "css":
		if r.Items != "" {
			if _, err := expr.CompileCSS(r.Items); err != nil {
				fail(fmt.Errorf("%s items CSS selector %q: %w", where, r.Items, err))
			}
		}
		if _, err := expr.CompileCSS(r.Expression); err != nil {
			fail(fmt.Errorf("%s CSS selector %q: %w", where, r.Expression, err))
		}
		for _, label := range expressionLabels(r) {
			if _, err := expr.CompileCSS(label.Expression); err != nil {
				fail(fmt.Errorf("%s label %q CSS selector %q: %w", where, label.Name, label.Expression, err))
			}
			// Without items a label selector could only match inside the
			// element whose whole text is the value, so it could only read
			// text that is part of the number.
			if r.Items == "" {
				fail(fmt.Errorf("%s label %q reads the response, which a css metric can do only with items: set items to the rows, such as '#servers tr:has(td)', and select the value and each label within a row", where, label.Name))
			}
		}
	case x.Transform.Type == "xpath":
		// xml is bound without being mapped, as XML binds it.
		namespaces := boundNamespaces(x.Response.Namespaces)
		if _, err := expr.CompileXPath(r.Expression, namespaces); err != nil {
			fail(fmt.Errorf("%s XPath %q: %w", where, r.Expression, err))
		}
		for _, label := range expressionLabels(r) {
			// An attribute read from the node by its name as written is
			// not compiled (ownAttributeLabel, xpathLabelShape); anything
			// else is XPath, with the collector's namespaces.
			if xpathLabelReadByName(label.Expression, x.Decoder.Type, namespaces) {
				continue
			}
			if _, err := expr.CompileXPath(label.Expression, namespaces); err != nil {
				// A label only HTML reads, of a collector taken to read XML
				// for its namespaces alone: the way out is not in the
				// XPath error.
				hint := ""
				if x.Decoder.Type != "xml" && x.Decoder.Type != "html" && len(namespaces) > 0 && xpathLabelReadByName(label.Expression, "html", nil) {
					hint = "; response.namespaces is set, so the labels are checked as those of an XML document: if the target answers HTML, where this label is an attribute's name as written, set decoder.type to html"
				}
				fail(fmt.Errorf("%s label %q XPath %q: %w%s", where, label.Name, label.Expression, err, hint))
			}
		}
	case x.Transform.Type == "prometheus":
		if pattern := r.Expression; pattern != "" {
			fail(checkRulePattern(where, pattern))
			if _, err := expr.CompileRegex(pattern); err != nil {
				fail(fmt.Errorf("%s expression %q: %w", where, pattern, err))
			}
		}
	}
	return model.JoinProblems(errs...)
}

// patternUnderName is what the load adds to its refusal of a prometheus
// rule's name that is no metric name and reads as a pattern.
const patternUnderName = `; a pattern to match the target's metric names by is a prometheus rule's expression, not its name, so if this is one, write it as expression, in single quotes, and leave name out, or set name to the one name the series it matches are to be exported under`

// notAMetricName is what the load says of a rule's name that is no metric
// name, after the collector and the rule, under name_escaping fail, where
// it is refused: what it said, and, as a scrape says of a response's name
// that is not classic, what would export it.
func notAMetricName(name string) string {
	return fmt.Sprintf(`%q is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`, name)
}

// A prometheus rule picks the target's metrics by its expression, a regular
// expression over their names, and its name is one metric's. A pattern
// written under name — node_.*, ^up$, up|node_load1 — was refused as no
// metric name, with the advice to use letters, digits, underscores and
// colons, which is true and sends the author to spoil a pattern that only
// stands under the wrong key. A name that is no metric name and holds a
// character a regular expression gives a meaning, . * + ? ^ $ | ( ) [ ] { }
// or a backslash, is now told as well that a pattern belongs in expression,
// and what name is beside one. Nothing else reads differently: a name that
// is none for another reason, a name Prometheus reserves, a name that is
// one, and the same names under the seven other transforms, whose rules
// have no pattern to be mistaken for. That is under name_escaping fail,
// written or left out, where a name that is not classic is refused, and is
// told as well what would export it. Under underscores and values a rule's
// name is held to what a scrape holds a name to, so each of these loads,
// under every transform, but one underscores would export beginning with
// "__", which Prometheus reserves.
func TestAPatternUnderAPrometheusRulesNameIsToldItBelongsInExpression(t *testing.T) {
	patterns := []string{"node_.*", "^up$", "up|node_load1", "node_+", "up?", "(up)", "node_[a-z]", "up{1}", `node_\w`, "$", ".", "http.server.duration", `up{job="x"}`, "é.", " .* "}
	others := []string{"bad-name", "node load", "1up", "é", "up,down", "a/b", "up!", "a=b", "@up", "a'b", `a"b`, "a#b", "~up", "a&b", "<up>", "a%b"}
	expressions := map[string]string{"jq": ".v", "yq": ".v", "xpath": "//v", "css": "td.v", "regex": `v=(\d+)`, "csv": "v", "prometheus": "^up$", "python": ""}
	for transformType, expression := range expressions {
		for _, escaping := range []string{"", NameEscapingFail, NameEscapingUnderscores, NameEscapingValues} {
			x := &model.Collector{Name: "node", NameEscaping: escaping, Transform: model.TransformConfig{Type: transformType}}
			said := func(name, expression string) string {
				err := CheckMetricRule(x, &model.MetricRule{Name: name, Expression: expression, Type: model.GaugeMetricType})
				if err == nil {
					return ""
				}
				return err.Error()
			}
			// What the load says of a name that is not classic: under
			// fail that it is none, and under the other two nothing,
			// unless underscores exports it as a reserved name.
			refusal := func(name string) string {
				switch exported := escapeName(name, escaping, true); {
				case escaping == NameEscapingValues || escaping == NameEscapingUnderscores && !strings.HasPrefix(exported, "__"):
					return ""
				case escaping == NameEscapingUnderscores:
					return `collector "node" metric ` + fmt.Sprintf("%q: %q is exported as %q under name_escaping underscores, which starts with \"__\", which Prometheus reserves", name, name, exported)
				}
				return `collector "node" metric ` + fmt.Sprintf("%q: ", name) + notAMetricName(name)
			}
			for _, name := range patterns {
				want := refusal(name)
				if transformType == "prometheus" && want != "" {
					want += patternUnderName
				}
				// With an expression beside it and, where the transform
				// takes a rule of a name alone, without.
				if got := said(name, expression); got != want {
					t.Errorf("%s, name_escaping %q, name %q:\n got %s\nwant %s", transformType, escaping, name, got, want)
				}
				if transformType == "prometheus" && said(name, "") != want {
					t.Errorf("prometheus, name %q without an expression: %s", name, said(name, ""))
				}
			}
			for _, name := range others {
				if got, want := said(name, expression), refusal(name); got != want {
					t.Errorf("%s, name_escaping %q, name %q:\n got %s\nwant %s", transformType, escaping, name, got, want)
				}
			}
			if got, want := said("__up", expression), `collector "node" metric "__up": "__up" starts with "__", which Prometheus reserves`; got != want {
				t.Errorf("%s, a reserved name: %s", transformType, got)
			}
			for _, name := range []string{"up", "node_load1", "job:up:sum", "_", "UP"} {
				if got := said(name, expression); got != "" {
					t.Errorf("%s, name %q: %s", transformType, name, got)
				}
			}
		}
	}
	// It is said beside whatever else the rule is refused for, of the name
	// alone, and to a rule the loader names by its place the same.
	x := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{{Name: "up"}, {Name: "node_.*", Expression: "(", Items: ".rows[]"}}}
	want := `collector "node" metric "node_.*": ` + notAMetricName("node_.*") + patternUnderName + "\n" +
		`collector "node" metric "node_.*" sets items, which only the jq, yq and css transforms support` + "\n" +
		`collector "node" metric "node_.*" expression "(": `
	for what, err := range map[string]error{"by its name": CheckMetricRule(x, &x.Metrics[1]), "by its place": CheckMetricRuleAt(x, 1)} {
		if problems := problemTexts(err); len(problems) != 3 || !strings.HasPrefix(strings.Join(problems, "\n"), want) {
			t.Errorf("a rule checked %s: %v\nwant three problems, %s...", what, err, want)
		}
	}
}

// problemTexts is the text of each problem of a rule check's error, none
// for a rule the check takes.
func problemTexts(err error) []string {
	if err == nil {
		return nil
	}
	problems := model.Problems{err}
	if several, ok := err.(model.Problems); ok { //nolint:errorlint // a rule check returns the list itself
		problems = several
	}
	texts := make([]string, len(problems))
	for i, problem := range problems {
		texts[i] = problem.Error()
	}
	return texts
}

// The loader names a rule in its messages by the rule's metric name, and a
// rule that has none to be named by — name left out, "" or nothing but
// blanks — by its place among its collector's rules, counted from 1:
// `metric ""` told its author nothing in a collector of thirty rules. A
// name that is no metric name for another reason is quoted as written,
// which finds its rule.
func TestARuleWithoutANameIsNamedByItsPlace(t *testing.T) {
	x := &model.Collector{Name: "node", Metrics: []model.MetricRule{{Name: "up"}, {}, {Name: "  "}, {Name: "\t"}, {Name: "bad-name"}, {Name: " up"}, {Name: ""}}}
	for index, want := range []string{`metric "up"`, "metrics rule 2", "metrics rule 3", "metrics rule 4", `metric "bad-name"`, `metric " up"`, "metrics rule 7"} {
		if got := RuleName(&x.Metrics[index], index); got != want {
			t.Errorf("rule %d is named %s, want %s", index+1, got, want)
		}
		if got := RuleWhere(x, index); got != `collector "node" `+want {
			t.Errorf("rule %d is told of as %s", index+1, got)
		}
	}
}

// Telling a prometheus rule's name that reads as a pattern where a pattern
// belongs, and naming a rule without a name by its place, change nothing
// else the check of a rule's name and expressions says. Over generated rules
// under each of the eight transforms — names that are metric names, that
// are none with and without a character of a pattern, reserved, of blanks
// and left out; expressions that compile and that do not, of blanks and
// left out; items, a scale that is none, a value_map, a time_format, a
// time_zone alone, and labels that read, are static, have a value_map and
// do not compile — CheckMetricRule says what it said, word for word and
// problem for problem, but for the sentence added to its refusal of a
// prometheus rule's name that is none and reads as a pattern, which is the
// first thing it says of the rule. CheckMetricRuleAt, by which the loader
// checks the rule at its place, says the same of a rule that has a name,
// and of one that has none the same with the place where `metric ""` or the
// blanks stood.
func TestOnlyAPatternUnderAPrometheusRulesNameReadsDifferently(t *testing.T) {
	zero, two := 0.0, 2.0
	type shape struct{ expression, label, items string }
	shapes := map[string]shape{
		"jq": {".v", ".l", ".rows[]"}, "yq": {".v", ".l", ".rows[]"}, "xpath": {"//v", "@l", ""}, "css": {"td.v", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "l", ""}, "prometheus": {"^up$", "l", ""}, "python": {"", "l", ""},
	}
	names := []string{"m", "", "  ", "__m", "bad-name", "node load", "node_.*", "^up$", "a|b", "http.server.duration", `a\d`, "é"}
	tried, added, placed, refused := 0, 0, 0, 0
	for transformType, shape := range shapes {
		lists := [][]model.LabelRule{
			nil, {{Name: "site", Expression: shape.label}}, {{Name: "site", Value: "x"}}, {{Name: "site", Expression: "("}},
			{{Name: "site", Expression: shape.label, ValueMap: map[string]string{"a": "b"}}}, {{Name: "site", Value: "x", ValueMap: map[string]string{"a": "b"}}},
		}
		rests := []model.MetricRule{{}, {Scale: &zero}, {ValueMap: map[string]float64{" up": 1}}, {Scale: &two}, {ValueMap: map[string]float64{"up": 1}}, {TimeFormat: "rfc3339"}, {TimeFormat: "never"}, {TimeZone: "UTC"}, {TimeFormat: "rfc3339", ValueMap: map[string]float64{"up": 1}}}
		// Under the race detector, which makes the check several times
		// slower, a sixth of the table.
		if raceDetector {
			lists, rests = lists[:3], rests[:3]
		}
		for _, name := range names {
			for _, expression := range []string{shape.expression, "", "  ", "(", ".*"} {
				for _, items := range []string{"", shape.items, "("} {
					for _, labels := range lists {
						for _, rule := range rests {
							rule.Name, rule.Expression, rule.Items, rule.Labels, rule.Type = name, expression, items, labels, model.GaugeMetricType
							// The rule is the third of its collector's.
							x := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: transformType}, Metrics: []model.MetricRule{{Name: "a"}, {Name: "b"}, rule}}
							said, before, there := problemTexts(CheckMetricRule(x, &x.Metrics[2])), problemTexts(checkMetricRuleBeforePatternNames(x, &x.Metrics[2])), problemTexts(CheckMetricRuleAt(x, 2))
							tried++
							if len(before) == 0 {
								if len(said) != 0 || len(there) != 0 {
									t.Errorf("%s rule %+v was taken, and is refused: %q, and at its place %q", transformType, rule, said, there)
								}
								continue
							}
							refused++
							if len(said) != len(before) || len(there) != len(before) {
								t.Errorf("%s rule %+v:\n now %q\n was %q\n at its place %q", transformType, rule, said, before, there)
								continue
							}
							pattern := transformType == "prometheus" && !model.ValidMetricName(name) && strings.ContainsAny(name, `.*+?^$|()[]{}\`)
							for i, text := range before {
								want := text
								if pattern && i == 0 {
									if !strings.HasSuffix(text, notAMetricName(name)) {
										t.Errorf("%s rule %+v: the first thing said was %s", transformType, rule, text)
									}
									want += patternUnderName
									added++
								}
								if said[i] != want {
									t.Errorf("%s rule %+v, problem %d:\n now %s\nwant %s", transformType, rule, i+1, said[i], want)
								}
								if strings.TrimSpace(name) == "" {
									want = `collector "node" metrics rule 3` + strings.TrimPrefix(want, fmt.Sprintf(`collector "node" metric %q`, name))
									placed++
								}
								if there[i] != want || !strings.HasPrefix(want, `collector "node" metric`) {
									t.Errorf("%s rule %+v at its place, problem %d:\n now %s\nwant %s", transformType, rule, i+1, there[i], want)
								}
							}
						}
					}
				}
			}
		}
	}
	if least := map[bool]int{false: 6, true: 1}[raceDetector]; tried < 10000*least || refused < 9000*least || added < 400*least || placed < 3000*least {
		t.Fatalf("%d rules were tried, %d of them refused, %d told where a pattern belongs and %d problems told of by the rule's place", tried, refused, added, placed)
	}
	t.Logf("%d generated rules: %d refused, %d of them a prometheus rule told where a pattern belongs, and %d problems of a rule without a name told of by its place", tried, refused, added, placed)
}
