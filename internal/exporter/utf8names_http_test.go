//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// utf8Collector is a prometheus pass-through under
// metric_name_validation_scheme utf8.
func utf8Collector(name string) model.Collector {
	c := passthrough(name, "", "")
	c.MetricNameValidationScheme = model.NameValidationUTF8
	return c
}

const utf8Exposition = "# HELP \"http.server.duration\" How long.\n# TYPE \"http.server.duration\" gauge\n{\"http.server.duration\",\"service.name\"=\"api\",code=\"200\"} 0.25\n" +
	"# TYPE \"my.requests_total\" counter\n{\"my.requests_total\"} 7\n" +
	"# TYPE \"my.latency\" histogram\n{\"my.latency_bucket\",le=\"0.1\"} 1\n{\"my.latency_bucket\",le=\"+Inf\"} 2\n{\"my.latency_sum\"} 0.3\n{\"my.latency_count\"} 2\nclassic_total 3\n"

// Under metric_name_validation_scheme utf8 a target's UTF-8 names are kept,
// and each answer writes them as its scraper's Accept asks: quoted for
// escaping=allow-utf-8, in the text format and in OpenMetrics, and escaped
// by underscores, dots or values otherwise, underscores when the range
// names no escaping or one the exporter does not know. The Content-Type says
// which. A classic name is never escaped, and the quoted answer reads back,
// with the exporter's own prometheus decoder, as the names the target gave.
func TestUTF8NamesAreWrittenAsTheScraperAsks(t *testing.T) {
	target := utf8Target(t, utf8Exposition)
	server := verboseServer(t, false, utf8Collector("utf8"))
	server.logger = testutil.QuietLogger(t)
	path := "/probe?collector=utf8&target=" + url.QueryEscape(target.URL)
	for _, tc := range []struct {
		accept, contentType string
		want                []string
	}{
		{"", expositionContentType + "; escaping=underscores", []string{
			"# HELP http_server_duration How long.\n# TYPE http_server_duration gauge\n",
			`http_server_duration{code="200",service_name="api"} 0.25`, "my_requests_total 7", `my_latency_bucket{le="0.1"} 1`, "my_latency_count 2", "classic_total 3",
		}},
		{"text/plain;version=1.0.0;escaping=allow-utf-8", expositionContentType + "; escaping=allow-utf-8", []string{
			"# HELP \"http.server.duration\" How long.\n# TYPE \"http.server.duration\" gauge\n",
			`{"http.server.duration",code="200","service.name"="api"} 0.25`, `{"my.requests_total"} 7`,
			`{"my.latency_bucket",le="0.1"} 1`, `{"my.latency_bucket",le="+Inf"} 2`, `{"my.latency_sum"} 0.3`, `{"my.latency_count"} 2`, "classic_total 3",
		}},
		{"text/plain;escaping=dots", expositionContentType + "; escaping=dots", []string{
			`http_dot_server_dot_duration{code="200",service_dot_name="api"} 0.25`, "my_dot_requests__total 7", `my_dot_latency_bucket{le="0.1"} 1`, "classic_total 3",
		}},
		{"text/plain;escaping=values", expositionContentType + "; escaping=values", []string{
			`U__http_2e_server_2e_duration{U__service_2e_name="api",code="200"} 0.25`, "U__my_2e_requests__total 7", `U__my_2e_latency_bucket{le="0.1"} 1`, "classic_total 3",
		}},
		{"text/plain;escaping=whatever", expositionContentType + "; escaping=underscores", []string{"http_server_duration{"}},
		{"application/openmetrics-text;version=1.0.0;escaping=allow-utf-8", "application/openmetrics-text; version=1.0.0; charset=utf-8; escaping=allow-utf-8", []string{
			"# TYPE \"http.server.duration\" gauge\n# HELP \"http.server.duration\" How long.\n",
			`{"http.server.duration",code="200","service.name"="api"} 0.25`,
			"# TYPE \"my.requests\" counter\n{\"my.requests_total\"} 7\n",
			`{"my.latency_bucket",le="0.1"} 1`, "# EOF\n",
		}},
		{prometheus2Accept, "application/openmetrics-text; version=1.0.0; charset=utf-8; escaping=underscores", []string{
			"# TYPE my_requests counter\nmy_requests_total 7\n", `http_server_duration{code="200",service_name="api"} 0.25`,
		}},
	} {
		header := http.Header{}
		if tc.accept != "" {
			header.Set("Accept", tc.accept)
		}
		r := probeOnce(t, server, path, header)
		body := r.Body.String()
		if r.Code != http.StatusOK || r.Header().Get("Content-Type") != tc.contentType {
			t.Fatalf("Accept %q: %d %q\n%s", tc.accept, r.Code, r.Header().Get("Content-Type"), body)
		}
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("Accept %q: no %q in\n%s", tc.accept, want, body)
			}
		}
		if !strings.Contains(tc.accept, "openmetrics") {
			if err := parseExposition([]byte(body)); err != nil {
				t.Errorf("Accept %q: the answer does not parse: %v\n%s", tc.accept, err, body)
			}
		}
	}

	// The quoted answer is the target's names again.
	r := probeOnce(t, server, path, http.Header{"Accept": {"text/plain;version=1.0.0;escaping=allow-utf-8"}})
	again := utf8Target(t, r.Body.String())
	back := probeOnce(t, server, "/probe?collector=utf8&target="+url.QueryEscape(again.URL), http.Header{"Accept": {"text/plain;version=1.0.0;escaping=allow-utf-8"}})
	if back.Body.String() != r.Body.String() {
		t.Fatalf("read back:\n%s\nwas\n%s", back.Body.String(), r.Body.String())
	}
}

