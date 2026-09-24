package config

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// items evaluates a jq or yq metric one item at a time: the value and each
// label see the item as ".", and the whole document as $root.

func itemsSet(t *testing.T, transformType, contentType, body string, rules ...model.MetricRule) (*model.MetricSet, error) {
	t.Helper()
	c := model.Collector{Name: "items", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: transformType}, Metrics: rules, Limits: model.Limits{MaxMetrics: 100}}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {contentType}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return transform.Transform(context.Background(), d, r, &c, "python3")
}

func exprLabel(name, expression string) model.LabelRule {
	return model.LabelRule{Name: name, Type: "expression", Expression: expression}
}

const itemsDocument = `{
  "site": "eu-1",
  "groups": [{"id": "g1", "name": "Frontend"}],
  "servers": [
    {"name": "web01", "cpu": 72, "group": "g1"},
    {"name": "web02", "cpu": 18},
    {"name": "db01", "cpu": 41, "group": "g1"}
  ]
}`

func TestItemsEvaluatesEachItemOnItsOwn(t *testing.T) {
	set, err := itemsSet(t, "jq", "application/json", itemsDocument, model.MetricRule{
		Name: "server_cpu", Type: model.GaugeMetricType, Items: ".servers[]", Expression: ".cpu",
		Labels: []model.LabelRule{
			exprLabel("server", ".name"),
			// web02 has no group. With parallel streams this label would have
			// two values for three series and land on the wrong ones; per item
			// it is simply absent on web02.
			exprLabel("group", `.group as $id | first($root.groups[] | select(.id == $id)) | .name`),
			exprLabel("site", "$root.site"),
			{Name: "source", Type: "string", Value: "demo"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"server_cpu group=Frontend server=web01 site=eu-1 source=demo 72",
		"server_cpu server=web02 site=eu-1 source=demo 18",
		"server_cpu group=Frontend server=db01 site=eu-1 source=demo 41",
	}
	if got := describeSet(set); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// yq speaks the same language.
func TestItemsWithYQ(t *testing.T) {
	set, err := itemsSet(t, "yq", "application/yaml", "servers:\n  - name: web01\n    cpu: 72\n  - name: web02\n    cpu: 18\n", model.MetricRule{
		Name: "server_cpu", Type: model.GaugeMetricType, Items: ".servers[]", Expression: ".cpu", Labels: []model.LabelRule{exprLabel("server", ".name")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := describeSet(set); len(got) != 2 || got[1] != "server_cpu server=web02 18" {
		t.Fatalf("got %v", got)
	}
}

// A value missing for one item is that item's missing metric: error_mode log
// drops the one series and keeps the rest, and fail fails the scrape.
func TestItemsMissingValue(t *testing.T) {
	testutil.CaptureLogs(t)
	rule := model.MetricRule{Name: "server_cpu", Type: model.GaugeMetricType, Items: ".servers[]", Expression: ".load", Labels: []model.LabelRule{exprLabel("server", ".name")}}
	body := `{"servers": [{"name": "web01", "load": 3}, {"name": "web02"}, {"name": "web03", "load": null}]}`

	rule.ErrorMode = model.ErrorModeLog
	set, err := itemsSet(t, "jq", "application/json", body, rule)
	if err != nil {
		t.Fatal(err)
	}
	if got := describeSet(set); len(got) != 1 || got[0] != "server_cpu server=web01 3" {
		t.Fatalf("got %v", got)
	}

	rule.ErrorMode = model.ErrorModeFail
	_, err = itemsSet(t, "jq", "application/json", body, rule)
	var failure *transform.MetricFailure
	if !errors.As(err, &failure) || !strings.Contains(err.Error(), `metric "server_cpu" value is missing for item 1`) {
		t.Fatalf("err=%v", err)
	}

	// Not required: the missing ones are skipped without complaint.
	notRequired := false
	rule.ErrorMode, rule.Required = model.ErrorModeFail, &notRequired
	set, err = itemsSet(t, "jq", "application/json", body, rule)
	if err != nil || len(set.Metrics) != 1 {
		t.Fatalf("set=%v err=%v", set, err)
	}
}

// An expression must give one value per item; two is ambiguous and an error.
func TestItemsExpressionWithSeveralValues(t *testing.T) {
	testutil.CaptureLogs(t)
	_, err := itemsSet(t, "jq", "application/json", `{"servers": [{"name": "web01", "cpu": [1, 2]}]}`, model.MetricRule{
		Name: "server_cpu", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeFail, Items: ".servers[]", Expression: ".cpu[]",
	})
	if err == nil || !strings.Contains(err.Error(), `expression ".cpu[]" produced 2 values for one item; it must produce at most one`) {
		t.Fatalf("err=%v", err)
	}
	_, err = itemsSet(t, "jq", "application/json", `{"servers": [{"name": ["a", "b"], "cpu": 1}]}`, model.MetricRule{
		Name: "server_cpu", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeFail, Items: ".servers[]", Expression: ".cpu",
		Labels: []model.LabelRule{exprLabel("server", ".name[]")},
	})
	if err == nil || !strings.Contains(err.Error(), `label "server"`) || !strings.Contains(err.Error(), "produced 2 values") {
		t.Fatalf("err=%v", err)
	}
}

// No items at all is a missing metric when required, and nothing otherwise.
func TestItemsSelectingNothing(t *testing.T) {
	testutil.CaptureLogs(t)
	rule := model.MetricRule{Name: "server_cpu", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeFail, Items: ".servers[]", Expression: ".cpu"}
	if _, err := itemsSet(t, "jq", "application/json", `{"servers": []}`, rule); err == nil || !strings.Contains(err.Error(), "items selected nothing") {
		t.Fatalf("err=%v", err)
	}
	notRequired := false
	rule.Required = &notRequired
	set, err := itemsSet(t, "jq", "application/json", `{"servers": []}`, rule)
	if err != nil || len(set.Metrics) != 0 {
		t.Fatalf("set=%v err=%v", set, err)
	}
}

// Without items, expressions still see the whole document, and $root is the
// same document, so an expression written for items also works without.
func TestRootIsTheDocumentWithoutItems(t *testing.T) {
	set, err := itemsSet(t, "jq", "application/json", itemsDocument, model.MetricRule{
		Name: "servers", Type: model.GaugeMetricType, Expression: "$root.servers | length", Labels: []model.LabelRule{exprLabel("site", ".site")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := describeSet(set); len(got) != 1 || got[0] != "servers site=eu-1 3" {
		t.Fatalf("got %v", got)
	}
}

// describeSet renders a set as "name label=value ... value" lines, labels
// sorted, in order.
func describeSet(set *model.MetricSet) []string {
	var out []string

	for _, m := range set.Metrics {
		var line strings.Builder

		line.WriteString(m.Name)

		keys := make([]string, 0, len(m.Labels))
		for k := range m.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			line.WriteByte(' ')
			line.WriteString(k)
			line.WriteByte('=')
			line.WriteString(m.Labels[k])
		}

		line.WriteByte(' ')
		line.WriteString(strconv.FormatFloat(m.Value, 'g', -1, 64))

		out = append(out, line.String())
	}

	return out
}
