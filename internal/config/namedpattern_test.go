package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// namedPatternTwice is what the load says two prometheus rules do of which
// one passes on the metric of its name and the other's expression matches
// that name, where the rule with the expression makes a series of every
// series it matches or fails the scrape: as it has said it since such a
// pair was first refused, word for word.
const namedPatternTwice = `, so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series`

// namedPatternSaid is what the load says of two prometheus rules it
// refuses, at earlier and later of the collector's metrics, which the load
// counts from 1, of which the one at named passes on the metric of its
// name, and the other has the expression, which matches that name. The
// rules are as they are written.
//
// Where the rule with the expression can fail on a series and carry on —
// its error_mode is not fail, and it sets a type, a scale or a required
// label — not every series need be made twice, and the load names what the
// rule sets and says that the series it does not fail on are; and that the
// scrape fails on inconsistent types too, where one of the rules sets a
// type the other does not.
func namedPatternSaid(collector string, earlier, later, named int, rules []model.MetricRule) string {
	matching := earlier + later - named
	name, pattern := rules[named].Name, rules[matching]
	twice := namedPatternTwice
	if !strings.EqualFold(strings.TrimSpace(pattern.ErrorMode), "fail") {
		var sets []string
		if pattern.Type != "" {
			sets = append(sets, "the type")
		}
		if pattern.Scale != nil {
			sets = append(sets, "the scale")
		}
		seen := map[string]bool{}
		for _, label := range pattern.Labels {
			if label.Required && !seen[label.Name] {
				seen[label.Name] = true
				sets = append(sets, `the required label "`+label.Name+`"`)
			}
		}
		of := fmt.Sprintf(" of rule %d ", matching+1)
		switch len(sets) {
		case 0:
		case 1:
			twice = "; " + sets[0] + of + "does"
		case 2:
			twice = "; " + sets[0] + " and " + sets[1] + of + "do"
		default:
			twice = "; " + strings.Join(sets[:len(sets)-1], ", ") + " and " + sets[len(sets)-1] + of + "do"
		}
		if len(sets) > 0 {
			twice += fmt.Sprintf(" not keep it from the metric, since every series of the metric that rule %d does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series", matching+1)
			if rules[named].Type != pattern.Type {
				twice += " or, where the two rules give the metric different types, as a metric of inconsistent types"
			}
		}
	}
	return fmt.Sprintf("collector %q metrics rule %d and rule %d both pass on metric %q: rule %d passes on the metric of that name, the expression %q of rule %d matches that name, and their labels are alike%s; take one of the two out, write the expression so that it does not match %q, or tell their series apart by a label, as with a static label that has another value in each", collector, earlier+1, later+1, name, named+1, pattern.Expression, matching+1, twice, name)
}

// keptToItsType says, by the book and apart from the check, whether a
// prometheus rule with an expression, as it is written, leaves the metric
// of a rule of a name to that rule by its type, so that the load takes the
// two whatever else they say: its type is histogram or summary, which
// applies to a metric of that type and to no other; its error_mode is not
// fail, so that it carries on without the metrics of other types; and the
// rule of the name does not set that same type.
func keptToItsType(named, pattern model.MetricRule) bool {
	switch pattern.Type {
	case model.HistogramMetricType, model.SummaryMetricType:
		return !strings.EqualFold(strings.TrimSpace(pattern.ErrorMode), "fail") && named.Type != pattern.Type
	}
	return false
}

// checkRulesDifferBeforeNamedPatterns is checkRulesDiffer as it was before
// a prometheus rule of a name beside one whose expression matches that name
// was refused, kept as an oracle.
func checkRulesDifferBeforeNamedPatterns(x *model.Collector, sound []bool) []error {
	if x.Transform.Type == "python" {
		return nil
	}
	// Rules alike in the four texts are few, and only those have their
	// labels and value_maps compared.
	type texts struct{ name, expression, items, timeFormat string }
	var errs []error
	alike := map[texts][]int{}
	for i := range x.Metrics {
		if !sound[i] {
			continue
		}
		r := &x.Metrics[i]
		key := texts{r.Name, r.Expression, r.Items, r.TimeFormat}
		first := slices.IndexFunc(alike[key], func(earlier int) bool {
			other := &x.Metrics[earlier]
			return maps.Equal(r.ValueMap, other.ValueMap) && slices.EqualFunc(ruleLabels(x, r), ruleLabels(x, other), func(a, b model.LabelRule) bool {
				return a.Name == b.Name && a.Value == b.Value && a.Expression == b.Expression
			})
		})
		if first < 0 {
			alike[key] = append(alike[key], i)
			continue
		}
		of := fmt.Sprintf("of metric %q", r.Name)
		if r.Name == "" {
			of = fmt.Sprintf("of the metrics that match %q", r.Expression)
		}
		errs = append(errs, fmt.Errorf("collector %q metrics rule %d and rule %d are the same rule %s: alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each", x.Name, alike[key][first]+1, i+1, of))
	}
	return errs
}

// validateMetricRulesBeforeNamedPatterns is validateMetricRules as it was
// then, with that check. The checks of one rule it calls, and of the label
// value maps of the rules together, are as they were: the change is no part
// of them.
func validateMetricRulesBeforeNamedPatterns(_ *model.Config, x *model.Collector) error {
	var errs []error
	sound := make([]bool, len(x.Metrics))
	for i := range x.Metrics {
		err := validateMetricRule(x, i)
		if err == nil {
			err = checkPrometheusRuleSelects(x, i)
		}
		sound[i] = err == nil
		errs = append(errs, err)
	}
	errs = append(errs, transform.CheckLabelValueMapsAgree(x))
	errs = append(errs, checkRulesDifferBeforeNamedPatterns(x, sound)...)
	return model.JoinProblems(errs...)
}

// couldFailOnASeries is canFailOnASeries as it was while a rule of a name
// beside a rule whose expression matches that name was refused whatever
// that rule sets, kept as an oracle.
func couldFailOnASeries(r *model.MetricRule) []string {
	var settings []string
	if r.Type != "" {
		settings = append(settings, "the type")
	}
	if r.Scale != nil {
		settings = append(settings, "the scale")
	}
	for _, label := range r.Labels {
		if !label.Required {
			continue
		}
		// A label written twice is named once.
		if setting := fmt.Sprintf("the required label %q", label.Name); !slices.Contains(settings, setting) {
			settings = append(settings, setting)
		}
	}
	return settings
}

