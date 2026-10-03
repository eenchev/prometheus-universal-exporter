package transform

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A regex rule's value is the capture group named value when the regex has
// one, and the first group otherwise (regexValueGroup). With it a label can
// stand before the number in the text: the station that starts a report's
// line, the temperature near its end.

// regexRule is a collector of one regex rule.
func regexRule(rule model.MetricRule) model.Collector {
	rule.Type = model.GaugeMetricType
	return model.Collector{Name: "text", Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"}, Metrics: []model.MetricRule{rule}}
}

// The group named value is the value wherever it stands, and the groups
// before it are labels, by name or by number: a group's number is its place
// in the regex, whichever of them is the value.
func TestARegexRuleReadsItsValueFromTheGroupNamedValue(t *testing.T) {
	c := regexRule(model.MetricRule{
		Name:       "temperature_celsius",
		Expression: `(?m)^(?P<station>[A-Z]{4}) (\d{6})Z .* (?P<value>\d{2})/\d{2} Q\d{4}`,
		Labels: []model.LabelRule{
			{Name: "station", Expression: "station"},
			{Name: "issued", Expression: "2"},
			{Name: "first", Expression: "1"},
			{Name: "kind", Value: "metar"},
		},
	})
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, err := runBody(t, c, "text/plain", "2026/10/03 09:00\nLBSF 030900Z 12004KT 9999 14/08 Q1021\nLBBG 030900Z VRB02KT CAVOK 21/15 Q1017\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Metric{
		{Name: "temperature_celsius", Type: model.GaugeMetricType, Value: 14, Labels: map[string]string{"station": "LBSF", "issued": "030900", "first": "LBSF", "kind": "metar"}},
		{Name: "temperature_celsius", Type: model.GaugeMetricType, Value: 21, Labels: map[string]string{"station": "LBBG", "issued": "030900", "first": "LBBG", "kind": "metar"}},
	}
	if !reflect.DeepEqual(set.Metrics, want) {
		t.Fatalf("got %#v\nwant %#v", set.Metrics, want)
	}
}

// value_map and scale apply to the group named value as they do to the first
// group, a regex may have that group and no other, and (?<value>...) names it
// as (?P<value>...) does.
func TestTheGroupNamedValueIsMappedAndScaled(t *testing.T) {
	minus := -1.0
	for name, tc := range map[string]struct {
		rule model.MetricRule
		body string
		want float64
	}{
		"scaled":               {model.MetricRule{Expression: `(\w+) M(?P<value>\d{2})/`, Scale: &minus}, "LBWN M05/M11", -5},
		"mapped":               {model.MetricRule{Expression: `(\w+) is (?P<value>\w+)`, ValueMap: map[string]float64{"up": 1, "*": 0}}, "db is up", 1},
		"mapped by the star":   {model.MetricRule{Expression: `(\w+) is (?P<value>\w+)`, ValueMap: map[string]float64{"up": 1, "*": 0}}, "db is gone", 0},
		"the only group":       {model.MetricRule{Expression: `load=(?P<value>[\d.]+)`}, "load=0.25", 0.25},
		"the first group too":  {model.MetricRule{Expression: `(?P<value>\d+) (\w+)`}, "7 jobs", 7},
		"named without the P":  {model.MetricRule{Expression: `(\w+)=(?<value>\d+)`}, "jobs=7", 7},
		"surrounded by blanks": {model.MetricRule{Expression: `(\w+)=(?P<value>\s*\d+\s*);`}, "jobs= 7 ;", 7},
	} {
		t.Run(name, func(t *testing.T) {
			tc.rule.Name = "v"
			c := regexRule(tc.rule)
			if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
				t.Fatal(err)
			}
			set, err := runBody(t, c, "text/plain", tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if len(set.Metrics) != 1 || set.Metrics[0].Value != tc.want {
				t.Fatalf("got %#v, want one series of %v", set.Metrics, tc.want)
			}
		})
	}
}

// A label may read the group that is the value, by its name or its number,
// and takes its text as written, as a label reading group 1 of a regex
// without a named value does.
func TestALabelMayReadTheValueGroup(t *testing.T) {
	c := regexRule(model.MetricRule{Name: "v", Expression: `(\w+)=(?P<value>\d+)`, Labels: []model.LabelRule{
		{Name: "by_name", Expression: "value"}, {Name: "by_number", Expression: "2"},
	}})
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, err := runBody(t, c, "text/plain", "jobs=07")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"by_name": "07", "by_number": "07"}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 7 || !reflect.DeepEqual(set.Metrics[0].Labels, want) {
		t.Fatalf("got %#v, want 7 with %v", set.Metrics, want)
	}
	first := regexRule(model.MetricRule{Name: "v", Expression: `(\d+) (\w+)`, Labels: []model.LabelRule{{Name: "text", Expression: "1"}}})
	set, err = runBody(t, first, "text/plain", "07 jobs")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Labels["text"] != "07" {
		t.Fatalf("got %#v, want the label to be the first group's text", set.Metrics)
	}
}

