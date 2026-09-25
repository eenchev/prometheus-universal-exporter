package exporter

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Identical probes that arrive while one is already in flight share it. Two or
// three Prometheus replicas scraping the same targets on the same interval
// tend to probe at the same moment, and without this each of them goes to the
// target: a slow endpoint is hit several times over, and a rate-limited one
// can start refusing. With it, the first probe goes to the target and the
// others wait for its answer and get an exact copy of it — status, headers and
// body, a failure included.
//
// Identical means the same key the response cache uses (probeCacheKey): the
// same collector definition, target, probe parameters and forwarded headers,
// credentials included. Two probes that could get different answers never
// share one. The response cache only helps once a probe has finished; this
// covers the probes that arrive while it is still running, and it works for a
// collector without a cache too.
//
// The shared work belongs to no single caller. It runs detached from the probe
// that started it, so that probe's client going away does not fail the others,
// and it is cancelled only when every probe waiting on it has gone.

// probeResult is a finished probe, recorded so it can be written to any number
// of callers.
type probeResult struct {
	status int
	header http.Header
	body   []byte
	// ok is whether the probe counts as a success in the self-metrics.
	ok bool
	// abandoned says the trip was cancelled because every probe waiting for
	// it went away: nobody reads it, and it is not the target's failure.
	abandoned bool
	// unauthorized says the target refused the trip's credential
	// (collected.unauthorized): no stale result answers it.
	unauthorized bool
}

func (p *probeResult) writeTo(w http.ResponseWriter) {
	for key, values := range p.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(p.status)
	_, _ = w.Write(p.body)
}

// probeRecorder is the http.ResponseWriter the shared work writes into.
type probeRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newProbeRecorder() *probeRecorder {
	return &probeRecorder{header: http.Header{}, status: http.StatusOK}
}

func (r *probeRecorder) Header() http.Header { return r.header }

func (r *probeRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *probeRecorder) WriteHeader(status int) { r.status = status }

func (r *probeRecorder) result(ok bool) *probeResult {
	return &probeResult{status: r.status, header: r.header, body: r.body.Bytes(), ok: ok}
}

type probeFlight struct {
	done    chan struct{}
	result  *probeResult
	waiters int
	cancel  context.CancelFunc
}

type probeFlights struct {
	mu      sync.Mutex
	flights map[string]*probeFlight
}

func newProbeFlights() *probeFlights {
	return &probeFlights{flights: map[string]*probeFlight{}}
}

// do runs work for key, or joins the run already in flight for it. shared
// reports whether the result came from another caller's run. err is only this
// caller's own context ending while it waited; the work's failures are in the
// result.
func (f *probeFlights) do(ctx context.Context, key string, work func(context.Context) *probeResult) (result *probeResult, shared bool, err error) {
	f.mu.Lock()
	flight, joining := f.flights[key]
	if joining {
		flight.waiters++
	} else {
		workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &probeFlight{done: make(chan struct{}), waiters: 1, cancel: cancel}
		f.flights[key] = flight
		go f.run(workCtx, key, flight, work)
	}
	f.mu.Unlock()

	select {
	case <-flight.done:
		return flight.result, joining, nil
	case <-ctx.Done():
		f.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 {
			// Nobody is left to answer. Cancel the work, and let a probe that
			// arrives from now on start afresh rather than join a cancelled run.
			flight.cancel()
			if f.flights[key] == flight {
				delete(f.flights, key)
			}
		}
		f.mu.Unlock()
		return nil, joining, ctx.Err()
	}
}

func (f *probeFlights) run(ctx context.Context, key string, flight *probeFlight, work func(context.Context) *probeResult) {
	defer func() {
		// The work runs on its own goroutine, where a panic would take the
		// whole exporter down rather than one request as it would in a handler.
		if recovered := recover(); recovered != nil {
			recorder := newProbeRecorder()
			http.Error(recorder, fmt.Sprintf("probe failed: internal error: %v", recovered), http.StatusInternalServerError)
			flight.result = recorder.result(false)
			slog.Default().Error("probe panicked", "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
		}
		f.mu.Lock()
		if f.flights[key] == flight {
			delete(f.flights, key)
		}
		f.mu.Unlock()
		flight.cancel()
		close(flight.done)
	}()
	flight.result = work(ctx)
}

// inFlight reports how many distinct probes are running, for tests.
func (f *probeFlights) inFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.flights)
}

// coalesceProbes reports whether a collector shares identical concurrent
// probes. It does unless the collector sets coalesce: false.
func coalesceProbes(c *model.Collector) bool {
	return c.Coalesce == nil || *c.Coalesce
}
