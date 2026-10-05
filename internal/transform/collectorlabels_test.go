package transform

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transform.labels, remove_labels and rename_labels apply to the metrics of
// every transform (applyCollectorLabels).

func runCollectorLabels(t *testing.T, c model.Collector, body string) *model.MetricSet {
	t.Helper()
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// A jq collector gets the collector-wide labels as a prometheus passthrough
// does: added, removed and renamed, on every metric.
func TestCollectorLabelsApplyToEveryTransform(t *testing.T) {
	c := model.Collector{
		Name: "jq", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq", Labels: map[string]string{"env": "prod", "team": "core"}, RemoveLabels: []string{"team"}, RenameLabels: map[string]string{"host": "instance"}},
		Metrics: []model.MetricRule{
			{Name: "a", Type: model.GaugeMetricType, Expression: ".a", Labels: []model.LabelRule{{Name: "host", Expression: ".host"}}},
			{Name: "b", Type: model.GaugeMetricType, Expression: ".b"},
		},
	}
	set := runCollectorLabels(t, c, `{"a":1,"b":2,"host":"web01"}`)
	got := map[string]string{}
	for _, m := range set.Metrics {
		got[m.Name] = fmt.Sprint(m.Labels)
	}
	if got["a"] != "map[env:prod instance:web01]" || got["b"] != "map[env:prod]" {
		t.Fatalf("labels %v", got)
	}
}

// Renames are made at once from the labels as they were, so they never chain,
// whatever order the map is walked in.
func TestLabelRenamesDoNotDependOnOrder(t *testing.T) {
	c := model.Collector{
		Name: "passthrough", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "prometheus"},
		Transform: model.TransformConfig{Type: "prometheus", RenameLabels: map[string]string{"a": "b", "b": "c"}},
	}
	for range 200 {
		set := runCollectorLabels(t, c, "v{a=\"A\",b=\"B\"} 1\n")
		if got := fmt.Sprint(set.Metrics[0].Labels); got != "map[b:A c:B]" {
			t.Fatalf("labels %s, want map[b:A c:B]", got)
		}
	}
}

// The output never shares a label map with the decoded input.
func TestCollectorLabelsLeaveTheInputAlone(t *testing.T) {
	in := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"a": "A"}}}}
	out := &model.MetricSet{Metrics: append([]model.Metric(nil), in.Metrics...)}
	applyCollectorLabels(out, model.TransformConfig{Labels: map[string]string{"env": "prod"}, RenameLabels: map[string]string{"a": "b"}})
	if fmt.Sprint(in.Metrics[0].Labels) != "map[a:A]" || fmt.Sprint(out.Metrics[0].Labels) != "map[b:A env:prod]" {
		t.Fatalf("in %v, out %v", in.Metrics[0].Labels, out.Metrics[0].Labels)
	}
}

// A transform.labels value written "" is the label left out, as a key
// written "" is the key left out everywhere else and as a rule's label is
// left off a series it has no value for. It was exported: the answer carried
// site="", which Prometheus reads as no label, the label counted towards
// limits.max_labels_per_metric, and a label of that name a rule had given
// was overwritten with the empty value. Now the entry adds nothing and takes
// nothing away: the rule's label stays, and a remove_labels or a
// rename_labels of the name finds what the series had. A value that is not
// empty still replaces a rule's label of the same name, as it did.
func TestAnEmptyCollectorLabelIsLeftOff(t *testing.T) {
	collector := func(labels map[string]string) model.Collector {
		return model.Collector{
			Name: "jq", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
			Transform: model.TransformConfig{Type: "jq", Labels: labels},
			Metrics: []model.MetricRule{
				{Name: "a", Type: model.GaugeMetricType, Expression: ".a", Labels: []model.LabelRule{{Name: "site", Expression: ".site"}}},
				{Name: "b", Type: model.GaugeMetricType, Expression: ".b"},
			},
		}
	}
	labelsOf := func(c model.Collector) map[string]string {
		got := map[string]string{}
		for _, m := range runCollectorLabels(t, c, `{"a":1,"b":2,"site":"rack1"}`).Metrics {
			got[m.Name] = fmt.Sprint(m.Labels)
		}
		return got
	}
	for name, test := range map[string]struct {
		labels map[string]string
		a, b   string
	}{
		"an empty value":                     {map[string]string{"site": "", "env": "prod"}, "map[env:prod site:rack1]", "map[env:prod]"},
		"an empty value alone":               {map[string]string{"site": ""}, "map[site:rack1]", "map[]"},
		"a value":                            {map[string]string{"site": "dc1", "env": "prod"}, "map[env:prod site:dc1]", "map[env:prod site:dc1]"},
		"a value of blanks, which is a text": {map[string]string{"site": " "}, "map[site: ]", "map[site: ]"},
		"none":                               {nil, "map[site:rack1]", "map[]"},
	} {
		if got := labelsOf(collector(test.labels)); got["a"] != test.a || got["b"] != test.b {
			t.Errorf("%s: labels %v, want a %s and b %s", name, got, test.a, test.b)
		}
	}
	// The label that is left off does not count towards the limit, which
	// the two series with one label each are within.
	c := collector(map[string]string{"site": "", "env": "prod"})
	c.Metrics[0].Labels = nil
	if err := runCollectorLabels(t, c, `{"a":1,"b":2}`).Validate(model.Limits{MaxLabelsPerMetric: 1}); err != nil {
		t.Errorf("with max_labels_per_metric 1: %v", err)
	}
	// remove_labels and rename_labels see the series' own label under the
	// empty entry.
	c = collector(map[string]string{"site": ""})
	c.Transform.RenameLabels = map[string]string{"site": "location"}
	if got := labelsOf(c); got["a"] != "map[location:rack1]" || got["b"] != "map[]" {
		t.Errorf("renamed: labels %v", got)
	}
}

