package config

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// sameRuleAdvice is what the load says of two rules of a collector that are
// the same rule, after their places and what they are rules of.
const sameRuleAdvice = `alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each`

// sameRuleSaid is that message for the rules at first and second of the
// collector's metrics, which the load counts from 1, the rule being the one
// of them: a rule with a name is a rule of that metric, and a prometheus
// rule without one of the metrics its expression matches.
func sameRuleSaid(collector string, first, second int, rule model.MetricRule) string {
	of := fmt.Sprintf("of metric %q", rule.Name)
	if rule.Name == "" {
		of = fmt.Sprintf("of the metrics that match %q", rule.Expression)
	}
	return fmt.Sprintf("collector %q metrics rule %d and rule %d are the same rule %s: %s", collector, first+1, second+1, of, sameRuleAdvice)
}

// theSameRule says whether two rules of the collector are the same rule,
// by the book, written apart from checkRulesDiffer: alike in name,
// expression, items, time_format and value_map, and with the same labels,
// which is the same values or expressions under each label name, in the
// order they are written under that name; and, for a prometheus rule one of
// whose labels reads a label the rule sets under another name, the same
// labels in the order they are written. Two python rules never are.
func theSameRule(x *model.Collector, a, b model.MetricRule) bool {
	if x.Transform.Type == "python" || a.Name != b.Name || a.Expression != b.Expression || a.Items != b.Items || a.TimeFormat != b.TimeFormat || len(a.ValueMap) != len(b.ValueMap) {
		return false
	}
	for text, value := range a.ValueMap {
		if other, mapped := b.ValueMap[text]; !mapped || other != value {
			return false
		}
	}
	type written struct{ name, value, expression string }
	byName := func(rule model.MetricRule) (map[string][]written, []written) {
		labels, list := map[string][]written{}, []written{}
		for _, label := range rule.Labels {
			labels[label.Name] = append(labels[label.Name], written{label.Name, label.Value, label.Expression})
			list = append(list, written{label.Name, label.Value, label.Expression})
		}
		return labels, list
	}
	labelsA, listA := byName(a)
	labelsB, listB := byName(b)
	if !reflect.DeepEqual(labelsA, labelsB) {
		return false
	}
	if x.Transform.Type == "prometheus" {
		for _, label := range a.Labels {
			if _, sets := labelsA[label.Expression]; sets && label.Expression != "" && label.Expression != label.Name {
				return reflect.DeepEqual(listA, listB)
			}
		}
	}
	return true
}

// seriesOf is what a scrape makes of a body with the collector's rules, as
// they are once the load filled in their defaults: the series, and what
// validating them says, which is what fails a scrape.
func seriesOf(t *testing.T, x *model.Collector, contentType, body string) ([]model.Metric, error) {
	t.Helper()
	response := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Content-Type": {contentType}}}
	decoded, err := decode.Decode(response, x)
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	set, err := transform.Transform(transform.LeaveRuleLoggingToCaller(context.Background()), decoded, response, x, "python3")
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if set == nil {
		return nil, nil
	}
	return set.Metrics, set.Validate(x.Limits)
}

