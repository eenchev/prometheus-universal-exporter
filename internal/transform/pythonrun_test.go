package transform

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// How a script's run is handed to a worker, timed, and ended, and what
// becomes of workers that die (pythonworker.go, python.go).

// runWorkerText runs c's script on a text response of the given body.
func runWorkerText(ctx context.Context, c *model.Collector, body string) (*model.MetricSet, error) {
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	return executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
}

// The response's body goes to the worker once: the request is not much
// larger than the body, where a copy each for response.text and for data
// made it three times the size, and the script still has all three, as one
// string.
func TestPythonRequestCarriesTheBodyOnce(t *testing.T) {
	body := strings.Repeat("requests 12345 worker=a\n", 1<<16)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	c := workerCollector("once", `metric(name="same", value=1 if response.text is response.body and data is response.body else 0)
metric(name="length", value=len(response.text))`)
	payload, err := pythonRequest("metrics", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > len(body)+len(body)/4 || bytes.Count(payload, []byte("requests 12345 worker=a")) != 1<<16 {
		t.Fatalf("the request for a body of %d bytes is %d bytes", len(body), len(payload))
	}
	// Data that is not the body, as after a pre-script or from another
	// decoder, is sent as it is.
	other, err := pythonRequest("metrics", c.Transform.Script, &decode.Decoded{Kind: "json", Data: map[string]any{"a": "changed"}, Raw: r.Body}, r, c)
	if err != nil || !bytes.Contains(other, []byte(`"data":{"a":"changed"}`)) || bytes.Contains(other, []byte("data_is_body")) {
		t.Fatalf("%v %.200s", err, other)
	}
	requirePython(t)
	set, err := runWorkerText(context.Background(), c, body)
	if err != nil {
		t.Fatal(err)
	}
	if workerMetricValue(t, set, "same") != 1 || workerMetricValue(t, set, "length") != float64(len(body)) {
		t.Fatalf("%+v", set.Metrics)
	}
}

// heldWorker stands in for a worker's process: what it returns takes a
// request as a worker does, a line from the pipe, and then answers as
// answer says, a line at a time, each after its delay.
func heldWorker(t *testing.T, answers ...heldAnswer) *pythonWorker {
	t.Helper()
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(requestRead, requestWrite) })
	// It has no process, which its command says, so a pool can stop it.
	worker := &pythonWorker{collector: "held", cmd: &exec.Cmd{}, requests: requestWrite, lines: make(chan pythonLine, 1), exited: make(chan struct{}), stderr: &tailBuffer{max: pythonStderrTail}}
	go func() {
		if _, err := bufio.NewReader(requestRead).ReadString('\n'); err != nil {
			return
		}
		for _, answer := range answers {
			time.Sleep(answer.after)
			worker.lines <- pythonLine{data: []byte(answer.line)}
		}
	}()
	return worker
}

type heldAnswer struct {
	after time.Duration
	line  string
}

