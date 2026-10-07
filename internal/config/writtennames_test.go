package config

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// validateMetricRuleBeforeEscapedNames is validateMetricRule as it was while
// a label's name was held to the classic names whatever the collector's
// name_escaping, kept as an oracle. What it calls is as it is now: of those
// transform.CheckMetricRuleAt changed with it, in what it says of a rule's
// own name, which the tests of the transform package hold to what it was
// (transform.TestAWrittenNameIsHeldToWhatAScrapeHoldsItTo).
func validateMetricRuleBeforeEscapedNames(x *model.Collector, index int) error {
	r, where := &x.Metrics[index], transform.RuleWhere(x, index)
	// Whether the rule wrote a type and an error_mode is asked before their
	// defaults are filled in, after which a key left out reads as one that
	// was written (checkPythonRule).
	wroteType, wroteErrorMode := r.Type != "", r.ErrorMode != ""
	if r.ErrorMode == "" {
		r.ErrorMode = model.ErrorModeLog
	}
	if err := normalizeErrorPolicy(x.Name, transform.RuleName(r, index)+" error_mode", &r.ErrorMode); err != nil {
		return err
	}
	// A prometheus transform's rule without a type keeps the type of
	// the series it passes through: a counter stays a counter, a
	// histogram a histogram. Every other rule makes its own samples,
	// gauges unless it says otherwise.
	if r.Type == "" && x.Transform.Type != "prometheus" {
		r.Type = model.GaugeMetricType
	}
	switch r.Type {
	case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
	case "":
	case model.HistogramMetricType, model.SummaryMetricType:
		// Only a series that is one already has buckets or quantiles to
		// expose; a rule reading one number would expose a histogram
		// with a single plain sample, which no parser accepts.
		if x.Transform.Type != "prometheus" {
			return fmt.Errorf("%s has type %s, which only a prometheus transform can give, passing through a %s that has its buckets or quantiles; a %s rule reads one value, so use gauge, counter or untyped", where, r.Type, r.Type, x.Transform.Type)
		}
	default:
		return fmt.Errorf("%s has invalid type %q", where, r.Type)
	}
	// The rule has no name to be named by, so where says which rule of the
	// collector it is.
	if strings.TrimSpace(r.Name) == "" && x.Transform.Type != "prometheus" && x.Transform.Type != "python" {
		return fmt.Errorf("%s has no name", where)
	}
	if strings.TrimSpace(r.Expression) == "" && x.Transform.Type != "python" && x.Transform.Type != "prometheus" {
		return fmt.Errorf("%s has no expression", where)
	}
	for _, label := range r.Labels {
		if strings.TrimSpace(label.Name) == "" {
			return fmt.Errorf("%s has a label without a name", where)
		}
		if !namePattern.MatchString(label.Name) {
			return fmt.Errorf("%s has invalid label name %q", where, label.Name)
		}
		if err := model.CheckLabelName(label.Name); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		// An expression written as nothing but blanks is neither the key left
		// out, which only "" is, nor anything to read a label with. The label
		// is not static then (model.LabelRule.Static), so a value beside it
		// was never exported, while a csv rule read the column of that name
		// and a prometheus rule the source label of that name.
		if label.Expression != "" && strings.TrimSpace(label.Expression) == "" {
			return fmt.Errorf("%s label %q expression %q is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant", where, label.Name, label.Expression)
		}
		hasValue, hasExpression := label.Value != "", strings.TrimSpace(label.Expression) != ""
		switch {
		case hasValue && hasExpression:
			return fmt.Errorf("%s label %q sets both value and expression; set value for a static label, or expression to read it from the response", where, label.Name)
		case !hasValue && !hasExpression:
			return fmt.Errorf("%s label %q needs a value, for a static label, or an expression, to read it from the response", where, label.Name)
		case hasValue && label.Required:
			return fmt.Errorf("%s label %q has a static value, so it cannot be required; its value is always there", where, label.Name)
		case label.Required && x.Transform.Type == "python":
			return fmt.Errorf("%s label %q cannot be required: a python transform's labels come from its script, not from label expressions", where, label.Name)
		}
	}
	if err := checkPythonRuleLabels(x, r, where); err != nil {
		return err
	}
	if err := transform.CheckMetricRuleAt(x, index); err != nil {
		return err
	}
	return checkPythonRule(x, r, where, wroteType, wroteErrorMode)
}

