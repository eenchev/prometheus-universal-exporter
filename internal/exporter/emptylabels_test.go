package exporter

import (
	"reflect"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A static target's label written "" is the label left out, as a
// transform.labels value written "" is: it was added to every series of the
// target that did not have the label, and to the target's health series, as
// team="". Now it adds nothing, to either; a series that has the label from
// its collector keeps it, as it did, and a label with a value is added as
// it was.
func TestAnEmptyStaticTargetLabelIsLeftOff(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "demo", Labels: map[string]string{"region": "from-metric"}},
		{Name: "bare"},
	}}
	out := withTargetLabels(set, map[string]string{"team": "", "region": "", "zone": "a"})
	for i, want := range []map[string]string{{"region": "from-metric", "zone": "a"}, {"zone": "a"}} {
		if got := out.Metrics[i].Labels; !reflect.DeepEqual(got, want) {
			t.Errorf("%s has the labels %v, want %v", out.Metrics[i].Name, got, want)
		}
	}
	target := model.StaticTarget{Name: "eu", Target: "http://a.example", Labels: map[string]string{"team": "", "zone": "a", "collector": "other"}}
	health := staticTargetHealthMetrics(target, &model.Collector{Name: "text"}, 1, 0.5, time.Time{})
	if len(health.Metrics) != 3 {
		t.Fatalf("%d health series", len(health.Metrics))
	}
	for _, m := range health.Metrics {
		if want := map[string]string{"collector": "text", "static_target": "eu", "target": "http://a.example", "zone": "a"}; !reflect.DeepEqual(m.Labels, want) {
			t.Errorf("%s has the labels %v, want %v", m.Name, m.Labels, want)
		}
	}
}

// withTargetLabelsAndEmptyValues is withTargetLabels as it was while a
// label written "" was added like any other, kept as an oracle.
func withTargetLabelsAndEmptyValues(set model.MetricSet, labels map[string]string) model.MetricSet {
	if len(labels) == 0 {
		return set
	}
	out := model.CloneMetricSet(set)
	for i := range out.Metrics {
		if out.Metrics[i].Labels == nil {
			out.Metrics[i].Labels = map[string]string{}
		}
		own := model.SeriesOwnLabel(out.Metrics[i])
		for name, value := range labels {
			if _, exists := out.Metrics[i].Labels[name]; !exists && name != own {
				out.Metrics[i].Labels[name] = value
			}
		}
	}
	return out
}

// Leaving a static target's empty label off changes nothing else: over a
// table of targets' labels and series, a series gets the labels it got when
// no label is empty, and otherwise the ones it got from the same labels
// without the empty ones, and so do the target's health series.
func TestOnlyAnEmptyStaticTargetLabelIsAppliedAnew(t *testing.T) {
	series := []model.Metric{
		{Name: "bare"}, {Name: "none", Labels: map[string]string{}}, {Name: "own", Labels: map[string]string{"team": "core", "site": ""}},
		{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Count: 1}, Labels: map[string]string{"zone": "b"}},
		{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Count: 1}},
	}
	tried, empty := 0, 0
	for _, labels := range []map[string]string{
		nil, {}, {"team": "x"}, {"team": ""}, {"team": " "}, {"team": "", "zone": "a"}, {"team": "x", "zone": ""}, {"le": "", "quantile": "q"}, {"le": "l", "quantile": ""},
		{"site": "", "zone": "", "team": ""}, {"site": "s", "zone": "a", "team": "x", "le": "l"},
	} {
		without := map[string]string{}
		for name, value := range labels {
			if value != "" {
				without[name] = value
			}
		}
		if len(without) != len(labels) {
			empty++
		}
		// The health series have the labels they had from the target's
		// labels that are not empty: the three of their own, and each of
		// the target's that is none of those.
		health := map[string]string{"collector": "text", "static_target": "eu", "target": "http://a.example"}
		for name, value := range without {
			health[name] = value
		}
		for _, m := range staticTargetHealthMetrics(model.StaticTarget{Name: "eu", Target: "http://a.example", Labels: labels}, &model.Collector{Name: "text"}, 1, 0.5, time.Time{}).Metrics {
			if !reflect.DeepEqual(m.Labels, health) {
				t.Errorf("%v: %s has the labels %v, want %v", labels, m.Name, m.Labels, health)
			}
		}
		for _, m := range series {
			tried++
			in := model.MetricSet{Metrics: []model.Metric{model.CloneMetric(m)}}
			got, want := withTargetLabels(in, labels), withTargetLabelsAndEmptyValues(in, without)
			// With nothing but empty labels the series is copied where it
			// was handed on as it was, and has no labels either way.
			if g, w := got.Metrics[0].Labels, want.Metrics[0].Labels; !reflect.DeepEqual(g, w) && len(g)+len(w) > 0 {
				t.Errorf("%v on %s%v: labels %v, want %v", labels, m.Name, m.Labels, g, w)
			}
			if !reflect.DeepEqual(in.Metrics[0].Labels, m.Labels) {
				t.Errorf("%v on %s: the series' own labels were changed to %v", labels, m.Name, in.Metrics[0].Labels)
			}
		}
	}
	if tried < 50 || empty < 6 {
		t.Fatalf("%d series were tried, under %d sets of labels with an empty one", tried, empty)
	}
}