// limits.script_timeout measures the script's own run: its clock starts
// when the worker says it has read the request, so a worker that takes
// longer than the timeout to read a large response, and then runs its script
// in no time, does not time out; a script that runs longer than the timeout
// does, however quickly the request was read.
//
// The workers are stand-ins and the clock the test's own (testing/synctest),
// so the reading and the running take exactly the time given here, and the
// script is said to have run for exactly what it ran. On the machine's
// clock the first held only where the test was not left waiting for the
// timeout between the worker's word and its answer, which was there
// already, and the second only where the test took the worker's word
// within two timeouts of its being given.
func TestPythonScriptTimeoutStartsWhenTheWorkerHasTheRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 300 * time.Millisecond
		slowToRead := heldWorker(t, heldAnswer{2 * timeout, pythonRequestTaken}, heldAnswer{0, `{"ok": true}`})
		began := time.Now()
		line, ran, err := slowToRead.call(context.Background(), []byte(`{}`), timeout)
		if err != nil || string(line) != `{"ok": true}` {
			t.Fatalf("a request read in %s and run at once, under a timeout of %s: %q %v", 2*timeout, timeout, line, err)
		}
		if took := time.Since(began); ran != 0 || took != 2*timeout {
			t.Fatalf("the script is said to have run %s of a run of %s, want none of the %s the request was read in", ran, took, 2*timeout)
		}
		slowToRun := heldWorker(t, heldAnswer{0, pythonRequestTaken}, heldAnswer{3 * timeout, `{"ok": true}`})
		if _, ran, err := slowToRun.call(context.Background(), []byte(`{}`), timeout); !errors.Is(err, errPythonTimeout) || ran != timeout {
			t.Fatalf("a script that ran %s under a timeout of %s: ran %s, %v", 3*timeout, timeout, ran, err)
		}
		// The clock stops when this function returns, so the stand-in is
		// first left the time to end.
		time.Sleep(3 * timeout)
		// A worker that cannot read the request answers why instead of
		// saying it has it, and that is the run's answer.
		refused := heldWorker(t, heldAnswer{0, `{"ok": false, "error": "no"}`})
		if line, _, err := refused.call(context.Background(), []byte(`{}`), timeout); err != nil || string(line) != `{"ok": false, "error": "no"}` {
			t.Fatalf("%q %v", line, err)
		}
	})
}

// With a real worker and a large response: the time a probe reports its
// script took (ScriptTimer) is the script's, a small part of the run, most
// of which is handing the response over. The response is of 32 MiB, and of
// 8 MiB under the race detector, where writing it for the worker takes
// several times as long and is as large a part of the run.
//
// The script's part is a thousandth of the run and is held to a third. It
// is measured from the worker's word that it has the request to its answer,
// so a machine that leaves the worker, or the exporter reading it, waiting
// between the two adds the wait to it, and can only add: the run is made
// again then, up to five times, and the first whose script is within the
// third is the answer. A script timed with the response's handing over is
// most of every run.
func TestPythonScriptDurationDoesNotCountHandingOverTheResponse(t *testing.T) {
	requirePython(t)
	c := workerCollector("handover", `metric(name="length", value=len(data))`)
	c.Limits.ScriptTimeout = model.Duration(30 * time.Second)
	if _, err := runWorkerText(context.Background(), c, "warm"); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("requests 12345 worker=a\n", alloctest.UnlessRaced(32<<20, 8<<20)/24)
	for run := 1; ; run++ {
		ctx, timer := WithScriptTimer(context.Background())
		start := time.Now()
		set, err := runWorkerText(ctx, c, body)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("after %s: %v", took, err)
		}
		if workerMetricValue(t, set, "length") != float64(len(body)) {
			t.Fatalf("%+v", set.Metrics)
		}
		seconds, ran := timer.Seconds()
		if ran && seconds <= took.Seconds()/3 {
			return
		}
		if !ran || run == 5 {
			t.Fatalf("the script is said to have run %vs of a run that took %s, the last of %d runs", seconds, took, run)
		}
	}
}

