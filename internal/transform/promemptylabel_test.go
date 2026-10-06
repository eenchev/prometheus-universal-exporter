package transform

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A label with an empty value is on no series the exporter makes: to
// Prometheus m{l=""} is the series m. A rule's expression label and a
// script's label were left off already; the label a target's own exposition
// writes so is left off by the decoder now, and a label that truncate: true
// cuts to nothing by the cut. These tests say what a prometheus transform
// makes of an exposition that has such labels, and that it makes of every
// other what it made.

// resultText is a transform's result as text, a series to a line
// (seriesText), or its error.
func resultText(set *model.MetricSet, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if set == nil {
		return "no set"
	}
	return setText(set.Metrics)
}

// transformedBody is what a collector makes of a body: runBody, with a body
// the decoder refuses, as one past limits.max_metrics, as the error it is.
func transformedBody(t *testing.T, c model.Collector, body string) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Content-Type": {"text/plain"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		return nil, err
	}
	return Transform(t.Context(), d, r, &c, "python3")
}

// withoutEmptyMaps is a result whose series without labels have no map of
// them either: the decoder gives such a series an empty map and a script
// none, which is the same to everything that reads a series.
func withoutEmptyMaps(set *model.MetricSet, err error) (*model.MetricSet, error) {
	if set == nil {
		return set, err
	}
	out := &model.MetricSet{Metrics: slices.Clone(set.Metrics)}
	for i := range out.Metrics {
		if len(out.Metrics[i].Labels) == 0 {
			out.Metrics[i].Labels = nil
		}
	}
	return out, err
}

