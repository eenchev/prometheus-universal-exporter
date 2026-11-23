package transform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// freshStates empties the states kept, before the test and after it.
func freshStates(t *testing.T) {
	t.Helper()
	reset := func() {
		pythonStates.mu.Lock()
		pythonStates.kept, pythonStates.clock, pythonStates.bytes, pythonStates.dropped = map[pythonStateKey]*pythonStateEntry{}, 0, 0, nil
		pythonStates.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// runWithState runs c's transform script for target, with ctx, and returns
// the value of its one series.
func runWithState(ctx context.Context, t *testing.T, c *model.Collector, target string) (float64, error) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(`{}`), Headers: http.Header{}, Target: target}
	set, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "{}", Raw: r.Body}, r, c)
	if err != nil {
		return 0, err
	}
	if len(set.Metrics) != 1 {
		t.Fatalf("series %+v", set.Metrics)
	}
	return set.Metrics[0].Value, nil
}

// A collector with transform.state gives its script a dict named state
// that is kept from one run to the next: what a run leaves in it is what
// the next run for the same target finds, and each target has its own, as
// have a pre-script and a transform. Without the key there is no such
// name, and nothing of state goes to the worker or comes from it.
func TestAPythonScriptsStateIsKeptBetweenItsRuns(t *testing.T) {
	requirePython(t)
	freshStates(t)
	counting := workerCollector("counting", `state["runs"] = state.get("runs", 0) + 1
state.setdefault("seen", []).append(target)
metric(name="runs", value=state["runs"] * 100 + len(state["seen"]))`)
	counting.Transform.State = true
	for i, want := range []struct {
		target string
		value  float64
	}{{"a", 101}, {"a", 202}, {"b", 101}, {"a", 303}, {"b", 202}} {
		if got, err := runWithState(context.Background(), t, counting, want.target); err != nil || got != want.value {
			t.Fatalf("run %d, for %s: %v, %v; want %v", i+1, want.target, got, err, want.value)
		}
	}
	// A probe with parameters has a state of its own for them.
	withParams := WithLabelParams(context.Background(), map[string]string{"param_tenant": "one"})
	for _, want := range []float64{101, 202} {
		if got, err := runWithState(withParams, t, counting, "a"); err != nil || got != want {
			t.Fatalf("with a parameter: %v, %v; want %v", got, err, want)
		}
	}
	// A pre-script's state is its own, beside the transform's.
	pre := workerCollector("counting", `metric(name="runs", value=state.get("runs", 0) * 100 + data)`)
	pre.Transform.State, pre.Transform.PreScript = true, `state["pre"] = state.get("pre", 0) + 1
data = state["pre"]`
	for run := 1; run <= 2; run++ {
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(`{}`), Headers: http.Header{}, Target: "a"}
		data, err := executePythonPreScript(context.Background(), "python3", pre.Transform.PreScript, &decode.Decoded{Kind: "text", Data: "{}", Raw: r.Body}, r, pre)
		if err != nil || fmt.Sprint(data) != fmt.Sprint(run) {
			t.Fatalf("the pre-script's run %d left %v, %v", run, data, err)
		}
	}
	if got, err := runWithState(context.Background(), t, counting, "a"); err != nil || got != 404 {
		t.Fatalf("the transform's state after the pre-script's runs: %v, %v; want 404", got, err)
	}
	// Without the key, state is a name the script does not have.
	plain := workerCollector("plain", `metric(name="runs", value=state.get("runs", 0))`)
	if _, err := runWithState(context.Background(), t, plain, "a"); err == nil || !strings.Contains(err.Error(), "NameError: name 'state' is not defined") {
		t.Fatalf("without transform.state: %v", err)
	}
}

