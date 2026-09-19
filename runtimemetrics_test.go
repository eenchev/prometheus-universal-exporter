package main

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
)

// resourceServer builds a server whose configuration asks for the resource
// metrics, or does not.
func resourceServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	cfg := &Config{
		Collectors: []Collector{testCollector("example", "text")},
		Web:        WebConfig{SelfMetrics: SelfMetricsConfig{ResourceMetrics: enabled}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
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
	cfg := &Config{Collectors: []Collector{testCollector("example", "text")}}
	if err := cfg.Validate(); err != nil {
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
			t.Errorf("%s is missing:\n%s", name, firstLines(exposition, 5))
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

// The CPU classes come from runtime/metrics by name. A Go release that drops or
// renames one must make the series disappear rather than publish a zero.
func TestCPUClassMetricsComeFromTheRuntime(t *testing.T) {
	published := map[string]bool{}
	for _, metric := range cpuClassMetrics() {
		published[metric.Name] = true
	}
	if len(published) == 0 {
		t.Skip("this Go release publishes none of the CPU classes")
	}
	for _, want := range cpuClassSamples {
		if !published[want.metricName] {
			t.Logf("%s is not published by %s", want.metricName, runtime.Version())
		}
	}
	// Whatever is published must be a counter: these only ever grow.
	for _, metric := range cpuClassMetrics() {
		if metric.Type != CounterMetricType {
			t.Errorf("%s has type %v, want a counter", metric.Name, metric.Type)
		}
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

// /proc/self/stat cannot be split on spaces: the second field is the executable
// name in parentheses and may contain them.
func TestStatFieldsHandleACommandNameWithSpaces(t *testing.T) {
	tests := []struct {
		name  string
		stat  string
		pid   string
		comm  string
		third string
	}{
		{"plain", "42 (exporter) S 1 42", "42", "exporter", "S"},
		{"spaces", "42 (my exporter) S 1 42", "42", "my exporter", "S"},
		{"parenthesis inside", "42 (odd (name)) S 1 42", "42", "odd (name)", "S"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := statFields(test.stat)
			if len(fields) < 3 {
				t.Fatalf("parsed %v", fields)
			}
			if fields[0] != test.pid || fields[1] != test.comm || fields[2] != test.third {
				t.Fatalf("parsed %q as %q / %q / %q", test.stat, fields[0], fields[1], fields[2])
			}
		})
	}
	if fields := statFields("no parentheses here"); fields != nil {
		t.Fatalf("a malformed line should parse to nothing, got %v", fields)
	}
}

// Publishing two sets of metrics into one exposition must not declare a family
// twice; a second HELP or TYPE for the same name invalidates the whole document.
func TestResourceMetricsDeclareEachFamilyOnce(t *testing.T) {
	cfg := &Config{
		Collectors: []Collector{testCollector("example", "text")},
		Web: WebConfig{SelfMetrics: SelfMetricsConfig{
			Verbose:         true,
			ResourceMetrics: true,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

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
	quiet := &Config{Collectors: []Collector{testCollector("toggled", "text")}}
	if err := quiet.Validate(); err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(quiet, "", slog.Default())
	server := NewServer(manager, "python3", slog.Default())
	if strings.Contains(selfMetrics(t, server), "go_goroutines") {
		t.Fatal("resource metrics appeared while they were off")
	}

	loud := &Config{
		Collectors: []Collector{testCollector("toggled", "text")},
		Web:        WebConfig{SelfMetrics: SelfMetricsConfig{ResourceMetrics: true}},
	}
	if err := loud.Validate(); err != nil {
		t.Fatal(err)
	}
	manager.current.Store(loud)
	if !strings.Contains(selfMetrics(t, server), "go_goroutines") {
		t.Fatal("resource metrics did not appear after the configuration enabled them")
	}

	manager.current.Store(quiet)
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