// Two rules of a collector that are alike in everything that decides which
// series a rule makes are the same rule: each makes every series the other
// makes, of one name and one set of labels, so every scrape failed on a
// duplicate metric series, a mistake of the configuration told on every
// scrape as a failure of that scrape. The load took them, and refuses them
// now, naming both by their places and the metric.
//
// For each pair here the checks as they were take both rules; what a scrape
// then makes of a response is put through the validation that fails a
// scrape; and the load is asked what it says now. A pair that is the same
// rule gave a duplicate series and is refused: the rule written twice under
// each transform that makes series, with items and labels, with the labels
// in another order, and with what does not decide the series changed in
// one — scale, description, required, error_mode, a time_zone, a label's
// truncate and required. A pair that is not loads as it did, and its series
// are valid: rules of one name with other expressions, items or constant
// labels, as the documentation has several rules export one family; rules
// that read a value from different texts, by value_map or by time_format,
// of which one reads what the other cannot; labels of one name in another
// order, the last being the one a series gets; a prometheus rule whose
// label reads one it sets, where the order decides; and a python rule
// twice, which makes no series.
func TestTheSameRuleTwiceMakesEverySeriesTwice(t *testing.T) {
	no := false
	two := 2.0
	static := func(name, value string) model.LabelRule { return model.LabelRule{Name: name, Value: value} }
	reads := func(name, expression string) model.LabelRule {
		return model.LabelRule{Name: name, Expression: expression}
	}
	with := func(rule model.MetricRule, change func(*model.MetricRule)) model.MetricRule {
		rule.Labels = slices.Clone(rule.Labels)
		change(&rule)
		return rule
	}
	const queues = `{"v":1,"state":"up","at":"2026-01-02T03:04:05Z","queues":[{"name":"a","jobs":2},{"name":"b","jobs":3}]}`
	jobs := model.MetricRule{Name: "jobs", Items: ".queues[]", Expression: ".jobs", Labels: []model.LabelRule{reads("queue", ".name"), static("site", "rack1")}}
	at := model.MetricRule{Name: "at", Expression: ".at", TimeFormat: "rfc3339"}
	const exposition = "# TYPE up gauge\nup{job=\"api\",b=\"target\"} 1\n# TYPE node_load1 gauge\nnode_load1 0.5\n"
	type pair struct {
		transform, decoder, contentType, body string
		rules                                 []model.MetricRule
		// same is the metric the rules are the same rule of, and series
		// how many valid series rules that are not make of the body.
		same   string
		series int
	}
	for name, tc := range map[string]pair{
		"jq, a rule pasted twice":         {"jq", "json", "application/json", queues, []model.MetricRule{{Name: "up", Expression: ".v"}, {Name: "up", Expression: ".v"}}, "up", 0},
		"jq, with items and labels":       {"jq", "json", "application/json", queues, []model.MetricRule{jobs, jobs}, "jobs", 0},
		"jq, the labels in another order": {"jq", "json", "application/json", queues, []model.MetricRule{jobs, with(jobs, func(r *model.MetricRule) { slices.Reverse(r.Labels) })}, "jobs", 0},
		"jq, another scale":               {"jq", "json", "application/json", queues, []model.MetricRule{jobs, with(jobs, func(r *model.MetricRule) { r.Scale = &two })}, "jobs", 0},
		"jq, another description, required and error_mode": {"jq", "json", "application/json", queues, []model.MetricRule{jobs, with(jobs, func(r *model.MetricRule) {
			r.Description, r.Required, r.ErrorMode = "Jobs waiting.", &no, "ignore"
		})}, "jobs", 0},
		"jq, a label cut and required in one": {"jq", "json", "application/json", queues, []model.MetricRule{jobs, with(jobs, func(r *model.MetricRule) { r.Labels[0].Truncate, r.Labels[0].Required = true, true })}, "jobs", 0},
		"jq, another time_zone":               {"jq", "json", "application/json", queues, []model.MetricRule{at, with(at, func(r *model.MetricRule) { r.TimeZone = "Europe/Sofia" })}, "at", 0},
		"jq, the same value_map":              {"jq", "json", "application/json", queues, []model.MetricRule{{Name: "state", Expression: ".state", ValueMap: map[string]float64{"up": 1}}, {Name: "state", Expression: ".state", ValueMap: map[string]float64{"up": 1}}}, "state", 0},
		"yq":                                  {"yq", "yaml", "application/yaml", "v: 1\n", []model.MetricRule{{Name: "up", Expression: ".v"}, {Name: "up", Expression: ".v"}}, "up", 0},
		"xpath":                               {"xpath", "xml", "application/xml", `<r><v unit="s">1</v></r>`, []model.MetricRule{{Name: "up", Expression: "//v", Labels: []model.LabelRule{reads("unit", "@unit")}}, {Name: "up", Expression: "//v", Labels: []model.LabelRule{reads("unit", "@unit")}}}, "up", 0},
		"css":                                 {"css", "html", "text/html", `<table><tr><td class="n">a</td><td class="v">1</td></tr></table>`, []model.MetricRule{{Name: "up", Items: "tr", Expression: "td.v", Labels: []model.LabelRule{reads("row", "td.n")}}, {Name: "up", Items: "tr", Expression: "td.v", Labels: []model.LabelRule{reads("row", "td.n")}}}, "up", 0},
		"regex":                               {"regex", "text", "text/plain", "up=1 site=a\n", []model.MetricRule{{Name: "up", Expression: `up=(\d+) site=(?P<site>\w+)`, Labels: []model.LabelRule{reads("site", "site")}}, {Name: "up", Expression: `up=(\d+) site=(?P<site>\w+)`, Labels: []model.LabelRule{reads("site", "site")}}}, "up", 0},
		"csv":                                 {"csv", "csv", "text/csv", "host,used\nh1,10\nh2,20\n", []model.MetricRule{{Name: "used", Expression: "used", Labels: []model.LabelRule{reads("host", "host")}}, {Name: "used", Expression: "used", Labels: []model.LabelRule{reads("host", "host")}}}, "used", 0},
		"prometheus, a name twice":            {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up"}, {Name: "up"}}, "up", 0},
		"prometheus, an expression twice":     {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Expression: "^node_"}, {Expression: "^node_"}}, "node_load1", 0},
		"prometheus, both and another scale":  {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "load", Expression: "^node_load1$"}, {Name: "load", Expression: "^node_load1$", Scale: &two}}, "load", 0},
		"prometheus, labels in another order": {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("service", "job"), static("site", "rack1")}}, {Name: "up", Labels: []model.LabelRule{static("site", "rack1"), reads("service", "job")}}}, "up", 0},
		"prometheus, a label that reads one it sets, written alike": {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("a", "b"), static("b", "x")}}, {Name: "up", Labels: []model.LabelRule{reads("a", "b"), static("b", "x")}}}, "up", 0},
		"a third rule between them":                                 {"jq", "json", "application/json", queues, []model.MetricRule{{Name: "up", Expression: ".v"}, jobs, {Name: "up", Expression: ".v"}}, "up", 0},

		"jq, one name and other expressions":                           {"jq", "json", "application/json", `{"used":1,"free":2}`, []model.MetricRule{{Name: "bytes", Expression: ".used", Labels: []model.LabelRule{static("kind", "used")}}, {Name: "bytes", Expression: ".free", Labels: []model.LabelRule{static("kind", "free")}}}, "", 2},
		"jq, one expression and other constants":                       {"jq", "json", "application/json", queues, []model.MetricRule{jobs, with(jobs, func(r *model.MetricRule) { r.Labels[1].Value = "rack2" })}, "", 4},
		"jq, other items":                                              {"jq", "json", "application/json", `{"fast":[{"name":"a","jobs":1}],"slow":[{"name":"b","jobs":2}]}`, []model.MetricRule{with(jobs, func(r *model.MetricRule) { r.Items = ".fast[]" }), with(jobs, func(r *model.MetricRule) { r.Items = ".slow[]" })}, "", 2},
		"jq, a label more":                                             {"jq", "json", "application/json", queues, []model.MetricRule{jobs, with(jobs, func(r *model.MetricRule) { r.Labels = append(r.Labels, static("copy", "2")) })}, "", 4},
		"jq, value_maps that read other texts":                         {"jq", "json", "application/json", queues, []model.MetricRule{{Name: "state", Expression: ".state", ErrorMode: "ignore", ValueMap: map[string]float64{"up": 1}}, {Name: "state", Expression: ".state", ErrorMode: "ignore", ValueMap: map[string]float64{"down": 0}}}, "", 1},
		"jq, a value_map and a number":                                 {"jq", "json", "application/json", queues, []model.MetricRule{{Name: "state", Expression: ".state", ErrorMode: "ignore", ValueMap: map[string]float64{"up": 1}}, {Name: "state", Expression: ".state", ErrorMode: "ignore"}}, "", 1},
		"jq, time_formats that read other texts":                       {"jq", "json", "application/json", queues, []model.MetricRule{with(at, func(r *model.MetricRule) { r.ErrorMode = "ignore" }), with(at, func(r *model.MetricRule) { r.ErrorMode, r.TimeFormat = "ignore", "rfc1123" })}, "", 1},
		"csv, a time_format and a number":                              {"csv", "csv", "text/csv", "at\n2026-01-02T03:04:05Z\n", []model.MetricRule{{Name: "at", Expression: "at", ErrorMode: "ignore", TimeFormat: "rfc3339"}, {Name: "at", Expression: "at", ErrorMode: "ignore"}}, "", 1},
		"jq, labels of one name in another order":                      {"jq", "json", "application/json", queues, []model.MetricRule{{Name: "up", Expression: ".v", Labels: []model.LabelRule{static("a", "1"), static("a", "2")}}, {Name: "up", Expression: ".v", Labels: []model.LabelRule{static("a", "2"), static("a", "1")}}}, "", 2},
		"prometheus, other expressions":                                {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up"}, {Name: "load", Expression: "^node_"}}, "", 2},
		"prometheus, a label that reads one it sets, in another order": {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up", Labels: []model.LabelRule{reads("a", "b"), static("b", "x")}}, {Name: "up", Labels: []model.LabelRule{static("b", "x"), reads("a", "b")}}}, "", 2},
	} {
		x := &model.Collector{Name: "node", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}}
		applyLimitDefaults(&x.Limits)
		before, now := withRules(x, tc.rules), withRules(x, tc.rules)
		if err := validateMetricRulesBeforeSameRules(nil, before); err != nil {
			t.Errorf("%s: the rules were refused: %v", name, err)
			continue
		}
		series, invalid := seriesOf(t, before, tc.contentType, tc.body)
		err := validateMetricRules(nil, now)
		if tc.same == "" {
			if err != nil || invalid != nil || len(series) != tc.series {
				t.Errorf("%s: the load says %v, and a scrape makes %d series, %+v, of which validation says %v\nwant the rules loaded and %d valid series", name, err, len(series), series, invalid, tc.series)
			}
			continue
		}
		if invalid == nil || invalid.Error() != fmt.Sprintf("duplicate metric series %q", tc.same) {
			t.Errorf("%s: a scrape makes %+v, of which validation says %v, want a duplicate series of %s", name, series, invalid, tc.same)
		}
		last := len(tc.rules) - 1
		if want := sameRuleSaid("node", 0, last, tc.rules[last]); err == nil || err.Error() != want {
			t.Errorf("%s: the load says %v\nwant %s", name, err, want)
		}
	}

	// A python rule makes no series, the script does: the same rule twice
	// names one of the script's twice and changes nothing, so it loads.
	scripted := &model.Collector{Name: "node", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "python", Script: "metric('up', 'gauge', 1, labels={'note': data['note']})"}}
	applyLimitDefaults(&scripted.Limits)
	scripted.Limits.MaxLabelValueLength = 8
	cut := model.MetricRule{Name: "up", Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}}}
	scripted.Metrics = []model.MetricRule{cut, with(cut, func(*model.MetricRule) {})}
	if err := validateMetricRules(nil, scripted); err != nil {
		t.Errorf("a python rule twice: %v", err)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		if series, invalid := seriesOf(t, scripted, "application/json", `{"note":"a note longer than eight bytes"}`); invalid != nil || len(series) != 1 || len(series[0].Labels["note"]) > 8 {
			t.Errorf("a python rule twice: a scrape makes %+v, of which validation says %v", series, invalid)
		}
	}
}

