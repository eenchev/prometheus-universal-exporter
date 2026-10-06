package transform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An interpreter has a limit to say it is ready in (pythonworker.go). These
// tests pin whose limit it is, and what a start that fails says of the
// interpreter: that it exited, or that it was still starting.

// shortStart is the start timeout of the tests of that limit: what they
// start never says it is ready, so a slow machine only makes them slower.
const shortStart = 50 * time.Millisecond

// standInInterpreter is the path of a shell script that stands in for
// python3: it is started as the worker is, with the request pipe as its
// descriptor 3 and the answer pipe as its 4, and does what script says.
func standInInterpreter(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	path := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a script has to be executable
		t.Fatal(err)
	}
	return path
}

// The stand-ins. One that is running reads its requests to their end, as a
// worker does, so it ends when it is stopped even where nothing kills it.
const (
	standInServes = "exec cat <&3 >/dev/null"
	// standInNeverReady starts and says nothing.
	standInNeverReady = standInServes
	// standInExitsSilently is an interpreter that ends before it is ready.
	standInExitsSilently = "exit 3"
)

// standInReadyWhen says it is ready once the file gate exists, and not
// before: a start as slow as the test makes it.
func standInReadyWhen(gate string) string {
	return "while [ ! -e '" + gate + "' ]; do sleep 0.01; done\necho '{\"ok\": true, \"ready\": true}' >&4\n" + standInServes
}

// standInAnswers writes line as its first answer, and stays.
func standInAnswers(line string) string {
	return "echo '" + line + "' >&4\n" + standInServes
}

func standInSpec(path string) pythonSpec {
	return pythonSpec{Path: path, Collector: "start", MaxOutput: 1 << 10}
}

// runStandIn runs a transform script with the stand-in as the interpreter,
// and returns the failure a scrape would have.
func runStandIn(t *testing.T, standIn string) error {
	t.Helper()
	c := workerCollector("start", `metric(name="v", value=1)`)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	_, err := executePython(context.Background(), standIn, c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c)
	if err == nil {
		t.Fatal("a stand-in that is no interpreter ran a script")
	}
	return err
}

// stillStarting is what a start says that ran out of limit with the
// interpreter running.
func stillStarting(limit time.Duration) string {
	return "the interpreter did not start within " + limit.String() + ": it was still running and had not said it was ready, so it was stopped; it did not crash: look at how busy the machine is and at how long the libraries the collector declares take to import"
}

// An interpreter a test starts has a minute to say it is ready in, on the
// process's own pool and on each pool a test is given, and keeps it in a
// test of the script's timeout, which starts an interpreter as any other
// does: the exporter's ten seconds have been overrun by one start on a busy
// machine, and failed a test that was not about them. A pool nobody has set
// gives the ten seconds, which is the exporter's.
func TestEveryPoolOfTheTestsLeavesAnInterpreterAMinuteToStartIn(t *testing.T) {
	limit := func() time.Duration { return time.Duration(PythonWorkers().startTimeout.Load()) }
	if got := limit(); got != time.Minute {
		t.Fatalf("the process's own pool gives a start %s, want the minute TestMain sets", got)
	}
	func() {
		defer IsolatePythonWorkers()()
		if got := limit(); got != 10*time.Second {
			t.Fatalf("a fresh pool gives a start %s, want the exporter's 10s", got)
		}
	}()
	usePythonPool(t)
	if got := limit(); got != time.Minute {
		t.Fatalf("the pool a test is given gives a start %s, want a minute", got)
	}
	holdScriptsToTheirTimeout(t)
	if got := limit(); got != time.Minute {
		t.Fatalf("a test of the script's timeout gives a start %s, want the minute still", got)
	}
}

