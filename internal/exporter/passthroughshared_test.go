//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// cachedSets is a deep copy of every set the cache holds, by key.
func cachedSets(server *Server) map[string]model.MetricSet {
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	out := map[string]model.MetricSet{}
	for key, entry := range server.cache.entries {
		out[key] = model.CloneMetricSet(entry.set)
	}
	return out
}

// withoutAge is an answer without the value of its result's age, which
// differs from one answer to the next.
func withoutAge(answer string) string {
	lines := strings.Split(answer, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, resultAgeMetric+" ") {
			lines[i] = resultAgeMetric
		}
	}
	return strings.Join(lines, "\n")
}

// A prometheus transform that changes nothing hands on the series as they
// were decoded, and one with rules the label maps of the series its rules
// give no label, without copying either (transform.applyPrometheusTransform).
// Nothing that then uses the result may change it in place, and nothing
// does: the set a probe caches is the same after it has answered a second
// probe, after a static target with labels of its own has been scraped from
// it and queued for OTLP, and after the freshness series were added to each
// answer, and the answers of the probes are the same throughout. The static
// target's series carry its labels and the cached ones do not.
func TestCachedPassthroughIsLeftAsItWasCached(t *testing.T) {
	const exposition = `# HELP jobs Jobs by owner.
# TYPE jobs gauge
jobs{owner="a",queue="x"} 1
jobs{owner="b",queue="x"} 2
# TYPE up gauge
up 1
# TYPE latency_seconds histogram
latency_seconds_bucket{handler="/a",le="0.1"} 1
latency_seconds_bucket{handler="/a",le="+Inf"} 2
latency_seconds_sum{handler="/a"} 0.5
latency_seconds_count{handler="/a"} 2
`
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(exposition))
	}))
	defer target.Close()
	collector := func(name string, rules ...model.MetricRule) model.Collector {
		return model.Collector{Name: name, Request: model.RequestConfig{Type: "http", Method: http.MethodGet}, Transform: model.TransformConfig{Type: "prometheus"}, Metrics: rules,
			Cache:         model.CacheConfig{TTL: model.Duration(time.Minute), StaleIfError: model.Duration(time.Minute)},
			ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"}}
	}
	cfg := &model.Config{OTLP: otlpConfig("http://collector.invalid/v1/metrics"), Collectors: []model.Collector{
		collector("passthrough"),
		collector("rules", model.MetricRule{Name: "jobs"}, model.MetricRule{Name: "work", Expression: "^jobs$"}, model.MetricRule{Name: "up", Labels: []model.LabelRule{{Name: "site", Value: "a"}}}),
	}}
	labels := map[string]string{"environment": "production", "owner": "the target's"}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{ExportViaOTLP: true, Name: "passthrough_target", Collector: "passthrough", Target: target.URL, Labels: labels},
		{ExportViaOTLP: true, Name: "rules_target", Collector: "rules", Target: target.URL, Labels: labels},
	}}
	server := newStaticServer(t, cfg, file)

	probe := func(name string) string {
		t.Helper()
		recorder := probeOnce(t, server, "/probe?collector="+name+"&target="+url.QueryEscape(target.URL), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, recorder.Code, recorder.Body.String())
		}
		return withoutAge(recorder.Body.String())
	}
	first := map[string]string{"passthrough": probe("passthrough"), "rules": probe("rules")}
	cached := cachedSets(server)
	if len(cached) != 2 || requests.Load() != 2 {
		t.Fatalf("%d sets cached after %d requests, want one of each collector", len(cached), requests.Load())
	}
	for key, set := range cached {
		for _, m := range set.Metrics {
			if m.Name == resultAgeMetric || m.Name == resultStaleMetric {
				t.Fatalf("the cached set %s has the freshness series of an answer: %+v", key, set.Metrics)
			}
		}
	}
	unchanged := func(after string) {
		t.Helper()
		if now := cachedSets(server); !reflect.DeepEqual(now, cached) {
			t.Fatalf("the cached sets changed %s:\n%+v\nwere:\n%+v", after, now, cached)
		}
	}
	for name, want := range first {
		if got := probe(name); got != want {
			t.Fatalf("%s answered from the cache:\n%s\nwas:\n%s", name, got, want)
		}
	}
	unchanged("after a second probe")

	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	unchanged("after the static targets were scraped")
	if resources := server.drainOTLP(); len(resources) == 0 {
		t.Fatal("the static targets queued nothing for OTLP")
	}
	unchanged("after the static targets' series were taken for OTLP")
	labelled := 0
	server.staticMu.Lock()
	for _, result := range server.staticResults {
		for _, m := range result.Metrics {
			if m.Name != "jobs" && m.Name != "work" {
				continue
			}
			labelled++
			// A target's label never replaces one the series has.
			if m.Labels["environment"] != "production" || m.Labels["owner"] == "the target's" {
				t.Errorf("a static target's series has the labels %v", m.Labels)
			}
		}
	}
	server.staticMu.Unlock()
	if labelled != 6 {
		t.Fatalf("%d series of the static targets are jobs or work, want 6", labelled)
	}
	for name, want := range first {
		if got := probe(name); got != want {
			t.Fatalf("%s answered after the static targets were scraped:\n%s\nwas:\n%s", name, got, want)
		}
	}
	unchanged("after a third probe")
	if got := requests.Load(); got != 2 {
		t.Fatalf("%d requests to the target, want the two of the first probes", got)
	}
	if strings.Contains(first["passthrough"], "environment") || !strings.Contains(first["passthrough"], `jobs{owner="a",queue="x"} 1`) {
		t.Fatalf("the probe's answer:\n%s", first["passthrough"])
	}
}
