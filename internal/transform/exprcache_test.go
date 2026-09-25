package transform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// One compiled program serves concurrent scrapes.
func TestCompiledJQIsSafeConcurrently(t *testing.T) {
	code, err := expr.CompileJQ(`.n * 2`)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func(n int) {
			values, err := evaluateJQ(context.Background(), map[string]any{"n": n}, nil, `.n * 2`)
			if err == nil && (len(values) != 1 || values[0] != n*2) {
				err = fmt.Errorf("%d*2 = %v", n, values)
			}
			done <- err
		}(i)
	}
	for i := 0; i < 50; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	_ = code
}

func BenchmarkJQCompiledOnce(b *testing.B) {
	data := map[string]any{"servers": []any{map[string]any{"cpu": 1}, map[string]any{"cpu": 2}}}
	for b.Loop() {
		if _, err := evaluateJQ(context.Background(), data, data, `[.servers[].cpu] | add`); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJQPerItemField is a rule's value and labels read from each item:
// a field path is looked up directly, while ".id | ." is the same value
// through a gojq run, which is what every field path used to cost.
func BenchmarkJQPerItemField(b *testing.B) {
	item := map[string]any{"id": "c1", "status": map[string]any{"code": 200}}
	for _, expression := range []string{".status.code", ".status.code | ."} {
		b.Run(expression, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := evaluateJQ(context.Background(), item, item, expression); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJQItems is a jq rule over 2000 items, its value and three labels
// field paths of each, as a status page's components are read.
func BenchmarkJQItems(b *testing.B) {
	var body strings.Builder
	body.WriteString(`{"items":[`)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"c%d","name":"component %d","status":"operational","cpu":%d.5}`, i, i, i%100)
	}
	body.WriteString("]}")
	c := model.Collector{Name: "items", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Limits: model.Limits{MaxMetrics: 10000, MaxLabelsPerMetric: 20, MaxLabelValueLength: 500, MaxMetricNameLength: 200},
		Metrics: []model.MetricRule{{Name: "component_cpu", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".cpu", ErrorMode: model.ErrorModeFail,
			Labels: []model.LabelRule{{Name: "id", Expression: ".id"}, {Name: "name", Expression: ".name"}, {Name: "status", Expression: ".status"}}}}}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body.String()), Headers: http.Header{"Content-Type": {"application/json"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		set, err := Transform(context.Background(), d, r, &c, "python3")
		if err != nil || len(set.Metrics) != 2000 {
			b.Fatalf("%v", err)
		}
	}
}