// The limit a start runs under is its pool's. The same stand-in, which says
// it is ready only when the test lets it, starts under the tests' minute
// after three times the short limit, and under the short limit is stopped
// and reported as not started within it.
func TestAStartHasTheTimeItsPoolGivesIt(t *testing.T) {
	usePythonPool(t)
	gate := filepath.Join(t.TempDir(), "ready")
	spec := standInSpec(standInInterpreter(t, standInReadyWhen(gate)))

	type started struct {
		worker *pythonWorker
		err    error
	}
	done := make(chan started, 1)
	begun := time.Now()
	go func() {
		worker, err := startPythonWorker(context.Background(), spec)
		done <- started{worker, err}
	}()
	// The stand-in's own slowness: longer than the short limit below.
	time.Sleep(3 * shortStart)
	select {
	case got := <-done:
		t.Fatalf("the start ended before the stand-in was ready: %v", got.err)
	default:
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("a start of %s under a limit of a minute: %v", time.Since(begun), got.err)
	}
	got.worker.stop()
	<-got.worker.exited

	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	PythonWorkers().SetStartTimeout(shortStart)
	begun = time.Now()
	worker, err := startPythonWorker(context.Background(), spec)
	if err == nil {
		worker.stop()
		t.Fatalf("a stand-in that never says it is ready started under a limit of %s", shortStart)
	}
	if took := time.Since(begun); took < shortStart || err.Error() != stillStarting(shortStart) {
		t.Fatalf("after %s under a limit of %s:\n got %v\nwant %s", took, shortStart, err, stillStarting(shortStart))
	}
}

// An interpreter still running when its time to start runs out is said to
// have been running, not to have exited: the scrape fails with what the
// operator is to look at, a busy machine or a slow import, and not a crash.
// The start is counted as failed, and the failure log takes it for the same
// failure every time, since nothing in its text was measured.
func TestAnInterpreterStillStartingAtTheLimitIsNotSaidToHaveExited(t *testing.T) {
	usePythonPool(t)
	PythonWorkers().SetStartTimeout(shortStart)
	err := runStandIn(t, standInInterpreter(t, standInNeverReady))
	want := "python transform failed: " + stillStarting(shortStart)
	if err.Error() != want {
		t.Fatalf("\n got %v\nwant %s", err, want)
	}
	if strings.Contains(err.Error(), "exited") {
		t.Fatalf("an interpreter that was running is said to have exited: %v", err)
	}
	if same := model.SameFailureText(err); same != want {
		t.Fatalf("recognised by %q, want the text itself", same)
	}
	if snap := PythonWorkers().Snapshot("start"); snap.StartFailures != 1 || snap.Starts != 0 || snap.Starting != 0 || snap.Runs[pythonRunFailed] != 1 {
		t.Fatalf("snapshot=%+v", snap)
	}
}

// An interpreter that exits before it is ready is still said to have exited,
// with all it wrote to stderr, which is where CPython says why: the start
// waits for the process it found gone, so that what it wrote is whole.
func TestAnInterpreterThatExitsBeforeItIsReadyIsSaidToHaveExited(t *testing.T) {
	usePythonPool(t)
	err := runStandIn(t, standInInterpreter(t, "echo \"ModuleNotFoundError: No module named 'lxml'\" >&2\nexit 1"))
	want := "python transform failed: the interpreter did not start: the interpreter exited: ModuleNotFoundError: No module named 'lxml'"
	if err.Error() != want {
		t.Fatalf("\n got %v\nwant %s", err, want)
	}
	if same := model.SameFailureText(err); same != want {
		t.Fatalf("recognised by %q, want the text itself", same)
	}
}

// The stderr of a process is whole when the process has been waited for, not
// when its answers end, which is what a start sees first. The stand-in here
// is a worker whose stderr arrives once it has been stopped, and only then
// is it waited for: the failure has the word. A start that cannot wait
// longer, its scrape given up, says that the interpreter exited without it.
func TestAStartWhoseAnswersEndedWaitsForTheProcessBeforeItReadsItsStderr(t *testing.T) {
	stoppedWorker := func(t *testing.T) (*pythonWorker, *os.File) {
		t.Helper()
		requestRead, requestWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeFiles(requestRead, requestWrite) })
		return &pythonWorker{collector: "ended", cmd: &exec.Cmd{}, requests: requestWrite, lines: make(chan pythonLine, 1), exited: make(chan struct{}), stderr: &tailBuffer{max: pythonStderrTail}}, requestRead
	}
	worker, requests := stoppedWorker(t)
	go func() {
		// Stopped, the worker's request pipe is closed, which ends this
		// read; that is when its last word arrives.
		_, _ = io.Copy(io.Discard, requests)
		// What must not be missed is given long to be missed in.
		time.Sleep(shortStart)
		_, _ = worker.stderr.Write([]byte("Fatal Python error: out of memory\n"))
		close(worker.exited)
	}()
	err := worker.startEnded(context.Background(), nil)
	if want := "the interpreter did not start: the interpreter exited: Fatal Python error: out of memory"; err.Error() != want {
		t.Fatalf("\n got %v\nwant %s", err, want)
	}

	given, giveUp := context.WithCancel(context.Background())
	giveUp()
	worker, _ = stoppedWorker(t)
	if err, want := worker.startEnded(given, nil), "the interpreter did not start: the interpreter exited"; err.Error() != want {
		t.Fatalf("a scrape given up:\n got %v\nwant %s", err, want)
	}
	overrun := make(chan time.Time, 1)
	overrun <- time.Now()
	worker, _ = stoppedWorker(t)
	if err, want := worker.startEnded(context.Background(), overrun), "the interpreter did not start: the interpreter exited"; err.Error() != want {
		t.Fatalf("a start out of time:\n got %v\nwant %s", err, want)
	}
}

