package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

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

func passthrough(name, escaping, prefix string) model.Collector {
	c := testutil.Collector(name, "prometheus")
	c.Transform = model.TransformConfig{Type: "prometheus"}
	c.Metrics = nil
	c.NameEscaping = escaping
	c.MetricsPrefix = prefix
	c.Limits = model.Limits{}
	return c
}

func TestUTF8NamesThroughAProbe(t *testing.T) {
	target := utf8Target(t, "# TYPE \"http.server.duration\" gauge\n{\"http.server.duration\",\"service.name\"=\"api\",code=\"200\"} 0.25\nclassic_total 3\n")
	server := verboseServer(t, false,
		passthrough("strict", "", ""),
		passthrough("underscores", transform.NameEscapingUnderscores, ""),
		passthrough("values", transform.NameEscapingValues, ""),
		passthrough("prefixed", transform.NameEscapingValues, "otel"),
	)
	server.logger = testutil.QuietLogger(t)
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
		if _, err := decode.ParsePrometheusText([]byte(body)); err != nil {
			t.Errorf("%s: the answer does not parse: %v", collector, err)
		}
	}
}

// A UTF-8 label name alone fails the scrape by default too.
func TestUTF8LabelNameFailsByDefault(t *testing.T) {
	target := utf8Target(t, "up_total{\"service.name\"=\"api\"} 1\n")
	server := verboseServer(t, false, passthrough("strict", "", ""))
	server.logger = testutil.QuietLogger(t)
	r := probeOnce(t, server, "/probe?collector=strict&target="+url.QueryEscape(target.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "is not a classic Prometheus label name") {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
}

// Names that escape to one name are refused, never merged.
func TestEscapedNamesThatCollide(t *testing.T) {
	labels := utf8Target(t, "m{\"a.b\"=\"1\",a_b=\"2\"} 1\n")
	metrics := utf8Target(t, "{\"a.b\"} 1\na_b 2\n")
	server := verboseServer(t, false, passthrough("u", transform.NameEscapingUnderscores, ""))
	server.logger = testutil.QuietLogger(t)
	r := probeOnce(t, server, "/probe?collector=u&target="+url.QueryEscape(labels.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), `labels`) || !strings.Contains(r.Body.String(), `both become`) {
		t.Fatalf("colliding labels: %d %s", r.Code, r.Body.String())
	}
	r = probeOnce(t, server, "/probe?collector=u&target="+url.QueryEscape(metrics.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "duplicate metric series") {
		t.Fatalf("colliding metrics: %d %s", r.Code, r.Body.String())
	}
}
