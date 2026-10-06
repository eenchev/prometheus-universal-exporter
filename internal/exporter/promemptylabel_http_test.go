//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// emptyLabelTarget is an exposition whose series have labels with empty
// values: a gauge beside a label that has one, a histogram on every sample,
// and a counter that has nothing else.
const emptyLabelTarget = "# TYPE m gauge\nm{l=\"\",k=\"v\"} 1\n" +
	"# TYPE h histogram\nh_bucket{l=\"\",le=\"1\"} 1\nh_bucket{le=\"+Inf\",l=\"\"} 2\nh_sum{l=\"\"} 3\nh_count{l=\"\"} 2\n" +
	"# TYPE c_total counter\nc_total{l=\"\",zone=\"\"} 5\n"

// emptyLabelAnswer is the text format's answer to a probe of it.
const emptyLabelAnswer = "# TYPE m gauge\nm{k=\"v\"} 1\n" +
	"# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n" +
	"# TYPE c_total counter\nc_total 5\n"

// emptyLabelOpenMetrics is the OpenMetrics answer.
const emptyLabelOpenMetrics = "# TYPE m gauge\nm{k=\"v\"} 1\n" +
	"# TYPE h histogram\nh_bucket{le=\"1.0\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n" +
	"# TYPE c counter\nc_total 5\n# EOF\n"

const openMetricsAccept = "application/openmetrics-text; version=1.0.0"

// emptyAttributes lists the attributes of an OTLP export, read as JSON,
// that have no value, with how many attributes it has in all: an attribute
// is an object of a key and a value, wherever it stands.
func emptyAttributes(t *testing.T, export []byte) (empty []string, all int) {
	t.Helper()
	var document any
	if err := json.Unmarshal(export, &document); err != nil {
		t.Fatalf("%v\n%s", err, export)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			if key, keyed := v["key"].(string); keyed {
				all++
				if value, _ := v["value"].(map[string]any); value["stringValue"] == nil || value["stringValue"] == "" {
					empty = append(empty, key)
				}
				return
			}
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(document)
	return empty, all
}

// End to end: a target whose exposition has m{l="",k="v"} is answered
// without l, in the text format and in OpenMetrics, which a strict parser
// reads, and its series reach the OTLP endpoint without an attribute that
// has no value. A prometheus collector without a script passed l="" on,
// to the scrape and, as an attribute of no value, to OTLP. An OTLP resource
// attribute written "" is on no resource either.
func TestATargetsEmptyLabelIsInNoAnswerAndNoExport(t *testing.T) {
	target := utf8Target(t, emptyLabelTarget)
	received := make(chan []byte, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- readOTLPBody(t, r)
	}))
	defer endpoint.Close()
	otlp := otlpConfig(endpoint.URL + "/v1/metrics")
	otlp.ResourceAttributes["service.namespace"] = ""
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{passthrough("pass", "", "")}, OTLP: otlp}, nil)
	server.logger = testutil.QuietLogger(t)
	probe := "/probe?collector=pass&target=" + url.QueryEscape(target.URL)

	text := probeOnce(t, server, probe, nil)
	if text.Code != http.StatusOK || text.Body.String() != emptyLabelAnswer {
		t.Errorf("the probe is answered %d:\n%s\nwant\n%s", text.Code, text.Body, emptyLabelAnswer)
	}
	open := probeOnce(t, server, probe, http.Header{"Accept": {openMetricsAccept}})
	answer := open.Body.String()
	if open.Code != http.StatusOK || !strings.HasPrefix(open.Header().Get("Content-Type"), "application/openmetrics-text") {
		t.Fatalf("asked for OpenMetrics, the probe is answered %d as %s:\n%s", open.Code, open.Header().Get("Content-Type"), answer)
	}
	if err := strictOpenMetricsError(answer); err != nil {
		t.Errorf("a strict parser refuses the answer: %v\n%s", err, answer)
	}
	if answer != emptyLabelOpenMetrics {
		t.Errorf("asked for OpenMetrics, the probe is answered\n%s\nwant\n%s", answer, emptyLabelOpenMetrics)
	}
	for format, body := range map[string]string{"text": text.Body.String(), "OpenMetrics": answer} {
		if strings.Contains(body, `=""`) || strings.Contains(body, "l=") || strings.Contains(body, "zone") {
			t.Errorf("the %s answer has a label the target gave no value:\n%s", format, body)
		}
	}

	server.exportOTLP(context.Background(), time.Minute)
	select {
	case export := <-received:
		empty, all := emptyAttributes(t, export)
		if len(empty) > 0 || all < 5 {
			t.Errorf("the export has %d attributes, and these without a value: %q\n%s", all, empty, export)
		}
		for _, want := range []string{`{"key":"k","value":{"stringValue":"v"}}`, `{"key":"deployment.environment","value":{"stringValue":"test"}}`, `"name":"c_total"`, `"name":"h"`} {
			if !strings.Contains(string(export), want) {
				t.Errorf("the export lacks %s:\n%s", want, export)
			}
		}
		if strings.Contains(string(export), "service.namespace") || strings.Contains(string(export), `"key":"l"`) || strings.Contains(string(export), `"key":"zone"`) {
			t.Errorf("the export names an attribute that has no value:\n%s", export)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("nothing was exported")
	}
}

