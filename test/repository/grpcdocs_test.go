//go:build !select_request_types || request_type_grpc

package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples in docs/GRPC.md load, and run against the queue service the
// page describes, answering what the page says they answer.
func TestGRPCDocumentationExamples(t *testing.T) {
	blocks := docBlocks(t, "docs/GRPC.md", "yaml")
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return `{"shards": [{"id": "s1", "depth": 12}, {"id": "s2"}], "total": "12"}`, nil
	}})
	probe := func(t *testing.T, cfg *model.Config, query string) string {
		t.Helper()
		server := exporter.NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+url.QueryEscape(upstream.Addr)+"&"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, recorder.Code, recorder.Body)
		}
		return recorder.Body.String()
	}
	load := func(t *testing.T, yaml string) *model.Config {
		t.Helper()
		cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yaml))
		if err != nil {
			t.Fatalf("%v\n%s", err, yaml)
		}
		return cfg
	}

	// The collector example: the empty shard is there, with 0.
	collector := docBlock(t, blocks, "collectors:\n  - name: queue_stats\n")
	cfg := load(t, collector)
	body := probe(t, cfg, "collector=queue_stats&param_queue=orders")
	for _, want := range []string{`queue_depth{shard="s1"} 12`, `queue_depth{shard="s2"} 0`, "queue_total 12"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	calls := upstream.Calls()
	if last := calls[len(calls)-1]; last.Request != `{"queue":"orders","include_shards":true}` || last.Metadata.Get("x-tenant")[0] != "default" {
		t.Fatalf("called with %s %v", last.Request, last.Metadata)
	}

	// The static targets example loads against it.
	targets := testutil.WriteIn(t, t.TempDir(), "static-targets.yaml", docBlock(t, blocks, "interval: 1m\n"))
	file, err := config.LoadStaticTargets(targets)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}

	// The descriptor set and .proto examples load with the files they name.
	dir := t.TempDir()
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
	grpctest.WriteSources(t, dir)
	protoset := strings.Replace(docBlock(t, blocks, "collectors:\n  - name: queue_stats_protoset\n"), "/etc/exporter/protos/queue.pb", set, 1)
	if body := probe(t, load(t, protoset), "collector=queue_stats_protoset"); !strings.Contains(body, "queue_total 12") {
		t.Fatalf("%s", body)
	}
	sources := strings.ReplaceAll(docBlock(t, blocks, "collectors:\n  - name: queue_stats_proto\n"), "/etc/exporter/protos", dir)
	if body := probe(t, load(t, sources), "collector=queue_stats_proto"); !strings.Contains(body, "queue_total 12") {
		t.Fatalf("%s", body)
	}

	// The health check needs no descriptors.
	health := load(t, docBlock(t, blocks, "collectors:\n  - name: grpc_health\n"))
	if body := probe(t, health, "collector=grpc_health"); !strings.Contains(body, "grpc_serving 1") {
		t.Fatalf("%s", body)
	}

	// The Python example, and the retry snippet in a collector.
	python := load(t, docBlock(t, blocks, "collectors:\n  - name: queue_py\n"))
	if python.Collectors[0].Transform.Type != "python" {
		t.Fatal("the Python example is not a python transform")
	}
	retry := strings.Replace(collector, "      descriptors: reflection\n", "      descriptors: reflection\n"+indent(docBlock(t, blocks, "retry:\n"), "      "), 1)
	if c := load(t, retry).Collectors[0]; strings.Join(c.Request.Retry.Codes, ",") != "UNAVAILABLE,RESOURCE_EXHAUSTED,ABORTED" || c.Request.Retry.Attempts != 2 {
		t.Fatalf("retry %+v", c.Request.Retry)
	}
}
