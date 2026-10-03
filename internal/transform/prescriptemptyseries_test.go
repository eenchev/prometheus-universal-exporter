package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A pre-script of a prometheus transform that left a histogram with no
// buckets, no sum and no count had the series read back as one with nothing,
// which was exported as a "# TYPE made histogram" line with no sample under
// it: what a misspelled key leaves, "bucket" for "buckets" and "cnt" for
// "count", each read as a key that is not there. Such a series fails the
// scrape, naming it, the keys a histogram or a summary series has and the
// keys this one has. A histogram of a sum alone, of a count alone or of
// buckets alone, and a summary of one of its three, are read back as before.
func TestAPrometheusPreScriptMustNotLeaveAHistogramOrSummaryWithNothing(t *testing.T) {
	series := func(s map[string]any) any {
		return map[string]any{"metrics": []any{map[string]any{"name": "g", "value": 1.0}, s}}
	}
	bucket := []any{map[string]any{"le": 1.0, "count": 1.0}}
	quantile := []any{map[string]any{"quantile": 0.5, "value": 2.0}}
	for name, tc := range map[string]struct {
		series map[string]any
		want   string
	}{
		"a histogram with misspelled keys": {
			map[string]any{"name": "made", "type": "histogram", "bucket": bucket, "cnt": 1.0, "labels": map[string]any{"a": "b"}},
			`data["metrics"][1]: the histogram made has no buckets, no sum and no count; a histogram series has the keys "buckets", "sum" and "count", and this one has the keys "bucket", "cnt", "labels", "name", "type"`,
		},
		"a histogram emptied": {
			map[string]any{"name": "made", "type": "histogram", "buckets": []any{}, "sum": nil, "count": nil},
			`data["metrics"][1]: the histogram made has no buckets, no sum and no count; a histogram series has the keys "buckets", "sum" and "count", and this one has the keys "buckets", "count", "name", "sum", "type"`,
		},
		"a summary with misspelled keys": {
			map[string]any{"name": "lat", "type": "summary", "quantile": quantile, "total": 3.0},
			`data["metrics"][1]: the summary lat has no quantiles, no sum and no count; a summary series has the keys "quantiles", "sum" and "count", and this one has the keys "name", "quantile", "total", "type"`,
		},
		"a histogram with many keys": {
			map[string]any{"name": "made", "type": "histogram", "a": 1.0, "b": 1.0, "c": 1.0, "d": 1.0, "e": 1.0, "f": 1.0, "g": 1.0, "h": 1.0, "i": 1.0, "j": 1.0, "k": 1.0, "l": 1.0},
			`and this one has the keys "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l" and 2 more`,
		},
		"a histogram of a sum alone":    {map[string]any{"name": "made", "type": "histogram", "sum": 3.0}, ""},
		"a histogram of a count alone":  {map[string]any{"name": "made", "type": "histogram", "count": 3.0}, ""},
		"a histogram of buckets alone":  {map[string]any{"name": "made", "type": "histogram", "buckets": bucket}, ""},
		"a summary of a count alone":    {map[string]any{"name": "lat", "type": "summary", "count": 3.0}, ""},
		"a summary of quantiles alone":  {map[string]any{"name": "lat", "type": "summary", "quantiles": quantile}, ""},
		"an untyped series has a value": {map[string]any{"name": "plain", "value": 2.0}, ""},
	} {
		set, err := prometheusFromPython(series(tc.series))
		switch {
		case tc.want == "" && (err != nil || len(set.Metrics) != 2):
			t.Errorf("%s: %v, %d series", name, err, len(set.Metrics))
		case tc.want != "" && (err == nil || !strings.HasSuffix(err.Error(), tc.want)):
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// Through a transform, the script that misspells its keys fails the scrape
// as a script's failure, and the one that spells them is read back.
func TestAPrometheusPreScriptWithMisspelledHistogramKeysFailsTheTransform(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "prometheus"}}
	c.Transform.PreScript = `data["metrics"].append({"name": "made", "type": "histogram", "bucket": [{"le": 1, "count": 1}], "cnt": 1})`
	_, err := runBody(t, c, "text/plain", "# TYPE up_thing gauge\nup_thing 1\n")
	if want := `data["metrics"][1]: the histogram made has no buckets, no sum and no count; a histogram series has the keys "buckets", "sum" and "count", and this one has the keys "bucket", "cnt", "name", "type"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err=%v, want %q", err, want)
	}
	c.Transform.PreScript = `data["metrics"].append({"name": "made", "type": "histogram", "buckets": [{"le": 1, "count": 1}], "count": 1})`
	set, err := runBody(t, c, "text/plain", "# TYPE up_thing gauge\nup_thing 1\n")
	if err != nil || len(set.Metrics) != 2 || set.Metrics[1].Histogram == nil || set.Metrics[1].Histogram.Count != 1 || len(set.Metrics[1].Histogram.Buckets) != 1 {
		t.Fatalf("%v %+v", err, set)
	}
}
