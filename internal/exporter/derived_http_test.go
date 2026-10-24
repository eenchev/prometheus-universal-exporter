//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end through /probe: a derived series is exported as any other, with
// its HELP and TYPE, the collector's transform.labels and its prefix; a
// hidden entry is not exported. Each derived entry has its
// http_exporter_rule_failures_total series, and an entry whose operand a
// pass-through shows to be a histogram has it counted there.
func TestDerivedSeriesOnTheProbeResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prom" {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"+Inf\"} 1\nlatency_seconds_sum 1\nlatency_seconds_count 1\nup 1\n"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("STAT get_hits 90\nSTAT get_misses 10\n"))
	}))
	defer upstream.Close()
	document := `collectors:
  - name: memcached
    metrics_prefix: mc
    request:
      type: http
    transform:
      type: regex
      labels:
        env: prod
    metrics:
      - name: get_hits_total
        type: counter
        expression: 'STAT get_hits (\d+)'
      - name: get_misses_total
        type: counter
        expression: 'STAT get_misses (\d+)'
    derived:
      - name: gets_total
        hidden: true
        sum: [get_hits_total, get_misses_total]
      - name: get_hit_ratio
        description: The share of gets that found their item
        ratio: [get_hits_total, gets_total]
  - name: node
    request:
      type: http
    transform:
      type: prometheus
    derived:
      - name: latency_twice
        sum: [latency_seconds, latency_seconds]
`
	path := testutil.WriteFile(t, "config.yaml", document)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, path, slog.New(slog.DiscardHandler)), "python3", slog.New(slog.DiscardHandler))
	body := probeOnce(t, server, "/probe?collector=memcached&target="+url.QueryEscape(upstream.URL), nil).Body.String()
	for _, want := range []string{
		"# HELP mc_get_hit_ratio The share of gets that found their item\n",
		"# TYPE mc_get_hit_ratio gauge\n",
		`mc_get_hit_ratio{env="prod"} 0.9` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "gets_total") {
		t.Errorf("the hidden entry is exported:\n%s", body)
	}
	body = probeOnce(t, server, "/probe?collector=node&target="+url.QueryEscape(upstream.URL+"/prom"), nil).Body.String()
	if !strings.Contains(body, "up 1\n") || strings.Contains(body, "latency_twice") {
		t.Errorf("the pass-through:\n%s", body)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/self-metrics", nil))
	self := recorder.Body.String()
	for _, want := range []string{
		`http_exporter_rule_failures_total{collector="memcached",metric="get_hit_ratio"} 0`,
		`http_exporter_rule_failures_total{collector="node",metric="latency_twice"} 1`,
	} {
		if !strings.Contains(self, want+"\n") {
			t.Errorf("no %q in\n%s", want, self)
		}
	}
}

// A derived entry whose pass-through operand turns out a histogram fails as
// a rule under log does: logged once for the target, and its recovery logged
// once the operand is a gauge again and the entry makes its series.
func TestADerivedEntryFailureIsLoggedAndRecovers(t *testing.T) {
	out := testutil.CaptureLogs(t)
	var histogram atomic.Bool
	histogram.Store(true)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if histogram.Load() {
			_, _ = w.Write([]byte("# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"+Inf\"} 1\nlatency_seconds_sum 1\nlatency_seconds_count 1\n"))
			return
		}
		_, _ = w.Write([]byte("# TYPE latency_seconds gauge\nlatency_seconds 2\n"))
	}))
	t.Cleanup(target.Close)
	path := testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: node\n    request:\n      type: http\n    transform:\n      type: prometheus\n    derived:\n      - name: latency_twice\n        sum: [latency_seconds, latency_seconds]\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, path, slog.Default()), "python3", slog.Default())
	probe(t, server, target.URL, "node")
	probe(t, server, target.URL, "node")
	histogram.Store(false)
	if body := probe(t, server, target.URL, "node").Body.String(); !strings.Contains(body, "latency_twice 4\n") {
		t.Fatalf("the recovered entry:\n%s", body)
	}
	failed, recovered := 0, 0
	for _, record := range logRecords(t, out) {
		if record["metric"] != "latency_twice" {
			continue
		}
		switch record["msg"] {
		case "metric extraction failed":
			failed++
		case "metric extraction recovered":
			recovered++
		}
	}
	if failed != 1 || recovered != 1 {
		t.Fatalf("%d failures and %d recoveries logged, want 1 and 1:\n%s", failed, recovered, out.String())
	}
}
