//go:build !select_request_types || request_type_grpc

package exporter

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// With the watch on, a descriptor file of the configuration in force that
// changes reloads the configuration, and the static target file whose
// target's message is checked against it. The definitions did not change,
// so the reload keeps what a reload of an unchanged configuration keeps: the
// cached result of the collector, which goes on answering, what the failure
// log remembers of it, and its target's place in the schedule.
func TestAReloadForAChangedDescriptorFileKeepsTheCollectorsState(t *testing.T) {
	upstream := grpctest.Start(t, grpctest.Options{Answer: queueAnswer})
	dir := t.TempDir()
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
	const message = `{"queue": "orders", "include_shards": true}`
	configPath := testutil.WriteIn(t, dir, "config.yaml", "collectors:\n  - name: queue_stats\n    request:\n      type: grpc\n      rpc: acme.queue.v1.QueueService/GetStats\n      message: '"+message+"'\n"+
		"      descriptors: protoset\n      protoset_file: "+set+"\n    cache:\n      ttl: 1h\n    transform:\n      type: jq\n    metrics:\n      - name: queue_total\n        expression: .total\n")
	targetsPath := testutil.WriteIn(t, dir, "targets.yaml", "interval: 1h\ntargets:\n  - name: orders\n    collector: queue_stats\n    target: "+upstream.Addr+"\n    request:\n      message: '"+message+"'\n")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadStaticTargets(targetsPath)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	manager := config.NewManager(cfg, configPath, slog.New(slog.NewJSONHandler(logs, nil)))
	manager.SetPythonPath("python3")
	manager.SetTargets(targetsPath, file)
	// The watch is on from here, which stamps the descriptor set as it is
	// now; its loop is started below, once the set has been replaced.
	manager.SetWatchInterval(time.Millisecond)
	server := NewServer(manager, "python3", testutil.QuietLogger(t))

	// The collector has a cached result, a remembered failure and a target
	// with a place in the schedule.
	server.scrapeStaticTargets(t.Context(), 0)
	if calls := len(upstream.Calls()); calls != 1 {
		t.Fatalf("the first scrape made %d calls", calls)
	}
	server.failures.failed(testutil.QuietLogger(t), slog.LevelError, staticTargetKey("queue_stats", "orders"), "static target scrape failed", "fetch", context.DeadlineExceeded)
	held := func() (cached, remembered int) {
		server.cache.mu.Lock()
		for _, entry := range server.cache.entries {
			if entry.collector == "queue_stats" {
				cached++
			}
		}
		server.cache.mu.Unlock()
		server.failures.mu.Lock()
		defer server.failures.mu.Unlock()
		for _, st := range server.failures.entries {
			if st.key.collector == "queue_stats" {
				remembered++
			}
		}
		return cached, remembered
	}
	if cached, remembered := held(); cached != 1 || remembered != 1 {
		t.Fatalf("before the reload: %d cached results and %d remembered failures, want one of each", cached, remembered)
	}
	before := server.followedInForce()
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	schedule.planFollowed(before.config, staticTargetsOf(before.targets), before, now)
	place := schedule.states["orders"]

	// The descriptor set is replaced by one that says the same, and the
	// watch's next tick is waited for by what it does.
	raw, err := os.ReadFile(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(set, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(set, later, later); err != nil {
		t.Fatal(err)
	}
	installed := make(chan struct{}, 1)
	manager.OnInstall(func() {
		select {
		case installed <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { manager.ReloadLoop(ctx); close(stopped) }()
	select {
	case <-installed:
	case <-time.After(30 * time.Second):
		t.Error("the watch did not reload for the changed descriptor set")
	}
	cancel()
	<-stopped
	for _, want := range []string{`"msg":"configuration reloaded","trigger":"watch"`, `"msg":"static targets reloaded","trigger":"watch"`} {
		if strings.Count(logs.String(), want) != 1 {
			t.Fatalf("the watch did not log %s once:\n%s", want, logs)
		}
	}

	after := server.followedInForce()
	if after.config == before.config || after.targets == before.targets || after.generation != before.generation+1 {
		t.Fatalf("the reload was followed as generation %d after %d, with another configuration: %v, and another target file: %v", after.generation, before.generation, after.config != before.config, after.targets != before.targets)
	}
	if cached, remembered := held(); cached != 1 || remembered != 1 {
		t.Fatalf("after the reload: %d cached results and %d remembered failures, want the one of each it had", cached, remembered)
	}
	server.scrapeStaticTargets(t.Context(), 0)
	if calls := len(upstream.Calls()); calls != 1 {
		t.Fatalf("after the reload the target was called again, %d calls in all: its cached result was dropped", calls)
	}
	schedule.planFollowed(after.config, staticTargetsOf(after.targets), after, now.Add(time.Minute))
	if schedule.states["orders"] != place {
		t.Fatal("the reload started the static target again in the schedule")
	}
}
