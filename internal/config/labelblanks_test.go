package config

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// validateMetricRuleBeforeBlankExpressions is validateMetricRule as it was
// before a label's expression of nothing but blanks, and a constant value on
// a python rule's label, were refused, kept as an oracle: every other rule
// must get from validateMetricRule the verdict, the words and the defaults it
// got from this. It is in two parts, as the check is: the rule's own
// settings, which stop at the first mistake, and then its expressions.
func validateMetricRuleBeforeBlankExpressions(x *model.Collector, r *model.MetricRule) error {
	if err := ruleSettingsBeforeBlankExpressions(x, r); err != nil {
		return err
	}
	return transform.CheckMetricRule(x, r)
}

// ruleSettingsBeforeBlankExpressions is the first part of the check as it
// was: everything it said of a rule before it compiled the expressions.
func ruleSettingsBeforeBlankExpressions(x *model.Collector, r *model.MetricRule) error {
	if r.ErrorMode == "" {
		r.ErrorMode = model.ErrorModeLog
	}
	if err := normalizeErrorPolicy(x.Name, fmt.Sprintf("metric %q error_mode", r.Name), &r.ErrorMode); err != nil {
		return err
	}
	if r.Type == "" && x.Transform.Type != "prometheus" {
		r.Type = model.GaugeMetricType
	}
	switch r.Type {
	case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
	case "":
	case model.HistogramMetricType, model.SummaryMetricType:
		if x.Transform.Type != "prometheus" {
			return fmt.Errorf("collector %q metric %q has type %s, which only a prometheus transform can give, passing through a %s that has its buckets or quantiles; a %s rule reads one value, so use gauge, counter or untyped", x.Name, r.Name, r.Type, r.Type, x.Transform.Type)
		}
	default:
		return fmt.Errorf("collector %q metric %q has invalid type %q", x.Name, r.Name, r.Type)
	}
	if strings.TrimSpace(r.Name) == "" && x.Transform.Type != "prometheus" && x.Transform.Type != "python" {
		return fmt.Errorf("collector %q has a metric without a name", x.Name)
	}
	if strings.TrimSpace(r.Expression) == "" && x.Transform.Type != "python" && x.Transform.Type != "prometheus" {
		return fmt.Errorf("collector %q metric %q has no expression", x.Name, r.Name)
	}
	for _, label := range r.Labels {
		if strings.TrimSpace(label.Name) == "" {
			return fmt.Errorf("collector %q metric %q has a label without a name", x.Name, r.Name)
		}
		if !namePattern.MatchString(label.Name) {
			return fmt.Errorf("collector %q metric %q has invalid label name %q", x.Name, r.Name, label.Name)
		}
		if err := model.CheckLabelName(label.Name); err != nil {
			return fmt.Errorf("collector %q metric %q: %w", x.Name, r.Name, err)
		}
		hasValue, hasExpression := label.Value != "", strings.TrimSpace(label.Expression) != ""
		switch {
		case hasValue && hasExpression:
			return fmt.Errorf("collector %q metric %q label %q sets both value and expression; set value for a static label, or expression to read it from the response", x.Name, r.Name, label.Name)
		case !hasValue && !hasExpression:
			return fmt.Errorf("collector %q metric %q label %q needs a value, for a static label, or an expression, to read it from the response", x.Name, r.Name, label.Name)
		case hasValue && label.Required:
			return fmt.Errorf("collector %q metric %q label %q has a static value, so it cannot be required; its value is always there", x.Name, r.Name, label.Name)
		case label.Required && x.Transform.Type == "python":
			return fmt.Errorf("collector %q metric %q label %q cannot be required: a python transform's labels come from its script, not from label expressions", x.Name, r.Name, label.Name)
		}
	}
	return nil
}

// refusedAnew is what the check refuses a rule for that the check as it was
// did not.
type refusedAnew int

const (
	asBefore refusedAnew = iota
	forBlanks
	forAPythonValue
)

