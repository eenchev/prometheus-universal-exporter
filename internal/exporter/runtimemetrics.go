package exporter

import (
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Resource metrics are the familiar `go_` series every Go dashboard already
// knows, and the `process_` series beside them. They are built from the
// standard library rather than from prometheus/client_golang: this exporter
// renders its own exposition from a []Metric and keeps no registry, so pulling
// in a registry to gather a few dozen numbers would add a dependency and an
// adapter for no gain. The names and meanings match what client_golang
// publishes, which is the whole point of the prefix — an existing dashboard or
// alert should work unchanged.
//
// They are on unless web.self_metrics.resource_metrics_enabled switches them
// off. Nothing here stops the world: the memory statistics come from
// runtime/metrics and the collection pauses from runtime/debug.ReadGCStats,
// never from runtime.ReadMemStats, which pauses every goroutine to fill its
// struct and is what kept these off by default while the exporter used it.

// resourceMetrics reports whether the running configuration asks for them.
func (s *Server) resourceMetrics() bool {
	return s.manager.Get().Web.SelfMetrics.ResourceMetricsEnabled()
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

// runtimeSeries are the go_ series read from runtime/metrics. Each is the sum
// of its sources, which is how client_golang's Go collector rebuilds the
// MemStats fields from runtime/metrics for Go 1.17 and later (memStatsFromRM):
// heap_inuse is the live objects plus the unused space of the spans that hold
// them, heap_idle the free plus the released, and so on. Mallocs and frees both
// count the tiny allocations, as MemStats does, so that mallocs minus frees
// stays the number of live objects. gc_sys is the GC metadata other than the
// span and cache structures, and other_sys the runtime's other allocations
// without the profiling buckets, again as in client_golang, which publishes
// those structures as series of their own that this exporter does not.
//
// The sources are asked for by name and a series is left out when the Go
// release in use does not describe one of them: a sum with a part missing
// would be a wrong number under a known name, which is worse than no series.
var runtimeSeries = []struct {
	name    string
	help    string
	typ     model.MetricType
	sources []string
}{
	{"go_memstats_alloc_bytes", "Number of bytes allocated in heap and currently in use.", model.GaugeMetricType, []string{"/memory/classes/heap/objects:bytes"}},
	{"go_memstats_alloc_bytes_total", "Total number of bytes allocated in heap until now, including released memory.", model.CounterMetricType, []string{"/gc/heap/allocs:bytes"}},
	{"go_memstats_sys_bytes", "Number of bytes obtained from system.", model.GaugeMetricType, []string{"/memory/classes/total:bytes"}},
	{"go_memstats_mallocs_total", "Total number of heap objects allocated, both live and gc-ed.", model.CounterMetricType, []string{"/gc/heap/allocs:objects", "/gc/heap/tiny/allocs:objects"}},
	{"go_memstats_frees_total", "Total number of heap objects frees.", model.CounterMetricType, []string{"/gc/heap/frees:objects", "/gc/heap/tiny/allocs:objects"}},

	{"go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and currently in use.", model.GaugeMetricType, []string{"/memory/classes/heap/objects:bytes"}},
	{"go_memstats_heap_sys_bytes", "Number of heap bytes obtained from system.", model.GaugeMetricType, []string{"/memory/classes/heap/objects:bytes", "/memory/classes/heap/unused:bytes", "/memory/classes/heap/free:bytes", "/memory/classes/heap/released:bytes"}},
	{"go_memstats_heap_idle_bytes", "Number of heap bytes waiting to be used.", model.GaugeMetricType, []string{"/memory/classes/heap/free:bytes", "/memory/classes/heap/released:bytes"}},
	{"go_memstats_heap_inuse_bytes", "Number of heap bytes that are in use.", model.GaugeMetricType, []string{"/memory/classes/heap/objects:bytes", "/memory/classes/heap/unused:bytes"}},
	{"go_memstats_heap_released_bytes", "Number of heap bytes released to OS.", model.GaugeMetricType, []string{"/memory/classes/heap/released:bytes"}},
	{"go_memstats_heap_objects", "Number of currently allocated objects.", model.GaugeMetricType, []string{"/gc/heap/objects:objects"}},

	{"go_memstats_stack_inuse_bytes", "Number of bytes obtained from system for stack allocator in non-CGO environments.", model.GaugeMetricType, []string{"/memory/classes/heap/stacks:bytes"}},
	{"go_memstats_stack_sys_bytes", "Number of bytes obtained from system for stack allocator.", model.GaugeMetricType, []string{"/memory/classes/heap/stacks:bytes", "/memory/classes/os-stacks:bytes"}},
	{"go_memstats_gc_sys_bytes", "Number of bytes used for garbage collection system metadata.", model.GaugeMetricType, []string{"/memory/classes/metadata/other:bytes"}},
	{"go_memstats_other_sys_bytes", "Number of bytes used for other system allocations.", model.GaugeMetricType, []string{"/memory/classes/other:bytes"}},
	{"go_memstats_next_gc_bytes", "Number of heap bytes when next garbage collection will take place.", model.GaugeMetricType, []string{"/gc/heap/goal:bytes"}},

	// The CPU accounting the runtime keeps, under the names client_golang
	// derives from the same sources.
	{"go_cpu_classes_total_cpu_seconds_total", "Estimated total available CPU time for user Go code or the Go runtime, as defined by GOMAXPROCS.", model.CounterMetricType, []string{"/cpu/classes/total:cpu-seconds"}},
	{"go_cpu_classes_user_cpu_seconds_total", "Estimated total CPU time spent running user Go code.", model.CounterMetricType, []string{"/cpu/classes/user:cpu-seconds"}},
	{"go_cpu_classes_idle_cpu_seconds_total", "Estimated total available CPU time not spent executing any Go or Go runtime code.", model.CounterMetricType, []string{"/cpu/classes/idle:cpu-seconds"}},
	{"go_cpu_classes_gc_total_cpu_seconds_total", "Estimated total CPU time spent performing garbage collection.", model.CounterMetricType, []string{"/cpu/classes/gc/total:cpu-seconds"}},
	{"go_cpu_classes_scavenge_total_cpu_seconds_total", "Estimated total CPU time spent returning unneeded memory to the OS.", model.CounterMetricType, []string{"/cpu/classes/scavenge/total:cpu-seconds"}},
}

// runtimeReader holds what one scrape reads, so a scrape allocates neither the
// samples nor the pause history: ReadGCStats keeps reusing the slices it was
// given once they are large enough. Scrapes take turns under the mutex; both
// reads are quick and neither stops the world.
type runtimeReader struct {
	mu      sync.Mutex
	samples []metrics.Sample
	// sources[i] are the indices into samples that runtimeSeries[i] sums,
	// nil when the runtime does not describe one of them.
	sources [][]int
	gc      debug.GCStats
}

var (
	runtimeReaderOnce sync.Once
	sharedRuntime     *runtimeReader
)

// theRuntimeReader is built once: which names the runtime describes cannot
// change while the process runs.
func theRuntimeReader() *runtimeReader {
	runtimeReaderOnce.Do(func() { sharedRuntime = newRuntimeReader(metrics.All()) })
	return sharedRuntime
}

// newRuntimeReader keeps the sources described with a kind the reader can
// turn into a number, each once however many series sum it.
func newRuntimeReader(described []metrics.Description) *runtimeReader {
	known := make(map[string]bool, len(described))
	for _, d := range described {
		if d.Kind == metrics.KindUint64 || d.Kind == metrics.KindFloat64 {
			known[d.Name] = true
		}
	}
	r := &runtimeReader{sources: make([][]int, len(runtimeSeries))}
	index := map[string]int{}
	for i, series := range runtimeSeries {
		indices := make([]int, 0, len(series.sources))
		for _, name := range series.sources {
			if !known[name] {
				indices = nil
				break
			}
			at, seen := index[name]
			if !seen {
				at = len(r.samples)
				index[name] = at
				r.samples = append(r.samples, metrics.Sample{Name: name})
			}
			indices = append(indices, at)
		}
		r.sources[i] = indices
	}
	r.gc.PauseQuantiles = make([]time.Duration, len(gcQuantiles))
	return r
}

// gcQuantiles are the quantiles of go_gc_duration_seconds, the minimum,
// quartiles and maximum, as client_golang publishes them. ReadGCStats fills a
// PauseQuantiles of this length with exactly these.
var gcQuantiles = [...]float64{0, 0.25, 0.5, 0.75, 1}

// read appends the runtime series to out, reading every source with one
// metrics.Read.
func (r *runtimeReader) read(out []model.Metric) []model.Metric {
	r.mu.Lock()
	defer r.mu.Unlock()

	metrics.Read(r.samples)
	for i, series := range runtimeSeries {
		indices := r.sources[i]
		if indices == nil {
			continue
		}
		var value float64
		valid := true
		for _, at := range indices {
			switch v := r.samples[at].Value; v.Kind() {
			case metrics.KindUint64:
				value += float64(v.Uint64())
			case metrics.KindFloat64:
				value += v.Float64()
			default:
				valid = false
			}
		}
		if !valid {
			continue
		}
		out = append(out, model.Metric{Name: series.name, Help: series.help, Type: series.typ, Value: value})
	}

	debug.ReadGCStats(&r.gc)
	collections := uint64(max(r.gc.NumGC, 0)) // #nosec G115 -- not negative, as checked
	quantiles := make([]model.Quantile, len(gcQuantiles))
	for i, q := range gcQuantiles {
		quantiles[i] = model.Quantile{Quantile: q, Value: r.gc.PauseQuantiles[i].Seconds()}
	}
	return append(out,
		gauge("go_memstats_last_gc_time_seconds", "Number of seconds since 1970 of last garbage collection.", lastGCSeconds(r.gc.LastGC), nil),
		model.Metric{Name: "go_gc_duration_seconds", Help: "A summary of the wall-time pause (stop-the-world) duration in garbage collection cycles.", Type: model.SummaryMetricType,
			Summary: &model.Summary{Quantiles: quantiles, Count: collections, Sum: r.gc.PauseTotal.Seconds()}},
	)
}

// goMetrics are the runtime's own numbers.
func goMetrics() []model.Metric {
	out := make([]model.Metric, 0, 6+len(runtimeSeries))
	out = append(out,
		gauge("go_info", "Information about the Go environment.", 1, map[string]string{"version": runtime.Version()}),
		gauge("go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine()), nil),
		gauge("go_threads", "Number of OS threads created.", float64(threadCount()), nil),
		gauge("go_sched_gomaxprocs_threads", "The current runtime.GOMAXPROCS setting, or the number of operating system threads that can execute user-level Go code simultaneously.", float64(runtime.GOMAXPROCS(0)), nil),
	)
	return theRuntimeReader().read(out)
}

// lastGCSeconds reports 0 rather than the epoch when no collection has run yet
// (ReadGCStats then gives the Unix time 0), so a fresh process does not look
// as though it last collected in 1970.
func lastGCSeconds(last time.Time) float64 {
	nanos := last.UnixNano()
	if nanos <= 0 {
		return 0
	}
	return float64(nanos) / float64(time.Second)
}

// threadCount is what client_golang reports as go_threads.
func threadCount() int {
	if profile := pprof.Lookup("threadcreate"); profile != nil {
		return profile.Count()
	}
	return 0
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
