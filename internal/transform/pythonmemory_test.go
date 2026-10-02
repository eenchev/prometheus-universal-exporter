package transform

import (
	"context"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What counts against limits.max_script_memory, and that the thread which
// watches a worker's parent neither takes from it nor ends of it
// (pythonworker.go).

// workerStatus reads one field of a process's /proc status, such as VmSize
// or Threads, without its unit.
func workerStatus(t *testing.T, pid int, field string) int {
	t.Helper()
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, field+":"); ok {
			n, err := strconv.Atoi(strings.Fields(value)[0])
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no %s in the status of process %d", field, pid)
	return 0
}

// requireProcStatus skips a test that reads a worker's address space or
// threads where there is no /proc to read them from, and runs it without
// the tester's own MALLOC_ARENA_MAX, which the workers would be given.
func requireProcStatus(t *testing.T) {
	t.Helper()
	requirePython(t)
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc, and RLIMIT_AS is enforced on Linux")
	}
	unsetArenas(t)
}

// unsetArenas leaves MALLOC_ARENA_MAX unset until the test ends, whatever
// the tests were started with.
func unsetArenas(t *testing.T) {
	t.Helper()
	// Setenv restores the variable, or its absence, when the test ends.
	t.Setenv(pythonArenaVariable, "")
	if err := os.Unsetenv(pythonArenaVariable); err != nil {
		t.Fatal(err)
	}
}

// A worker is started with one malloc arena, MALLOC_ARENA_MAX=1 added to
// the exporter's own environment, and the operator's own setting of that
// variable stands. The environment given is not written to.
func TestPythonWorkersAreStartedWithOneMallocArena(t *testing.T) {
	own := []string{"PATH=/usr/bin", "HOME=/"}
	if got := pythonWorkerEnvironment(own); !slices.Equal(got, []string{"PATH=/usr/bin", "HOME=/", "MALLOC_ARENA_MAX=1"}) {
		t.Fatalf("%q", got)
	}
	if !slices.Equal(own, []string{"PATH=/usr/bin", "HOME=/"}) {
		t.Fatalf("the environment given was changed: %q", own)
	}
	set := []string{"MALLOC_ARENA_MAX=8", "PATH=/usr/bin"}
	if got := pythonWorkerEnvironment(set); !slices.Equal(got, set) {
		t.Fatalf("%q", got)
	}
	// A variable that only begins alike is another variable.
	if got := pythonWorkerEnvironment([]string{"MALLOC_ARENA_MAX_OLD=8"}); !slices.Equal(got, []string{"MALLOC_ARENA_MAX_OLD=8", "MALLOC_ARENA_MAX=1"}) {
		t.Fatalf("%q", got)
	}

	requirePython(t)
	unsetArenas(t)
	seen := workerCollector("arenas", `metric(name="arenas", value=int(os.environ.get("MALLOC_ARENA_MAX", "0")))`)
	set1, err := runWorkerScript(t, seen)
	if err != nil {
		t.Fatal(err)
	}
	if v := workerMetricValue(t, set1, "arenas"); v != 1 {
		t.Fatalf("a worker sees MALLOC_ARENA_MAX=%v, want 1", v)
	}
	t.Setenv(pythonArenaVariable, "8")
	usePythonPool(t)
	set8, err := runWorkerScript(t, seen)
	if err != nil {
		t.Fatal(err)
	}
	if v := workerMetricValue(t, set8, "arenas"); v != 8 {
		t.Fatalf("a worker of an exporter run with MALLOC_ARENA_MAX=8 sees %v", v)
	}
}