// checkedAsBefore puts one rule through the loader's check and through the
// check as it was, each with a copy of its own, and fails unless the two
// agree: on the error, word for word, and on the rule the check leaves, with
// its defaults filled in. They may differ in two cases, and it reports
// which the rule was. One is a rule the check gets as far as a label whose
// expression is nothing but blanks, which it must refuse in its own words
// whatever the check as it was said. How far the check gets is asked of the
// check as it was: with neither a value nor an expression, that label is
// the one it says needs one, unless something before it refuses the rule.
// The other is a python rule whose settings the check as it was found
// nothing wrong with and one of whose labels sets a value: the check must
// refuse it for the first such label, before it looks at the expressions.
func checkedAsBefore(t *testing.T, x *model.Collector, rule model.MetricRule) refusedAnew {
	t.Helper()
	now, before := rule, rule
	now.Labels, before.Labels = slices.Clone(rule.Labels), slices.Clone(rule.Labels)
	err, was := validateMetricRule(x, &now), validateMetricRuleBeforeBlankExpressions(x, &before)
	blank := slices.IndexFunc(rule.Labels, func(label model.LabelRule) bool {
		return label.Expression != "" && strings.TrimSpace(label.Expression) == ""
	})
	if blank >= 0 {
		label, bare := rule.Labels[blank], rule
		bare.Labels = slices.Clone(rule.Labels)
		bare.Labels[blank].Value, bare.Labels[blank].Expression = "", ""
		needs := fmt.Sprintf("collector %q metric %q label %q needs a value, for a static label, or an expression, to read it from the response", x.Name, rule.Name, label.Name)
		if reached := validateMetricRuleBeforeBlankExpressions(x, &bare); reached != nil && reached.Error() == needs {
			want := fmt.Sprintf("collector %q metric %q label %q expression %q is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant", x.Name, rule.Name, label.Name, label.Expression)
			if err == nil || err.Error() != want {
				t.Errorf("%s rule %+v: %v, want %s", x.Transform.Type, rule, err, want)
			}
			return forBlanks
		}
	}
	if x.Transform.Type == "python" {
		settings := rule
		settings.Labels = slices.Clone(rule.Labels)
		constant := slices.IndexFunc(rule.Labels, func(label model.LabelRule) bool { return label.Value != "" })
		if constant >= 0 && ruleSettingsBeforeBlankExpressions(x, &settings) == nil {
			want := fmt.Sprintf("collector %q metric %q label %q sets value, which a python rule's label does not take: the script sets the labels of its series itself, with metric(..., labels={...}), and a rule's label only names one of them to cut with truncate: true; for a constant on every series of the collector, set transform.labels", x.Name, rule.Name, rule.Labels[constant].Name)
			if err == nil || err.Error() != want || !reflect.DeepEqual(now, settings) {
				t.Errorf("python rule %+v: %v, leaving %+v\n want %s, leaving %+v", rule, err, now, want, settings)
			}
			return forAPythonValue
		}
	}
	if (err == nil) != (was == nil) || err != nil && err.Error() != was.Error() || !reflect.DeepEqual(now, before) {
		t.Errorf("%s rule %+v:\n now %v, leaving %+v\n was %v, leaving %+v", x.Transform.Type, rule, err, now, was, before)
	}
	return asBefore
}

