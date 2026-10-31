//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// resourceServer builds a server whose configuration asks for the resource
// metrics, or does not.
func resourceServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("example", "text")},
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{ResourceMetrics: new(enabled)}},
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

// They are on without being asked for, and resource_metrics_enabled: false
// takes every one of them away.
func TestResourceMetricsArePublishedByDefault(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("example", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Web.SelfMetrics.ResourceMetricsEnabled() {
		t.Fatal("resource metrics must be on unless switched off")
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	if exposition := selfMetrics(t, server); !strings.Contains(exposition, "\ngo_goroutines ") {
		t.Fatalf("the default configuration publishes no go_goroutines:\n%s", testutil.FirstLines(exposition, 5))
	}

	exposition := selfMetrics(t, resourceServer(t, false))
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, "go_") || strings.HasPrefix(line, "process_") ||
			strings.HasPrefix(line, "# HELP go_") || strings.HasPrefix(line, "# HELP process_") {
			t.Fatalf("resource metrics appeared while switched off: %q", line)
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

// allocationSink keeps the allocations of TestRuntimeMetricsHangTogether
// reachable, so the compiler cannot put them on the stack.
var allocationSink [][]byte

// The memory series are sums of runtime/metrics classes, so they must relate
// as the MemStats fields they stand for do, and the counters must count.
func TestRuntimeMetricsHangTogether(t *testing.T) {
	values := func() map[string]float64 {
		out := map[string]float64{}
		for _, metric := range goMetrics() {
			if metric.Type != model.SummaryMetricType {
				out[metric.Name] = metric.Value
			}
		}
		return out
	}
	before := values()
	for name, value := range before {
		if value < 0 {
			t.Errorf("%s=%v is negative", name, value)
		}
	}
	for _, name := range goFamilies {
		if _, found := before[name]; !found && !strings.HasPrefix(name, "go_gc_duration_seconds") {
			t.Errorf("%s is not published", name)
		}
	}
	for _, pair := range [][2]string{
		{"go_memstats_heap_inuse_bytes", "go_memstats_heap_sys_bytes"},
		{"go_memstats_heap_idle_bytes", "go_memstats_heap_sys_bytes"},
		{"go_memstats_heap_released_bytes", "go_memstats_heap_idle_bytes"},
		{"go_memstats_heap_alloc_bytes", "go_memstats_heap_inuse_bytes"},
		{"go_memstats_stack_inuse_bytes", "go_memstats_stack_sys_bytes"},
		{"go_memstats_heap_sys_bytes", "go_memstats_sys_bytes"},
		{"go_memstats_frees_total", "go_memstats_mallocs_total"},
	} {
		if before[pair[0]] > before[pair[1]] {
			t.Errorf("%s=%v is more than %s=%v", pair[0], before[pair[0]], pair[1], before[pair[1]])
		}
	}
	if got := before["go_memstats_heap_inuse_bytes"] + before["go_memstats_heap_idle_bytes"]; got != before["go_memstats_heap_sys_bytes"] {
		t.Errorf("heap in use plus idle is %v, heap from the system %v", got, before["go_memstats_heap_sys_bytes"])
	}
	if before["go_memstats_alloc_bytes"] != before["go_memstats_heap_alloc_bytes"] {
		t.Errorf("go_memstats_alloc_bytes=%v differs from go_memstats_heap_alloc_bytes=%v", before["go_memstats_alloc_bytes"], before["go_memstats_heap_alloc_bytes"])
	}
	for _, name := range []string{"go_memstats_next_gc_bytes", "go_memstats_heap_objects", "go_memstats_alloc_bytes"} {
		if before[name] <= 0 {
			t.Errorf("%s=%v, want a positive value", name, before[name])
		}
	}

	for range 64 {
		allocationSink = append(allocationSink, make([]byte, 64<<10))
	}
	after := values()
	allocationSink = nil
	if grown := after["go_memstats_alloc_bytes_total"] - before["go_memstats_alloc_bytes_total"]; grown < 64*64<<10 {
		t.Errorf("go_memstats_alloc_bytes_total grew by %v after allocating 4 MiB", grown)
	}
	if after["go_memstats_mallocs_total"] < before["go_memstats_mallocs_total"]+64 {
		t.Errorf("go_memstats_mallocs_total went from %v to %v over 64 allocations", before["go_memstats_mallocs_total"], after["go_memstats_mallocs_total"])
	}
}

// go_gc_duration_seconds is the summary client_golang publishes: the minimum,
// the quartiles and the maximum of the recent pauses, in that order, with the
// number of collections and their total pause.
func TestGCDurationIsASummaryWithQuantiles(t *testing.T) {
	summary := func() *model.Summary {
		for _, metric := range goMetrics() {
			if metric.Name == "go_gc_duration_seconds" {
				if metric.Type != model.SummaryMetricType || metric.Summary == nil {
					t.Fatalf("go_gc_duration_seconds is %v, want a summary", metric.Type)
				}
				return metric.Summary
			}
		}
		t.Fatal("go_gc_duration_seconds is not published")
		return nil
	}
	runtime.GC()
	before := summary()
	runtime.GC()
	after := summary()
	if after.Count < before.Count+1 || after.Count == 0 {
		t.Errorf("the count went from %d to %d over a collection", before.Count, after.Count)
	}
	if after.Sum < before.Sum {
		t.Errorf("the sum went down from %v to %v", before.Sum, after.Sum)
	}
	want := []float64{0, 0.25, 0.5, 0.75, 1}
	if len(after.Quantiles) != len(want) {
		t.Fatalf("quantiles %v, want %v", after.Quantiles, want)
	}
	for i, q := range after.Quantiles {
		if q.Quantile != want[i] || q.Value < 0 || (i > 0 && q.Value < after.Quantiles[i-1].Value) {
			t.Errorf("quantiles %v are not the five, ascending", after.Quantiles)
			break
		}
	}
	if after.Quantiles[len(want)-1].Value <= 0 {
		t.Errorf("after two collections the longest pause is %v", after.Quantiles[len(want)-1].Value)
	}

	exposition := selfMetrics(t, resourceServer(t, true))
	for _, quantile := range []string{"0", "0.25", "0.5", "0.75", "1"} {
		if want := `go_gc_duration_seconds{quantile="` + quantile + `"} `; !strings.Contains(exposition, "\n"+want) {
			t.Errorf("the exposition has no %s", want)
		}
	}
}

// A source the runtime does not describe takes away every series that sums
// it, rather than publishing the rest of the sum under the series' name.
func TestRuntimeSeriesNeedEverySource(t *testing.T) {
	var described []metrics.Description
	for _, d := range metrics.All() {
		if d.Name != "/memory/classes/heap/unused:bytes" {
			described = append(described, d)
		}
	}
	reader := newRuntimeReader(described)
	published := map[string]bool{}
	for _, metric := range reader.read(nil) {
		published[metric.Name] = true
	}
	for _, name := range []string{"go_memstats_heap_sys_bytes", "go_memstats_heap_inuse_bytes"} {
		if published[name] {
			t.Errorf("%s is published without one of its sources", name)
		}
	}
	for _, name := range []string{"go_memstats_heap_idle_bytes", "go_memstats_alloc_bytes", "go_memstats_last_gc_time_seconds"} {
		if !published[name] {
			t.Errorf("%s is missing although its sources are there", name)
		}
	}
	// With everything described, everything is published.
	for i, sources := range newRuntimeReader(metrics.All()).sources {
		if sources == nil {
			t.Errorf("%s: %s does not describe one of %v", runtimeSeries[i].name, runtime.Version(), runtimeSeries[i].sources)
		}
	}
}

// Scrapes that overlap share one reader and must not trip over its samples.
func TestRuntimeMetricsReadConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if len(goMetrics()) < len(goFamilies) {
					t.Error("a concurrent read lost series")
					return
				}
			}
		})
	}
	wg.Wait()
}

// Reading the resource metrics reuses its samples and the pause history, so
// a scrape of them allocates the series it returns and little else: not the
// 256 pauses ReadGCStats copies, nor the samples metrics.Read fills.
// Five allocations of about 3 KiB in all at the time of writing: the series,
// go_info's labels, the summary and its quantiles; ReadMemStats with a fresh
// slice of samples cost seven and 8 KiB.
func TestRuntimeMetricsAllocateLittle(t *testing.T) {
	if allocs := alloctest.AllocsAtMost(20, 6, func() { goMetrics() }); allocs > 6 {
		t.Errorf("a read of the runtime series allocates %v times, want at most 6", allocs)
	}
	if bytes := alloctest.BytesAtMost(20, 4096, func() { goMetrics() }); bytes > 4096 {
		t.Errorf("a read of the runtime series allocates %d bytes, want at most 4096", bytes)
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
			ResourceMetrics: new(true),
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
	quiet := &model.Config{
		Collectors: []model.Collector{testutil.Collector("toggled", "text")},
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{ResourceMetrics: new(false)}},
	}
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
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{ResourceMetrics: new(true)}},
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
