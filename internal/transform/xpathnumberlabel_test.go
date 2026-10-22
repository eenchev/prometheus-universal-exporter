package transform

import (
	"context"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A number an XPath label computes is written as a jq or yq label's is
// (labelNumber), where Go's shortest form made exponents of a million and
// more: count(//item) * 2000000 was the label "4e+06" and the same number
// from jq "4000000". Over XML and HTML, for a label that is one sum() too,
// which has a path of its own (summedLabelText).
func TestAnXPathNumberLabelReadsAsAJQOne(t *testing.T) {
	const body = `<html><body><r><item n="1234567"/><item n="0.5"/></r></body></html>`
	for _, tc := range []struct {
		xpath, jq, want string
	}{
		{"count(//item) * 2000000", "2 * 2000000", "4000000"},
		{"sum(//item/@n)", "1234567 + 0.5", "1234567.5"},
		{"sum(//item/@n) * 1000", "1234567500", "1234567500"},
		{"0 - 3000000", "-3000000", "-3000000"},
		{"1 div 4", "1 / 4", "0.25"},
		{"-1 div 8", "-1 / 8", "-0.125"},
		{"0.000001", "0.000001", "0.000001"},
		{"1 div 10000000", "1 / 10000000", "1e-07"},
		{"1000000 * 1000000 * 1000000 * 1000", "1e21", "1e+21"},
		{"0 * -1", "0 * -1", "0"},
	} {
		set, err := runJQLabels(t, `{}`, model.MetricRule{Name: "up", Type: model.GaugeMetricType, Expression: "1", Labels: []model.LabelRule{{Name: "n", Expression: tc.jq}}})
		if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Labels["n"] != tc.want {
			t.Fatalf("jq %q: %+v %v; want the label %q", tc.jq, set, err, tc.want)
		}
		for _, decoder := range []string{"xml", "html"} {
			c := xpathRuleCollector(decoder, model.MetricRule{Name: "up", Type: model.GaugeMetricType, Expression: "count(//item)", Labels: []model.LabelRule{{Name: "n", Expression: tc.xpath}}})
			if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
				t.Fatal(err)
			}
			set, _, err := transformWith(context.Background(), t, c, markupContentType[decoder], body)
			if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Labels["n"] != tc.want {
				t.Errorf("%s xpath %q: %v %v; want the label %q, as jq's", decoder, tc.xpath, htmlSeries(set), err, tc.want)
			}
		}
	}
}
