package transform

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// copyingPrometheusTransform is applyPrometheusTransform as it was while it
// copied every series it passed on into a slice grown from nothing, and gave
// every series of a rule a label map of its own. It is the oracle the
// transform that copies only what it changes is held to.
func copyingPrometheusTransform(ctx context.Context, in model.MetricSet, c *model.Collector, t model.TransformConfig, rules []model.MetricRule) (*model.MetricSet, error) {
	if len(rules) > 0 {
		out := model.MetricSet{}
		// Each rule's pattern is compiled once, not once per series; a rule
		// whose pattern does not compile is left without one.
		patterns := make([]*regexp.Regexp, len(rules))
		for i, rule := range rules {
			pattern := rule.Expression
			if pattern == "" {
				pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
			}
			re, err := expr.CompileRegex(pattern)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q expression: %w", rule.Name, err))
			}
			patterns[i] = re
		}
		matched := make([]bool, len(rules))
		for _, source := range in.Metrics {
			for i, rule := range rules {
				if patterns[i] == nil || !patterns[i].MatchString(source.Name) {
					continue
				}
				matched[i] = true
				metric := source
				if rule.Name != "" {
					metric.Name = rule.Name
				}
				if rule.Description != "" {
					metric.Help = rule.Description
				}
				if rule.Type != "" && rule.Type != metric.Type {
					// A histogram's or summary's type is its shape: it
					// cannot be exported as another, nor another as one.
					if metric.Histogram != nil || metric.Summary != nil || rule.Type == model.HistogramMetricType || rule.Type == model.SummaryMetricType {
						err := fmt.Errorf("metric %q type %s cannot apply to %s, a %s: a histogram or summary keeps its own type, and no other series can become one", rule.Name, rule.Type, source.Name, source.Type)
						if handleMetricError(ctx, c, rule, err) {
							continue
						}
						return nil, ruleFailure(c, rule, err)
					}
					metric.Type = rule.Type
				}
				if rule.Scale != nil {
					if metric.Histogram != nil || metric.Summary != nil {
						err := fmt.Errorf("metric %q scale cannot apply to %s, a %s: its buckets and quantiles are bounds as well as counts", rule.Name, source.Name, source.Type)
						if handleMetricError(ctx, c, rule, err) {
							continue
						}
						return nil, ruleFailure(c, rule, err)
					}
					metric.Value = scaled(rule, metric.Value)
				}
				// The series gets labels of its own: rules add and remove
				// them, and another rule may match the same source metric.
				metric.Labels = model.CloneLabels(source.Labels)
				for _, label := range rule.Labels {
					if label.Static() {
						metric.Labels[label.Name] = label.Value
					} else if value, ok := metric.Labels[label.Expression]; ok {
						metric.Labels[label.Name] = value
					}
					// A rule without a name keeps each series' own, which
					// truncateLabels cannot look it up by, so its labels are
					// cut here.
					if label.Truncate && c.Limits.MaxLabelValueLength > 0 {
						if value, ok := metric.Labels[label.Name]; ok {
							metric.Labels[label.Name] = truncateLabelValue(value, c.Limits.MaxLabelValueLength)
						}
					}
				}
				if missing := missingRequiredLabel(rule, metric.Labels); missing != nil {
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				if err := takeSeries(ctx); err != nil {
					return nil, err
				}
				out.Metrics = append(out.Metrics, metric)
			}
		}
		// A rule no series' name matched has no value, as a regex that
		// matched no text has none: a required rule is missing its value,
		// and its error mode decides, rather than the scrape passing
		// without the metric it was written for.
		for i, rule := range rules {
			if patterns[i] == nil || matched[i] || !requiredRule(rule, c) {
				continue
			}
			missing := model.MarkError(prometheusRuleUnmatched(rule), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return nil, ruleFailure(c, rule, missing)
		}
		return &out, nil
	}
	out := model.MetricSet{}
	includes := make([]*regexp.Regexp, 0, len(t.Include))
	for _, expression := range t.Include {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, err
		}
		includes = append(includes, re)
	}
	excludes := make([]*regexp.Regexp, 0, len(t.Exclude))
	for _, expression := range t.Exclude {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, err
		}
		excludes = append(excludes, re)
	}
	for _, metric := range in.Metrics {
		included := len(includes) == 0
		for _, expression := range includes {
			if expression.MatchString(metric.Name) {
				included = true
			}
		}
		for _, expression := range excludes {
			if expression.MatchString(metric.Name) {
				included = false
			}
		}
		if !included {
			continue
		}
		if name, ok := t.Rename[metric.Name]; ok {
			metric.Name = name
		}
		if err := takeSeries(ctx); err != nil {
			return nil, err
		}
		out.Metrics = append(out.Metrics, metric)
	}
	return &out, nil
}