// Refusing a label's expression of nothing but blanks changes the verdict on
// nothing else. Every rule of the shipped configurations — the examples, the
// ones under configs and the fixtures' — and of a generated table of labels,
// under each transform, gets from the loader what it got before: the same
// error, word for word, or none, and the same defaults. In the table a label
// has each of some values beside each of some expressions, the blanks
// strings.TrimSpace takes off among them and two characters that only look
// like blanks, alone, required, with a value_map, without a name, and before
// and after a label that is in order and one that is not, in a rule that is
// in order and in one refused for something else; the rules the check
// reaches such a label of, and only those, are refused in the new words.
// Before, a label whose expression is nothing but blanks beside a
// value was taken for a static label by this check and for an expression by
// everything after it: a jq, yq, css, xpath or regex rule was refused for
// what a blank is in its language, and a csv, prometheus or python rule
// loaded.
func TestOnlyALabelExpressionOfBlanksIsRefusedAnew(t *testing.T) {
	// The shipped files, as they are written: read without being validated,
	// which a build with only some request types could not do for them all.
	files, rules, labels := 0, 0, 0
	for _, root := range []string{"../../examples", "../../configs", "../../testdata"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			var written []model.Collector
			_, _ = load(path, nil, func(c *model.Config) {
				for _, collector := range c.Collectors {
					collector.Metrics = slices.Clone(collector.Metrics)
					for i := range collector.Metrics {
						collector.Metrics[i].Labels = slices.Clone(collector.Metrics[i].Labels)
					}
					written = append(written, collector)
				}
			})
			if len(written) > 0 {
				files++
			}
			for i := range written {
				x := &written[i]
				x.Transform.Type = strings.ToLower(strings.TrimSpace(x.Transform.Type))
				for _, rule := range x.Metrics {
					rules++
					labels += len(rule.Labels)
					if checkedAsBefore(t, x, rule) != asBefore {
						t.Errorf("%s: collector %q metric %q has a label whose expression is nothing but blanks, or a python rule's label with a value", path, x.Name, rule.Name)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 13 || rules < 100 || labels < 100 {
		t.Fatalf("%d files with %d rules and %d labels were found", files, rules, labels)
	}

	type shape struct {
		expression, label string
		items             string
	}
	shapes := map[string]shape{
		"jq": {".v", ".l", ""}, "yq": {".v", ".l", ""}, "xpath": {"//v", "@l", ""}, "css": {"td.v", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "l", ""}, "prometheus": {"^up$", "l", ""}, "python": {"", "l", ""},
	}
	values := []string{"", "x", " ", "  ", " x "}
	expressions := []string{"", " ", "  ", "\t", "\n", " \t\r\n", "\u00A0", "\u0085", "\u3000", "\u2028 ", "\u200B", "\uFEFF", " \u200B "}
	refused, tried := 0, 0
	for name, shape := range shapes {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		for _, value := range values {
			// Beside the table's, an expression of the transform's own, and
			// the same with blanks around it.
			for _, expression := range append(slices.Clone(expressions), shape.label, " "+shape.label+" ") {
				for _, label := range []model.LabelRule{
					{Name: "site", Value: value, Expression: expression},
					{Name: "site", Value: value, Expression: expression, Required: true},
					{Name: "site", Value: value, Expression: expression, Truncate: true},
					{Name: "site", Value: value, Expression: expression, ValueMap: map[string]string{"a": "b"}},
					{Name: "", Value: value, Expression: expression},
					{Name: "bad-name", Value: value, Expression: expression},
				} {
					good := model.LabelRule{Name: "zone", Expression: shape.label}
					for _, list := range [][]model.LabelRule{{label}, {good, label}, {label, good}, {{Name: "zone"}, label}, {label, {Name: "zone"}}} {
						for _, rule := range []model.MetricRule{
							{Name: "m", Items: shape.items, Expression: shape.expression, Labels: list},
							{Name: "m", Items: shape.items, Expression: shape.expression, Labels: list, ErrorMode: "panic"},
							{Name: "m", Items: shape.items, Expression: " ", Labels: list},
							{Name: "", Items: shape.items, Expression: shape.expression, Labels: list},
						} {
							tried++
							if checkedAsBefore(t, x, rule) == forBlanks {
								refused++
							}
						}
					}
				}
			}
		}
	}
	if tried < 50000 || refused < 5000 {
		t.Fatalf("%d rules were tried and %d refused for a label of blanks", tried, refused)
	}
	t.Logf("%d files with %d rules and %d labels, and %d generated rules, %d of them refused for a label of blanks", files, rules, labels, tried, refused)
}

// The schema's rule for a label's expression refuses exactly what the loader
// refuses of it: over every character there is, the pattern under not takes
// the character alone, twice, and beside a space when strings.TrimSpace
// leaves nothing of it, and never the text with a letter in it or the empty
// text, which is the key left out. As the pattern of a value_map's keys, it
// is written for Go and for JavaScript alike.
func TestTheLabelExpressionPatternTakesWhatIsNothingButBlanks(t *testing.T) {
	rule := onlyBlanks()
	if rule["type"] != "string" {
		t.Fatalf("the rule is of %v", rule["type"])
	}
	pattern := regexp.MustCompile(rule["pattern"].(string))
	blanks, texts := 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if unicode.IsSpace(r) {
			blanks++
		} else if r > 0x3000 && r%97 != 0 {
			// Past the last blank, one character in 97 is tried.
			continue
		}
		for _, text := range []string{string(r), string(r) + string(r), " " + string(r), string(r) + "\n", string(r) + ".l", ".l" + string(r), " " + string(r) + "l" + string(r)} {
			texts++
			if got, want := pattern.MatchString(text), strings.TrimSpace(text) == ""; got != want {
				t.Errorf("%q (U+%04X): the pattern takes it: %v; TrimSpace leaves nothing of it: %v", text, r, got, want)
			}
		}
	}
	if blanks != 25 || texts < 100000 {
		t.Fatalf("%d blanks and %d texts were tried", blanks, texts)
	}
	for _, text := range []string{"", ".l", " .l ", "1", "\u200B", "\uFEFF", "\u180E", " \u200B "} {
		if pattern.MatchString(text) {
			t.Errorf("%q is taken for blanks", text)
		}
	}
	for _, escape := range regexp.MustCompile(`\\.`).FindAllString(rule["pattern"].(string), -1) {
		if !slices.Contains([]string{`\t`, `\n`, `\v`, `\f`, `\r`, `\x`}, escape) {
			t.Errorf("the pattern has the escape %s, which Go and JavaScript may not read alike", escape)
		}
	}
	// The rule is on the label's expression, in the configuration's schema
	// and in a collector file's, and leaves the key the types it took.
	for name, schema := range map[string]map[string]any{"the configuration": configSchema(), "a collector file": collectorFileSchema()} {
		node := schema
		for _, key := range []string{"collectors", "metrics", "labels"} {
			node = node["properties"].(map[string]any)[key].(map[string]any)["items"].(map[string]any)
		}
		expression := node["properties"].(map[string]any)["expression"].(map[string]any)
		if !reflect.DeepEqual(expression["not"], rule) || !reflect.DeepEqual(expression["type"], []string{"string", "number", "boolean"}) {
			t.Errorf("%s: a label's expression is described as %v", name, expression)
		}
		if value := node["properties"].(map[string]any)["value"].(map[string]any); value["not"] != nil || value["pattern"] != nil {
			t.Errorf("%s: a label's value is held to %v", name, value)
		}
	}
}