// The comparison of two rules is of what they write. Rules that select the
// same series by other words are two rules to it, and load as they did,
// though a scrape fails on them as it did: a prometheus rule that matches
// the metric of its name beside one whose expression is that name, with a
// name or without, and two jq expressions of one meaning. They are what is
// left.
func TestRulesThatMeanTheSameInOtherWordsLoadAsTheyDid(t *testing.T) {
	const exposition = "# TYPE up gauge\nup 1\n"
	for name, tc := range map[string]struct {
		transform, decoder, contentType, body string
		rules                                 []model.MetricRule
	}{
		"a name, and the name with its pattern":      {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up"}, {Name: "up", Expression: "^up$"}}},
		"a name, and its pattern alone":              {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Name: "up"}, {Expression: "^up$"}}},
		"two patterns of one name":                   {"prometheus", "prometheus", "text/plain", exposition, []model.MetricRule{{Expression: "^up$"}, {Expression: "^(up)$"}}},
		"two jq expressions of one meaning":          {"jq", "json", "application/json", `{"v":1}`, []model.MetricRule{{Name: "up", Expression: ".v"}, {Name: "up", Expression: ".v "}}},
		"two time_formats written in another case":   {"jq", "json", "application/json", `{"v":"2026-01-02T03:04:05Z"}`, []model.MetricRule{{Name: "up", Expression: ".v", TimeFormat: "rfc3339"}, {Name: "up", Expression: ".v", TimeFormat: "RFC3339"}}},
		"a label read by two expressions of a value": {"jq", "json", "application/json", `{"v":1,"l":"a"}`, []model.MetricRule{{Name: "up", Expression: ".v", Labels: []model.LabelRule{{Name: "l", Expression: ".l"}}}, {Name: "up", Expression: ".v", Labels: []model.LabelRule{{Name: "l", Expression: `.["l"]`}}}}},
	} {
		x := &model.Collector{Name: "node", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}}
		applyLimitDefaults(&x.Limits)
		before, now := withRules(x, tc.rules), withRules(x, tc.rules)
		err, was := validateMetricRules(nil, now), validateMetricRulesBeforeSameRules(nil, before)
		if err != nil || was != nil {
			t.Errorf("%s: the load says %v, and said %v", name, err, was)
			continue
		}
		if series, invalid := seriesOf(t, now, tc.contentType, tc.body); invalid == nil || invalid.Error() != `duplicate metric series "up"` {
			t.Errorf("%s: a scrape makes %+v, of which validation says %v", name, series, invalid)
		}
	}
}