// checkNoPatternRepeatsANameWhateverItsRuleSets is
// checkNoPatternRepeatsAName as it was then, kept as an oracle: no type of
// the rule with the expression kept the pair from being refused.
func checkNoPatternRepeatsANameWhateverItsRuleSets(x *model.Collector, distinct []int) []error {
	if x.Transform.Type != "prometheus" {
		return nil
	}
	var errs []error
	for at, later := range distinct {
		for _, earlier := range distinct[:at] {
			named, matching := earlier, later
			if x.Metrics[named].Expression != "" {
				named, matching = later, earlier
			}
			n, m := &x.Metrics[named], &x.Metrics[matching]
			if n.Name == "" || n.Expression != "" || m.Expression == "" || m.Name != "" && m.Name != n.Name {
				continue
			}
			if pattern, err := expr.CompileRegex(m.Expression); err != nil || !pattern.MatchString(n.Name) || !alikeInLabels(x, n, m) {
				continue
			}
			twice := ", so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series"
			if settings := couldFailOnASeries(m); len(settings) > 0 && m.ErrorMode != model.ErrorModeFail {
				// The rule carries on without a series it fails on, so not
				// every series need be made twice; and the two make one of
				// two types where one sets a type the other has not.
				does, list := "does", settings[0]
				if last := len(settings) - 1; last > 0 {
					does, list = "do", strings.Join(settings[:last], ", ")+" and "+settings[last]
				}
				types := ""
				if n.Type != m.Type {
					types = " or, where the two rules give the metric different types, as a metric of inconsistent types"
				}
				twice = fmt.Sprintf("; %s of rule %d %s not keep it from the metric, since every series of the metric that rule %d does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series%s", list, matching+1, does, matching+1, types)
			}
			errs = append(errs, fmt.Errorf("collector %q metrics rule %d and rule %d both pass on metric %q: rule %d passes on the metric of that name, the expression %q of rule %d matches that name, and their labels are alike%s; take one of the two out, write the expression so that it does not match %q, or tell their series apart by a label, as with a static label that has another value in each", x.Name, earlier+1, later+1, n.Name, named+1, m.Expression, matching+1, twice, n.Name))
		}
	}
	return errs
}

// checkRulesDifferWhateverAPatternSets is checkRulesDiffer as it was then,
// with that check.
func checkRulesDifferWhateverAPatternSets(x *model.Collector, sound []bool) []error {
	if x.Transform.Type == "python" {
		return nil
	}
	// Rules alike in the four texts are few, and only those have their
	// labels and value_maps compared.
	type texts struct{ name, expression, items, timeFormat string }
	var errs []error
	alike := map[texts][]int{}
	// The rules that repeat no earlier one, in their order.
	var distinct []int
	for i := range x.Metrics {
		if !sound[i] {
			continue
		}
		r := &x.Metrics[i]
		key := texts{r.Name, r.Expression, r.Items, r.TimeFormat}
		first := slices.IndexFunc(alike[key], func(earlier int) bool {
			other := &x.Metrics[earlier]
			return maps.Equal(r.ValueMap, other.ValueMap) && alikeInLabels(x, r, other)
		})
		if first < 0 {
			alike[key] = append(alike[key], i)
			distinct = append(distinct, i)
			continue
		}
		of := fmt.Sprintf("of metric %q", r.Name)
		if r.Name == "" {
			of = fmt.Sprintf("of the metrics that match %q", r.Expression)
		}
		errs = append(errs, fmt.Errorf("collector %q metrics rule %d and rule %d are the same rule %s: alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each", x.Name, alike[key][first]+1, i+1, of))
	}
	return append(errs, checkNoPatternRepeatsANameWhateverItsRuleSets(x, distinct)...)
}

// validateMetricRulesWhateverAPatternSets is validateMetricRules as it was
// then, with that check. The checks of one rule it calls, and of the label
// value maps of the rules together, are as they were: the change is no part
// of them.
func validateMetricRulesWhateverAPatternSets(_ *model.Config, x *model.Collector) error {
	var errs []error
	sound := make([]bool, len(x.Metrics))
	for i := range x.Metrics {
		err := validateMetricRule(x, i)
		if err == nil {
			err = checkPrometheusRuleSelects(x, i)
		}
		sound[i] = err == nil
		errs = append(errs, err)
	}
	errs = append(errs, transform.CheckLabelValueMapsAgree(x))
	errs = append(errs, checkRulesDifferWhateverAPatternSets(x, sound)...)
	return model.JoinProblems(errs...)
}

// patternRepeatsName says, by the book and apart from the check, whether of
// two rules of a prometheus collector that the checks of a rule take one
// passes on the metric of its name and the other's expression matches that
// name, the two giving its series one name and the same labels; and which
// of the two is the rule of the name, a or b. The expression is compiled by
// the standard library, as the transform compiles it, and matched anywhere
// in the name. The labels are the same as those of two rules that are the
// same rule are (theSameRule), which is asked of two rules of nothing but
// the labels. What else either rule says — a type, a scale, a label's
// required, an error_mode — is no part of it.
func patternRepeatsName(x *model.Collector, a, b model.MetricRule) (aIsNamed, repeats bool) {
	if x.Transform.Type != "prometheus" {
		return false, false
	}
	named, matching := a, b
	if aIsNamed = a.Expression == ""; !aIsNamed {
		named, matching = b, a
	}
	if named.Name == "" || named.Expression != "" || matching.Expression == "" {
		return false, false
	}
	if matching.Name != "" && matching.Name != named.Name {
		return false, false
	}
	pattern, err := regexp.Compile(matching.Expression)
	if err != nil || !pattern.MatchString(named.Name) {
		return false, false
	}
	return aIsNamed, theSameRule(x, model.MetricRule{Labels: named.Labels}, model.MetricRule{Labels: matching.Labels})
}

