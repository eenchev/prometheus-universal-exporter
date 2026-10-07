package transform

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// PythonLibraries are the third-party libraries the image installs, under the
// names a collector may declare them by: the package name or the import name.
var PythonLibraries = map[string]bool{"lxml": true, "PyYAML": true, "yaml": true, "python-dateutil": true, "dateutil": true}

// Python scripts run in long-lived worker interpreters rather than in a fresh
// process per scrape. Starting CPython and importing lxml, PyYAML or dateutil
// takes tens to hundreds of milliseconds; running a typical collector script
// takes well under one. A worker pays the start-up once and then serves
// scrape after scrape.
//
// Isolation. A worker only ever runs one collector's scripts: workers are
// pooled per collector, interpreter, declared libraries, output limit and
// script text, so a script cannot see or disturb another collector's state,
// and a reloaded script gets fresh workers rather than inheriting the old
// one's module state. Each run gets a fresh set of globals. The same sandbox
// as before is installed once, at start-up, before any script runs.
//
// Protocol. Requests go to the worker on file descriptor 3 and answers come
// back on file descriptor 4, one JSON document per line. Neither is stdin or
// stdout, so a script that prints, or reads stdin, cannot corrupt the stream;
// its output is captured per run, as before. The worker's stdin and stdout are
// /dev/null, and stderr is kept, bounded, for error messages.
//
// A worker that has read and parsed a request says so, with a line of its
// own, before it runs the script, and then answers with the result
// (pythonWorker.call).
//
// Time. limits.script_timeout bounds the run of a script: its clock starts
// when the worker says it has the request. It bounds neither the start of the
// interpreter, which, with importing the collector's declared libraries, has
// its own budget, nor handing the worker the request, which takes as long as
// the response is large and is bounded by the probe's deadline, and by
// pythonHandoverTimeout where that is later or there is none. A script that
// overruns is not interrupted inside the interpreter, which Python cannot do
// reliably; the worker is killed, and the next scrape starts another. A
// script the probe's deadline ends first is stopped the same way, and
// reported and counted as that rather than as overrunning script_timeout.
//
// Lifetime. A worker that fails in any way is discarded. A healthy one is
// reused up to pythonWorkerMaxRuns times, so a slow leak in a script or a
// library cannot grow without bound, and at most pythonWorkerMaxIdle are kept
// per pool once a burst of concurrent scrapes has passed. Idle workers are
// stopped after pythonWorkerIdleTimeout, checked every
// PythonWorkerReapInterval whether or not anything runs Python, so a collector
// nobody scrapes any more does not keep its interpreters. A reload stops at
// once the idle workers of scripts it removed or changed, and those still busy
// when they finish.
//
// An idle worker can die: the kernel may kill it for memory, and a script
// can arm a signal that outlives its run. One found dead when it is taken
// from the pool is replaced, and one that turns out dead before it took the
// request, when nothing of the script has run, costs the scrape another
// worker rather than a failure (PythonPool.run).
//
// When the exporter exits, an idle worker sees its request pipe close and
// exits too. A busy one is not reading the pipe, and when the exporter is
// killed nobody is left to stop it, so every worker also watches for the
// process that started it: a thread of the launcher looks once a second
// whether its parent is still that process, and ends the worker when it is
// not. The worker does this itself, rather than asking the kernel to signal
// it when its parent dies, because that request is tied to the thread of the
// exporter that started the worker, which the Go runtime may end long before
// the exporter does.

const (
	// pythonStartupTimeout is how long an interpreter may take to say it is
	// ready: every pool's start timeout, which only a test changes
	// (PythonPool.SetStartTimeout).
	pythonStartupTimeout = 10 * time.Second
	// pythonHandoverTimeout is how long a worker may take to read and parse
	// a request when the probe's deadline does not end it sooner: far longer
	// than the largest response takes, and short enough that a worker that
	// has stopped reading does not hold a scrape without a deadline for ever.
	pythonHandoverTimeout   = 30 * time.Second
	pythonWorkerMaxRuns     = 1000
	pythonWorkerMaxIdle     = 4
	pythonWorkerIdleTimeout = 5 * time.Minute
	// PythonWorkerReapInterval is how often idle workers are checked against
	// the idle timeout, so one lives at most the sum of the two unused.
	PythonWorkerReapInterval = time.Minute
	pythonStderrTail         = 4096
	pythonDefaultMaxOutput   = 1 << 20
)

var (
	errPythonTimeout        = errors.New("python script timed out")
	errPythonOutputTooLarge = errors.New("python output exceeds limit")
)

// pythonDeadlineError is a run the probe's deadline ended, rather than
// limits.script_timeout: started says whether the script had started, and
// ran for how long it had run.
type pythonDeadlineError struct {
	started bool
	ran     time.Duration
}

func (e pythonDeadlineError) Error() string {
	return "the probe's deadline ended the python script"
}

// Unwrap makes the error the deadline it is.
func (e pythonDeadlineError) Unwrap() error { return context.DeadlineExceeded }

// pythonWorkerGone is a worker found dead before it took a request, so that
// nothing of the script has run and another worker can run it.
type pythonWorkerGone struct{ why string }

func (e pythonWorkerGone) Error() string { return e.why }

// pythonRequestTaken is the line a worker writes when it has read and parsed
// a request, before it runs the script.
const pythonRequestTaken = `{"started": true}`

// pythonLibraryModules are the modules a declared library preloads. Importing
// them at start-up keeps their import time out of script_timeout, and lets a
// library that imports a module the sandbox blocks, such as threading, load
// before the sandbox is installed.
var pythonLibraryModules = map[string][]string{
	"lxml":            {"lxml", "lxml.etree", "lxml.html"},
	"PyYAML":          {"yaml"},
	"yaml":            {"yaml"},
	"python-dateutil": {"dateutil", "dateutil.parser", "dateutil.tz"},
	"dateutil":        {"dateutil", "dateutil.parser", "dateutil.tz"},
}

// pythonSpec says which workers a script may run in.
type pythonSpec struct {
	Path      string
	Collector string
	Modules   []string
	MaxOutput int
	// MaxMemory bounds the worker's address space (RLIMIT_AS); 0 leaves it
	// unbounded.
	MaxMemory int64
	Scripts   string // a digest of the collector's scripts
}

func pythonWorkerSpec(pythonPath string, c *model.Collector) pythonSpec {
	seen := map[string]bool{}
	var modules []string
	for _, lib := range append(append([]string(nil), c.Transform.Libraries...), c.Transform.RequiredLibs...) {
		for _, module := range pythonLibraryModules[lib] {
			if !seen[module] {
				seen[module] = true
				modules = append(modules, module)
			}
		}
	}
	sort.Strings(modules)
	maxOutput := int(min(c.Limits.MaxOutputBytes, math.MaxInt32))
	if maxOutput <= 0 {
		maxOutput = pythonDefaultMaxOutput
	}
	digest := sha256.Sum256([]byte(c.Transform.PreScript + "\x00" + c.Transform.Script))
	return pythonSpec{Path: pythonPath, Collector: c.Name, Modules: modules, MaxOutput: maxOutput, MaxMemory: int64(c.Limits.MaxScriptMemory), Scripts: hex.EncodeToString(digest[:8])}
}

func (s pythonSpec) key() string {
	return strings.Join([]string{s.Path, s.Collector, strings.Join(s.Modules, ","), strconv.Itoa(s.MaxOutput), strconv.FormatInt(s.MaxMemory, 10), s.Scripts}, "\x00")
}

// PythonPool keeps the Python workers that run collector scripts, idle ones
// ready for the next run of the same script, and counts what they do for the
// self-metrics.
type PythonPool struct {
	mu      sync.Mutex
	idle    map[string][]*pythonWorker
	started atomic.Int64
	// stats are kept per collector, under mu. They cost a map lookup per
	// run, and are published only with verbose self-metrics (exporter/verbosemetrics.go).
	stats map[string]*pythonCollectorStats
	// busy counts the workers of each key that are running a script, and
	// obsolete holds the keys a reload dropped while some of their workers
	// were busy; those are stopped when they finish.
	busy     map[string]int
	obsolete map[string]bool
	// closed pools keep no worker: an idle one is stopped at once, and a busy
	// one when it finishes.
	closed bool
	// maxWorkers, when positive, bounds the workers alive at once, starting,
	// busy or idle, of every collector together (--python.max-workers).
	// live counts them. waiting is the runs waiting for a worker, in
	// line: a worker going idle or stopping wakes the first of them only,
	// which any such change lets through — it takes the idle worker, or
	// evicts it, or starts one in the stopped worker's place — rather than
	// every waiter racing for it.
	maxWorkers int
	live       int
	waiting    []chan struct{}
	// start starts a worker: startPythonWorker. It is a field so a test can
	// give the pool a stand-in whose start-up takes the time the test says,
	// rather than what an interpreter takes on the machine as it is loaded.
	start func(ctx context.Context, spec pythonSpec) (*pythonWorker, error)
	// leastScriptTimeout, when positive, is the least time a script is given,
	// whatever its limits.script_timeout (SetLeastScriptTimeout). It is for
	// the tests, as start is.
	leastScriptTimeout atomic.Int64
	// startTimeout is how long an interpreter the pool starts may take to
	// say it is ready: pythonStartupTimeout, but in the tests
	// (SetStartTimeout).
	startTimeout atomic.Int64
}

// Why a worker stopped, and how a run ended: bounded sets, so they can be
// label values.
const (
	pythonStopTimeout     = "timeout"
	pythonStopDeadline    = "deadline"
	pythonStopCrash       = "crash"
	pythonStopOutputLimit = "output_limit"
	pythonStopCancelled   = "cancelled"
	pythonStopRetired     = "retired"
	pythonStopSurplus     = "surplus"
	pythonStopIdle        = "idle"
	pythonStopReload      = "reload"
	pythonStopEvicted     = "evicted"

	pythonRunOK          = "ok"
	pythonRunScriptError = "script_error"
	pythonRunTimeout     = "timeout"
	pythonRunDeadline    = "deadline"
	pythonRunOutputLimit = "output_limit"
	pythonRunFailed      = "failed"
)

