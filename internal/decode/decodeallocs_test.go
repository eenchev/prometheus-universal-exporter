package decode

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The json decoder and the parser of Prometheus text are written to
// allocate little for each item and each series they read (jsonvalue.go,
// promparse.go), which is most of what they cost. These tests bound the
// allocations of a decode of a fixed body, so that a change that brings back
// an allocation for every key, every label or every line fails here rather
// than being found in a profile. Each bound lies well above what the decoder
// needs and well below what it needed before, both of which are said.

// allocationsFor is the allocations of one decode of body, by collector c,
// for each of n things in it, as alloctest counts them.
func allocationsFor(t *testing.T, c model.Collector, contentType, body string, n int) float64 {
	t.Helper()
	headers := http.Header{"Content-Type": {contentType}}
	c.Request = model.RequestConfig{Type: fetch.RequestTypeHTTP}
	raw := []byte(body)
	var err error
	allocations, _ := alloctest.Allocations(5, func() {
		_, err = Decode(&fetch.HTTPResponse{Body: raw, Headers: headers}, &c)
	})
	if err != nil {
		t.Fatal(err)
	}
	return allocations / float64(n)
}

// An object of five members and a nested object, as the items of an API's
// answer are, costs the json decoder ten allocations: two for each of its
// two maps, one for the nested array, one for each number above 255 and two
// for the one string that no other item has; the keys, and the strings the
// items share, are made once for the document. It cost 41 when every key,
// every number's text and every value was allocated for itself, through
// reflection, and the numbers a second time afterwards.
func TestJSONDecoderAllocatesLittleForEachItem(t *testing.T) {
	const items = 1000
	var body strings.Builder
	body.WriteString(`{"items":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"item-%d","region":"eu-%d","kind":"k%d","value":%d,"size":%d,"extra":{"a":1,"b":"two","c":[1,2,3]}}`, i, i%7, i%3, i, i*1024)
	}
	body.WriteString("]}")
	c := model.Collector{Decoder: model.DecoderConfig{Type: "json"}}
	if each := allocationsFor(t, c, "application/json", body.String(), items); each > 16 {
		t.Fatalf("%.1f allocations for each item, want at most 16", each)
	}
}

// A series with two labels costs the parser three allocations, two for the
// map of its labels and one for a label value the series before it did not
// have: the line is read where it lies, and its labels are made a map once.
// It cost eight, with the line copied to a string, the labels collected in a
// slice that grew, a set made to find a name written twice, and each value
// built a byte at a time.
func TestPromParserAllocatesLittleForEachSeries(t *testing.T) {
	const series = 1000
	var body strings.Builder
	body.WriteString("# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.\n# TYPE node_cpu_seconds_total counter\n")
	for i := 0; i < series; i++ {
		fmt.Fprintf(&body, "node_cpu_seconds_total{cpu=\"%d\",mode=\"m%d\"} %d.25\n", i/8, i%8, i*3)
	}
	c := model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}}
	if each := allocationsFor(t, c, "text/plain; version=0.0.4", body.String(), series); each > 5 {
		t.Fatalf("%.1f allocations for each series, want at most 5", each)
	}
}

// A histogram's bucket, _sum or _count line costs the parser nothing once
// its series is there, and a series of twelve lines six allocations, half an
// allocation a line: the series is found by a signature written over the
// same buffer for every line, and its buckets have room for as many as the
// series before it had. A line cost nine allocations when each made a map
// of its labels and a string of its signature only to find its series.
//
// A line of a family the collector's rules do not keep costs nothing at
// all, where it cost seven: it is checked, and nothing is made of it.
func TestPromParserAllocatesLittleForEachHistogramLine(t *testing.T) {
	const histograms = 100
	var body strings.Builder
	body.WriteString("# HELP http_request_duration_seconds Request latency.\n# TYPE http_request_duration_seconds histogram\n")
	bounds := []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "+Inf"}
	for i := 0; i < histograms; i++ {
		for b, le := range bounds {
			fmt.Fprintf(&body, "http_request_duration_seconds_bucket{handler=\"/h%d\",le=\"%s\"} %d\n", i, le, (b+1)*10)
		}
		fmt.Fprintf(&body, "http_request_duration_seconds_sum{handler=\"/h%d\"} 12.5\nhttp_request_duration_seconds_count{handler=\"/h%d\"} 100\n", i, i)
	}
	lines := histograms * (len(bounds) + 2)
	c := model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}}
	if each := allocationsFor(t, c, "text/plain; version=0.0.4", body.String(), lines); each > 3 {
		t.Errorf("%.1f allocations for each line of a histogram, want at most 3", each)
	}
	c.Transform = model.TransformConfig{Type: "prometheus", Include: []string{"^another_family$"}}
	if each := allocationsFor(t, c, "text/plain; version=0.0.4", body.String(), lines); each > 1 {
		t.Fatalf("%.1f allocations for each line of a family that is not kept, want at most 1", each)
	}
}