// A match whose group named value took no part in it, or captured only
// blanks, is a missing value of that match alone, whatever the first group
// captured: the error names the group, an optional rule leaves the match
// out, and the other matches keep their series.
func TestAValueGroupThatCapturedNothingIsAMissingValue(t *testing.T) {
	optional := false
	for name, expression := range map[string]string{
		"took no part":      `(\w+)=(?P<value>\d+)?;`,
		"captured nothing":  `(\w+)=(?P<value>\d*);`,
		"captured a blank":  `(\w+)=(?P<value>[\d ]*);`,
		"value is not last": `(\w+):(?P<value>\d+)?=(\w+);`,
	} {
		t.Run(name, func(t *testing.T) {
			body := "a=1;b=;c=3;"
			if name == "captured a blank" {
				body = "a=1;b= ;c=3;"
			}
			if name == "value is not last" {
				body = "x:1=a;x:=b;x:3=c;"
			}
			rule := model.MetricRule{Name: "v", Expression: expression, ErrorMode: model.ErrorModeFail, Labels: []model.LabelRule{{Name: "key", Expression: "1"}}}
			if name == "value is not last" {
				rule.Labels[0].Expression = "3"
			}
			_, err := runBody(t, regexRule(rule), "text/plain", body)
			if !errors.Is(err, model.ErrMissingValue) || !strings.Contains(err.Error(), `regex for metric "v" matched, but its capture group named value captured no value`) {
				t.Fatalf("got %v, want a missing value naming the group", err)
			}
			rule.Required = &optional
			set, err := runBody(t, regexRule(rule), "text/plain", body)
			if err != nil {
				t.Fatal(err)
			}
			if len(set.Metrics) != 2 || set.Metrics[0].Value != 1 || set.Metrics[0].Labels["key"] != "a" || set.Metrics[1].Value != 3 || set.Metrics[1].Labels["key"] != "c" {
				t.Fatalf("got %#v, want the two matches with a value", set.Metrics)
			}
		})
	}
	// Text in the group that is no number is a failure of the rule, not a
	// missing value, as it is in the first group.
	_, err := runBody(t, regexRule(model.MetricRule{Name: "v", Expression: `(\w+)=(?P<value>\w+);`, ErrorMode: model.ErrorModeFail, Required: &optional}), "text/plain", "a=up;")
	if err == nil || errors.Is(err, model.ErrMissingValue) || !strings.Contains(err.Error(), `value "up" is not a number`) {
		t.Fatalf("got %v, want text that is no number refused", err)
	}
}