// rulesAsBeforeSameRules puts the rules of a collector through the loader's
// check of them and through that check as it was, and fails unless they
// agree on everything but two things. Where the check as it was told of a
// rule without a name as `metric ""` or as a metric without a name, the
// check tells of it by its place (saidNow); and of the rules the checks of
// one rule take, each that is the same rule as an earlier one, by the book
// (theSameRule), is refused for that, after everything else, against the
// first of its kind. Everything else is said word for word and in the order
// it was, and the defaults both fill in are the same. It returns how many
// rules are refused anew, how many problems read differently for a rule's
// place, and whether the check as it was refused the collector.
func rulesAsBeforeSameRules(t *testing.T, x *model.Collector, rules []model.MetricRule) (anew, placed int, refusedBefore bool) {
	t.Helper()
	now, before := withRules(x, rules), withRules(x, rules)
	err, was := validateMetricRules(nil, now), validateMetricRulesBeforeSameRules(nil, before)
	if !reflect.DeepEqual(now.Metrics, before.Metrics) {
		t.Errorf("%s rules %+v are left as %+v, and were left as %+v", x.Transform.Type, rules, now.Metrics, before.Metrics)
	}
	// What the check as it was says of each rule, by itself, which is what
	// it says of them together, in order; and the same as it reads now.
	var said, want []error
	sound := make([]bool, len(rules))
	for i, rule := range rules {
		alone := withRules(x, rules)
		problem := validateMetricRuleBeforeSameRules(alone, &alone.Metrics[i])
		if problem == nil {
			problem = checkPrometheusRuleSelects(alone, i)
		}
		sound[i] = problem == nil
		said = append(said, problem)
		if problem != nil {
			reads := saidNow(problem.Error(), x.Name, rule.Name, i)
			if reads != problem.Error() {
				placed++
			}
			problem = fmt.Errorf("%s", reads)
		}
		want = append(want, problem)
	}
	together := transform.CheckLabelValueMapsAgree(withRules(x, rules))
	if composed := model.JoinProblems(append(said, together)...); (composed == nil) != (was == nil) || was != nil && composed.Error() != was.Error() {
		t.Fatalf("%s rules %+v: the check as it was says\n%v\nand of each rule by itself\n%v", x.Transform.Type, rules, was, composed)
	}
	want = append(want, together)
	for i := range rules {
		if !sound[i] {
			continue
		}
		for first := range i {
			if sound[first] && theSameRule(x, before.Metrics[first], before.Metrics[i]) {
				want = append(want, fmt.Errorf("%s", sameRuleSaid(x.Name, first, i, rules[i])))
				anew++
				break
			}
		}
	}
	wanted := model.JoinProblems(want...)
	if (err == nil) != (wanted == nil) || err != nil && err.Error() != wanted.Error() {
		t.Errorf("%s rules %+v:\n now %v\nwant %v", x.Transform.Type, rules, err, wanted)
	}
	// And so a collector none of whose rules is refused anew or told of by
	// its place gets exactly what it got.
	if anew == 0 && placed == 0 {
		if (err == nil) != (was == nil) || err != nil && err.Error() != was.Error() {
			t.Errorf("%s rules %+v:\n now %v\n was %v", x.Transform.Type, rules, err, was)
		}
	}
	return anew, placed, was != nil
}

