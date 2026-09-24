package transform

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A label marked required that a series has no value for fails that series
// under the rule's error_mode; an optional one is left off the series.

// labelCase is one transform's collector, a body in which the second series
// has no value for the label "who", and the content type to serve it with.
type labelCase struct {
	name        string
	collector   func(label model.LabelRule) model.Collector
	body        string
	contentType string
	// who is the first series\' label, "a" when unset.
	who string
}

func (c labelCase) first() string {
	if c.who == "" {
		return "a"
	}
	return c.who
}

func labelCases() []labelCase {
	rule := func(expression string, label model.LabelRule) []model.MetricRule {
		return []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: expression, Labels: []model.LabelRule{label}}}
	}
	collector := func(transform, format, expression string, label model.LabelRule) model.Collector {
		c := model.Collector{Name: "labels", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Response: model.ResponseConfig{Format: format}, Transform: model.TransformConfig{Type: transform}, Metrics: rule(expression, label)}
		if format == "csv" {
			header := true
			c.Response.CSV.Header = &header
		}
		return c
	}
	return []labelCase{
		{
			name: "jq items",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = ".who"
				c := collector("jq", "json", ".v", label)
				c.Metrics[0].Items = ".[]"
				return c
			},
			body: `[{"v":1,"who":"a"},{"v":2}]`, contentType: "application/json",
		},
		{
			name: "jq by position",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = ".[].who"
				return collector("jq", "json", ".[].v", label)
			},
			body: `[{"v":1,"who":"a"},{"v":2,"who":null}]`, contentType: "application/json",
		},
		{
			name: "regex",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "who"
				return collector("regex", "text", `v=(\d+)(?: (?P<who>\S+))?`, label)
			},
			body: "v=1 a\nv=2\n", contentType: "text/plain",
		},
		{
			name: "XPath attribute",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "@who"
				return collector("xpath", "xml", "/r/v", label)
			},
			body: `<r><v who="a">1</v><v>2</v></r>`, contentType: "application/xml",
		},
		{
			name: "XPath path",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "../w"
				return collector("xpath", "xml", "/r/i/v", label)
			},
			body: `<r><i><v>1</v><w>a</w></i><i><v>2</v></i></r>`, contentType: "application/xml",
		},
		{
			name: "CSS",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "b"
				return collector("css", "html", "td", label)
			},
			// A label selector runs inside the value's node, whose text
			// includes it, so the label here is the value's own markup.
			body: `<table><tr><td><b>1</b></td></tr><tr><td>2</td></tr></table>`, contentType: "text/html", who: "1",
		},
		{
			name: "CSS items",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "td.who"
				c := collector("css", "html", "td.v", label)
				c.Metrics[0].Items = "tr"
				return c
			},
			body: `<table><tr><td class="v">1</td><td class="who">a</td></tr><tr><td class="v">2</td></tr></table>`, contentType: "text/html",
		},
		{
			name: "CSV",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "who"
				return collector("csv", "csv", "v", label)
			},
			body: "v,who\n1,a\n2,\n", contentType: "text/csv",
		},
		{
			name: "prometheus",
			collector: func(label model.LabelRule) model.Collector {
				label.Expression = "instance"
				return collector("prometheus", "prometheus", "^v$", label)
			},
			body: "# TYPE v gauge\nv{instance=\"a\"} 1\nv{job=\"x\"} 2\n", contentType: "text/plain; version=0.0.4",
		},
	}
}

