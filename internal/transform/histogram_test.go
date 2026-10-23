package transform

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A metric rule with a histogram block counts the values it reads in a
// histogram (histogram.go) instead of making each a series.

// histogramRule is a rule of type histogram of the buckets, under log.
func histogramRule(name, expression string, buckets ...float64) model.MetricRule {
	return model.MetricRule{Name: name, Expression: expression, Type: model.HistogramMetricType, ErrorMode: model.ErrorModeLog, Histogram: &model.HistogramRule{Buckets: buckets}}
}

// shownHistogram is a histogram series as a test compares it: its labels,
// and each bucket, the sum and the count, as the text format writes them.
func shownHistogram(m model.Metric) string {
	if m.Type != model.HistogramMetricType || m.Histogram == nil {
		return fmt.Sprintf("%s is a %s of %v, not a histogram", m.Name, m.Type, m.Value)
	}
	var b strings.Builder
	labels := slices.Sorted(maps.Keys(m.Labels))
	for _, name := range labels {
		fmt.Fprintf(&b, "%s=%s ", name, m.Labels[name])
	}
	for _, bucket := range m.Histogram.Buckets {
		fmt.Fprintf(&b, "le=%g:%d ", bucket.UpperBound, bucket.CumulativeCount)
	}
	fmt.Fprintf(&b, "sum=%g count=%d", m.Histogram.Sum, m.Histogram.Count)
	return b.String()
}

// shownHistograms are the histograms of a set, each shown, in the order of
// the set.
func shownHistograms(set *model.MetricSet) []string {
	if set == nil {
		return nil
	}
	var shown []string
	for _, m := range set.Metrics {
		shown = append(shown, shownHistogram(m))
	}
	return shown
}

// The buckets are cumulative, a value on a bound counted in that bound's
// bucket, as Prometheus' le is "less than or equal"; +Inf counts every
// value, as the count does; the sum is of the values; scale applies to each
// value before it is counted, so milliseconds land in buckets of seconds.
// An infinity is counted as client libraries count one.
func TestAHistogramRuleCountsItsValuesInCumulativeBuckets(t *testing.T) {
	milli := 0.001
	for name, tc := range map[string]struct {
		values string
		scale  *float64
		want   string
	}{
		"on and between the bounds": {`[1, 5, 5.5, 10, 250, 2000]`, &milli, "le=0.005:2 le=0.01:4 le=0.1:4 le=1:5 le=+Inf:6 sum=2.2715 count=6"},
		"below the first bound":     {`[-3, 0]`, &milli, "le=0.005:2 le=0.01:2 le=0.1:2 le=1:2 le=+Inf:2 sum=-0.003 count=2"},
		"above the last bound":      {`[1500, 5000]`, &milli, "le=0.005:0 le=0.01:0 le=0.1:0 le=1:0 le=+Inf:2 sum=6.5 count=2"},
		"without a scale":           {`[0.005, 0.01, 1]`, nil, "le=0.005:1 le=0.01:2 le=0.1:2 le=1:3 le=+Inf:3 sum=1.015 count=3"},
		"text that is a number":     {`["0.5", " 0.05 "]`, nil, "le=0.005:0 le=0.01:0 le=0.1:1 le=1:2 le=+Inf:2 sum=0.55 count=2"},
		"infinities":                {`["Inf", "-Inf"]`, nil, "le=0.005:1 le=0.01:1 le=0.1:1 le=1:1 le=+Inf:2 sum=NaN count=2"},
		"+Inf alone":                {`["+Inf", 0.5]`, nil, "le=0.005:0 le=0.01:0 le=0.1:0 le=1:1 le=+Inf:2 sum=+Inf count=2"},
	} {
		t.Run(name, func(t *testing.T) {
			rule := histogramRule("request_duration_seconds", ".requests[]", 0.005, 0.01, 0.1, 1)
			rule.Scale = tc.scale
			c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{rule}}
			set, err := runBody(t, c, "application/json", `{"requests": `+tc.values+`}`)
			if err != nil {
				t.Fatal(err)
			}
			if got := shownHistograms(set); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %q, want [%q]", got, tc.want)
			}
		})
	}
}