// copyingTransform is Transform of a prometheus response with
// copyingPrometheusTransform in place of applyPrometheusTransform, and what
// its rules carried on without.
func copyingTransform(d *decode.Decoded, c *model.Collector) (*model.MetricSet, []RuleFailure, error) {
	ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(context.Background()))
	ctx = withSeriesBudget(ctx, c.Limits.MaxMetrics)
	ctx, failures := withRuleFailures(ctx)
	set, err := copyingPrometheusTransform(ctx, d.Data.(model.MetricSet), c, c.Transform, c.Metrics)
	failures.finish(c, report)
	if err != nil {
		return nil, report.Failures(), err
	}
	model.SanitizeUTF8(set)
	mapLabelValues(set, c)
	truncateLabels(set, c)
	applyCollectorLabels(set, c.Transform)
	applyMetricsPrefix(set, c.MetricsPrefix)
	if err := escapeNames(set, c.NameEscaping); err != nil {
		return nil, report.Failures(), err
	}
	return set, report.Failures(), nil
}

// passthroughExposition is a response with every kind of series a
// prometheus transform passes on: gauges with and without labels, a
// counter, a histogram, a summary, a long label value, an empty one, a
// name and a label name only UTF-8 allows, and a label value that is not
// valid UTF-8 when invalid says so.
func passthroughExposition(invalid bool) string {
	owner := "ok"
	if invalid {
		owner = "b\xffc"
	}
	return `# HELP jobs Jobs by owner.
# TYPE jobs gauge
jobs{owner="` + owner + `",queue="a"} 1
jobs{owner="second",queue="a",note="a note that is rather longer than the others are"} 2
jobs{owner="",queue="b"} 3
# TYPE up gauge
up 1
# TYPE requests_total counter
requests_total{code="200",method="get"} 10
requests_total{code="500",method="get"} 1
# TYPE latency_seconds histogram
latency_seconds_bucket{handler="/a",le="0.1"} 1
latency_seconds_bucket{handler="/a",le="+Inf"} 2
latency_seconds_sum{handler="/a"} 0.5
latency_seconds_count{handler="/a"} 2
# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.2
rpc_seconds_sum 3
rpc_seconds_count 9
# TYPE "http.server.duration" gauge
{"http.server.duration","service.name"="api",plain="x"} 4
`
}

// randomExposition is a response of random gauges and counters over a few
// names and labels, some of them long, empty or not valid UTF-8.
func randomExposition(r *rand.Rand) string {
	var b strings.Builder
	values := []string{"a", "b", "", "a value long enough to be cut by a small limit", "caf\xc3\xa9", "bad\xff"}
	for f := range 1 + r.IntN(5) {
		name := []string{"jobs", "up", "requests_total", "node_load", "queue_depth"}[f]
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, []string{"gauge", "counter", "untyped"}[r.IntN(3)])
		labels := r.IntN(4)
		for s := range 1 + r.IntN(6) {
			if labels == 0 && s > 0 {
				break
			}
			b.WriteString(name)
			if labels > 0 {
				b.WriteString("{")
				for l := range labels {
					if l > 0 {
						b.WriteString(",")
					}
					value := values[r.IntN(len(values))]
					if l == 0 {
						// One label tells the series of a family apart.
						value = fmt.Sprintf("s%d", s)
					}
					fmt.Fprintf(&b, `%s="%s"`, []string{"id", "owner", "queue", "note"}[l], value)
				}
				b.WriteString("}")
			}
			fmt.Fprintf(&b, " %d\n", r.IntN(1000))
		}
	}
	return b.String()
}

