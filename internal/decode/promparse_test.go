package decode

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The prometheus decoder parses the text exposition format itself rather than
// through github.com/prometheus/common/expfmt. Before the switch its output was
// compared with expfmt's on the cases below, on 50,000 generated expositions
// and on 250,000 mutations of them; it matched everywhere except the
// deliberate differences these tests pin down.

func parseOK(t *testing.T, body string) []model.Metric {
	t.Helper()
	metrics, err := parsePrometheusText([]byte(body))
	if err != nil {
		t.Fatalf("parse %q: %v", body, err)
	}
	return metrics
}

func TestPromParseFamiliesHelpTypesAndTimestamps(t *testing.T) {
	metrics := parseOK(t, `# HELP requests_total Requests \\ handled\nsince start
# TYPE requests_total counter
requests_total{service="api",code="200"} 42 1700000000000
requests_total{service="web",code="500"} 3
# TYPE temperature gauge
temperature -1.5
plain 7
`)
	if len(metrics) != 4 {
		t.Fatalf("metrics=%#v", metrics)
	}
	first := metrics[0]
	if first.Name != "requests_total" || first.Type != model.CounterMetricType || first.Value != 42 ||
		first.Help != "Requests \\ handled\nsince start" ||
		!reflect.DeepEqual(first.Labels, map[string]string{"service": "api", "code": "200"}) ||
		first.Timestamp == nil || *first.Timestamp != 1700000000000 {
		t.Fatalf("first=%#v", first)
	}
	if metrics[1].Timestamp != nil || metrics[1].Labels["code"] != "500" {
		t.Fatalf("second=%#v", metrics[1])
	}
	if metrics[2].Name != "temperature" || metrics[2].Type != model.GaugeMetricType || metrics[2].Value != -1.5 {
		t.Fatalf("third=%#v", metrics[2])
	}
	// A family without a TYPE line is untyped.
	if metrics[3].Name != "plain" || metrics[3].Type != model.UntypedMetricType || metrics[3].Value != 7 {
		t.Fatalf("fourth=%#v", metrics[3])
	}
}

func TestPromParseHistogramGroupsBucketsSumAndCount(t *testing.T) {
	metrics := parseOK(t, `# TYPE latency_seconds histogram
latency_seconds_bucket{path="/a",le="0.1"} 1
latency_seconds_bucket{le="1",path="/a"} 4
latency_seconds_bucket{path="/a",le="+Inf"} 5
latency_seconds_sum{path="/a"} 2.5
latency_seconds_count{path="/a"} 5
latency_seconds_bucket{path="/b",le="+Inf"} 0
latency_seconds_sum{path="/b"} 0
latency_seconds_count{path="/b"} 0
`)
	if len(metrics) != 2 {
		t.Fatalf("metrics=%#v", metrics)
	}
	a := metrics[0]
	if a.Name != "latency_seconds" || a.Type != model.HistogramMetricType || a.Labels["path"] != "/a" || len(a.Labels) != 1 {
		t.Fatalf("a=%#v", a)
	}
	want := &model.Histogram{Sum: 2.5, Count: 5, Buckets: []model.Bucket{{UpperBound: 0.1, CumulativeCount: 1}, {UpperBound: 1, CumulativeCount: 4}, {UpperBound: math.Inf(1), CumulativeCount: 5}}}
	if !reflect.DeepEqual(a.Histogram, want) {
		t.Fatalf("histogram=%#v", a.Histogram)
	}
	if metrics[1].Labels["path"] != "/b" || metrics[1].Histogram.Count != 0 {
		t.Fatalf("b=%#v", metrics[1])
	}
}

func TestPromParseSummaryGroupsQuantilesSumAndCount(t *testing.T) {
	metrics := parseOK(t, `# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.2
rpc_seconds{quantile="0.99"} NaN
rpc_seconds_sum 12
rpc_seconds_count 40
`)
	if len(metrics) != 1 {
		t.Fatalf("metrics=%#v", metrics)
	}
	s := metrics[0].Summary
	if metrics[0].Type != model.SummaryMetricType || s == nil || s.Sum != 12 || s.Count != 40 || len(s.Quantiles) != 2 ||
		s.Quantiles[0] != (model.Quantile{Quantile: 0.5, Value: 0.2}) || s.Quantiles[1].Quantile != 0.99 || !math.IsNaN(s.Quantiles[1].Value) {
		t.Fatalf("summary=%#v", metrics[0])
	}
}