// A script the probe's deadline ends before its script_timeout is reported
// as that, naming both, and counted as a run that ended by the deadline and
// a worker stopped for it; one that overruns script_timeout with time left
// on the probe's deadline is a timeout, as before.
func TestPythonRunCutByTheProbesDeadlineIsNotATimeout(t *testing.T) {
	requirePython(t)
	holdScriptsToTheirTimeout(t)
	c := workerCollector("deadline", "import time\nif data == 'slow': time.sleep(30)\nmetric(name='v', value=1)")
	c.Limits.ScriptTimeout = model.Duration(20 * time.Second)
	if _, err := runWorkerText(context.Background(), c, "fast"); err != nil {
		t.Fatal(err)
	}
	// The deadline has to pass while the script runs, and nothing tells the
	// test when the worker has taken the request and begun it. So the
	// deadline is a second away, and where that second passed with the
	// request still on its way, on a machine that left the worker waiting
	// so long, the run says the script never ran: that run is counted, and
	// the next has twice the time, up to sixteen seconds, which the script's
	// own twenty are still past.
	var err error
	cut := uint64(0)
	for within := time.Second; ; within *= 2 {
		ctx, cancel := context.WithTimeout(context.Background(), within)
		_, err = runWorkerText(ctx, c, "slow")
		cancel()
		cut++
		if err == nil || !strings.Contains(err.Error(), "python transform did not run") || within >= 16*time.Second {
			break
		}
		// The worker was stopped with its run; the next run finds one
		// waiting, as the first did.
		if _, err := runWorkerText(context.Background(), c, "fast"); err != nil {
			t.Fatal(err)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "python transform was stopped after ") ||
		!strings.Contains(err.Error(), "because its probe or scrape ran out of time, not because of limits.script_timeout (20s)") ||
		strings.Contains(err.Error(), "timed out") || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, model.ErrScriptFailed) {
		t.Fatalf("err=%v", err)
	}
	// How long it ran is no part of what the failure is to the log.
	if same := model.SameFailureText(err); same == err.Error() || !strings.Contains(same, "python transform was stopped after # because its probe or scrape ran out of time") {
		t.Fatalf("the failure %v is recognised by %q", err, same)
	}
	snap := PythonWorkers().Snapshot(c.Name)
	if snap.Runs[pythonRunDeadline] != cut || snap.Runs[pythonRunTimeout] != 0 || snap.Stops[pythonStopDeadline] != cut || snap.Stops[pythonStopTimeout] != 0 {
		t.Fatalf("runs %v stops %v, want %d of each ended by the deadline", snap.Runs, snap.Stops, cut)
	}

	// The deadline already past when the request is handed over: the script
	// never ran, which the error says.
	if _, err := runWorkerText(context.Background(), c, "fast"); err != nil {
		t.Fatal(err)
	}
	past, cancelPast := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelPast()
	if _, err := runWorkerText(past, c, "fast"); err == nil || !strings.Contains(err.Error(), "python transform did not run: its probe or scrape ran out of time while the response was handed to the worker") {
		t.Fatalf("err=%v", err)
	}
	if snap := PythonWorkers().Snapshot(c.Name); snap.Runs[pythonRunDeadline] != cut+1 {
		t.Fatalf("runs %v", snap.Runs)
	}

	// script_timeout first, the probe's deadline far away.
	c.Limits.ScriptTimeout = model.Duration(200 * time.Millisecond)
	far, cancelFar := context.WithTimeout(context.Background(), time.Minute)
	defer cancelFar()
	if _, err := runWorkerText(far, c, "slow"); err == nil || !strings.Contains(err.Error(), "python transform timed out after 200ms") {
		t.Fatalf("err=%v", err)
	}
	if snap := PythonWorkers().Snapshot(c.Name); snap.Runs[pythonRunTimeout] != 1 || snap.Stops[pythonStopTimeout] != 1 || snap.Runs[pythonRunDeadline] != cut+1 {
		t.Fatalf("runs %v stops %v", snap.Runs, snap.Stops)
	}
	for _, name := range []string{pythonRunDeadline, pythonStopDeadline} {
		if !strings.Contains(strings.Join(PythonRunOutcomes, ",")+"|"+strings.Join(PythonStopReasons, ","), name) {
			t.Fatalf("%s is not among the outcomes and reasons the self-metrics publish", name)
		}
	}
}

