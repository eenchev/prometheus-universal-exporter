package transform

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The text of a label value a jq or yq expression gives (labeltext.go).

func TestLabelTextWritesValuesAsTheyRead(t *testing.T) {
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	for _, tc := range []struct {
		value any
		want  string
	}{
		{"web-01", "web-01"},
		{"", ""},
		{true, "true"},
		{false, "false"},
		{float64(8080), "8080"},
		{float64(1234567), "1234567"},
		{float64(9007199254740993), "9007199254740992"}, // as JSON decoding rounds it
		{float64(1e20), "100000000000000000000"},
		{float64(1e21), "1e+21"},
		{float64(-42), "-42"},
		{0.25, "0.25"},
		{1234567.5, "1234567.5"},
		{0.000001, "0.000001"},
		{0.0000001, "1e-07"},
		{math.Copysign(0, -1), "0"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
		{float32(2.5), "2.5"},
		{7, "7"},
		{int64(-7), "-7"},
		{uint64(18446744073709551615), "18446744073709551615"},
		{json.Number("12345678901234567890"), "12345678901234567890"},
		{huge, "123456789012345678901234567890"},
	} {
		got, err := labelText(tc.value)
		if err != nil || got != tc.want {
			t.Errorf("labelText(%#v) = %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
}

// A label is one value: an object or an array is refused, saying what to do.
func TestLabelTextRefusesObjectsAndArrays(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{map[string]any{"a": 1.0}, "is an object, not a single value; select one of its fields"},
		{[]any{"x", "y"}, `is an array of 2 values, not a single value; select one, or join them with join(",")`},
	} {
		if _, err := labelText(tc.value); err == nil || err.Error() != tc.want {
			t.Errorf("labelText(%#v): got %v, want %q", tc.value, err, tc.want)
		}
	}
}

func runJQLabels(t *testing.T, body string, rule model.MetricRule) (*model.MetricSet, error) {
	t.Helper()
	c := model.Collector{
		Name: "ids", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{rule},
	}
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"application/json"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return Transform(context.Background(), d, r, &c, "python3")
}

// Numbers read from a response keep the form they were written in, with or
// without items, where %v made an ID of a million or more an exponent.
func TestJQNumberLabelsReadAsWritten(t *testing.T) {
	const body = `{"account": 1234567, "items": [{"id": 20000000, "ratio": 0.5, "up": 1}, {"id": 8080, "ratio": 1.25, "up": 1}]}`
	set, err := runJQLabels(t, body, model.MetricRule{
		Name: "up", Type: model.GaugeMetricType, Expression: ".items[].up",
		Labels: []model.LabelRule{{Name: "account", Expression: ".account"}, {Name: "id", Expression: ".items[].id"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["account"] != "1234567" || set.Metrics[0].Labels["id"] != "20000000" || set.Metrics[1].Labels["id"] != "8080" {
		t.Fatalf("labels: %+v", set.Metrics)
	}

	set, err = runJQLabels(t, body, model.MetricRule{
		Name: "up", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".up",
		Labels: []model.LabelRule{{Name: "id", Expression: ".id"}, {Name: "ratio", Expression: ".ratio"}, {Name: "account", Expression: "$root.account"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["id"] != "20000000" || set.Metrics[0].Labels["ratio"] != "0.5" || set.Metrics[0].Labels["account"] != "1234567" || set.Metrics[1].Labels["ratio"] != "1.25" {
		t.Fatalf("labels with items: %+v", set.Metrics)
	}
}

// An object or an array as a label is a failure of the rule, handled by its
// error_mode, instead of Go syntax exported as the label.
func TestJQObjectOrArrayLabelsFailTheRule(t *testing.T) {
	const body = `{"up": 1, "owner": {"team": "a"}, "tags": ["x", "y"], "items": [{"up": 1, "tags": ["x"]}]}`
	for name, rule := range map[string]model.MetricRule{
		"object":        {Labels: []model.LabelRule{{Name: "owner", Expression: ".owner"}}, Expression: ".up"},
		"array":         {Labels: []model.LabelRule{{Name: "tags", Expression: ".tags"}}, Expression: ".up"},
		"array in item": {Items: ".items[]", Labels: []model.LabelRule{{Name: "tags", Expression: ".tags"}}, Expression: ".up"},
	} {
		t.Run(name, func(t *testing.T) {
			rule.Name, rule.Type, rule.ErrorMode = "up", model.GaugeMetricType, model.ErrorModeFail
			_, err := runJQLabels(t, body, rule)
			if err == nil || !strings.Contains(err.Error(), "not a single value") {
				t.Fatalf("got %v, want the label refused as not a single value", err)
			}
			var failure *MetricFailure
			if !errors.As(err, &failure) {
				t.Fatalf("the error is not the rule's failure: %v", err)
			}

			rule.ErrorMode = model.ErrorModeLog
			set, err := runJQLabels(t, body, rule)
			if err != nil {
				t.Fatalf("under log the scrape failed: %v", err)
			}
			if len(set.Metrics) != 0 {
				t.Fatalf("a series with a malformed label was exported: %+v", set.Metrics)
			}
		})
	}

	// join makes one value of an array.
	set, err := runJQLabels(t, body, model.MetricRule{Name: "up", Type: model.GaugeMetricType, Expression: ".up", Labels: []model.LabelRule{{Name: "tags", Expression: `.tags | join(",")`}}})
	if err != nil || set.Metrics[0].Labels["tags"] != "x,y" {
		t.Fatalf("join: %+v %v", set, err)
	}
}