// seriesOf is the series of a result by their names and labels alone, a
// series to a line, or the result's error.
func seriesOf(set *model.MetricSet, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if set == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range set.Metrics {
		b.WriteString(m.Name)
		names := model.SortedKeys(m.Labels)
		for i, name := range names {
			b.WriteString(map[bool]string{true: "{", false: ","}[i == 0])
			fmt.Fprintf(&b, "%s=%q", name, m.Labels[name])
		}
		if len(names) > 0 {
			b.WriteByte('}')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// What a prometheus transform makes of a target's m{l="",k="v"}, with the
// decoder leaving l off: the series m{k="v"}, whether the collector has no
// rules, picks and renames with include, exclude and rename, or has rules
// that pass the series, rename it or give it labels; whatever is applied to
// a collector's series afterwards finds no l either. A rule's label read
// from l finds no such label, as it finds none the target did not write:
// the label is left off, a required one is missing, and a label of another
// name the rule would have filled from l keeps the value the target gave it,
// which the empty l took from it before. A rule's static label of the name
// is the rule's value, as it was.
func TestAPrometheusTransformReadsATargetsSeriesWithoutItsEmptyLabels(t *testing.T) {
	prom := func(rules ...model.MetricRule) model.Collector {
		return model.Collector{Name: "p", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Metrics: rules}
	}
	with := func(c model.Collector, change func(*model.Collector)) model.Collector {
		change(&c)
		return c
	}
	label := func(rule model.MetricRule, labels ...model.LabelRule) model.MetricRule {
		rule.Labels = labels
		return rule
	}
	const body = "m{l=\"\",k=\"v\"} 1\n"
	for name, test := range map[string]struct {
		collector model.Collector
		want      string
	}{
		"no rules": {prom(), "m{k=\"v\"}\n"},
		"include":  {with(prom(), func(c *model.Collector) { c.Transform.Include = []string{"^m$"} }), "m{k=\"v\"}\n"},
		"exclude and rename": {with(prom(), func(c *model.Collector) {
			c.Transform.Exclude, c.Transform.Rename = []string{"^x$"}, map[string]string{"m": "n"}
		}), "n{k=\"v\"}\n"},
		"a rule by name":           {prom(model.MetricRule{Name: "m"}), "m{k=\"v\"}\n"},
		"a rule without a name":    {prom(model.MetricRule{Expression: "^m$"}), "m{k=\"v\"}\n"},
		"a rule that renames":      {prom(model.MetricRule{Name: "n", Expression: "^m$"}), "n{k=\"v\"}\n"},
		"a label read from it":     {prom(label(model.MetricRule{Name: "m"}, model.LabelRule{Name: "x", Expression: "l"})), "m{k=\"v\"}\n"},
		"the label itself":         {prom(label(model.MetricRule{Name: "m"}, model.LabelRule{Name: "l", Expression: "l"})), "m{k=\"v\"}\n"},
		"a label that has a value": {prom(label(model.MetricRule{Name: "m"}, model.LabelRule{Name: "k", Expression: "l"})), "m{k=\"v\"}\n"},
		"a required label":         {prom(label(model.MetricRule{Name: "m", ErrorMode: model.ErrorModeFail}, model.LabelRule{Name: "x", Expression: "l", Required: true})), "error: metric \"m\" label \"x\" is missing\n"},
		"the label required":       {prom(label(model.MetricRule{Name: "m", ErrorMode: model.ErrorModeFail}, model.LabelRule{Name: "l", Expression: "l", Required: true})), "error: metric \"m\" label \"l\" is missing\n"},
		"a required label ignored": {prom(label(model.MetricRule{Name: "m", ErrorMode: model.ErrorModeIgnore}, model.LabelRule{Name: "x", Expression: "l", Required: true})), ""},
		"a static label of it":     {prom(label(model.MetricRule{Name: "m"}, model.LabelRule{Name: "l", Value: "rule"})), "m{k=\"v\",l=\"rule\"}\n"},
		"a static label beside it": {prom(label(model.MetricRule{Name: "m"}, model.LabelRule{Name: "z", Value: "rule"})), "m{k=\"v\",z=\"rule\"}\n"},
		"transform.labels of it":   {with(prom(), func(c *model.Collector) { c.Transform.Labels = map[string]string{"l": "all"} }), "m{k=\"v\",l=\"all\"}\n"},
		"rename_labels of it":      {with(prom(), func(c *model.Collector) { c.Transform.RenameLabels = map[string]string{"l": "z"} }), "m{k=\"v\"}\n"},
		"remove_labels beside it":  {with(prom(), func(c *model.Collector) { c.Transform.RemoveLabels = []string{"k"} }), "m\n"},
		"a prefix":                 {with(prom(), func(c *model.Collector) { c.MetricsPrefix = "site" }), "site_m{k=\"v\"}\n"},
	} {
		c := test.collector
		got := strings.TrimSuffix(seriesOf(runBody(t, c, "text/plain", body)), "\n")
		if want := strings.TrimSuffix(test.want, "\n"); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// promPairCollectors are collectors with a prometheus transform of every
// shape: passing series through, picking and renaming them, and with rules
// that rename, type and scale them, give them static labels, read labels
// from theirs, require and cut them, alone and with what a collector
// applies to all its series.
func promPairCollectors() map[string]model.Collector {
	scale := 2.0
	collectors := map[string]model.Collector{}
	add := func(name string, change func(*model.Collector)) {
		c := model.Collector{Name: name, Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
		change(&c)
		collectors[name] = c
	}
	add("no rules", func(*model.Collector) {})
	add("no rules, limited", func(c *model.Collector) { c.Limits.MaxMetrics = 3 })
	add("include", func(c *model.Collector) { c.Transform.Include = []string{"^(m|h|up)"} })
	add("exclude and rename", func(c *model.Collector) {
		c.Transform.Exclude, c.Transform.Rename = []string{"^s"}, map[string]string{"m": "renamed", "h": "latency"}
	})
	add("collector labels", func(c *model.Collector) {
		c.Transform.Labels, c.Transform.RemoveLabels, c.Transform.RenameLabels = map[string]string{"env": "prod", "site": ""}, []string{"job"}, map[string]string{"k": "key", "l": "ell"}
		c.MetricsPrefix = "site"
	})
	add("rules by name", func(c *model.Collector) {
		c.Metrics = []model.MetricRule{{Name: "m"}, {Name: "h"}, {Name: "s"}, {Name: "absent", ErrorMode: model.ErrorModeIgnore}}
	})
	add("rules by pattern", func(c *model.Collector) {
		c.Metrics = []model.MetricRule{{Expression: "^(m|up)$", Description: "Passed on.", Scale: &scale}, {Name: "every", Expression: "^[mu]", Type: model.CounterMetricType}}
	})
	add("rules with labels", func(c *model.Collector) {
		c.Limits.MaxLabelValueLength = 6
		c.Metrics = []model.MetricRule{
			{Name: "m", Labels: []model.LabelRule{{Name: "site", Value: "dc1"}, {Name: "copy", Expression: "k"}, {Name: "l", Expression: "l"}, {Name: "note", Expression: "note", Truncate: true}}},
			{Expression: "^(up|h)$", Labels: []model.LabelRule{{Name: "k", Expression: "l"}, {Name: "note", Expression: "note", Truncate: true}, {Name: "j", Value: "a long constant", Truncate: true}}},
			{Name: "s", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{{Name: "k", Expression: "k", Required: true}}},
		}
	})
	// A rule whose family an exposition does not have makes nothing of it,
	// so that every exposition is transformed by every collector but the
	// last, whose rule fails for the label its series lack.
	optional := false
	for name := range collectors {
		for i := range collectors[name].Metrics {
			collectors[name].Metrics[i].Required = &optional
		}
	}
	add("a required label", func(c *model.Collector) {
		c.Metrics = []model.MetricRule{{Name: "m", ErrorMode: model.ErrorModeFail, Labels: []model.LabelRule{{Name: "location", Expression: "l", Required: true}}}}
	})
	return collectors
}

// promPairs are expositions written twice: as a target writes them, with
// labels of an empty value, and without those labels.
var promPairs = [][2]string{
	{"m{l=\"\",k=\"v\"} 1\n", "m{k=\"v\"} 1\n"},
	{"m{l=\"\"} 1\nup{k=\"\",l=\"x\"} 1\n", "m 1\nup{l=\"x\"} 1\n"},
	{"# TYPE m gauge\nm{l=\"x\",k=\"\"} 1\nm{l=\"\",k=\"w\",note=\"a long note\"} 2\nm{k=\"\",note=\"\",l=\"\"} 3\n", "# TYPE m gauge\nm{l=\"x\"} 1\nm{k=\"w\",note=\"a long note\"} 2\nm 3\n"},
	{"# TYPE m counter\nm{l=\"\",job=\"a\"} 1 1700000000000\nup{job=\"\",note=\"日本語のノート\"} 1\n", "# TYPE m counter\nm{job=\"a\"} 1 1700000000000\nup{note=\"日本語のノート\"} 1\n"},
	{"# TYPE h histogram\nh_bucket{l=\"\",k=\"v\",le=\"1\"} 1\nh_bucket{l=\"\",k=\"v\",le=\"+Inf\"} 2\nh_sum{l=\"\",k=\"v\"} 3\nh_count{l=\"\",k=\"v\"} 2\n", "# TYPE h histogram\nh_bucket{k=\"v\",le=\"1\"} 1\nh_bucket{k=\"v\",le=\"+Inf\"} 2\nh_sum{k=\"v\"} 3\nh_count{k=\"v\"} 2\n"},
	{"# TYPE s summary\ns{l=\"\",quantile=\"0.5\"} 1\ns_sum{l=\"\"} 3\ns_count{l=\"\"} 2\ns{k=\"v\",l=\"\",quantile=\"0.5\"} 1\n", "# TYPE s summary\ns{quantile=\"0.5\"} 1\ns_sum 3\ns_count 2\ns{k=\"v\",quantile=\"0.5\"} 1\n"},
	{"# TYPE m gauge\nm{le=\"\",quantile=\"\",k=\"v\"} 1\nm{l=\"\"} 2\nm 3\n", "# TYPE m gauge\nm{k=\"v\"} 1\nm 2\nm 3\n"},
	{"m{l=\"\",k=\"1\"} 1\nm{l=\"\",k=\"2\"} 2\nm{l=\"\",k=\"3\"} 3\nm{l=\"\",k=\"4\"} 4\nup{l=\"\"} 1\n", "m{k=\"1\"} 1\nm{k=\"2\"} 2\nm{k=\"3\"} 3\nm{k=\"4\"} 4\nup 1\n"},
}

// An exposition with labels of an empty value is transformed as the same
// exposition without those labels is, by every shape of prometheus
// collector: the same series, with the same labels, values, types, help and
// times, in the same order, or the same error. So whatever a transform does
// with a series' labels, an empty one is to it as one the target did not
// write. The result has no label with an empty value, and is what a
// pre-script that changes nothing gives, and a python transform is given
// the series without the label.
func TestAnExpositionWithEmptyLabelsIsTransformedAsTheOneWithoutThem(t *testing.T) {
	collectors := promPairCollectors()
	made, failed := 0, 0
	for name := range collectors {
		for _, pair := range promPairs {
			set, err := transformedBody(t, collectors[name], pair[0])
			with, without := resultText(set, err), resultText(transformedBody(t, collectors[name], pair[1]))
			if with != without {
				t.Errorf("%s: %q is transformed as\n%s\nand without its empty labels as\n%s", name, pair[0], with, without)
			}
			if err != nil {
				failed++
				continue
			}
			for _, m := range set.Metrics {
				made++
				for label, value := range m.Labels {
					if value == "" {
						t.Errorf("%s: %q is transformed with %s of the series %s empty", name, pair[0], label, m.Name)
					}
				}
			}
		}
	}
	if made < 150 || failed < 5 {
		t.Errorf("%d series were made and %d transforms failed: the table should have many of the first and some of the second", made, failed)
	}
}

// With a pre-script that passes data on as it got it, a prometheus
// collector's answer is the one without the script: the script was given
// l="" by the decoder and its answer was read without it, while the
// transform alone kept it. And a python transform is given a target's
// series without the label.
func TestAPassThroughPreScriptChangesNothingOfAnExpositionWithEmptyLabels(t *testing.T) {
	requirePython(t)
	collectors := promPairCollectors()
	for _, name := range []string{"no rules", "exclude and rename", "collector labels", "rules by name", "rules with labels"} {
		for _, pair := range promPairs {
			plain := collectors[name]
			scripted := plain
			scripted.Limits.ScriptTimeout, scripted.Transform.PreScript = scriptLimits().ScriptTimeout, "data = data"
			alone := resultText(withoutEmptyMaps(transformedBody(t, plain, pair[0])))
			if passed := resultText(withoutEmptyMaps(transformedBody(t, scripted, pair[0]))); alone != passed {
				t.Errorf("%s: %q is transformed as\n%s\nand through a pre-script that changes nothing as\n%s", name, pair[0], alone, passed)
			}
		}
	}
	python := model.Collector{Name: "reads", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "python", Script: `
for series in data["metrics"]:
    metric("seen", "gauge", 1, {"labels": ",".join(sorted(series["labels"]))})
`}}
	if got := seriesOf(runBody(t, python, "text/plain", "m{l=\"\",k=\"v\",j=\"\"} 1\n")); got != "seen{labels=\"k\"}\n" {
		t.Errorf("a python transform is given the series as %q, want k alone", got)
	}
}

// promPipeline is a prometheus transform and the cut of its rules' labels,
// as Transform runs the two, with the oracle's functions or the code's.
type promPipeline struct {
	apply    func(context.Context, model.MetricSet, *model.Collector, model.TransformConfig, []model.MetricRule) (*model.MetricSet, bool, error)
	truncate func(*model.MetricSet, *model.Collector)
}

// run gives in to the pipeline and says what it made: the series, which
// are a copy when the transform handed on the decoded ones, the error, and
// the failures of the rules that carried on.
func (p promPipeline) run(t *testing.T, in model.MetricSet, c *model.Collector) (*model.MetricSet, string) {
	t.Helper()
	ctx, failures := withRuleFailures(LeaveRuleLoggingToCaller(withSeriesBudget(t.Context(), c.Limits.MaxMetrics)))
	set, borrowed, err := p.apply(ctx, in, c, c.Transform, c.Metrics)
	var said strings.Builder
	fmt.Fprintf(&said, "borrowed=%v err=%v nil=%v", borrowed, err, set == nil)
	for _, failed := range failures.rules {
		fmt.Fprintf(&said, " failed(%q %q %d %d %v %v)", failed.rule.Name, failed.rule.Expression, failed.count, failed.missing, failed.first, failed.logged)
	}
	if err != nil || set == nil {
		return nil, said.String()
	}
	if borrowed {
		set = &model.MetricSet{Metrics: slices.Clone(set.Metrics)}
	}
	p.truncate(set, c)
	return set, said.String()
}

// generatedPromSeries are series as the decoder hands them to a prometheus
// transform: of a few names and every type, each with a few labels, none of
// them empty, of values short and long, of one byte to a character and of
// several.
func generatedPromSeries(random *rand.Rand) model.MetricSet {
	pick := func(choices ...string) string { return choices[random.IntN(len(choices))] }
	var set model.MetricSet
	for i, n := 0, random.IntN(7); i < n; i++ {
		m := model.Metric{Name: pick("up", "m", "m", "m_total", "h", "s", "node_load"), Type: model.GaugeMetricType, Value: float64(random.IntN(100)), Labels: map[string]string{}}
		switch m.Name {
		case "h":
			m.Type, m.Histogram = model.HistogramMetricType, &model.Histogram{Count: 2, Sum: 3, Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}}}
		case "s":
			m.Type, m.Summary = model.SummaryMetricType, &model.Summary{Count: 2, Sum: 3, Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}}
		case "m_total":
			m.Type = model.CounterMetricType
		}
		for j, labels := 0, random.IntN(4); j < labels; j++ {
			m.Labels[pick("a", "k", "l", "site", "job", "note")] = pick("v", "1", "rack1", "日本", "日本語のノート", "é", "éa", "x", "a long note of many bytes", "…", "ab", "abc", " ")
		}
		if random.IntN(12) == 0 {
			m.Labels = nil
		}
		set.Metrics = append(set.Metrics, m)
	}
	return set
}

// generatedPromCollector is a prometheus collector of a shape drawn at
// random: with rules, each with labels of every kind, or picking and
// renaming, under a limit on a label's length that is often very short and
// now and then one on the number of series.
func generatedPromCollector(random *rand.Rand) *model.Collector {
	pick := func(choices ...string) string { return choices[random.IntN(len(choices))] }
	c := &model.Collector{Name: "generated", Transform: model.TransformConfig{Type: "prometheus"}}
	c.Limits.MaxLabelValueLength = []int{0, 1, 1, 2, 2, 3, 4, 8, 500}[random.IntN(9)]
	if random.IntN(6) == 0 {
		c.Limits.MaxMetrics = 1 + random.IntN(4)
	}
	if random.IntN(5) == 0 {
		c.Transform.Labels = map[string]string{"env": "prod"}
	}
	labels := []model.LabelRule{
		{Name: "site", Value: "dc1"}, {Name: "site", Value: "日本語のノート", Truncate: true}, {Name: "j", Value: "éa", Truncate: true}, {Name: "j", Value: "a long constant", Truncate: true},
		{Name: "x", Expression: "a"}, {Name: "a", Expression: "a", Required: true}, {Name: "k", Expression: "l"}, {Name: "b", Expression: "missing"}, {Name: "b", Expression: "missing", Required: true},
		{Name: "note", Expression: "note", Truncate: true}, {Name: "note", Expression: "note", Truncate: true, Required: true}, {Name: "copy", Expression: "site", Truncate: true}, {Name: "l", Expression: "l", Truncate: true},
	}
	switch random.IntN(5) {
	case 0:
	case 1:
		c.Transform.Include, c.Transform.Exclude, c.Transform.Rename = []string{pick("^m", "^(up|h|s)$", ".")}, []string{pick("_total$", "^x$")}, map[string]string{"m": "renamed"}
	default:
		scale := 0.5
		for i, n := 0, 1+random.IntN(3); i < n; i++ {
			rule := model.MetricRule{Name: pick("m", "h", "s", "up", "renamed", "", "", ""), Expression: pick("", "", "^m", "^(up|h|s)$", ".*", "^m$"), ErrorMode: pick(model.ErrorModeFail, model.ErrorModeLog, model.ErrorModeIgnore)}
			if rule.Name == "" && rule.Expression == "" {
				rule.Expression = "^(m|up)$"
			}
			switch random.IntN(8) {
			case 0:
				rule.Type = model.CounterMetricType
			case 1:
				rule.Scale = &scale
			case 2:
				rule.Description = "Described."
			}
			for j, m := 0, random.IntN(4); j < m; j++ {
				rule.Labels = append(rule.Labels, labels[random.IntN(len(labels))])
			}
			c.Metrics = append(c.Metrics, rule)
		}
	}
	return c
}

// The prometheus transform and the cut of its rules' labels make what they
// made, series for series, error for error and failure for failure, of
// 20,000 generated sets of series under as many generated collectors (2,000
// under the race detector), but for one thing: a label that truncate: true
// cut to nothing, under a limit of one or two bytes that the value's first
// character does not fit, was exported as l="" where the rule's label was a
// constant or the rule has a name, and is left off. The series are as the
// decoder hands them on, without a label of an empty value, and neither
// function writes to them.
func TestAPrometheusTransformMakesWhatItMadeButForALabelCutToNothing(t *testing.T) {
	random := rand.New(rand.NewPCG(2026, 1006))
	old := promPipeline{applyPrometheusTransformAsItWas, truncateLabelsAsItWas}
	code := promPipeline{applyPrometheusTransform, truncateLabels}
	sets := alloctest.UnlessRaced(20000, 2000)
	same, cut, cutLabels, failed, series := 0, 0, 0, 0, 0
	for i := 0; i < sets && !t.Failed(); i++ {
		generated, c := generatedPromSeries(random), generatedPromCollector(random)
		in, given := model.CloneMetricSet(generated), model.CloneMetricSet(generated)
		was, wasSaid := old.run(t, model.CloneMetricSet(in), c)
		got, gotSaid := code.run(t, given, c)
		if !reflect.DeepEqual(given, in) {
			t.Fatalf("%+v under %+v: the transform changed the series it was given to %+v", in, c, given)
		}
		if wasSaid != gotSaid {
			t.Fatalf("%+v under %+v: %s, was %s", in, c, gotSaid, wasSaid)
		}
		if was == nil {
			failed++
			continue
		}
		// What it was, without the labels that were cut to nothing.
		without, emptied := withoutEmptyLabels(was.Metrics)
		if g, w := setText(got.Metrics), setText(without); g != w {
			t.Fatalf("%+v under %+v:\n%s\nwas, without its labels cut to nothing,\n%s", in, c, g, w)
		}
		series += len(got.Metrics)
		if emptied > 0 {
			cut++
			cutLabels += emptied
		} else {
			same++
		}
	}
	t.Logf("%d sets are transformed as they were, %d but for %d labels cut to nothing, and %d fail as they did; %d series", same, cut, cutLabels, failed, series)
	if same < sets/2 || cut < sets/200 || failed < sets/50 || series < sets {
		t.Fatal("the generator no longer makes sets and collectors of every kind")
	}
}

// A label that truncate: true cuts to nothing is left off its series: under
// limits.max_label_value_length of 1 or 2 there is no room for the mark of
// three bytes, nor for a first character of more bytes than the limit, and
// the label was exported as note="". A value whose first character fits is
// cut to it as it was, and so is every value under a limit of three bytes
// and more. It holds for a rule with a name, whose labels are cut once the
// rules have run, for a prometheus rule without one, which cuts its own,
// and for a constant the rule gives.
func TestALabelCutToNothingIsLeftOff(t *testing.T) {
	jq := func(limit int) model.Collector {
		return model.Collector{Name: "cut", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Limits: model.Limits{MaxLabelValueLength: limit},
			Metrics: []model.MetricRule{{Name: "m", Type: model.GaugeMetricType, Expression: ".v", Labels: []model.LabelRule{{Name: "note", Expression: ".note", Truncate: true}, {Name: "k", Expression: ".k"}}}}}
	}
	for _, test := range []struct {
		limit      int
		note, want string
	}{
		{2, "日本", "m{k=\"v\"}\n"}, {1, "éa", "m{k=\"v\"}\n"}, {1, "日本", "m{k=\"v\"}\n"}, {2, "…", "m{k=\"v\"}\n"},
		{2, "éa", "m{k=\"v\",note=\"é\"}\n"}, {1, "abc", "m{k=\"v\",note=\"a\"}\n"}, {2, "abc", "m{k=\"v\",note=\"ab\"}\n"}, {2, "ab", "m{k=\"v\",note=\"ab\"}\n"},
		{3, "日本", "m{k=\"v\",note=\"…\"}\n"}, {3, "abcd", "m{k=\"v\",note=\"…\"}\n"}, {4, "abcde", "m{k=\"v\",note=\"a…\"}\n"}, {6, "日本語のノート", "m{k=\"v\",note=\"日…\"}\n"},
	} {
		body := fmt.Sprintf(`{"v": 1, "k": "v", "note": %q}`, test.note)
		if got := seriesOf(runBody(t, jq(test.limit), "application/json", body)); got != test.want {
			t.Errorf("%q under a limit of %d bytes: %q, want %q", test.note, test.limit, got, test.want)
		}
	}
	prom := model.Collector{Name: "cut", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Limits: model.Limits{MaxLabelValueLength: 2},
		Metrics: []model.MetricRule{
			{Expression: "^m$", Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}, {Name: "site", Value: "日本", Truncate: true}}},
			{Name: "named", Expression: "^m$", Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}, {Name: "site", Value: "日本", Truncate: true}, {Name: "zone", Value: "éa", Truncate: true}}},
		}}
	if got, want := seriesOf(runBody(t, prom, "text/plain", "m{note=\"日本\",k=\"v\"} 1\n")), "m{k=\"v\"}\nnamed{k=\"v\",zone=\"é\"}\n"; got != want {
		t.Errorf("a prometheus rule's labels cut to nothing: %q, want %q", got, want)
	}
}