// An answer of classic names alone has nothing to escape, under utf8 too, and
// is written and typed as it always was: no escaping in its Content-Type.
func TestClassicNamesUnderUTF8AreWrittenAsBefore(t *testing.T) {
	target := utf8Target(t, "classic_total 3\n")
	server := verboseServer(t, false, utf8Collector("utf8"))
	r := probeOnce(t, server, "/probe?collector=utf8&target="+url.QueryEscape(target.URL), http.Header{"Accept": {"text/plain;escaping=allow-utf-8"}})
	if r.Header().Get("Content-Type") != expositionContentType || !strings.Contains(r.Body.String(), "classic_total 3\n") {
		t.Fatalf("%q\n%s", r.Header().Get("Content-Type"), r.Body.String())
	}
}

// Under utf8 a rule may name a metric and its labels in any UTF-8, as a
// pass-through may: the configuration loads, where under legacy it is
// refused as it always was. A name Prometheus reserves, beginning with __,
// is refused under both, and so is a scheme that is neither legacy nor utf8.
func TestUTF8RuleNamesLoadOnlyUnderUTF8(t *testing.T) {
	rule := func(scheme, name, label string) error {
		c := testutil.Collector("rules", "json")
		c.Transform = model.TransformConfig{Type: "jq"}
		c.MetricNameValidationScheme = scheme
		c.Metrics = []model.MetricRule{{Name: name, Expression: ".v", Labels: []model.LabelRule{{Name: label, Expression: ".l"}}}}
		cfg := &model.Config{Collectors: []model.Collector{c}}
		return config.Validate(cfg)
	}
	if err := rule(model.NameValidationUTF8, "http.server.duration", "service.name"); err != nil {
		t.Fatalf("utf8: %v", err)
	}
	if err := rule("", "http.server.duration", "site"); err == nil || !strings.Contains(err.Error(), "is not a valid Prometheus metric name") {
		t.Fatalf("legacy: %v", err)
	}
	if err := rule(model.NameValidationLegacy, "up", "service.name"); err == nil || !strings.Contains(err.Error(), `invalid label name "service.name"`) {
		t.Fatalf("legacy label: %v", err)
	}
	if err := rule(model.NameValidationUTF8, "up", "__service.name"); err == nil || !strings.Contains(err.Error(), "starts with __") {
		t.Fatalf("reserved: %v", err)
	}
	if err := rule("UTF8", "up", "site"); err == nil || !strings.Contains(err.Error(), `metric_name_validation_scheme must be legacy or utf8, not "UTF8"`) {
		t.Fatalf("scheme: %v", err)
	}
}

// Two labels that an escaping makes one name are one label, the same in every
// answer: the classic name's value, or of two escaped names the first's.
func TestLabelsThatEscapeToOneNameAreOneLabel(t *testing.T) {
	target := utf8Target(t, "m{\"a.b\"=\"dot\",a_b=\"classic\"} 1\nn{\"c.d\"=\"dot\",\"c-d\"=\"dash\"} 1\n")
	server := verboseServer(t, false, utf8Collector("utf8"))
	path := "/probe?collector=utf8&target=" + url.QueryEscape(target.URL)
	for range 20 {
		body := probeOnce(t, server, path, nil).Body.String()
		if !strings.Contains(body, `m{a_b="classic"} 1`) || !strings.Contains(body, `n{c_d="dash"} 1`) {
			t.Fatalf("%s", body)
		}
	}
}

// Over OTLP a UTF-8 name and label name are sent as they are, and
// otlp.metric_names: otel moves a unit suffix into the unit as it does of a
// classic name.
func TestUTF8NamesOverOTLP(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{{Name: "http.server.duration_seconds", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"service.name": "api"}}}}
	for otel, want := range map[bool][2]string{false: {"http.server.duration_seconds", ""}, true: {"http.server.duration", "s"}} {
		var out []otlpMetric
		if otel {
			out, _ = otlpPoints(set, -1, true, "1", nil, nil)
		} else {
			out = otlpMetrics(set, "1", nil)
		}
		if len(out) != 1 || out[0].Name != want[0] || out[0].Unit != want[1] || out[0].Gauge.DataPoints[0].Attributes[0].Key != "service.name" {
			t.Fatalf("otel %v: %+v", otel, out)
		}
	}
}

// The static targets endpoint writes a utf8 collector's names as /probe does:
// quoted for allow-utf-8, escaped by underscores for a scraper that names no
// escaping, with the escaping in its Content-Type, beside its static_target
// label, which is classic.
func TestStaticTargetsWriteUTF8NamesAsTheScraperAsks(t *testing.T) {
	target := utf8Target(t, "{\"http.server.duration\",\"service.name\"=\"api\"} 0.25\n")
	cfg := &model.Config{Collectors: []model.Collector{utf8Collector("utf8")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "utf8", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 0)
	for accept, want := range map[string][2]string{
		"":                                {expositionContentType + "; escaping=underscores", `http_server_duration{service_name="api",static_target="one"} 0.25`},
		"text/plain;escaping=allow-utf-8": {expositionContentType + "; escaping=allow-utf-8", `{"http.server.duration","service.name"="api",static_target="one"} 0.25`},
	} {
		request := httptest.NewRequest(http.MethodGet, "/static-targets", nil)
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Header().Get("Content-Type") != want[0] || !strings.Contains(recorder.Body.String(), want[1]) {
			t.Fatalf("Accept %q: %q\n%s", accept, recorder.Header().Get("Content-Type"), recorder.Body)
		}
	}
}