// A prometheus rule without an expression passes on the metric of its name,
// and a rule with one passes on every metric whose name the expression
// matches, anywhere in it, under the metric's own name or, when the rule
// has a name too, under that. Where one rule passes on the metric of its
// name and another's expression matches that very name, and the two give
// its series one name and the same labels, each makes every series of the
// metric: a scrape of a target that has it failed on a duplicate metric
// series, every time, and the load took the pair. It refuses them now,
// naming both by their places, the metric and the expression.
//
// For each pair here the checks as they were take both rules; what a scrape
// then makes of an exposition is put through the validation that fails a
// scrape; and the load is asked what it says now. A pair that is refused
// gave a duplicate series: a name beside the name with its pattern and
// beside the pattern alone, in either order, beside a pattern of every
// metric, one matched inside the name and one in another case, with the
// same labels in another order, with what does not decide the series
// changed in the rule of the name, with a label cut in one and a rule
// between, and of a histogram. With another type in the rule of the name
// the scrape failed on the metric's two types before it came to the
// duplicate. Where the scale of the rule of the name cannot apply to the
// metric, a histogram, that rule failed on the metric instead of making
// it, and of a target that has not the metric it made nothing, missing its
// metric or, where it is not required, saying nothing: either way the rule
// of the name made a series of no scrape that passed. And a rule with an
// expression and error_mode fail fails the scrape where its scale cannot
// apply, and makes the series twice where it can.
//
// A rule with an expression that sets a scale, a type or a required label
// and carries on without the series it fails on, under log or ignore, is
// held against a rule of a name too: the pair is refused whatever the
// scrape made of it. That was a duplicate where the scale and the type
// apply and the label is there, and valid series, the rule of the name's
// alone, where the scale or the type cannot apply to the metric, a
// histogram, or the series have not the label
// (TestOnlyAPatternOfHistogramsOrSummariesIsTakenBesideANameItMatches has
// each by the metric's type). The load says of such a pair what the rule
// sets, and that the series it does not fail on are made twice.
//
// One such rule is taken beside the name: a rule whose type is histogram or
// summary, which applies to a metric of that type and to no other, so that
// under log or ignore the rule is about the histograms, or the summaries,
// and leaves a metric of another type to the rule of its name. `name: up`
// beside a pattern of every metric with `type: histogram` makes up once and
// the target's histogram once, with a scale in the rule of the name and
// with another type there too, and so does a rule that exports the
// summaries it matches under the name. The price is the target whose metric
// of that name is of that type: `name: lat` beside that pattern loads, and
// the scrape of the target, where lat is a histogram, fails on the
// duplicate. With that same type in the rule of the name the pair makes
// the metric twice or not at all, and is refused; and so it is under
// error_mode fail, where the rule fails the scrape on a metric of another
// type.
//
// Any other pair that is taken is one the configuration does not decide.
// Its series are valid when a label tells them apart, a constant with
// another value in each or a label more, when the expression does not match
// the name, when its rule exports what it matches under another name, when
// a rule exports another metric under the name, and when a label reads one
// the rule sets, written in another order. Or the scrape fails, on a
// duplicate:
// with two expressions that both match a metric the target has, a name
// under two rules with one expression, and a constant label the target's
// series has already.
func TestAPatternThatMatchesANamedRulesMetricMakesItsSeriesTwice(t *testing.T) {
	no := false
	two := 2.0
	static := func(name, value string) model.LabelRule { return model.LabelRule{Name: name, Value: value} }
	reads := func(name, expression string) model.LabelRule {
		return model.LabelRule{Name: name, Expression: expression}
	}
	const exposition = "# TYPE up gauge\nup{job=\"api\",b=\"target\"} 1\n# TYPE node_load1 gauge\nnode_load1 0.5\n# TYPE node_cpu counter\nnode_cpu{cpu=\"0\"} 5\n# TYPE lat histogram\nlat_bucket{le=\"1\"} 1\nlat_bucket{le=\"+Inf\"} 2\nlat_sum 3\nlat_count 2\n"
	const duplicateUp = `duplicate metric series "up"`
	type pair struct {
		rules []model.MetricRule
		// refused says the load refuses the rules at earlier and later, of
		// which the one at named is the rule of the name.
		refused               bool
		earlier, later, named int
		// invalid is what validation says of what a scrape makes of the
		// exposition, "" for valid series, and series how many it makes.
		invalid string
		series  int
	}
	for name, tc := range map[string]pair{
		"a name, and the name with its pattern": {[]model.MetricRule{{Name: "up"}, {Name: "up", Expression: "^up$"}}, true, 0, 1, 0, duplicateUp, 2},
		"a name, and its pattern alone":         {[]model.MetricRule{{Name: "up"}, {Expression: "^up$"}}, true, 0, 1, 0, duplicateUp, 2},
		"the pattern first":                     {[]model.MetricRule{{Expression: "^up$"}, {Name: "up"}}, true, 0, 1, 1, duplicateUp, 2},
		"a pattern of every metric":             {[]model.MetricRule{{Name: "up"}, {Expression: ".*"}}, true, 0, 1, 0, duplicateUp, 5},
		"a pattern matched inside the name":     {[]model.MetricRule{{Name: "node_load1"}, {Expression: "load"}}, true, 0, 1, 0, `duplicate metric series "node_load1"`, 2},
		"a pattern in another case":             {[]model.MetricRule{{Name: "up"}, {Expression: "(?i)^UP$"}}, true, 0, 1, 0, duplicateUp, 2},
		"the same labels in another order":      {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("service", "job"), static("site", "rack1")}}, {Expression: "^up", Labels: []model.LabelRule{static("site", "rack1"), reads("service", "job")}}}, true, 0, 1, 0, duplicateUp, 2},
		"what a rule says besides":              {[]model.MetricRule{{Name: "up", Description: "Whether it is up.", Scale: &two, Required: &no, ErrorMode: "ignore"}, {Expression: "^up$"}}, true, 0, 1, 0, duplicateUp, 2},
		"a label cut in one":                    {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("service", "job")}}, {Expression: "^up$", Labels: []model.LabelRule{{Name: "service", Expression: "job", Truncate: true}}}}, true, 0, 1, 0, duplicateUp, 2},
		"a label the named rule requires":       {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{{Name: "zone", Expression: "zone", Required: true}}}, {Expression: "^up$", Labels: []model.LabelRule{reads("zone", "zone")}}}, true, 0, 1, 0, "", 1},
		"a rule between them":                   {[]model.MetricRule{{Name: "up"}, {Name: "node_load1"}, {Expression: "^up$"}}, true, 0, 2, 0, duplicateUp, 3},
		"a histogram":                           {[]model.MetricRule{{Name: "lat"}, {Expression: "^lat"}}, true, 0, 1, 0, `duplicate metric series "lat"`, 2},
		"another type in one":                   {[]model.MetricRule{{Name: "up", Type: model.CounterMetricType}, {Expression: "^up$"}}, true, 0, 1, 0, `metric "up" has inconsistent types`, 2},
		"a scale that cannot apply":             {[]model.MetricRule{{Name: "lat", Scale: &two, ErrorMode: "ignore"}, {Expression: "^lat$"}}, true, 0, 1, 0, "", 1},
		"the pattern's scale, under fail":       {[]model.MetricRule{{Name: "up"}, {Expression: "^up$", Scale: &two, ErrorMode: "fail"}}, true, 0, 1, 0, duplicateUp, 2},
		"the pattern's type, under fail":        {[]model.MetricRule{{Name: "up"}, {Expression: "^up$", Type: model.CounterMetricType, ErrorMode: " Fail "}}, true, 0, 1, 0, `metric "up" has inconsistent types`, 2},
		"a name the target has not":             {[]model.MetricRule{{Name: "down"}, {Expression: "^down$"}}, true, 0, 1, 0, "", 0},
		"that name, not required":               {[]model.MetricRule{{Name: "down", Required: &no}, {Expression: ".*"}}, true, 0, 1, 0, "", 4},

		"the pattern's scale cannot apply":         {[]model.MetricRule{{Name: "lat"}, {Expression: ".*", Scale: &two, ErrorMode: "ignore"}}, true, 0, 1, 0, "", 4},
		"the pattern's type cannot apply":          {[]model.MetricRule{{Name: "lat"}, {Expression: "^lat", Type: model.GaugeMetricType}}, true, 0, 1, 0, "", 1},
		"the pattern's rule requires a label":      {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("zone", "zone")}}, {Expression: "^up$", Labels: []model.LabelRule{{Name: "zone", Expression: "zone", Required: true}}}}, true, 0, 1, 0, "", 1},
		"the pattern's scale applies":              {[]model.MetricRule{{Name: "up"}, {Expression: ".*", Scale: &two, ErrorMode: "ignore"}}, true, 0, 1, 0, duplicateUp, 4},
		"the pattern's type applies":               {[]model.MetricRule{{Name: "up"}, {Expression: "^up$", Type: model.GaugeMetricType}}, true, 0, 1, 0, duplicateUp, 2},
		"the label the pattern's rule requires is": {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("service", "job")}}, {Expression: "^up$", Labels: []model.LabelRule{{Name: "service", Expression: "job", Required: true}}}}, true, 0, 1, 0, duplicateUp, 2},
		"the pattern's scale, written first":       {[]model.MetricRule{{Expression: ".*", Scale: &two}, {Name: "lat"}}, true, 0, 1, 1, "", 4},
		"the pattern's type of histogram in both":  {[]model.MetricRule{{Name: "lat", Type: model.HistogramMetricType}, {Expression: ".*", Type: model.HistogramMetricType, ErrorMode: "ignore"}}, true, 0, 1, 0, `duplicate metric series "lat"`, 2},
		"the pattern's type of summary in both":    {[]model.MetricRule{{Expression: "^up", Type: model.SummaryMetricType}, {Name: "up", Type: model.SummaryMetricType}}, true, 0, 1, 1, "", 0},
		"a pattern of histograms, under fail":      {[]model.MetricRule{{Name: "lat"}, {Expression: "^lat$", Type: model.HistogramMetricType, ErrorMode: "FAIL"}}, true, 0, 1, 0, `duplicate metric series "lat"`, 2},

		"a pattern of every histogram":              {[]model.MetricRule{{Name: "up"}, {Expression: ".*", Type: model.HistogramMetricType, ErrorMode: "ignore"}}, false, 0, 0, 0, "", 2},
		"that pattern, written first":               {[]model.MetricRule{{Expression: ".*", Type: model.HistogramMetricType}, {Name: "up", Scale: &two}}, false, 0, 0, 0, "", 2},
		"another type in the rule of the name":      {[]model.MetricRule{{Name: "up", Type: model.CounterMetricType}, {Expression: ".*", Type: model.HistogramMetricType}}, false, 0, 0, 0, "", 2},
		"the other of the two types in it":          {[]model.MetricRule{{Name: "lat", Type: model.SummaryMetricType}, {Expression: ".*", Type: model.HistogramMetricType}}, false, 0, 0, 0, "", 1},
		"the summaries a pattern matches, as up":    {[]model.MetricRule{{Name: "up"}, {Name: "up", Expression: "^u", Type: model.SummaryMetricType}}, false, 0, 0, 0, "", 1},
		"a pattern of histograms with a scale":      {[]model.MetricRule{{Name: "lat"}, {Expression: ".*", Type: model.HistogramMetricType, Scale: &two}}, false, 0, 0, 0, "", 1},
		"a pattern of every histogram, of that one": {[]model.MetricRule{{Name: "lat"}, {Expression: ".*", Type: model.HistogramMetricType, ErrorMode: "ignore"}}, false, 0, 0, 0, `duplicate metric series "lat"`, 2},

		"a label of another value in each":          {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{static("copy", "1")}}, {Expression: "^up$", Labels: []model.LabelRule{static("copy", "2")}}}, false, 0, 0, 0, "", 2},
		"a label more in one":                       {[]model.MetricRule{{Name: "up"}, {Expression: ".*", Labels: []model.LabelRule{static("site", "rack1")}}}, false, 0, 0, 0, "", 5},
		"a pattern that does not match the name":    {[]model.MetricRule{{Name: "up"}, {Expression: "^node_"}}, false, 0, 0, 0, "", 3},
		"a pattern of a longer name":                {[]model.MetricRule{{Name: "node"}, {Expression: "^node_load1$"}}, false, 0, 0, 0, "", 1},
		"the pattern's series under another name":   {[]model.MetricRule{{Name: "up"}, {Name: "alive", Expression: "^up$"}}, false, 0, 0, 0, "", 2},
		"another metric under the name":             {[]model.MetricRule{{Name: "up"}, {Name: "up", Expression: "^node_load1$"}}, false, 0, 0, 0, "", 2},
		"a label that reads one it sets, otherwise": {[]model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("a", "b"), static("b", "x")}}, {Expression: "^up$", Labels: []model.LabelRule{static("b", "x"), reads("a", "b")}}}, false, 0, 0, 0, "", 2},
		"two patterns that both match a metric":     {[]model.MetricRule{{Expression: "^node_"}, {Expression: "^node_cpu"}}, false, 0, 0, 0, `duplicate metric series "node_cpu"`, 3},
		"a name under two rules with one pattern":   {[]model.MetricRule{{Name: "up", Expression: "^up$"}, {Expression: "^up$"}}, false, 0, 0, 0, duplicateUp, 2},
		"a constant label the target's series has":  {[]model.MetricRule{{Name: "up"}, {Expression: "^up$", Labels: []model.LabelRule{static("job", "api")}}}, false, 0, 0, 0, duplicateUp, 2},
	} {
		x := &model.Collector{Name: "node", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
		applyLimitDefaults(&x.Limits)
		before, now := withRules(x, tc.rules), withRules(x, tc.rules)
		if err := validateMetricRulesBeforeNamedPatterns(nil, before); err != nil {
			t.Errorf("%s: the rules were refused: %v", name, err)
			continue
		}
		series, invalid := seriesOf(t, before, "text/plain", exposition)
		if said := fmt.Sprint(invalid); len(series) != tc.series || (invalid == nil) != (tc.invalid == "") || invalid != nil && said != tc.invalid {
			t.Errorf("%s: a scrape makes %d series, %+v, of which validation says %v\nwant %d series and %q", name, len(series), series, invalid, tc.series, tc.invalid)
		}
		err := validateMetricRules(nil, now)
		if !tc.refused {
			if err != nil {
				t.Errorf("%s: the load says %v", name, err)
			}
			continue
		}
		if want := namedPatternSaid("node", tc.earlier, tc.later, tc.named, tc.rules); err == nil || err.Error() != want {
			t.Errorf("%s: the load says %v\nwant %s", name, err, want)
		}
	}
}