// prometheusCollectors are prometheus collectors for every way the
// transform and what follows it can change a series, and for none.
func prometheusCollectors() map[string]model.Collector {
	scale := 0.001
	optional := false
	labelled := func(labels ...model.LabelRule) []model.LabelRule { return labels }
	collectors := map[string]model.Collector{
		"passthrough":                {},
		"prefix":                     {MetricsPrefix: "vendor"},
		"collector labels":           {Transform: model.TransformConfig{Labels: map[string]string{"site": "a", "queue": "fixed"}}},
		"remove and rename labels":   {Transform: model.TransformConfig{RemoveLabels: []string{"note"}, RenameLabels: map[string]string{"owner": "queue", "queue": "lane"}}},
		"underscores":                {NameEscaping: NameEscapingUnderscores},
		"values":                     {NameEscaping: NameEscapingValues},
		"fail":                       {NameEscaping: NameEscapingFail},
		"everything after":           {MetricsPrefix: "v", NameEscaping: NameEscapingValues, Transform: model.TransformConfig{Labels: map[string]string{"site": "a"}, RenameLabels: map[string]string{"owner": "team"}}},
		"include":                    {Transform: model.TransformConfig{Include: []string{"^jobs$", "^up$", "seconds"}}},
		"exclude":                    {Transform: model.TransformConfig{Exclude: []string{"_total$"}}},
		"include, exclude, rename":   {Transform: model.TransformConfig{Include: []string{"s"}, Exclude: []string{"^rpc"}, Rename: map[string]string{"jobs": "work", "requests_total": "calls_total"}}},
		"rename and prefix":          {MetricsPrefix: "v", Transform: model.TransformConfig{Rename: map[string]string{"up": "alive"}}},
		"exclude everything":         {Transform: model.TransformConfig{Exclude: []string{"."}}},
		"limit":                      {Limits: model.Limits{MaxMetrics: 3}},
		"limit with include":         {Limits: model.Limits{MaxMetrics: 2}, Transform: model.TransformConfig{Include: []string{"^jobs$"}}},
		"limit at the count":         {Limits: model.Limits{MaxMetrics: 9}},
		"rules by name":              {Metrics: []model.MetricRule{{Name: "jobs"}, {Name: "up"}, {Name: "latency_seconds"}, {Name: "rpc_seconds"}, {Name: "absent", ErrorMode: model.ErrorModeLog}}},
		"rules by expression":        {Metrics: []model.MetricRule{{Name: "work", Expression: "^jobs$", Description: "Work.", Type: model.CounterMetricType}, {Expression: "_total$"}, {Expression: "^none$", Required: &optional}}},
		"a rule with labels":         {Metrics: []model.MetricRule{{Name: "jobs", Labels: labelled(model.LabelRule{Name: "site", Value: "a"}, model.LabelRule{Name: "team", Expression: "owner"}, model.LabelRule{Name: "absent", Expression: "nothing"})}}},
		"two rules for one series":   {Metrics: []model.MetricRule{{Name: "jobs"}, {Name: "work", Expression: "^jobs$", Labels: labelled(model.LabelRule{Name: "site", Value: "a"})}, {Name: "again", Expression: "^jobs$"}}},
		"a required label":           {Metrics: []model.MetricRule{{Name: "jobs", ErrorMode: model.ErrorModeIgnore, Labels: labelled(model.LabelRule{Name: "team", Expression: "owner", Required: true})}, {Name: "up"}}},
		"a required label, failing":  {Metrics: []model.MetricRule{{Name: "up"}, {Name: "jobs", ErrorMode: model.ErrorModeFail, Labels: labelled(model.LabelRule{Name: "team", Expression: "owner", Required: true})}}},
		"a type no histogram takes":  {Metrics: []model.MetricRule{{Name: "latency_seconds", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog}, {Name: "jobs", Type: model.HistogramMetricType, ErrorMode: model.ErrorModeIgnore}, {Name: "up"}}},
		"scale":                      {Metrics: []model.MetricRule{{Name: "jobs", Scale: &scale}, {Name: "rpc_seconds", Scale: &scale, ErrorMode: model.ErrorModeLog}}},
		"a pattern that is none":     {Metrics: []model.MetricRule{{Name: "bad", Expression: "(", ErrorMode: model.ErrorModeLog}, {Name: "up"}}},
		"rules and a limit":          {Limits: model.Limits{MaxMetrics: 2}, Metrics: []model.MetricRule{{Name: "jobs"}, {Name: "up"}}},
		"label value maps":           {Metrics: []model.MetricRule{{Name: "jobs", Labels: labelled(model.LabelRule{Name: "queue", Expression: "queue", ValueMap: map[string]string{"a": "first", "b": ""}})}, {Name: "work", Expression: "^jobs$"}}},
		"truncation by name":         {Limits: model.Limits{MaxLabelValueLength: 12}, Metrics: []model.MetricRule{{Name: "jobs", Expression: "^none$", Required: &optional, Labels: labelled(model.LabelRule{Name: "note", Expression: "note", Truncate: true})}, {Name: "jobs"}, {Name: "work", Expression: "^jobs$"}}},
		"truncation without a name":  {Limits: model.Limits{MaxLabelValueLength: 12}, Metrics: []model.MetricRule{{Expression: "^jobs$", Labels: labelled(model.LabelRule{Name: "note", Expression: "note", Truncate: true})}, {Name: "work", Expression: "^jobs$"}}},
		"rules and everything after": {MetricsPrefix: "v", NameEscaping: NameEscapingUnderscores, Transform: model.TransformConfig{Labels: map[string]string{"site": "a"}, RemoveLabels: []string{"queue"}}, Metrics: []model.MetricRule{{Name: "jobs"}, {Expression: "duration"}, {Name: "up", Labels: labelled(model.LabelRule{Name: "k", Value: "v"})}}},
	}
	for name, c := range collectors {
		c.Name, c.Decoder.Type, c.Transform.Type = "prom", "prometheus", "prometheus"
		collectors[name] = c
	}
	return collectors
}

