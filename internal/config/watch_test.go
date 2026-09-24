package config

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const watchConfigTemplate = "collectors:\n  - name: watched\n    request:\n      type: http\n    transform:\n      type: regex\n" +
	"    metrics:\n      - name: %s\n        expression: 'value=(\\d+)'\n"

func writeWatchedConfig(t *testing.T, path, metric string) {
	t.Helper()
	document := strings.Replace(watchConfigTemplate, "%s", metric, 1)
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
}

func watchedManager(t *testing.T) (*Manager, string) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	writeWatchedConfig(t, path, "first_value")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, path, slog.Default())
	manager.SetPythonPath("python3")
	return manager, path
}

// activeMetric names the metric the running configuration extracts, which is
// how these tests observe whether a reload happened.
func activeMetric(m *Manager) string { return m.Get().Collectors[0].Metrics[0].Name }

func waitForMetric(t *testing.T, m *Manager, want string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if activeMetric(m) == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return activeMetric(m) == want
}

func TestWatchIsDisabledByDefault(t *testing.T) {
	manager, path := watchedManager(t)
	if manager.WatchEnabled() {
		t.Fatal("the configuration watch must be opt-in")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { manager.ReloadLoop(ctx); close(done) }()

	// A disabled watch returns immediately rather than idling in a ticker.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReloadLoop should return at once when the watch is disabled")
	}

	writeWatchedConfig(t, path, "second_value")
	time.Sleep(100 * time.Millisecond)
	if got := activeMetric(manager); got != "first_value" {
		t.Fatalf("configuration changed to %q without the watch enabled", got)
	}
}

func TestWatchReloadsTheConfigurationWhenEnabled(t *testing.T) {
	manager, path := watchedManager(t)
	manager.SetWatchInterval(10 * time.Millisecond)
	if !manager.WatchEnabled() {
		t.Fatal("SetWatchInterval should enable the watch")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.ReloadLoop(ctx)

	writeWatchedConfig(t, path, "second_value")
	if !waitForMetric(t, manager, "second_value", 5*time.Second) {
		t.Fatalf("the watch did not pick up the change; active metric is %q", activeMetric(manager))
	}
}

func TestWatchStopsWithTheContext(t *testing.T) {
	manager, path := watchedManager(t)
	manager.SetWatchInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.ReloadLoop(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReloadLoop should return when its context is cancelled")
	}

	writeWatchedConfig(t, path, "second_value")
	time.Sleep(100 * time.Millisecond)
	if got := activeMetric(manager); got != "first_value" {
		t.Fatalf("configuration reloaded to %q after the watch stopped", got)
	}
}

// A watch must not weaken the reload rules: an invalid configuration is still
// rejected with the previous one left active.
func TestWatchStillRejectsAnInvalidConfiguration(t *testing.T) {
	manager, path := watchedManager(t)
	manager.SetWatchInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.ReloadLoop(ctx)

	if err := os.WriteFile(path, []byte("collectors:\n  - name: bad-name\n"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := activeMetric(manager); got != "first_value" {
		t.Fatalf("an invalid configuration was activated: %q", got)
	}

	writeWatchedConfig(t, path, "third_value")
	if !waitForMetric(t, manager, "third_value", 5*time.Second) {
		t.Fatalf("a later valid configuration should still load; active metric is %q", activeMetric(manager))
	}
}

func TestWatchReloadsTheStaticTargetFile(t *testing.T) {
	dir := t.TempDir()
	configPath := dir + "/config.yaml"
	document := "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n" +
		"collectors:\n  - name: watched\n    request:\n      type: http\n    transform:\n      type: regex\n" +
		"    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(configPath, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	targetsPath := dir + "/targets.yaml"
	if err := os.WriteFile(targetsPath, []byte("interval: 1m\ntargets:\n  - name: one\n    collector: watched\n    target: http://a.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(targetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, configPath, slog.Default())
	manager.SetPythonPath("python3")
	manager.SetTargets(targetsPath, file)
	manager.SetWatchInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.ReloadLoop(ctx)

	if err := os.WriteFile(targetsPath, []byte("interval: 1m\ntargets:\n  - name: two\n    collector: watched\n    target: http://b.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if targets := manager.StaticTargets(); len(targets) == 1 && targets[0].Name == "two" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the watch did not reload the target file; targets are %+v", manager.StaticTargets())
}

func TestWatchIntervalIsHonoured(t *testing.T) {
	manager, _ := watchedManager(t)
	if manager.WatchEnabled() {
		t.Fatal("the watch should start disabled")
	}
	for _, interval := range []time.Duration{0, -time.Second} {
		manager.SetWatchInterval(interval)
		if manager.WatchEnabled() {
			t.Fatalf("a %s interval must leave the watch disabled", interval)
		}
	}
	manager.SetWatchInterval(DefaultWatchInterval)
	if !manager.WatchEnabled() {
		t.Fatal("a positive interval must enable the watch")
	}
	if DefaultWatchInterval != 60*time.Second {
		t.Fatalf("DefaultWatchInterval=%s, want the documented 60s", DefaultWatchInterval)
	}
}

// A watch reload and a triggered one at once do not interleave: each loads
// and installs the whole configuration under reloadMu.
func TestWatchAndTriggeredReloadsAreSerialized(t *testing.T) {
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", testutil.CollectorsDocument("first"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, path, testutil.QuietLogger(t))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = manager.Reload(ReloadTriggerHTTP)
		}()
		go func() {
			defer wg.Done()
			// A newer modification time is a change for the watch to find.
			later := time.Now().Add(time.Duration(i+1) * time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Error(err)
			}
			manager.reloadConfig()
		}()
	}
	wg.Wait()
	if st := manager.Reloads.files[reloadFileConfig]; st.failures != 0 || st.successes == 0 {
		t.Fatalf("status=%+v", st)
	}
}