// Refusing two rules that are the same rule changes the verdict on nothing
// else, and naming a rule without a name by its place changes no other
// word. The rules of every collector of the shipped configurations, and
// generated collectors of one to four rules under each of the eight
// transforms, are put through the loader's check of a collector's rules and
// through that check as it was, kept as an oracle with the checks of one
// rule it called. The generated rules are a rule and what is made of it by
// changing one thing: a scale, a description with required and error_mode,
// the order of its labels, a label's truncate, a constant label's value,
// its expression, its name, a value_map, a time_format, a time_zone, its
// labels left out, an error_mode in another case; and a rule without a
// name, a rule of nothing and a rule with a name that is none and an
// expression that does not compile.
//
// A collector with no two rules that are the same rule and no rule without
// a name gets what it got, error for error and word for word, with the same
// defaults. A problem of a rule without a name reads as it did but for the
// rule's place where `metric ""` stood. And of the rules the checks of one
// rule take, each that repeats an earlier one is refused, after everything
// that was said, against the first of its kind and for nothing else: no
// python rule, no rule that is refused for something else, and no rule of
// the shipped configurations.
func TestOnlyTheSameRuleTwiceIsRefusedAnew(t *testing.T) {
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
		if anew, _, _ := rulesAsBeforeSameRules(t, x, x.Metrics); anew != 0 {
			t.Errorf("collector %q of the shipped configurations has %d rules the load refuses now", x.Name, anew)
		}
	}

	no := false
	two := 2.0
	type shape struct{ expression, other, label, items string }
	shapes := map[string]shape{
		"jq": {".v", ".w", ".l", ""}, "yq": {".v", ".w", ".l", ""}, "xpath": {"//v", "//w", "@l", ""}, "css": {"td.v", "td.w", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, `w=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "w", "l", ""}, "prometheus": {"^up$", "^node_", "l", ""}, "python": {"", "", "l", ""},
	}
	tried, collectors, refusedBefore, placedProblems := 0, 0, 0, 0
	anew, taken := map[string]int{}, map[string]int{}
	for name, shape := range shapes {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		base := model.MetricRule{Name: "m", Expression: shape.expression, Items: shape.items, Labels: []model.LabelRule{{Name: "site", Expression: shape.label}, {Name: "zone", Value: "a"}}}
		made := func(change func(*model.MetricRule)) model.MetricRule {
			rule := base
			rule.Labels = slices.Clone(base.Labels)
			change(&rule)
			return rule
		}
		pool := []model.MetricRule{
			base,
			made(func(r *model.MetricRule) { r.Scale = &two }),
			made(func(r *model.MetricRule) { r.Description, r.Required, r.ErrorMode = "A description.", &no, "ignore" }),
			made(func(r *model.MetricRule) { slices.Reverse(r.Labels) }),
			made(func(r *model.MetricRule) { r.Labels[0].Truncate = true }),
			made(func(r *model.MetricRule) { r.Labels[1].Value = "b" }),
			made(func(r *model.MetricRule) { r.Expression = shape.other }),
			made(func(r *model.MetricRule) { r.Name = "n" }),
			made(func(r *model.MetricRule) { r.ValueMap = map[string]float64{"up": 1} }),
			made(func(r *model.MetricRule) { r.TimeFormat = "rfc3339" }),
			made(func(r *model.MetricRule) { r.TimeFormat, r.TimeZone = "rfc3339", "UTC" }),
			made(func(r *model.MetricRule) { r.Labels = nil }),
			made(func(r *model.MetricRule) { r.Labels, r.ErrorMode = nil, " LOG " }),
			made(func(r *model.MetricRule) { r.Name = "" }),
			{},
			{Name: "bad-name", Expression: "("},
		}
		check := func(rules ...model.MetricRule) {
			t.Helper()
			collectors++
			tried += len(rules)
			refused, placed, before := rulesAsBeforeSameRules(t, x, rules)
			anew[name] += refused
			placedProblems += placed
			switch {
			case before:
				refusedBefore++
			case refused == 0:
				taken[name]++
			}
		}
		// Under the race detector, which makes the check many times
		// slower, the collectors of three and four rules are of fewer.
		few := pool
		if alloctest.RaceDetector {
			few = []model.MetricRule{pool[0], pool[3], pool[5], pool[13]}
		}
		for _, first := range pool {
			check(first)
			for _, second := range pool {
				check(first, second)
			}
		}
		for _, first := range few {
			for _, second := range few {
				for _, third := range pool {
					check(first, second, third)
				}
				for _, third := range []model.MetricRule{pool[0], pool[3], pool[13]} {
					for _, fourth := range []model.MetricRule{pool[0], pool[5], pool[12]} {
						check(first, second, third, fourth)
					}
				}
			}
		}
	}
	for name := range shapes {
		if name == "python" && anew[name] != 0 {
			t.Errorf("%d python rules are refused anew", anew[name])
		}
		if name != "python" && (anew[name] < 100 || taken[name] < 20) {
			t.Errorf("%s: %d rules are refused anew and %d collectors have their rules taken", name, anew[name], taken[name])
		}
	}
	if least := map[bool]int{false: 10, true: 1}[alloctest.RaceDetector]; tried < 10000*least || refusedBefore < 2000*least || placedProblems < 1500*least {
		t.Fatalf("%d rules of %d collectors were tried, %d collectors refused before, %d problems told of by a rule's place; refused anew %v", tried, collectors, refusedBefore, placedProblems, anew)
	}
	t.Logf("%d files with %d collectors and %d rules; %d generated rules of %d collectors: %d collectors refused as they were, %d problems told of by a rule's place, taken %v, and rules refused anew as the same rule %v", files, len(shipped), shippedRuleCount, tried, collectors, refusedBefore, placedProblems, taken, anew)
}

// A rule that repeats an earlier one is told of against the first of its
// kind, each copy once, and the rules are compared whatever stands between
// them; rules that differ are not compared with what they differ from.
func TestEachCopyOfARuleIsToldOfAgainstTheFirst(t *testing.T) {
	x := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus"}}
	up, load, every := model.MetricRule{Name: "up"}, model.MetricRule{Name: "load", Expression: "^node_load1$"}, model.MetricRule{Expression: ".*"}
	now := withRules(x, []model.MetricRule{up, load, up, every, load, up, every, {Name: "down"}})
	err := validateMetricRules(nil, now)
	want := strings.Join([]string{sameRuleSaid("node", 0, 2, up), sameRuleSaid("node", 1, 4, load), sameRuleSaid("node", 0, 5, up), sameRuleSaid("node", 3, 6, every)}, "\n")
	var problems model.Problems
	if err == nil || !errors.As(err, &problems) || len(problems) != 4 {
		t.Fatalf("the load says %v", err)
	}
	texts := make([]string, len(problems))
	for i, problem := range problems {
		texts[i] = problem.Error()
	}
	if got := strings.Join(texts, "\n"); got != want {
		t.Errorf("the load says\n%s\nwant\n%s", got, want)
	}
}

// Rules alike in name, expression and items are one rule to the transform's
// report of its rules' failures and to the failure log, logged when either
// has error_mode log (transform.TestTwinRulesAreLoggedWhenEitherHasLog).
// Such rules still load when their labels tell their series apart, and are
// still one rule to the report: with a label of another value in each, a
// csv, a jq and an xpath pair are taken by the load, make the series of
// both, and report one entry with the failures of both, logged for the one
// under log. The same pairs with one label in both are the same rule, and
// are refused.
func TestRulesAlikeButForALabelLoadAndAreOneRuleToTheReport(t *testing.T) {
	for name, tc := range map[string]struct {
		decoder, contentType, body string
		rule                       model.MetricRule
		series                     int
	}{
		"csv":   {"csv", "text/csv", "host,used\nh1,10\nh2,\n", model.MetricRule{Name: "used", Expression: "used", Labels: []model.LabelRule{{Name: "host", Expression: "host"}}}, 2},
		"jq":    {"json", "application/json", `{"queues":[{"name":"a","jobs":null},{"name":"b","jobs":2}]}`, model.MetricRule{Name: "jobs", Items: ".queues[]", Expression: ".jobs", Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}}}, 2},
		"xpath": {"xml", "application/xml", `<disks><disk name="a"><used>1</used></disk><disk name="b"><used></used></disk></disks>`, model.MetricRule{Name: "used", Expression: "//disk/used", Labels: []model.LabelRule{{Name: "disk", Expression: "../@name"}}}, 2},
	} {
		x := &model.Collector{Name: "twins", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: name}}
		applyLimitDefaults(&x.Limits)
		twin := func(copied, mode string) model.MetricRule {
			rule := tc.rule
			rule.ErrorMode, rule.Labels = mode, append([]model.LabelRule{{Name: "copy", Value: copied}}, tc.rule.Labels...)
			return rule
		}
		alike := withRules(x, []model.MetricRule{twin("1", "ignore"), twin("1", "log")})
		if err := validateMetricRules(nil, alike); err == nil || err.Error() != sameRuleSaid("twins", 0, 1, tc.rule) {
			t.Errorf("%s, the label alike: %v", name, err)
		}
		twins := withRules(x, []model.MetricRule{twin("1", "ignore"), twin("2", "log")})
		if err := validateMetricRules(nil, twins); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		response := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(tc.body), Headers: http.Header{"Content-Type": {tc.contentType}}}
		decoded, err := decode.Decode(response, twins)
		if err != nil {
			t.Fatal(err)
		}
		ctx, report := transform.WithRuleReport(transform.LeaveRuleLoggingToCaller(context.Background()))
		set, err := transform.Transform(ctx, decoded, response, twins, "python3")
		if err != nil || set == nil || len(set.Metrics) != tc.series || set.Validate(twins.Limits) != nil {
			t.Errorf("%s: the twins make %+v, %v", name, set, err)
		}
		if failures := report.Failures(); len(failures) != 1 || failures[0].Metric != tc.rule.Name || failures[0].Expression != tc.rule.Expression || failures[0].Items != tc.rule.Items || failures[0].Failures != 2 || !failures[0].Logged {
			t.Errorf("%s: the report is %+v, want one entry of the rule with the failures of both, logged", name, failures)
		}
	}
}