// Every transform that reads values with rules counts them alike: the
// values the rule reads — those its jq or yq expression gives, or one per
// item, the nodes its XPath selects, the elements its CSS selector matches
// with items or without, the matches of its regex, a column's cells — are
// one histogram for each set of label values, in the order its first value
// came, and a label that is static is on every histogram. The values are
// 0.5, 1 and 2 of a and 7 of b.
func TestEveryTransformCountsAHistogramRulesValues(t *testing.T) {
	both := []string{
		"kind=fixed site=a le=1:2 le=5:3 le=+Inf:3 sum=3.5 count=3",
		"kind=fixed site=b le=1:0 le=5:0 le=+Inf:1 sum=7 count=1",
	}
	one := []string{"kind=fixed le=1:2 le=5:3 le=+Inf:4 sum=10.5 count=4"}
	static := model.LabelRule{Name: "kind", Value: "fixed"}
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body string
		items, expression, site               string
		want                                  []string
	}{
		"jq":                             {"json", "jq", "application/json", `{"r": [{"s": "a", "v": 0.5}, {"s": "b", "v": 7}, {"s": "a", "v": 1}, {"s": "a", "v": 2}]}`, "", ".r[].v", ".r[].s", both},
		"jq items":                       {"json", "jq", "application/json", `{"r": [{"s": "a", "v": 0.5}, {"s": "b", "v": 7}, {"s": "a", "v": 1}, {"s": "a", "v": 2}]}`, ".r[]", ".v", ".s", both},
		"jq without labels that read":    {"json", "jq", "application/json", `{"r": [0.5, 7, 1, 2]}`, "", ".r[]", "", one},
		"yq":                             {"yaml", "yq", "application/yaml", "r:\n  - {s: a, v: 0.5}\n  - {s: b, v: 7}\n  - {s: a, v: 1}\n  - {s: a, v: 2}\n", ".r[]", ".v", ".s", both},
		"xpath":                          {"xml", "xpath", "application/xml", `<r><i s="a">0.5</i><i s="b">7</i><i s="a">1</i><i s="a">2</i></r>`, "", "/r/i", "@s", both},
		"xpath without labels that read": {"xml", "xpath", "application/xml", `<r><i>0.5</i><i>7</i><i>1</i><i>2</i></r>`, "", "/r/i", "", one},
		"css items":                      {"html", "css", "text/html", `<table><tr><td class="s">a</td><td class="v">0.5</td></tr><tr><td class="s">b</td><td class="v">7</td></tr><tr><td class="s">a</td><td class="v">1</td></tr><tr><td class="s">a</td><td class="v">2</td></tr></table>`, "tr", "td.v", "td.s", both},
		"css without items":              {"html", "css", "text/html", `<ul><li>0.5</li><li>7</li><li>1</li><li>2</li></ul>`, "", "li", "", one},
		"csv":                            {"csv", "csv", "text/csv", "s,v\na,0.5\nb,7\na,1\na,2\n", "", "v", "s", both},
		"regex":                          {"text", "regex", "text/plain", "a=0.5\nb=7\na=1\na=2\n", "", `(?P<site>\w)=(?P<value>[\d.]+)`, "site", both},
	} {
		t.Run(name, func(t *testing.T) {
			rule := histogramRule("duration_seconds", tc.expression, 1, 5)
			rule.Items = tc.items
			rule.Labels = []model.LabelRule{static}
			if tc.site != "" {
				rule.Labels = append(rule.Labels, model.LabelRule{Name: "site", Expression: tc.site})
			}
			c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: []model.MetricRule{rule}}
			if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
				t.Fatal(err)
			}
			set, err := runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if got := shownHistograms(set); !slices.Equal(got, tc.want) {
				t.Fatalf("got\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// A histogram rule beside rules of other types leaves theirs as they were:
// a csv gauge still makes a series of each row, kept together, and the
// histogram follows them.
func TestAHistogramRuleBesideAGaugeRule(t *testing.T) {
	gauge := model.MetricRule{Name: "size_bytes", Expression: "size", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "file", Expression: "file"}}}
	c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"}, Metrics: []model.MetricRule{histogramRule("size_distribution_bytes", "size", 10, 100), gauge}}
	set, err := runBody(t, c, "text/csv", "file,size\na,5\nb,50\nc,500\n")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range set.Metrics {
		if m.Histogram != nil {
			got = append(got, shownHistogram(m))
			continue
		}
		got = append(got, fmt.Sprintf("%s %s %g", m.Name, m.Labels["file"], m.Value))
	}
	want := []string{"size_bytes a 5", "size_bytes b 50", "size_bytes c 500", "le=10:1 le=100:2 le=+Inf:3 sum=555 count=3"}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

// An empty list is a histogram that counted nothing, its series there as it
// is for a list with values, and no missing value, whatever required says,
// in every transform: a jq expression that gives no value, items that
// select nothing, an XPath or a CSS selector that selects nothing, a regex
// that matches nothing, a CSV of no rows. A rule with a label that reads
// the response has no labels to give such a histogram, and makes none.
func TestAnEmptyListIsAHistogramOfNothing(t *testing.T) {
	const zero = "kind=fixed le=1:0 le=5:0 le=+Inf:0 sum=0 count=0"
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body string
		items, expression, label              string
	}{
		"jq":        {"json", "jq", "application/json", `{"r": []}`, "", ".r[]", ".s"},
		"jq items":  {"json", "jq", "application/json", `{"r": []}`, ".r[]", ".v", ".s"},
		"xpath":     {"xml", "xpath", "application/xml", `<r/>`, "", "/r/i", "@s"},
		"css":       {"html", "css", "text/html", `<ul></ul>`, "", "li", ""},
		"css items": {"html", "css", "text/html", `<table></table>`, "tr", "td", "td.s"},
		"regex":     {"text", "regex", "text/plain", "nothing here\n", "", `v=(\d+)`, "1"},
		"csv":       {"csv", "csv", "text/csv", "s,v\n", "", "v", "s"},
	} {
		t.Run(name, func(t *testing.T) {
			rule := histogramRule("duration_seconds", tc.expression, 1, 5)
			rule.Items = tc.items
			required := true
			rule.Required = &required
			rule.ErrorMode = model.ErrorModeFail
			rule.Labels = []model.LabelRule{{Name: "kind", Value: "fixed"}}
			c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: []model.MetricRule{rule}}
			set, err := runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if got := shownHistograms(set); len(got) != 1 || got[0] != zero {
				t.Fatalf("got %q, want [%q]", got, zero)
			}
			if tc.label == "" {
				return
			}
			c.Metrics[0].Labels = append(c.Metrics[0].Labels, model.LabelRule{Name: "site", Expression: tc.label})
			set, err = runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if got := shownHistograms(set); len(got) != 0 {
				t.Fatalf("with a label that reads the response, got %q, want no histogram", got)
			}
		})
	}
}

