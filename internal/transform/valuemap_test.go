package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// value_map and scale work the same in every transform with rules.
func TestValueMapAndScaleInEveryTransform(t *testing.T) {
	states := map[string]float64{"up": 1, "degraded": 0.5, "down": 0, "*": -1}
	milli := 0.001
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body string
		rules                                 []model.MetricRule
	}{
		"regex": {"text", "regex", "text/plain", "state: degraded\nstate: gone\nlatency: 250\n", []model.MetricRule{
			{Name: "state", Expression: `state: (\w+)`, ValueMap: states},
			{Name: "latency_seconds", Expression: `latency: (\d+)`, Scale: &milli},
		}},
		"css": {"html", "css", "text/html", `<p class="s">degraded</p><p class="l">250</p>`, []model.MetricRule{
			{Name: "state", Expression: "p.s", ValueMap: states},
			{Name: "latency_seconds", Expression: "p.l", Scale: &milli},
		}},
		"xpath": {"xml", "xpath", "application/xml", `<r><s>degraded</s><l>250</l></r>`, []model.MetricRule{
			{Name: "state", Expression: "/r/s", ValueMap: states},
			{Name: "latency_seconds", Expression: "number(/r/l)", Scale: &milli},
		}},
		"csv": {"csv", "csv", "text/csv", "state,latency\ndegraded,250\n", []model.MetricRule{
			{Name: "state", Expression: "state", ValueMap: states},
			{Name: "latency_seconds", Expression: "latency", Scale: &milli},
		}},
		"jq": {"json", "jq", "application/json", `{"state": "degraded", "latency": 250, "ok": true}`, []model.MetricRule{
			{Name: "state", Expression: ".state", ValueMap: states},
			{Name: "latency_seconds", Expression: ".latency", Scale: &milli},
			{Name: "ok", Expression: ".ok", ValueMap: map[string]float64{"true": 2, "false": 3}},
		}},
		"yq": {"yaml", "yq", "application/yaml", "state: degraded\nlatency: 250\n", []model.MetricRule{
			{Name: "state", Expression: ".state", ValueMap: states},
			{Name: "latency_seconds", Expression: ".latency", Scale: &milli},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			for i := range tc.rules {
				tc.rules[i].Type = model.GaugeMetricType
			}
			c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: tc.rules}
			for i := range c.Metrics {
				if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
					t.Fatal(err)
				}
			}
			set, err := runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			var states []float64
			for _, m := range set.Metrics {
				switch m.Name {
				case "state":
					states = append(states, m.Value)
				case "latency_seconds":
					if m.Value != 0.25 {
						t.Errorf("latency %v, want 0.25", m.Value)
					}
				case "ok":
					if m.Value != 2 {
						t.Errorf("ok %v, want 2", m.Value)
					}
				}
			}
			if len(states) == 0 || states[0] != 0.5 {
				t.Fatalf("states %v, want 0.5 first", states)
			}
			if name == "regex" && (len(states) != 2 || states[1] != -1) {
				t.Fatalf("an unlisted state %v, want the * default, -1", states)
			}
		})
	}
}

// Without "*", a value the map does not list is read as a number, and one
// that is not a number fails naming value_map.
func TestValueMapWithoutADefault(t *testing.T) {
	c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "state", Type: model.GaugeMetricType, Expression: ".state", ValueMap: map[string]float64{"up": 1}, ErrorMode: model.ErrorModeFail}}}
	set, err := runBody(t, c, "application/json", `{"state": " 7 "}`)
	if err != nil || set.Metrics[0].Value != 7 {
		t.Fatalf("%v %v", err, set)
	}
	if _, err := runBody(t, c, "application/json", `{"state": "sideways"}`); err == nil || !strings.Contains(err.Error(), "neither in value_map nor a number") {
		t.Fatalf("err=%v", err)
	}
	set, err = runBody(t, c, "application/json", `{"state": " up "}`)
	if err != nil || set.Metrics[0].Value != 1 {
		t.Fatalf("a value is looked up without its blanks: %v %v", err, set)
	}
}

// scale multiplies a prometheus rule's samples, and refuses a histogram;
// value_map, and either on a python transform, is refused at load, as is a
// scale of 0.
func TestValueRulesPerTransform(t *testing.T) {
	half := 0.5
	c := model.Collector{Name: "p", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{{Expression: "^up$", Scale: &half}, {Expression: "^h$", Scale: &half, ErrorMode: model.ErrorModeFail}}}
	set, err := runBody(t, c, "text/plain", "# TYPE up gauge\nup 4\n")
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 2 {
		t.Fatalf("%v %+v", err, set)
	}
	if _, err := runBody(t, c, "text/plain", "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n"); err == nil || !strings.Contains(err.Error(), "scale cannot apply") {
		t.Fatalf("err=%v", err)
	}
	zero := 0.0
	for transformType, rule := range map[string]model.MetricRule{
		"prometheus": {Name: "x", Expression: "x", ValueMap: map[string]float64{"a": 1}},
		"python":     {Name: "x", Scale: &half},
		"jq":         {Name: "x", Expression: ".x", Scale: &zero},
	} {
		x := model.Collector{Name: "c", Transform: model.TransformConfig{Type: transformType}}
		if err := CheckMetricRule(&x, &rule); err == nil {
			t.Errorf("%s: %+v accepted", transformType, rule)
		}
	}
	blank := model.Collector{Name: "c", Transform: model.TransformConfig{Type: "jq"}}
	if err := CheckMetricRule(&blank, &model.MetricRule{Name: "x", Expression: ".x", ValueMap: map[string]float64{" up": 1}}); err == nil {
		t.Error("a key with blanks was accepted")
	}
}
