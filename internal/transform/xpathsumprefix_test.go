package transform

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A sum() written with a prefix, which the engine passes over, is the sum it
// is without one: alone it is added up by the exporter, each node trimmed,
// where it was taken for a part of a larger expression and had no value
// over numbers with blanks around them; with text that is no number it is
// missing its value as the sum without the prefix is, in the same words;
// and as a part of a larger expression it is what it was.
func TestAPrefixedSumAloneIsAddedUpAsTheSumIs(t *testing.T) {
	const spaced, text = `<r><v> 2 </v><v>4</v></r>`, `<r><v>2</v><v>n/a</v></r>`
	for _, test := range []struct{ expression, body, want string }{
		{"sum(//v)", spaced, "m 6"},
		{"fn:sum(//v)", spaced, "m 6"},
		{" x:sum( //v ) ", spaced, "m 6"},
		{"fn:sum(//v)", text, `metric "m" XPath "fn:sum(//v)" cannot be computed: it adds up text that is not a number, first "n/a" (1 of 2 nodes); `},
		{"fn:sum(//v) + 0", spaced, `metric "m" XPath "fn:sum(//v) + 0" cannot be computed: sum(//v) leaves out text it cannot read as a number, first " 2 " (1 of 2 nodes), and blanks around a number count; `},
		{"fn:sum(//v) + 0", `<r><v>2</v><v>4</v></r>`, "m 6"},
	} {
		c := xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: test.expression, ErrorMode: model.ErrorModeFail})
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Fatal(err)
		}
		set, _, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, "application/xml", test.body)
		// A failure is told by how it starts: what to do about it follows.
		got := strings.Join(htmlSeries(set), "; ")
		if err != nil {
			got = err.Error()
		}
		if got != test.want && (err == nil || !strings.HasSuffix(test.want, "; ") || !strings.HasPrefix(got, test.want)) {
			t.Errorf("%s over %s: %q, want %q", test.expression, test.body, got, test.want)
		}
	}
	// As a label too.
	c := xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "count(//v)", Labels: []model.LabelRule{{Name: "total", Expression: "fn:sum(//v)"}}})
	set, _, err := transformWith(context.Background(), t, c, "application/xml", spaced)
	if err != nil || !slices.Equal(htmlSeries(set), []string{`m{total="6"} 2`}) {
		t.Errorf("the label fn:sum(//v): %q, %v", htmlSeries(set), err)
	}
}
