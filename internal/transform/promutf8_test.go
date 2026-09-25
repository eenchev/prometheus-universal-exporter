package transform

import (
	"context"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A Prometheus label value that is not valid UTF-8 is repaired and counted,
// as the output of every other decoder is, rather than failing the decode and
// with it every other series of the scrape.
func TestPrometheusInvalidUTF8LabelValueIsRepaired(t *testing.T) {
	c := &model.Collector{Name: "prom", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
	r := &fetch.HTTPResponse{Body: []byte("# TYPE jobs gauge\njobs{owner=\"b\xffc\"} 1\njobs{owner=\"ok\"} 2\n"), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, report := WithRuleReport(context.Background())
	set, err := Transform(ctx, d, r, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["owner"] != "b�c" || set.Metrics[1].Labels["owner"] != "ok" {
		t.Fatalf("unexpected series: %#v", set.Metrics)
	}
	if repaired, first := report.UTF8Repairs(); repaired != 1 || first != "jobs" {
		t.Fatalf("repairs = %d, %q; want 1, jobs", repaired, first)
	}
}