// A regex needs a group for its value, and can have one group named value:
// both are refused when the configuration loads, in words that say which
// group is the value. A group named value in any place, alone or among
// others, loads.
func TestTheValueGroupIsCheckedWhenTheConfigurationLoads(t *testing.T) {
	c := &model.Collector{Name: "text", Transform: model.TransformConfig{Type: "regex"}}
	for expression, want := range map[string]string{
		`requests=\d+`:                          `collector "text" metric "requests" regex "requests=\\d+" has no capture group; the value is the capture group named value, as in '(?P<value>\d+)', or else the first, so wrap the number in one, such as 'requests=(\d+)'`,
		`(?:requests)=\d+`:                      `has no capture group`,
		`a=(?P<value>\d+)|b=(?P<value>\d+)`:     `collector "text" metric "requests" regex "a=(?P<value>\\d+)|b=(?P<value>\\d+)" has 2 capture groups named value; the group of that name is the value, so the regex can have one: name the others something else, or leave them unnamed`,
		`(?P<value>a)(?P<value>b)(?P<value>c)`:  `has 3 capture groups named value`,
		`requests=(\d+)`:                        "",
		`requests=(?P<value>\d+)`:               "",
		`(\w+)=(?P<value>\d+)`:                  "",
		`(?P<value>\d+) (?P<name>\w+) (\w+)`:    "",
		`(?P<name>\w+)=(?P<name>\d+)`:           "",
		`(?P<Value>\w+)=(?P<values>\d+)`:        "",
		`(?P<value>\d+)|(?P<other>(?P<v>\d+)x)`: "",
	} {
		err := CheckMetricRule(c, &model.MetricRule{Name: "requests", Expression: expression})
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: refused: %v", expression, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", expression, err, want)
		}
	}
	// A label still has to name a group the regex has, the value's included.
	rule := &model.MetricRule{Name: "requests", Expression: `(\w+)=(?P<value>\d+)`, Labels: []model.LabelRule{{Name: "a", Expression: "value"}, {Name: "b", Expression: "2"}, {Name: "c", Expression: "3"}}}
	err := CheckMetricRule(c, rule)
	if err == nil || !strings.Contains(err.Error(), `label "c" refers to capture group "3", which the regex does not have`) || strings.Contains(err.Error(), `label "a"`) || strings.Contains(err.Error(), `label "b"`) {
		t.Errorf("got %v, want the label of a third group refused alone", err)
	}
}

// formerTransformRegex and formerRegexSeries are the regex transform as it
// was before a group could be named value, for the differential test below.
func formerTransformRegex(ctx context.Context, text string, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		re, err := expr.CompileRegex(rule.Expression)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q regex: %w", rule.Name, err))
		}
		matches, found := regexMatches(ctx, re, text)
		match, ok := matches()
		if !ok {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("regex for metric %q matched no text", rule.Name), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		captures := labelCaptures(rule.Labels, re.SubexpNames())
		out.Metrics = growSeries(ctx, out.Metrics, found)
		for ; ok; match, ok = matches() {
			if err := interrupted(ctx, rule); err != nil {
				return nil, err
			}
			if err := formerRegexSeries(ctx, out, text, match, captures, rule, c); err != nil {
				return nil, err
			}
		}
	}
	return noSeriesIsNil(out), nil
}

func formerRegexSeries(ctx context.Context, out *model.MetricSet, text string, match []int, captures []int, rule model.MetricRule, c *model.Collector) error {
	if match[2] < 0 || isBlank(text[match[2]:match[3]]) {
		if requiredRule(rule, c) {
			missing := model.MarkError(fmt.Errorf("regex for metric %q matched, but its first capture group captured no value", rule.Name), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				return nil
			}
			return ruleFailure(c, rule, missing)
		}
		return nil
	}
	n, err := ruleTextValue(rule, text[match[2]:match[3]])
	if err != nil {
		if handleMetricError(ctx, c, rule, err) {
			return nil
		}
		return ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
	}
	labels := make(map[string]string, len(rule.Labels))
	for i, label := range rule.Labels {
		if label.Static() {
			labels[label.Name] = label.Value
			continue
		}
		index := captures[i]
		if index >= 0 && 2*index+1 < len(match) && match[2*index] >= 0 {
			labels[label.Name] = text[match[2*index]:match[2*index+1]]
		}
	}
	if missing := missingRequiredLabel(rule, labels); missing != nil {
		if handleMetricError(ctx, c, rule, missing) {
			return nil
		}
		return ruleFailure(c, rule, missing)
	}
	if err := takeSeries(ctx); err != nil {
		return err
	}
	out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
	return nil
}