func runLabelCase(t *testing.T, test labelCase, label model.LabelRule, mode string) (*model.MetricSet, error) {
	t.Helper()
	c := test.collector(label)
	c.Metrics[0].ErrorMode = mode
	r := &fetch.HTTPResponse{Body: []byte(test.body), Headers: http.Header{"Content-Type": {test.contentType}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return Transform(context.Background(), d, r, &c, "python3")
}

// whoValues lists each series' who label, "-" where it has none.
func whoValues(set *model.MetricSet) string {
	var values []string
	for _, m := range set.Metrics {
		value, ok := m.Labels["who"]
		if !ok {
			value = "-"
		}
		values = append(values, value)
	}
	return strings.Join(values, ",")
}

func TestAnOptionalLabelWithoutAValueIsLeftOff(t *testing.T) {
	for _, test := range labelCases() {
		t.Run(test.name, func(t *testing.T) {
			set, err := runLabelCase(t, test, model.LabelRule{Name: "who", Type: "expression"}, model.ErrorModeFail)
			if err != nil {
				t.Fatal(err)
			}
			// Left off, not exported empty: over OTLP the two differ.
			if got, want := whoValues(set), test.first()+",-"; got != want {
				t.Fatalf("who labels %s, want %s", got, want)
			}
		})
	}
}

func TestARequiredLabelWithoutAValueFailsItsSeries(t *testing.T) {
	required := model.LabelRule{Name: "who", Type: "expression", Required: true}
	for _, test := range labelCases() {
		t.Run(test.name, func(t *testing.T) {
			// log drops the series without the label, keeps the other,
			// and says which label was missing.
			logs := testutil.CaptureLogs(t)
			set, err := runLabelCase(t, test, required, model.ErrorModeLog)
			if err != nil {
				t.Fatal(err)
			}
			if got := whoValues(set); got != test.first() {
				t.Fatalf("log: who labels %s, want %s", got, test.first())
			}
			if !strings.Contains(logs.String(), `label \"who\" is missing`) {
				t.Fatalf("log: the missing label was not logged:\n%s", logs)
			}

			// ignore drops it quietly.
			logs.Reset()
			set, err = runLabelCase(t, test, required, model.ErrorModeIgnore)
			if err != nil || whoValues(set) != test.first() || logs.Len() != 0 {
				t.Fatalf("ignore: err=%v who=%s logs=%s", err, whoValues(set), logs)
			}

			// fail fails the rule, as a missing value.
			_, err = runLabelCase(t, test, required, model.ErrorModeFail)
			var failure *MetricFailure
			if !errors.As(err, &failure) || !errors.Is(err, model.ErrMissingValue) || !strings.Contains(err.Error(), `label "who" is missing`) {
				t.Fatalf("fail: err=%v", err)
			}
		})
	}
}

// Without items, jq pairs label values with series by position, so a required
// label giving a different number of values than there are series fails the
// rule rather than land on the wrong series.
func TestARequiredPositionalLabelMustPairWithTheSeries(t *testing.T) {
	c := model.Collector{Name: "pairs", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Response: model.ResponseConfig{Format: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{
		Name: "v", Type: model.GaugeMetricType, Expression: ".[].v", ErrorMode: model.ErrorModeFail,
		Labels: []model.LabelRule{{Name: "who", Type: "expression", Expression: ".[].who | select(. != null)", Required: true}},
	}}}
	r := &fetch.HTTPResponse{Body: []byte(`[{"v":1},{"v":2,"who":"b"},{"v":3,"who":"c"}]`), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Transform(context.Background(), d, r, &c, "python3"); err == nil || !strings.Contains(err.Error(), `label "who" gave 2 values for 3 series`) {
		t.Fatalf("err=%v", err)
	}
	// One value applies to every series, as for an optional label.
	c.Metrics[0].Labels[0].Expression = `"all"`
	set, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil || whoValues(set) != "all,all,all" {
		t.Fatalf("err=%v who=%s", err, whoValues(set))
	}
}

// Two rules matching one source metric each label their own copy of it.
func TestPassthroughRulesDoNotShareLabels(t *testing.T) {
	c := model.Collector{Name: "copies", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{
		{Name: "first", Expression: "^v$", Labels: []model.LabelRule{{Name: "copy", Type: "string", Value: "first"}}},
		{Name: "second", Expression: "^v$"},
	}}
	r := &fetch.HTTPResponse{Body: []byte("# TYPE v gauge\nv{instance=\"a\"} 1\n"), Headers: http.Header{"Content-Type": {"text/plain; version=0.0.4"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range set.Metrics {
		if m.Name == "second" && m.Labels["copy"] != "" {
			t.Fatalf("the second rule's series carries the first rule's label: %v", m.Labels)
		}
	}
}
