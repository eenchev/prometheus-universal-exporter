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
)

// A value that is not there and a value that is there but blank are the same
// to every transform: a missing value, handled by required and error_mode,
// not text that failed to read as a number (blankValue).

type blankCase struct {
	name, transform, decoder, contentType, expression, body string
}

var blankCases = []blankCase{
	{"jq empty string", "jq", "json", "application/json", ".v", `{"v": ""}`},
	{"jq whitespace", "jq", "json", "application/json", ".v", `{"v": "  "}`},
	{"jq null", "jq", "json", "application/json", ".v", `{"v": null}`},
	{"xpath empty element", "xpath", "xml", "application/xml", "//v", `<r><v/></r>`},
	{"xpath whitespace element", "xpath", "xml", "application/xml", "//v", `<r><v>  </v></r>`},
	{"css empty element", "css", "html", "text/html", "#v", `<p id="v"> </p>`},
	{"regex optional group that took no part", "regex", "text", "text/plain", `v=(\d+)?;`, "v=;"},
	{"regex group that captured nothing", "regex", "text", "text/plain", `v=(\d*);`, "v=;"},
	{"csv blank cell", "csv", "csv", "text/csv", "v", "name,v\na, \n"},
}

func runBlankCase(t *testing.T, tc blankCase, required *bool, errorMode string) (*model.MetricSet, error) {
	t.Helper()
	c := model.Collector{
		Name: "blank", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform},
		Metrics: []model.MetricRule{{Name: "value", Type: model.GaugeMetricType, Expression: tc.expression, Required: required, ErrorMode: errorMode}},
	}
	r := &fetch.HTTPResponse{Body: []byte(tc.body), Headers: http.Header{"Content-Type": {tc.contentType}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return Transform(context.Background(), d, r, &c, "python3")
}

func TestABlankValueIsAMissingValue(t *testing.T) {
	optional := false
	for _, tc := range blankCases {
		t.Run(tc.name, func(t *testing.T) {
			// Required, under fail: the rule fails with a missing value.
			_, err := runBlankCase(t, tc, nil, model.ErrorModeFail)
			if !errors.Is(err, model.ErrMissingValue) {
				t.Fatalf("a blank value failed with %v, want a missing value", err)
			}
			// Optional: the series is left out, and nothing fails.
			set, err := runBlankCase(t, tc, &optional, model.ErrorModeFail)
			if err != nil {
				t.Fatalf("an optional blank value failed the rule: %v", err)
			}
			if len(set.Metrics) != 0 {
				t.Fatalf("a blank value was exported: %#v", set.Metrics)
			}
		})
	}
}

// Text that is there and is not a number is still a failure to read it, which
// required: false does not excuse.
func TestTextThatIsNotANumberIsNotAMissingValue(t *testing.T) {
	optional := false
	_, err := runBlankCase(t, blankCase{"", "jq", "json", "application/json", ".v", `{"v": "up"}`}, &optional, model.ErrorModeFail)
	if err == nil || errors.Is(err, model.ErrMissingValue) {
		t.Fatalf("got %v, want a conversion failure", err)
	}
}

// An optional regex capture group that took no part in the match reads as a
// missing value, not out of the text's bounds.
func TestRegexOptionalGroupKeepsTheOtherMatches(t *testing.T) {
	tc := blankCase{"", "regex", "text", "text/plain", `v=(\d+)?;`, "v=1;v=;v=3;"}
	set, err := runBlankCase(t, tc, nil, model.ErrorModeLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Value != 1 || set.Metrics[1].Value != 3 {
		t.Fatalf("got %#v, want the two matches with a value", set.Metrics)
	}
}

// The first capture group is the value, so a regex without one is refused
// when the configuration loads rather than read some other way.
func TestARegexRuleWithoutACaptureGroupIsRefused(t *testing.T) {
	c := &model.Collector{Name: "text", Transform: model.TransformConfig{Type: "regex"}}
	err := CheckMetricRule(c, &model.MetricRule{Name: "requests", Expression: `requests=\d+`})
	if err == nil || !strings.Contains(err.Error(), "no capture group") {
		t.Fatalf("got %v, want a missing capture group refused", err)
	}
	if err := CheckMetricRule(c, &model.MetricRule{Name: "requests", Expression: `requests=(\d+)`}); err != nil {
		t.Fatal(err)
	}
}

// Without items a css rule is one series. Several matching elements would be
// several series with the same labels, so the rule fails, pointing at items,
// instead of the scrape failing later on duplicate series.
func TestACSSRuleWithoutItemsMatchesOneElement(t *testing.T) {
	c := model.Collector{
		Name: "rows", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "css"},
		Metrics: []model.MetricRule{{Name: "server_cpu", Type: model.GaugeMetricType, Expression: "td", ErrorMode: model.ErrorModeFail}},
	}
	_, err := runCSS(t, c, `<table><tr><td>72</td></tr><tr><td>up</td></tr><tr><td>31</td></tr></table>`)
	if err == nil || !strings.Contains(err.Error(), "matched 3 elements") || !strings.Contains(err.Error(), "set items") {
		t.Fatalf("got %v, want several matches refused with a pointer to items", err)
	}
	// Under log, the rule leaves nothing behind: not the elements before the
	// one that failed, nor those after.
	c.Metrics[0].ErrorMode = model.ErrorModeLog
	set, err := runCSS(t, c, `<table><tr><td>72</td></tr><tr><td>up</td></tr><tr><td>31</td></tr></table>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 0 {
		t.Fatalf("a rule that failed exported %#v", set.Metrics)
	}
}