// What is at --python.path may answer something else than that it is ready.
// It is then running, and stopped for its answer: the failure shows the
// answer's start and says what the path must name, where it said that the
// interpreter had exited. A long answer is shown by its start, and
// recognised whatever its length.
func TestAFirstAnswerThatIsNotReadyIsShownAndNotSaidToBeAnExit(t *testing.T) {
	usePythonPool(t)
	const told = ", not that it is ready, so it was stopped; --python.path must name a Python 3 interpreter that runs the exporter's worker"
	err := runStandIn(t, standInInterpreter(t, standInAnswers("Python 2.7.18")))
	if want := `python transform failed: the interpreter did not start: its first answer was "Python 2.7.18"` + told; err.Error() != want || model.SameFailureText(err) != want {
		t.Fatalf("\n got %v\nsame %s\nwant %s", err, model.SameFailureText(err), want)
	}
	long := strings.Repeat("x", 200)
	err = runStandIn(t, standInInterpreter(t, standInAnswers(long)))
	shown := `python transform failed: the interpreter did not start: its first answer was "` + long[:64] + `"... (%s bytes)` + told
	if want := fmt.Sprintf(shown, "200"); err.Error() != want {
		t.Fatalf("\n got %v\nwant %s", err, want)
	}
	if want := fmt.Sprintf(shown, model.MovingMark); model.SameFailureText(err) != want {
		t.Fatalf("recognised by\n     %s\nwant %s", model.SameFailureText(err), want)
	}
}

// What a start that ran out of time says is what was true then, asked before
// the worker is stopped. One whose process had exited is said to have
// exited, with its stderr. One still running is said to have been running,
// with what it had written to stderr so far; how much that is depends on
// when the limit ran out, so the failure is recognised without it, the same
// for two starts that got differently far. Either way the worker is stopped.
func TestAStartThatRanOutOfTimeSaysWhatWasTrueWhenItDid(t *testing.T) {
	stopped := func(worker *pythonWorker) bool {
		_, err := worker.requests.Write([]byte("\n"))
		return errors.Is(err, os.ErrClosed)
	}
	t.Run("exited", func(t *testing.T) {
		for stderr, want := range map[string]string{
			"":                                       "the interpreter did not start within 50ms: the interpreter exited",
			"Fatal Python error: init_fs_encoding\n": "the interpreter did not start within 50ms: the interpreter exited: Fatal Python error: init_fs_encoding",
		} {
			worker := heldWorker(t)
			_, _ = worker.stderr.Write([]byte(stderr))
			close(worker.exited)
			err := worker.startOverran(shortStart)
			if err.Error() != want || model.SameFailureText(err) != want {
				t.Errorf("stderr %q:\n got %v\nsame %s\nwant %s", stderr, err, model.SameFailureText(err), want)
			}
			if !stopped(worker) {
				t.Errorf("stderr %q: the worker was not stopped", stderr)
			}
		}
	})
	t.Run("running", func(t *testing.T) {
		const written = "; it had written to stderr: "
		for _, stderr := range []string{"importing lxml\n", "importing lxml\nimporting yaml\n"} {
			worker := heldWorker(t)
			_, _ = worker.stderr.Write([]byte(stderr))
			// As a failed run is reported (pythonResult).
			err := fmt.Errorf("python transform failed: %w", worker.startOverran(shortStart))
			if want := "python transform failed: " + stillStarting(shortStart) + written + strings.TrimSpace(stderr); err.Error() != want {
				t.Errorf("stderr %q:\n got %v\nwant %s", stderr, err, want)
			}
			if want := "python transform failed: " + stillStarting(shortStart) + written + model.MovingMark; model.SameFailureText(err) != want {
				t.Errorf("stderr %q: recognised by\n     %s\nwant %s", stderr, model.SameFailureText(err), want)
			}
			if !stopped(worker) {
				t.Errorf("stderr %q: the worker was not stopped", stderr)
			}
		}
	})
}

