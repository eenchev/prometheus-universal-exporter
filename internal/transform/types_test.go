package transform

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every transform model.TransformTypes lists has a case in the transform
// package: each reads a sample it accepts into a metric. A new transform needs
// a sample here too, so adding one to the list without handling it fails.
func TestEveryListedTransformIsHandled(t *testing.T) {
	type sample struct{ decoder, body, expression string }
	samples := map[string]sample{
		"jq":         {"json", `{"v":1}`, ".v"},
		"yq":         {"yaml", "v: 1\n", ".v"},
		"xpath":      {"xml", "<r><v>1</v></r>", "/r/v"},
		"css":        {"html", "<p>1</p>", "p"},
		"csv":        {"csv", "v\n1\n", "v"},
		"regex":      {"text", "v=1", `v=(\d+)`},
		"prometheus": {"prometheus", "v 1\n", "^v$"},
		// Needs an interpreter to run; without a script it is refused by
		// its own case, which is what this test looks for.
		"python": {"text", "v=1", ""},
	}
	for _, transformType := range model.TransformTypes {
		t.Run(transformType, func(t *testing.T) {
			s, ok := samples[transformType]
			if !ok {
				t.Fatalf("no sample for transform %q; add one", transformType)
			}
			header := true
			c := &model.Collector{
				Name: "types", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
				Decoder: model.DecoderConfig{Type: s.decoder}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: &header}},
				Transform: model.TransformConfig{Type: transformType},
				Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: s.expression, ErrorMode: model.ErrorModeFail}},
			}
			r := &fetch.HTTPResponse{Body: []byte(s.body), Headers: http.Header{}}
			d, err := decode.Decode(r, c)
			if err != nil {
				t.Fatal(err)
			}
			set, err := Transform(context.Background(), d, r, c, "python3")
			if transformType == "python" {
				if err == nil || !strings.Contains(err.Error(), "python transform requires a script") {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 1 {
				t.Fatalf("err=%v set=%+v", err, set)
			}
		})
	}
}
