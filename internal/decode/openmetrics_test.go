package decode

import (
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// OpenMetrics input is read by the text parser in OpenMetrics mode
// (openmetrics.go); each case is an exposition and the series it is read as.

func ms(v int64) *int64 { return &v }

func TestOpenMetricsInput(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []model.Metric
	}{
		{
			name: "counter family foo, sample foo_total, _created dropped",
			body: "# TYPE jobs counter\n# HELP jobs Jobs done.\njobs_total{q=\"a\"} 3\njobs_created{q=\"a\"} 1700000000.5\n# EOF\n",
			want: []model.Metric{{Name: "jobs_total", Help: "Jobs done.", Type: model.CounterMetricType, Value: 3, Labels: map[string]string{"q": "a"}}},
		},
		{
			name: "counter family already named foo_total",
			body: "# TYPE jobs_total counter\njobs_total 3\njobs_created 1700000000\n# EOF\n",
			want: []model.Metric{{Name: "jobs_total", Type: model.CounterMetricType, Value: 3, Labels: map[string]string{}}},
		},
		{
			name: "timestamps are float seconds",
			body: "# TYPE t gauge\nt 1 1700000000\nt{a=\"b\"} 2 1700000000.123\n# EOF\n",
			want: []model.Metric{
				{Name: "t", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{}, Timestamp: ms(1700000000000)},
				{Name: "t", Type: model.GaugeMetricType, Value: 2, Labels: map[string]string{"a": "b"}, Timestamp: ms(1700000000123)},
			},
		},
		{
			name: "exemplars are skipped, with or without a timestamp",
			body: "# TYPE r counter\nr_total 5 # {trace_id=\"abc\"} 1.0 1700000000.1\n# TYPE s counter\ns_total 6 1700000001 # {trace_id=\"d # e\"} 2\n# EOF\n",
			want: []model.Metric{
				{Name: "r_total", Type: model.CounterMetricType, Value: 5, Labels: map[string]string{}},
				{Name: "s_total", Type: model.CounterMetricType, Value: 6, Labels: map[string]string{}, Timestamp: ms(1700000001000)},
			},
		},
		{
			name: "unknown is untyped, and UNIT is ignored",
			body: "# TYPE u unknown\n# UNIT u seconds\nu 7\n# EOF\n",
			want: []model.Metric{{Name: "u", Type: model.UntypedMetricType, Value: 7, Labels: map[string]string{}}},
		},
		{
			name: "histogram with _created",
			body: "# TYPE h histogram\nh_bucket{le=\"1.0\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_count 2\nh_sum 1.5\nh_created 1700000000\n# EOF\n",
			want: []model.Metric{{Name: "h", Type: model.HistogramMetricType, Labels: map[string]string{}, Histogram: &model.Histogram{
				Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}, {UpperBound: posInf, CumulativeCount: 2}}, Count: 2, Sum: 1.5}}},
		},
		{
			name: "summary with _created",
			body: "# TYPE s summary\ns{quantile=\"0.5\"} 0.2\ns_count 4\ns_sum 1\ns_created 1700000000\n# EOF\n",
			want: []model.Metric{{Name: "s", Type: model.SummaryMetricType, Labels: map[string]string{}, Summary: &model.Summary{
				Quantiles: []model.Quantile{{Quantile: 0.5, Value: 0.2}}, Count: 4, Sum: 1}}},
		},
		{
			name: "info is the gauge foo_info",
			body: "# TYPE build info\n# HELP build Build information.\nbuild_info{version=\"1.2\"} 1\n# EOF\n",
			want: []model.Metric{{Name: "build_info", Help: "Build information.", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"version": "1.2"}}},
		},
		{
			name: "stateset is a gauge",
			body: "# TYPE state stateset\nstate{state=\"up\"} 1\nstate{state=\"down\"} 0\n# EOF\n",
			want: []model.Metric{
				{Name: "state", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"state": "up"}},
				{Name: "state", Type: model.GaugeMetricType, Value: 0, Labels: map[string]string{"state": "down"}},
			},
		},
		{
			name: "gaugehistogram is gauges",
			body: "# TYPE q gaugehistogram\n# HELP q Queue.\nq_bucket{le=\"1.0\"} 3\nq_bucket{le=\"+Inf\"} 5\nq_gcount 5\nq_gsum 4\n# EOF\n",
			want: []model.Metric{
				{Name: "q_bucket", Help: "Queue.", Type: model.GaugeMetricType, Value: 3, Labels: map[string]string{"le": "1.0"}},
				{Name: "q_bucket", Help: "Queue.", Type: model.GaugeMetricType, Value: 5, Labels: map[string]string{"le": "+Inf"}},
				{Name: "q_gcount", Help: "Queue.", Type: model.GaugeMetricType, Value: 5, Labels: map[string]string{}},
				{Name: "q_gsum", Help: "Queue.", Type: model.GaugeMetricType, Value: 4, Labels: map[string]string{}},
			},
		},
		{
			name: "a body without # EOF is accepted",
			body: "# TYPE g gauge\ng 1\n",
			want: []model.Metric{{Name: "g", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseExposition([]byte(tc.body), promOptions{openMetrics: true})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %s\nwant %s", describe(got), describe(tc.want))
			}
		})
	}
}