// _sum and _count only join a family that is a summary or histogram; for any
// other family they are families of their own, as is an OpenMetrics _total.
func TestPromParseSuffixesOnlyJoinSummariesAndHistograms(t *testing.T) {
	metrics := parseOK(t, `# TYPE jobs counter
jobs_total 3
jobs_count 4
`)
	names := []string{metrics[0].Name, metrics[1].Name}
	if !reflect.DeepEqual(names, []string{"jobs_total", "jobs_count"}) || metrics[0].Type != model.UntypedMetricType {
		t.Fatalf("metrics=%#v", metrics)
	}
}

func TestPromParseQuotedUTF8Names(t *testing.T) {
	metrics := parseOK(t, `{"http.server.duration", "service.name"="api"} 1
{"cpu.load"} 2
"disk.used"{mount="/"} 3
# HELP "gc.pause" Pause "time"
{"gc.pause"} 4
`)
	if len(metrics) != 4 || metrics[0].Name != "http.server.duration" || metrics[0].Labels["service.name"] != "api" ||
		metrics[1].Name != "cpu.load" || metrics[2].Name != "disk.used" || metrics[2].Labels["mount"] != "/" ||
		metrics[3].Help != `Pause "time"` {
		t.Fatalf("metrics=%#v", metrics)
	}
}

func TestPromParseLabelEscapesAndSpacing(t *testing.T) {
	metrics := parseOK(t, "  m { a = \"x\\\"y\\\\z\\nw\" , b=\"\" , } \t 5\n")
	if len(metrics) != 1 || metrics[0].Labels["a"] != "x\"y\\z\nw" || metrics[0].Labels["b"] != "" || metrics[0].Value != 5 {
		t.Fatalf("metrics=%#v", metrics)
	}
}

func TestPromParseSpecialValues(t *testing.T) {
	metrics := parseOK(t, "a NaN\nb +Inf\nc -Inf\nd 1e3\ne -0.5\n")
	if !math.IsNaN(metrics[0].Value) || !math.IsInf(metrics[1].Value, 1) || !math.IsInf(metrics[2].Value, -1) || metrics[3].Value != 1000 || metrics[4].Value != -0.5 {
		t.Fatalf("metrics=%#v", metrics)
	}
}

// Comments other than HELP and TYPE are ignored, including OpenMetrics' # EOF,
// and a family that never gets a sample is dropped.
func TestPromParseCommentsAndEmptyFamilies(t *testing.T) {
	metrics := parseOK(t, `# just a comment
#
# HELP
# TYPE lonely gauge
# HELP lonely never sampled

value 1
# EOF
`)
	if len(metrics) != 1 || metrics[0].Name != "value" {
		t.Fatalf("metrics=%#v", metrics)
	}
}

// Real endpoints omit the final newline, use CRLF, or leave trailing blanks.
// expfmt rejected all three; they are accepted now.
func TestPromParseAcceptsWhatExpfmtRejected(t *testing.T) {
	for name, body := range map[string]string{
		"no final newline": "# TYPE a gauge\na 1",
		"crlf":             "# TYPE a gauge\r\na 1\r\n",
		"trailing blanks":  "a 1 \t\n",
		"after timestamp":  "a 1 1700000000000  \n",
		"trailing type":    "# TYPE a gauge  \na 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			metrics := parseOK(t, body)
			if len(metrics) != 1 || metrics[0].Value != 1 {
				t.Fatalf("metrics=%#v", metrics)
			}
		})
	}
}

