package transform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// boundSeries is how many items the responses of the allocation bounds
// below describe: enough that what a transform allocates per series shows
// beside what it allocates once.
const boundSeries = 2000

// transformCost is what one Transform of body by c costs, per series it
// makes: allocations, and bytes allocated, as alloctest measures them. The
// response is decoded once, and transformed once before anything is counted,
// so the expressions are compiled already, as they are on every scrape but a
// collector's first.
func transformCost(t *testing.T, c model.Collector, body string, wantSeries int) (allocs, bytes float64) {
	t.Helper()
	c.Name = "bounded"
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	var set *model.MetricSet
	run := func() { set, err = Transform(context.Background(), d, r, &c, "") }
	run()
	if err != nil || len(set.Metrics) != wantSeries {
		t.Fatalf("%d series, %v; want %d", len(set.Metrics), err, wantSeries)
	}
	allocs, each := alloctest.Allocations(5, run)
	return allocs / float64(wantSeries), float64(each) / float64(wantSeries)
}

// boundItemsJSON is a JSON document of n items, as a jq collector reads.
func boundItemsJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"item-%d","region":"eu-%d","kind":"k%d","value":%d,"size":%d}`, i, i%7, i%3, i, i*1024)
	}
	b.WriteString("]}")
	return b.String()
}

// What a transform allocates for each series it makes is bounded, for a
// response of two thousand items in each format, in allocations and in
// bytes. A series costs its labels' map, which is two allocations while it
// has up to eight labels, and its place in the set, 96 bytes; everything
// above that was overhead these bounds keep away, each set between what the
// transform took before and what it takes now (before → now, allocations
// and bytes per series):
//
//   - the set's slice is made once at its final size, where it grew from
//     nothing and was copied each time it filled, and a rule adds to the
//     set itself: every case, some 270 bytes a series;
//   - a prometheus pass-through hands on the decoded series (0.011, 356 →
//     0.004, 0), and a rule that gives a series no label leaves it the
//     labels it was decoded with (2.01, 692 → 0.004, 90); with a label of
//     its own the rule's series still get a map each (2.01, 692 → 2.0, 426),
//     as do an include's, which copies the series alone (0.011, 356 → 0.004,
//     90);
//   - a jq rule with items no longer makes a list of the one value each of
//     its expressions gives an item (7.9, 911 → 3.9, 491), and rules that
//     share their items read them from one evaluation (7.9, 970 → 2.9, 526);
//   - an XPath label that is an attribute of the parent or the text of a
//     sibling is read from the tree and not by the engine (42, 1978 → 5.0,
//     524);
//   - a regex rule looks its labels' capture groups up once, where each
//     match made an error of asking whether a group's name is a number, and
//     reads its value without boxing the text (8.0, 955 → 3.0, 570); css
//     likewise (12.0, 1027 → 11.0, 655), the rest being goquery's; csv had
//     only the slice to gain (2.0, 692 → 2.0, 426).
//
// The figures are those of a build without the race detector, under which
// the test is skipped (raceDetector).
func TestTransformAllocationsPerSeriesAreBounded(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	jqLabels := []model.LabelRule{{Name: "id", Expression: ".id"}, {Name: "region", Expression: ".region"}, {Name: "kind", Expression: ".kind"}}
	var xml, text, csv, html strings.Builder
	xml.WriteString("<items>")
	csv.WriteString("id,region,value\n")
	html.WriteString("<html><body><table>")
	for i := range boundSeries {
		fmt.Fprintf(&xml, `<item id="item-%d"><region>eu-%d</region><value>%d</value></item>`, i, i%7, i)
		fmt.Fprintf(&text, "item value=%d id=item-%d region=eu-%d\n", i, i, i%7)
		fmt.Fprintf(&csv, "item-%d,eu-%d,%d\n", i, i%7, i)
		fmt.Fprintf(&html, `<tr class="item"><td class="id">item-%d</td><td class="value">%d</td></tr>`, i, i)
	}
	xml.WriteString("</items>")
	html.WriteString("</table></body></html>")
	prometheus := model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
	withRules := func(c model.Collector, rules ...model.MetricRule) model.Collector {
		c.Metrics = rules
		return c
	}
	jq := model.Collector{Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}}
	costs := map[string]float64{}
	for _, tc := range []struct {
		name      string
		collector model.Collector
		body      string
		series    int
		// maxAllocs and maxBytes bound what a series may cost; zero is no
		// bound, where there was nothing to gain, or a handful of
		// allocations in a whole transform, which the bytes tell better.
		maxAllocs, maxBytes float64
	}{
		{"prometheus passthrough", prometheus, promSeries(boundSeries), boundSeries, 0, 60},
		{"prometheus rule without labels", withRules(prometheus, model.MetricRule{Name: "n"}), promSeries(boundSeries), boundSeries, 1, 300},
		{"prometheus rule with labels", withRules(prometheus, model.MetricRule{Name: "n", Labels: []model.LabelRule{{Name: "site", Value: "a"}}}), promSeries(boundSeries), boundSeries, 0, 560},
		{"prometheus include", model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus", Include: []string{"^n$"}}}, promSeries(boundSeries), boundSeries, 0, 220},
		{"jq items, one rule", withRules(jq, model.MetricRule{Name: "item_value", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".value", Labels: jqLabels}), boundItemsJSON(boundSeries), boundSeries, 6, 700},
		{"jq items, two rules sharing them", withRules(jq,
			model.MetricRule{Name: "item_value", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".value", Labels: jqLabels},
			model.MetricRule{Name: "item_size_bytes", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".size", Labels: jqLabels}), boundItemsJSON(boundSeries), 2 * boundSeries, 3.4, 610},
		{"jq items, two rules with items of their own", withRules(jq,
			model.MetricRule{Name: "item_value", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".value", Labels: jqLabels},
			model.MetricRule{Name: "item_size_bytes", Type: model.GaugeMetricType, Items: ".items[1:][]", Expression: ".size", Labels: jqLabels}), boundItemsJSON(boundSeries), 2*boundSeries - 1, 6, 0},
		{"xpath", model.Collector{Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
			Metrics: []model.MetricRule{{Name: "node_value", Type: model.GaugeMetricType, Expression: "//item/value", Labels: []model.LabelRule{{Name: "id", Expression: "../@id"}, {Name: "region", Expression: "../region"}}}}}, xml.String(), boundSeries, 15, 1000},
		{"regex", model.Collector{Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
			Metrics: []model.MetricRule{{Name: "line_value", Type: model.GaugeMetricType, Expression: `(?m)^item value=(\d+) id=(?P<id>\S+) region=(?P<region>\S+)$`, Labels: []model.LabelRule{{Name: "id", Expression: "id"}, {Name: "region", Expression: "region"}}}}}, text.String(), boundSeries, 5, 760},
		{"csv", model.Collector{Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"},
			Metrics: []model.MetricRule{{Name: "row_value", Type: model.GaugeMetricType, Expression: "value", Labels: []model.LabelRule{{Name: "id", Expression: "id"}, {Name: "region", Expression: "region"}}}}}, csv.String(), boundSeries, 0, 560},
		{"css items", model.Collector{Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "css"},
			Metrics: []model.MetricRule{{Name: "row_value", Type: model.GaugeMetricType, Items: "tr.item", Expression: "td.value", Labels: []model.LabelRule{{Name: "id", Expression: "td.id"}}}}}, html.String(), boundSeries, 11.5, 840},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocs, bytes := transformCost(t, tc.collector, tc.body, tc.series)
			costs[tc.name] = allocs
			t.Logf("%.3f allocations and %.0f bytes per series", allocs, bytes)
			if tc.maxAllocs > 0 && allocs > tc.maxAllocs {
				t.Errorf("%.3f allocations per series, more than %v", allocs, tc.maxAllocs)
			}
			if tc.maxBytes > 0 && bytes > tc.maxBytes {
				t.Errorf("%.0f bytes per series, more than %v", bytes, tc.maxBytes)
			}
		})
	}
	// Rules that each read items of their own pay nothing for the sharing
	// they do not use: a series of theirs costs what a series of one rule
	// alone does.
	if one, own := costs["jq items, one rule"], costs["jq items, two rules with items of their own"]; own > one+0.05 {
		t.Errorf("a series of two rules with items of their own costs %.3f allocations, of one rule %.3f", own, one)
	}
}

// The room a transform makes for its series beforehand is never more than
// limits.max_metrics allows them: a response that describes twenty thousand
// series, of which five have a value, gets room for as many as the limit of
// a hundred, give or take what the allocator rounds a size up by, and not
// for twenty thousand. Each rule carries on without the items that have no
// value, so the scrape passes with the five.
func TestRoomForSeriesIsBoundedByTheLimit(t *testing.T) {
	const described, limit = 20000, 100
	optional := false
	// each is a response of described items made by item, of which every
	// four-thousandth has the value 1 and the others none.
	each := func(before string, item func(value string) string, after string) string {
		var b strings.Builder
		b.WriteString(before)
		for i := range described {
			if i%4000 == 0 {
				b.WriteString(item("1"))
			} else {
				b.WriteString(item(""))
			}
		}
		b.WriteString(after)
		return b.String()
	}
	for _, tc := range []struct {
		name      string
		collector model.Collector
		body      string
	}{
		{"csv", model.Collector{Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"},
			Metrics: []model.MetricRule{{Name: "n", Expression: "v", Required: &optional}}},
			each("v,id\n", func(value string) string { return value + ",a\n" }, "")},
		{"xpath", model.Collector{Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
			Metrics: []model.MetricRule{{Name: "n", Expression: "//v", Required: &optional}}},
			each("<r>", func(value string) string { return "<v>" + value + "</v>" }, "</r>")},
		{"css items", model.Collector{Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "css"},
			Metrics: []model.MetricRule{{Name: "n", Items: "li", Expression: "b", Required: &optional}}},
			each("<html><body><ul>", func(value string) string { return "<li><b>" + value + "</b></li>" }, "</ul></body></html>")},
		{"regex", model.Collector{Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
			Metrics: []model.MetricRule{{Name: "n", Expression: `v=(\d*)`, Required: &optional}}},
			each("", func(value string) string { return "v=" + value + "\n" }, "")},
		{"jq items", model.Collector{Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
			Metrics: []model.MetricRule{{Name: "n", Items: ".items[]", Expression: ".v", Required: &optional}, {Name: "m", Items: ".items[]", Expression: ".v", Required: &optional}}},
			each(`{"items":[`, func(value string) string {
				if value == "" {
					value = "null"
				}
				return `{"v":` + value + `},`
			}, `{}]}`)},
		{"prometheus rules", model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
			Metrics: []model.MetricRule{{Name: "n", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{{Name: "kept", Expression: "v", Required: true}}}}},
			func() string {
				i := 0
				return each("# TYPE n gauge\n", func(value string) string {
					i++
					return fmt.Sprintf("n{i=\"%d\",v=\"%s\"} 1\n", i, value)
				}, "")
			}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.collector
			c.Name, c.Limits.MaxMetrics = "bounded", limit
			want := 5
			if tc.name == "jq items" {
				want = 10
			}
			// Decoded without the limit, which the prometheus decoder
			// would stop at itself.
			unlimited := c
			unlimited.Limits.MaxMetrics = 0
			d, r := decodedBody(t, unlimited, "", tc.body)
			set, err := Transform(LeaveRuleLoggingToCaller(context.Background()), d, r, &c, "")
			if err != nil || len(set.Metrics) != want {
				t.Fatalf("%d series, %v; want %d", len(set.Metrics), err, want)
			}
			if cap(set.Metrics) >= 2*limit {
				t.Fatalf("room was made for %d series, with a limit of %d", cap(set.Metrics), limit)
			}
		})
	}
}