// PythonStopReasons and PythonRunOutcomes are every reason a worker stops and
// every way a run ends, so each has a series from the start.
var (
	PythonStopReasons = []string{pythonStopTimeout, pythonStopDeadline, pythonStopCrash, pythonStopOutputLimit, pythonStopCancelled, pythonStopRetired, pythonStopSurplus, pythonStopIdle, pythonStopReload, pythonStopEvicted}
	PythonRunOutcomes = []string{pythonRunOK, pythonRunScriptError, pythonRunTimeout, pythonRunDeadline, pythonRunOutputLimit, pythonRunFailed}
)

type pythonCollectorStats struct {
	starting, busy int
	starts         uint64
	startFailures  uint64
	stops          map[string]uint64
	runs           map[string]uint64
}

// pythonPoolRef is the exporter's worker pool. It is behind an atomic pointer
// only so each test can run against a pool of its own (IsolatePythonWorkers):
// the counts a test checks are then its own, whatever ran before it, in
// whatever order, however often.
var pythonPoolRef atomic.Pointer[PythonPool]

func init() { pythonPoolRef.Store(newPythonPool()) }

// PythonWorkers returns the worker pool.
func PythonWorkers() *PythonPool { return pythonPoolRef.Load() }

// IsolatePythonWorkers puts a fresh worker pool in place of the current one
// and returns what puts the previous pool back, stopping the fresh pool's
// workers. It is for tests, which read the pool's counts, and must not run in
// parallel with anything else using the pool.
func IsolatePythonWorkers() (restore func()) {
	pool := newPythonPool()
	previous := pythonPoolRef.Swap(pool)
	return func() {
		pythonPoolRef.Store(previous)
		pool.shutdown()
	}
}

func newPythonPool() *PythonPool {
	pool := &PythonPool{idle: map[string][]*pythonWorker{}, stats: map[string]*pythonCollectorStats{}, busy: map[string]int{}, obsolete: map[string]bool{}, start: startPythonWorker}
	pool.startTimeout.Store(int64(pythonStartupTimeout))
	return pool
}

// SetLeastScriptTimeout gives every script the pool runs at least this long,
// whatever its limits.script_timeout, and 0 holds each to its own again. It
// is for tests. A script of a millisecond overruns the default 100ms on a
// machine busy enough, as a test runner is under the race detector with
// other packages' tests beside it; a test that is not about the timeout then
// fails by it. Such a test runs against a pool that leaves its scripts a
// minute, which a slow machine makes slower and never fails, and a test of
// the timeout itself sets 0.
func (p *PythonPool) SetLeastScriptTimeout(least time.Duration) {
	p.leastScriptTimeout.Store(int64(least))
}

// SetStartTimeout is how long an interpreter the pool starts may take to say
// it is ready, which is ten seconds in the exporter. It is for tests. An
// interpreter that starts in a tenth of a second has taken more than the ten
// on a machine busy enough, and a test that is not about the start then
// fails by it; such a test runs against a pool that leaves the start a
// minute, as it leaves a script (SetLeastScriptTimeout). A test of the limit
// itself sets a short one, and starts something that never says it is ready.
func (p *PythonPool) SetStartTimeout(limit time.Duration) {
	p.startTimeout.Store(int64(limit))
}

// SetMaxWorkers bounds the workers alive at once, of every collector
// together; 0 leaves them unbounded.
func (p *PythonPool) SetMaxWorkers(limit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxWorkers = limit
	// A higher limit may let every waiter through.
	for len(p.waiting) > 0 {
		p.notifyLocked()
	}
}

// ValidateMaxWorkers refuses a negative --python.max-workers.
func ValidateMaxWorkers(limit int) error {
	if limit < 0 {
		return fmt.Errorf("--python.max-workers must not be negative, got %d", limit)
	}
	return nil
}

// notifyLocked wakes the first run waiting for a worker; mu is held.
func (p *PythonPool) notifyLocked() {
	if len(p.waiting) == 0 {
		return
	}
	first := p.waiting[0]
	p.waiting = p.waiting[1:]
	close(first)
}

// stopLocked stops a worker that is no longer counted as busy or idle, and
// frees its place; mu is held.
func (p *PythonPool) stopLocked(worker *pythonWorker) {
	worker.stop()
	p.live--
	p.notifyLocked()
}

// statsLocked returns a collector's statistics, creating them; mu is held.
func (p *PythonPool) statsLocked(collector string) *pythonCollectorStats {
	st := p.stats[collector]
	if st == nil {
		st = &pythonCollectorStats{stops: map[string]uint64{}, runs: map[string]uint64{}}
		p.stats[collector] = st
	}
	return st
}

func (p *PythonPool) count(collector string, update func(*pythonCollectorStats)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	update(p.statsLocked(collector))
}

// recordRun counts how a run ended. The pool cannot tell a script's own error
// from a successful answer, so the caller, which reads the answer, records it.
func (p *PythonPool) recordRun(collector, outcome string) {
	p.count(collector, func(st *pythonCollectorStats) { st.runs[outcome]++ })
}

// run sends one request to a worker for spec and returns its answer line and
// how long the script ran, without starting a worker or handing it the
// request. A worker from the pool that turns out to have died while it was
// idle, before it took the request, is replaced by another, so its death
// fails no scrape; one just started that dies is the failure it looks like.
func (p *PythonPool) run(ctx context.Context, spec pythonSpec, payload []byte, timeout time.Duration) ([]byte, time.Duration, error) {
	for {
		worker, reused, err := p.acquire(ctx, spec)
		if err != nil {
			return nil, 0, err
		}
		line, ran, err := worker.call(ctx, payload, timeout)
		if err == nil {
			p.release(spec, worker)
			return line, ran, nil
		}
		p.discard(worker, stopReason(err))
		if gone := (pythonWorkerGone{}); reused && errors.As(err, &gone) {
			continue
		}
		return nil, ran, err
	}
}

func stopReason(err error) string {
	switch {
	case errors.Is(err, errPythonTimeout):
		return pythonStopTimeout
	case errors.As(err, &pythonDeadlineError{}):
		return pythonStopDeadline
	case errors.Is(err, errPythonOutputTooLarge):
		return pythonStopOutputLimit
	case errors.Is(err, context.Canceled):
		return pythonStopCancelled
	}
	return pythonStopCrash
}

// discard stops a busy worker and counts why.
func (p *PythonPool) discard(worker *pythonWorker, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked(worker)
	st := p.statsLocked(worker.collector)
	st.busy--
	st.stops[reason]++
	p.unbusyLocked(worker.key)
}

// unbusyLocked counts a worker of key as no longer busy; mu is held.
func (p *PythonPool) unbusyLocked(key string) {
	p.busy[key]--
	if p.busy[key] <= 0 {
		delete(p.busy, key)
		delete(p.obsolete, key)
	}
}

// acquire gives a worker for spec: an idle one of the pool, which reused
// then says, or one it starts.
func (p *PythonPool) acquire(ctx context.Context, spec pythonSpec) (*pythonWorker, bool, error) {
	key := spec.key()
	p.mu.Lock()
	p.reapLocked(time.Now())
	woken := false
	for {
		var worker *pythonWorker
		if idle := p.idle[key]; len(idle) > 0 {
			worker = idle[len(idle)-1]
			p.idle[key] = idle[:len(idle)-1]
		}
		if worker != nil && worker.gone() {
			// It died while it was idle: stopped and counted as the crash
			// it was, and the next one, or a new one, serves the run.
			p.stopLocked(worker)
			p.statsLocked(worker.collector).stops[pythonStopCrash]++
			continue
		}
		if worker != nil {
			p.statsLocked(spec.Collector).busy++
			p.busy[key]++
			p.mu.Unlock()
			return worker, true, nil
		}
		if p.maxWorkers <= 0 || p.live < p.maxWorkers || p.evictIdleLocked() {
			break
		}
		// Every worker the limit allows is starting or busy: wait in line
		// for one to finish, within the run's own deadline. A waiter woken
		// that still finds none, because a run that never waited took it
		// first, keeps its place at the head of the line.
		wake := make(chan struct{})
		if woken {
			p.waiting = append([]chan struct{}{wake}, p.waiting...)
		} else {
			p.waiting = append(p.waiting, wake)
		}
		p.mu.Unlock()
		select {
		case <-wake:
			woken = true
		case <-ctx.Done():
			p.mu.Lock()
			select {
			case <-wake:
				// Woken as it gave up: the wake goes to the next in line.
				p.notifyLocked()
			default:
				p.waiting = slices.DeleteFunc(p.waiting, func(c chan struct{}) bool { return c == wake })
			}
			limit := p.maxWorkers
			p.mu.Unlock()
			return nil, false, fmt.Errorf("the exporter already runs %d Python workers, its --python.max-workers, and none was free in time: %w", limit, ctx.Err())
		}
		p.mu.Lock()
	}
	p.live++
	st := p.statsLocked(spec.Collector)
	st.starting++
	p.mu.Unlock()

	p.started.Add(1)
	worker, err := p.start(ctx, spec)
	p.mu.Lock()
	defer p.mu.Unlock()
	st = p.statsLocked(spec.Collector)
	st.starting--
	if err != nil {
		p.live--
		p.notifyLocked()
		st.startFailures++
		return nil, false, err
	}
	st.starts++
	st.busy++
	p.busy[key]++
	return worker, false, nil
}

