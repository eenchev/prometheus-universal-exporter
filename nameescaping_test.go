package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// UTF-8 metric and label names, and name_escaping (nameescaping.go).

func TestEscapeName(t *testing.T) {
	for _, tc := range []struct {
		name, scheme string
		label        bool
		want         string
	}{
		{"http_requests_total", NameEscapingUnderscores, false, "http_requests_total"},
		{"http_requests_total", NameEscapingValues, false, "http_requests_total"},
		{"job:rate5m", NameEscapingValues, false, "job:rate5m"},
		{"http.server.duration", NameEscapingUnderscores, false, "http_server_duration"},
		{"http.server.duration", NameEscapingValues, false, "U__http_2e_server_2e_duration"},
		{"a_b.c", NameEscapingValues, false, "U__a__b_2e_c"},
		{"1st", NameEscapingUnderscores, false, "_st"},
		{"1st", NameEscapingValues, false, "U___31_st"},
		{"température", NameEscapingUnderscores, false, "temp_rature"},
		{"température", NameEscapingValues, false, "U__temp_e9_rature"},
		{"cpu😀", NameEscapingValues, false, "U__cpu_1f600_"},
		{"bad\xffname", NameEscapingValues, false, "U__bad_FFFD_name"},
		// Colons are classic in metric names, not in label names.
		{"a:b", NameEscapingUnderscores, true, "a_b"},
		{"a:b", NameEscapingValues, true, "U__a_3a_b"},
		{"service.name", NameEscapingUnderscores, true, "service_name"},
		{"service.name", NameEscapingFail, true, "service.name"},
	} {
		if got := escapeName(tc.name, tc.scheme, !tc.label); got != tc.want {
			t.Errorf("escapeName(%q, %s, label=%v) = %q, want %q", tc.name, tc.scheme, tc.label, got, tc.want)
		}
	}
}

func TestNameEscapingIsValidated(t *testing.T) {
	c := testCollector("text", "text")
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil || cfg.Collectors[0].NameEscaping != NameEscapingFail {
		t.Fatalf("default: %v %q", err, cfg.Collectors[0].NameEscaping)
	}
	c.NameEscaping = "dots"
	cfg = &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `name_escaping must be fail, underscores or values, not "dots"`) {
		t.Fatalf("dots: %v", err)
	}
}

// utf8Target serves Prometheus text with UTF-8 names, as Prometheus 3 targets
// may.
func utf8Target(t *testing.T, body string) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(target.Close)
	return target
}

func passthrough(name, escaping, prefix string) Collector {
	c := testCollector(name, "prometheus")
	c.Transform = TransformConfig{Type: "prometheus"}
	c.Metrics = nil
	c.NameEscaping = escaping
	c.MetricsPrefix = prefix
	c.Limits = Limits{}
	return c
}

func TestUTF8NamesThroughAProbe(t *testing.T) {
	target := utf8Target(t, "# TYPE \"http.server.duration\" gauge\n{\"http.server.duration\",\"service.name\"=\"api\",code=\"200\"} 0.25\nclassic_total 3\n")
	server := verboseServer(t, false,
		passthrough("strict", "", ""),
		passthrough("underscores", NameEscapingUnderscores, ""),
		passthrough("values", NameEscapingValues, ""),
		passthrough("prefixed", NameEscapingValues, "otel"),
	)
	server.logger = quietLogger(t)
	probe := func(collector string) (int, string) {
		r := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
		return r.Code, r.Body.String()
	}
	code, body := probe("strict")
	if code != http.StatusBadGateway || !strings.Contains(body, `metric name "http.server.duration" is not a classic Prometheus name; set the collector's name_escaping`) {
		t.Fatalf("fail: %d %s", code, body)
	}
	for collector, want := range map[string][]string{
		"underscores": {`http_server_duration{code="200",service_name="api"} 0.25`, "classic_total 3", "# TYPE http_server_duration gauge"},
		"values":      {`U__http_2e_server_2e_duration{U__service_2e_name="api",code="200"} 0.25`, "classic_total 3"},
		"prefixed":    {`U__otel__http_2e_server_2e_duration{U__service_2e_name="api",code="200"} 0.25`, "otel_classic_total 3"},
	} {
		code, body := probe(collector)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", collector, code, body)
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %s in\n%s", collector, w, body)
			}
		}
		if _, err := parsePrometheusText([]byte(body)); err != nil {
			t.Errorf("%s: the answer does not parse: %v", collector, err)
		}
	}
}

// A UTF-8 label name alone fails the scrape by default too.
func TestUTF8LabelNameFailsByDefault(t *testing.T) {
	target := utf8Target(t, "up_total{\"service.name\"=\"api\"} 1\n")
	server := verboseServer(t, false, passthrough("strict", "", ""))
	server.logger = quietLogger(t)
	r := probeOnce(t, server, "/probe?collector=strict&target="+url.QueryEscape(target.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "is not a classic Prometheus label name") {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
}

// Names that escape to one name are refused, never merged.
func TestEscapedNamesThatCollide(t *testing.T) {
	labels := utf8Target(t, "m{\"a.b\"=\"1\",a_b=\"2\"} 1\n")
	metrics := utf8Target(t, "{\"a.b\"} 1\na_b 2\n")
	server := verboseServer(t, false, passthrough("u", NameEscapingUnderscores, ""))
	server.logger = quietLogger(t)
	r := probeOnce(t, server, "/probe?collector=u&target="+url.QueryEscape(labels.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), `labels`) || !strings.Contains(r.Body.String(), `both become`) {
		t.Fatalf("colliding labels: %d %s", r.Code, r.Body.String())
	}
	r = probeOnce(t, server, "/probe?collector=u&target="+url.QueryEscape(metrics.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "duplicate metric series") {
		t.Fatalf("colliding metrics: %d %s", r.Code, r.Body.String())
	}
}

// A label map shared among a transform's metrics is not changed in place.
func TestEscapeNamesCopiesSharedLabels(t *testing.T) {
	shared := map[string]string{"service.name": "api"}
	set := &MetricSet{Metrics: []Metric{{Name: "a.b", Labels: shared}, {Name: "c", Labels: shared}}}
	if err := escapeNames(set, NameEscapingUnderscores); err != nil {
		t.Fatal(err)
	}
	if set.Metrics[0].Name != "a_b" || set.Metrics[0].Labels["service_name"] != "api" || set.Metrics[1].Labels["service_name"] != "api" {
		t.Fatalf("%+v", set.Metrics)
	}
	if _, ok := shared["service.name"]; !ok || len(shared) != 1 {
		t.Fatalf("the shared map was changed: %v", shared)
	}
}
