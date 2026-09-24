package exporter

import (
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Resource metrics are the familiar `go_` series every Go dashboard already
// knows, and the `process_` series beside them. They are built from the
// standard library rather than from prometheus/client_golang: this exporter
// renders its own exposition from a []Metric and keeps no registry, so pulling
// in a registry to gather sixteen numbers would add a dependency and an
// adapter for no gain. The names and meanings match what client_golang
// publishes, which is the whole point of the prefix — an existing dashboard or
// alert should work unchanged.
//
// They are opt-in through web.self_metrics.resource_metrics_enabled. Reading
// them is cheap but not free: runtime.ReadMemStats briefly stops the world, and
// an exporter scraped every few seconds by several Prometheis should not pay
// that unless somebody wants the numbers.

// resourceMetrics reports whether the running configuration asks for them.
func (s *Server) resourceMetrics() bool {
	return s.manager.Get().Web.SelfMetrics.ResourceMetrics
}

// runtimeMetrics returns the go_ and process_ series, or nothing when they are
// switched off.
func (s *Server) runtimeMetrics() []model.Metric {
	if !s.resourceMetrics() {
		return nil
	}
	out := goMetrics()
	return append(out, processMetrics()...)
}

func gauge(name, help string, value float64, labels map[string]string) model.Metric {
	return model.Metric{Name: name, Help: help, Type: model.GaugeMetricType, Value: value, Labels: labels}
}

func counter(name, help string, value float64) model.Metric {
	return model.Metric{Name: name, Help: help, Type: model.CounterMetricType, Value: value}
}

// goMetrics are the runtime's own numbers. Every one of these is available on
// every platform the exporter builds for.
func goMetrics() []model.Metric {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	out := []model.Metric{
		gauge("go_info", "Information about the Go environment.", 1, map[string]string{"version": runtime.Version()}),
		gauge("go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine()), nil),
		gauge("go_threads", "Number of OS threads created.", float64(threadCount()), nil),
		gauge("go_sched_gomaxprocs_threads", "The current runtime.GOMAXPROCS setting, or the number of operating system threads that can execute user-level Go code simultaneously.", float64(runtime.GOMAXPROCS(0)), nil),

		gauge("go_memstats_alloc_bytes", "Number of bytes allocated in heap and currently in use.", float64(mem.Alloc), nil),
		counter("go_memstats_alloc_bytes_total", "Total number of bytes allocated in heap until now, including released memory.", float64(mem.TotalAlloc)),
		gauge("go_memstats_sys_bytes", "Number of bytes obtained from system.", float64(mem.Sys), nil),
		counter("go_memstats_mallocs_total", "Total number of heap objects allocated, both live and gc-ed.", float64(mem.Mallocs)),
		counter("go_memstats_frees_total", "Total number of heap objects frees.", float64(mem.Frees)),

		gauge("go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and currently in use.", float64(mem.HeapAlloc), nil),
		gauge("go_memstats_heap_sys_bytes", "Number of heap bytes obtained from system.", float64(mem.HeapSys), nil),
		gauge("go_memstats_heap_idle_bytes", "Number of heap bytes waiting to be used.", float64(mem.HeapIdle), nil),
		gauge("go_memstats_heap_inuse_bytes", "Number of heap bytes that are in use.", float64(mem.HeapInuse), nil),
		gauge("go_memstats_heap_released_bytes", "Number of heap bytes released to OS.", float64(mem.HeapReleased), nil),
		gauge("go_memstats_heap_objects", "Number of currently allocated objects.", float64(mem.HeapObjects), nil),

		gauge("go_memstats_stack_inuse_bytes", "Number of bytes obtained from system for stack allocator in non-CGO environments.", float64(mem.StackInuse), nil),
		gauge("go_memstats_stack_sys_bytes", "Number of bytes obtained from system for stack allocator.", float64(mem.StackSys), nil),
		gauge("go_memstats_gc_sys_bytes", "Number of bytes used for garbage collection system metadata.", float64(mem.GCSys), nil),
		gauge("go_memstats_other_sys_bytes", "Number of bytes used for other system allocations.", float64(mem.OtherSys), nil),
		gauge("go_memstats_next_gc_bytes", "Number of heap bytes when next garbage collection will take place.", float64(mem.NextGC), nil),
		gauge("go_memstats_last_gc_time_seconds", "Number of seconds since 1970 of last garbage collection.", lastGCSeconds(mem), nil),

		// A summary, as client_golang publishes it, with its count and sum and
		// without the quantiles the runtime statistics cannot give.
		{Name: "go_gc_duration_seconds", Help: "A summary of the wall-time pause (stop-the-world) duration in garbage collection cycles.", Type: model.SummaryMetricType, Summary: &model.Summary{Count: uint64(mem.NumGC), Sum: float64(mem.PauseTotalNs) / float64(time.Second)}},
	}
	return append(out, cpuClassMetrics()...)
}

// lastGCSeconds reports 0 rather than the epoch when no collection has run yet,
// so a fresh process does not look as though it last collected in 1970.
func lastGCSeconds(mem runtime.MemStats) float64 {
	if mem.LastGC == 0 {
		return 0
	}
	return float64(mem.LastGC) / float64(time.Second)
}

// threadCount is what client_golang reports as go_threads.
func threadCount() int {
	if profile := pprof.Lookup("threadcreate"); profile != nil {
		return profile.Count()
	}
	return 0
}

// cpuClassSamples are the CPU accounting the runtime keeps, under the names
// client_golang derives from the same runtime/metrics sources. They are asked
// for by name and skipped when a Go release does not have them, so the exporter
// publishes what the runtime it was built with actually offers rather than a
// fixed list that a toolchain upgrade could invalidate.
var cpuClassSamples = []struct {
	runtimeName string
	metricName  string
	help        string
}{
	{"/cpu/classes/total:cpu-seconds", "go_cpu_classes_total_cpu_seconds_total", "Estimated total available CPU time for user Go code or the Go runtime, as defined by GOMAXPROCS."},
	{"/cpu/classes/user:cpu-seconds", "go_cpu_classes_user_cpu_seconds_total", "Estimated total CPU time spent running user Go code."},
	{"/cpu/classes/idle:cpu-seconds", "go_cpu_classes_idle_cpu_seconds_total", "Estimated total available CPU time not spent executing any Go or Go runtime code."},
	{"/cpu/classes/gc/total:cpu-seconds", "go_cpu_classes_gc_total_cpu_seconds_total", "Estimated total CPU time spent performing garbage collection."},
	{"/cpu/classes/scavenge/total:cpu-seconds", "go_cpu_classes_scavenge_total_cpu_seconds_total", "Estimated total CPU time spent returning unneeded memory to the OS."},
}

func cpuClassMetrics() []model.Metric {
	samples := make([]metrics.Sample, 0, len(cpuClassSamples))
	for _, want := range cpuClassSamples {
		samples = append(samples, metrics.Sample{Name: want.runtimeName})
	}
	metrics.Read(samples)

	out := make([]model.Metric, 0, len(samples))
	for i, sample := range samples {
		// A name this Go release does not know comes back as KindBad rather
		// than as an error, which is what makes skipping the right response.
		if sample.Value.Kind() != metrics.KindFloat64 {
			continue
		}
		out = append(out, counter(cpuClassSamples[i].metricName, cpuClassSamples[i].help, sample.Value.Float64()))
	}
	return out
}

// processMetrics are the operating system's view of the process. They are read
// from /proc, so they appear on Linux — where the exporter's container runs —
// and are simply absent elsewhere rather than being faked from runtime numbers
// that mean something different.
func processMetrics() []model.Metric {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return nil
	}
	fields := statFields(string(stat))
	// The documented layout: utime is field 14 and stime field 15, one-based,
	// after the comm field which may itself contain spaces.
	if len(fields) < 24 {
		return nil
	}
	ticks := float64(clockTicksPerSecond)
	utime := parseFloat(fields[13])
	stime := parseFloat(fields[14])
	vsize := parseFloat(fields[22])
	rss := parseFloat(fields[23]) * float64(os.Getpagesize())

	out := []model.Metric{
		counter("process_cpu_seconds_total", "Total user and system CPU time spent in seconds.", (utime+stime)/ticks),
		gauge("process_virtual_memory_bytes", "Virtual memory size in bytes.", vsize, nil),
		gauge("process_resident_memory_bytes", "Resident memory size in bytes.", rss, nil),
	}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		out = append(out, gauge("process_open_fds", "Number of open file descriptors.", float64(len(fds)), nil))
	}
	if started, ok := processStartSeconds(); ok {
		out = append(out, gauge("process_start_time_seconds", "Start time of the process since unix epoch in seconds.", started, nil))
	}
	return out
}

