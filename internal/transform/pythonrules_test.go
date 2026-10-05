package transform

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Under a python transform the script makes every series, and of a rule of
// the collector the transform reads one thing: the labels it marks
// truncate: true, by the rule's name and the label's. Everything else a
// python rule may hold changes no series: its expression and its labels'
// expressions are not read, as the documentation says, and neither is a
// label's value, which is why the load refuses one; nor are the rule's type
// and description, whether the script gives the series its own or leaves
// them out, when the series is a gauge without a help text, and nor are
// required and error_mode, since a rule that makes no series has no value
// to miss. The script's series are the same with such a rule as with none.
// Which is why the load refuses each of them but the expressions, and with
// them a rule without a name and a label without truncate: true, the last
// two cases here (config.checkPythonRule).
func TestAPythonRuleChangesNoSeriesButByTruncate(t *testing.T) {
	requirePython(t)
	const script = `
metric(name="up", value=1, labels={"note": "a note of thirty-four bytes in all", "site": "rack1"})
metric(name="jobs", type="counter", value=3, help="From the script.", labels={"note": "a note of thirty-four bytes in all"})
`
	no := false
	run := func(rules ...model.MetricRule) string {
		c := workerCollector("rules", script)
		c.Limits.MaxLabelValueLength = 20
		c.Metrics = rules
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
		set, err := Transform(t.Context(), &decode.Decoded{Kind: "text", Data: "value=7", Raw: r.Body}, r, c, "python3")
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, m := range set.Metrics {
			lines = append(lines, fmt.Sprintf("%s %s %q %v %v", m.Name, m.Type, m.Help, m.Labels, m.Value))
		}
		return strings.Join(lines, "\n")
	}
	const whole = "up gauge \"\" map[note:a note of thirty-four bytes in all site:rack1] 1\njobs counter \"From the script.\" map[note:a note of thirty-four bytes in all] 3"
	for name, rules := range map[string][]model.MetricRule{
		"no rule": nil,
		"a rule of a type and a description": {
			{Name: "up", Type: model.CounterMetricType, Description: "From the rule."},
			{Name: "jobs", Type: model.GaugeMetricType, Description: "From the rule."},
		},
		"a rule with an expression, required and an error_mode": {{Name: "up", Expression: ".missing[", Required: &no, ErrorMode: model.ErrorModeFail}, {Name: "absent", ErrorMode: model.ErrorModeFail}},
		"a label with a value, which the load refuses":          {{Name: "up", Labels: []model.LabelRule{{Name: "site", Value: "x"}, {Name: "zone", Value: "a"}}}},
		"a label with an expression":                            {{Name: "up", Labels: []model.LabelRule{{Name: "site", Expression: ".other"}, {Name: "zone", Expression: "site"}}}},
		"a rule without a name":                                 {{Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}}}},
	} {
		if got := run(rules...); got != whole {
			t.Errorf("%s: the series are\n%s\nwant\n%s", name, got, whole)
		}
	}
	// truncate: true on a label of a rule that names the script's metric
	// cuts that label of that metric, with an expression, which is how the
	// load takes it, and no other label and no other metric.
	const cut = "up gauge \"\" map[note:a note of thirty-… site:rack1] 1\njobs counter \"From the script.\" map[note:a note of thirty-four bytes in all] 3"
	if got := run(model.MetricRule{Name: "up", Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}}}); got != cut {
		t.Errorf("truncate: the series are\n%s\nwant\n%s", got, cut)
	}
	if len("a note of thirty-…") != 20 {
		t.Fatal("the cut value is not the limit long")
	}
}