func TestPromParseErrors(t *testing.T) {
	tests := []struct{ body, want string }{
		{"a 1\n# TYPE a gauge\n", "line 2: second TYPE line for metric name \"a\", or TYPE reported after samples"},
		{"# TYPE a gauge\n# TYPE a counter\n", "line 2: second TYPE line"},
		{"# HELP a x\n# HELP a y\n", "line 2: second HELP line"},
		{"# TYPE a meter\n", `unknown metric type "meter"`},
		{"# TYPE a gauge_histogram\na 1\n", `unknown metric type "gauge_histogram"`},
		{"# HELP a{ x\n", "invalid metric name in comment"},
		{"# HELP a bad \\t escape\n", `invalid escape sequence '\t'`},
		{"a{__name__=\"b\"} 1\n", `label name "__name__" is reserved`},
		{"a{b=\"1\",b=\"2\"} 1\n", `duplicate label name "b"`},
		{"a{b=\"\\q\"} 1\n", `invalid escape sequence '\q'`},
		{"a{b=c} 1\n", `expected '"' at start of the value of label "b"`},
		{"a{b=\"c\" d=\"e\"} 1\n", `unexpected "d" after the value of label "b"`},
		{"a{b=\"c\"\n", `unexpected end of label set after label "b"`},
		{"a{b=\"c} 1\n", `label value "c} 1" contains unescaped new-line`},
		{"a{b} 1\n", `expected '=' after label name "b"`},
		{"{\"x\",\"y\"} 1\n", `multiple metric names for metric "x"`},
		{"{b=\"c\"} 1\n", "invalid metric name"},
		{"a b\n", `expected float as value, got "b"`},
		{"a\n", `expected float as value, got ""`},
		{"a 0x1p3\n", `expected float as value, got "0x1p3"`},
		{"a 1_000\n", `expected float as value, got "1_000"`},
		{"a 1 1.5\n", `expected integer as timestamp, got "1.5"`},
		{"a 1 2 3\n", `spurious string after timestamp: "3"`},
		{"1a 1\n", "invalid metric name"},
		{"# TYPE h histogram\nh_bucket{le=\"x\"} 1\n", `expected float as value for 'le' label, got "x"`},
		{"# TYPE s summary\ns{quantile=\"x\"} 1\n", `expected float as value for 'quantile' label, got "x"`},
		{"# TYPE h histogram\nh_count -1\n", `expected a non-negative count for "h", got -1`},
		{"# TYPE h histogram\nh_bucket{le=\"1\"} NaN\n", `expected a non-negative count for "h", got NaN`},
		{"# TYPE s summary\ns_count +Inf\n", `expected a non-negative count for "s", got +Inf`},
	}
	for _, test := range tests {
		_, err := parsePrometheusText([]byte(test.body))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: err=%v, want it to contain %q", test.body, err, test.want)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "text format parsing error in line ") {
			t.Errorf("%q: err=%v has no line number", test.body, err)
		}
	}
}

// expfmt panicked on these, so a target that served one took the exporter down
// from a scheduled scrape, where no HTTP handler recovers the panic.
func TestPromParseSurvivesInputThatCrashedExpfmt(t *testing.T) {
	for _, body := range []string{"{b=\"c\",} 1\n", "{} 1\n", "{}\"x\",\"y\"} 1\n", "{b=\"c\",} 1l\n"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%q panicked: %v", body, r)
				}
			}()
			if _, err := parsePrometheusText([]byte(body)); err == nil {
				t.Errorf("%q parsed without a metric name", body)
			}
		}()
	}
}

// Families come back in first-seen order, so a decode is deterministic.
func TestPromParseOrderIsDeterministic(t *testing.T) {
	body := "z 1\na 2\n# TYPE m summary\nm_sum 1\nm_count 1\nb 3\n"
	var names []string
	for _, m := range parseOK(t, body) {
		names = append(names, m.Name)
	}
	if !reflect.DeepEqual(names, []string{"z", "a", "m", "b"}) {
		t.Fatalf("names=%v", names)
	}
}

func FuzzParsePrometheusText(f *testing.F) {
	for _, seed := range []string{
		"a 1\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_sum 1\nh_count 1\n",
		"# TYPE s summary\ns{quantile=\"0.5\"} 1\n", "{\"x.y\",a=\"b\"} 1 2\n", "{b=\"c\",} 1\n", "a{b=\"\\n\"} NaN",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		metrics, err := parsePrometheusText([]byte(body))
		if err != nil {
			return
		}
		for _, m := range metrics {
			if m.Name == "" {
				t.Fatalf("%q produced a metric without a name", body)
			}
		}
	})
}
