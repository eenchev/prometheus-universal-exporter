package transform

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// valueHelpersScript calls each helper on each vector of the data, a
// series per vector: ok with the number, value_error with the message, or
// type_error with the message, labelled by the helper and the vector's
// place.
const valueHelpersScript = `
for name, helper in (('parse_bytes', parse_bytes), ('parse_duration', parse_duration),
                     ('parse_time', parse_time), ('parse_percent', parse_percent)):
    for i, v in enumerate(data[name]):
        labels = {'helper': name, 'i': str(i)}
        try:
            metric(name='ok', value=helper(v['input']), labels=labels)
        except ValueError as e:
            metric(name='value_error', value=0, labels=dict(labels, message=str(e)))
        except TypeError as e:
            metric(name='type_error', value=0, labels=dict(labels, message=str(e)))
`

// The Python helpers give every vector of testdata/values/helpers.json —
// the file the jq functions are tested against (internal/expr) — the same
// number to the bit, or fail with exactly its error, a ValueError, or, for
// input that is not text, a TypeError saying what the helper reads. They
// are the names a script has, as metric and fail are.
func TestValueHelpersInPythonMatchTheSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/values/helpers.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	type vector struct {
		Input     any      `json:"input"`
		Output    *float64 `json:"output"`
		Error     *string  `json:"error"`
		TypeError bool     `json:"type_error"`
	}
	vectors := map[string][]vector{}
	for _, helper := range expr.ValueHelpers {
		var list []vector
		if err := json.Unmarshal(doc[helper], &list); err != nil {
			t.Fatal(helper, err)
		}
		vectors[helper] = list
	}
	c := model.Collector{Name: "values", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "python", Script: valueHelpersScript}, Metrics: []model.MetricRule{},
		Limits: model.Limits{MaxMetrics: 10000, ScriptTimeout: model.Duration(30e9)}}
	d, r := decodedBody(t, c, "application/json", string(raw))
	set, err := executePython(context.Background(), "python3", c.Transform.Script, d, r, &c)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.Metric{}
	for _, m := range set.Metrics {
		got[m.Labels["helper"]+"/"+m.Labels["i"]] = m
	}
	total := 0
	for helper, list := range vectors {
		for i, v := range list {
			total++
			m, ok := got[helper+"/"+strconv.Itoa(i)]
			switch {
			case !ok:
				t.Errorf("%s(%#v): no series", helper, v.Input)
			case v.Output != nil:
				if m.Name != "ok" || math.Float64bits(m.Value) != math.Float64bits(*v.Output) {
					t.Errorf("%s(%#v) = %s %v %q, want %v", helper, v.Input, m.Name, m.Value, m.Labels["message"], *v.Output)
				}
			case v.Error != nil:
				if m.Name != "value_error" || m.Labels["message"] != *v.Error {
					t.Errorf("%s(%#v) = %s %v %q\n want the ValueError %s", helper, v.Input, m.Name, m.Value, m.Labels["message"], *v.Error)
				}
			case v.TypeError:
				if m.Name != "type_error" || !strings.HasPrefix(m.Labels["message"], helper+" reads text") {
					t.Errorf("%s(%#v) = %s %q, want a TypeError saying what it reads", helper, v.Input, m.Name, m.Labels["message"])
				}
			}
		}
	}
	if total < 200 {
		t.Fatalf("only %d vectors", total)
	}
}

// A value a helper cannot read fails as any other failing expression of its
// transform does: a jq or yq rule with the helper's error, under its
// error_mode, and a Python script with the ValueError, the traceback holding
// the script's line and none of the worker's own frames. A pre-script has
// the helpers too.
func TestValueHelpersFailAsTheirTransformFails(t *testing.T) {
	for _, kind := range []string{"jq", "yq"} {
		decoder, contentType, body := "json", "application/json", `{"mem": "1.00M", "bad": "lots"}`
		if kind == "yq" {
			decoder, contentType, body = "yaml", "application/yaml", "mem: 1.00M\nbad: lots\n"
		}
		c := model.Collector{Name: "values", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: kind},
			Limits: model.Limits{MaxMetrics: 10}, Metrics: []model.MetricRule{
				{Name: "mem_bytes", Type: model.GaugeMetricType, Expression: ".mem | parse_bytes"},
				{Name: "bad_bytes", Type: model.GaugeMetricType, Expression: ".bad | parse_bytes", ErrorMode: model.ErrorModeLog},
			}}
		set, failures, err := transformWith(context.Background(), t, c, contentType, body)
		if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 1048576 {
			t.Fatalf("%s: set=%v err=%v", kind, set, err)
		}
		want := `parse_bytes cannot read "lots" as a size: write a number`
		if len(failures) != 1 || failures[0].Metric != "bad_bytes" || !strings.Contains(failures[0].First.Error(), want) {
			t.Fatalf("%s: failures = %+v, want bad_bytes failing with %s", kind, failures, want)
		}
		c.Metrics[1].ErrorMode = model.ErrorModeFail
		if _, _, err := transformWith(context.Background(), t, c, contentType, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s with error_mode fail: err = %v", kind, err)
		}
	}

	c := model.Collector{Name: "values", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "python", Script: "metric(name='m', value=parse_duration(data['up']))\nmetric(name='n', value=parse_bytes(data['bad']))"},
		Metrics:   []model.MetricRule{}, Limits: model.Limits{MaxMetrics: 10, ScriptTimeout: model.Duration(30e9)}}
	_, _, err := transformWith(context.Background(), t, c, "application/json", `{"up": "1h2m", "bad": "lots"}`)
	if err == nil {
		t.Fatal("the script did not fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, `line 2, in <module>`) || !strings.HasSuffix(msg, `ValueError: parse_bytes cannot read "lots" as a size: write a number, then optionally blanks and a unit: B, KiB to EiB, kB to EB, or K to E`) || strings.Contains(msg, "<string>") {
		t.Fatalf("error = %s", msg)
	}

	pre := model.Collector{Name: "values", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq", PreScript: "data['up'] = parse_duration(data['up'])"},
		Metrics:   []model.MetricRule{{Name: "up_seconds", Type: model.GaugeMetricType, Expression: ".up"}},
		Limits:    model.Limits{MaxMetrics: 10, ScriptTimeout: model.Duration(30e9)}}
	set, _, err := transformWith(context.Background(), t, pre, "application/json", `{"up": "1-02:03:04"}`)
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 93784 {
		t.Fatalf("pre-script: set=%v err=%v", set, err)
	}
}