// An argument of metric(...) of the wrong type, and an entry a script
// appends to metrics itself that is not a metric, fail the scrape naming the
// metric and the argument, not with the JSON decoder's error about the
// exporter's own structures.
func TestPythonMetricArgumentsOfTheWrongTypeAreNamed(t *testing.T) {
	requirePython(t)
	for script, want := range map[string]string{
		`metric("m", value=1, help=5)`:                                 `ValueError: metric 'm' help 5 is not a string`,
		`metric("m", value=1, type=5)`:                                 `ValueError: metric 'm' type 5 is not a string; give "gauge", "counter" or "untyped"`,
		`metric(5, value=1)`:                                           `ValueError: metric name 5 is not a string`,
		`metric("m", value=1, labels=["a"])`:                           `ValueError: metric 'm' labels must be a mapping of label names to values, not a list`,
		`metrics.append({"name": 5, "value": 1})`:                      `python transform: metric name 5 is not a string`,
		`metrics.append({"name": "m", "value": 1, "help": 5})`:         `python transform: metric "m" help 5 is not a string`,
		`metrics.append({"name": "m", "value": 1, "type": ["gauge"]})`: `python transform: metric "m" type an array of 1 item is not a string; give "gauge", "counter" or "untyped"`,
		`metrics.append({"name": "m", "value": 1, "labels": ["a"]})`:   `python transform: metric "m" labels are an array of 1 item, not a mapping of label names to values`,
		`metrics.append(5)`:                                            `python transform: metrics[0] is 5, not a metric; call metric(...), or append a mapping with a name and a value`,
		`metric("ok", value=1); metrics.append("m")`:                   `python transform: metrics[1] is "m", not a metric`,
	} {
		_, err := runWorkerScript(t, workerCollector("wrong_types", script))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "json:") || strings.Contains(err.Error(), "unmarshal") {
			t.Errorf("%s: err=%v, want %q", script, err, want)
		}
	}
	// None for the help or the type is none given.
	set, err := runWorkerScript(t, workerCollector("wrong_types", `metric("m", value=1, help=None, type=None)
metrics.append({"name": "n", "value": 2, "help": None, "type": None, "labels": None})`))
	if err != nil || len(set.Metrics) != 2 || set.Metrics[0].Type != model.GaugeMetricType || set.Metrics[1].Type != model.GaugeMetricType || set.Metrics[1].Help != "" {
		t.Fatalf("%v %+v", err, set)
	}
}

