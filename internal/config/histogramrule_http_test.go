//go:build !select_request_types || request_type_http

package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A rule's histogram block, which makes a histogram of the values the rule
// reads, loads on a rule of type histogram of every transform that reads
// values; type: histogram without it is refused there, as it was, now with
// the block as the way out. What the block may not be is refused when the
// configuration loads, each in words that say what to write instead
// (checkHistogramRule).
func TestAHistogramBlockIsCheckedWhenTheConfigurationLoads(t *testing.T) {
	expressions := map[string]string{"jq": ".r[]", "yq": ".r[]", "xpath": "//v", "css": "li", "regex": `'v=(\d+)'`, "csv": "v"}
	for transform, expression := range expressions {
		rule := "      - name: duration_seconds\n        type: histogram\n        expression: " + expression + "\n        scale: 0.001\n        labels:\n          - name: kind\n            value: fixed\n        histogram:\n          buckets: [0.005, 0.01, 0.5, 1]\n"
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", collectorRules(transform, "", rule)))
		if err != nil {
			t.Fatalf("a %s histogram rule does not load: %v", transform, err)
		}
		if got := cfg.Collectors[0].Metrics[0].Histogram; got == nil || len(got.Buckets) != 4 || got.Buckets[3] != 1 {
			t.Fatalf("a %s histogram rule loads with the block %+v", transform, got)
		}
		without := "      - name: duration_seconds\n        type: histogram\n        expression: " + expression + "\n"
		if got := problemsOf(t, collectorRules(transform, "", without)); len(got) != 1 || !strings.HasSuffix(got[0], "so use gauge, counter or untyped, or add a histogram block with the upper bounds of its buckets, as in histogram: {buckets: [0.1, 0.5, 1]}, to count the values the rule reads in a histogram") {
			t.Errorf("a %s rule of type histogram without the block: the load says %q", transform, got)
		}
	}

	const where = `collector "node" metric "h"`
	for name, tc := range map[string]struct{ transform, rule, want string }{
		"on a gauge": {"jq", "type: gauge\n        histogram: {buckets: [1]}\n",
			" sets histogram, but its type is gauge; the block makes a histogram of the values the rule reads, so set type: histogram, or take the block out"},
		"without a type": {"jq", "histogram: {buckets: [1]}\n",
			" sets histogram, but its type is gauge; the block makes a histogram of the values the rule reads, so set type: histogram, or take the block out"},
		"on a summary": {"jq", "type: summary\n        histogram: {buckets: [1]}\n",
			" sets histogram, but its type is summary; the block makes a histogram of the values the rule reads, so set type: histogram, or take the block out"},
		"of a prometheus rule": {"prometheus", "type: histogram\n        histogram: {buckets: [1]}\n",
			" sets histogram, which a prometheus rule does not take: it passes on the histograms the target exposes, with their own buckets; take the block out"},
		"of a python rule": {"python", "histogram: {buckets: [1]}\n",
			" sets histogram, which a python rule does not take: the script makes each of its series and gives it its type itself; take the block out"},
		"without buckets": {"jq", "type: histogram\n        histogram: {}\n",
			" histogram has no buckets; list the upper bounds of its buckets in increasing order, as in buckets: [0.1, 0.5, 1], and the bucket +Inf is added"},
		"with no buckets": {"jq", "type: histogram\n        histogram: {buckets: []}\n",
			" histogram has no buckets; list the upper bounds of its buckets in increasing order, as in buckets: [0.1, 0.5, 1], and the bucket +Inf is added"},
		"with +Inf": {"jq", "type: histogram\n        histogram: {buckets: [1, .inf]}\n",
			" histogram bucket 2 is +Inf, which every histogram has as its last bucket; leave it out"},
		"with -Inf": {"jq", "type: histogram\n        histogram: {buckets: [-.inf, 1]}\n",
			" histogram bucket 1 is -Inf, not a finite number"},
		"with NaN": {"jq", "type: histogram\n        histogram: {buckets: [1, .nan]}\n",
			" histogram bucket 2 is NaN, not a finite number"},
		"out of order": {"jq", "type: histogram\n        histogram: {buckets: [0.1, 1, 0.5]}\n",
			" histogram bucket 3, 0.5, is not greater than the one before it, 1; list the upper bounds in increasing order, each once"},
		"twice": {"jq", "type: histogram\n        histogram: {buckets: [0.1, 0.1]}\n",
			" histogram bucket 2, 0.1, is not greater than the one before it, 0.1; list the upper bounds in increasing order, each once"},
		"too many": {"jq", "type: histogram\n        histogram:\n          buckets: [" + strings.Join(ascending(model.MaxHistogramBuckets+1), ", ") + "]\n",
			" histogram has 101 buckets, more than the most, 100; every bucket is a sample of every histogram on every scrape, so choose fewer bounds"},
		"with a label le": {"jq", "type: histogram\n        histogram: {buckets: [1]}\n        labels:\n          - name: le\n            value: x\n",
			" has a label le, which a histogram's buckets carry; name the label something else"},
	} {
		rule := "      - name: h\n        expression: .v\n        " + tc.rule
		if got := problemsOf(t, collectorRules(tc.transform, "", rule)); len(got) != 1 || got[0] != where+tc.want {
			t.Errorf("%s: the load says\n%q\nwant\n%q", name, got, where+tc.want)
		}
	}
	// The most buckets load.
	most := "      - name: h\n        expression: .v\n        type: histogram\n        histogram:\n          buckets: [" + strings.Join(ascending(model.MaxHistogramBuckets), ", ") + "]\n"
	if got := problemsOf(t, collectorRules("jq", "", most)); got != nil {
		t.Errorf("%d buckets: the load says %q", model.MaxHistogramBuckets, got)
	}
}

// ascending is n bounds in increasing order: 1, 2, 3 and on.
func ascending(n int) []string {
	bounds := make([]string, n)
	for i := range bounds {
		bounds[i] = strconv.Itoa(i + 1)
	}
	return bounds
}