// A prometheus transform gives what it gave while it copied every series
// and every rule's labels: the same series, in a slice or none as before,
// each with the same labels in a map or none, the same error, and the same
// failures counted. And whatever it shares with the decoded response, it
// leaves that as it was decoded, and gives the same again when the same
// decoded response is transformed a second time. Every collector is run
// over a response with every kind of series, with and without a label value
// to repair, and over random ones, forty of them and ten under the race
// detector.
func TestPrometheusTransformGivesWhatCopyingGave(t *testing.T) {
	bodies := []string{passthroughExposition(false), passthroughExposition(true), "", "up 1\n"}
	random := rand.New(rand.NewPCG(20261002, 2))
	for range alloctest.UnlessRaced(40, 10) {
		bodies = append(bodies, randomExposition(random))
	}
	compared := 0
	for name, c := range prometheusCollectors() {
		for i, body := range bodies {
			r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
			unlimited := c
			unlimited.Limits.MaxMetrics = 0
			d, err := decode.Decode(r, &unlimited)
			if err != nil {
				t.Fatalf("%s, body %d: %v\n%s", name, i, err, body)
			}
			// The decoder gives a series without labels an empty map;
			// the second time round such a series has no map at all.
			for _, noMaps := range []bool{false, true} {
				decoded := d.Data.(model.MetricSet)
				if noMaps {
					for s := range decoded.Metrics {
						if len(decoded.Metrics[s].Labels) == 0 {
							decoded.Metrics[s].Labels = nil
						}
					}
				}
				kept := copiedSeries(decoded)
				want, wantFailures, wantErr := copyingTransform(&decode.Decoded{Kind: d.Kind, Data: copiedSeries(decoded)}, &c)
				for run := range 2 {
					ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(context.Background()))
					got, gotErr := Transform(ctx, d, r, &c, "")
					if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || errors.Is(gotErr, model.ErrLimitExceeded) != errors.Is(wantErr, model.ErrLimitExceeded) || !reflect.DeepEqual(got, want) {
						t.Fatalf("%s, body %d, run %d:\n%+v, %v\ncopying gave:\n%+v, %v\n%s", name, i, run, got, gotErr, want, wantErr, body)
					}
					if failures := report.Failures(); fmt.Sprint(failures) != fmt.Sprint(wantFailures) {
						t.Fatalf("%s, body %d, run %d: failures %+v, copying counted %+v", name, i, run, failures, wantFailures)
					}
					if !reflect.DeepEqual(decoded, kept) {
						t.Fatalf("%s, body %d, run %d: the decoded response was changed:\n%+v\nwas:\n%+v", name, i, run, decoded, kept)
					}
					compared++
				}
			}
		}
	}
	t.Logf("%d transforms compared", compared)
}

