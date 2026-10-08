package transform

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The least limits.max_output_bytes a configuration may set
// (MinPythonOutputBytes) is the longest of the lines every worker has to
// write. These tests hold the constant, and the lines it is worked out
// from, to what the launcher writes and to how the exporter measures a
// line, so that an answer that gains or loses a key takes the constant with
// it. The floor is the loader's: a worker runs under any limit, as here.

// leastOutputRun runs script under an output limit, as a transform's script
// or, for the mode "data", as a pre-script, with interpreter as the
// interpreter, and returns what a scrape would fail with.
func leastOutputRun(t *testing.T, interpreter, mode, script string, limit int) error {
	t.Helper()
	c := workerCollector(fmt.Sprintf("least-%s-%d", mode, limit), script)
	c.Limits.MaxOutputBytes = model.ByteSize(limit)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	d := &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}
	if mode == "data" {
		_, err := executePythonPreScript(t.Context(), interpreter, script, d, r, c)
		return err
	}
	set, err := executePython(t.Context(), interpreter, script, d, r, c)
	if err == nil && len(set.Metrics) != 0 {
		t.Errorf("under %d bytes for an answer, %q gave the series %+v", limit, script, set.Metrics)
	}
	return err
}

// The lines the constant is the longest of are the launcher's own, byte for
// byte: a worker answers a transform's script that emits nothing with
// pythonEmptyMetricsAnswer, a pre-script that leaves None in data with
// pythonNoDataAnswer, and says of each request that it has it with
// pythonRequestTaken, which call reads away; a pre-script's other answers
// of little are shorter, and the longest of them all is as long as the
// constant, 38 bytes, which is what the documentation and the loader's
// refusal say. And a worker says it is ready in pythonReadyLine: it starts
// under a limit that long, and not under a byte less.
func TestTheLeastOutputLimitIsTheLongestLineAWorkerHasToWrite(t *testing.T) {
	requirePython(t)
	// Under a limit with room to spare, so that a line that has grown is
	// shown as it is written; the next test is of the least itself.
	worker, err := startPythonWorker(t.Context(), pythonSpec{Path: "python3", Collector: "least", MaxOutput: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.stop()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	d := &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}
	longest := max(len(pythonReadyLine), len(pythonRequestTaken))
	for _, run := range []struct{ mode, script, want string }{
		{"metrics", "pass", pythonEmptyMetricsAnswer},
		{"data", "data = None", pythonNoDataAnswer},
		{"data", "data = {}", `{"ok": true, "log": "", "data": {}}`},
		{"data", "data = []", `{"ok": true, "log": "", "data": []}`},
		{"data", "data = ''", `{"ok": true, "log": "", "data": ""}`},
		{"data", "data = 0", `{"ok": true, "log": "", "data": 0}`},
	} {
		payload, err := pythonRequest(run.mode, run.script, d, r, workerCollector("least", run.script))
		if err != nil {
			t.Fatal(err)
		}
		line, _, err := worker.call(t.Context(), payload, time.Minute)
		if err != nil || string(line) != run.want {
			t.Errorf("%s, as %s: the worker answers %s (%v), want %s", run.script, run.mode, line, err, run.want)
		}
		longest = max(longest, len(line))
	}
	if longest != MinPythonOutputBytes || MinPythonOutputBytes != 38 {
		t.Errorf("the longest line a worker has to write is %d bytes, and MinPythonOutputBytes is %d; want both 38, as docs/PYTHON.md, docs/CONFIGURATION.md and the specification say", longest, MinPythonOutputBytes)
	}

	ready, err := startPythonWorker(t.Context(), pythonSpec{Path: "python3", Collector: "ready", MaxOutput: len(pythonReadyLine)})
	if err != nil {
		t.Fatalf("under the %d bytes of the line that it is ready, no worker starts: %v", len(pythonReadyLine), err)
	}
	ready.stop()
	if early, err := startPythonWorker(t.Context(), pythonSpec{Path: "python3", Collector: "ready", MaxOutput: len(pythonReadyLine) - 1}); err == nil || err.Error() != "the interpreter did not start: python output exceeds limit" {
		if err == nil {
			early.stop()
		}
		t.Errorf("under a byte less than the line that it is ready, a start ends with %v", err)
	}
}

// Under MinPythonOutputBytes a worker starts and a script that has nothing to
// say succeeds, of either kind: a transform's script that emits no metric,
// and a pre-script that leaves None, an empty dict or, the shortest answer
// there is, a number of one digit in data. Under a byte less the
// transform's script cannot answer, though the pre-script still can, which
// is why the least is the transform's; the pre-script cannot under a byte
// less than its own answer with None, and nothing runs at all under a byte
// less than the line a worker is ready with.
func TestAScriptAnswersUnderTheLeastOutputLimitAndNotUnderAByteLess(t *testing.T) {
	requirePython(t)
	const (
		tooLong    = "python transform output exceeds limit"
		preTooLong = "python pre-script output exceeds limit"
		noStart    = "python transform failed: the interpreter did not start: python output exceeds limit"
	)
	for _, run := range []struct {
		limit        int
		mode, script string
		want         string
	}{
		{MinPythonOutputBytes, "metrics", "pass", ""},
		{MinPythonOutputBytes, "data", "data = None", ""},
		{MinPythonOutputBytes, "data", "data = {}", ""},
		{MinPythonOutputBytes, "data", "data = 0", ""},
		{MinPythonOutputBytes - 1, "metrics", "pass", tooLong},
		{MinPythonOutputBytes - 1, "data", "data = None", ""},
		{len(pythonNoDataAnswer) - 1, "data", "data = None", preTooLong},
		{len(pythonReadyLine) - 1, "metrics", "pass", noStart},
	} {
		if err := leastOutputRun(t, "python3", run.mode, run.script, run.limit); (err == nil) != (run.want == "") || err != nil && err.Error() != run.want {
			t.Errorf("under %d bytes for an answer, %q as %s ends with %v, want %q", run.limit, run.script, run.mode, err, run.want)
		}
	}
}

// The exporter measures a line as the constant counts it, without its line
// break: a stand-in for the interpreter that writes the launcher's lines as
// the constants have them is read under MinPythonOutputBytes, its answer of
// no metric taken; under a byte less that answer is output over the limit;
// and under a byte less than the line it is ready with, it did not start.
func TestTheExporterReadsALineAsLongAsTheLeastOutputLimit(t *testing.T) {
	usePythonPool(t)
	standIn := standInInterpreter(t, strings.Join([]string{
		"echo '" + pythonReadyLine + "' >&4",
		"while read -r request <&3; do",
		"  echo '" + pythonRequestTaken + "' >&4",
		"  echo '" + pythonEmptyMetricsAnswer + "' >&4",
		"done",
	}, "\n"))
	for limit, want := range map[int]string{
		MinPythonOutputBytes:      "",
		MinPythonOutputBytes - 1:  "python transform output exceeds limit",
		len(pythonReadyLine):      "python transform output exceeds limit",
		len(pythonReadyLine) - 1:  "python transform failed: the interpreter did not start: python output exceeds limit",
		len(pythonRequestTaken):   "python transform failed: the interpreter did not start: python output exceeds limit",
		MinPythonOutputBytes + 1:  "",
		MinPythonOutputBytes << 8: "",
	} {
		if err := leastOutputRun(t, standIn, "metrics", "pass", limit); (err == nil) != (want == "") || err != nil && err.Error() != want {
			t.Errorf("under %d bytes for an answer, the stand-in's run ends with %v, want %q", limit, err, want)
		}
	}
}