// applyCollectorLabelsWithEmptyValues is applyCollectorLabels as it was
// while a transform.labels value of "" was set like any other, kept as an
// oracle.
func applyCollectorLabelsWithEmptyValues(set *model.MetricSet, t model.TransformConfig) {
	if len(t.Labels) == 0 && len(t.RemoveLabels) == 0 && len(t.RenameLabels) == 0 {
		return
	}
	for i := range set.Metrics {
		labels := model.CloneLabels(set.Metrics[i].Labels)
		for name, value := range t.Labels {
			labels[name] = value
		}
		for _, name := range t.RemoveLabels {
			delete(labels, name)
		}
		renamed := map[string]string{}
		for from, to := range t.RenameLabels {
			if value, ok := labels[from]; ok {
				renamed[to] = value
			}
		}
		for from := range t.RenameLabels {
			delete(labels, from)
		}
		for name, value := range renamed {
			labels[name] = value
		}
		set.Metrics[i].Labels = labels
	}
}

// Leaving an empty transform.labels value off changes nothing else: over a
// table of settings and series, the labels a series gets are the ones it
// got, when no value is empty, and otherwise the ones it got from the same
// settings without the empty entries.
func TestOnlyAnEmptyCollectorLabelIsAppliedAnew(t *testing.T) {
	series := []map[string]string{nil, {}, {"site": "rack1"}, {"site": ""}, {"env": "dev", "host": "a"}, {"site": "rack1", "env": "dev", "internal": "x"}}
	labelSets := []map[string]string{nil, {}, {"site": "dc1"}, {"site": ""}, {"site": " "}, {"site": "", "env": "prod"}, {"site": "dc1", "env": ""}, {"env": "", "host": "", "site": ""}, {"zone": "a", "env": "prod"}}
	removes := [][]string{nil, {"site"}, {"env", "internal"}, {""}}
	renames := []map[string]string{nil, {"site": "location"}, {"env": "site"}, {"site": "env", "env": "site"}, {"host": "instance", "zone": "az"}}
	tried, empty := 0, 0
	for _, labels := range labelSets {
		without := map[string]string{}
		for name, value := range labels {
			if value != "" {
				without[name] = value
			}
		}
		for _, remove := range removes {
			for _, rename := range renames {
				settings := model.TransformConfig{Labels: labels, RemoveLabels: remove, RenameLabels: rename}
				oracle := settings
				if len(without) != len(labels) {
					oracle.Labels = without
					empty++
				}
				for _, own := range series {
					tried++
					in := model.Metric{Name: "v", Labels: model.CloneLabels(own)}
					if own == nil {
						in.Labels = nil
					}
					got, want := &model.MetricSet{Metrics: []model.Metric{in}}, &model.MetricSet{Metrics: []model.Metric{in}}
					applyCollectorLabels(got, settings)
					applyCollectorLabelsWithEmptyValues(want, oracle)
					if fmt.Sprint(got.Metrics[0].Labels) != fmt.Sprint(want.Metrics[0].Labels) {
						t.Errorf("%+v on %v: labels %v, want %v", settings, own, got.Metrics[0].Labels, want.Metrics[0].Labels)
					}
					if fmt.Sprint(in.Labels) != fmt.Sprint(own) {
						t.Errorf("%+v on %v: the series' own labels were changed to %v", settings, own, in.Labels)
					}
				}
			}
		}
	}
	if tried < 1000 || empty < 50 {
		t.Fatalf("%d series were tried, under %d settings with an empty value", tried, empty)
	}
}
