package config

import (
	"context"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// Expressions are compiled once, when the configuration loads, and scrapes
// reuse the programs.

func TestExpressionsAreCompiledOnceAndReused(t *testing.T) {
	expression := `.servers | length | . * 1 // "unique-expression-for-this-test"`
	c := model.Collector{Name: "cached", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "servers", Type: model.GaugeMetricType, Expression: expression}}}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	first, err := expr.CompileJQ(expression)
	if err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte(`{"servers": [1, 2]}`), Headers: http.Header{"Content-Type": {"application/json"}}}
	for i := 0; i < 3; i++ {
		d, err := decode.Decode(r, &c)
		if err != nil {
			t.Fatal(err)
		}
		set, err := transform.Transform(context.Background(), d, r, &c, "python3")
		if err != nil || set.Metrics[0].Value != 2 {
			t.Fatalf("set=%v err=%v", set, err)
		}
	}
	again, _ := expr.CompileJQ(expression)
	if first != again {
		t.Fatal("the program was compiled again instead of reused")
	}
}
