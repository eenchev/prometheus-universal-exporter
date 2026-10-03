//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// resourceServer builds a server whose configuration asks for the resource
// metrics, or does not.
func resourceServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("example", "text")},
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{ResourceMetrics: enabled}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

// goFamilies are published on every platform the exporter builds for, because
// they come from the runtime rather than from the operating system.
var goFamilies = []string{
	"go_info",
	"go_goroutines",
	"go_threads",
	"go_sched_gomaxprocs_threads",
	"go_memstats_alloc_bytes",
	"go_memstats_alloc_bytes_total",
	"go_memstats_sys_bytes",
	"go_memstats_mallocs_total",
	"go_memstats_frees_total",
	"go_memstats_heap_alloc_bytes",
	"go_memstats_heap_sys_bytes",
	"go_memstats_heap_idle_bytes",
	"go_memstats_heap_inuse_bytes",
	"go_memstats_heap_released_bytes",
	"go_memstats_heap_objects",
	"go_memstats_stack_inuse_bytes",
	"go_memstats_stack_sys_bytes",
	"go_memstats_gc_sys_bytes",
	"go_memstats_other_sys_bytes",
	"go_memstats_next_gc_bytes",
	"go_memstats_last_gc_time_seconds",
	"go_gc_duration_seconds_count",
	"go_gc_duration_seconds_sum",
}

func TestResourceMetricsAreAbsentByDefault(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("example", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Web.SelfMetrics.ResourceMetrics {
		t.Fatal("resource metrics must be opt-in")
	}

	exposition := selfMetrics(t, resourceServer(t, false))
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, "go_") || strings.HasPrefix(line, "process_") ||
			strings.HasPrefix(line, "# HELP go_") || strings.HasPrefix(line, "# HELP process_") {
			t.Fatalf("resource metrics appeared without being configured: %q", line)
		}
	}
}

func TestResourceMetricsArePublishedWhenEnabled(t *testing.T) {
	exposition := selfMetrics(t, resourceServer(t, true))
	for _, name := range goFamilies {
		if !strings.Contains(exposition, "\n"+name+"{") && !strings.Contains(exposition, "\n"+name+" ") {
			t.Errorf("%s is missing:\n%s", name, testutil.FirstLines(exposition, 5))
		}
	}
	// The version label is what makes go_info useful, and it has to be this
	// binary's runtime rather than a build-time constant.
	if want := fmt.Sprintf(`go_info{version=%q} 1`, runtime.Version()); !strings.Contains(exposition, want) {
		t.Errorf("expected %s", want)
	}
}

// The numbers have to be the real ones. A series that is present and always
// zero is worse than an absent one, because an alert on it never fires.
func TestResourceMetricValuesAreReal(t *testing.T) {
	exposition := selfMetrics(t, resourceServer(t, true))
	for _, name := range []string{
		"go_goroutines",
		"go_threads",
		"go_sched_gomaxprocs_threads",
		"go_memstats_sys_bytes",
		"go_memstats_heap_inuse_bytes",
		"go_memstats_mallocs_total",
	} {
		if got := metricValue(t, exposition, name); got <= 0 {
			t.Errorf("%s=%v, want a positive value from the live runtime", name, got)
		}
	}
	if got := metricValue(t, exposition, "go_goroutines"); got < 1 {
		t.Errorf("go_goroutines=%v while this test is running", got)
	}
}

// The process series are read from /proc, so they appear where /proc does and
// are simply absent elsewhere rather than being faked from runtime numbers that
// measure something else.
func TestProcessMetricsFollowTheOperatingSystem(t *testing.T) {
	metrics := processMetrics()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		if len(metrics) != 0 {
			t.Fatalf("no /proc, but %d process metrics were published", len(metrics))
		}
		t.Skip("no /proc on this platform")
	}
	names := map[string]bool{}
	for _, metric := range metrics {
		names[metric.Name] = true
	}
	for _, want := range []string{
		"process_cpu_seconds_total",
		"process_virtual_memory_bytes",
		"process_resident_memory_bytes",
		"process_open_fds",
		"process_start_time_seconds",
	} {
		if !names[want] {
			t.Errorf("%s is missing on a platform with /proc", want)
		}
	}
	exposition := selfMetrics(t, resourceServer(t, true))
	if got := metricValue(t, exposition, "process_resident_memory_bytes"); got <= 0 {
		t.Errorf("process_resident_memory_bytes=%v, want the real resident size", got)
	}
}

// Publishing two sets of metrics into one exposition must not declare a family
// twice; a second HELP or TYPE for the same name invalidates the whole document.
func TestResourceMetricsDeclareEachFamilyOnce(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("example", "text")},
		Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{
			Verbose:         true,
			ResourceMetrics: true,
		}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())

	seen := map[string]int{}
	for _, line := range strings.Split(selfMetrics(t, server), "\n") {
		if !strings.HasPrefix(line, "# HELP ") && !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		seen[fields[1]+" "+fields[2]]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%q is declared %d times", key, count)
		}
	}
}

// Like every other self-metrics setting, this one is configuration rather than
// a flag, so a reload turns it on and off.
func TestResourceMetricsFollowTheConfiguration(t *testing.T) {
	quiet := &model.Config{Collectors: []model.Collector{testutil.Collector("toggled", "text")}}
	if err := config.Validate(quiet); err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(quiet, "", slog.Default())
	server := NewServer(manager, "python3", slog.Default())
	if strings.Contains(selfMetrics(t, server), "go_goroutines") {
		t.Fatal("resource metrics appeared while they were off")
	}

	loud := &model.Config{
		Collectors: []model.Collector{testutil.Collector("toggled", "text")},
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{ResourceMetrics: true}},
	}
	if err := config.Validate(loud); err != nil {
		t.Fatal(err)
	}
	installConfig(server, loud)
	if !strings.Contains(selfMetrics(t, server), "go_goroutines") {
		t.Fatal("resource metrics did not appear after the configuration enabled them")
	}

	installConfig(server, quiet)
	if strings.Contains(selfMetrics(t, server), "go_goroutines") {
		t.Fatal("turning them off must drop them rather than leave them exposed")
	}
}

// The metrics delivered over OTLP are the same set, so a dashboard built on
// either source reads the same.
func TestResourceMetricsReachTheOTLPSet(t *testing.T) {
	set := resourceServer(t, true).selfMetricSet()
	names := map[string]bool{}
	for _, metric := range set.Metrics {
		names[metric.Name] = true
	}
	for _, want := range []string{"go_goroutines", "go_memstats_heap_inuse_bytes", "go_info"} {
		if !names[want] {
			t.Errorf("%s is missing from the OTLP self-metric set", want)
		}
	}

	quiet := resourceServer(t, false).selfMetricSet()
	for _, metric := range quiet.Metrics {
		if strings.HasPrefix(metric.Name, "go_") || strings.HasPrefix(metric.Name, "process_") {
			t.Errorf("%s reached the OTLP set while resource metrics were off", metric.Name)
		}
	}
}