// evictIdleLocked stops the idle worker unused for longest, of whatever
// script, to make room under --python.max-workers, and says whether there
// was one; mu is held.
func (p *PythonPool) evictIdleLocked() bool {
	var oldestKey string
	oldest := -1
	for key, workers := range p.idle {
		for i, worker := range workers {
			if oldest < 0 || worker.idleSince.Before(p.idle[oldestKey][oldest].idleSince) {
				oldestKey, oldest = key, i
			}
		}
	}
	if oldest < 0 {
		return false
	}
	workers := p.idle[oldestKey]
	worker := workers[oldest]
	workers = append(workers[:oldest], workers[oldest+1:]...)
	if len(workers) == 0 {
		delete(p.idle, oldestKey)
	} else {
		p.idle[oldestKey] = workers
	}
	p.stopLocked(worker)
	p.statsLocked(worker.collector).stops[pythonStopEvicted]++
	return true
}

func (p *PythonPool) release(spec pythonSpec, worker *pythonWorker) {
	worker.runs++
	if worker.runs >= pythonWorkerMaxRuns {
		p.discard(worker, pythonStopRetired)
		return
	}
	key := spec.key()
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.statsLocked(spec.Collector)
	st.busy--
	obsolete := p.obsolete[key]
	p.unbusyLocked(key)
	if p.closed {
		p.stopLocked(worker)
		return
	}
	if obsolete {
		// A reload removed or changed this script while it ran.
		p.stopLocked(worker)
		st.stops[pythonStopReload]++
		return
	}
	if len(p.idle[key]) >= pythonWorkerMaxIdle {
		p.stopLocked(worker)
		st.stops[pythonStopSurplus]++
		return
	}
	worker.idleSince = time.Now()
	p.idle[key] = append(p.idle[key], worker)
	p.notifyLocked()
}

// reapLocked stops workers idle for longer than the idle timeout, which also
// retires the workers of a script a reload replaced.
func (p *PythonPool) reapLocked(now time.Time) {
	for key, workers := range p.idle {
		kept := workers[:0]
		for _, worker := range workers {
			if now.Sub(worker.idleSince) > pythonWorkerIdleTimeout {
				p.stopLocked(worker)
				p.statsLocked(worker.collector).stops[pythonStopIdle]++
				continue
			}
			kept = append(kept, worker)
		}
		if len(kept) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = kept
		}
	}
}

// shutdown stops the pool's idle workers and keeps no more; a worker still
// busy stops when it finishes.
func (p *PythonPool) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for key, workers := range p.idle {
		for _, worker := range workers {
			p.stopLocked(worker)
		}
		delete(p.idle, key)
	}
}

// reapIdle stops the workers idle for longer than the idle timeout.
func (p *PythonPool) reapIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reapLocked(now)
}

// ReapLoop stops idle workers on a timer until ctx ends, so they are stopped
// even when nothing asks for a worker any more.
func (p *PythonPool) ReapLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.reapIdle(now)
		}
	}
}

// Retain makes keys the scripts in use: idle workers of any other are stopped
// at once, and busy ones when they finish.
func (p *PythonPool) Retain(keys map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range keys {
		delete(p.obsolete, key)
	}
	for key, count := range p.busy {
		if count > 0 && !keys[key] {
			p.obsolete[key] = true
		}
	}
	for key, workers := range p.idle {
		if keys[key] {
			continue
		}
		for _, worker := range workers {
			p.stopLocked(worker)
			p.statsLocked(worker.collector).stops[pythonStopReload]++
		}
		delete(p.idle, key)
	}
}

// PythonWorkerKeys are the worker keys a configuration uses.
func PythonWorkerKeys(pythonPath string, c *model.Config) map[string]bool {
	keys := map[string]bool{}
	for i := range c.Collectors {
		x := &c.Collectors[i]
		if x.Transform.Type == "python" || x.Transform.PreScript != "" {
			path := pythonPath
			if path == "" {
				path = "python3"
			}
			keys[pythonWorkerSpec(path, x).key()] = true
		}
	}
	return keys
}

// pythonWorkerSnapshot is one collector's worker statistics at a moment.
type pythonWorkerSnapshot struct {
	Starting, Idle, Busy int
	// Waiting is the runs waiting for a worker, pool-wide only.
	Waiting               int
	Starts, StartFailures uint64
	Stops, Runs           map[string]uint64
}

// Snapshot copies a collector's statistics and counts its idle workers.
func (p *PythonPool) Snapshot(collector string) pythonWorkerSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.statsLocked(collector)
	out := pythonWorkerSnapshot{Starting: st.starting, Busy: st.busy, Starts: st.starts, StartFailures: st.startFailures, Stops: map[string]uint64{}, Runs: map[string]uint64{}}
	for reason, n := range st.stops {
		out.Stops[reason] = n
	}
	for outcome, n := range st.runs {
		out.Runs[outcome] = n
	}
	for _, workers := range p.idle {
		for _, worker := range workers {
			if worker.collector == collector {
				out.Idle++
			}
		}
	}
	return out
}

// PoolSnapshot sums the statistics of every collector the pool has served,
// including collectors a reload has since removed, so its counters never go
// backwards.
func (p *PythonPool) PoolSnapshot() pythonWorkerSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := pythonWorkerSnapshot{Stops: map[string]uint64{}, Runs: map[string]uint64{}}
	out.Waiting = len(p.waiting)
	for _, st := range p.stats {
		out.Starting += st.starting
		out.Busy += st.busy
		out.Starts += st.starts
		out.StartFailures += st.startFailures
		for reason, n := range st.stops {
			out.Stops[reason] += n
		}
		for outcome, n := range st.runs {
			out.Runs[outcome] += n
		}
	}
	for _, workers := range p.idle {
		out.Idle += len(workers)
	}
	return out
}

type pythonLine struct {
	data []byte
	err  error
}

type pythonWorker struct {
	collector string
	key       string
	cmd       *exec.Cmd
	requests  *os.File
	lines     chan pythonLine
	// exited is closed when the worker's process has ended.
	exited    chan struct{}
	stderr    *tailBuffer
	runs      int
	idleSince time.Time
	stopOnce  sync.Once
	// maxMemory is the worker's limits.max_script_memory, 0 for none.
	maxMemory int64
}

func startPythonWorker(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
	return startPythonWorkerRunning(ctx, spec, pythonWorkerLauncher)
}

// startPythonWorkerRunning starts a worker that runs launcher, which is
// pythonWorkerLauncher but in the tests that compare it with what it was.
func startPythonWorkerRunning(ctx context.Context, spec pythonSpec, launcher string) (*pythonWorker, error) {
	modules, err := json.Marshal(append([]string{}, spec.Modules...))
	if err != nil {
		return nil, err
	}
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	answerRead, answerWrite, err := os.Pipe()
	if err != nil {
		closeFiles(requestRead, requestWrite)
		return nil, err
	}
	// The worker outlives the scrape that starts it, so it must not be tied to
	// that scrape's context; stop() ends it. -B: the importer never writes
	// bytecode caches. Where a module's cache is missing or stale and its
	// directory writable, it would otherwise try to write one through the
	// sandboxed _io.FileIO, whose refusal it does not expect, and the import
	// would fail. The worker is told how deep a decoded value nests, which
	// is how deep it writes what a pre-script leaves in data (deep_check):
	// the depth is stated once, with the decoders. And it is told how long
	// an answer may be, limits.max_output_bytes: it does not write one
	// that a list or a dict is in so many times over that it is longer
	// (weigh), nor one whose strings alone are, each as often as it is
	// written, nor a script's error, which it cuts to what is shown of one
	// (failed). The exporter measures the line it reads, as it did.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), spec.Path, "-I", "-B", "-c", launcher, string(modules), strconv.FormatInt(spec.MaxMemory, 10), strconv.Itoa(decode.MaxDepth), strconv.Itoa(spec.MaxOutput)) // #nosec G204 -- the interpreter is the operator's --python.path
	cmd.ExtraFiles = []*os.File{requestRead, answerWrite}                                                                                                                                                              // descriptors 3 and 4
	cmd.Env = pythonWorkerEnvironment(os.Environ())
	stderr := &tailBuffer{max: pythonStderrTail}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		closeFiles(requestRead, requestWrite, answerRead, answerWrite)
		return nil, err
	}
	// The child holds its own copies of these ends.
	closeFiles(requestRead, answerWrite)

	worker := &pythonWorker{collector: spec.Collector, key: spec.key(), cmd: cmd, requests: requestWrite, lines: make(chan pythonLine, 1), exited: make(chan struct{}), stderr: stderr, maxMemory: spec.MaxMemory}
	go worker.readAnswers(answerRead, spec.MaxOutput)
	go func() {
		_ = cmd.Wait()
		close(worker.exited)
	}()

	// The pool's limit, which is pythonStartupTimeout but in a test.
	limit := time.Duration(PythonWorkers().startTimeout.Load())
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case line, ok := <-worker.lines:
		switch {
		case !ok:
			return nil, worker.startEnded(ctx, timer.C)
		case line.err != nil:
			worker.stop()
			return nil, fmt.Errorf("the interpreter did not start: %s", worker.describe(line.err))
		case !strings.Contains(string(line.data), `"ready": true`):
			// It answered, and is running: what it is stopped for is the
			// answer, which the exporter's worker never gives.
			worker.stop()
			return nil, worker.startFailure(model.Errorf("the interpreter did not start: its first answer was %s, not that it is ready, so it was stopped; --python.path must name a Python 3 interpreter that runs the exporter's worker", model.Quoted(line.data)))
		}
		return worker, nil
	case <-timer.C:
		return nil, worker.startOverran(limit)
	case <-ctx.Done():
		worker.stop()
		return nil, ctx.Err()
	}
}

// startEnded stops a worker whose answers ended before it said it was
// ready, and is why its start failed: it exited. Why it did is on its
// stderr, which is whole only once the process has been waited for, so that
// is waited for, until overrun, the start's own limit, or the scrape's end.
func (w *pythonWorker) startEnded(ctx context.Context, overrun <-chan time.Time) error {
	w.stop()
	select {
	case <-w.exited:
	case <-overrun:
	case <-ctx.Done():
	}
	return fmt.Errorf("the interpreter did not start: %s", w.describe(nil))
}