// A regex without a group named value gives what it gave before one could be
// named: the same series in the same order, the same labels, and the same
// error word for word, for every expression, text, error mode and
// requirement of the table — groups that are optional, blank, named
// otherwise, nested, read by labels that exist and that do not, mapped and
// scaled values, and text that is no number.
func TestARegexWithoutAValueGroupReadsAsBefore(t *testing.T) {
	half := 0.5
	optional := false
	expressions := []string{
		`(\d+)`, `v=(\d+)?;`, `v=(\d*);`, `v=( *\d* *);`, `(?m)^(\w+) (\d+)$`, `(?m)^(?P<name>\w+) (?P<n>\d+)$`,
		`(?P<n>\d+)`, `(?P<Value>\d+)`, `(?P<values>\d+)`, `(?P<val>(?P<ue>\d+))`, `((\w)(\d))`, `(\w+)=(\w+)`,
		`(?:x|(\d+)) (\w+)`, `(\d+)|(\w+)`, `()`, `(?P<name>)(\d+)`, `no match (\d+)`, `(?i)(up|down)`,
	}
	texts := []string{
		"", "7", "v=1;v=;v=3;", "v= ;v= 4 ;", "web 3\ndb 5\nqueue x\n", "a=1 b=up c=3", "x jobs 12 done", "state: UP\nstate: down\n",
		"1 2 3 4 5 6 7 8 9", "no match here", "v=9999999999999999999999;", "  42  ",
	}
	labelSets := [][]model.LabelRule{
		nil,
		{{Name: "one", Expression: "1"}, {Name: "two", Expression: "2"}, {Name: "kind", Value: "k"}},
		{{Name: "name", Expression: "name"}, {Name: "none", Expression: "9"}, {Name: "whole", Expression: "0"}},
		{{Name: "need", Expression: "2", Required: true}},
	}
	variants := []model.MetricRule{
		{},
		{Scale: &half},
		{ValueMap: map[string]float64{"up": 1, "UP": 2, "7": 70}},
		{ValueMap: map[string]float64{"down": 0, "*": -1}, Scale: &half},
		{Required: &optional},
	}
	compared := 0
	for _, expression := range expressions {
		if re, err := expr.CompileRegex(expression); err != nil || re.SubexpIndex(regexValueName) >= 0 {
			t.Fatalf("%s: %v, or it has a group named value", expression, err)
		}
		for _, text := range texts {
			for _, labels := range labelSets {
				for _, variant := range variants {
					for _, mode := range []string{model.ErrorModeFail, model.ErrorModeLog, model.ErrorModeIgnore} {
						rule := variant
						rule.Name, rule.Type, rule.Expression, rule.Labels, rule.ErrorMode = "m", model.GaugeMetricType, expression, labels, mode
						c := &model.Collector{Name: "text", Transform: model.TransformConfig{Type: "regex"}}
						ctx := LeaveRuleLoggingToCaller(context.Background())
						got, gotErr := transformRegex(ctx, text, []model.MetricRule{rule}, c)
						want, wantErr := formerTransformRegex(ctx, text, []model.MetricRule{rule}, c)
						if !reflect.DeepEqual(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
							t.Fatalf("%s on %q, labels %v, %+v under %s:\ngot  %+v, %v\nwant %+v, %v", expression, text, labels, variant, mode, got, gotErr, want, wantErr)
						}
						compared++
					}
				}
			}
		}
	}
	if compared != len(expressions)*len(texts)*len(labelSets)*len(variants)*3 {
		t.Fatalf("compared %d cases", compared)
	}
}