var posInf = math.Inf(1)

func describe(metrics []model.Metric) string {
	var b strings.Builder
	for _, m := range metrics {
		fmt.Fprintf(&b, "\n  %s %s help=%s", m.Name, m.Type, m.Help)
		if m.Timestamp != nil {
			fmt.Fprintf(&b, " ts=%d", *m.Timestamp)
		}
		for _, k := range model.SortedKeys(m.Labels) {
			fmt.Fprintf(&b, " %s=%s", k, m.Labels[k])
		}
	}
	return b.String()
}

func TestOpenMetricsInputErrors(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"# TYPE g gauge\ng 1\n# EOF\ng 2\n", "line 4: unexpected content after # EOF"},
		{"# TYPE g gauge\ng 1 yesterday\n", `expected a number of seconds as timestamp, got "yesterday"`},
		{"# TYPE g meter\n", `unknown metric type "meter"`},
	} {
		_, err := parseExposition([]byte(tc.body), promOptions{openMetrics: true})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err=%v, want %q", tc.body, err, tc.want)
		}
	}
}

// The text format 0.0.4 is read as before: its timestamps are milliseconds,
// unknown is no type of it, and _created and _total are series of their own.
func TestTextFormatIsNotReadAsOpenMetrics(t *testing.T) {
	got := parseOK(t, "# TYPE c counter\nc_total 1 1700000000000\nc_created 5\n")
	if len(got) != 2 || got[0].Name != "c_total" || got[0].Type != model.UntypedMetricType || *got[0].Timestamp != 1700000000000 || got[1].Name != "c_created" {
		t.Fatalf("got %s", describe(got))
	}
	if _, err := parsePrometheusText([]byte("# TYPE u unknown\n")); err == nil {
		t.Fatal("unknown was accepted by the text format")
	}
}

// OpenMetrics is chosen by the response's Content-Type, or by a final # EOF
// when the Content-Type does not say the text format.
func TestOpenMetricsIsDetected(t *testing.T) {
	body := "# TYPE jobs counter\njobs_total 3 1700000000\n# EOF\n"
	c := &model.Collector{Decoder: model.DecoderConfig{Type: "auto"}}
	for _, tc := range []struct {
		contentType string
		body        string
		openMetrics bool
	}{
		{"application/openmetrics-text; version=1.0.0; charset=utf-8", body, true},
		{"", body, true},
		{"text/plain", body, true},
		{"text/plain; version=0.0.4", "# TYPE jobs counter\njobs_total 3 1700000000\n", false},
		{"application/openmetrics-text", strings.TrimSuffix(body, "# EOF\n"), true},
	} {
		c.Decoder.Type = "prometheus"
		r := &fetch.HTTPResponse{Body: []byte(tc.body), Headers: http.Header{}}
		if tc.contentType != "" {
			r.Headers.Set("Content-Type", tc.contentType)
		}
		d, err := Decode(r, c)
		if err != nil {
			t.Fatalf("%q: %v", tc.contentType, err)
		}
		m := d.Data.(model.MetricSet).Metrics[0]
		if tc.openMetrics != (m.Type == model.CounterMetricType && *m.Timestamp == 1700000000000) {
			t.Errorf("%q: read as %s at %d", tc.contentType, m.Type, *m.Timestamp)
		}
	}
	// Auto-detection sends OpenMetrics to the prometheus decoder.
	c.Decoder.Type = "auto"
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"application/openmetrics-text; version=1.0.0"}}}
	if d, err := Decode(r, c); err != nil || d.Kind != "prometheus" {
		t.Fatalf("auto: %v", err)
	}
}