// oracleStartPythonWorker is startPythonWorker as it was before a start that
// fails said what was true of the interpreter. Its limit is the pool's, as
// the start's is now, which leaves a start a minute in a test: with the ten
// seconds it had, python3 on a busy machine ran out of them here and the
// two no longer ended alike, in a test of the starts that do not run out.
func oracleStartPythonWorker(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	answerRead, answerWrite, err := os.Pipe()
	if err != nil {
		closeFiles(requestRead, requestWrite)
		return nil, err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), spec.Path, "-I", "-B", "-c", pythonWorkerLauncher, "[]", strconv.FormatInt(spec.MaxMemory, 10), strconv.Itoa(decode.MaxDepth), strconv.Itoa(spec.MaxOutput)) // #nosec G204 -- the tests' own stand-in
	cmd.ExtraFiles = []*os.File{requestRead, answerWrite}
	cmd.Env = pythonWorkerEnvironment(os.Environ())
	stderr := &tailBuffer{max: pythonStderrTail}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		closeFiles(requestRead, requestWrite, answerRead, answerWrite)
		return nil, err
	}
	closeFiles(requestRead, answerWrite)

	worker := &pythonWorker{collector: spec.Collector, key: spec.key(), cmd: cmd, requests: requestWrite, lines: make(chan pythonLine, 1), exited: make(chan struct{}), stderr: stderr, maxMemory: spec.MaxMemory}
	go worker.readAnswers(answerRead, spec.MaxOutput)
	go func() {
		_ = cmd.Wait()
		close(worker.exited)
	}()

	limit := time.Duration(PythonWorkers().startTimeout.Load())
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case line, ok := <-worker.lines:
		if !ok || line.err != nil || !strings.Contains(string(line.data), `"ready": true`) {
			worker.stop()
			return nil, fmt.Errorf("the interpreter did not start: %s", worker.describe(line.err))
		}
		return worker, nil
	case <-timer.C:
		worker.stop()
		return nil, fmt.Errorf("the interpreter did not start within %s: %s", limit, worker.describe(nil))
	case <-ctx.Done():
		worker.stop()
		return nil, ctx.Err()
	}
}

// Outside a start that runs out of time, and a first answer that is not
// "ready", a start ends as it did, which the start as it was says beside
// it: an interpreter that is not there, one that exits without a word, one
// whose first answer is longer than limits.max_output_bytes, a scrape
// given up before the interpreter is ready, and python3 itself, which
// starts. (One that exits with a word on stderr is said to have exited, as
// it was; the start as it was read stderr without waiting for the process,
// so it is no oracle for the word.)
func TestAStartEndsAsItDidWhereTheInterpreterDoesNotOverrunOrAnswerOtherwise(t *testing.T) {
	usePythonPool(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		path func(t *testing.T) string
		want string
	}{
		{"no interpreter", context.Background(), func(*testing.T) string { return "/nonexistent/python3" }, "fork/exec /nonexistent/python3: no such file or directory"},
		{"exits with status 3", context.Background(), func(t *testing.T) string { return standInInterpreter(t, standInExitsSilently) }, "the interpreter did not start: the interpreter exited"},
		{"exits with status 0", context.Background(), func(t *testing.T) string { return standInInterpreter(t, "exit 0") }, "the interpreter did not start: the interpreter exited"},
		{"a first answer over the limit", context.Background(), func(t *testing.T) string {
			return standInInterpreter(t, standInAnswers(strings.Repeat("x", 2<<10)))
		}, "the interpreter did not start: python output exceeds limit"},
		{"the scrape given up", cancelled, func(t *testing.T) string { return standInInterpreter(t, standInNeverReady) }, "context canceled"},
		{"python3", context.Background(), func(t *testing.T) string {
			if _, err := exec.LookPath("python3"); err != nil {
				t.Skip("python3 is not available")
			}
			return "python3"
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := standInSpec(test.path(t))
			text := func(worker *pythonWorker, err error) string {
				if err != nil {
					return err.Error()
				}
				worker.stop()
				return ""
			}
			was := text(oracleStartPythonWorker(test.ctx, spec))
			got := text(startPythonWorker(test.ctx, spec))
			if got != was || got != test.want {
				t.Fatalf("\n got %q\n was %q\nwant %q", got, was, test.want)
			}
		})
	}
}