// startOverran stops a worker that has not said it is ready within limit,
// and is why its start failed. What it says is what was true when the limit
// ran out, so that is asked before the worker is stopped: stopped, every
// worker has exited, and an operator told so of one that was only slow looks
// for a crash that did not happen.
func (w *pythonWorker) startOverran(limit time.Duration) error {
	defer w.stop()
	select {
	case <-w.exited:
		return fmt.Errorf("the interpreter did not start within %s: %s", limit, w.describe(nil))
	default:
	}
	return w.startFailure(fmt.Errorf("the interpreter did not start within %s: it was still running and had not said it was ready, so it was stopped; it did not crash: look at how busy the machine is and at how long the libraries the collector declares take to import", limit))
}

// startFailure is err, the failure of a start that ended with the
// interpreter still running, with what the interpreter had written to stderr
// by then. How much that is depends on when it was stopped, so the failure
// is recognised without it (model.SameFailureText): a start that is too slow
// on every scrape is one failure to the log, however far each got.
func (w *pythonWorker) startFailure(err error) error {
	tail := strings.TrimSpace(w.stderr.String())
	if tail == "" {
		return err
	}
	const written = "; it had written to stderr: "
	return model.SameFailureAs(fmt.Errorf("%w%s%s", err, written, shownStderr(tail)), model.SameFailureText(err)+written+model.MovingMark)
}

// pythonWorkerEnvironment is the environment a worker starts in: the
// exporter's own, and MALLOC_ARENA_MAX=1 unless the operator has set that
// variable, whose value then stands.
//
// glibc gives a second thread a malloc arena of its own, for which it
// reserves 64 MiB of address space, used or not. A worker has a second
// thread, the one that watches its parent, and limits.max_script_memory
// bounds its address space: without the variable a worker that is ready
// holds about 82 MiB where its interpreter needs 17, and the difference is
// taken from what the limit leaves the script. With one arena both threads
// allocate from the same heap. Other C libraries and systems ignore the
// variable.
func pythonWorkerEnvironment(environ []string) []string {
	for _, entry := range environ {
		if strings.HasPrefix(entry, pythonArenaVariable+"=") {
			return environ
		}
	}
	return append(environ[:len(environ):len(environ)], pythonArenaVariable+"=1")
}

// pythonArenaVariable is the variable glibc reads the greatest number of
// malloc arenas from.
const pythonArenaVariable = "MALLOC_ARENA_MAX"

// readAnswers turns the answer stream into lines until the worker exits or
// writes a line longer than the output limit.
func (w *pythonWorker) readAnswers(answers *os.File, maxOutput int) {
	defer close(w.lines)
	defer closeFiles(answers)
	scanner := bufio.NewScanner(answers)
	scanner.Buffer(make([]byte, 0, min(4096, maxOutput+1)), maxOutput+1)
	for scanner.Scan() {
		w.lines <- pythonLine{data: append([]byte(nil), scanner.Bytes()...)}
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		w.lines <- pythonLine{err: errPythonOutputTooLarge}
	}
}

// gone reports, without waiting, whether an idle worker can no longer serve:
// its process has ended, or its answers have, or it wrote a line nobody asked
// for, after which its answers cannot be told from one another.
func (w *pythonWorker) gone() bool {
	select {
	case <-w.exited:
		return true
	case <-w.lines:
		return true
	default:
		return false
	}
}

// call runs one request in the worker, and returns its answer line and how
// long the script ran.
//
// The request is handed over first: written, and read and parsed by the
// worker, which then says it has it (pythonRequestTaken). That takes as long
// as the response is large, so it is bounded by the probe's deadline, or by
// pythonHandoverTimeout when that comes first, and not by timeout,
// limits.script_timeout, whose clock starts when the worker has the request:
// a trivial script given a large response does not time out. A worker that
// turns out dead before it has the request fails with pythonWorkerGone.
//
// What ends the wait for the answer says what the run failed of: the
// script's own time, errPythonTimeout; the probe's deadline, where that
// comes before it, a pythonDeadlineError; or the probe being abandoned, the
// context's error.
func (w *pythonWorker) call(ctx context.Context, payload []byte, timeout time.Duration) ([]byte, time.Duration, error) {
	handover := time.Now().Add(pythonHandoverTimeout)
	probeDeadline, hasDeadline := ctx.Deadline()
	if hasDeadline && probeDeadline.Before(handover) {
		handover = probeDeadline
	}
	notTaken := func() error {
		if hasDeadline && !handover.Before(probeDeadline) {
			return pythonDeadlineError{}
		}
		return fmt.Errorf("the worker did not take the request within %s", pythonHandoverTimeout)
	}
	_ = w.requests.SetWriteDeadline(handover)
	for _, part := range [][]byte{payload, {'\n'}} {
		if _, err := w.requests.Write(part); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, 0, notTaken()
			}
			return nil, 0, pythonWorkerGone{why: w.describe(err)}
		}
	}
	taken := time.NewTimer(time.Until(handover))
	defer taken.Stop()
	select {
	case line, ok := <-w.lines:
		switch {
		case !ok:
			return nil, 0, pythonWorkerGone{why: w.describe(nil) + w.memoryHint()}
		case line.err != nil:
			return nil, 0, line.err
		case string(line.data) != pythonRequestTaken:
			// The worker could not read the request, and says why.
			return line.data, 0, nil
		}
	case <-taken.C:
		return nil, 0, notTaken()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, 0, pythonDeadlineError{}
		}
		return nil, 0, ctx.Err()
	}

	// The script runs. Its own time ends it unless the probe's deadline
	// comes first, which then is what ended it.
	started := time.Now()
	var overrun <-chan time.Time
	if !hasDeadline || !probeDeadline.Before(started.Add(timeout)) {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		overrun = timer.C
	}
	select {
	case line, ok := <-w.lines:
		ran := time.Since(started)
		if !ok {
			return nil, ran, errors.New(w.describe(nil) + w.memoryHint())
		}
		if line.err != nil {
			return nil, ran, line.err
		}
		return line.data, ran, nil
	case <-overrun:
		return nil, time.Since(started), errPythonTimeout
	case <-ctx.Done():
		ran := time.Since(started)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ran, pythonDeadlineError{started: true, ran: ran}
		}
		return nil, ran, ctx.Err()
	}
}

// describe explains why a worker failed, with the end of what it wrote to
// stderr, which is where CPython reports a crash or a failed start.
func (w *pythonWorker) describe(err error) string {
	message := "the interpreter exited"
	if err != nil {
		message = err.Error()
	}
	if tail := strings.TrimSpace(w.stderr.String()); tail != "" {
		message += ": " + shownStderr(tail)
	}
	return message
}

// memoryHint says, of a worker that died mid-run under a memory limit, that
// the limit may be why: a C library such as lxml that cannot allocate may
// end the interpreter instead of raising a MemoryError the script would
// report.
func (w *pythonWorker) memoryHint() string {
	if w.maxMemory <= 0 {
		return ""
	}
	return fmt.Sprintf(" (the worker runs under limits.max_script_memory, %d bytes; a library that runs out of memory can end the interpreter rather than raise MemoryError, so raise the limit if the script needs more)", w.maxMemory)
}

func (w *pythonWorker) stop() {
	w.stopOnce.Do(func() {
		closeFiles(w.requests)
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
	})
}

