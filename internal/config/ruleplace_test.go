package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// saidNow is what the load says of the rule at index of a collector's
// metrics, named name, where it said text before a rule that has no name to
// be named by — name left out, "" or nothing but blanks — was named by its
// place among the collector's rules. Such a rule read `collector "demo"
// metric ""` and, where the name was what it lacked, `collector "demo" has a
// metric without a name`; it reads `collector "demo" metrics rule 2` and
// `collector "demo" metrics rule 2 has no name`. Of a rule that has a name
// nothing is said differently. It is the list of what changed, as the
// differential tests of the rule checks read the checks as they were by.
//
// One thing more changed, for a rule with a name as for one without: the
// refusal of a label's name that is not classic says, as a scrape's does,
// what would export it (transform.EscapingAdvice). The checks as they were
// are of collectors that set no name_escaping, under which such a name is
// refused as it was, and end their refusal with the name.
func saidNow(text, collector, name string, index int) string {
	if at := strings.LastIndex(text, " has invalid label name "); at >= 0 && !strings.Contains(text[at:], "\n") {
		if label, err := strconv.Unquote(text[at+len(" has invalid label name "):]); err == nil {
			text += transform.EscapingAdvice(label)
		}
	}
	if strings.TrimSpace(name) != "" {
		return text
	}
	place := fmt.Sprintf("collector %q metrics rule %d", collector, index+1)
	text = strings.ReplaceAll(text, fmt.Sprintf("collector %q has a metric without a name", collector), place+" has no name")
	return strings.ReplaceAll(text, fmt.Sprintf("collector %q metric %q", collector, name), place)
}

// validateRuleAlone puts a rule through validateMetricRule as the one rule
// of a copy of the collector, and leaves in the rule what the check left
// there, its defaults.
func validateRuleAlone(x *model.Collector, r *model.MetricRule) error {
	alone := *x
	alone.Metrics = []model.MetricRule{*r}
	err := validateMetricRule(&alone, 0)
	*r = alone.Metrics[0]
	return err
}

// placesFor is what a check of a collector's rules as it was says of the
// rules, read as the check says it now: each rule without a name to be
// named by is given one for the asking, no other rule's and no part of
// another's, and what the check as it was says of `metric "<that name>"` is
// what the check must say of the rule's place. A name is nothing to these
// checks but what they tell of a rule by, and what the rules of one name
// share, which a rule without a name shares with none.
func placesFor(x *model.Collector, was func(*model.Collector) error) string {
	named := withRules(x, x.Metrics)
	for i := range named.Metrics {
		if strings.TrimSpace(named.Metrics[i].Name) == "" {
			named.Metrics[i].Name = fmt.Sprintf("the_rule_at_place_%d", i+1)
		}
	}
	err := was(named)
	if err == nil {
		return ""
	}
	text := err.Error()
	for i := range named.Metrics {
		text = strings.ReplaceAll(text, fmt.Sprintf("collector %q metric %q", x.Name, fmt.Sprintf("the_rule_at_place_%d", i+1)), fmt.Sprintf("collector %q metrics rule %d", x.Name, i+1))
	}
	return text
}

