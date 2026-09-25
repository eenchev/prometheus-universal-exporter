package transform

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A response describing far more series than limits.max_metrics allows fails
// with the limit's error once the transform has made one series too many,
// rather than after it has made them all. The work is measured in bytes
// allocated, which grow with every series made: each case's body is 100,000
// series, and refusing it must allocate less than half of what making every
// one of them does. What remains is what the response costs before any
// series is made — gojq's walk over the array, the nodes an XPath selects —
// and is a fraction of it; refusing the scrape after making every series, as
// the count afterwards did, allocated all of it.

const (
	manySeries  = 100000
	seriesLimit = 10
	// boundedAllocs bounds the allocations of a decode that stops at the
	// 11th series; one that reads every line makes several per line.
	boundedAllocs   = 5000
	wantLimitFailed = "metric count 11 exceeds limit 10"
)

func repeated(item string, n int) string { return strings.Repeat(item, n) }

func TestTransformsStopAtTheSeriesLimit(t *testing.T) {
	tests := []struct {
		name      string
		collector model.Collector
		body      string
	}{
		{"regex", model.Collector{Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
			Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Expression: `(\d+)`}}},
			repeated("1\n", manySeries)},
		{"jq", model.Collector{Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
			Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Expression: `.[]`}}},
			"[" + strings.TrimSuffix(repeated("1,", manySeries), ",") + "]"},
		{"jq items", model.Collector{Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
			Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Items: `.[]`, Expression: `.v`, Labels: []model.LabelRule{{Name: "id", Expression: ".id"}}}}},
			"[" + strings.TrimSuffix(repeated(`{"v":1,"id":"a"},`, manySeries), ",") + "]"},
		{"csv", model.Collector{Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"},
			Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Expression: "v", Labels: []model.LabelRule{{Name: "id", Expression: "id"}}}}},
			"v,id\n" + repeated("1,a\n", manySeries)},
		{"xpath", model.Collector{Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
			Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Expression: "//v", Labels: []model.LabelRule{{Name: "id", Expression: "@id"}}}}},
			"<r>" + repeated(`<v id="a">1</v>`, manySeries) + "</r>"},
		{"css items", model.Collector{Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "css"},
			Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Items: "li", Expression: "b"}}},
			"<html><body><ul>" + repeated("<li><b>1</b></li>", manySeries) + "</ul></body></html>"},
		{"prometheus rules", model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
			Metrics: []model.MetricRule{{Name: "n", Expression: "^n$", Labels: []model.LabelRule{{Name: "copy", Expression: "i"}}}}},
			promSeries(manySeries)},
		{"prometheus passthrough", model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}},
			promSeries(manySeries)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.collector
			c.Name = "limited"
			// Decoded without a limit, so the transform is what stops; the
			// prometheus decoder's own stop is TestPrometheusDecoderStopsAtTheSeriesLimit.
			unlimited := c
			r := &fetch.HTTPResponse{Body: []byte(tc.body), Headers: http.Header{}}
			d, err := decode.Decode(r, &unlimited)
			if err != nil {
				t.Fatal(err)
			}
			// At the limit exactly, the scrape passes, making every series.
			c.Limits.MaxMetrics = manySeries
			var set *model.MetricSet
			all := allocated(func() { set, err = Transform(context.Background(), d, r, &c, "") })
			if err != nil || len(set.Metrics) != manySeries {
				t.Fatalf("at the limit: %d series, %v", len(set.Metrics), err)
			}
			c.Limits.MaxMetrics = seriesLimit
			limited := allocated(func() { _, err = Transform(context.Background(), d, r, &c, "") })
			if err == nil || err.Error() != wantLimitFailed || !errors.Is(err, model.ErrLimitExceeded) {
				t.Fatalf("err = %v, want %q marked as a limit", err, wantLimitFailed)
			}
			t.Logf("all %d, limited %d", all, limited)
			if limited > all/2 {
				t.Fatalf("refusing the scrape allocated %d bytes, making every series %d; it should stop at the 11th series", limited, all)
			}
		})
	}
}

// allocated is how many bytes run allocates.
func allocated(run func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func promSeries(n int) string {
	var b strings.Builder
	b.WriteString("# TYPE n gauge\n")
	for i := range n {
		b.WriteString(`n{i="`)
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(`",j="`)
		b.WriteString(itoa(i))
		b.WriteString("\"} 1\n")
	}
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for ; i > 0; i /= 10 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
	}
	return string(digits)
}

// The prometheus decoder keeps only the series its transform passes on, and
// stops at the first past the limit, with the same error, so a large
// exposition is refused without being held in memory.
func TestPrometheusDecoderStopsAtTheSeriesLimit(t *testing.T) {
	body := []byte(promSeries(manySeries))
	for _, tc := range []struct {
		name      string
		transform model.TransformConfig
		rules     []model.MetricRule
	}{
		{"passthrough", model.TransformConfig{Type: "prometheus"}, nil},
		{"include", model.TransformConfig{Type: "prometheus", Include: []string{"^n$"}}, nil},
		{"rules", model.TransformConfig{Type: "prometheus"}, []model.MetricRule{{Name: "copied", Expression: "^n$"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := model.Collector{Name: "limited", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: tc.transform, Metrics: tc.rules, Limits: model.Limits{MaxMetrics: seriesLimit}}
			r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
			var decodeErr error
			allocs := testing.AllocsPerRun(2, func() { _, decodeErr = decode.Decode(r, &c) })
			if decodeErr == nil || decodeErr.Error() != wantLimitFailed || !errors.Is(decodeErr, model.ErrLimitExceeded) {
				t.Fatalf("err = %v, want %q marked as a limit", decodeErr, wantLimitFailed)
			}
			if allocs > boundedAllocs {
				t.Fatalf("refusing the exposition took %.0f allocations", allocs)
			}
		})
	}

	// Series the transform leaves out are not kept, and do not count: a
	// scrape keeping a few of many passes.
	c := model.Collector{Name: "picked", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus", Exclude: []string{"^n$"}}, Limits: model.Limits{MaxMetrics: seriesLimit}}
	r := &fetch.HTTPResponse{Body: append([]byte("kept 1\n"), body...), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "")
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Name != "kept" {
		t.Fatalf("series %#v, %v", set, err)
	}
	// A pre-script sees every series, so the decoder keeps them all.
	c.Transform = model.TransformConfig{Type: "prometheus", PreScript: "pass"}
	d, err = decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(d.Data.(model.MetricSet).Metrics); n != manySeries+1 {
		t.Fatalf("a pre-script's decode kept %d series", n)
	}
}
