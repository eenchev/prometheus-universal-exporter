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

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// End to end: a label written "" under transform.labels, or among a static
// target's labels, is in no answer. A probe of a collector with
// transform.labels: {site: "", env: prod} was answered v{env="prod",site=""},
// and one whose rule gives site had the rule's value replaced by the empty
// one; the static targets endpoint served team="" on a target's series and
// on its health series, and the OTLP export carried each as a label. Now
// the answers carry the labels that have a value, the rule's label keeps
// its value, a transform.labels value that is not empty still replaces the
// rule's, and limits.max_labels_per_metric counts what is exported.
func TestALabelWrittenEmptyIsInNoAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("v=7 site=rack1\n"))
	}))
	defer upstream.Close()
	collector := func(name string, labels map[string]string, rule ...model.LabelRule) model.Collector {
		return model.Collector{
			Name: name, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Limits: model.Limits{MaxLabelsPerMetric: 1},
			Transform: model.TransformConfig{Type: "regex", Labels: labels},
			Metrics:   []model.MetricRule{{Name: "v", Expression: `v=(\d+) site=(?P<site>\w+)`, Labels: rule}},
		}
	}
	site := model.LabelRule{Name: "site", Expression: "site"}
	cfg := &model.Config{OTLP: otlpConfig("http://collector.invalid/v1/metrics"), Collectors: []model.Collector{
		collector("constant", map[string]string{"site": "", "env": "prod"}),
		collector("beside_a_rule", map[string]string{"site": ""}, site),
		collector("over_a_rule", map[string]string{"site": "dc1"}, site),
		collector("plain", nil),
	}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{ExportViaOTLP: true, Name: "eu", Collector: "plain", Target: upstream.URL, Labels: map[string]string{"team": "", "zone": "a"}},
	}}
	server := newStaticServer(t, cfg, file)
	for name, want := range map[string]string{"constant": `v{env="prod"} 7`, "beside_a_rule": `v{site="rack1"} 7`, "over_a_rule": `v{site="dc1"} 7`, "plain": "v 7"} {
		response := probeOnce(t, server, "/probe?collector="+name+"&target="+url.QueryEscape(upstream.URL), nil)
		if body := response.Body.String(); response.Code != http.StatusOK || body != "# TYPE v gauge\n"+want+"\n" {
			t.Errorf("collector %s is answered %d:\n%s\nwant %s", name, response.Code, body, want)
		}
	}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			for name, value := range m.Labels {
				if value == "" {
					t.Errorf("the OTLP export of a probe carries %s with the label %s empty: %v", m.Name, name, m.Labels)
				}
			}
		}
	}

	server.scrapeStaticTargets(context.Background(), 0)
	body := probeOnce(t, server, "/static-targets", nil).Body.String()
	target := `collector="plain",static_target="eu",target="` + upstream.URL + `",zone="a"`
	for _, want := range []string{`v{static_target="eu",zone="a"} 7` + "\n", "http_exporter_target_up{" + target + "} 1\n", "http_exporter_target_scrape_duration_seconds{" + target + "} "} {
		if !strings.Contains(body, want) {
			t.Errorf("the static targets endpoint lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `=""`) || strings.Contains(body, "team") {
		t.Errorf("the static targets endpoint serves an empty label:\n%s", body)
	}
	exported := 0
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			exported++
			if _, has := m.Labels["team"]; has || m.Labels["zone"] != "a" {
				t.Errorf("the OTLP export of the target carries %s with the labels %v", m.Name, m.Labels)
			}
		}
	}
	if exported < 4 {
		t.Errorf("%d series of the target were queued for OTLP", exported)
	}
}
