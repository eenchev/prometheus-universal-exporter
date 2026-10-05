package transform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// rulesPassOn are the names of the series a prometheus collector's rules
// make of the series named, and the failures the rules reported.
func rulesPassOn(t *testing.T, c *model.Collector, names ...string) ([]string, []RuleFailure) {
	t.Helper()
	in := model.MetricSet{}
	for _, name := range names {
		in.Metrics = append(in.Metrics, model.Metric{Name: name, Type: model.GaugeMetricType, Value: 1})
	}
	ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(context.Background()))
	set, err := Transform(ctx, &decode.Decoded{Kind: "prometheus", Data: in}, &fetch.HTTPResponse{StatusCode: 200, Headers: http.Header{}}, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	made := []string{}
	if set != nil {
		for _, m := range set.Metrics {
			made = append(made, m.Name)
		}
	}
	return made, report.Failures()
}

// blankPattern is what the load says of a prometheus rule's expression of
// nothing but blanks, after the collector and the metric.
func blankPattern(expression string) string {
	return fmt.Sprintf(`expression %q is nothing but blanks; a prometheus rule's expression is a regular expression matched against a metric's name as the target gives it, anywhere in it, so this one matches only the names that hold these blanks: write the pattern that was meant, or, for one that does mean a blank, '[ ]' or '\x20' in single quotes, or leave expression out for the rule to pass on the metric its name names`, expression)
}