// closeFiles closes pipe ends whose close cannot usefully fail: each is
// either already handed to the child or being abandoned with the worker.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// pythonWorkerLauncher is the worker. It opens its request and answer
// descriptors and preloads the declared libraries, then installs the sandbox
// — blocked modules, disabled process, file and descriptor functions — and
// only then answers that it is ready. Each request runs the script in fresh
// globals holding the same names scripts have always had, with stdout and
// stderr captured, and answers with the metrics, the data or the error.
const pythonWorkerLauncher = `import sys,json,builtins,contextlib,io,os,traceback,decimal,linecache
requests=os.fdopen(3,'r',encoding='utf-8')
answers=os.fdopen(4,'w',encoding='utf-8')
def watch_parent():
    # A worker busy in a script does not see its request pipe close when the
    # exporter is gone, and an exporter that was killed stops nobody. So a
    # thread looks once a second whether the worker's parent is still the
    # process that started it, and ends the worker when it is not: an orphan
    # is given another parent. It is started here, before the memory limit
    # and the sandbox, with what it calls bound now, so neither a limit too
    # small for a thread nor a script that replaces os.getppid stops it, and
    # the launcher goes on only once the thread runs: a thread still starting
    # when the limit is installed could fail of it before its first line.
    # Its stack is small, since it counts against that limit, and its
    # modules are imported in here, so the launcher's own names stay as they
    # were.
    import _thread,time
    parent,getppid,leave,sleep=os.getppid(),os.getppid,os._exit,time.sleep
    running=_thread.allocate_lock()
    running.acquire()
    def watch():
        running.release()
        while True:
            # A script that has used all of limits.max_script_memory leaves
            # none for the number getppid answers with. The watch outlives
            # that, and whatever else is raised in here, and looks again a
            # second later: it must not end while the worker lives. Nothing
            # is named after except, so nothing a script can replace.
            try:
                sleep(1)
                if getppid()!=parent: leave(0)
            except: pass
    try: _thread.stack_size(262144)
    except Exception: pass
    try:
        _thread.start_new_thread(watch,())
        running.acquire(True,5)
    except Exception: pass
    try: _thread.stack_size(0)
    except Exception: pass
watch_parent()
del watch_parent
for _module in json.loads(sys.argv[1]) or []:
    try: __import__(_module)
    except Exception: pass
max_memory=int(sys.argv[2]) if len(sys.argv)>2 else 0
if max_memory>0:
    # limits.max_script_memory: the worker's whole address space, the
    # interpreter and its preloaded libraries included, set after they have
    # loaded so a limit too small for them fails a run, not the start.
    import resource
    resource.setrlimit(resource.RLIMIT_AS,(max_memory,max_memory))
# zoneinfo reads its search path through sysconfig, which imports threading
# from Python 3.12 on. threading is blocked, so a script's import zoneinfo
# would fail; it is imported here, before the sandbox, and sysconfig's
# reference to threading is dropped below once the sandbox is in place.
try: import zoneinfo
except Exception: pass
blocked={'socket','_socket','ssl','_ssl','subprocess','_posixsubprocess','ctypes','_ctypes','multiprocessing','_multiprocessing','threading','mmap','pty','pathlib','shutil','tempfile'}
script_blocked={'importlib','posix','nt','_io','_thread','select','selectors','fcntl','termios'}
import _io
real_import=builtins.__import__
def guarded_import(name,globals=None,*a,**kw):
    top=name.split('.')[0]
    if top in blocked or name in {'urllib.request','urllib.error','urllib.robotparser'}: raise ImportError('module disabled by exporter')
    if top in script_blocked and (globals or {}).get('__name__')=='__collector__': raise ImportError('module disabled by exporter')
    return real_import(name,globals,*a,**kw)
builtins.__import__=guarded_import
for _name in [n for n in sys.modules if n.split('.')[0] in blocked or n in {'posix','nt'}]: del sys.modules[_name]
# sysconfig needs threading only for the lock it made when it loaded; left
# as its attribute, the blocked module would be one attribute away.
if 'threading' in vars(sys.modules.get('sysconfig',sys)): del sys.modules['sysconfig'].threading
def denied(*a,**kw): raise RuntimeError('operation disabled by exporter')
for _name in ('system','popen','spawnl','spawnle','spawnlp','spawnlpe','spawnv','spawnve','spawnvp','spawnvpe','posix_spawn','posix_spawnp','execl','execle','execlp','execlpe','execv','execve','execvp','execvpe','fork','forkpty','openpty','pipe','pipe2','open','listdir','scandir','walk','fwalk','remove','unlink','rename','replace','mkdir','makedirs','rmdir','removedirs','link','symlink','truncate','ftruncate','chmod','chown','lchown','mkfifo','mknod','fdopen','read','readv','pread','write','writev','pwrite','sendfile','dup','dup2','close','closerange','kill','killpg'):
    if hasattr(os,_name): setattr(os,_name,denied)
def tz_roots():
    # Time zone data may be read, and nothing else: the system's (zoneinfo's
    # TZPATH, where dateutil.tz looks too), dateutil's bundled copy and the
    # tzdata package's, when they are there. Resolved now, before the
    # sandbox, so a symlink out of them leads nowhere.
    roots=['/usr/share/zoneinfo','/usr/lib/zoneinfo','/usr/share/lib/zoneinfo','/etc/zoneinfo']
    try:
        import zoneinfo
        roots+=list(zoneinfo.TZPATH)
    except Exception: pass
    for package in ('dateutil.zoneinfo','tzdata'):
        try: roots.append(os.path.dirname(__import__(package,fromlist=['_']).__file__))
        except Exception: pass
    return tuple(sorted({os.path.realpath(r).rstrip(os.sep)+os.sep for r in roots if r}))
tz_readable_roots=tz_roots()
tz_readable_files={os.path.realpath('/etc/localtime')}
def tz_readable(file,mode):
    if mode not in ('r','rb','rt','br','tr') or not isinstance(file,(str,bytes,os.PathLike)): return False
    try:
        path=os.fsdecode(os.fspath(file))
        real=os.path.realpath(path)
    except Exception: return False
    return real in tz_readable_files or real.startswith(tz_readable_roots)
def tz_only(real):
    # open(), io.open() and io.FileIO read time zone data and nothing else.
    def opened(file,mode='r',*a,**kw):
        if tz_readable(file,mode): return real(file,mode,*a,**kw)
        raise RuntimeError('operation disabled by exporter')
    return opened
def code_only(open_code):
    # The importer reads a module's source and bytecode through _io.open.
    def opened(file,mode='r',*a,**kw):
        if mode=='rb' and isinstance(file,str) and file.endswith(('.py','.pyc')): return open_code(file,mode,*a,**kw)
        if tz_readable(file,mode): return open_code(file,mode,*a,**kw)
        raise RuntimeError('operation disabled by exporter')
    return opened
builtins.open=tz_only(io.open); io.FileIO=tz_only(_io.FileIO); _io.open=code_only(_io.open); io.open=builtins.open; _io.FileIO=io.FileIO
del _io, _name, code_only, tz_only, tz_roots
class Response:
    # The body is sent once: text is the same string.
    def __init__(self,x): self.status_code=x['status_code']; self.headers=x['headers']; self.body=self.text=x['body']
    def header(self,name,default=None):
        # One header's values joined by ", ", as jq's $headers has them, the
        # name in any case; default when the response has none.
        values=[v for k,vs in (self.headers or {}).items() if k.lower()==name.lower() for v in vs]
        return ', '.join(values) if values else default
    def json(self): return json.loads(self.text)
    def yaml(self):
        try:
            import yaml
        except ImportError: raise RuntimeError('yaml library is not available')
        return yaml.safe_load(self.text)
def label_text(name,v):
    # A label value as text, the way jq labels are written (transform/labeltext.go):
    # numbers as JSON writes them, booleans as true and false, None as no label.
    if v is None or isinstance(v,str): return v
    if isinstance(v,bool): return 'true' if v else 'false'
    if isinstance(v,int): return str(v)
    if isinstance(v,float):
        if v!=v: return 'NaN'
        if v in (float('inf'),float('-inf')): return '+Inf' if v>0 else '-Inf'
        if v==0: return '0'
        if 1e-6<=abs(v)<1e21:
            text=format(decimal.Decimal(repr(v)),'f')
            return text.rstrip('0').rstrip('.') if '.' in text else text
        return repr(v)
    if isinstance(v,(dict,list,tuple,set)): raise ValueError('label %r is a %s, not a single value; pass one value, or join them with ",".join(...)'%(name,type(v).__name__))
    # Whatever else it is, as it writes itself; what holds so many of its
    # own kind, one inside another, that the interpreter cannot write it is
    # no single value either.
    try: return str(v)
    except RecursionError: pass
    raise ValueError('label %r is a %s nested too deep to be written, not a single value; pass one value'%(name,type(v).__name__))
def shown(v):
    # A value as an error names it: as Python writes it, but a list, a
    # dict, a tuple or a set as what it is and how many items it has, as
    # the exporter names one (model.ShowValue). Written out, one nested
    # deeper than the interpreter follows it ended the error that was to
    # name it with a RecursionError, and one of a thousand items made an
    # error of a thousand items.
    if isinstance(v,(dict,list,tuple,set,frozenset)): return 'a %s of %d item%s'%(type(v).__name__,len(v),'' if len(v)==1 else 's')
    try: return repr(v)
    except RecursionError: return 'a %s nested too deep to be written'%type(v).__name__
def wire(v):
    # A string is written as long as it is at least, each time it is met:
    # its length is added to what the answer's strings come to (whole[2]).
    if type(v) is str: whole[2]+=len(v); return v
    # NaN and the infinities have no JSON form: each goes as a marker the
    # exporter reads back as the float (transform/python.go).
    if isinstance(v,float) and (v!=v or v in (float('inf'),float('-inf'))):
        return '\x00pue-nonfinite:'+('NaN' if v!=v else '+Inf' if v>0 else '-Inf')+'\x00'
    if isinstance(v,(dict,list,tuple)):
        # A list or a dict gone through before is in the answer more than
        # once, or holds itself: the answer is then weighed, once (weigh),
        # before any more of it is copied. Nothing is called for one met
        # the first time, and what is called for one met again may not
        # have a frame left where this walk still has: it is then asked
        # of the next.
        i=id(v)
        if i not in met: met.add(i)
        elif whole[1] is None:
            try: again()
            except RecursionError: pass
        if isinstance(v,dict): return {k:wire(x) for k,x in v.items()}
        return [wire(x) for x in v]
    return v
def metric_number(name,what,v):
    # A value or timestamp as a number: a bool as 1 or 0, a numeric string
    # as the number it reads as, and anything else refused naming the metric.
    if isinstance(v,bool): return 1.0 if v else 0.0
    if isinstance(v,(int,float)): return float(v)
    if isinstance(v,str):
        try: return float(v.strip())
        except ValueError: pass
    raise ValueError('metric %r %s %s is not a number'%(name,what,shown(v)))
def script_error(e):
    # The error as a traceback of the script's own frames, innermost last:
    # the worker's frames (this launcher, run as "<string>") are dropped, and
    # of the rest the five innermost are kept, the failing line among them.
    # Chained exceptions ("During handling of ...") are trimmed alike. It is
    # handed on in the pieces the traceback module makes it of, the lines of
    # a frame or of an exception each, for what writes it to join (failed).
    shown=traceback.TracebackException.from_exception(e)
    te,seen=shown,set()
    while te is not None and id(te) not in seen:
        seen.add(id(te))
        te.stack=traceback.StackSummary.from_list([f for f in te.stack if f.filename!='<string>'][-5:])
        te=te.__cause__ or te.__context__
    return shown.format()
deepest,most=int(sys.argv[3]),int(sys.argv[4])
def too_deep(): return RecursionError('data is nested more than %d deep, or a list or a dict in it holds itself; the exporter reads what a script leaves in data nested %d deep at most, as deep as it decodes a response'%(deepest,deepest))
def too_long(what): return OverflowError('what the script left in %s is longer than limits.max_output_bytes (%d bytes) written out, a list or a dict that is there more than once being written each time; leave less there, or raise limits.max_output_bytes'%(what,most))
# A script can leave one list or dict in an answer many times over, or in
# itself. Python keeps it once, and JSON writes it out each time it is met:
# a list that holds another twice, which holds a third twice, forty of them,
# is forty lists to the script and a million million values written, and
# one that holds itself has no end. A walk that does not remember where it
# has been goes through all of that. So a walk that meets a list or a dict
# it has met (wire), or whose count of values grows (plain) or has passed
# what fits the bytes the exporter takes, limits.max_output_bytes (plain),
# has the answer weighed before it goes on. weigh goes through each list and
# dict one time, however often it is there, in a loop, remembering the ones
# it is inside of: it finds one that holds itself when it comes back to it,
# and adds up what the rest is written as without writing it, in the time
# and the memory the script took to build it. An answer that holds nothing
# twice is not bounded here: it is as long as what the script built, and is
# written as it always was, for the exporter to measure.
#
# A string is another matter. One long string that is in an answer many
# times, as the items of a list, the values of a dict or a label of every
# metric, is in a list or a dict that is there once, and is written each
# time: a hundred thousand characters held five thousand times are five
# hundred million written, which the worker made in its memory, a gigabyte
# and seconds of it, for the exporter to read the first of, refuse and stop
# the worker over. So the walks that go through an answer anyway add up how
# long its strings are, each as often as it is met, which is how often it
# is written (plain, wire; whole[2]): a string is written as long as it is
# at least, whatever its characters are escaped as, so an answer whose
# strings come to more than limits.max_output_bytes is longer than the
# exporter takes, and is refused unwritten (answer), as one that holds a
# list too often is. The sum is of the values, not of the keys of dicts,
# which no walk goes through, and of strings of exactly that type; an
# answer that is longer than the limit only with its keys, its numbers and
# its punctuation is written, and refused by the exporter, as before.
class Unwritable(Exception): pass
whole=[None,None,0]
met=set()
def weigh(document,steps=1<<62):
    # The least length document is written in, a list or a dict that is in
    # it twice counted twice: a string as long as it is and its quotes, any
    # other value one character, a list or a dict its brackets, a key four
    # for its quotes, colon and space, and as long as it is when it is a
    # string. Lists and dicts are gone through as wire goes through them.
    # -1 when one holds itself, 0 when none is there twice, and None when
    # there is more to go through than steps values. length has the
    # length inside each list and dict done, and -1 for each the walk is
    # inside of; held are those, each with the length so far of the one
    # around it; and done keeps the ones done, so that none is taken for
    # another that was given its place in memory.
    leaves=frozenset((int,float,bool,type(None)))
    length={}; held=[]; done=[]; values=iter((document,)); here=None; n=0; twice=False
    while True:
        for x in values:
            t=type(x)
            if t is str: n+=len(x)+2; continue
            n+=1
            if t in leaves: continue
            if t is not dict and t is not list and t is not tuple and not isinstance(x,(dict,list,tuple)): continue
            known=length.get(id(x))
            if known is None:
                keys=0
                if t is dict:
                    inner=x.values(); keys=4*len(x)
                    try: keys+=sum(map(len,x))
                    except TypeError: pass
                elif isinstance(x,dict): inner=list(dict(x.items()).values()); keys=4*len(inner)
                else: inner=x
                steps-=len(inner)+1
                if steps<0: return None
                length[id(x)]=-1; held.append((values,here,n)); values=iter(inner); here=x; n=1+keys
                break
            if known<0: return -1
            n+=known; twice=True
        else:
            if not held: return n if twice else 0
            length[id(here)]=n; done.append(here)
            below=n; values,here,n=held.pop(); n+=below
def sized():
    # weigh of the answer being written, weighed once.
    if whole[1] is None: whole[1]=weigh(whole[0])
    return whole[1]
def unwritable():
    # Whether the answer holds a list or a dict in itself, or more than
    # once and is longer than limits.max_output_bytes whatever it holds.
    n=sized()
    return n<0 or n>most
def again():
    if unwritable(): raise Unwritable
def plain_check():
    # Whether an answer can be written as it is, without wire: when it is
    # dicts, lists and tuples of strings, numbers, booleans and None, every
    # one of exactly that type, with every float finite, wire changes nothing
    # that json writes, and walking a whole answer through it, a call for
    # each value, cost more than the script that made the answer. The answer
    # is looked through a level at a time, in one loop, with no call for a
    # value. It must also be nested far less deep than wire could follow
    # under the recursion limit as it is now, so that an answer wire would
    # have failed on still fails there: one nested deeper is left to wire, as
    # is whatever else this does not recognise. A transform's answer is
    # looked through no deeper than its metrics go (levels), the values of
    # their labels, so that what a script nested in one, which may hold
    # itself many times over, is not gone through here level after level.
    # A level is every value of the lists and dicts of the one above, one
    # that is there twice taken twice, so the levels of an answer that
    # holds one list many times over, or in itself, grow without end where
    # the script built little. So the values are counted, and each time
    # the count has grown fourfold, from 4096, the answer is weighed as
    # far as a sixty-fourth as many values go: that is all of one whose
    # levels are many times what the script built, and costs one that
    # holds nothing twice a look at one value in fifty. An answer too
    # large to be weighed so is weighed whole when more values are counted
    # than half of limits.max_output_bytes, most: it has two bytes at
    # least for each value in a list or a dict, one of its own and one of
    # the brackets or of the comma and space before it, and is longer than
    # the exporter takes. None says the answer is unwritable, and not to
    # be walked at all. The strings the look passes are added up, one
    # addition for each, and an answer that is plain is left with how
    # long its strings are together (whole[2]).
    recursion_limit=sys.getrecursionlimit
    scalars=frozenset((int,bool,type(None)))
    def plain(document,levels=1<<30):
        level=[document]; room=half=most//2; look=4096; s=0
        for _ in range(min(levels,recursion_limit()//2-10)):
            below=[]
            extend=below.extend
            for v in level:
                t=type(v)
                if t is str: s+=len(v); continue
                if t is dict:
                    extend(v.values())
                    if len(below)>room:
                        if unwritable(): return None
                        room=look=1<<62
                elif t is float:
                    # NaN and the infinities, which less themselves are NaN.
                    if v-v!=0: return False
                elif t is list or t is tuple:
                    extend(v)
                    if len(below)>room:
                        if unwritable(): return None
                        room=look=1<<62
                elif t not in scalars: return False
            if not below: whole[2]=s; return True
            room-=len(below)
            if half-room>look:
                look=(half-room)*4
                n=whole[1]=weigh(document,look>>8)
                if n is not None:
                    if n<0 or n>most: return None
                    look=1<<62
            level=below
        return False
    return plain
plain=plain_check()
del plain_check
def deep_check(deepest):
    # A request and an answer are read and written by json, which calls
    # itself for every list and dict inside another, and an interpreter
    # bounds how deep calls nest: by its recursion limit, 1000 unless a
    # script changed it, up to Python 3.11; from 3.12 by a limit of its own
    # for calls in C, which nothing a script does changes (10,000 of them in
    # a release build on Linux, and from 3.14 what the stack holds). So a
    # response nested as deep as the exporter decodes one, deepest, failed
    # with a RecursionError on its way to a script, and so did what a
    # pre-script left of it on its way back, where wire calls itself too.
    # Both are gone through here in one loop, the lists and dicts being read
    # or written kept in a list, as deep as they go. That is slower than
    # json, and is done only once json has refused for the depth: every
    # other request and answer is read and written as it always was.
    import re
    scanstring=json.decoder.scanstring
    quote=json.encoder.encode_basestring_ascii
    token=re.compile(r'[ \t\n\r]*(?:(")|([\[{])|([\]}])|[,:]|(-?(?:0|[1-9][0-9]*))((?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?)|(NaN|-?Infinity|true|false|null))').match
    words={'true':True,'false':False,'null':None,'NaN':float('nan'),'Infinity':float('inf'),'-Infinity':float('-inf')}
    def loads(text):
        # What json.loads reads of a request, which is JSON as the exporter
        # writes it: a string by json's own reading of one, a number as an
        # int unless it has a fraction or an exponent, and NaN and the
        # infinities by their names.
        whole=[]; inside=[]; at=whole; key=None; i=0
        while True:
            m=token(text,i)
            if m is None: break
            i=m.end()
            quoted,opens,closes,digits,rest,word=m.groups()
            if quoted:
                v,i=scanstring(text,i)
                # In a dict that has no key waiting for its value, a key.
                if key is None and at.__class__ is dict: key=v; continue
            elif opens: v=[] if opens=='[' else {}
            elif closes: at=inside.pop(); continue
            elif digits: v=float(digits+rest) if rest else int(digits)
            elif word: v=words[word]
            else: continue
            if at.__class__ is dict: at[key]=v; key=None
            else: at.append(v)
            if opens: inside.append(at); at=v
        if inside or len(whole)!=1 or text[i:].strip(' \t\n\r'): raise ValueError('the request is not one JSON value')
        return whole[0]
    def key_text(k):
        # A key as json writes it: one that is no string as json makes it
        # one, or refuses it.
        if k.__class__ is str: return quote(k)
        return json.dumps({k:None},allow_nan=False)[1:-7]
    def dumps(document):
        # What json.dumps(wire(document),allow_nan=False) writes: a dict's
        # and a list's or a tuple's values in their order, and any other
        # value as wire and json write it, or refuse it.
        text=[]; put=text.append; inside=[]; none=inside
        v=document
        while True:
            if isinstance(v,dict): put('{'); inside.append([iter(v.items()),'}',''])
            elif isinstance(v,(list,tuple)): put('['); inside.append([iter(v),']',''])
            else:
                put(json.dumps(wire(v),allow_nan=False))
                # Its strings are counted as wire meets them, and it is
                # not written on once they are longer than an answer may be.
                if whole[2]>most: raise too_long('metrics' if 'metrics' in document else 'data')
            # The answer is one dict deeper than what a script left in data.
            if len(inside)>deepest+1: raise too_deep()
            # The next value: of the innermost list or dict that has one
            # left, those that have none being closed.
            while inside:
                at=inside[-1]
                v=next(at[0],none)
                if v is not none: break
                put(at[1]); inside.pop()
            else: return ''.join(text)
            put(at[2]); at[2]=', '
            if at[1]=='}': put(key_text(v[0])); put(': '); v=v[1]
    return loads,dumps
deep_loads,deep_dumps=deep_check(deepest)
del deep_check
def flat(metrics):
    # A metric is flat: a name, a type, a value, a help and a timestamp,
    # each one value, and labels, a dict of one value each. A script that
    # appends to metrics itself can put a list or a dict where one value
    # belongs, which the exporter refuses, naming the metric and what it
    # found there by its kind and how many items it has, and under a key no
    # metric has, which it does not read. What is inside such a list or
    # dict it reads nowhere, so an answer that wire and json could not
    # follow for what one holds, nested deep or holding itself, is written
    # with None for each item of it: the exporter then says of it what it
    # says of any list or dict that stands where it does. An entry, its
    # labels, or a list or a dict in them that is in metrics many times
    # over is made once here and stands in what is made as often, so that
    # this takes no more than the script took to build them; made keeps
    # each with what was made of it, by what it is made as.
    made={}
    def once(make,v):
        got=made.get((make,id(v)))
        if got is None: got=made[make,id(v)]=(make(v),v)
        return got[0]
    def cut(v):
        if isinstance(v,dict): return dict.fromkeys(v)
        return [None]*len(v)
    def value(v):
        if isinstance(v,(dict,list,tuple)): return once(cut,v)
        return v
    def labels(v): return {n:value(x) for n,x in v.items()}
    def entry(m): return {k:once(labels,v) if k=='labels' and isinstance(v,dict) else value(v) for k,v in m.items()}
    return [once(entry,m) if isinstance(m,dict) else value(m) for m in metrics]
def answer(document):
    # An answer that is plain is written as it is; any other, and one whose
    # writing fails, goes through wire as every answer did, so that what is
    # written, or raised, is what it was. But what a pre-script left in data
    # nested deeper than wire and json follow it, which ended there with a
    # RecursionError, is written without them (deep_check), and so are a
    # transform's metrics, without what no metric has (flat). An answer
    # that holds a list or a dict in itself, or more than once and is
    # longer than limits.max_output_bytes whatever is in it (weigh), is
    # not walked through wire, which would copy it out whole: data like
    # that is refused, saying which of the two it is, and metrics are
    # written without what no metric has, as those wire cannot follow,
    # and refused when they are longer than the limit even so. What a
    # script left whose strings alone are longer than the limit, each as
    # often as it is written, is refused the same way, unwritten, by the
    # count of the walk that went through all of it (whole[2]): plain,
    # where the answer is plain, wire, where it went through wire to its
    # end, and otherwise what writes one nested too deep for wire, as it
    # writes. A count that wire did not finish is dropped: what is written
    # then may be without what it counted. An answer that is not a
    # script's data or metrics, an error or a word to the exporter, is
    # written however long.
    left='metrics' if 'metrics' in document else 'data' if 'data' in document else None
    text=None; follow=False; whole[0]=document; whole[1]=None; whole[2]=0
    try:
        follow=plain(document,5) if left=='metrics' else plain(document)
        if follow and (whole[2]<=most or not left): text=json.dumps(document,allow_nan=False)
    except Exception: text=None; follow=False
    if text is None and follow is False:
        whole[2]=0
        try:
            copy=wire(document)
            if whole[2]<=most or not left: text=json.dumps(copy,allow_nan=False)
        except Unwritable: whole[2]=0
        except RecursionError:
            if not left: raise
            whole[2]=0
        finally: met.clear()
    # Outside the handler, so that what is raised here is raised alone.
    if text is None:
        if whole[2]>most: raise too_long(left)
        if left=='metrics':
            document=dict(document,metrics=flat(document['metrics']))
            if weigh(document)>most: raise too_long('metrics')
        else:
            n=sized()
            if n<0: raise too_deep()
            if n>most: raise too_long('data')
        text=deep_dumps(document)
    answers.write(text+'\n'); answers.flush()
def failed_check(most):
    # A script's error was written whole, however long: the traceback with
    # a message as long as the response, joined, written as JSON and sent,
    # each a copy of it, for the exporter to refuse a line past
    # limits.max_output_bytes as output over the limit and stop the worker
    # over it. But of an error past 1,500 bytes the exporter shows only the
    # exception's own line and the frames before it, each line by its first
    # 200 bytes (transform/scripterror.go, shownScriptError). So a long
    # error, and one whose line would be longer than the exporter takes, is
    # not written: the worker makes of it what the exporter would have
    # shown (cut) and writes that, which is within the limit, and carries
    # on. A traceback of ordinary length is joined and written as it
    # always was (failed).
    #
    # What cuts an error is its source here, compiled when an error first
    # needs it (cut): compiled at every start it was a tenth of what
    # starting a worker takes, for an error most workers never see. The
    # source is a raw string, so every backslash in it is the compiler's to
    # read, as it would be were this the launcher's own code.
    source=r'''def cut_check(most):
    import re,collections
    # What the exporter shows of an error and of a line of one, in bytes
    # (scriptErrorBytes, scriptLineBytes), and what it trims an error of
    # (strings.TrimSpace: the characters unicode.IsSpace names).
    whole,start=1500,200
    space='\t\n\v\f\r \x85\xa0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000'
    inked=re.compile('[^'+space+']').search
    halved=re.compile('[\ud800-\udfff]').search
    # Runs of pairs of halves, a thousand pairs at most at a time: the search
    # for a pattern that begins with what the first half is passes quickly
    # over what holds none.
    paired=re.compile('[\ud800-\udbff][\udc00-\udfff](?:[\ud800-\udbff][\udc00-\udfff]){0,1023}').finditer
    unindented=re.compile('^(?! )',re.M).search
    def read(t):
        # t as the exporter reads it from JSON, where a string with half a
        # surrogate pair in it is written with the half: U+FFFD for a half
        # alone, and of two halves the character they are of.
        return t.encode('utf-16-le','surrogatepass').decode('utf-16-le','replace')
    def size(s,a,b,halves=False):
        # How many bytes s[a:b] is in UTF-8, without a copy of all of it,
        # and as the exporter reads it where it holds halves of surrogate
        # pairs (halves): written as it is, a half is the three bytes
        # U+FFFD is, so the two of a pair are two bytes more than the four
        # of their character, a byte for each half of a run of pairs.
        n=0
        for i in range(a,b,65536): n+=len(s[i:min(i+65536,b)].encode('utf-8','surrogatepass'))
        if halves: n-=sum(m.end()-m.start() for m in paired(s,a,b))
        return n
    def inked_to(s):
        # Where the last character of s that is no space ends.
        i=len(s)
        while i>0:
            j=max(0,i-4096); t=s[j:i].rstrip(space)
            if t: return j+len(t)
            i=j
        return 0
    def cut(pieces):
        # The answer line of an error that pieces are the text of: what
        # shownScriptError makes of the whole text, trimmed, by the same
        # rules to the byte, so that the exporter's error is the one it
        # would have made had the text reached it. The text is the lines
        # of the pieces; own is the exception's own line, the first that
        # is not indented after the last that names a frame, or the last
        # line. Shown are the lines from own on, the first that fit in
        # half of whole, or in whole where there is no frame (first), and
        # of those before it the last that fit in the rest (before), each
        # by its first start bytes and its length, with how many lines
        # each part had where some are left out. A line takes its length
        # as the failure is recognised by it, with one byte for a number,
        # and its line break.
        #
        # No long piece is joined to another or copied: each is looked at
        # where the traceback module left it, by the lines that can still
        # be shown, the first after own and the last so far (last), and is
        # let go of before the next is made. A message of a hundred
        # megabytes is in memory as often as the traceback module makes it,
        # and its lines are counted, not gone through. Short pieces, the
        # frames of a long chain of exceptions or the lines of a note, are
        # looked at together, joined until they come to 64 KiB.
        lines=0; own=0; skipping=False; first=[]; used=0; full=False; before=[]
        last=collections.deque(); kept=0; text=[]; chars=0; halves=False
        def line(s,x,y,plain,known):
            # The line s[x:y] as it is shown: its text or its first start
            # bytes, cut between two characters; its length in bytes when
            # it is cut; and what it takes.
            r=known.get(x)
            if r is None:
                n=y-x if plain else size(s,x,y,halves)
                if n<=start: r=(read(s[x:y]) if halves else s[x:y],None,n+1)
                elif plain: r=(s[x:x+start],n,start+14)
                else:
                    head=s[x:x+start+1]; head=(read(head) if halves else head).encode('utf-8'); k=start
                    while head[k]&192==128: k-=1
                    r=(head[:k].decode('utf-8'),n,k+14)
                known[x]=r
            return r
        def back(s,a,z,plain,known):
            # The last lines of s[a:z], in their order, as many as can be
            # shown at most, and whether they are all of them.
            out=[]; t=0
            while True:
                x=max(s.rfind('\n',a,z)+1,a)
                r=line(s,x,z,plain,known); out.append(r); t+=r[2]
                if x==a or t>=2*whole: out.reverse(); return out,x==a
                z=x-1
        def take(s,a,b,ends):
            # s[a:b] is the next lines of the text: whole lines, the last
            # ended by the line break before b, or by b where the text ends.
            nonlocal lines,own,skipping,first,used,full,before,kept,text,chars,halves
            e=b if ends else b-1
            plain=s.isascii(); known={}; halves=not plain and halved(s) is not None
            if text is not None:
                # The text itself, while it may be one the exporter shows whole.
                chars+=b-a
                if chars>whole: text=None
                else: text.append(s[a:b])
            # The last line here that names a frame: own is after it.
            f=s.rfind('\n  File ',a,e)+1
            if not f: f=a if s.startswith('  File ',a,e) else -1
            x=a
            if f>=0:
                skipping=True; first=[]; used=0; full=False
                x=s.find('\n',f,e)+1 or None
            if skipping and x is not None:
                m=unindented(s,x,e)
                x=m and m.start()
                if m:
                    skipping=False; own=lines+s.count('\n',a,x)
                    before,whence=back(s,a,x-1,plain,known) if x>a else ([],True)
                    if whence: before=list(last)+before
            if not skipping and not full:
                room=whole//2 if own else whole
                while True:
                    y=s.find('\n',x,e)
                    if y<0: y=e
                    r=line(s,x,y,plain,known)
                    if first and used+r[2]>room: full=True; break
                    first.append(r); used+=r[2]
                    if y==e: break
                    x=y+1
            lines+=s.count('\n',a,e)+1
            recent,whence=back(s,a,e,plain,known)
            if not whence: last.clear(); kept=0
            last.extend(recent); kept+=sum(r[2] for r in recent)
            while kept-last[0][2]>=2*whole: kept-=last.popleft()[2]
        # The text is trimmed as the exporter trims it. What stands before
        # its first character that is no space is passed over; a piece is
        # held until another that is not blank follows it, and the blank
        # ones after it with it, so that the last of them is taken only as
        # far as its last character that is no space. A piece that does not
        # end a line, which the traceback module does not make, is joined
        # to the next. A string that holds half a surrogate pair is left as
        # it is, however long: what is shown of it and the lengths of its
        # lines are made of it as the exporter reads it (read, size).
        held=None; blank=[]; carried=''; short=[]; n=0
        def chunk(s):
            nonlocal held,carried
            if carried: s=carried+s; carried=''
            if not s.endswith('\n'): carried=s; return
            m=inked(s)
            if m is None:
                if held: blank.append(s)
                return
            if held:
                take(held[0],held[1],len(held[0]),False)
                for b in blank: take(b,0,len(b),False)
                blank.clear()
                held=(s,0)
            else: held=(s,m.start())
        for piece in pieces:
            # A long piece that is held is let go of with the next piece.
            if len(piece)<65536 and not (held and len(held[0])>=65536):
                short.append(piece); n+=len(piece)
                if n<65536: continue
                piece=None
            if short: chunk(''.join(short)); short.clear(); n=0
            if piece: chunk(piece)
        piece=None
        # The last line of the text ends with it.
        chunk(''.join(short)+'\n')
        if not held: return json.dumps({'ok':False,'error':''})
        take(held[0],held[1],inked_to(held[0]),True)
        held=None
        if text is not None:
            # A text of whole bytes or fewer is shown whole.
            text=read(''.join(text))
            if size(text,0,len(text))<=whole:
                written=json.dumps({'ok':False,'error':text})
                if len(written)<=most: return written
        if skipping:
            # No line after the last frame is not indented: the last is own.
            before=list(last); own=lines-1; first=[before.pop()]
        def shown(budget):
            # The error as it is shown in budget bytes, in parts: a string,
            # or a number that was measured, which the exporter writes as it
            # is and recognises the failure without (shownByWorker).
            room=budget//2 if own else budget
            n=1; u=first[0][2]
            while n<len(first) and u+first[n][2]<=room: u+=first[n][2]; n+=1
            i=len(before)
            while i>0 and u+before[i-1][2]<=budget: i-=1; u+=before[i][2]
            out=['... (',own,' lines)'] if len(before)-i<own else ['']
            for r in before[i:]+first[:n]:
                out[-1]+=('\n' if len(out)>1 or out[0] else '')+r[0]
                if r[1] is not None: out[-1]+='... ('; out+=[r[1],' bytes)']
            if n<lines-own: out[-1]+='\n... ('; out+=[lines-own,' lines)']
            return out
        # What the exporter would show is some 1,600 bytes, and as JSON at
        # most six times that. Under a limit too small for it the error is
        # what fits: shown in half the bytes, and half again, which leaves
        # out frames and then lines of the message, down to the exception's
        # own line by its start; and under a limit too small for that, as
        # many of that line's first characters as fit.
        budget=whole
        while True:
            written=json.dumps({'ok':False,'shown':shown(budget)})
            if len(written)<=most or not budget: break
            budget//=2
        n=len(first[0][0])
        while len(written)>most and n>=0:
            written=json.dumps({'ok':False,'error':first[0][0][:n]}); n-=1
        return written
    return cut
'''
    cutter=[]
    def cut(pieces):
        # The line of an error that is cut, by what source defines: compiled
        # for the first of them, and kept once it is whole, so that a fault
        # in making it leaves it to be made for the next.
        if not cutter:
            scope={'json':json}
            exec(compile(source,'<string>','exec'),scope)
            cutter.append(scope['cut_check'](most))
        return cutter[0](pieces)
    def line_of(pieces):
        # The answer line of an error pieces are the text of. An error of
        # 16,384 or fewer characters whose line is within the limit is
        # joined and written whole, the line it always was.
        # What the exporter shows of an error is within 16 KiB however it
        # is written, so no error that reached the exporter whole under a
        # smaller limit is cut here to less than was shown of it. The
        # pieces of any other error are cut one by one, unjoined, whatever
        # the limit is: the worker makes no line of megabytes for the
        # exporter to show a few lines of.
        pieces=iter(pieces); early=[]; n=0
        for piece in pieces:
            early.append(piece); n+=len(piece)
            if n>16384: break
        else:
            written=json.dumps({'ok': False, 'error': ''.join(early)})
            if len(written)<=most: return written
        piece=None
        def rest():
            early.reverse()
            while early: yield early.pop()
            yield from pieces
        return cut(rest())
    def named(kind):
        # A type as a traceback names it, by its first 80 characters, and
        # whatever asking a script's class for its name does.
        try:
            name,module=kind.__qualname__,kind.__module__
            if module not in ('builtins','__main__'): name=(module if type(module) is str else '<unknown>')+'.'+name
            if type(name) is str: return name[:80]
        except BaseException: pass
        return 'an exception'
    def failed(e,said=None):
        # Answers that the script failed with e: with said, the pieces of
        # the error's text, or with e's traceback (script_error, line_of).
        #
        # Making that line can fail too: the traceback module makes a copy
        # of the message, which limits.max_script_memory may not hold beside
        # the message, and whatever else is raised in here was raised with
        # nothing around it, so the interpreter ended, the run failed as a
        # worker's and not as the script's, and the next paid for another.
        # So whatever is raised, the answer is still the script's failure:
        # its exception's type, and that the text could not be written and
        # of what, which is all that is certain to fit in memory and in the
        # limit (as many of its first characters as fit, under a limit too
        # small for it). Nothing has been written when it is made, the line
        # being written here alone, and what the attempt held is let go of
        # with the fault, whose traceback holds it, before it is made.
        try: written=line_of(script_error(e) if said is None else said)
        except BaseException as fault: written=None; why=type(fault)
        if written is None:
            text='%s: (the text of this error could not be written: %s)'%(named(type(e)),named(why))
            n=len(text)
            while True:
                written=json.dumps({'ok': False, 'error': text[:n]})
                if len(written)<=most or not n: break
                n-=1
        answers.write(written+'\n'); answers.flush()
    return failed
failed=failed_check(most)
del failed_check
answer({'ok': True, 'ready': True})
while True:
    line=requests.readline()
    if not line: break
    try:
        # A request nested deeper than json reads one is read without it.
        try: p=json.loads(line)
        except RecursionError: p=None
        if p is None: p=deep_loads(line)
        # The request's text is not kept while the script runs, and data
        # that is the body is the body's own string, not a copy.
        line=None
        if p.get('data_is_body'): p['data']=p['response']['body']
        # The request is read: the script's time, limits.script_timeout,
        # starts when the exporter reads this line.
        answer({'started': True})
        metrics=[]
        def metric(name,type='gauge',value=0,labels=None,help=None,timestamp=None,_metrics=metrics):
            if not isinstance(name,str): raise ValueError('metric name %s is not a string'%shown(name))
            if type is None: type='gauge'
            if not isinstance(type,str): raise ValueError('metric %r type %s is not a string; give "gauge", "counter" or "untyped"'%(name,shown(type)))
            if help is not None and not isinstance(help,str): raise ValueError('metric %r help %s is not a string'%(name,shown(help)))
            if labels is None: labels={}
            if not isinstance(labels,dict): raise ValueError('metric %r labels must be a mapping of label names to values, not a %s'%(name,labels.__class__.__name__))
            # A label that is a string already is its own text, which is
            # what label_text returns for it: asking for each one, through
            # a generator, was a third of what a call of metric costs.
            texts={}
            for k,t in labels.items():
                if t.__class__ is not str:
                    t=label_text(k,t)
                    if t is None: continue
                texts[str(k)]=t
            labels=texts
            value=metric_number(name,'value',value)
            if timestamp is not None:
                timestamp=metric_number(name,'timestamp',timestamp)
                if timestamp!=timestamp or timestamp in (float('inf'),float('-inf')): raise ValueError('metric %r timestamp is not a number of milliseconds'%name)
                timestamp=int(timestamp)
            _metrics.append({'name':name,'type':type,'value':value,'labels':labels,'help':help or '','timestamp':timestamp})
        def fail(message): raise RuntimeError(str(message))
        scope={'__builtins__':builtins,'__name__':'__collector__','sys':sys,'json':json,'builtins':builtins,'contextlib':contextlib,'io':io,'os':os,
               'metric':metric,'fail':fail,'Response':Response,'response':Response(p['response']),'target':p['target'],'collector':p['collector'],'data':p['data'],'metrics':metrics}
        sink=io.StringIO()
        # The script's source, for its lines in a traceback.
        linecache.cache['<collector-python>']=(len(p['script']),None,p['script'].splitlines(True),'<collector-python>')
        with contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
            exec(compile(p['script'],'<collector-python>','exec'),scope,scope)
        log=sink.getvalue()
        if len(log)>4096: log=log[:4096]+'... (%d more characters)'%(len(log)-4096)
        result={'ok':True,'log':log}
        if p.get('mode')=='data': result['data']=scope.get('data')
        else: result['metrics']=metrics
        answer(result)
    except MemoryError as e:
        failed(e,('MemoryError: the script ran out of memory under limits.max_script_memory (%d bytes)'%max_memory,) if max_memory>0 else None)
    except BaseException as e:
        failed(e)`
