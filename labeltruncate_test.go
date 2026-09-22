package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateLabelValue(t *testing.T) {
	tests := []struct {
		value string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"abcdefghijk", 10, "abcdefg…"},    // 7 bytes and the 3-byte mark
		{"ünïcödé-ünïcödé", 12, "ünïcöd…"}, // never splits a character
		{"abcdef", 2, "ab"},                // no room for the mark
		{"日本語のテキスト", 10, "日本…"},            // 3-byte characters
	}
	for _, test := range tests {
		got := truncateLabelValue(test.value, test.limit)
		if got != test.want || len(got) > test.limit || !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q (%d bytes), want %q", test.value, test.limit, got, len(got), test.want)
		}
	}
}

func truncateCollector(truncate bool) Collector {
	return Collector{
		Name: "truncate", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: "jq"},
		Limits: Limits{MaxLabelValueLength: 20},
		Metrics: []MetricRule{{Name: "status", Type: GaugeMetricType, Expression: "1", Labels: []LabelRule{
			{Name: "message", Type: "expression", Expression: ".message", Truncate: truncate},
			{Name: "other", Type: "expression", Expression: ".message"},
		}}},
	}
}

func runTruncate(t *testing.T, c Collector, body string) (*MetricSet, error) {
	t.Helper()
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"application/json"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform(context.Background(), d, r, &c, "python3")
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
	if err == nil || !strings.Contains(err.Error(), `label "other" is too long`) {
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
	c := Collector{
		Name: "truncate", Request: RequestConfig{Type: RequestTypeHTTP}, Response: ResponseConfig{Format: "text"}, Transform: TransformConfig{Type: "regex"},
		Limits: Limits{MaxLabelValueLength: 10},
		Metrics: []MetricRule{{Name: "status", Type: GaugeMetricType, Expression: `(?P<value>\d+) (?P<text>.*)`, Labels: []LabelRule{
			{Name: "text", Type: "expression", Expression: "text", Truncate: true},
		}}},
	}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte("1 a rather long explanation"), Headers: http.Header{"Content-Type": {"text/plain"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Metrics[0].Labels["text"]; got != "a rathe…" {
		t.Fatalf("text=%q", got)
	}
}
