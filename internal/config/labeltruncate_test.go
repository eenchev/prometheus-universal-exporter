//go:build !select_request_types || request_type_http

package config

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

func truncateCollector(truncate bool) model.Collector {
	return model.Collector{
		Name: "truncate", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "jq"},
		Limits: model.Limits{MaxLabelValueLength: 20},
		Metrics: []model.MetricRule{{Name: "status", Type: model.GaugeMetricType, Expression: "1", Labels: []model.LabelRule{
			{Name: "message", Expression: ".message", Truncate: truncate},
			{Name: "other", Expression: ".message"},
		}}},
	}
}

func runTruncate(t *testing.T, c model.Collector, body string) (*model.MetricSet, error) {
	t.Helper()
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"application/json"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform.Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		return nil, err
	}
	return set, set.Validate(c.Limits)
}

// A label with truncate: true is cut to the limit; one without still fails the
// scrape, as it always has, because a silently shortened value would surprise.
func TestTruncateOnlyAppliesToTheLabelThatAsksForIt(t *testing.T) {
	c := truncateCollector(true)
	c.Metrics[0].Labels = c.Metrics[0].Labels[:1]
	set, err := runTruncate(t, c, `{"message": "A fix is currently being rolled out"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Metrics[0].Labels["message"]; got != "A fix is currentl…" || len(got) > 20 {
		t.Fatalf("message=%q", got)
	}

	_, err = runTruncate(t, truncateCollector(true), `{"message": "A fix is currently being rolled out"}`)
	if err == nil || !strings.Contains(err.Error(), `metric "status" label "other" value is 35 bytes, longer than limits.max_label_value_length 20; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`) {
		t.Fatalf("err=%v", err)
	}
	// Short values are untouched.
	set, err = runTruncate(t, truncateCollector(true), `{"message": "fixed"}`)
	if err != nil || set.Metrics[0].Labels["message"] != "fixed" {
		t.Fatalf("set=%v err=%v", set, err)
	}
}

// Truncation happens before the prefix, so it finds the rule's metrics by the
// name they were declared with.
func TestTruncateWorksWithAMetricsPrefix(t *testing.T) {
	c := truncateCollector(true)
	c.Metrics[0].Labels = c.Metrics[0].Labels[:1]
	c.MetricsPrefix = "vendor"
	set, err := runTruncate(t, c, `{"message": "A fix is currently being rolled out"}`)
	if err != nil {
		t.Fatal(err)
	}
	if set.Metrics[0].Name != "vendor_status" || set.Metrics[0].Labels["message"] != "A fix is currentl…" {
		t.Fatalf("metric=%+v", set.Metrics[0])
	}
}

// It applies to every transform's declared labels, since it runs on the
// transform's output.
func TestTruncateWithRegex(t *testing.T) {
	c := model.Collector{
		Name: "truncate", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
		Limits: model.Limits{MaxLabelValueLength: 10},
		Metrics: []model.MetricRule{{Name: "status", Type: model.GaugeMetricType, Expression: `(?P<value>\d+) (?P<text>.*)`, Labels: []model.LabelRule{
			{Name: "text", Expression: "text", Truncate: true},
		}}},
	}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte("1 a rather long explanation"), Headers: http.Header{"Content-Type": {"text/plain"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform.Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Metrics[0].Labels["text"]; got != "a rathe…" {
		t.Fatalf("text=%q", got)
	}
}

// A python script's label is cut as any other when a rule of the collector
// declares the script's metric and the label with truncate: true: the rules
// of a python collector make no series, and name the script's. A label no
// rule declares still fails the scrape, with a message that holds for it:
// the label can be cut where a rule gives it.
func TestTruncateCutsAPythonScriptsLabelARuleDeclares(t *testing.T) {
	c := model.Collector{
		Name: "scripted", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "text"},
		Transform: model.TransformConfig{Type: "python", Script: "metric('up', 'gauge', 1, {'note': 'x' * 30, 'other': 'short'})\n"},
		Limits:    model.Limits{MaxLabelValueLength: 10},
		Metrics: []model.MetricRule{{Name: "up", Labels: []model.LabelRule{
			{Name: "note", Expression: "note", Truncate: true},
		}}},
	}
	set, err := runTruncate(t, c, "value=42\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Metrics[0].Labels; len(set.Metrics) != 1 || got["note"] != "xxxxxxx…" || got["other"] != "short" {
		t.Fatalf("the script's series are %+v, want note cut to xxxxxxx…", set.Metrics)
	}

	c.Metrics = nil
	_, err = runTruncate(t, c, "value=42\n")
	if want := `metric "up" label "note" value is 30 bytes, longer than limits.max_label_value_length 10; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`; err == nil || err.Error() != want {
		t.Fatalf("without the rule the scrape fails with\n%v\nwant\n%s", err, want)
	}
}

// The failure of a label value over the limit names the series as it is
// exposed, under the collector's metrics_prefix and the name rename_labels
// gave the label: the check is made of what the scrape answers.
func TestALabelOverTheLimitIsNamedAsItIsExposed(t *testing.T) {
	c := truncateCollector(false)
	c.MetricsPrefix = "app"
	c.Metrics[0].Labels = c.Metrics[0].Labels[1:]
	c.Transform.RenameLabels = map[string]string{"other": "detail"}
	_, err := runTruncate(t, c, `{"message": "A fix is currently being rolled out"}`)
	if want := `metric "app_status" label "detail" value is 35 bytes, longer than limits.max_label_value_length 20; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`; err == nil || err.Error() != want {
		t.Fatalf("the scrape fails with\n%v\nwant\n%s", err, want)
	}
}