// A rule with an expression that sets a type, a scale or a required label
// can fail on a series it matches, and under error_mode log or ignore
// carries on without it. Beside a rule of a name its expression matches,
// with the same labels, what a scrape makes is the target's to say. The
// load refuses such a pair whatever the target has, but for one: a rule
// whose type is histogram or summary is about the metrics of that type
// alone, and is taken beside the name.
//
// What a scrape makes of each such pair is asked here of a target whose
// metric is a gauge, a counter, untyped, a histogram and a summary, the
// rules being as the check took them before it held such pairs. A type of
// gauge, counter or untyped applies to the first three: each rule makes the
// series, and the scrape fails, on a duplicate where the type is the
// metric's own and on inconsistent types where it is not; a histogram and a
// summary keep their type, the rule fails on them, and the scrape passes
// with the series the rule of the name made. A scale fails the scrape of
// the first three and passes on the other two. A required label fails every
// scrape whose series have it, and none whose series have not. Where the
// rule of the name cannot make the series either, by a type of its own or a
// label it requires, the scrape passes with no series of the metric. So the
// pairs that fail every scrape of an ordinary target and the pairs that
// pass some are refused alike.
//
// A type of histogram or summary applies to a metric of that type alone:
// the rule fails on every other, and the scrape passes with the series the
// rule of the name made, of four of the five. That pair is taken, and
// fails the scrape of the one target whose metric is of that type, on the
// duplicate. It is taken with a scale or a required label besides, which
// keep the rule from more series and from no fewer: with a scale it fails
// on the histogram too, and makes no series of any of the five. And it is
// taken with another type in the rule of the name, a gauge or the other of
// the two, where no scrape has a series twice: of a metric of the
// pattern's type the rule of the name fails and the rule with the
// expression makes the one series, as a scrape under error_mode fail,
// which that rule does not fail, shows. With the same type in both rules
// the rule of the name makes the metric only where the other makes it too,
// a duplicate of a metric of that type and no series of any other, and the
// pair is refused — with a scale in the rule with the expression as well,
// though that rule then makes no series and no scrape has one twice, the
// type alone deciding.
//
// The check as it was, which refused every one of these pairs (kept as an
// oracle, validateMetricRulesWhateverAPatternSets), named what the rule
// with the expression sets — the type, the scale, each required label once
// — said that the series the rule does not fail on are made by each rule,
// and that the scrape then fails on a duplicate, or on inconsistent types
// where one rule sets a type the other does not. The load says so still of
// each pair it refuses, word for word, and says nothing of the pairs it
// takes, which are those the book names (keptToItsType). Under error_mode
// fail the rule fails the scrape where it fails on a series, so no scrape
// of such a pair passes but one the rule of the name fails on: every pair
// is refused then, in the words it was.
func TestOnlyAPatternOfHistogramsOrSummariesIsTakenBesideANameItMatches(t *testing.T) {
	two := 2.0
	reads := func(name string, required bool) model.LabelRule {
		return model.LabelRule{Name: name, Expression: name, Required: required}
	}
	targets := [5]struct{ metric, exposition string }{
		{"a gauge", "# TYPE m gauge\nm{job=\"api\"} 1\n"},
		{"a counter", "# TYPE m counter\nm{job=\"api\"} 1\n"},
		{"untyped", "m{job=\"api\"} 1\n"},
		{"a histogram", "# TYPE m histogram\nm_bucket{job=\"api\",le=\"1\"} 1\nm_bucket{job=\"api\",le=\"+Inf\"} 2\nm_sum{job=\"api\"} 3\nm_count{job=\"api\"} 2\n"},
		{"a summary", "# TYPE m summary\nm{job=\"api\",quantile=\"0.5\"} 1\nm_sum{job=\"api\"} 3\nm_count{job=\"api\"} 2\n"},
	}
	// What a scrape makes of the metric: each rule's series, which fails it
	// as a duplicate or as two types, the series of the rule of the name
	// alone, that of the rule with the expression alone, the rule of the
	// name failing on the metric, or none.
	const (
		twice = `duplicate metric series "m"`
		types = `metric "m" has inconsistent types`
		once  = "one series"
		other = "one series, the pattern's rule's"
		none  = "no series"
	)
	m := model.MetricRule{Name: "m"}
	typed := func(metricType model.MetricType) model.MetricRule {
		return model.MetricRule{Name: "m", Type: metricType}
	}
	pattern := func(rule model.MetricRule) model.MetricRule {
		rule.Expression = "^m$"
		return rule
	}
	histogram, summary := model.HistogramMetricType, model.SummaryMetricType
	shapes, takenShapes := 0, 0
	for name, tc := range map[string]struct {
		named, pattern model.MetricRule
		// taken says the load takes the pair under log and ignore.
		taken bool
		// sets is what the check as it was named of the rule with the
		// expression, and the load names where it refuses the pair, and
		// twoTypes whether it says the rules may give the metric two types.
		sets     string
		twoTypes bool
		made     [5]string
	}{
		"type: gauge":   {m, pattern(model.MetricRule{Type: model.GaugeMetricType}), false, "the type of rule 2 does", true, [5]string{twice, types, types, once, once}},
		"type: counter": {m, pattern(model.MetricRule{Type: model.CounterMetricType}), false, "the type of rule 2 does", true, [5]string{types, twice, types, once, once}},
		"type: untyped": {m, pattern(model.MetricRule{Type: model.UntypedMetricType}), false, "the type of rule 2 does", true, [5]string{types, types, twice, once, once}},
		"a scale":       {m, pattern(model.MetricRule{Scale: &two}), false, "the scale of rule 2 does", false, [5]string{twice, twice, twice, once, once}},
		"a required label the series have": {
			model.MetricRule{Name: "m", Labels: []model.LabelRule{reads("job", false)}}, pattern(model.MetricRule{Labels: []model.LabelRule{reads("job", true)}}),
			false, `the required label "job" of rule 2 does`, false, [5]string{twice, twice, twice, twice, twice},
		},
		"a required label the series have not": {
			model.MetricRule{Name: "m", Labels: []model.LabelRule{reads("zone", false)}}, pattern(model.MetricRule{Labels: []model.LabelRule{reads("zone", true)}}),
			false, `the required label "zone" of rule 2 does`, false, [5]string{once, once, once, once, once},
		},
		"a label both rules require": {
			model.MetricRule{Name: "m", Labels: []model.LabelRule{reads("zone", true)}}, pattern(model.MetricRule{Labels: []model.LabelRule{reads("zone", true)}}),
			false, `the required label "zone" of rule 2 does`, false, [5]string{none, none, none, none, none},
		},
		"a type and a scale": {m, pattern(model.MetricRule{Type: model.GaugeMetricType, Scale: &two}), false, "the type and the scale of rule 2 do", true, [5]string{twice, types, types, once, once}},
		"a type, a scale and two required labels, one written twice": {
			model.MetricRule{Name: "m", Labels: []model.LabelRule{reads("job", false), reads("zone", false), reads("job", false)}},
			pattern(model.MetricRule{Type: model.CounterMetricType, Scale: &two, Labels: []model.LabelRule{reads("job", true), reads("zone", true), reads("job", true)}}),
			false, `the type, the scale, the required label "job" and the required label "zone" of rule 2 do`, true, [5]string{once, once, once, once, once},
		},
		"a type in the rule of the name alone": {typed(model.CounterMetricType), pattern(model.MetricRule{Scale: &two}), false, "the scale of rule 2 does", true, [5]string{types, twice, types, none, none}},
		"one type in both rules":               {typed(model.CounterMetricType), pattern(model.MetricRule{Type: model.CounterMetricType}), false, "the type of rule 2 does", false, [5]string{twice, twice, twice, none, none}},

		// A rule about the histograms alone, or the summaries.
		"type: histogram":             {m, pattern(model.MetricRule{Type: histogram}), true, "the type of rule 2 does", true, [5]string{once, once, once, twice, once}},
		"type: summary":               {m, pattern(model.MetricRule{Type: summary}), true, "the type of rule 2 does", true, [5]string{once, once, once, once, twice}},
		"type: histogram and a scale": {m, pattern(model.MetricRule{Type: histogram, Scale: &two}), true, "the type and the scale of rule 2 do", true, [5]string{once, once, once, once, once}},
		"type: summary and a scale":   {m, pattern(model.MetricRule{Type: summary, Scale: &two}), true, "the type and the scale of rule 2 do", true, [5]string{once, once, once, once, once}},
		"type: histogram and a required label the series have": {
			model.MetricRule{Name: "m", Labels: []model.LabelRule{reads("job", false)}}, pattern(model.MetricRule{Type: histogram, Labels: []model.LabelRule{reads("job", true)}}),
			true, `the type and the required label "job" of rule 2 do`, true, [5]string{once, once, once, twice, once},
		},
		"type: histogram and a required label the series have not": {
			model.MetricRule{Name: "m", Labels: []model.LabelRule{reads("zone", false)}}, pattern(model.MetricRule{Type: histogram, Labels: []model.LabelRule{reads("zone", true)}}),
			true, `the type and the required label "zone" of rule 2 do`, true, [5]string{once, once, once, once, once},
		},
		"type: histogram beside type: gauge":   {typed(model.GaugeMetricType), pattern(model.MetricRule{Type: histogram}), true, "the type of rule 2 does", true, [5]string{once, once, once, other, none}},
		"type: summary beside type: counter":   {typed(model.CounterMetricType), pattern(model.MetricRule{Type: summary}), true, "the type of rule 2 does", true, [5]string{once, once, once, none, other}},
		"type: histogram beside type: summary": {typed(summary), pattern(model.MetricRule{Type: histogram}), true, "the type of rule 2 does", true, [5]string{none, none, none, other, once}},
		"type: summary beside type: histogram": {typed(histogram), pattern(model.MetricRule{Type: summary}), true, "the type of rule 2 does", true, [5]string{none, none, none, once, other}},
		"type: histogram beside a scale":       {model.MetricRule{Name: "m", Scale: &two}, pattern(model.MetricRule{Type: histogram}), true, "the type of rule 2 does", true, [5]string{once, once, once, other, none}},
		"type: histogram in both rules":        {typed(histogram), pattern(model.MetricRule{Type: histogram}), false, "the type of rule 2 does", false, [5]string{none, none, none, twice, none}},
		"type: summary in both rules":          {typed(summary), pattern(model.MetricRule{Type: summary}), false, "the type of rule 2 does", false, [5]string{none, none, none, none, twice}},
		"type: histogram in both, and a scale": {typed(histogram), pattern(model.MetricRule{Type: histogram, Scale: &two}), false, "the type and the scale of rule 2 do", false, [5]string{none, none, none, once, none}},
		"type: histogram in both, and its label": {
			model.MetricRule{Name: "m", Type: histogram, Labels: []model.LabelRule{reads("job", false)}}, pattern(model.MetricRule{Type: histogram, Labels: []model.LabelRule{reads("job", true)}}),
			false, `the type and the required label "job" of rule 2 do`, false, [5]string{none, none, none, twice, none},
		},
	} {
		shapes++
		if tc.taken {
			takenShapes++
		}
		for _, mode := range []string{"log", "ignore", "fail"} {
			x := &model.Collector{Name: "node", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
			applyLimitDefaults(&x.Limits)
			tc.pattern.ErrorMode = mode
			rules := []model.MetricRule{tc.named, tc.pattern}
			before, then, now := withRules(x, rules), withRules(x, rules), withRules(x, rules)
			if err := validateMetricRulesBeforeNamedPatterns(nil, before); err != nil {
				t.Errorf("%s, %s: the rules were refused: %v", name, mode, err)
				continue
			}
			for at, target := range targets {
				series, invalid, failed := scrapeOf(t, before, target.exposition)
				made := map[bool]string{true: none, false: once}[len(series) == 0]
				if invalid != nil {
					made = invalid.Error()
				}
				// Which rule made a series that is there once is told by
				// error_mode fail alone: that of the rule with the expression
				// is there then, and the scrape passes.
				want := tc.made[at]
				if want == other {
					want = once
				}
				switch {
				case mode != "fail" || tc.made[at] == other:
					if failed != nil || made != want || made == once && len(series) != 1 {
						t.Errorf("%s, %s, of %s: a scrape makes %d series, %q, and fails with %v\nwant %q", name, mode, target.metric, len(series), made, failed, tc.made[at])
					}
				case tc.made[at] == once || tc.made[at] == none:
					// What the rule carried on without fails the scrape.
					if failed == nil {
						t.Errorf("%s, %s, of %s: a scrape makes %d series, %q, and the rule fails it nowhere", name, mode, target.metric, len(series), made)
					}
				case failed != nil || made != tc.made[at]:
					t.Errorf("%s, %s, of %s: a scrape makes %d series, %q, and fails with %v\nwant %q", name, mode, target.metric, len(series), made, failed, tc.made[at])
				}
			}
			// The check as it was refused every one, in these words.
			was, err := validateMetricRulesWhateverAPatternSets(nil, then), validateMetricRules(nil, now)
			want := namedPatternSaid("node", 0, 1, 0, rules)
			if was == nil || was.Error() != want {
				t.Errorf("%s, %s: the load said %v\nwant %s", name, mode, was, want)
				continue
			}
			taken := tc.taken && mode != "fail"
			if book := keptToItsType(rules[0], rules[1]); book != taken {
				t.Errorf("%s, %s: by the book the rule with the expression keeps to its type: %v, want %v", name, mode, book, taken)
			}
			switch {
			case taken && err != nil:
				t.Errorf("%s, %s: the load says %v", name, mode, err)
			case !taken && (err == nil || err.Error() != want):
				t.Errorf("%s, %s: the load says %v\nwant %s", name, mode, err, want)
			}
			if mode == "fail" {
				// In the words of a rule that makes every series or fails
				// the scrape.
				if !strings.Contains(want, namedPatternTwice) || strings.Contains(want, "inconsistent types") {
					t.Errorf("%s, %s: the load said %v", name, mode, was)
				}
				continue
			}
			if sets := "their labels are alike; " + tc.sets + " not keep it from the metric, since every series of the metric that rule 2 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series"; !strings.Contains(want, sets) || strings.Contains(want, "where the two rules give the metric different types, as a metric of inconsistent types") != tc.twoTypes {
				t.Errorf("%s, %s: the load said %s\nwant it to say %q, and of two types: %v", name, mode, want, sets, tc.twoTypes)
			}
		}
	}
	if shapes != 26 || takenShapes != 11 {
		t.Errorf("%d pairs were tried, of which the load takes %d under log and ignore", shapes, takenShapes)
	}
}

// scrapeOf is what a scrape makes of an exposition with the collector's
// rules, as they are once the load filled in their defaults: the series and
// what validating them says, which is what fails a scrape, or the failure
// of a rule that fails it.
func scrapeOf(t *testing.T, x *model.Collector, exposition string) (series []model.Metric, invalid, failed error) {
	t.Helper()
	response := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(exposition), Headers: http.Header{"Content-Type": {"text/plain"}}}
	decoded, err := decode.Decode(response, x)
	if err != nil {
		t.Fatalf("%s: %v", exposition, err)
	}
	set, err := transform.Transform(transform.LeaveRuleLoggingToCaller(context.Background()), decoded, response, x, "python3")
	if err != nil || set == nil {
		return nil, nil, err
	}
	return set.Metrics, set.Validate(x.Limits), nil
}

