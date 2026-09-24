package transform

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// With items, a CSS rule selects the rows a metric is about, and its value
// and labels are cells of the same row (transformCSSItems).

func cssItemsCollector(items, expression string, labels ...model.LabelRule) model.Collector {
	return model.Collector{
		Name: "rows", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Response: model.ResponseConfig{Format: "html"}, Transform: model.TransformConfig{Type: "css"},
		Metrics: []model.MetricRule{{Name: "server_cpu", Type: model.GaugeMetricType, Items: items, Expression: expression, Labels: labels}},
	}
}

func runCSS(t *testing.T, c model.Collector, body string) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"text/html"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return Transform(context.Background(), d, r, &c, "python3")
}

// series renders a set as name{server} value lines, in order.
func series(set *model.MetricSet) string {
	var lines []string
	for _, m := range set.Metrics {
		lines = append(lines, m.Name+"{"+m.Labels["server"]+"} "+strconv.FormatFloat(m.Value, 'g', -1, 64))
	}
	return strings.Join(lines, "\n")
}

// A table's rows: the value from one cell, the label from another. The header
// row has no td cells, and tr:has(td) leaves it out.
func TestCSSItemsReadATable(t *testing.T) {
	body, err := os.ReadFile("../../testdata/html/status.html")
	if err != nil {
		t.Fatal(err)
	}
	c := cssItemsCollector("#servers tr:has(td)", "td:nth-child(2)", model.LabelRule{Name: "server", Expression: "td:nth-child(1)", Required: true})
	set, err := runCSS(t, c, string(body))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := series(set), "server_cpu{web01} 72\nserver_cpu{web02} 31"; got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A row whose value cell is missing is that row's missing metric: log drops
// the row and keeps the rest, fail fails the rule, and an optional metric
// skips the row quietly.
func TestCSSItemsAMissingValueIsThatRowsMissingMetric(t *testing.T) {
	const body = `<table><tr><td class="s">web01</td><td class="v">72</td></tr><tr><td class="s">web02</td></tr><tr><td class="s">web03</td><td class="v">5</td></tr></table>`
	label := model.LabelRule{Name: "server", Expression: "td.s"}

	logs := testutil.CaptureLogs(t)
	c := cssItemsCollector("tr", "td.v", label)
	c.Metrics[0].ErrorMode = model.ErrorModeLog
	set, err := runCSS(t, c, body)
	if err != nil || series(set) != "server_cpu{web01} 72\nserver_cpu{web03} 5" || !strings.Contains(logs.String(), "value is missing for item 1") {
		t.Fatalf("log: err=%v series:\n%s\nlogs:\n%s", err, series(set), logs)
	}

	c.Metrics[0].ErrorMode = model.ErrorModeFail
	if _, err := runCSS(t, c, body); !errors.Is(err, model.ErrMissingValue) {
		t.Fatalf("fail: err=%v", err)
	}

	optional := false
	c.Metrics[0].Required = &optional
	logs.Reset()
	set, err = runCSS(t, c, body)
	if err != nil || series(set) != "server_cpu{web01} 72\nserver_cpu{web03} 5" || logs.Len() != 0 {
		t.Fatalf("optional: err=%v series:\n%s\nlogs:\n%s", err, series(set), logs)
	}
}

// Within a row, a selector matching two cells is an error: there is no
// telling which belongs to the series.
func TestCSSItemsSelectorsMatchAtMostOneElementPerRow(t *testing.T) {
	const body = `<table><tr><td>web01</td><td>72</td></tr></table>`
	c := cssItemsCollector("tr", "td", model.LabelRule{Name: "server", Expression: "td:nth-child(1)"})
	c.Metrics[0].ErrorMode = model.ErrorModeFail
	if _, err := runCSS(t, c, body); err == nil || !strings.Contains(err.Error(), `CSS selector "td" matched 2 elements`) {
		t.Fatalf("value: err=%v", err)
	}
	c = cssItemsCollector("tr", "td:nth-child(2)", model.LabelRule{Name: "server", Expression: "td"})
	c.Metrics[0].ErrorMode = model.ErrorModeFail
	if _, err := runCSS(t, c, body); err == nil || !strings.Contains(err.Error(), `CSS selector "td" matched 2 elements`) {
		t.Fatalf("label: err=%v", err)
	}
}

// items selecting nothing is a missing metric when the metric is required.
func TestCSSItemsSelectingNothing(t *testing.T) {
	c := cssItemsCollector("#absent tr", "td")
	c.Metrics[0].ErrorMode = model.ErrorModeFail
	if _, err := runCSS(t, c, `<table><tr><td>1</td></tr></table>`); !errors.Is(err, model.ErrMissingValue) || !strings.Contains(err.Error(), "items CSS selector") {
		t.Fatalf("err=%v", err)
	}
	optional := false
	c.Metrics[0].Required = &optional
	if set, err := runCSS(t, c, `<table><tr><td>1</td></tr></table>`); err != nil || len(set.Metrics) != 0 {
		t.Fatalf("optional: err=%v set=%v", err, set)
	}
}
