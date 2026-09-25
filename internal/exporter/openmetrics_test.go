package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The exporter answers in OpenMetrics when the scraper asks for it, as
// Prometheus does, and in the Prometheus text format otherwise (exposition.go).

const (
	// What Prometheus 2 sends by default.
	prometheus2Accept = "application/openmetrics-text;version=1.0.0,application/openmetrics-text;version=0.0.1;q=0.75,text/plain;version=0.0.4;q=0.5,*/*;q=0.1"
	// What Prometheus 3 sends with its default scrape protocols, protobuf
	// first when native histograms are on.
	prometheus3Accept = "application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.6,application/openmetrics-text;version=1.0.0;escaping=allow-utf-8;q=0.5,application/openmetrics-text;version=0.0.1;q=0.4,text/plain;version=1.0.0;escaping=allow-utf-8;q=0.3,text/plain;version=0.0.4;q=0.2,*/*;q=0.1"
	openMetricsType1  = "application/openmetrics-text; version=1.0.0; charset=utf-8"
)

func TestNegotiateFormat(t *testing.T) {
	for _, tc := range []struct {
		name, accept string
		openMetrics  bool
		version      string
	}{
		{name: "no Accept", accept: ""},
		{name: "a browser", accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		{name: "curl", accept: "*/*"},
		{name: "Prometheus 2", accept: prometheus2Accept, openMetrics: true, version: "1.0.0"},
		{name: "Prometheus 3", accept: prometheus3Accept, openMetrics: true, version: "1.0.0"},
		{name: "OpenMetrics without a version", accept: "application/openmetrics-text", openMetrics: true, version: "1.0.0"},
		{name: "only OpenMetrics 0.0.1", accept: "application/openmetrics-text;version=0.0.1", openMetrics: true, version: "0.0.1"},
		{name: "case and spaces", accept: " Application/OpenMetrics-Text ; Version=1.0.0 ; q=0.9 , text/plain;q=0.5", openMetrics: true, version: "1.0.0"},
		{name: "a version it does not write", accept: "application/openmetrics-text;version=2.0.0,text/plain;q=0.1"},
		{name: "text preferred", accept: "application/openmetrics-text;q=0.4,text/plain;q=0.5"},
		{name: "a tie goes to text", accept: "application/openmetrics-text;q=0.5,text/plain;q=0.5"},
		{name: "OpenMetrics refused", accept: "application/openmetrics-text;q=0,*/*;q=0.1"},
		{name: "an invalid quality is ignored", accept: "application/openmetrics-text;q=high,text/plain;q=0.1"},
		{name: "only protobuf", accept: "application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := negotiateFormat(tc.accept)
			if got.openMetrics != tc.openMetrics || (tc.openMetrics && got.version != tc.version) {
				t.Fatalf("negotiateFormat(%q) = %+v, want openMetrics=%v version %q", tc.accept, got, tc.openMetrics, tc.version)
			}
		})
	}
}

// The OpenMetrics rendering of every metric type, with the rules where it
// differs from the text format.
func TestOpenMetricsExposition(t *testing.T) {
	at := int64(1727000000123)
	whole := int64(1727000000000)
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "requests", Help: `Requests "served", by path` + "\n" + `and a \ backslash.`, Type: model.CounterMetricType, Value: 7, Labels: map[string]string{"path": `/a"b`}},
		{Name: "temperature", Type: model.GaugeMetricType, Value: -1.5, Timestamp: &at},
		// A second series of requests after another family: written with
		// its family.
		{Name: "requests", Type: model.CounterMetricType, Value: 3, Labels: map[string]string{"path": "/c"}},
		{Name: "errors_total", Help: "Errors.", Type: model.CounterMetricType, Value: 2},
		{Name: "mystery", Type: model.UntypedMetricType, Value: 1, Timestamp: &whole},
		{Name: "latency", Type: model.HistogramMetricType, Labels: map[string]string{"op": "get"}, Histogram: &model.Histogram{
			Buckets: []model.Bucket{{UpperBound: 0.5, CumulativeCount: 1}, {UpperBound: 1, CumulativeCount: 3}, {UpperBound: 1e6, CumulativeCount: 4}},
			Sum:     2.5, Count: 5,
		}},
		{Name: "size", Type: model.SummaryMetricType, Summary: &model.Summary{
			Quantiles: []model.Quantile{{Quantile: 0.5, Value: 10}, {Quantile: 1, Value: 99}},
			Sum:       120, Count: 4,
		}},
	}}
	want := `# TYPE requests counter
# HELP requests Requests \"served\", by path\nand a \\ backslash.
requests_total{path="/a\"b"} 7
requests_total{path="/c"} 3
# TYPE temperature gauge
temperature -1.5 1727000000.123
# TYPE errors counter
# HELP errors Errors.
errors_total 2
# TYPE mystery unknown
mystery 1 1727000000
# TYPE latency histogram
latency_bucket{le="0.5",op="get"} 1
latency_bucket{le="1.0",op="get"} 3
latency_bucket{le="1e+06",op="get"} 4
latency_bucket{le="+Inf",op="get"} 5
latency_sum{op="get"} 2.5
latency_count{op="get"} 5
# TYPE size summary
size{quantile="0.5"} 10
size{quantile="1.0"} 99
size_sum 120
size_count 4
# EOF
`
	if got := string(appendOpenMetrics(nil, set)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := string(appendOpenMetrics(nil, &model.MetricSet{})); got != "# EOF\n" {
		t.Fatalf("an empty answer is %q, want only # EOF", got)
	}
}

