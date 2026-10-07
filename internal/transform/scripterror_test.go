package transform

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// failedAnswer is the error the transform makes of a worker's answer that
// its script failed with text.
func failedAnswer(t *testing.T, what, text string) error {
	t.Helper()
	line, err := json.Marshal(map[string]any{"ok": false, "error": text})
	if err != nil {
		t.Fatal(err)
	}
	_, failure := pythonResult(&model.Collector{Name: "failing"}, what, time.Second, line, nil)
	if failure == nil {
		t.Fatalf("an answer that the script failed with %.80q is no error", text)
	}
	return failure
}

// traceback is a traceback as the worker writes one: for each exception of
// a chain its frames, innermost last, each with its source line, and then
// the exception's own line with its message.
func traceback(random *rand.Rand, exceptions, frames, source int, message string) string {
	var b strings.Builder
	for e := range exceptions {
		if e > 0 {
			b.WriteString("\nThe above exception was the direct cause of the following exception:\n\n")
		}
		b.WriteString("Traceback (most recent call last):\n")
		for f := range frames {
			fmt.Fprintf(&b, "  File \"<collector-python>\", line %d, in step_%d\n    %s\n", 1+random.IntN(400), f, "value = read(row)"+strings.Repeat(" + more", source))
		}
		if e < exceptions-1 {
			fmt.Fprintf(&b, "KeyError: 'level %d'\n", e)
		}
	}
	b.WriteString(message)
	return b.String()
}

// A script's error of 1,500 bytes or fewer is in the transform's error as it
// was, to the letter, and is recognised by all of it: for generated
// tracebacks of one to three exceptions, of up to five frames each and of
// messages of one line and of several, the error is what writing the text
// after "python transform failed: " gave, which is how it was made before a
// long one was shown in part.
func TestAScriptErrorOfOrdinaryLengthIsInTheErrorAsItWas(t *testing.T) {
	random := rand.New(rand.NewPCG(36, 3))
	whole, messages := 0, []string{"ValueError: could not convert string to float: 'n/a'", "KeyError: 'status'", "RuntimeError: the queue is paused\nsince 2026-10-06, by the operator", "yaml.scanner.ScannerError: mapping values are not allowed here\n  in \"<unicode string>\", line 1, column 5:\n    a: b: c\n        ^", "Exception", ""}
	for range alloctest.UnlessRaced(1500, 300) {
		text := traceback(random, 1+random.IntN(3), random.IntN(6), random.IntN(8), messages[random.IntN(len(messages))])
		if random.IntN(8) == 0 {
			// A script may fail with no traceback at all, as fail() from
			// the worker's own frame does.
			text = messages[random.IntN(len(messages))]
		}
		if len(strings.TrimSpace(text)) > scriptErrorBytes {
			continue
		}
		whole++
		what := []string{"transform", "pre-script"}[random.IntN(2)]
		// As pythonResult worded it before.
		was := fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(text))
		err := failedAnswer(t, what, text)
		if err.Error() != was.Error() || model.SameFailureText(err) != was.Error() {
			t.Fatalf("a script's error of %d bytes is in the error as\n%q\nrecognised by\n%q\nand was\n%q", len(text), err, model.SameFailureText(err), was)
		}
	}
	if floor := alloctest.UnlessRaced(700, 140); whole < floor {
		t.Errorf("%d tracebacks were within %d bytes, want %d", whole, scriptErrorBytes, floor)
	}
	exact := "ValueError: " + strings.Repeat("v", scriptErrorBytes-len("ValueError: "))
	if err := failedAnswer(t, "transform", exact); err.Error() != "python transform failed: "+exact {
		t.Errorf("a script's error of exactly %d bytes is cut: %.100q", scriptErrorBytes, err)
	}
}