// A prometheus rule's expression is a regular expression matched against the
// names of the target's metrics, anywhere in a name. One of nothing but
// blanks compiles, and matches only the names that hold those blanks, which
// a UTF-8 name may: of up, node_load1 and "disk  free" the rule passes on
// the last alone, and of the first two nothing — which, with
// required: false, it does without a word. Written bare it is a slip, as it
// is in transform.include, so the load refuses it, with every blank
// strings.TrimSpace takes off, naming the collector and the metric and
// saying how a pattern that means a blank is written; '[ ]' and '\x20'
// compile and match what the blank matched. An expression written "" is
// the key left out, and the rule then matches the metric of its name.
func TestAPrometheusRulesExpressionOfBlanksIsRefused(t *testing.T) {
	no := false
	rules := func(rule model.MetricRule) *model.Collector {
		rule.ErrorMode = model.ErrorModeLog
		return &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{rule}}
	}
	// What the rule did: it matched the names that hold the blanks, and
	// when no name does, an optional rule said nothing and a required one
	// was missing its value.
	if made, failures := rulesPassOn(t, rules(model.MetricRule{Expression: "  ", Required: &no}), "up", "node_load1", "disk  free"); !slices.Equal(made, []string{"disk  free"}) || len(failures) != 0 {
		t.Fatalf("an expression of two blanks passes on %q, with failures %+v", made, failures)
	}
	if made, failures := rulesPassOn(t, rules(model.MetricRule{Name: "up", Expression: "  ", Required: &no}), "up", "node_load1"); len(made) != 0 || len(failures) != 0 {
		t.Fatalf("an optional rule named up with an expression of two blanks passes on %q, with failures %+v", made, failures)
	}
	if made, failures := rulesPassOn(t, rules(model.MetricRule{Name: "up", Expression: "  "}), "up", "node_load1"); len(made) != 0 || len(failures) != 1 || failures[0].First.Error() != `metric "up" expression "  " matched no metric in the response` {
		t.Fatalf("a required rule named up with an expression of two blanks passes on %q, with failures %+v", made, failures)
	}
	for _, expression := range []string{" ", "  ", "\t", " \t\r\n", "\u00A0", "\u0085", "\u3000 ", "\u2028"} {
		for _, rule := range []model.MetricRule{{Expression: expression}, {Name: "up", Expression: expression}, {Name: "up", Expression: expression, Required: &no}} {
			want := fmt.Sprintf(`collector "node" metric %q %s`, rule.Name, blankPattern(expression))
			c := rules(rule)
			if err := CheckMetricRule(c, &c.Metrics[0]); err == nil || err.Error() != want {
				t.Errorf("%+v: %v\nwant %s", rule, err, want)
			}
		}
	}
	// It is one more of the rule's mistakes, reported beside the others.
	c := rules(model.MetricRule{Name: "bad-name", Expression: " ", ValueMap: map[string]float64{"up": 1}})
	var problems model.Problems
	if err := CheckMetricRule(c, &c.Metrics[0]); !errors.As(err, &problems) || len(problems) != 3 || !strings.HasSuffix(problems[2].Error(), blankPattern(" ")) {
		t.Fatalf("a rule with two other mistakes: %v", err)
	}
	// A blank among other characters is a pattern like any other, the two
	// ways the message gives of writing one match what the blank matched,
	// and without an expression the rule matches the metric of its name.
	names := []string{"up", "disk free", "disk  free", "disk\tfree"}
	for name, test := range map[string]struct {
		rule model.MetricRule
		want []string
	}{
		"'[ ]'":        {model.MetricRule{Expression: "[ ]"}, []string{"disk free", "disk  free"}},
		`'\x20'`:       {model.MetricRule{Expression: `\x20`}, []string{"disk free", "disk  free"}},
		`'\x20{2}'`:    {model.MetricRule{Expression: `\x20{2}`}, []string{"disk  free"}},
		"'k f'":        {model.MetricRule{Expression: "k f"}, []string{"disk free"}},
		"' free'":      {model.MetricRule{Expression: " free"}, []string{"disk free", "disk  free"}},
		`'\s'`:         {model.MetricRule{Expression: `\s`}, []string{"disk free", "disk  free", "disk\tfree"}},
		"a name alone": {model.MetricRule{Name: "up"}, []string{"up"}},
		"U+200B":       {model.MetricRule{Expression: "\u200B", Required: &no}, []string{}},
		"U+FEFF":       {model.MetricRule{Expression: "\uFEFF", Required: &no}, []string{}},
	} {
		c := rules(test.rule)
		if err := CheckMetricRule(c, &c.Metrics[0]); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if made, failures := rulesPassOn(t, c, names...); !slices.Equal(made, test.want) || len(failures) != 0 {
			t.Errorf("%s passes on %q with failures %+v, want %q", name, made, failures, test.want)
		}
	}
	// Under every other transform an expression of blanks is none in the
	// transform's language, where there is one to compile, as it was; the
	// loader refuses such a rule before this as one that has no expression
	// (config.validateMetricRule).
	for transform, message := range map[string]string{
		"jq": `collector "node" metric "v" expression "  ": `, "yq": `collector "node" metric "v" expression "  ": `, "css": `collector "node" metric "v" CSS selector "  ": `,
		"xpath": `collector "node" metric "v" XPath "  ": `, "regex": `collector "node" metric "v" regex "  " has no capture group`, "csv": "", "python": "",
	} {
		c := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: transform}, Metrics: []model.MetricRule{{Name: "v", Expression: "  "}}}
		err := CheckMetricRule(c, &c.Metrics[0])
		if message == "" && err != nil || message != "" && (err == nil || !strings.HasPrefix(err.Error(), message)) || err != nil && strings.Contains(err.Error(), "nothing but blanks") {
			t.Errorf("%s: %v, want %q", transform, err, message)
		}
	}
}

// checkMetricRuleBeforeBlankPatterns is CheckMetricRule as it was before a
// prometheus rule's expression of nothing but blanks was refused, kept as an
// oracle: every rule gets from CheckMetricRule what it got from this, but
// for a prometheus rule with such an expression, which gets that one
// problem more, after the others.
func checkMetricRuleBeforeBlankPatterns(x *model.Collector, r *model.MetricRule) error {
	where := fmt.Sprintf("collector %q metric %q", x.Name, r.Name)
	var errs []error
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if r.Name != "" {
		if err := checkMetricName(r.Name); err != nil {
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
			if _, err := expr.CompileRegex(pattern); err != nil {
				fail(fmt.Errorf("%s expression %q: %w", where, pattern, err))
			}
		}
	}
	return model.JoinProblems(errs...)
}