// OpenMetrics does not allow two families to claim one name, where the text
// format writes them side by side: the counter foo_total and the gauge foo
// would both be the family foo. The families involved are written as unknown
// under the names their samples have in the text format, in either order, so
// the answer is valid and Prometheus stores the same series from either
// format.
func TestOpenMetricsFamiliesThatWouldClaimOneName(t *testing.T) {
	counter := func(name string, v float64) model.Metric {
		return model.Metric{Name: name, Type: model.CounterMetricType, Value: v}
	}
	gauge := func(name string, v float64) model.Metric {
		return model.Metric{Name: name, Type: model.GaugeMetricType, Value: v}
	}
	histogram := model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{
		Buckets: []model.Bucket{{UpperBound: 0.5, CumulativeCount: 1}}, Sum: 2, Count: 3,
	}}
	summary := model.Metric{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{
		Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: 2, Count: 3,
	}}
	for _, tc := range []struct {
		name    string
		metrics []model.Metric
		want    string
	}{
		{
			name:    "a counter foo_total, then a gauge foo",
			metrics: []model.Metric{counter("foo_total", 1), gauge("foo", 2)},
			want:    "# TYPE foo_total unknown\nfoo_total 1\n# TYPE foo gauge\nfoo 2\n# EOF\n",
		},
		{
			name:    "a gauge foo, then a counter foo_total",
			metrics: []model.Metric{gauge("foo", 2), counter("foo_total", 1)},
			want:    "# TYPE foo gauge\nfoo 2\n# TYPE foo_total unknown\nfoo_total 1\n# EOF\n",
		},
		{
			name:    "counters jobs and jobs_total",
			metrics: []model.Metric{counter("jobs", 1), counter("jobs_total", 2)},
			want:    "# TYPE jobs unknown\njobs 1\n# TYPE jobs_total unknown\njobs_total 2\n# EOF\n",
		},
		{
			name:    "counters jobs_total and jobs",
			metrics: []model.Metric{counter("jobs_total", 2), counter("jobs", 1)},
			want:    "# TYPE jobs_total unknown\njobs_total 2\n# TYPE jobs unknown\njobs 1\n# EOF\n",
		},
		{
			name:    "a counter foo, then a gauge foo_total",
			metrics: []model.Metric{counter("foo", 1), gauge("foo_total", 2)},
			want:    "# TYPE foo unknown\nfoo 1\n# TYPE foo_total gauge\nfoo_total 2\n# EOF\n",
		},
		{
			// The counter gives way, and the histogram keeps its type.
			name:    "a histogram h, then a counter h_sum_total",
			metrics: []model.Metric{histogram, counter("h_sum_total", 1)},
			want:    "# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"+Inf\"} 3\nh_sum 2\nh_count 3\n# TYPE h_sum_total unknown\nh_sum_total 1\n# EOF\n",
		},
		{
			// The text format writes the gauge and the histogram's count under
			// one name; so does OpenMetrics, in one unknown family.
			name:    "a gauge h_count, then a histogram h",
			metrics: []model.Metric{gauge("h_count", 9), histogram},
			want:    "# TYPE h_count unknown\nh_count 9\nh_count 3\n# TYPE h_bucket unknown\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"+Inf\"} 3\n# TYPE h_sum unknown\nh_sum 2\n# EOF\n",
		},
		{
			name:    "a summary s, then a gauge s_sum",
			metrics: []model.Metric{summary, gauge("s_sum", 9)},
			want:    "# TYPE s unknown\ns{quantile=\"0.5\"} 1\n# TYPE s_sum unknown\ns_sum 2\ns_sum 9\n# TYPE s_count unknown\ns_count 3\n# EOF\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := &model.MetricSet{Metrics: tc.metrics}
			got := string(appendOpenMetrics(nil, set))
			if got != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			checkOpenMetricsClaims(t, got)
			text := string(appendMetricSet(nil, set))
			if g, w := exposedSeries(got), exposedSeries(text); !slices.Equal(g, w) {
				t.Fatalf("OpenMetrics series %q, the text format's %q", g, w)
			}
		})
	}
}

