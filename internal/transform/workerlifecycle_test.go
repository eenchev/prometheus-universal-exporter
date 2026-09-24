package transform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The timer adds up the scripts a probe runs and says whether any ran.
func TestScriptTimerSumsTheRuns(t *testing.T) {
	ctx, timer := WithScriptTimer(context.Background())
	if _, ran := timer.Seconds(); ran {
		t.Fatal("a fresh timer says a script ran")
	}
	scriptTimerFrom(ctx).add(30 * time.Millisecond)
	scriptTimerFrom(ctx).add(20 * time.Millisecond)
	if seconds, ran := timer.Seconds(); !ran || seconds < 0.049 || seconds > 0.051 {
		t.Fatalf("seconds=%v ran=%v", seconds, ran)
	}
	if scriptTimerFrom(context.Background()) != nil {
		t.Fatal("a context without a timer has one")
	}
}

// idleWorkers counts a collector's idle workers.
func idleWorkers(collector string) int { return PythonWorkers().Snapshot(collector).Idle }

// ageIdleWorkers makes a collector's idle workers look idle for longer than
// the idle timeout.
func ageIdleWorkers(collector string) {
	PythonWorkers().mu.Lock()
	defer PythonWorkers().mu.Unlock()
	for _, workers := range PythonWorkers().idle {
		for _, worker := range workers {
			if worker.collector == collector {
				worker.idleSince = time.Now().Add(-pythonWorkerIdleTimeout - time.Minute)
			}
		}
	}
}

// Idle workers are stopped on a timer, though nothing asks for a worker again.
func TestIdleWorkersAreReapedOnATimer(t *testing.T) {
	requirePython(t)
	c := workerCollector("reaped_on_timer", `metric(name="v", value=1)`)
	if _, err := runWorkerScript(t, c); err != nil {
		t.Fatal(err)
	}
	if idleWorkers(c.Name) != 1 {
		t.Fatalf("idle=%d before reaping", idleWorkers(c.Name))
	}
	before := PythonWorkers().Snapshot(c.Name).Stops[pythonStopIdle]
	ageIdleWorkers(c.Name)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		PythonWorkers().ReapLoop(ctx, 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for idleWorkers(c.Name) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the idle worker was never reaped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := PythonWorkers().Snapshot(c.Name).Stops[pythonStopIdle]; got != before+1 {
		t.Fatalf("idle stops %d, want %d", got, before+1)
	}
}

// Which counter an error raises depends on its kind, whatever its message.
func TestErrorKindsAreMarkedNotGuessedFromText(t *testing.T) {
	marked := model.MarkError(errors.New("anything at all"), model.ErrMissingValue)
	wrapped := fmt.Errorf("outer: %w", &MetricFailure{Collector: "c", Metric: "m", Err: marked})
	if !errors.Is(wrapped, model.ErrMissingValue) || errors.Is(wrapped, model.ErrScriptFailed) || errors.Is(wrapped, model.ErrLimitExceeded) {
		t.Fatal("the kind does not survive wrapping, or leaks into another")
	}
	if wrapped.Error() != "outer: anything at all" {
		t.Fatalf("marking changed the message: %q", wrapped)
	}
	if model.MarkError(nil, model.ErrMissingValue) != nil {
		t.Fatal("marking nil made an error")
	}
	if errors.Is(errors.New("value is missing; response size exceeds limit; python failed"), model.ErrMissingValue) {
		t.Fatal("an unmarked error was classified by its text")
	}
}