// Refusing a prometheus rule's expression of nothing but blanks changes
// what the check says of no other rule. Over a generated table under each
// of the eight transforms — a rule's expression of each of some texts,
// blanks, the empty one, patterns with a blank in them and ones that do not
// compile among them, in a rule in order and in rules with each of the
// other mistakes the check finds, with labels and without — the check
// agrees with a copy of itself as it was, on the error word for word, for
// every rule but the prometheus rules whose expression is written and is
// nothing but blanks: those get what they got and the new problem after
// it, and no rule of another transform does.
func TestOnlyAPrometheusRulesExpressionOfBlanksIsRefusedAnew(t *testing.T) {
	two := 2.0
	type shape struct{ expression, label, items string }
	shapes := map[string]shape{
		"jq": {".v", ".l", ""}, "yq": {".v", ".l", ""}, "xpath": {"//v", "@l", ""}, "css": {"td.v", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "l", ""}, "prometheus": {"^up$", "l", ""}, "python": {"", "l", ""},
	}
	tried, refused := 0, map[string]int{}
	for name, shape := range shapes {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		for _, expression := range []string{shape.expression, "", " ", "  ", "\t", "\n", " \t\r\n", "\u00A0", "\u0085", "\u3000", "\u2028 ", "\u200B", "\uFEFF", " \u200B ", " up ", "a b", "[ ]", `\x20`, "(", " ("} {
			for _, labels := range [][]model.LabelRule{nil, {{Name: "site", Expression: shape.label}}, {{Name: "site", Value: "x"}}, {{Name: "site", Expression: shape.label, ValueMap: map[string]string{"a": "b"}}}, {{Name: "site", Expression: "("}}} {
				for _, rule := range []model.MetricRule{
					{Name: "m", Items: shape.items, Expression: expression},
					{Name: "", Items: shape.items, Expression: expression},
					{Name: "bad-name", Items: shape.items, Expression: expression},
					{Name: "__m", Items: shape.items, Expression: expression},
					{Name: "m", Items: ".items[]", Expression: expression},
					{Name: "m", Items: shape.items, Expression: expression, Scale: &two},
					{Name: "m", Items: shape.items, Expression: expression, ValueMap: map[string]float64{"up": 1}},
					{Name: "m", Items: shape.items, Expression: expression, ValueMap: map[string]float64{" up": 1}},
					{Name: "m", Items: shape.items, Expression: expression, TimeFormat: "rfc3339", TimeZone: "UTC"},
					{Name: "m", Items: shape.items, Expression: expression, TimeZone: "UTC"},
					{Name: "bad-name", Items: shape.items, Expression: expression, ValueMap: map[string]float64{"up": 1}, TimeZone: "UTC"},
				} {
					rule.Labels = labels
					tried++
					now, before := rule, rule
					err, was := CheckMetricRule(x, &now), checkMetricRuleBeforeBlankPatterns(x, &before)
					want := was
					if name == "prometheus" && expression != "" && strings.TrimSpace(expression) == "" {
						refused[name]++
						want = model.JoinProblems(was, fmt.Errorf("collector %q metric %q %s", x.Name, rule.Name, blankPattern(expression)))
					}
					if (err == nil) != (want == nil) || err != nil && err.Error() != want.Error() {
						t.Errorf("%s rule %+v:\n now %v\nwant %v", name, rule, err, want)
					}
					if err != nil && was == nil {
						refused["nothing else"]++
					}
				}
			}
		}
	}
	if tried < 8000 || refused["prometheus"] < 400 || refused["nothing else"] < 20 || len(refused) != 2 {
		t.Fatalf("%d rules were tried, and refused anew were %v", tried, refused)
	}
	t.Logf("%d generated rules, %d of them prometheus rules with an expression of blanks, %d of which had no other mistake", tried, refused["prometheus"], refused["nothing else"])
}
