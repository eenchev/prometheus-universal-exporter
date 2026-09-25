package transform

import (
	"context"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A pre-script's data comes back with its numbers read as the JSON decoder
// reads a response's, so an integer above 2^53 that the script passes through
// keeps every digit. Read as a float64 it was rounded, and the label read
// 1500000000000000000.
func TestPreScriptKeepsLargeIntegersExact(t *testing.T) {
	requirePython(t)
	c := &model.Collector{Name: "ids", Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq", PreScript: `data = data`},
		Limits:    scriptLimits(),
		Metrics: []model.MetricRule{{Name: "item_up", Type: model.GaugeMetricType, Expression: `.up`, Labels: []model.LabelRule{
			{Name: "id", Expression: ".id"},
			{Name: "big", Expression: ".big"},
			{Name: "ratio", Expression: ".ratio"},
		}}}}
	r := &fetch.HTTPResponse{Body: []byte(`{"id": 1500000000000000001, "big": 123456789012345678901234567890, "ratio": 0.5, "up": 1}`), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"id": "1500000000000000001", "big": "123456789012345678901234567890", "ratio": "0.5"}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 1 {
		t.Fatalf("unexpected series: %#v", set.Metrics)
	}
	for name, value := range want {
		if got := set.Metrics[0].Labels[name]; got != value {
			t.Errorf("label %s = %q, want %q", name, got, value)
		}
	}
}

// The numbers of a python transform's metrics, and of the series a
// prometheus pre-script leaves, are read the same way and still work as
// values, timestamps and counts.
func TestPythonNumbersReadAsJSONNumbers(t *testing.T) {
	requirePython(t)
	c := &model.Collector{Name: "prom", Decoder: model.DecoderConfig{Type: "prometheus"},
		Transform: model.TransformConfig{Type: "prometheus", PreScript: "for m in data['metrics']:\n    m['labels']['id'] = 1500000000000000001"},
		Limits:    scriptLimits()}
	body := "# TYPE h histogram\nh_bucket{le=\"1\"} 2 1700000000000\nh_bucket{le=\"+Inf\"} 3 1700000000000\nh_sum 4 1700000000000\nh_count 3 1700000000000\ng 7\n"
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 {
		t.Fatalf("unexpected series: %#v", set.Metrics)
	}
	h, g := set.Metrics[0], set.Metrics[1]
	if h.Histogram == nil || h.Histogram.Count != 3 || h.Histogram.Sum != 4 || len(h.Histogram.Buckets) != 2 || h.Histogram.Buckets[0].CumulativeCount != 2 ||
		h.Timestamp == nil || *h.Timestamp != 1700000000000 || h.Labels["id"] != "1500000000000000001" {
		t.Fatalf("histogram: %#v %#v", h, h.Histogram)
	}
	if g.Value != 7 || g.Labels["id"] != "1500000000000000001" {
		t.Fatalf("gauge: %#v", g)
	}

	py := &model.Collector{Name: "py", Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "python", Script: "metrics.append({'name': 'n', 'value': 3, 'timestamp': 1700000000000, 'labels': {'id': 1500000000000000001}})"},
		Limits:    scriptLimits()}
	r = &fetch.HTTPResponse{Body: []byte(`{}`), Headers: http.Header{}}
	d, err = decode.Decode(r, py)
	if err != nil {
		t.Fatal(err)
	}
	set, err = Transform(context.Background(), d, r, py, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 3 || set.Metrics[0].Timestamp == nil || *set.Metrics[0].Timestamp != 1700000000000 || set.Metrics[0].Labels["id"] != "1500000000000000001" {
		t.Fatalf("python transform: %#v", set.Metrics)
	}
}
