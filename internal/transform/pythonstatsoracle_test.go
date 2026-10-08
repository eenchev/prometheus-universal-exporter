package transform

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// oraclePool is the worker pool as it counted before a collector's
// statistics followed a reload: every count kept under the collector's name,
// asked for by that name wherever something was counted, and kept for the
// life of the pool. Its methods are the pool's as they were then, word for
// word but for the names of the types, so a test can hold the pool to them
// for whatever no reload retires (pythonstats_test.go).
type oraclePool struct {
	mu         sync.Mutex
	idle       map[string][]*pythonWorker
	started    atomic.Int64
	stats      map[string]*oracleCollectorStats
	busy       map[string]int
	obsolete   map[string]bool
	closed     bool
	maxWorkers int
	live       int
	waiting    []chan struct{}
	start      func(ctx context.Context, spec pythonSpec) (*pythonWorker, error)
}

type oracleCollectorStats struct {
	starting, busy int
	starts         uint64
	startFailures  uint64
	stops          map[string]uint64
	runs           map[string]uint64
}

func newOraclePool() *oraclePool {
	return &oraclePool{idle: map[string][]*pythonWorker{}, stats: map[string]*oracleCollectorStats{}, busy: map[string]int{}, obsolete: map[string]bool{}}
}

// SetMaxWorkers bounds the workers alive at once, as the pool's does.
func (p *oraclePool) SetMaxWorkers(limit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxWorkers = limit
	for len(p.waiting) > 0 {
		p.notifyLocked()
	}
}

// notifyLocked wakes the first run waiting for a worker; mu is held.
func (p *oraclePool) notifyLocked() {
	if len(p.waiting) == 0 {
		return
	}
	first := p.waiting[0]
	p.waiting = p.waiting[1:]
	close(first)
}

// stopLocked stops a worker that is no longer counted as busy or idle, and
// frees its place; mu is held.
func (p *oraclePool) stopLocked(worker *pythonWorker) {
	worker.stop()
	p.live--
	p.notifyLocked()
}

// statsLocked returns a collector's statistics, creating them; mu is held.
func (p *oraclePool) statsLocked(collector string) *oracleCollectorStats {
	st := p.stats[collector]
	if st == nil {
		st = &oracleCollectorStats{stops: map[string]uint64{}, runs: map[string]uint64{}}
		p.stats[collector] = st
	}
	return st
}

func (p *oraclePool) count(collector string, update func(*oracleCollectorStats)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	update(p.statsLocked(collector))
}

// recordRun counts how a run ended. The pool cannot tell a script's own error
// from a successful answer, so the caller, which reads the answer, records it.
func (p *oraclePool) recordRun(collector, outcome string) {
	p.count(collector, func(st *oracleCollectorStats) { st.runs[outcome]++ })
}

// discard stops a busy worker and counts why.
func (p *oraclePool) discard(worker *pythonWorker, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked(worker)
	st := p.statsLocked(worker.collector)
	st.busy--
	st.stops[reason]++
	p.unbusyLocked(worker.key)
}

// unbusyLocked counts a worker of key as no longer busy; mu is held.
func (p *oraclePool) unbusyLocked(key string) {
	p.busy[key]--
	if p.busy[key] <= 0 {
		delete(p.busy, key)
		delete(p.obsolete, key)
	}
}

// acquire gives a worker for spec: an idle one of the pool, which reused
// then says, or one it starts.
func (p *oraclePool) acquire(ctx context.Context, spec pythonSpec) (*pythonWorker, bool, error) {
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
func (p *oraclePool) evictIdleLocked() bool {
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

func (p *oraclePool) release(spec pythonSpec, worker *pythonWorker) {
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
func (p *oraclePool) reapLocked(now time.Time) {
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

// reapIdle stops the workers idle for longer than the idle timeout.
func (p *oraclePool) reapIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reapLocked(now)
}

// Retain makes keys the scripts in use: idle workers of any other are stopped
// at once, and busy ones when they finish.
func (p *oraclePool) Retain(keys map[string]bool) {
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

// Snapshot copies a collector's statistics and counts its idle workers.
func (p *oraclePool) Snapshot(collector string) pythonWorkerSnapshot {
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
func (p *oraclePool) PoolSnapshot() pythonWorkerSnapshot {
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