// Naming a rule without a name by its place changes nothing else of what
// the three checks of a collector that tell of a rule say: the check that a
// csv collector without a header row reads its columns by number, the check
// that no placeholder stands where a probe's parameters fill none, and the
// check of the metric families, which refuses a description and a constant
// label longer than the limits. Each is put beside a copy of itself as it
// was, over the shipped collectors and generated collectors of one to three
// rules, with names, without, and with names of blanks. A collector all of
// whose rules have names gets what it got, word for word. Of a rule
// without one the check says what it said, with the rule's place where
// `metric ""` stood, which is what it said of a rule in that place that
// had a name of its own.
func TestTheChecksOfACollectorTellOfARuleWithANameAsTheyDid(t *testing.T) {
	no := false
	checks := []struct {
		name     string
		now, was func(*model.Collector) error
		x        model.Collector
		pool     []model.MetricRule
	}{
		{"csv columns", checkCSVColumns, checkCSVColumnsBeforeRulePlaces,
			model.Collector{Name: "demo", Transform: model.TransformConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: &no}}},
			[]model.MetricRule{
				{Name: "m", Expression: "2"}, {Name: "m", Expression: "host", Labels: []model.LabelRule{{Name: "rack", Expression: "rack"}, {Name: "site", Value: "x"}}},
				{Expression: "host"}, {Expression: "1", Labels: []model.LabelRule{{Name: "rack", Expression: "rack"}}}, {Name: "  ", Expression: "cpu"}, {Name: "n", Expression: "0"},
			}},
		{"placeholders", checkPlaceholdersAreFilled, checkPlaceholdersAreFilledBeforeRulePlaces,
			model.Collector{Name: "demo", Transform: model.TransformConfig{Type: "prometheus"}},
			[]model.MetricRule{
				{Name: "m", Labels: []model.LabelRule{{Name: "site", Value: "{{param_site}}"}}}, {Expression: "^node_", Labels: []model.LabelRule{{Name: "zone", Value: "a"}, {Name: "site", Value: "{{param_site}}"}}},
				{Expression: "^node_", Description: "Of {{param_site}}.", TimeZone: "{{param_zone}}"}, {Name: "  ", Description: "Of {{param_site}}."}, {Name: "n", Expression: "{{param_x}}", Items: "{{param_y}}"},
				{Expression: "^up$"}, {Name: "{{param_name}}"},
			}},
		{"metric families", checkMetricFamilies, checkMetricFamiliesBeforeRulePlaces,
			model.Collector{Name: "demo", Transform: model.TransformConfig{Type: "prometheus"}, Limits: model.Limits{MaxHelpLength: 10, MaxLabelValueLength: 5}},
			[]model.MetricRule{
				{Name: "m", Description: "A description longer than ten bytes."}, {Expression: "^node_", Description: "A description longer than ten bytes."},
				{Expression: "^node_", Labels: []model.LabelRule{{Name: "zone", Value: "a"}, {Name: "site", Value: "rack-one"}}}, {Name: "m", Labels: []model.LabelRule{{Name: "site", Value: "rack-one"}}},
				{Expression: "^up$"}, {Name: "m", Type: model.CounterMetricType}, {Name: "m", Type: model.GaugeMetricType}, {Name: "  ", Description: "A description longer than ten bytes."},
				{Expression: "^up$", Labels: []model.LabelRule{{Name: "site", Value: "rack-one", Truncate: true}}},
			}},
	}
	shipped, files := 0, 0
	for _, check := range checks {
		var last *model.Collector
		files, _ = shippedRules(t, func(_ string, x *model.Collector, _ model.MetricRule) {
			if x == last {
				return
			}
			last = x
			shipped++
			now, was := check.now(withRules(x, x.Metrics)), check.was(withRules(x, x.Metrics))
			if (now == nil) != (was == nil) || now != nil && now.Error() != was.Error() {
				t.Errorf("%s, shipped collector %q:\n now %v\n was %v", check.name, x.Name, now, was)
			}
		})
		tried, refused, placed := 0, 0, 0
		try := func(rules ...model.MetricRule) {
			t.Helper()
			tried++
			x := withRules(&check.x, rules)
			now, was := check.now(withRules(x, rules)), check.was(withRules(x, rules))
			said := ""
			if now != nil {
				said = now.Error()
				refused++
			}
			nameless := slices.ContainsFunc(rules, func(rule model.MetricRule) bool { return strings.TrimSpace(rule.Name) == "" })
			if !nameless && ((now == nil) != (was == nil) || now != nil && said != was.Error()) {
				t.Errorf("%s, rules %+v:\n now %v\n was %v", check.name, rules, now, was)
			}
			if (now == nil) != (was == nil) {
				t.Errorf("%s, rules %+v: now %v, and was %v", check.name, rules, now, was)
			}
			if want := placesFor(x, check.was); said != want || strings.Contains(said, `metric ""`) || strings.Contains(said, `metric "  "`) {
				t.Errorf("%s, rules %+v:\n now %s\nwant %s", check.name, rules, said, want)
			}
			if was != nil && said != was.Error() {
				placed++
			}
		}
		for _, first := range check.pool {
			try(first)
			for _, second := range check.pool {
				try(first, second)
				for _, third := range check.pool {
					try(first, second, third)
				}
			}
		}
		if tried < 200 || refused < 100 || placed < 50 || refused == tried {
			t.Errorf("%s: %d collectors were tried, %d of them refused, %d told of by a rule's place", check.name, tried, refused, placed)
		}
		t.Logf("%s: %d generated collectors, %d of them refused, %d told of by a rule's place", check.name, tried, refused, placed)
	}
	if files < 13 || shipped < 3*13 {
		t.Fatalf("%d files with %d collectors were found", files, shipped/len(checks))
	}
}
