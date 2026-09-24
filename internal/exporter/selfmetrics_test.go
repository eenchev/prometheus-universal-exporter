package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The self-metrics are rendered from one set (selfMetricSet), for /metrics,
// the dedicated self-metrics path and OTLP alike.

// Every family is one contiguous block, declared once with the type the set
// gives it, and a counter's name ends in _total; the text parses; and the set
// OTLP exports has the same families with the same types.
func TestSelfMetricsExpositionIsWellFormed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, true, testutil.Collector("first", "text"), testutil.Collector("second", "text"), pythonCollector("third", `metric(name="v", value=1)`))
	server.manager.Get().Web.SelfMetrics.ResourceMetrics = true
	// With OTLP on, so its export status families are checked too.
	server.manager.Get().OTLP = otlpConfig("http://otel.invalid:4318/v1/metrics")
	probeOnce(t, server, "/probe?collector=first&target="+target.URL, nil)
	probeOnce(t, server, "/probe?collector=second&target="+target.URL, nil)
	exposition := selfMetrics(t, server)
	if _, err := decode.ParsePrometheusText([]byte(exposition)); err != nil {
		t.Fatalf("the self-metrics do not parse: %v\n%s", err, exposition)
	}

	types := map[string]string{}
	finished := map[string]bool{}
	current := ""
	for _, line := range strings.Split(strings.TrimSpace(exposition), "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			if _, again := types[name]; again {
				t.Errorf("%s is declared twice", name)
			}
			types[name] = typ
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.FieldsFunc(line, func(r rune) bool { return r == '{' || r == ' ' })[0]
		family := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(name, suffix); ok && (types[base] == "histogram" || types[base] == "summary") {
				family = base
			}
		}
		if family != current {
			if finished[family] {
				t.Errorf("the samples of %s are split into more than one block", family)
			}
			finished[current] = true
			current = family
		}
		if _, declared := types[family]; !declared {
			t.Errorf("%s has samples but no TYPE line before them", family)
		}
	}
	for name, typ := range types {
		if typ == "counter" && !strings.HasSuffix(name, "_total") {
			t.Errorf("the counter %s does not end in _total", name)
		}
		if strings.HasSuffix(name, "_total") && typ != "counter" {
			t.Errorf("%s ends in _total but is a %s", name, typ)
		}
	}
	for _, d := range selfMetricDescriptors {
		if types[d.Name] != string(d.Type) {
			t.Errorf("%s is exposed as %q, want %q", d.Name, types[d.Name], d.Type)
		}
	}
	for _, m := range server.selfMetricSet().Metrics {
		if types[m.Name] != string(m.Type) {
			t.Errorf("%s is a %s over OTLP and a %q in the text", m.Name, m.Type, types[m.Name])
		}
	}
}

// The reload status reads successful from startup, turns and stays
// unsuccessful while a reload is rejected, and counts reloads by result. The
// scheduled target file is reported only when there is one.
func TestReloadStatusMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	good := testutil.CollectorsDocument("reloaded")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, path, testutil.QuietLogger(t))
	server := NewServer(manager, "python3", slog.Default())
	started := time.Now()

	expect := func(when string, want map[string]float64) float64 {
		t.Helper()
		exposition := selfMetrics(t, server)
		for series, value := range want {
			if got := seriesValue(t, exposition, series); got != value {
				t.Errorf("%s: %s = %v, want %v", when, series, got, value)
			}
		}
		if strings.Contains(exposition, `file="targets"`) {
			t.Errorf("%s: a target file is reported though none is configured", when)
		}
		return seriesValue(t, exposition, `http_exporter_config_last_reload_success_timestamp_seconds{file="config"}`)
	}
	loadedAt := expect("at startup", map[string]float64{
		`http_exporter_config_last_reload_successful{file="config"}`:         1,
		`http_exporter_config_reloads_total{file="config",result="success"}`: 0,
		`http_exporter_config_reloads_total{file="config",result="failure"}`: 0,
	})
	if loadedAt <= 0 || loadedAt > float64(started.Unix()+1) {
		t.Fatalf("startup timestamp %v", loadedAt)
	}

	if err := os.WriteFile(path, []byte("collectors: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.LastMod = time.Time{}
	manager.ReloadConfig()
	if got := expect("after a rejected reload", map[string]float64{
		`http_exporter_config_last_reload_successful{file="config"}`:         0,
		`http_exporter_config_reloads_total{file="config",result="success"}`: 0,
		`http_exporter_config_reloads_total{file="config",result="failure"}`: 1,
	}); got != loadedAt {
		t.Errorf("a rejected reload moved the success timestamp from %v to %v", loadedAt, got)
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.LastMod = time.Time{}
	manager.ReloadConfig()
	if got := expect("after a successful reload", map[string]float64{
		`http_exporter_config_last_reload_successful{file="config"}`:         1,
		`http_exporter_config_reloads_total{file="config",result="success"}`: 1,
		`http_exporter_config_reloads_total{file="config",result="failure"}`: 1,
	}); got <= loadedAt {
		t.Errorf("a successful reload did not move the success timestamp: %v", got)
	}
}

// With a scheduled target file, its reloads are reported under file="targets".
func TestReloadStatusCoversTheTargetFile(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	targets := filepath.Join(t.TempDir(), "targets.yaml")
	good := "targets:\n  - name: one\n    collector: text\n    target: http://a.example\n"
	if err := os.WriteFile(targets, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadTargets(targets)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets(targets, file)
	server := NewServer(manager, "python3", slog.Default())
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_config_last_reload_successful{file="targets"}`); got != 1 {
		t.Fatalf("targets at startup = %v", got)
	}
	if err := os.WriteFile(targets, []byte("targets:\n  - name: one\n    collector: missing\n    target: http://a.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.TargetsLastMod = time.Time{}
	manager.ReloadTargets()
	exposition := selfMetrics(t, server)
	if got := seriesValue(t, exposition, `http_exporter_config_last_reload_successful{file="targets"}`); got != 0 {
		t.Errorf("after a rejected target reload = %v", got)
	}
	if got := seriesValue(t, exposition, `http_exporter_config_reloads_total{file="targets",result="failure"}`); got != 1 {
		t.Errorf("target reload failures = %v", got)
	}
	// The configuration itself is unaffected.
	if got := seriesValue(t, exposition, `http_exporter_config_last_reload_successful{file="config"}`); got != 1 {
		t.Errorf("config = %v", got)
	}
}