// checkOpenMetricsClaims fails t if an OpenMetrics answer names a family
// twice, has a family claim a name another family claims (its name and its
// type's sample names), or has a sample its family's type does not allow, as
// a strict OpenMetrics parser does.
func checkOpenMetricsClaims(t *testing.T, answer string) {
	t.Helper()
	suffixes := map[string][]string{
		"counter":   {"_total", "_created"},
		"histogram": {"_bucket", "_count", "_sum", "_created"},
		"summary":   {"", "_count", "_sum", "_created"},
	}
	claimed := map[string]string{}
	var allowed []string
	for _, line := range strings.Split(strings.TrimSuffix(answer, "# EOF\n"), "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "# HELP "):
		case strings.HasPrefix(line, "# TYPE "):
			fields := strings.Fields(line)
			family, typ := fields[2], fields[3]
			names := []string{family}
			allowed = allowed[:0]
			sampleSuffixes, ok := suffixes[typ]
			if !ok {
				sampleSuffixes = []string{""}
			}
			for _, suffix := range sampleSuffixes {
				names = append(names, family+suffix)
				allowed = append(allowed, family+suffix)
			}
			for _, name := range names {
				if other, taken := claimed[name]; taken && other != family {
					t.Fatalf("families %s and %s both claim %s:\n%s", other, family, name, answer)
				} else if taken && name == family {
					t.Fatalf("# TYPE %s appears twice:\n%s", family, answer)
				}
			}
			for _, name := range names {
				claimed[name] = family
			}
		default:
			name := line[:strings.IndexAny(line, "{ ")]
			if !slices.Contains(allowed, name) {
				t.Fatalf("sample %s is not one its family allows (%q):\n%s", name, allowed, answer)
			}
		}
	}
}

// exposedSeries is the series an answer has, name and labels, sorted.
func exposedSeries(answer string) []string {
	var series []string
	for _, line := range strings.Split(answer, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series = append(series, line[:strings.LastIndexByte(line, ' ')])
	}
	slices.Sort(series)
	return series
}

// A probe, the self-metrics and the static targets endpoint answer in the
// format asked for, and say so, and vary on Accept.
func TestEndpointsAnswerInTheFormatAskedFor(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(target.Close)
	server := flightServer(t, testutil.Collector("om", "text"))
	for _, path := range []string{probePath("om", target.URL, ""), "/self-metrics"} {
		for _, tc := range []struct {
			accept, contentType, ending string
		}{
			{"", expositionContentType, "\n"},
			{prometheus3Accept, openMetricsType1, "# EOF\n"},
		} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if tc.accept != "" {
				request.Header.Set("Accept", tc.accept)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != tc.contentType || !strings.HasSuffix(body, tc.ending) {
				t.Fatalf("%s with Accept %q: %d %q\n%s", path, tc.accept, recorder.Code, recorder.Header().Get("Content-Type"), body)
			}
			if !slices.Contains(recorder.Header().Values("Vary"), "Accept") || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s: headers %v", path, recorder.Header())
			}
			if tc.accept == "" && strings.Contains(body, "# EOF") {
				t.Fatalf("%s: the text format ended with # EOF", path)
			}
		}
	}
}

// Probes sharing one trip to the target each get the format they asked for.
func TestSharedProbesEachGetTheirOwnFormat(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("shared", "text"))
	path := probePath("shared", target.URL, "")
	text := probeAsync(context.Background(), server, path, nil)
	om := probeAsync(context.Background(), server, path, http.Header{"Accept": {prometheus2Accept}})
	waitForWaiters(t, server, 2)
	target.open()
	if got := <-text; got.code != http.StatusOK || strings.Contains(got.body, "# EOF") || !strings.Contains(got.body, "demo_value 42\n") {
		t.Fatalf("the text probe got %d:\n%s", got.code, got.body)
	}
	if got := <-om; got.code != http.StatusOK || !strings.HasSuffix(got.body, "# EOF\n") || !strings.Contains(got.body, "demo_value 42\n") {
		t.Fatalf("the OpenMetrics probe got %d:\n%s", got.code, got.body)
	}
	if n := target.requests.Load(); n != 1 {
		t.Fatalf("the target was asked %d times, want once", n)
	}
}

// The static targets endpoint answers in the format asked for, both from the
// rendering it keeps for unfiltered reads and for a filtered read.
func TestStaticTargetsAnswerInTheFormatAskedFor(t *testing.T) {
	up := textTarget(t, "value=42\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: up.URL},
		{Name: "us", Collector: "text", Target: up.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	for _, path := range []string{"/static-targets", "/static-targets?targets=eu"} {
		for _, accept := range []string{"", prometheus2Accept} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if accept != "" {
				request.Header.Set("Accept", accept)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			body := recorder.Body.String()
			om := accept != ""
			if recorder.Code != http.StatusOK || strings.HasSuffix(body, "# EOF\n") != om || !strings.Contains(body, `demo_value{static_target="eu"} 42`) {
				t.Fatalf("%s with Accept %q: %d\n%s", path, accept, recorder.Code, body)
			}
			if want := map[bool]string{false: expositionContentType, true: openMetricsType1}[om]; recorder.Header().Get("Content-Type") != want {
				t.Fatalf("%s with Accept %q: Content-Type %q", path, accept, recorder.Header().Get("Content-Type"))
			}
		}
	}
}