// With a pre-script that passes data on as it got it, the answer is the one
// without the script, in both formats: the script was given the target's
// l="" and its answer was read without it, while the collector without a
// script kept it, so that adding a script that does nothing changed what
// was scraped.
func TestAPassThroughPreScriptAnswersAsTheCollectorWithoutIt(t *testing.T) {
	requirePython(t)
	target := utf8Target(t, emptyLabelTarget)
	scripted := passthrough("scripted", "", "")
	scripted.Limits, scripted.Transform.PreScript = scriptLimits(), "data = data"
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{passthrough("pass", "", ""), scripted}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}, nil)
	server.logger = testutil.QuietLogger(t)
	for format, header := range map[string]http.Header{"text": nil, "OpenMetrics": {"Accept": {openMetricsAccept}}} {
		alone := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL), header)
		passed := probeOnce(t, server, "/probe?collector=scripted&target="+url.QueryEscape(target.URL), header)
		if alone.Code != http.StatusOK || passed.Code != http.StatusOK || alone.Body.String() != passed.Body.String() {
			t.Errorf("%s: the collector answers %d:\n%s\nand through a pre-script that changes nothing %d:\n%s", format, alone.Code, alone.Body, passed.Code, passed.Body)
		}
		if format == "text" && alone.Body.String() != emptyLabelAnswer {
			t.Errorf("the collector answers\n%s\nwant\n%s", alone.Body, emptyLabelAnswer)
		}
	}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			for name, value := range m.Labels {
				if value == "" {
					t.Errorf("%s is queued for OTLP with the label %s empty: %v", m.Name, name, m.Labels)
				}
			}
		}
	}
}

// Two series a target tells apart by nothing but a label with an empty
// value are one series twice, and the probe is refused for the duplicate,
// as one of a target that writes the series twice is. It was refused
// before, the check having always read such a label as none; the answer
// then said that the empty label was why. A histogram written so is one
// histogram with its samples twice, refused where it is read.
func TestATargetsSeriesToldApartByAnEmptyLabelAreRefused(t *testing.T) {
	for body, want := range map[string]string{
		"# TYPE m gauge\nm{l=\"\"} 1\nm 2\n":                                           `collector pass validation failed: duplicate metric series "m"` + "\n",
		"# TYPE m gauge\nm 1\nm 2\n":                                                   `collector pass validation failed: duplicate metric series "m"` + "\n",
		"# TYPE m gauge\nm{l=\"\",k=\"v\"} 1\nm{k=\"v\",j=\"\"} 2\n":                   `collector pass validation failed: duplicate metric series "m"` + "\n",
		"# TYPE h histogram\nh_sum{l=\"\"} 1\nh_count{l=\"\"} 1\nh_sum 1\nh_count 1\n": "collector pass decode failed: decoding Prometheus exposition: text format parsing error in line 4: second h_sum sample for the histogram h\n",
	} {
		target := utf8Target(t, body)
		server := newStaticServer(t, &model.Config{Collectors: []model.Collector{passthrough("pass", "", "")}}, nil)
		server.logger = testutil.QuietLogger(t)
		answer := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL), nil)
		if answer.Code != http.StatusBadGateway || !strings.HasSuffix(answer.Body.String(), want) {
			t.Errorf("%q: answered %d %q, want it refused with %q", body, answer.Code, answer.Body, want)
		}
	}
}

// The exporter's own series have no label with an empty value either: with
// verbose self-metrics, after probes that were answered, refused and never
// reached a target, every label of every series on the self-metrics path has
// a value, in the text format and in OpenMetrics, the per-request families
// and their _created series among them.
func TestTheExportersOwnSeriesHaveNoEmptyLabel(t *testing.T) {
	target := utf8Target(t, emptyLabelTarget)
	ruled := passthrough("ruled", "", "")
	ruled.Metrics = []model.MetricRule{{Expression: "^m$"}, {Name: "histogram", Expression: "^h$"}}
	server := verboseServer(t, true, passthrough("pass", "", ""), ruled)
	server.logger = testutil.QuietLogger(t)
	for _, probe := range []string{"/probe?collector=pass&target=" + url.QueryEscape(target.URL), "/probe?collector=ruled&target=" + url.QueryEscape(target.URL), "/probe?collector=pass&target=" + url.QueryEscape("http://127.0.0.1:1/"), "/probe?collector=pass", "/probe?collector=absent&target=x"} {
		probeOnce(t, server, probe, nil)
	}
	for format, header := range map[string]http.Header{"text": nil, "OpenMetrics": {"Accept": {openMetricsAccept}}} {
		answer := probeOnce(t, server, DefaultSelfMetricsPath, header)
		body := answer.Body.String()
		if answer.Code != http.StatusOK || strings.Count(body, "\n") < 100 || !strings.Contains(body, `http_method="GET"`) || !strings.Contains(body, "http_exporter_build_info{") {
			t.Fatalf("%s: the self-metrics are answered %d:\n%s", format, answer.Code, body)
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, `=""`) {
				t.Errorf("%s: a series of the exporter's own has a label with an empty value: %s", format, line)
			}
		}
	}
}
