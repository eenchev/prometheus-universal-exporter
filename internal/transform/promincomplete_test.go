package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

const incompleteExposition = "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE s summary\ns{quantile=\"0.5\"} 2\n# TYPE full histogram\nfull_bucket{le=\"+Inf\"} 2\nfull_sum 3\nfull_count 2\n"

// A histogram or summary the target wrote without a _sum or a _count reaches
// a pre-script without a sum or count key, and what the script leaves without
// one, or with None for it, is read back as a series that has none, a
// histogram's count being its +Inf bucket's.
func TestAPrometheusPreScriptKeepsASeriesWithoutSumOrCount(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "prometheus", PreScript: `
for s in data["metrics"]:
    if s["name"] in ("h", "s") and ("sum" in s or "count" in s):
        raise ValueError(s["name"] + " was given a sum or a count")
    if s["name"] == "full":
        s["sum"] = None
        del s["count"]
`}}
	set, err := runBody(t, c, "text/plain", incompleteExposition)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	if h := got["h"].Histogram; h == nil || !h.NoSum || !h.NoCount || h.Count != 7 || len(h.Buckets) != 2 {
		t.Fatalf("h %+v", h)
	}
	if s := got["s"].Summary; s == nil || !s.NoSum || !s.NoCount || len(s.Quantiles) != 1 {
		t.Fatalf("s %+v", s)
	}
	if full := got["full"].Histogram; full == nil || !full.NoSum || !full.NoCount || full.Count != 2 {
		t.Fatalf("full %+v", full)
	}
}

// What a pre-script leaves is checked as what the target wrote is: a bucket
// or a quantile given twice and a count that is a fraction fail the scrape
// naming the series.
func TestAPrometheusPreScriptMustLeaveEachHistogramOneSeries(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "prometheus"}}
	for script, want := range map[string]string{
		`data["metrics"][0]["buckets"].append({"le": 1.0, "count": 6})`:         `the histogram h has two buckets with the upper bound 1`,
		`data["metrics"][1]["quantiles"].append({"quantile": 0.5, "value": 3})`: `the summary s has two values for the quantile 0.5`,
		`data["metrics"][0]["buckets"][0]["count"] = 4.5`:                       `h bucket count 4.5 is not a count of observations, a whole number from 0`,
		`data["metrics"][1]["count"] = 2.5`:                                     `s count 2.5 is not a count of observations, a whole number from 0`,
	} {
		c.Transform.PreScript = script
		if _, err := runBody(t, c, "text/plain", incompleteExposition); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", script, err, want)
		}
	}
}

// A histogram's +Inf bucket and its count are each kept as written, also
// when they differ, as a target read between two of its updates writes them:
// a pre-script is given both numbers, the +Inf entry of buckets and count,
// and what it leaves is read back with both. A script that left a count other
// than its +Inf bucket's, or took the +Inf bucket out of a histogram without
// a count, failed the scrape; it now leaves a histogram with two numbers, or
// with neither, which is passed on as it is.
func TestAPrometheusPreScriptSeesAndLeavesAnInfBucketAndACountThatDiffer(t *testing.T) {
	requirePython(t)
	const torn = "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3\nh_count 8\n"
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "prometheus"}}
	for name, tc := range map[string]struct {
		body, script string
		inf          uint64
		hasInf       bool
		count        uint64
		noCount      bool
	}{
		"the target's two numbers reach the script and come back": {
			body: torn,
			script: `
h = data["metrics"][0]
if h["count"] != 8 or [b["count"] for b in h["buckets"] if b["le"] == float("inf")] != [7]:
    raise ValueError("the script was given %r" % (h,))
`,
			inf: 7, hasInf: true, count: 8,
		},
		"the script changes the count alone":     {body: incompleteExposition, script: `data["metrics"][2]["count"] = 9`, inf: 2, hasInf: true, count: 9},
		"the script sets a count beside buckets": {body: incompleteExposition, script: `data["metrics"][0]["count"] = 9`, inf: 7, hasInf: true, count: 9},
		"the script takes the +Inf bucket out":   {body: incompleteExposition, script: `data["metrics"][0]["buckets"].pop()`, noCount: true},
	} {
		t.Run(name, func(t *testing.T) {
			c.Transform.PreScript = tc.script
			set, err := runBody(t, c, "text/plain", tc.body)
			if err != nil {
				t.Fatal(err)
			}
			index := 0
			if strings.Contains(tc.script, `[2]`) {
				index = 2
			}
			h := set.Metrics[index].Histogram
			if h == nil {
				t.Fatalf("%+v", set.Metrics[index])
			}
			inf, hasInf := h.InfBucket()
			if hasInf != tc.hasInf || hasInf && inf != tc.inf || h.NoCount != tc.noCount || !h.NoCount && h.Count != tc.count {
				t.Fatalf("the histogram is %+v, want a +Inf bucket of %d (%v) and a count of %d (none: %v)", h, tc.inf, tc.hasInf, tc.count, tc.noCount)
			}
		})
	}
}