// clockTicksPerSecond is the value Linux reports process CPU time in. It is
// 100 on every architecture Go supports, and reading it properly needs cgo.
const clockTicksPerSecond = 100

// statFields splits /proc/self/stat, which cannot be split on spaces alone: the
// second field is the executable name in parentheses and may contain spaces.
func statFields(stat string) []string {
	start := strings.IndexByte(stat, '(')
	end := strings.LastIndexByte(stat, ')')
	if start < 0 || end < 0 || end < start {
		return nil
	}
	fields := []string{strings.TrimSpace(stat[:start]), stat[start+1 : end]}
	return append(fields, strings.Fields(stat[end+1:])...)
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// processStartSeconds converts the process start time, which /proc reports in
// clock ticks since boot, into a unix timestamp, from the boot time /proc/stat
// gives in whole seconds (btime), as client_golang does. The value is the same
// at every scrape: one worked out from the uptime and the time now would move
// by a fraction of a second between scrapes, and look like a restart to
// changes(process_start_time_seconds[1h]).
func processStartSeconds() (float64, bool) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	fields := statFields(string(stat))
	if len(fields) < 22 {
		return 0, false
	}
	sinceBoot := parseFloat(fields[21]) / float64(clockTicksPerSecond)
	boot, ok := bootTime()
	if !ok {
		return 0, false
	}
	return boot + sinceBoot, true
}

// bootTime is when the system booted, in unix seconds, from /proc/stat.
func bootTime() (float64, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(line, "btime "); found {
			if boot, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && boot > 0 {
				return boot, true
			}
		}
	}
	return 0, false
}
