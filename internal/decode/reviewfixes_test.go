package decode

import (
	"net/http"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A ; inside a function's brackets is its argument's, not the result's tags.
func TestGraphiteTagsOnlyAtTheTopLevel(t *testing.T) {
	now := time.Unix(1727000000, 0)
	graphiteNowIs(t, now)
	series := decodeGraphiteBody(t, `[
	  {"target": "movingAverage(cpu.load;env=prod,'5min')", "datapoints": [[1, 1727000000]]},
	  {"target": "sumSeries(a;x=1,b;flag)", "tags": {"name": "sumSeries"}, "datapoints": [[2, 1727000000]]},
	  {"target": "cpu.load;env=prod;dc=eu", "datapoints": [[3, 1727000000]]}
	]`, model.GraphiteConfig{})
	if len(series) != 3 {
		t.Fatalf("%d series", len(series))
	}
	first := series[0].(map[string]any)
	if first["path"] != "movingAverage(cpu.load;env=prod,'5min')" || len(first["tags"].(map[string]any)) != 1 {
		t.Fatalf("%s", asJSON(t, first))
	}
	if second := series[1].(map[string]any); second["path"] != "sumSeries(a;x=1,b;flag)" {
		t.Fatalf("%s", asJSON(t, second))
	}
	third := series[2].(map[string]any)
	if tags := third["tags"].(map[string]any); third["path"] != "cpu.load" || tags["env"] != "prod" || tags["dc"] != "eu" {
		t.Fatalf("%s", asJSON(t, third))
	}
}

// A JSON body that holds "# HELP " in a string is still JSON.
func TestJSONIsSniffedBeforePrometheusComments(t *testing.T) {
	c := &model.Collector{Name: "sniff", Decoder: model.DecoderConfig{Type: "auto"}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte(`{"help": "# HELP me", "v": 1}`), Headers: http.Header{"Content-Type": {"text/plain"}}}, c)
	if err != nil || d.Kind != "json" {
		t.Fatalf("%v %+v", err, d)
	}
	d, err = Decode(&fetch.HTTPResponse{Body: []byte("# HELP up Up.\n# TYPE up gauge\nup 1\n"), Headers: http.Header{"Content-Type": {"text/plain"}}}, c)
	if err != nil || d.Kind != "prometheus" {
		t.Fatalf("%v %+v", err, d)
	}
}

// A tag value may hold quotes and brackets; only the path is read with
// brackets and quotes in mind.
func TestGraphiteTagValuesWithQuotesAndBrackets(t *testing.T) {
	graphiteNowIs(t, time.Unix(1727000000, 0))
	series := decodeGraphiteBody(t, "cpu.load;owner=o'neil;env=prod 1 1727000000\ndisk.free;mount=C:\\(x;env=prod 2 1727000000\n", model.GraphiteConfig{})
	for _, s := range series {
		tags := s.(map[string]any)["tags"].(map[string]any)
		if tags["env"] != "prod" {
			t.Fatalf("%s", asJSON(t, s))
		}
	}
	if tags := series[0].(map[string]any)["tags"].(map[string]any); tags["owner"] != "o'neil" && tags["mount"] != "C:\\(x" {
		t.Fatalf("%s", asJSON(t, series))
	}
}