// The thread that watches a worker's parent made glibc reserve a second
// malloc arena, 64 MiB of address space: a ready worker held about 82 MiB
// where main's held 17, all of it counted against limits.max_script_memory.
// With one arena a ready worker with its two threads holds what the
// interpreter needs.
func TestAReadyPythonWorkerReservesNoSecondArena(t *testing.T) {
	requireProcStatus(t)
	for _, limit := range []int64{0, 64 << 20, 256 << 20} {
		c := workerCollector("ready", `metric(name="v", value=1)`)
		c.Limits.MaxScriptMemory = model.ByteSize(limit)
		worker, err := startPythonWorker(context.Background(), pythonWorkerSpec("python3", c))
		if err != nil {
			t.Fatal(err)
		}
		pid := worker.cmd.Process.Pid
		size, threads := workerStatus(t, pid, "VmSize"), workerStatus(t, pid, "Threads")
		worker.stop()
		// An arena alone is 65,536 kB.
		if size >= 48<<10 {
			t.Fatalf("under a limit of %d bytes a ready worker's address space is %d kB", limit, size)
		}
		if threads != 2 {
			t.Fatalf("under a limit of %d bytes a ready worker has %d threads, want the interpreter's and the watch's", limit, threads)
		}
	}
}

// limitedCollector is a python collector whose workers run under
// limits.max_script_memory.
func limitedCollector(name, script string, limit model.ByteSize) *model.Collector {
	c := workerCollector(name, script)
	c.Limits.MaxScriptMemory = limit
	return c
}

// Under limits.max_script_memory: 256MiB a script that needs 200 MiB failed
// with a MemoryError, the reserved arena having taken 64 MiB of the limit;
// main ran it. It runs, again and again in the same worker, and a script
// that needs more than the limit leaves still fails naming it.
func TestAScriptHasTheMemoryLimitLessTheInterpreter(t *testing.T) {
	requireProcStatus(t)
	fits := limitedCollector("headroom", `metric(name="v", value=len(bytearray(200 << 20)))`, 256<<20)
	for range 3 {
		set, err := runWorkerScript(t, fits)
		if err != nil {
			t.Fatalf("200 MiB under limits.max_script_memory 256MiB: %v", err)
		}
		if v := workerMetricValue(t, set, "v"); v != 200<<20 {
			t.Fatalf("v=%v", v)
		}
	}
	if snap := PythonWorkers().Snapshot("headroom"); snap.Starts != 1 || snap.Idle != 1 {
		t.Fatalf("%d workers started, %d idle, stops %v", snap.Starts, snap.Idle, snap.Stops)
	}
	tooLarge := limitedCollector("headroom", `metric(name="v", value=len(bytearray(250 << 20)))`, 256<<20)
	if _, err := runWorkerScript(t, tooLarge); err == nil || !strings.Contains(err.Error(), "MemoryError: the script ran out of memory under limits.max_script_memory (268435456 bytes)") {
		t.Fatalf("250 MiB under limits.max_script_memory 256MiB: err=%v", err)
	}
}

// Under a small limit a script that needs next to nothing runs, as on main:
// at the smallest limit there is, 32MiB, the worker was already past it
// when it was ready, and a script that only called metric() ran out of
// memory.
func TestASmallMemoryLimitRunsASmallScript(t *testing.T) {
	requireProcStatus(t)
	for _, limit := range []model.ByteSize{32 << 20, 64 << 20} {
		for script, want := range map[string]float64{
			`metric(name="v", value=1)`:                       1,
			`metric(name="v", value=len(bytearray(8 << 20)))`: 8 << 20,
		} {
			set, err := runWorkerScript(t, limitedCollector("small", script, limit))
			if err != nil {
				t.Fatalf("limit %d, %s: %v", limit, script, err)
			}
			if v := workerMetricValue(t, set, "v"); v != want {
				t.Fatalf("limit %d, %s: v=%v", limit, script, v)
			}
		}
		if _, err := runWorkerScript(t, limitedCollector("small", `metric(name="v", value=len(bytearray(200 << 20)))`, limit)); err == nil || !strings.Contains(err.Error(), "limits.max_script_memory") {
			t.Fatalf("limit %d, 200 MiB: err=%v", limit, err)
		}
	}
}

