//go:build !select_request_types || request_type_grpc

package exporter

import (
	"context"
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
` + grpcPlainCollector

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

// A debug probe of a grpc collector lists the call, its code and the answer
// as JSON.
func TestADebugProbeOfAGRPCCollector(t *testing.T) {
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: queueAnswer})
	server := grpcExporter(t, false, "0s")
	server.SetProbeDebug(true)
	recorder := probeOnce(t, server, "/probe?debug=true&collector=queue_stats&param_queue=orders&target="+url.QueryEscape(upstream.Addr), nil)
	body := recorder.Body.String()
	for _, want := range []string{
		"1. POST grpc://" + upstream.Addr + "/acme.queue.v1.QueueService/GetStats -> grpc OK",
		"gRPC status 0", `"total":"9007199254740993"`, "grpc         ok", "queue_depth: 2",
		"A probe would have answered 200 with 4 series",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
}

// A call refused as UNAUTHENTICATED or PERMISSION_DENIED gets no stale
// result, as an HTTP 401 or 403 does not; UNAVAILABLE still does.
func TestAGRPCCallRefusedOnItsCredentialGetsNoStaleResult(t *testing.T) {
	for code, stale := range map[codes.Code]bool{codes.Unauthenticated: false, codes.PermissionDenied: false, codes.Unavailable: true} {
		t.Run(code.String(), func(t *testing.T) {
			var fail atomic.Bool
			upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(ctx context.Context, method, request string) (string, error) {
				if fail.Load() {
					return "", status.Error(code, "no")
				}
				return queueAnswer(ctx, method, request)
			}})
			yaml := strings.Replace(strings.NewReplacer("VERBOSE", "false", "CACHE", "1ms").Replace(grpcExporterConfig), "      ttl: 1ms\n", "      ttl: 1ms\n      stale_if_error: 1h\n", 1)
			cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yaml))
			if err != nil {
				t.Fatal(err)
			}
			server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
			path := "/probe?collector=queue_stats&param_queue=orders&target=" + url.QueryEscape(upstream.Addr)
			if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK {
				t.Fatalf("%d %s", r.Code, r.Body)
			}
			time.Sleep(5 * time.Millisecond)
			fail.Store(true)
			r := probeOnce(t, server, path, nil)
			if served := r.Code == http.StatusOK && strings.Contains(r.Body.String(), "queue_total"); served != stale {
				t.Fatalf("%s: stale served %v, want %v: %d %s", code, served, stale, r.Code, r.Body)
			}
		})
	}
}