// A script's error past 1,500 bytes is shown by what says most. An
// exception whose message is as long as a response is shown with its frames
// and the first 200 bytes of its own line, with the line's length, where
// the error was the megabytes of the message; a chain of four hundred
// exceptions by the last of them, its frames and its own line, after how
// many lines stood before, where it was 119 kB that began with the first; a
// message of thousands of lines by the exception's own line and the lines
// after it that fit in half of the 1,500 bytes, and then how many lines the
// message had; a text with no frame in it by its start. Each is recognised
// with the mark in place of every length, so the same failure of another
// size is one failure to the log, and the transform's error is within the
// 2,000 bytes of any failure without being cut again.
func TestALongScriptErrorIsShownByItsFramesAndItsOwnLine(t *testing.T) {
	size := alloctest.UnlessRaced(10_000_000, 200_000)
	frames := "Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    raise Exception('x'*10**7)\n"
	chain := func(levels int) string {
		var b strings.Builder
		b.WriteString("Traceback (most recent call last):\n  File \"<collector-python>\", line 3, in f\n    if n == 0: raise ValueError('bottom')\nValueError: bottom\n")
		for level := 1; level <= levels; level++ {
			b.WriteString("\nThe above exception was the direct cause of the following exception:\n\nTraceback (most recent call last):\n")
			if level == levels {
				b.WriteString("  File \"<collector-python>\", line 7, in <module>\n    f(400)\n")
			}
			b.WriteString("  File \"<collector-python>\", line 6, in f\n    raise RuntimeError('level of the chain') from e\nRuntimeError: level of the chain\n")
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	lastOfChain := "\n\nThe above exception was the direct cause of the following exception:\n\nTraceback (most recent call last):\n  File \"<collector-python>\", line 7, in <module>\n    f(400)\n  File \"<collector-python>\", line 6, in f\n    raise RuntimeError('level of the chain') from e\nRuntimeError: level of the chain"
	manyLines := func(lines int) string {
		return frames + "RuntimeError: the rows that failed\n" + strings.Repeat("row 17 of the table has no status\n", lines-1) + "row 17 of the table has no status"
	}
	// tailOf is how a text none of whose lines is cut, and whose exception
	// has a message of one line, is shown: how many lines stood before the
	// exception's own, and the last lines that are within 1,500 bytes
	// together, each with its line break.
	tailOf := func(text, lines string) string {
		all := strings.Split(text, "\n")
		from, used := len(all), 0
		for from > 0 && used+len(all[from-1])+1 <= scriptErrorBytes {
			from--
			used += len(all[from]) + 1
		}
		return "... (" + lines + " lines)\n" + strings.Join(all[from:], "\n")
	}
	for name, tc := range map[string]struct {
		text func(scale int) string
		// said is the error at scale 1, and same what it is recognised by
		// at every scale.
		said, same string
	}{
		"a chain of four hundred exceptions": {
			func(scale int) string { return chain(400 / scale) },
			tailOf(chain(400), strconv.Itoa(strings.Count(chain(400), "\n"))),
			tailOf(chain(400), "#"),
		},
		"a message as long as a response": {
			func(scale int) string { return frames + "Exception: " + strings.Repeat("x", size/scale) },
			frames + "Exception: " + strings.Repeat("x", 189) + fmt.Sprintf("... (%d bytes)", size+len("Exception: ")),
			frames + "Exception: " + strings.Repeat("x", 189) + "... (# bytes)",
		},
		"a message of thousands of lines": {
			func(scale int) string { return manyLines(4000 / scale) },
			frames + "RuntimeError: the rows that failed" + strings.Repeat("\nrow 17 of the table has no status", 21) + "\n... (4001 lines)",
			frames + "RuntimeError: the rows that failed" + strings.Repeat("\nrow 17 of the table has no status", 21) + "\n... (# lines)",
		},
		"a text with no frame": {
			func(scale int) string {
				return "MemoryError: " + strings.Repeat("é", size/10/scale) + "\nsaid the library"
			},
			"MemoryError: " + strings.Repeat("é", 93) + fmt.Sprintf("... (%d bytes)", 2*(size/10)+len("MemoryError: ")) + "\nsaid the library",
			"MemoryError: " + strings.Repeat("é", 93) + "... (# bytes)\nsaid the library",
		},
	} {
		err := failedAnswer(t, "transform", tc.text(1))
		if text := err.Error(); text != "python transform failed: "+tc.said || len(text) > model.MaxFailureBytes || !utf8.ValidString(text) {
			t.Errorf("%s is shown in %d bytes as\n%s\nwant\n%s", name, len(text), text, "python transform failed: "+tc.said)
		}
		if same := model.SameFailureText(err); same != "python transform failed: "+tc.same {
			t.Errorf("%s is recognised by\n%s\nwant\n%s", name, same, "python transform failed: "+tc.same)
		}
		if bounded := model.BoundedFailure(err); bounded != err { //nolint:errorlint // the very error
			t.Errorf("%s is cut again by the bound of every failure: %.200q", name, bounded)
		}
		if name == "a chain of four hundred exceptions" && (!strings.HasSuffix(err.Error(), lastOfChain) || strings.Count(err.Error(), "RuntimeError: level of the chain") < 5) {
			t.Errorf("%s is shown without the last of them whole, or without those before it:\n%s", name, err)
		}
		half := failedAnswer(t, "transform", tc.text(2))
		if same := model.SameFailureText(half); same != "python transform failed: "+tc.same || half.Error() == err.Error() {
			t.Errorf("%s, half the size, is shown as\n%s\nrecognised by\n%s\nwant another length and the same failure", name, half, same)
		}
	}
}

// Whatever a script fails with, the transform's error is within the 2,000
// bytes of any failure by itself, so the bound of every failure leaves it
// its end: for generated tracebacks over 1,500 bytes — chains of up to forty
// exceptions, frames with source lines of up to two kilobytes, messages of
// one line of up to 400 kB or of up to a thousand lines — the error is
// under 1,700 bytes of valid UTF-8, it has the exception's own line, by its
// first 200 bytes when that is longer, and the line of the frame before
// it, and it is recognised by no more than the 1,500 bytes and two marks
// for lines left out. Under the race detector the messages are of up to
// 16 kB of characters, ten times what is shown of a traceback.
func TestAScriptErrorOfAnyShapeIsWithinTheBoundByItself(t *testing.T) {
	random := rand.New(rand.NewPCG(36, 4))
	long := 0
	for range alloctest.UnlessRaced(300, 60) {
		message := "ValueError: " + strings.Repeat([]string{"x", "é", "日"}[random.IntN(3)], random.IntN(alloctest.UnlessRaced(1<<17, 1<<14)))
		if random.IntN(3) == 0 {
			message = "RuntimeError: rows" + strings.Repeat("\n"+strings.Repeat("r", random.IntN(300)), random.IntN(1000))
		}
		text := traceback(random, 1+random.IntN(40), 1+random.IntN(5), random.IntN(300), message)
		if len(text) <= scriptErrorBytes {
			continue
		}
		long++
		err := failedAnswer(t, "pre-script", text)
		said, same := err.Error(), model.SameFailureText(err)
		if len(said) > 1700 || !utf8.ValidString(said) || len(same) > len("python pre-script failed: ")+scriptErrorBytes+2*len("... (# lines)\n") {
			t.Fatalf("a script's error of %d bytes is shown in %d, recognised by %d:\n%s", len(text), len(said), len(same), said)
		}
		own, _, _ := strings.Cut(message, "\n")
		if shown := model.Shown(own, scriptLineBytes).String(); !strings.Contains(said, "\n"+shown) || !strings.Contains(said, "\n  File \"<collector-python>\", line ") {
			t.Fatalf("a script's error of %d bytes is shown without its own line, %.80q, or the frame before it:\n%s", len(text), shown, said)
		}
	}
	if floor := alloctest.UnlessRaced(250, 50); long < floor {
		t.Errorf("%d tracebacks were over %d bytes, want %d", long, scriptErrorBytes, floor)
	}
}

// A script that raises an exception with ten million characters fails its
// transform in an error of some 360 bytes that is whole: the frame that
// raised it, the first 200 bytes of the exception's own line and how long
// the line was. It was ten megabytes, the message whole. A pre-script that
// raises a million characters fails the same way, and a metric the script
// names with a million and gives a type that is none is refused by the
// worker with the name in the exception's line, by its start.
func TestAScriptThatRaisesAMessageOfMegabytesFailsInAShortError(t *testing.T) {
	requirePython(t)
	characters := alloctest.UnlessRaced(10_000_000, 400_000)
	// raised is the script that raises so many characters, and the error
	// its exception is shown in.
	raised := func(characters int) (script, shown string) {
		script = fmt.Sprintf("raise Exception('x'*%d)", characters)
		return script, fmt.Sprintf("Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    %s\nException: %s... (%d bytes)", script, strings.Repeat("x", 189), characters+len("Exception: "))
	}
	script, want := raised(characters)
	c := workerCollector("raising", script)
	c.Limits.MaxOutputBytes = 64 << 20
	if _, err := runWorkerScript(t, c); err == nil || err.Error() != "python transform failed: "+want {
		t.Errorf("the transform fails with %d bytes:\n%.600v\nwant\n%s", len(fmt.Sprint(err)), err, want)
	} else if !errors.Is(err, model.ErrScriptFailed) {
		t.Errorf("the failure is not counted as a script's: %.200v", err)
	}
	script, want = raised(characters / 10)
	pre := workerCollector("raising-first", "")
	pre.Transform = model.TransformConfig{Type: "regex", PreScript: script}
	pre.Metrics = []model.MetricRule{{Name: "m", Expression: `value=(\d+)`}}
	pre.Limits.MaxOutputBytes = 64 << 20
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
	_, err := Transform(t.Context(), &decode.Decoded{Kind: "text", Data: "value=7", Raw: r.Body}, r, pre, "python3")
	if err == nil || err.Error() != "python pre-script failed: "+want {
		t.Errorf("the pre-script fails with %d bytes:\n%.600v\nwant\n%s", len(fmt.Sprint(err)), err, want)
	}
	named := workerCollector("naming", fmt.Sprintf("metric('m'*%d, 1)", characters/10))
	named.Limits.MaxOutputBytes = 64 << 20
	if _, err := runWorkerScript(t, named); err == nil || len(err.Error()) > 600 || !strings.Contains(err.Error(), "\nValueError: metric 'mmmm") || !strings.HasSuffix(err.Error(), " bytes)") {
		t.Errorf("a metric of a long name and a type that is none fails with %d bytes:\n%.700v", len(fmt.Sprint(err)), err)
	}
}

// What a worker wrote to stderr is in the error of a worker that died by its
// last 1,500 bytes, after "... ", when it wrote more: where CPython says why
// it stopped, and the hint about limits.max_script_memory after it, are in
// the error, which is within the 2,000 bytes of any failure by itself. The
// worker keeps the last 4,096 bytes, and an error with all of them was cut
// at the bound to its first 2,000: the start of what was kept, without the
// reason and without the hint. Stderr of 1,500 bytes or fewer is in the
// error whole, as it was, and so is that of a start that was stopped, which
// is recognised without it. A cut does not fall inside a character.
func TestTheEndOfWhatAWorkerWroteToStderrIsWhatItsErrorShows(t *testing.T) {
	worker := func(wrote string, memory int64) *pythonWorker {
		w := &pythonWorker{collector: "died", cmd: &exec.Cmd{}, stderr: &tailBuffer{max: pythonStderrTail}, maxMemory: memory}
		_, _ = w.stderr.Write([]byte(wrote))
		return w
	}
	fatal := "Fatal Python error: Segmentation fault\n\nCurrent thread 0x00007f (most recent call first):\n  File \"<collector-python>\", line 4 in <module>"
	noise := strings.Repeat("W", 100000) + "\n"
	died := worker(noise+fatal+"\n", 1<<30)
	text := died.describe(nil) + died.memoryHint()
	kept := (noise + fatal)[len(noise+fatal)-stderrShownBytes:]
	if want := "the interpreter exited: ... " + kept + died.memoryHint(); text != want || len("python transform failed: "+text) > model.MaxFailureBytes || !strings.Contains(text, fatal) || !strings.HasSuffix(text, "so raise the limit if the script needs more)") {
		t.Errorf("a worker that died after writing %d bytes is described in %d bytes:\n%s\nwant\n%s", len(noise+fatal), len(text), text, want)
	}
	for _, wrote := range []string{"", "\n", fatal + "\n", strings.Repeat("e", stderrShownBytes), strings.Repeat("é", stderrShownBytes/2)} {
		w := worker(wrote, 0)
		was := "the interpreter exited"
		if tail := strings.TrimSpace(wrote); tail != "" {
			was += ": " + tail
		}
		if got := w.describe(nil); got != was {
			t.Errorf("a worker that wrote %d bytes is described as %.200q, and was as %.200q", len(wrote), got, was)
		}
		stopped := errors.New("the interpreter did not start within 10s")
		wasStopped := stopped
		if tail := strings.TrimSpace(wrote); tail != "" {
			wasStopped = fmt.Errorf("%w; it had written to stderr: %s", stopped, tail)
		}
		if got := w.startFailure(stopped); got.Error() != wasStopped.Error() {
			t.Errorf("a start stopped after writing %d bytes fails with %.200q, and did with %.200q", len(wrote), got, wasStopped)
		}
	}
	slow := worker(noise+fatal, 0).startFailure(errors.New("the interpreter did not start within 10s"))
	if text, same := slow.Error(), model.SameFailureText(slow); text != "the interpreter did not start within 10s; it had written to stderr: ... "+kept || same != "the interpreter did not start within 10s; it had written to stderr: #" {
		t.Errorf("a start stopped after writing %d bytes fails with %d bytes, %.200q, recognised by %q", len(noise+fatal), len(text), text, same)
	}
	// One byte over, of characters of two bytes: the cut is between two.
	if got, want := shownStderr("a"+strings.Repeat("é", stderrShownBytes/2)), "... "+strings.Repeat("é", stderrShownBytes/2); got != want {
		t.Errorf("stderr of %d bytes is shown in %d, %.40q ..., want its last %d bytes from a character's start", 1+stderrShownBytes, len(got), got, stderrShownBytes)
	}
	if got := shownStderr("éa" + strings.Repeat("é", stderrShownBytes/2)); got != "... "+strings.Repeat("é", stderrShownBytes/2) || !utf8.ValidString(got) {
		t.Errorf("stderr cut inside a character is shown as %.40q ...", got)
	}
}

// The first failure a rule reports is no longer than any failure, whatever
// it quotes of the response: a jq rule whose expression raises
// error(.message) with a message of megabytes, under error_mode log and
// under ignore, reports a first failure of 2,000 bytes that starts as the
// rule's error does and ends with its length, recognised without the
// length, and counts the failure as it did; and the line the transform logs
// of the rule under log, when no caller logs it, has the same 2,000 bytes.
// They were the megabytes whole, kept by the report, written by the log and
// remembered by the failure log for as long as the rule failed. A rule
// whose failure is short reports it as it was.
func TestTheFirstFailureARuleReportsIsNoLongerThanAnyFailure(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	message := strings.Repeat("a", alloctest.UnlessRaced(10<<20, 1<<18))
	c := &model.Collector{Name: "rules", Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{
		{Name: "raised", Expression: "error(.message)", ErrorMode: model.ErrorModeLog},
		{Name: "quiet", Expression: "error(.message)", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{{Name: "l", Expression: ".n"}}},
		{Name: "short", Expression: "error(.short)", ErrorMode: model.ErrorModeLog},
		{Name: "n", Expression: ".n"},
	}}
	ctx, report := WithRuleReport(t.Context())
	set, err := Transform(ctx, &decode.Decoded{Kind: "json", Data: map[string]any{"n": 1, "message": message, "short": "the queue is paused"}}, &fetch.HTTPResponse{}, c, "python3")
	if err != nil || set == nil || len(set.Metrics) != 1 {
		t.Fatalf("the transform gave %v, %.200v", set, err)
	}
	failures := report.Failures()
	if len(failures) != 3 {
		t.Fatalf("%d rules reported failures: %.300v", len(failures), failures)
	}
	whole := len("error: ") + len(message)
	for _, f := range failures[:2] {
		text, same := f.First.Error(), model.SameFailureText(f.First)
		if want := "error: " + message[:model.MaxFailureBytes-len("error: ")-len(fmt.Sprintf("... (%d bytes)", whole))] + fmt.Sprintf("... (%d bytes)", whole); text != want || f.Failures != 1 {
			t.Errorf("rule %s reports %d failures, the first in %d bytes, %.60q ... %q", f.Metric, f.Failures, len(text), text, text[max(0, len(text)-30):])
		}
		if len(same) != model.MaxFailureBytes || !strings.HasSuffix(same, "aaa... (# bytes)") {
			t.Errorf("rule %s's failure is recognised by %d bytes, ... %q", f.Metric, len(same), same[max(0, len(same)-30):])
		}
	}
	if f := failures[2]; f.First.Error() != "error: the queue is paused" || model.SameFailureText(f.First) != "error: the queue is paused" || !f.Logged {
		t.Errorf("the rule of a short failure reports %q", f.First)
	}
	if logs.Len() > 3*model.MaxFailureBytes || !strings.Contains(logs.String(), fmt.Sprintf(`aaa... (%d bytes)","failures":1}`, whole)) || !strings.Contains(logs.String(), `"metric":"short","error_mode":"log","error":"error: the queue is paused"`) {
		t.Errorf("the transform logged %d bytes of its rules' failures: %.300s ... %s", logs.Len(), logs, logs.String()[max(0, logs.Len()-300):])
	}
}