// copiedSeries is a copy of set that shares nothing with it, and has a
// label map where set has one and none where it has none.
func copiedSeries(set model.MetricSet) model.MetricSet {
	out := model.MetricSet{Metrics: make([]model.Metric, len(set.Metrics))}
	if set.Metrics == nil {
		out.Metrics = nil
	}
	for i, m := range set.Metrics {
		out.Metrics[i] = model.CloneMetric(m)
	}
	return out
}

// sharesSeries reports whether two sets are one slice of series.
func sharesSeries(a, b []model.Metric) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// sharesLabels reports whether two label maps are one map.
func sharesLabels(a, b map[string]string) bool {
	return a != nil && b != nil && reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// A pass-through that changes nothing hands on the decoded series
// themselves, and copies nothing. One whose collector changes the series
// afterwards, or whose response has a label value to repair, gets series of
// its own, which is all it copies: the labels of a series it does not change
// stay the decoded ones.
func TestPrometheusPassthroughCopiesOnlyWhatItChanges(t *testing.T) {
	collectors := prometheusCollectors()
	for _, tc := range []struct {
		collector    string
		invalid      bool
		sharesSeries bool
		sharesLabels bool
	}{
		{"passthrough", false, true, true},
		{"fail", false, true, true},
		{"passthrough", true, false, false},
		{"prefix", false, false, true},
		{"underscores", false, false, true},
		{"collector labels", false, false, false},
		{"include", false, false, true},
		{"include, exclude, rename", false, false, true},
		{"rules by name", false, false, true},
	} {
		c := collectors[tc.collector]
		r := &fetch.HTTPResponse{Body: []byte(passthroughExposition(tc.invalid)), Headers: http.Header{}}
		d, err := decode.Decode(r, &c)
		if err != nil {
			t.Fatal(err)
		}
		decoded := d.Data.(model.MetricSet)
		set, err := Transform(LeaveRuleLoggingToCaller(context.Background()), d, r, &c, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.collector, err)
		}
		if got := sharesSeries(set.Metrics, decoded.Metrics); got != tc.sharesSeries {
			t.Errorf("%s (invalid UTF-8 %v): shares the decoded series %v, want %v", tc.collector, tc.invalid, got, tc.sharesSeries)
		}
		// The first series of the response, jobs with two labels, is the
		// first of every one of these outputs.
		if got := sharesLabels(set.Metrics[0].Labels, decoded.Metrics[0].Labels); got != tc.sharesLabels {
			t.Errorf("%s (invalid UTF-8 %v): shares the decoded labels %v, want %v", tc.collector, tc.invalid, got, tc.sharesLabels)
		}
	}
}

// A label cut by truncate: true is cut in the series it belongs to and in no
// other: the series of a rule that gives no labels share the decoded
// series' label map, with one another too, so the cut is made in a copy.
func TestTruncationLeavesSharedLabelsAlone(t *testing.T) {
	c := prometheusCollectors()["truncation by name"]
	r := &fetch.HTTPResponse{Body: []byte(passthroughExposition(false)), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	const note = "a note that is rather longer than the others are"
	set, err := Transform(context.Background(), d, r, &c, "")
	if err != nil {
		t.Fatal(err)
	}
	notes := map[string]string{}
	for _, m := range set.Metrics {
		if m.Labels["owner"] == "second" {
			notes[m.Name] = m.Labels["note"]
		}
	}
	if want := map[string]string{"jobs": "a note th…", "work": note}; !reflect.DeepEqual(notes, want) {
		t.Fatalf("notes %q, want %q", notes, want)
	}
	if got := d.Data.(model.MetricSet).Metrics[1].Labels["note"]; got != note {
		t.Fatalf("the decoded series' note was cut to %q", got)
	}
}