// A value that is no number, NaN among them, which no bucket holds, and a
// value that is missing are each handled by the rule's error_mode, as any
// rule's value is: under log and ignore the histogram counts the other
// values, the failures counted against the rule, and under fail the scrape
// fails, naming the value.
func TestAHistogramValueThatFailsFollowsTheErrorMode(t *testing.T) {
	const counted = "le=1:1 le=5:2 le=+Inf:2 sum=2.5 count=2"
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body, expression string
		failures                                          uint64
		failure                                           string
	}{
		"jq":    {"json", "jq", "application/json", `{"r": [0.5, "slow", "NaN", null, 2]}`, ".r[]", 3, `value "slow" is not a number`},
		"xpath": {"xml", "xpath", "application/xml", `<r><i>0.5</i><i>slow</i><i>NaN</i><i/><i>2</i></r>`, "/r/i", 3, `value "slow" is not a number`},
		"csv":   {"csv", "csv", "text/csv", "v\n0.5\nslow\nNaN\n\n2\n", "v", 2, `value "slow" is not a number`},
		"css":   {"html", "css", "text/html", `<ul><li>0.5</li><li>slow</li><li>NaN</li><li> </li><li>2</li></ul>`, "li", 3, `value "slow" is not a number`},
		"nan":   {"json", "jq", "application/json", `{"r": [0.5, "NaN", 2]}`, ".r[]", 1, "value is NaN, which a histogram cannot count"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail} {
				rule := histogramRule("duration_seconds", tc.expression, 1, 5)
				rule.ErrorMode = mode
				c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: []model.MetricRule{rule}}
				ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(context.Background()))
				set, err := runBodyIn(t, ctx, c, tc.contentType, tc.body)
				if mode == model.ErrorModeFail {
					var failure *MetricFailure
					if !errors.As(err, &failure) || !strings.Contains(err.Error(), tc.failure) {
						t.Fatalf("%s: the scrape answers %v, want the rule's failure %q", mode, err, tc.failure)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				if got := shownHistograms(set); len(got) != 1 || got[0] != counted {
					t.Fatalf("%s: got %q, want [%q]", mode, got, counted)
				}
				var failures uint64
				for _, f := range report.Failures() {
					failures += f.Failures
				}
				if failures != tc.failures {
					t.Fatalf("%s: %d failures counted, want %d: %+v", mode, failures, tc.failures, report.Failures())
				}
			}
		})
	}
}