// What a script that failed left in state is not kept, and neither is
// what the script of a debug probe left: the next run finds what the last
// run that ended well left. A state that is no dict when the script ends,
// that JSON cannot carry, or that is longer than 64 KiB fails the script,
// saying which, and is not kept either.
func TestAPythonScriptsStateIsKeptOnlyOfARunThatEndedWell(t *testing.T) {
	requirePython(t)
	freshStates(t)
	script := func(after string) *model.Collector {
		c := workerCollector("careful", `state["runs"] = state.get("runs", 0) + 1
metric(name="runs", value=state["runs"])
`+after)
		c.Transform.State = true
		return c
	}
	if got, err := runWithState(context.Background(), t, script(""), "a"); err != nil || got != 1 {
		t.Fatalf("the first run: %v, %v", got, err)
	}
	for name, tc := range map[string]struct{ after, want string }{
		"a failure":          {`fail("no")`, "RuntimeError: no"},
		"no dict":            {`state = [1]`, "TypeError: state is a list when the script ends; it is the dict the exporter keeps for the script's next run, so change what is in it and leave it a dict"},
		"not JSON":           {`state["when"] = {1, 2}`, "TypeError: state holds what cannot be kept for the script's next run: Object of type set is not JSON serializable"},
		"a float JSON lacks": {`state["x"] = float("nan")`, "TypeError: state holds what cannot be kept for the script's next run: Out of range float values are not JSON compliant"},
		"too long":           {`state["big"] = "x" * 70000`, "OverflowError: state is 70022 bytes as JSON when the script ends, and the exporter keeps at most 65536 for a script and target; keep less in it"},
	} {
		if _, err := runWithState(context.Background(), t, script(tc.after), "a"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v\nwant %s", name, err, tc.want)
		}
	}
	// A debug probe's run reads the state and leaves it as it was.
	if got, err := runWithState(WithMemoryMeasured(context.Background()), t, script(""), "a"); err != nil || got != 2 {
		t.Fatalf("a debug probe's run: %v, %v; want 2, of the state the first run left", got, err)
	}
	if got, err := runWithState(context.Background(), t, script(""), "a"); err != nil || got != 2 {
		t.Fatalf("the run after those: %v, %v; want 2, the second that ended well", got, err)
	}
	// A state emptied is kept as none.
	if _, err := runWithState(context.Background(), t, script(`state.clear(); state["runs"] = 0; del state["runs"]; state["runs"] = 9; state.pop("runs"); state["runs"] = 5; state.clear(); state["runs"] = 0`), "a"); err != nil {
		t.Fatal(err)
	}
	pythonStates.mu.Lock()
	kept := len(pythonStates.kept)
	pythonStates.mu.Unlock()
	if kept != 1 {
		t.Fatalf("%d states are kept, want the one", kept)
	}
}

// The exporter keeps the states of the targets used last, pythonStatesKept
// of them: one more puts out the one used longest ago, a read being a use.
func TestTheStatesKeptAreBounded(t *testing.T) {
	freshStates(t)
	key := func(i int) pythonStateKey {
		return pythonStateKey{collector: "c", what: "transform", target: fmt.Sprint("target", i)}
	}
	for i := range pythonStatesKept {
		pythonStates.put(key(i), `{"n":1}`)
	}
	if got := pythonStates.get(key(0)); got != `{"n":1}` {
		t.Fatalf("the first state: %s", got)
	}
	pythonStates.put(key(pythonStatesKept), `{"n":2}`)
	if a, b, c := pythonStates.get(key(0)), pythonStates.get(key(1)), pythonStates.get(key(pythonStatesKept)); a != `{"n":1}` || b != "{}" || c != `{"n":2}` {
		t.Fatalf("after one more than is kept: the state read last is %s, the one used longest ago %s, the new one %s", a, b, c)
	}
	// The states kept are bounded in bytes too: of large ones, fewer.
	large := `{"v":"` + strings.Repeat("x", 60<<10) + `"}`
	for i := range 1200 {
		pythonStates.put(key(10000+i), large)
	}
	pythonStates.mu.Lock()
	bytes, counted := pythonStates.bytes, 0
	for _, entry := range pythonStates.kept {
		counted += len(entry.json)
	}
	pythonStates.mu.Unlock()
	if bytes != counted || bytes > pythonStateBytesKept || bytes < pythonStateBytesKept-len(large) {
		t.Fatalf("%d bytes of states are kept, counted as %d, want just under %d", counted, bytes, pythonStateBytesKept)
	}
	if c := PythonStateCounts()["c"]; c.Bytes != bytes || c.States*len(large) > pythonStateBytesKept || c.Dropped != uint64(1+pythonStatesKept+1200-c.States) {
		t.Fatalf("the collector's states are counted as %+v, of %d bytes kept", c, bytes)
	}
	if got := pythonStates.get(key(10000 + 1199)); got != large {
		t.Fatal("the state kept last is gone")
	}
	freshStates(t)
	for i := range pythonStatesKept {
		pythonStates.put(key(i), `{"n":1}`)
	}
	pythonStates.put(key(5), "{}")
	pythonStates.mu.Lock()
	kept := len(pythonStates.kept)
	pythonStates.mu.Unlock()
	if kept != pythonStatesKept-1 {
		t.Fatalf("%d states are kept after one was emptied, want %d", kept, pythonStatesKept-1)
	}
}