// ruleCheckedBothWays puts one rule through the loader's check and through
// the check as it was, as the one rule of a copy of the collector each, and
// returns what each said and the rule each left, its defaults filled in.
func ruleCheckedBothWays(x *model.Collector, rule model.MetricRule) (now, was string, left, leftBefore model.MetricRule) {
	text := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	a, b := *x, *x
	a.Metrics = []model.MetricRule{rule}
	a.Metrics[0].Labels = slices.Clone(rule.Labels)
	b.Metrics = []model.MetricRule{rule}
	b.Metrics[0].Labels = slices.Clone(rule.Labels)
	now, was = text(validateMetricRule(&a, 0)), text(validateMetricRuleBeforeEscapedNames(&b, 0))
	return now, was, a.Metrics[0], b.Metrics[0]
}

// Holding a name the configuration writes to what a scrape holds it to
// changes what the load says of a rule whose names are all classic in
// nothing, under any name_escaping. Every rule of the shipped
// configurations — the examples, the ones under configs and the fixtures',
// whose names are all classic — gets from the check of a rule what it got,
// word for word, and the same defaults, with name_escaping left out and
// under each of its three values; so does every rule of a generated table
// under each transform whose own name and whose labels' names are classic,
// whatever else is wrong with it. Of the table's other rules, under fail the
// load says what it said, and of a label's name that is not classic also
// what would export it; under underscores and values it says nothing of the
// names, or that underscores would export one as a name Prometheus
// reserves, and of everything else what it said.
func TestOnlyANameThatIsNotClassicIsReadAnew(t *testing.T) {
	modes := []string{"", transform.NameEscapingFail, transform.NameEscapingUnderscores, transform.NameEscapingValues}
	files, rules := 0, 0
	for _, root := range []string{"../../examples", "../../configs", "../../testdata"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			// The file as it is written: read without being validated,
			// which a build with only some request types could not do for
			// every one.
			var written []model.Collector
			_, _ = load(path, nil, func(c *model.Config) { written = slices.Clone(c.Collectors) })
			if len(written) > 0 {
				files++
			}
			for i := range written {
				x := &written[i]
				x.Transform.Type = strings.ToLower(strings.TrimSpace(x.Transform.Type))
				for _, rule := range x.Metrics {
					rules++
					if !model.ValidMetricName(rule.Name) && rule.Name != "" || slices.ContainsFunc(rule.Labels, func(label model.LabelRule) bool { return !model.ValidLabelName(label.Name) }) {
						t.Errorf("%s: collector %q metric %q has a name that is not classic", path, x.Name, rule.Name)
					}
					for _, mode := range modes {
						x.NameEscaping = mode
						if now, was, left, leftBefore := ruleCheckedBothWays(x, rule); now != was || !reflect.DeepEqual(left, leftBefore) {
							t.Errorf("%s: collector %q metric %q under name_escaping %q:\n now %s, leaving %+v\n was %s, leaving %+v", path, x.Name, rule.Name, mode, now, left, was, leftBefore)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 13 || rules < 100 {
		t.Fatalf("%d files with %d rules were found", files, rules)
	}

	type shape struct{ expression, label string }
	shapes := map[string]shape{
		"jq": {".v", ".l"}, "yq": {".v", ".l"}, "xpath": {"//v", "@l"}, "css": {"td.v", "td.l"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l"}, "csv": {"v", "l"}, "prometheus": {"^up$", "l"}, "python": {"", "l"},
	}
	names := []string{"up", "a_b", "__up", "http.server.duration", "node_.*", "1_x", "é", " up", "bad-name"}
	labelNames := []string{"site", "a_b", "__site", "service.name", "1_x", "..x", "é", " site", "bad-name", "", "  "}
	const advice = "; set the collector's name_escaping to underscores or values to export it escaped"
	classic, toldAnew, taken, reserved := 0, 0, 0, 0
	for transformType, shape := range shapes {
		for _, mode := range modes {
			x := &model.Collector{Name: "demo", NameEscaping: mode, Transform: model.TransformConfig{Type: transformType}}
			escapes := mode == transform.NameEscapingUnderscores || mode == transform.NameEscapingValues
			for _, name := range names {
				for _, labelName := range labelNames {
					for _, label := range []model.LabelRule{{Name: labelName, Expression: shape.label, Truncate: true}, {Name: labelName, Value: "x"}, {Name: labelName}} {
						for _, list := range [][]model.LabelRule{{label}, {{Name: "zone", Expression: shape.label, Truncate: true}, label}} {
							for _, rule := range []model.MetricRule{{Name: name, Expression: shape.expression, Labels: list}, {Name: name, Expression: shape.expression, Labels: list, ErrorMode: "panic"}} {
								now, was, left, leftBefore := ruleCheckedBothWays(x, rule)
								invalid := fmt.Sprintf("collector %q metric %q has invalid label name %q", x.Name, name, labelName)
								switch {
								case model.ValidLabelName(labelName) || strings.TrimSpace(labelName) == "" || was != invalid:
									// The label's name is classic, or the
									// check does not come to it, or refuses it
									// as no name at all.
									classic++
									if now != was || !reflect.DeepEqual(left, leftBefore) {
										t.Errorf("%s rule %+v under name_escaping %q:\n now %s, leaving %+v\n was %s, leaving %+v", transformType, rule, mode, now, left, was, leftBefore)
									}
								case !escapes:
									toldAnew++
									if now != was+advice || !reflect.DeepEqual(left, leftBefore) {
										t.Errorf("%s rule %+v under name_escaping %q:\n now %s\n was %s", transformType, rule, mode, now, was)
									}
								default:
									// The label's name is taken: the check
									// says what it said of the rule with a
									// classic name in its place, of this name,
									// unless underscores exports the name as
									// a reserved one. None of the table's
									// names has a colon, which a metric's name
									// may have and a label's may not, so the
									// name is escaped as a metric's is.
									if exported := transform.ExportedMetricName(&model.Collector{NameEscaping: mode}, labelName); strings.HasPrefix(exported, "__") {
										reserved++
										if want := fmt.Sprintf("collector %q metric %q: label name %q is exported as %q under name_escaping %s, which starts with __, which Prometheus reserves for its own labels", x.Name, name, labelName, exported, mode); now != want {
											t.Errorf("%s rule %+v under name_escaping %q: %s\nwant %s", transformType, rule, mode, now, want)
										}
										continue
									}
									taken++
									renamed := rule
									renamed.Labels = slices.Clone(rule.Labels)
									renamed.Labels[len(renamed.Labels)-1].Name = "classic_name"
									_, wasOfAClassicName, _, _ := ruleCheckedBothWays(x, renamed)
									if want := strings.ReplaceAll(wasOfAClassicName, `label "classic_name"`, fmt.Sprintf("label %q", labelName)); now != want {
										t.Errorf("%s rule %+v under name_escaping %q: %s\nwant %s", transformType, rule, mode, now, want)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if classic < 5000 || toldAnew < 500 || taken < 500 || reserved < 100 {
		t.Fatalf("%d rules of classic label names, %d told anew, %d taken and %d refused as reserved were tried", classic, toldAnew, taken, reserved)
	}
}