// limits.max_metrics counts histograms, not the values in them: a hundred
// thousand values of one histogram are one series, and a rule whose labels
// split its values into more histograms than the limit allows fails the
// scrape as a rule of too many series does.
func TestAHistogramIsOneSeriesOfTheLimit(t *testing.T) {
	var body strings.Builder
	body.WriteString("v\n")
	for i := range 100000 {
		fmt.Fprintf(&body, "%d\n", i%7)
	}
	c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"}, Limits: model.Limits{MaxMetrics: 1},
		Metrics: []model.MetricRule{histogramRule("value", "v", 3)}}
	set, err := runBody(t, c, "text/csv", body.String())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shownHistograms(set), "le=3:57144 le=+Inf:100000 sum=299995 count=100000"; len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want [%q]", got, want)
	}
	c = model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Limits: model.Limits{MaxMetrics: 2},
		Metrics: []model.MetricRule{histogramRule("value", ".[].v", 3)}}
	c.Metrics[0].Labels = []model.LabelRule{{Name: "site", Expression: ".[].s"}}
	if _, err := runBody(t, c, "application/json", `[{"s": "a", "v": 1}, {"s": "b", "v": 1}, {"s": "a", "v": 2}]`); err != nil {
		t.Fatalf("two histograms under a limit of 2: %v", err)
	}
	_, err = runBody(t, c, "application/json", `[{"s": "a", "v": 1}, {"s": "b", "v": 1}, {"s": "c", "v": 2}]`)
	if err == nil || !strings.Contains(err.Error(), "metric count 3 exceeds limit 2") {
		t.Fatalf("three histograms under a limit of 2: %v, want the limit's failure", err)
	}
}