// usesAllTheMemory is a script that leaves its worker no memory at all for
// longer than two turns of the parent watch, then gives it back. It fills
// the limit with ever smaller blocks down to the 32 bytes of a number,
// which is what os.getppid has to allocate for its answer, and it keeps
// what it allocates in slots made beforehand, by indexes made beforehand,
// so that filling the last block needs no other.
const usesAllTheMemory = `import time
slots = [None] * 200000
free = iter(list(range(200000)))
f = 2.5
for size in (1 << 20, 1 << 16, 1 << 12, 512, 256, 128, 96, 80, 64, 48):
    try:
        for i in free: slots[i] = bytes(size - 33)
    except MemoryError: pass
for _ in (1, 2, 3, 4):
    try:
        for i in free: slots[i] = f + 1.0
    except MemoryError: pass
time.sleep(2.5)
del slots, free
metric(name="filled", value=i)
`

// The thread that watches a worker's parent ended, with a MemoryError
// nobody saw, when a script had used all of limits.max_script_memory at
// the moment it looked: os.getppid could not allocate its answer. The
// worker then went on serving with nothing watching, and would have
// outlived a killed exporter. The watch outlives the script's use of all
// the memory, and the worker serves the next run with both its threads.
func TestThePythonParentWatchOutlivesAScriptThatUsesAllTheMemory(t *testing.T) {
	requireProcStatus(t)
	c := limitedCollector("exhausts", usesAllTheMemory, 64<<20)
	c.Limits.ScriptTimeout = model.Duration(20 * time.Second)
	set, err := runWorkerScript(t, c)
	if err != nil {
		t.Fatal(err)
	}
	// The slots were not all used: the script stopped at the limit, not at
	// its own end.
	if filled := workerMetricValue(t, set, "filled"); filled < 50 || filled >= 199999 {
		t.Fatalf("the script filled %v slots", filled)
	}
	worker := idleWorker(t, c)
	if threads := workerStatus(t, worker.cmd.Process.Pid, "Threads"); threads != 2 {
		t.Fatalf("after the script used all the memory its worker has %d threads: the watch ended (%s)", threads, strings.TrimSpace(worker.stderr.String()))
	}
	if _, err := runWorkerScript(t, limitedCollector("exhausts", `metric(name="v", value=1)`, 64<<20)); err != nil {
		t.Fatal(err)
	}
}

// idleWorker is the collector's one idle worker.
func idleWorker(t *testing.T, c *model.Collector) *pythonWorker {
	t.Helper()
	pool := PythonWorkers()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	idle := pool.idle[pythonWorkerSpec("python3", c).key()]
	if len(idle) != 1 {
		t.Fatalf("%d idle workers, want 1", len(idle))
	}
	return idle[0]
}

// The watch is running before the memory limit is installed: the launcher
// waits for it. Left to start by itself, the thread could take its first
// step under a limit the worker was already past - as it is where the
// operator's MALLOC_ARENA_MAX lets glibc reserve an arena for it - and end
// there, before its first line, where nothing it does can catch that.
func TestThePythonParentWatchRunsBeforeTheMemoryLimit(t *testing.T) {
	requireProcStatus(t)
	t.Setenv(pythonArenaVariable, "8")
	for range 5 {
		worker, err := startPythonWorker(context.Background(), pythonWorkerSpec("python3", limitedCollector("early", `metric(name="v", value=1)`, 32<<20)))
		if err != nil {
			t.Fatal(err)
		}
		// A thread left to start by itself takes its first step once the
		// launcher, ready, waits for a request.
		time.Sleep(20 * time.Millisecond)
		threads, complaint := workerStatus(t, worker.cmd.Process.Pid, "Threads"), strings.TrimSpace(worker.stderr.String())
		worker.stop()
		if threads != 2 || complaint != "" {
			t.Fatalf("a ready worker has %d threads and wrote %q", threads, complaint)
		}
	}
}
