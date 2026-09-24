//go:build !select_request_types || request_type_grpc

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const grpcExporterConfig = `web:
  self_metrics:
    verbose: VERBOSE
collectors:
  - name: queue_stats
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": {{param_queue|json}}, "include_shards": true}'
      descriptors: reflection
    cache:
      ttl: CACHE
    transform:
      type: jq
    metrics:
      - name: queue_depth
        items: .shards[]
        expression: .depth
        labels:
          - name: shard
            expression: .id
      - name: queue_total
        expression: .total
      - name: queue_empty
        expression: .empty_count
  - name: plain
    request:
      type: http
    decoder:
      type: text
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'v=(\d+)'
`

func grpcExporter(t *testing.T, verbose bool, cache string) *Server {
	t.Helper()
	yaml := strings.NewReplacer("VERBOSE", map[bool]string{true: "true", false: "false"}[verbose], "CACHE", cache).Replace(grpcExporterConfig)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yaml))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
}

// queueAnswer answers GetStats with a shard at zero, which proto3 would
// leave out, and a 64-bit total, which the JSON mapping writes as a string.
func queueAnswer(_ context.Context, _, request string) (string, error) {
	if !strings.Contains(request, `"include_shards":true`) {
		return "", status.Error(codes.InvalidArgument, "include_shards not set")
	}
	return `{"shards": [{"id": "s1", "depth": 12}, {"id": "s2"}], "total": "9007199254740993"}`, nil
}

// A probe calls the method and maps the answer with jq: the empty shard is
// there with 0, the 64-bit total is read as a number, and the gRPC code
// gauge reads 0 for the grpc collector alone.
func TestAGRPCCollectorProbe(t *testing.T) {
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: queueAnswer})
	server := grpcExporter(t, true, "0s")
	recorder := probeOnce(t, server, "/probe?collector=queue_stats&param_queue=orders&target="+url.QueryEscape(upstream.Addr), nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, body)
	}
	for _, want := range []string{`queue_depth{shard="s1"} 12`, `queue_depth{shard="s2"} 0`, `queue_total 9.007199254740992e+15`, `queue_empty 0`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		"# HELP http_exporter_scrape_grpc_status_code " + selfMetricHelp["http_exporter_scrape_grpc_status_code"] + "\n# TYPE http_exporter_scrape_grpc_status_code gauge\n",
		`http_exporter_scrape_grpc_status_code{collector="queue_stats"} 0`,
		`http_exporter_scrape_http_status_code{collector="queue_stats"} 200`,
		`http_exporter_scrape_grpc_status_code{collector="queue_stats",http_method="POST",url="grpc://` + upstream.Addr + `/acme.queue.v1.QueueService/GetStats"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="plain"`) {
		t.Error("an http collector has a gRPC code")
	}
}

// Before the first call the gauge reads -1; an exporter without grpc
// collectors does not have it at all.
func TestTheGRPCCodeGaugeIsOnlyForGRPCCollectors(t *testing.T) {
	server := grpcExporter(t, false, "0s")
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} -1`) {
		t.Fatalf("%s", metrics)
	}
	plain := verboseServer(t, false, testutil.Collector("plain", "text"))
	if metrics := selfMetrics(t, plain); strings.Contains(metrics, "grpc_status_code") {
		t.Fatalf("the family is there without grpc collectors:\n%s", metrics)
	}
}

// A status other than OK fails the probe under the grpc stage, with the
// code in the answer and the log and in the gauge; a message that does not
// fit fails at the message stage, with no call made.
func TestGRPCProbeFailures(t *testing.T) {
	var denied atomic.Bool
	denied.Store(true)
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(ctx context.Context, m, r string) (string, error) {
		if denied.Load() {
			return "", status.Error(codes.PermissionDenied, "tenant may not read queues")
		}
		return queueAnswer(ctx, m, r)
	}})
	logs := testutil.CaptureLogs(t)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.NewReplacer("VERBOSE", "false", "CACHE", "0s").Replace(grpcExporterConfig)))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	probe := "/probe?collector=queue_stats&param_queue=orders&target=" + url.QueryEscape(upstream.Addr)
	recorder := probeOnce(t, server, probe, nil)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "collector queue_stats grpc failed: gRPC call failed: PERMISSION_DENIED: tenant may not read queues") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(logs.String(), `"grpc_code":"PERMISSION_DENIED"`) || !strings.Contains(logs.String(), `"stage":"grpc"`) {
		t.Fatalf("logs:\n%s", logs)
	}
	metrics := selfMetrics(t, server)
	if !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} 7`) || !strings.Contains(metrics, `http_exporter_scrape_http_status_code{collector="queue_stats"} 0`) {
		t.Fatalf("%s", metrics)
	}

	calls := len(upstream.Calls())
	// A message probe parameter replaces the collector's, placeholders and
	// all, so param_queue has nothing to fill.
	recorder = probeOnce(t, server, probe+"&message="+url.QueryEscape(`{"queue": 5}`), nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "the message probe parameter replaces request.message") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	recorder = probeOnce(t, server, "/probe?collector=queue_stats&target="+url.QueryEscape(upstream.Addr)+"&message="+url.QueryEscape(`{"queue": 5}`), nil)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "collector queue_stats message failed: request.message does not fit acme.queue.v1.GetStatsRequest") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if len(upstream.Calls()) != calls {
		t.Fatal("a message that does not fit was sent")
	}
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} -1`) {
		t.Fatalf("%s", metrics)
	}

	// A probe parameter another type owns is refused before any call.
	recorder = probeOnce(t, server, probe+"&path=/x", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `request.type is "grpc"`) {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	// So is a malformed target.
	recorder = probeOnce(t, server, "/probe?collector=queue_stats&param_queue=orders&target=http://q:1/x", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "has the scheme http://") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}

	denied.Store(false)
	if recorder = probeOnce(t, server, probe, nil); recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} 0`) {
		t.Fatalf("%s", metrics)
	}
}