// Labels with an empty value are no labels, so values whose labels differ
// only in one are one histogram's, as they are one series to Prometheus.
func TestHistogramLabelsOfAnEmptyValueAreNone(t *testing.T) {
	rule := histogramRule("value", ".v", 3)
	rule.Items = ".[]"
	rule.Labels = []model.LabelRule{{Name: "site", Expression: ".s"}}
	c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{rule}}
	set, err := runBody(t, c, "application/json", `[{"s": "", "v": 1}, {"v": 2}, {"s": null, "v": 4}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shownHistograms(set), "le=3:2 le=+Inf:3 sum=7 count=3"; len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want [%q]", got, want)
	}
}

// A collector without a histogram rule gets no histogram folds, so its
// scrape allocates nothing for them; one with a rule does.
func TestOnlyACollectorWithAHistogramRuleFoldsHistograms(t *testing.T) {
	ctx := context.Background()
	plain := &model.Collector{Metrics: []model.MetricRule{{Name: "v", Type: model.GaugeMetricType}}}
	if withHistogramFolds(ctx, plain) != ctx {
		t.Error("a collector without a histogram rule has histogram folds")
	}
	histograms := &model.Collector{Metrics: []model.MetricRule{{Name: "v"}, histogramRule("h", ".h", 1)}}
	if _, ok := withHistogramFolds(ctx, histograms).Value(histogramFoldsKey{}).(histogramFolds); !ok {
		t.Error("a collector with a histogram rule has no histogram folds")
	}
}

// runBodyIn is runBody with the context the Transform is given.
func runBodyIn(t *testing.T, ctx context.Context, c model.Collector, contentType, body string) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Content-Type": {contentType}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return Transform(ctx, d, r, &c, "python3")
}

// Two sets of labels are two histograms whatever bytes their values hold: a
// value read from a response is repaired to UTF-8 only after the transform,
// so a byte of the key's own is no end to it. {a="x\xfeb\xffy"} once took
// the key of {a="x", b="y"} and counted its value.
func TestHistogramLabelsOfAnyBytesAreTheirOwnHistogram(t *testing.T) {
	rule := histogramRule("value", `(?m)^(?P<a>\S+) (?P<b>\S*) *(?P<value>\d+)$`, 6)
	rule.Labels = []model.LabelRule{{Name: "a", Expression: "a"}, {Name: "b", Expression: "b"}}
	c := model.Collector{Name: "h", Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"}, Metrics: []model.MetricRule{rule}}
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, err := runBody(t, c, "text/plain", "x\xfeb\xffy  5\nx y 7\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"a=x�b�y le=6:1 le=+Inf:1 sum=5 count=1",
		"a=x b=y le=6:0 le=+Inf:1 sum=7 count=1",
	}
	if got := shownHistograms(set); !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

// histogramKey gives two sets the same key only when they are one series:
// the same labels of a value, whichever bytes the names and values hold and
// however many labels of an empty value either has.
func TestHistogramKeysAreTheSameOnlyForOneSeries(t *testing.T) {
	// Parts of the bytes that once ended a name or a value, and of what a
	// length written before one looks like.
	parts := []string{"", "a", "b", "x", "\xfe", "\xff", "\x00", "\x01", "\x02", "a\xffb", "\xfeb\xff", "x\xfeb\xff", "1", "\x01a", "\x01a\x01"}
	var sets []map[string]string
	for _, n1 := range []string{"a", "b"} {
		for _, v1 := range parts {
			sets = append(sets, map[string]string{n1: v1})
			for _, v2 := range parts {
				sets = append(sets, map[string]string{"a": v1, "b": v2})
				sets = append(sets, map[string]string{n1: v1 + v2})
				sets = append(sets, map[string]string{"a": v1, "b": v2, "c": ""})
			}
		}
	}
	same := func(x, y map[string]string) bool {
		valued := func(m map[string]string) map[string]string {
			out := map[string]string{}
			for k, v := range m {
				if v != "" {
					out[k] = v
				}
			}
			return out
		}
		return maps.Equal(valued(x), valued(y))
	}
	keys := make([]string, len(sets))
	for i, set := range sets {
		keys[i] = histogramKey(set)
	}
	for i := range sets {
		for j := range i {
			if (keys[i] == keys[j]) != same(sets[i], sets[j]) {
				t.Fatalf("%q and %q: same key %t, one series %t", sets[i], sets[j], keys[i] == keys[j], same(sets[i], sets[j]))
			}
		}
	}
}

// FuzzHistogramKey looks for two sets of two labels that are not one series
// and share a key.
func FuzzHistogramKey(f *testing.F) {
	f.Add("a", "x\xfeb\xffy", "c", "", "a", "x", "b", "y")
	f.Add("a", "\x01b", "", "", "a", "", "b", "")
	f.Fuzz(func(t *testing.T, n1, v1, n2, v2, n3, v3, n4, v4 string) {
		x := map[string]string{n1: v1, n2: v2}
		y := map[string]string{n3: v3, n4: v4}
		valued := func(m map[string]string) map[string]string {
			for k, v := range m {
				if v == "" {
					delete(m, k)
				}
			}
			return m
		}
		kx, ky := histogramKey(x), histogramKey(y)
		if one := maps.Equal(valued(x), valued(y)); (kx == ky) != one {
			t.Fatalf("%q and %q: same key %t, one series %t", x, y, kx == ky, one)
		}
	})
}