// leaveIdleWorkers runs n scripts of the collector at once, which leaves n
// workers idle, and returns them.
//
// The runs are at once because each is held where it starts its worker
// until all n are there, and so has found no worker of another's to take.
// Left to the 0.3s their scripts sleep for, a run that the machine began
// that much later than another took the other's worker, and left one fewer.
func leaveIdleWorkers(t *testing.T, c *model.Collector, n int) []*pythonWorker {
	t.Helper()
	pool := PythonWorkers()
	start := pool.start
	var starting atomic.Int32
	together := make(chan struct{})
	pool.start = func(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
		if int(starting.Add(1)) == n {
			close(together)
		}
		select {
		case <-together:
		case <-time.After(30 * time.Second):
		}
		return start(ctx, spec)
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := runWorkerText(context.Background(), c, "hold"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	pool.start = start
	pool.mu.Lock()
	defer pool.mu.Unlock()
	idle := append([]*pythonWorker(nil), pool.idle[pythonWorkerSpec("python3", c).key()]...)
	if len(idle) != n {
		t.Fatalf("%d idle workers, want %d", len(idle), n)
	}
	return idle
}

// Workers that died while they were idle, as the kernel's OOM killer leaves
// them, fail no scrape: each is found dead when it is taken from the pool,
// counted as a crash, and replaced.
func TestPythonWorkersThatDiedIdleAreReplaced(t *testing.T) {
	requirePython(t)
	c := workerCollector("died_idle", "import time\nif data == 'hold': time.sleep(0.3)\nmetric(name='v', value=1)")
	idle := leaveIdleWorkers(t, c, 4)
	for _, worker := range idle {
		if err := worker.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		<-worker.exited
	}
	started := PythonWorkers().started.Load()
	for scrape := 1; scrape <= 5; scrape++ {
		if _, err := runWorkerText(context.Background(), c, "go"); err != nil {
			t.Fatalf("scrape %d after the idle workers were killed: %v", scrape, err)
		}
	}
	snap := PythonWorkers().Snapshot(c.Name)
	if snap.Stops[pythonStopCrash] != 4 || PythonWorkers().started.Load()-started != 1 || snap.Idle != 1 || snap.Busy != 0 {
		t.Fatalf("stops %v, %d started, %d idle, %d busy", snap.Stops, PythonWorkers().started.Load()-started, snap.Idle, snap.Busy)
	}
	if snap.Runs[pythonRunFailed] != 0 {
		t.Fatalf("runs %v", snap.Runs)
	}
}

// A worker of the pool that looks alive and cannot take the request, as one
// dying at that moment, costs the scrape another worker, not a failure:
// nothing of the script had run. Its request pipe is closed here to make it
// so without a race.
func TestPythonRunMovesToAnotherWorkerWhenAnIdleOneCannotTakeIt(t *testing.T) {
	requirePython(t)
	c := workerCollector("cannot_take", "import time\nif data == 'hold': time.sleep(0.3)\nmetric(name='v', value=1)")
	idle := leaveIdleWorkers(t, c, 2)
	for _, worker := range idle {
		closeFiles(worker.requests)
	}
	// The workers exit when they see their pipe closed; whether the pool
	// finds them gone already or learns it from the failed write, the run
	// goes through.
	set, err := runWorkerText(context.Background(), c, "go")
	if err != nil || workerMetricValue(t, set, "v") != 1 {
		t.Fatalf("%v %+v", err, set)
	}
	snap := PythonWorkers().Snapshot(c.Name)
	if snap.Stops[pythonStopCrash] != 2 || snap.Runs[pythonRunFailed] != 0 || snap.Runs[pythonRunOK] != 3 {
		t.Fatalf("stops %v runs %v", snap.Stops, snap.Runs)
	}
}

// A script can arm its worker's death by accident: an alarm outlives the
// run and kills the idle worker a second later. The next scrape gets
// another worker instead of a broken pipe.
//
// The alarm has to outlive the run it was armed in, which is a moment's
// work once it is armed, and on a machine that leaves the worker waiting a
// second at that moment it does not: the alarm ends the run. The test then
// arms one twice as far away, and so on up to sixteen seconds. The worker
// the alarm is to end is looked up, idle, when the run has returned: got by
// a second run, that run had to end within the alarm's second as well.
func TestPythonWorkerKilledByItsOwnAlarmIsReplaced(t *testing.T) {
	requirePython(t)
	c := workerCollector("alarm", "import signal\nif data != 'go': signal.alarm(int(data))\nmetric(name='v', value=1)")
	for seconds := 1; ; seconds *= 2 {
		_, err := runWorkerText(context.Background(), c, strconv.Itoa(seconds))
		if err == nil {
			break
		}
		if seconds >= 16 {
			t.Fatalf("the run that armed an alarm %d seconds away: %v", seconds, err)
		}
	}
	armed := idleWorker(t, c)
	select {
	case <-armed.exited:
	case <-time.After(time.Minute):
		t.Fatal("the alarm did not end the worker")
	}
	if _, err := runWorkerText(context.Background(), c, "go"); err != nil {
		t.Fatalf("after the alarm killed the idle worker: %v", err)
	}
}

// The thread that watches a worker's parent leaves a worker of a living
// exporter alone, idle or busy, for longer than its interval, and a script
// cannot make it end the worker by replacing what it calls.
func TestPythonWorkerOfALivingExporterIsLeftAlone(t *testing.T) {
	requirePython(t)
	c := workerCollector("watched", "import time\nos.getppid = lambda: 1\ntime.sleep(float(data))\nmetric(name='v', value=1)")
	c.Limits.ScriptTimeout = model.Duration(10 * time.Second)
	for _, sleep := range []string{"0", "2.5", "0"} {
		if _, err := runWorkerText(context.Background(), c, sleep); err != nil {
			t.Fatal(err)
		}
	}
	if snap := PythonWorkers().Snapshot(c.Name); snap.Starts != 1 || snap.Idle != 1 {
		t.Fatalf("%d workers started, %d idle, stops %v", snap.Starts, snap.Idle, snap.Stops)
	}
}

// busyWorkerHelperEnv makes TestBusyWorkerHelperProcess the exporter of
// TestBusyPythonWorkerDoesNotOutliveAKilledExporter.
const busyWorkerHelperEnv = "PUE_TEST_BUSY_WORKER_HELPER"

// TestBusyWorkerHelperProcess is not a test of its own: run as a process, it
// stands in for the exporter. It starts a worker, gives it a script that
// never ends, prints the worker's process ID once the script runs, and
// waits to be killed.
func TestBusyWorkerHelperProcess(t *testing.T) {
	if os.Getenv(busyWorkerHelperEnv) == "" {
		t.Skip("only runs as the helper process of another test")
	}
	c := workerCollector("orphan", "while True:\n    pass")
	worker, err := startPythonWorker(context.Background(), pythonWorkerSpec("python3", c))
	if err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	payload, err := pythonRequest("metrics", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.requests.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
	if line := <-worker.lines; string(line.data) != pythonRequestTaken {
		t.Fatalf("the worker answered %q", line.data)
	}
	fmt.Printf("worker %d\n", worker.cmd.Process.Pid)
	time.Sleep(time.Hour)
}

// processRuns reports whether a process is running, by /proc: one that has
// exited and whose new parent has not collected it yet is not.
func processRuns(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the command name, which is in parentheses.
	state := strings.TrimSpace(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return !strings.HasPrefix(state, "Z") && !strings.HasPrefix(state, "X")
}

// A worker busy in a script when the exporter is killed, which no longer
// reads the pipe whose closing ends an idle one and which nobody is left to
// stop, ends by itself within seconds, having seen that its parent is gone.
func TestBusyPythonWorkerDoesNotOutliveAKilledExporter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc to see the worker end")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestBusyWorkerHelperProcess$")
	helper.Env = append(os.Environ(), busyWorkerHelperEnv+"=1")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = helper.Process.Kill() })
	// The helper prints the worker's process ID; anything else it prints is
	// kept for the failure message.
	type helperOutput struct {
		worker int
		rest   string
	}
	printed := make(chan helperOutput, 1)
	go func() {
		var out helperOutput
		scanner := bufio.NewScanner(stdout)
		for out.worker == 0 && scanner.Scan() {
			if text, ok := strings.CutPrefix(scanner.Text(), "worker "); ok {
				out.worker, _ = strconv.Atoi(text)
				continue
			}
			out.rest += scanner.Text() + "\n"
		}
		if err := scanner.Err(); err != nil {
			out.rest += err.Error() + "\n"
		}
		printed <- out
	}()
	var worker int
	select {
	case out := <-printed:
		if out.worker == 0 {
			t.Fatalf("the helper process ended without starting a worker:\n%s", out.rest)
		}
		worker = out.worker
	case <-time.After(30 * time.Second):
		t.Fatal("the helper process did not start a worker")
	}
	// Whatever happens, the worker's endless script is not left running.
	t.Cleanup(func() {
		if process, err := os.FindProcess(worker); err == nil && processRuns(worker) {
			_ = process.Kill()
		}
	})
	if !processRuns(worker) {
		t.Fatal("the worker is not running its script")
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	// The worker looks once a second; half a minute bounds one that never
	// ends.
	testutil.WaitFor(t, "the busy worker to end once its exporter was killed", func() bool { return !processRuns(worker) })
}