// A pair is told of once, at the later of its two rules, after the rules
// that are the same rule as an earlier one, which are told of as that and
// of nothing else. Of up, a pattern of every metric, up again, down, the
// pattern again and load under a pattern of its own, the load reports the
// copies, rules 1 and 3 and rules 2 and 5, and then rules 1 and 2 and rules
// 2 and 4, each for the metric its rule names: not rule 3 beside rule 2,
// nor rule 5 beside rule 1 or rule 4, which taking the copies out leaves
// nothing of. A rule the load refuses for something else is held against no
// other.
func TestEachNamedMetricAPatternMatchesIsToldOfOnce(t *testing.T) {
	x := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus"}}
	up, down, every, load := model.MetricRule{Name: "up"}, model.MetricRule{Name: "down"}, model.MetricRule{Expression: ".*"}, model.MetricRule{Name: "load", Expression: "^node_load1$"}
	rules := []model.MetricRule{up, every, up, down, every, load}
	want := []string{sameRuleSaid("node", 0, 2, up), sameRuleSaid("node", 1, 4, every), namedPatternSaid("node", 0, 1, 0, rules), namedPatternSaid("node", 1, 3, 3, rules)}
	if got := problemTexts(validateMetricRules(nil, withRules(x, rules))); !slices.Equal(got, want) {
		t.Errorf("the load says\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A name two patterns match is told of beside each, and a pattern that
	// matches two names beside each of them.
	rules = []model.MetricRule{{Expression: "^up"}, up, {Expression: "up$"}, {Name: "startup"}}
	want = []string{namedPatternSaid("node", 0, 1, 1, rules), namedPatternSaid("node", 1, 2, 1, rules), namedPatternSaid("node", 2, 3, 3, rules)}
	if got := problemTexts(validateMetricRules(nil, withRules(x, rules))); !slices.Equal(got, want) {
		t.Errorf("the load says\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A rule refused for something else is refused for that alone.
	rules = []model.MetricRule{{Name: "up", Type: "meter"}, {Expression: "^up$"}, {Name: "up", Expression: "("}}
	got := problemTexts(validateMetricRules(nil, withRules(x, rules)))
	if len(got) != 2 || got[0] != `collector "node" metric "up" has invalid type "meter"` || !strings.HasPrefix(got[1], `collector "node" metric "up" expression "(": `) {
		t.Errorf("the load says\n%s", strings.Join(got, "\n"))
	}
}

// problemTexts is what a check says, problem by problem.
func problemTexts(err error) []string {
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
		texts[i] = problem.Error()
	}
	return texts
}

// rulesAsWhileEveryPatternWasRefused puts the rules of a collector through
// the loader's check of them and through that check as it was while a rule
// of a name beside a rule whose expression matches that name was refused
// whatever that rule sets, and fails unless they agree on everything but
// the pairs the load takes again.
//
// Of the rules the checks of a rule take and that repeat no earlier one
// (theSameRule), each two of which one passes on the metric of its name and
// the other's expression matches that name with the same labels, by the
// book (patternRepeatsName), are a pair. The check as it was reported every
// pair last, in the order of the later rule of each and then of the
// earlier, each in its words (namedPatternSaid): so it must have, which
// holds the book to it. The check reports those of them whose rule with the
// expression does not keep to its type (keptToItsType), in that order, in
// that place and in those words, and before them everything the check as it
// was said before its pairs, word for word and in the order it was. The
// defaults both fill in are the same.
//
// It returns how many pairs there are, how many of them the load takes
// again, and whether the check as it was, and the check, refuse the
// collector.
func rulesAsWhileEveryPatternWasRefused(t *testing.T, x *model.Collector, rules []model.MetricRule) (pairs, again int, refusedBefore, refused bool) {
	t.Helper()
	now, before := withRules(x, rules), withRules(x, rules)
	err, was := validateMetricRules(nil, now), validateMetricRulesWhateverAPatternSets(nil, before)
	if !reflect.DeepEqual(now.Metrics, before.Metrics) {
		t.Errorf("%s rules %+v are left as %+v, and were left as %+v", x.Transform.Type, rules, now.Metrics, before.Metrics)
	}
	// The rules that are held against each other: those of which a rule's
	// own checks say nothing, less the copies of an earlier one.
	var distinct []int
	for i := range rules {
		alone := withRules(x, rules)
		if validateMetricRule(alone, i) != nil || checkPrometheusRuleSelects(alone, i) != nil {
			continue
		}
		if !slices.ContainsFunc(distinct, func(first int) bool { return theSameRule(x, before.Metrics[first], before.Metrics[i]) }) {
			distinct = append(distinct, i)
		}
	}
	var every, still []string
	for at, later := range distinct {
		for _, earlier := range distinct[:at] {
			earlierIsNamed, repeats := patternRepeatsName(x, before.Metrics[earlier], before.Metrics[later])
			if !repeats {
				continue
			}
			named, matching := later, earlier
			if earlierIsNamed {
				named, matching = earlier, later
			}
			said := namedPatternSaid(x.Name, earlier, later, named, rules)
			every = append(every, said)
			if !keptToItsType(rules[named], rules[matching]) {
				still = append(still, said)
				continue
			}
			// Taken again: it was refused as a pair whose rule can fail on
			// a series and carry on, its type being what that rule sets.
			if again++; !strings.Contains(said, "; the type") || strings.Contains(said, namedPatternTwice) {
				t.Errorf("%s rules %+v: a pair taken again was told as one whose rule sets no type, or does not carry on: %s", x.Transform.Type, rules, said)
			}
		}
	}
	said, saidBefore := problemTexts(err), problemTexts(was)
	besides := len(saidBefore) - len(every)
	if besides < 0 || !slices.Equal(saidBefore[besides:], every) {
		t.Errorf("%s rules %+v:\n was %v\nwant it to end with %v", x.Transform.Type, rules, was, every)
		return len(every), again, was != nil, err != nil
	}
	if want := append(slices.Clone(saidBefore[:besides]), still...); !slices.Equal(said, want) {
		t.Errorf("%s rules %+v:\n now %v\nwant %v", x.Transform.Type, rules, err, strings.Join(want, "\n"))
	}
	// And so a collector without a pair taken again gets exactly what it
	// got.
	if again == 0 && ((err == nil) != (was == nil) || err != nil && err.Error() != was.Error()) {
		t.Errorf("%s rules %+v:\n now %v\n was %v", x.Transform.Type, rules, err, was)
	}
	return len(every), again, was != nil, err != nil
}

// Taking a prometheus rule of a name beside a rule of the histograms, or
// the summaries, that its expression matches changes the verdict on nothing
// else. The rules of every collector of the shipped configurations, and
// generated collectors of two and three rules, are put through the loader's
// check of a collector's rules and through that check as it was while such
// a pair was refused whatever the rule with the expression sets, kept as an
// oracle (validateMetricRulesWhateverAPatternSets).
//
// The generated prometheus rules are every pairing of a name — none, up,
// node_load1 — with an expression — none, a name anchored, a part of it,
// every metric, an alternation of both names, one that does not compile —
// and of that with labels: none, a constant, the constant with another
// value, a label read, the two in either order, and a label that reads one
// the rule sets, in either order; and with a scale, a scale under
// error_mode fail, a description with required and error_mode, a type, a
// label cut and a label required; and with a type of histogram, of summary
// under error_mode ignore, of histogram under error_mode fail, of histogram
// with a scale, and of summary with a required label. So a rule of each of
// the five is on either side: as the rule with the expression, and as the
// rule of the name, which then sets the type the other has, the other of
// the two, or `counter`. The collectors of three rules have among them a
// rule with a type and a scale under error_mode ignore and a rule of the
// histograms. Under jq, csv and python the rules are a rule of a name with
// an expression, and of that name with another, with a constant label and
// without an expression.
//
// A collector gets what it got, error for error and word for word, with the
// same defaults, unless two of its rules are a pair by a specification
// written apart from the check (patternRepeatsName) whose rule with the
// expression keeps to its type by the book (keptToItsType): its type is
// histogram or summary, its error_mode is not fail, and the rule of the
// name does not set that type. Exactly those pairs are reported no more,
// and everything else the check as it was said of their collector is said
// still, in the same words and order: a collector with nothing else wrong
// loads again, and one with another pair or another mistake is refused for
// that. No rule of another transform, no rule that is refused for
// something else, no copy of an earlier rule, and no rule of the shipped
// configurations is among the pairs taken again. Under the race detector
// the pairs and the collectors of three rules are of fewer rules.
func TestOnlyANameBesideAPatternOfHistogramsOrSummariesLoadsAgain(t *testing.T) {
	var shipped []*model.Collector
	files, shippedRuleCount := shippedRules(t, func(_ string, x *model.Collector, _ model.MetricRule) {
		if len(shipped) == 0 || shipped[len(shipped)-1] != x {
			shipped = append(shipped, x)
		}
	})
	if files < 13 || shippedRuleCount < 100 || len(shipped) < 13 {
		t.Fatalf("%d files with %d collectors and %d rules were found", files, len(shipped), shippedRuleCount)
	}
	for _, x := range shipped {
		if pairs, _, _, refused := rulesAsWhileEveryPatternWasRefused(t, x, x.Metrics); pairs != 0 || refused {
			t.Errorf("collector %q of the shipped configurations has %d pairs of a name and a pattern that matches it, and the load refuses its rules: %v", x.Name, pairs, refused)
		}
	}

	no := false
	two := 2.0
	static := func(name, value string) model.LabelRule { return model.LabelRule{Name: name, Value: value} }
	reads := func(name, expression string) model.LabelRule {
		return model.LabelRule{Name: name, Expression: expression}
	}
	labelSets := [][]model.LabelRule{
		nil,
		{static("site", "a")},
		{static("site", "b")},
		{reads("service", "job")},
		{reads("service", "job"), static("site", "a")},
		{static("site", "a"), reads("service", "job")},
		{reads("a", "b"), static("b", "x")},
		{static("b", "x"), reads("a", "b")},
	}
	var pool []model.MetricRule
	for _, name := range []string{"", "up", "node_load1"} {
		for _, expression := range []string{"", "^up$", "p", ".*", "^(up|node_load1)$", "("} {
			for _, labels := range labelSets {
				pool = append(pool, model.MetricRule{Name: name, Expression: expression, Labels: labels})
			}
			pool = append(pool,
				model.MetricRule{Name: name, Expression: expression, Scale: &two},
				model.MetricRule{Name: name, Expression: expression, Scale: &two, ErrorMode: " FAIL "},
				model.MetricRule{Name: name, Expression: expression, Description: "A description.", Required: &no, ErrorMode: "ignore"},
				model.MetricRule{Name: name, Expression: expression, Type: model.CounterMetricType},
				model.MetricRule{Name: name, Expression: expression, Labels: []model.LabelRule{{Name: "service", Expression: "job", Truncate: true}}},
				model.MetricRule{Name: name, Expression: expression, Labels: []model.LabelRule{{Name: "service", Expression: "job", Required: true}}},
				model.MetricRule{Name: name, Expression: expression, Type: model.HistogramMetricType},
				model.MetricRule{Name: name, Expression: expression, Type: model.SummaryMetricType, ErrorMode: "ignore"},
				model.MetricRule{Name: name, Expression: expression, Type: model.HistogramMetricType, ErrorMode: " Fail "},
				model.MetricRule{Name: name, Expression: expression, Type: model.HistogramMetricType, Scale: &two},
				model.MetricRule{Name: name, Expression: expression, Type: model.SummaryMetricType, Labels: []model.LabelRule{{Name: "service", Expression: "job", Required: true}}},
			)
		}
	}
	// A plain run pairs every rule of the pool with every other; under the
	// race detector, which makes the check many times slower, every ninth
	// is paired with every fifth, which still holds each name, each
	// expression and each of the nineteen rules of them on either side.
	firsts, seconds := alloctest.UnlessRaced(1, 9), alloctest.UnlessRaced(1, 5)
	prometheus := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: "prometheus"}}
	// What became of the collectors: taken as they were, refused in the
	// words they were, refused for less than they were, and taken again.
	tried, collectors, taken, refusedAsBefore, refusedForLess, takenAgain, pairs, pairsAgain := 0, 0, 0, 0, 0, 0, 0, 0
	check := func(x *model.Collector, rules ...model.MetricRule) int {
		t.Helper()
		collectors++
		tried += len(rules)
		every, again, before, refused := rulesAsWhileEveryPatternWasRefused(t, x, rules)
		switch {
		case !before && refused, again > 0 && !before:
			t.Errorf("%s rules %+v: refused now: %v, and before: %v, with %d pairs taken again", x.Transform.Type, rules, refused, before, again)
		case again > 0 && refused:
			refusedForLess++
		case again > 0:
			takenAgain++
		case before:
			refusedAsBefore++
		default:
			taken++
		}
		pairs += every
		pairsAgain += again
		return again
	}
	for i := 0; i < len(pool); i += firsts {
		for j := 0; j < len(pool); j += seconds {
			check(prometheus, pool[i], pool[j])
		}
	}
	// Collectors of three rules, of a few: a copy among them, a name that
	// two patterns match, and a name beside a pattern that is refused and
	// one that is taken again.
	few := []model.MetricRule{{Name: "up"}, {Expression: "^up$"}, {Expression: ".*"}, {Name: "up", Expression: "^up"}, {Name: "node_load1"}, {Expression: "p", Type: model.GaugeMetricType, Scale: &two, ErrorMode: " Ignore "}, {Expression: "p", Type: model.HistogramMetricType, ErrorMode: "ignore"}, {Name: "up", Labels: labelSets[1]}, {Expression: "p", Labels: labelSets[1]}, {Name: "other", Expression: "^up$"}, {Expression: "^up$", Scale: &two}, {}, {Name: "up", Type: "meter"}}
	few = few[:alloctest.UnlessRaced(len(few), 7)]
	for _, first := range few {
		for _, second := range few {
			for _, third := range few {
				check(prometheus, first, second, third)
			}
		}
	}
	// The floors are of the corpus each run goes through.
	if least := alloctest.UnlessRaced([7]int{200000, 2500, 500, 450, 30, 55000, 40000}, [7]int{5000, 200, 30, 12, 15, 1200, 1100}); tried < least[0] || pairs < least[1] || pairsAgain < least[2] || takenAgain < least[3] || refusedForLess < least[4] || taken < least[5] || refusedAsBefore < least[6] {
		t.Fatalf("%d rules of %d collectors were tried: %d collectors taken as before, %d refused as before, %d refused for less and %d taken again, for %d of %d pairs", tried, collectors, taken, refusedAsBefore, refusedForLess, takenAgain, pairsAgain, pairs)
	}

	// The other transforms have no rule of a name alone that passes on a
	// metric, and a python rule's name is the script's: nothing of theirs
	// was refused as such a pair, and nothing is taken again.
	others := 0
	for name, expressions := range map[string][2]string{"jq": {".up", ".v"}, "csv": {"up", "v"}, "python": {"", ""}} {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		rules := []model.MetricRule{{Name: "up", Expression: expressions[0]}, {Name: "up", Expression: expressions[1]}, {Name: "up", Expression: expressions[0], Labels: labelSets[1]}, {Name: "up"}, {Name: "v", Expression: "up"}, {Expression: "^up$"}}
		for _, first := range rules {
			for _, second := range rules {
				others += check(x, first, second)
			}
		}
	}
	if others != 0 {
		t.Errorf("%d pairs of rules of other transforms are taken again", others)
	}
	t.Logf("%d files with %d collectors and %d rules; %d generated rules of %d collectors: %d collectors taken as they were, %d refused as they were, %d refused for less and %d taken again, for %d of %d pairs of a name and a pattern that matches it", files, len(shipped), shippedRuleCount, tried, collectors, taken, refusedAsBefore, refusedForLess, takenAgain, pairsAgain, pairs)
}