// A Python script reads the same JSON.
func TestAGRPCCollectorWithPython(t *testing.T) {
	requirePython(t)
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: queueAnswer})
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", `collectors:
  - name: queue_py
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": "orders", "include_shards": true}'
      descriptors: reflection
    transform:
      type: python
      script: |
        for shard in data["shards"]:
            metric(name="queue_depth", value=int(shard["depth"]), labels={"shard": shard["id"]})
`))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := probeOnce(t, server, "/probe?collector=queue_py&target="+url.QueryEscape(upstream.Addr), nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `queue_depth{shard="s2"} 0`) {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
}

// Static targets bring their own message and metadata, which are part of
// their cache keys: two targets of one caching collector asking about
// different queues never read each other's result, and a probe asking what
// one of them asks shares its entry.
func TestGRPCStaticTargetsCacheApart(t *testing.T) {
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: queueAnswer})
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.NewReplacer("VERBOSE", "false", "CACHE", "1h").Replace(grpcExporterConfig)))
	if err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "orders", Collector: "queue_stats", Target: upstream.Addr, Request: model.TargetRequestConfig{Message: `{"queue": "orders", "include_shards": true}`}},
		{Name: "billing", Collector: "queue_stats", Target: upstream.Addr, Request: model.TargetRequestConfig{Message: `{"queue": "billing", "include_shards": true}`}},
		{Name: "billing_eu", Collector: "queue_stats", Target: upstream.Addr, Request: model.TargetRequestConfig{Message: `{"queue": "billing", "include_shards": true}`, Metadata: map[string]string{"x-region": "eu"}}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 10*time.Second)
	calls := upstream.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
	var regions int
	for _, call := range calls {
		if len(call.Metadata.Get("x-region")) == 1 {
			regions++
		}
	}
	if regions != 1 {
		t.Fatalf("x-region sent %d times", regions)
	}
	body := getStaticTargets(t, server, "/static-targets")
	if !strings.Contains(body, `queue_depth{shard="s1",static_target="billing_eu"} 12`) {
		t.Fatalf("%s", body)
	}
	// Scraped again, every one is answered from the cache; so is a probe
	// with the same message.
	server.scrapeStaticTargets(t.Context(), 10*time.Second)
	message := url.QueryEscape(`{"queue": "orders", "include_shards": true}`)
	if recorder := probeOnce(t, server, "/probe?collector=queue_stats&target="+url.QueryEscape(upstream.Addr)+"&message="+message, nil); recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if got := len(upstream.Calls()); got != 3 {
		t.Fatalf("%d calls after the cache should have answered", got)
	}
}

// The collectors page says what a grpc collector's target is, and which
// parameters its message takes.
func TestTheCollectorsPageShowsAGRPCTarget(t *testing.T) {
	server := grpcExporter(t, false, "0s")
	body := probeOnce(t, server, "/collectors", nil).Body.String()
	if !strings.Contains(body, "host:port") || !strings.Contains(body, "param_queue") {
		t.Fatalf("%s", body)
	}
}
