package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A script's error that may be longer than limits.max_output_bytes is cut
// by the worker to what the exporter shows of an error (the launcher's
// failed and cut, scripterror.go's shownByWorker). The worker wrote it
// whole, and the exporter refused the line as output over the limit and
// stopped the worker: these tests compare the two.

// formerErrorLauncher is pythonWorkerLauncher writing a script's error as
// it did: the traceback joined and written whole, however long.
func formerErrorLauncher(t *testing.T) string {
	t.Helper()
	launcher := pythonWorkerLauncher
	const ends = "del failed_check\n"
	from, to := strings.Index(launcher, "def failed_check(most):\n"), strings.Index(launcher, ends)
	if from < 0 || to < from {
		t.Fatal("the launcher has no failed_check to leave out")
	}
	launcher = launcher[:from] + launcher[to+len(ends):]
	for _, was := range [][2]string{
		{"    return shown.format()\n", "    return ''.join(shown.format())\n"},
		{
			"        failed(e,('MemoryError: the script ran out of memory under limits.max_script_memory (%d bytes)'%max_memory,) if max_memory>0 else None)",
			"        answer({'ok': False, 'error': 'MemoryError: the script ran out of memory under limits.max_script_memory (%d bytes)'%max_memory if max_memory>0 else script_error(e)})",
		},
		{"        failed(e)", "        answer({'ok': False, 'error': script_error(e)})"},
	} {
		if strings.Count(launcher, was[0]) != 1 {
			t.Fatalf("the launcher has %q %d times, not once", was[0], strings.Count(launcher, was[0]))
		}
		launcher = strings.Replace(launcher, was[0], was[1], 1)
	}
	return launcher
}

// formerErrorPool is a pool whose workers write a script's error as they
// did.
func formerErrorPool(t *testing.T) *PythonPool {
	t.Helper()
	launcher := formerErrorLauncher(t)
	pool := newPythonPool()
	pool.start = func(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
		return startPythonWorkerRunning(ctx, spec, launcher)
	}
	t.Cleanup(pool.shutdown)
	return pool
}

// failingScript is a script that fails, and how the exception's own line
// begins where the case knows it.
type failingScript struct {
	name, script, own string
}

// failureMessage is a kind of message: the Python expression of one of
// about n characters, and how the exception's own line begins where the
// message has a line that reads as a frame, after which Python's own line
// is no longer the last that is not indented.
type failureMessage struct {
	name string
	make func(n int) (expression, own string)
}

func failureMessages() []failureMessage {
	of := func(format string, per int) func(int) (string, string) {
		return func(n int) (string, string) { return fmt.Sprintf(format, n/per), "" }
	}
	return []failureMessage{
		{"ASCII", of(`'x'*%d`, 1)},
		{"two-byte characters", of(`'\u00e9'*%d`, 1)},
		{"three-byte characters", of(`'\u20ac'*%d`, 1)},
		{"characters beyond the BMP", of(`'\U0001f600'*%d`, 1)},
		{"control characters", of(`'\x01'*%d`, 1)},
		{"quotes and backslashes", of(`'"\\'*%d`, 2)},
		{"lines of many lengths", of(`'\n'.join('row %%d: %%s'%%(i,'v'*(i%%7*40)) for i in range(%d))`, 120)},
		{"lines past what one shows", of(`'\n'.join('w'*300 for i in range(%d))`, 300)},
		{"short lines", of(`'\n'.join(str(i) for i in range(%d))`, 5)},
		{"space around it", of(`'  \n\t'+'x'*%d+'\n \t\n'*50`, 1)},
		{"line breaks after it", of(`'x'*10+'\n'*%d`, 1)},
		{"a line that reads as a frame", func(n int) (string, string) {
			return fmt.Sprintf(`'x'*%d+'\n  File "elsewhere", line 9\n    indented\nafter '+'y'*%d`, n/2, n/2), "after"
		}},
		{"a frame and only indented lines after it", func(n int) (string, string) {
			return fmt.Sprintf(`'x'*%d+'\n  File "elsewhere", line 9\n'+'   indented\n'*40+'   the last, %d'`, n, n), "   the last,"
		}},
		{"halves of surrogate pairs", of(`'\ud800'*%d`, 1)},
		{"both halves, apart", of(`('\ud83d'+'\ude00')*%d`, 2)},
		{"halves among other characters", of(`('x'*193+'\ud83d\ude00\udc00\ud800\u20ac\ud83d'+'\ude00y\n')*%d`, 202)},
		{"characters of every length", of(`'a\u00e9\u20ac\U0001f600'*%d`, 4)},
		{"Unicode space around it", of(`'\u3000\u00a0'+'x'*%d+'\u2028\u0085 '`, 1)},
		{"what only Python calls space around it", of(`'\x1c'+'x'*%d+'\x1f\x1c'`, 1)},
		{"lines ended by CR LF", of(`'line\r\n'*%d`, 6)},
		{"blank lines between", of(`('\n \n'+'z'*50)*%d`, 53)},
	}
}

// failureKind is a way a script fails with one message or two: the script,
// how its exception's own line begins, and whether that line ends with the
// first message as it is, so that a frame the message reads as moves it.
type failureKind struct {
	name, script, own string
	verbatim          bool
}

func failureKinds() []failureKind {
	return []failureKind{
		{"raise", "raise ValueError(%[1]s)", "ValueError", true},
		{"fail()", "fail(%[1]s)", "RuntimeError", true},
		{"a KeyError", "raise KeyError(%[1]s)", "KeyError", false},
		{"an assertion", "assert False, %[1]s", "AssertionError", true},
		{"deep in the script's functions", "def f(n, m):\n    if n == 0:\n        raise LookupError(m)\n    return f(n - 1, m)\nf(8, %[1]s)", "LookupError", true},
		{"raise from", "try:\n    raise ValueError(%[1]s)\nexcept ValueError as e:\n    raise RuntimeError(%[2]s) from e", "RuntimeError", false},
		{"raised while another is handled", "try:\n    raise ValueError(%[1]s)\nexcept ValueError:\n    raise TypeError(%[2]s)", "TypeError", false},
		{"a chain of three, deep", "def f(n, m):\n    if n == 0:\n        raise LookupError(m)\n    return f(n - 1, m)\ntry:\n    try:\n        f(7, %[1]s)\n    except LookupError as e:\n        raise ValueError(%[2]s) from e\nexcept ValueError:\n    f(3, %[1]s)", "LookupError", false},
		{"an exception whose str() raises", "class Odd(Exception):\n    def __str__(self):\n        raise RuntimeError('no')\nraise Odd(%[1]s)", "__collector__.Odd: <exception str() failed>", false},
		{"an exception whose str() is long", "class Long(Exception):\n    def __str__(self):\n        return %[1]s\nraise Long()", "__collector__.Long", true},
		{"a SyntaxError of what it compiles", "compile('x = (' + %[1]s, 'inner', 'exec')", "", false},
		{"an exception group", "raise ExceptionGroup('several', [ValueError(%[1]s), TypeError(%[2]s)])", "", false},
		{"sys.exit", "sys.exit(%[1]s)", "SystemExit", true},
		{"a MemoryError of its own", "raise MemoryError(%[1]s)", "MemoryError", true},
		{"a note", "e = ValueError(%[1]s)\ne.add_note(%[2]s)\nraise e", "", false},
		{"a KeyboardInterrupt", "raise KeyboardInterrupt(%[1]s)", "KeyboardInterrupt", true},
		{"a KeyError of a tuple", "raise KeyError((%[1]s, 1))", "KeyError", false},
		{"an OSError", "raise OSError(5, %[1]s)", "OSError: [Errno 5]", false},
	}
}

// failureLengths are the lengths a message is given: none, around what a
// line shows (200 bytes) and what an error shows whole (1,500 bytes, the
// frame before the message being some hundred), around the limits the
// scripts are run under and the lengths the worker keeps an error whole to
// under them. Megabytes are raised by the tests after this one.
var failureLengths = []int{0, 1, 60, 199, 200, 201, 700, 1300, 1380, 1400, 1420, 1500, 1501, 1600, 1920, 2048, 5400, 16260, 16384, 16400, 87300, 87400}

// failingScripts is every way of failing with a message of 3,000
// characters, a plain raise of every kind of message, the failures that
// have no message of the test's, and generated more of them: any kind with
// any messages at any of the lengths.
func failingScripts(generated int) []failingScript {
	random := rand.New(rand.NewPCG(38, 41))
	kinds, messages := failureKinds(), failureMessages()
	scripts := []failingScript{
		{"no message", "raise ValueError", "ValueError"},
		{"a source line of kilobytes", "raise ValueError('" + strings.Repeat("s", 5000) + "')", "ValueError"},
		{"a SyntaxError of its own on a long line", "value = 'never closed " + strings.Repeat("y", 40000), ""},
		{"a SyntaxError of its own after many lines", strings.Repeat("value = 1\n", 2000) + "value = (", ""},
	}
	failing := func(kind failureKind, first, second failureMessage, n, m int) failingScript {
		expression, own := first.make(n)
		other, moved := second.make(m)
		switch {
		case own == "" && moved == "":
			own = kind.own
		case !kind.verbatim || moved != "":
			own = ""
		}
		return failingScript{
			name:   fmt.Sprintf("%s, %s of %d, %s of %d", kind.name, first.name, n, second.name, m),
			script: fmt.Sprintf(kind.script, expression, other), own: own,
		}
	}
	for _, kind := range kinds {
		scripts = append(scripts, failing(kind, messages[0], messages[1], 3000, 2000))
	}
	for _, message := range messages {
		scripts = append(scripts, failing(kinds[0], message, messages[0], 3000, 0))
	}
	for range generated {
		kind, first, second := kinds[random.IntN(len(kinds))], messages[random.IntN(len(messages))], messages[random.IntN(len(messages))]
		n, m := failureLengths[random.IntN(len(failureLengths))], failureLengths[random.IntN(len(failureLengths))]
		// A few characters more or fewer, so that a bound is met from
		// both sides and at itself.
		n = max(n+random.IntN(7)-3, 0)
		scripts = append(scripts, failing(kind, first, second, n, m))
	}
	return scripts
}

// pythonString is text as the worker's json.dumps writes a string: ASCII,
// every other character and every control character as \uXXXX, one beyond
// the BMP as the two halves of its pair.
func pythonString(text string) string {
	var written strings.Builder
	written.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"' || r == '\\':
			written.WriteByte('\\')
			written.WriteRune(r)
		case r >= ' ' && r <= '~':
			written.WriteRune(r)
		case r == '\n':
			written.WriteString(`\n`)
		case r == '\r':
			written.WriteString(`\r`)
		case r == '\t':
			written.WriteString(`\t`)
		case r == '\b':
			written.WriteString(`\b`)
		case r == '\f':
			written.WriteString(`\f`)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&written, `\u%04x\u%04x`, 0xD800+r>>10, 0xDC00+r&0x3FF)
		default:
			fmt.Fprintf(&written, `\u%04x`, r)
		}
	}
	written.WriteByte('"')
	return written.String()
}

// shownLine is the answer line of a worker that shows an error as shown:
// the text in parts, each a string or a number that was measured, which
// stands in the text and is the mark in what the failure is recognised by.
func shownLine(shown model.QuotedValue) string {
	said, same := shown.String(), shown.Same()
	parts := []string{}
	var text strings.Builder
	for i, j := 0, 0; j < len(same); j++ {
		if same[j] != model.MovingMark[0] || said[i] == model.MovingMark[0] {
			text.WriteByte(said[i])
			i++
			continue
		}
		digits := i
		for digits < len(said) && said[digits] >= '0' && said[digits] <= '9' {
			digits++
		}
		parts = append(parts, pythonString(text.String()), said[i:digits])
		text.Reset()
		i = digits
	}
	parts = append(parts, pythonString(text.String()))
	return `{"ok": false, "shown": [` + strings.Join(parts, ", ") + `]}`
}

// failureWhole is how many characters an error may have that a worker
// writes whole where its line fits the limit: a line no longer than that is
// the line of such an error, and an error of more is cut whatever the
// limit.
const failureWhole = 16384

// linesMarkLine is a line that says how many lines a part of an error had.
var linesMarkLine = regexp.MustCompile(`^\.\.\. \(\d+ lines\)$`)

// failureLimits are the limits.max_output_bytes the failing scripts are
// run under: the smallest a worker starts under, which its word that it is
// ready just fits; some that hold only a part of what an error shows;
// 16 KiB, within which all an error shows fits however it is written; and
// the default.
var failureLimits = []int{27, 120, 600, 2048, 16384, 1 << 20}

// The error of a script that fails is the error the exporter makes of the
// whole traceback, whatever limits.max_output_bytes is, wherever what the
// exporter shows of it fits the limit: for every way of failing — a raise,
// fail(), a KeyError, an assertion, sys.exit, exceptions raised from one
// another and while another is handled, one raised deep in the script's
// functions, a SyntaxError, an exception group, a note, an exception whose
// str() raises — with messages of every kind of character, of one line and
// of many, with space around them, of lengths around what is shown and
// around the limits, in a transform and in a pre-script, each under the
// default limit and two of the smaller ones.
// The whole traceback is what the worker as it was writes under a limit
// nothing reaches, and the error is what shownScriptError makes of it.
//
// The worker as it was wrote the same line under every limit, and a line
// longer than the limit failed the run as output over the limit and
// stopped the worker. Now the line is within the limit always, the run is a
// script's failure, and one worker for each limit serves every script.
// Where the line fitted before it is the line it was, or, of an error of
// more than 16,384 characters, which is cut under any limit, the line of
// what is shown of it. Where neither the whole error nor what is shown of it fits, the
// error is a part of what is shown: fewer of its lines, the exception's
// own line among them, or the start of that line alone.
func TestAScriptErrorIsWhatTheExporterShowsOfItUnderAnyOutputLimit(t *testing.T) {
	requirePython(t)
	former := formerErrorPool(t)
	scripts := failingScripts(alloctest.UnlessRaced(150, 16))
	c := &model.Collector{Name: "failing"}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
	d := &decode.Decoded{Kind: "text", Data: "value=7", Raw: r.Body}
	spec := func(limit int) pythonSpec {
		return pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: limit, Scripts: "failing"}
	}
	var runs, refused, cut, partly, started int
	for i, script := range scripts {
		mode, what := "metrics", "transform"
		if i%3 == 2 {
			mode, what = "data", "pre-script"
		}
		request, err := pythonRequest(mode, script.script, d, r, c)
		if err != nil {
			t.Fatal(err)
		}
		// The whole traceback, from the worker as it was.
		wholeLine, _, err := former.run(t.Context(), spec(math.MaxInt32), request, time.Minute)
		var whole pythonOutput
		if err != nil || json.Unmarshal(wholeLine, &whole) != nil || whole.OK || whole.Error == "" {
			t.Fatalf("%s: the worker as it was answers %.200q, %v", script.name, wholeLine, err)
		}
		trimmed := strings.TrimSpace(whole.Error)
		shown := shownScriptError(trimmed)
		want := model.Errorf("python %s failed: %s", what, shown)
		if len(want.Error()) > model.MaxFailureBytes {
			t.Fatalf("%s: the error is %d bytes", script.name, len(want.Error()))
		}
		// The lines a worker can write that give the exporter this error:
		// the traceback whole, trimmed where that is all of what is shown,
		// and what is shown of it.
		exact := []string{string(wholeLine), shownLine(shown)}
		if len(trimmed) <= scriptErrorBytes {
			exact = append(exact, `{"ok": false, "error": `+pythonString(trimmed)+`}`)
		}
		// Whatever limits the script is run under, 16 KiB hold what is shown.
		if len(exact[1]) > failureWhole {
			t.Fatalf("%s: what is shown of the error is a line of %d bytes, which a limit of %d does not hold", script.name, len(exact[1]), failureWhole)
		}
		// Each script under the default limit and under two of the smaller,
		// taken in turn: every limit has its share of every sort of script.
		smaller := failureLimits[:len(failureLimits)-1]
		for _, limit := range []int{smaller[i%len(smaller)], smaller[(i+2)%len(smaller)], failureLimits[len(failureLimits)-1]} {
			line, _, err := PythonWorkers().run(t.Context(), spec(limit), request, time.Minute)
			if err != nil {
				t.Fatalf("%s, under %d bytes: the run fails with %v", script.name, limit, err)
			}
			runs++
			if len(line) > limit {
				t.Fatalf("%s: the line is %d bytes under a limit of %d", script.name, len(line), limit)
			}
			_, failure := pythonResult(c, what, time.Minute, line, nil)
			if failure == nil || !strings.HasPrefix(failure.Error(), "python "+what+" failed: ") {
				t.Fatalf("%s, under %d bytes: the answer %.200q is read as %v", script.name, limit, line, failure)
			}
			fits, written := false, false
			for _, one := range exact {
				fits = fits || len(one) <= limit
				written = written || one == string(line)
			}
			same := failure.Error() == want.Error() && model.SameFailureText(failure) == model.SameFailureText(want)
			switch {
			case len(wholeLine) > limit:
				refused++
			case len(wholeLine) <= failureWhole:
				// It fitted, and is an error the worker writes whole.
				if string(line) != string(wholeLine) {
					t.Fatalf("%s, under %d bytes: the line is not the line it was:\n     %.300q\nwas  %.300q", script.name, limit, line, wholeLine)
				}
			}
			if utf8.RuneCountInString(whole.Error) > failureWhole && string(line) == string(wholeLine) {
				t.Fatalf("%s, under %d bytes: an error of %d characters is written whole", script.name, limit, utf8.RuneCountInString(whole.Error))
			}
			if same != fits || same != written {
				t.Fatalf("%s, under %d bytes: the error fits the limit (%t), is the exporter's (%t) and its line one of those that make it (%t):\n%.700q\nfails with\n%s\nrecognised by\n%s\nwant\n%s\nrecognised by\n%s", script.name, limit, fits, same, written, line, failure, model.SameFailureText(failure), want, model.SameFailureText(want))
			}
			if bytes.HasPrefix(line, []byte(`{"ok": false, "shown": `)) {
				cut++
			}
			if same {
				continue
			}
			// A part of what is shown: its lines in their order, without
			// some, or the start of the exception's own line.
			partly++
			said := strings.TrimPrefix(failure.Error(), "python "+what+" failed: ")
			if !utf8.ValidString(said) {
				t.Fatalf("%s, under %d bytes: the error is not UTF-8: %q", script.name, limit, said)
			}
			if !bytes.HasPrefix(line, []byte(`{"ok": false, "shown": `)) {
				started++
				held := false
				for _, whole := range strings.Split(shown.String(), "\n") {
					held = held || strings.Contains(whole, said)
				}
				if own := strings.TrimSpace(script.own); !held || !strings.HasPrefix(own, said) && !strings.HasPrefix(said, own) {
					t.Fatalf("%s, under %d bytes: the error is %q, which is not the start of the exception's own line, %q, of\n%s", script.name, limit, said, script.own, shown)
				}
				continue
			}
			rest, own := strings.Split(shown.String(), "\n"), false
			for _, kept := range strings.Split(said, "\n") {
				if linesMarkLine.MatchString(kept) {
					continue
				}
				at := -1
				for i, whole := range rest {
					// A line of an error the exporter shows whole is cut
					// here as a line of a longer one is.
					if whole == kept || model.Shown(whole, scriptLineBytes).String() == kept {
						at = i
						break
					}
				}
				if at < 0 {
					t.Fatalf("%s, under %d bytes: the line %q of the error is none of the lines, in their order, of\n%s\nThe error:\n%s", script.name, limit, kept, shown, said)
				}
				rest = rest[at+1:]
				own = own || script.own != "" && strings.HasPrefix(kept, script.own)
			}
			if script.own != "" && !own {
				t.Fatalf("%s, under %d bytes: the error is without the exception's own line, %q...:\n%s", script.name, limit, script.own, said)
			}
		}
	}
	// Every run was a script's failure, and no worker was stopped: one was
	// started for each limit, and served every script.
	now := PythonWorkers().Snapshot(c.Name)
	if now.Starts != uint64(len(failureLimits)) || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != uint64(runs) || len(now.Runs) != 1 {
		t.Errorf("%d runs under %d limits: %d workers started, stopped %v, runs %v", runs, len(failureLimits), now.Starts, now.Stops, now.Runs)
	}
	// The scripts are of each sort: lines the worker as it was wrote past
	// the limit, errors that are cut, and errors of which a limit holds
	// only a part, or only the start of the own line.
	if least := len(scripts); refused < least || cut < least/2 || partly < least/2 || started < least/4 {
		t.Errorf("of %d runs of %d scripts, %d had a line past the limit, %d were cut, %d were shown in part and %d by the start of a line", runs, len(scripts), refused, cut, partly, started)
	}
}

// failingCollector is a collector whose transform raises "x" as many times
// as its response says, or, where the response has a second word, the text
// that word is the Python expression of, and, where the response is "peak",
// emits the most memory its interpreter held since the run that asked last
// (tracemalloc).
func failingCollector(name string, limit model.ByteSize) *model.Collector {
	c := workerCollector(name, `
import tracemalloc
if not tracemalloc.is_tracing(): tracemalloc.start()
words = response.text.split()
if words[0] == "peak":
    metric("peak", value=tracemalloc.get_traced_memory()[1])
    tracemalloc.stop()
    tracemalloc.start()
else:
    raise ValueError((eval(words[1]) if len(words) > 1 else "x") * int(words[0]))
`)
	c.Limits.MaxOutputBytes = limit
	return c
}

// failingRun runs the collector's transform on a response of body.
func failingRun(t *testing.T, c *model.Collector, body string) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	return Transform(t.Context(), &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c, "python3")
}

// A script that raises an exception with two megabytes in it, under the
// default limit of one for an answer, fails with its exception: the frame
// that raised it, the first 200 bytes of the exception's own line and how
// long the line was, in a transform and in a pre-script, with fail() as
// with raise. It failed with "python transform output exceeds limit", which
// named neither the exception nor the line, and its worker was stopped for
// it. The worker that ran it runs the next script; the run is counted as a
// script's failure and not as one over the output limit; and the failure
// with half as much again in it is recognised as the same one, as the same
// as that of 20,000 characters, which the limit holds and the worker cuts,
// and as that of an exception short enough to reach the exporter whole.
//
// The script makes the text of as many characters as its response says:
// what is long here is the exception, not what the exporter sends.
func TestAScriptThatRaisesMoreThanTheOutputLimitFailsWithItsException(t *testing.T) {
	requirePython(t)
	const megabyte = 1 << 20
	shown := func(script string, line string, characters int) string {
		return fmt.Sprintf("Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    %s\n%s%s... (%d bytes)", script, line, strings.Repeat("x", 200-len(line)), len(line)+characters)
	}
	const raises, fails = "raise ValueError('x' * int(response.text))", "fail('x' * int(response.text))"
	raising := workerCollector("raising", raises)
	failing := workerCollector("failing", fails)
	first := workerCollector("raising-first", "")
	first.Transform = model.TransformConfig{Type: "regex", PreScript: raises}
	first.Metrics = []model.MetricRule{{Name: "m", Expression: `value=(\d+)`}}
	for _, run := range []struct {
		c                  *model.Collector
		what, script, line string
	}{
		{raising, "transform", raises, "ValueError: "},
		{failing, "transform", fails, "RuntimeError: "},
		{first, "pre-script", raises, "ValueError: "},
	} {
		var recognised string
		for i, characters := range []int{2 * megabyte, 3 * megabyte, 20000, 2000} {
			_, err := failingRun(t, run.c, strconv.Itoa(characters))
			want := "python " + run.what + " failed: " + shown(run.script, run.line, characters)
			if err == nil || err.Error() != want {
				t.Fatalf("%s, %d characters raised: the %s fails with\n%.700v\nwant\n%s", run.c.Name, characters, run.what, err, want)
			}
			if !errors.Is(err, model.ErrScriptFailed) || errors.Is(err, model.ErrLimitExceeded) {
				t.Errorf("%s, %d characters raised: the failure is not a script's alone: %.200v", run.c.Name, characters, err)
			}
			if same := model.SameFailureText(err); i == 0 {
				recognised = same
			} else if same != recognised || !strings.HasSuffix(same, "... ("+model.MovingMark+" bytes)") {
				t.Errorf("%s, %d characters raised: the failure is recognised by\n%s\nand that of the first by\n%s", run.c.Name, characters, same, recognised)
			}
		}
		// One worker ran them all, and was stopped for none.
		if now := PythonWorkers().Snapshot(run.c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 4 || len(now.Runs) != 1 || now.Idle != 1 {
			t.Errorf("%s: %d workers started, stopped %v, idle %d, runs %v; want one worker, four script errors and nothing else", run.c.Name, now.Starts, now.Stops, now.Idle, now.Runs)
		}
	}
}

// An answer longer than limits.max_output_bytes is still what it was,
// since only a script's error is cut. Metrics the worker writes that come
// to more than the limit fail the run as output over the limit and cost
// the worker; metrics whose strings alone are longer are refused by the
// worker with the OverflowError that says so, which under a limit that
// holds it is the line the worker as it was writes; and a worker that has
// cut a script's error before serves both.
func TestAnAnswerOverTheOutputLimitFailsAsItDidBesideAnErrorThatIsCut(t *testing.T) {
	requirePython(t)
	const limit = 4096
	c := workerCollector("answering", `
kind = response.text
if kind == "raise": raise ValueError("x" * 100000)
if kind == "numbers":
    for i in range(300): metric("m", value=i, labels={"i": i})
if kind == "strings": metric("m", value=1, labels={"l": "x" * 5000})
if kind == "few": metric("m", value=1)
`)
	c.Limits.MaxOutputBytes = limit
	if _, err := failingRun(t, c, "raise"); err == nil || !strings.HasSuffix(err.Error(), "\nValueError: "+strings.Repeat("x", 188)+"... (100012 bytes)") {
		t.Fatalf("a script that raises 100,000 characters fails with %.700v", err)
	}
	const refused = "python transform failed: OverflowError: what the script left in metrics is longer than limits.max_output_bytes (4096 bytes) written out, a list or a dict that is there more than once being written each time; leave less there, or raise limits.max_output_bytes"
	if _, err := failingRun(t, c, "strings"); err == nil || err.Error() != refused {
		t.Errorf("metrics whose strings are longer than the limit fail with\n%v\nwant\n%s", err, refused)
	}
	if set, err := failingRun(t, c, "few"); err != nil || len(set.Metrics) != 1 {
		t.Errorf("a metric after them: %v, %v", set, err)
	}
	if now := PythonWorkers().Snapshot(c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 2 || now.Runs[pythonRunOK] != 1 {
		t.Errorf("after an error that is cut, an answer that is refused and one that is taken: %d workers started, stopped %v, runs %v", now.Starts, now.Stops, now.Runs)
	}
	if _, err := failingRun(t, c, "numbers"); err == nil || err.Error() != "python transform output exceeds limit" || !errors.Is(err, model.ErrScriptFailed) {
		t.Errorf("metrics that are longer than the limit written out fail with %v", err)
	}
	if now := PythonWorkers().Snapshot(c.Name); now.Stops[pythonStopOutputLimit] != 1 || now.Runs[pythonRunOutputLimit] != 1 || now.Idle != 0 {
		t.Errorf("after an answer over the limit: stopped %v, runs %v, idle %d", now.Stops, now.Runs, now.Idle)
	}
	// The worker's own refusal is the line it was.
	former := formerErrorPool(t)
	request, err := pythonRequest("metrics", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "strings"}, &fetch.HTTPResponse{StatusCode: 200, Body: []byte("strings"), Headers: http.Header{}}, c)
	if err != nil {
		t.Fatal(err)
	}
	spec := pythonSpec{Path: "python3", Collector: "refusing", MaxOutput: limit}
	was, _, wasErr := former.run(t.Context(), spec, request, time.Minute)
	line, _, err := PythonWorkers().run(t.Context(), spec, request, time.Minute)
	if wasErr != nil || err != nil || string(line) != string(was) || !strings.Contains(string(line), `"error": "OverflowError: what the script left in metrics`) {
		t.Errorf("the worker refuses the metrics with\n%.400q, %v\nand as it was with\n%.400q, %v", line, err, was, wasErr)
	}
}

// A script that runs out of limits.max_script_memory fails with the
// MemoryError that names the limit, under a limit for an answer that does
// not hold that line too: then by as many of its first characters as the
// limit holds, and the worker carries on. The line was written whole,
// refused as output over the limit, and the worker stopped.
//
// Where that limit holds a script's message but not what the traceback
// module makes of it, a second copy of 72 MiB under 128 MiB, the run still
// fails as the script's: with the type of its exception and that its text
// could not be written, of a MemoryError; and the worker serves the next.
// The MemoryError was raised with nothing around it, and ended the
// interpreter.
//
// And a script that raises a message of 48 MiB under a memory limit of
// 256 MiB fails with its exception, in the worker that then runs it again:
// the message, with what the traceback module makes of it, is held twice,
// or three times as the version of Python has it. Joined, written as JSON
// and encoded it was held five times, which the limit does not hold, and
// the worker ended of it.
func TestAScriptErrorUnderAMemoryLimitFitsTheOutputLimit(t *testing.T) {
	requireProcStatus(t)
	const said = "MemoryError: the script ran out of memory under limits.max_script_memory (134217728 bytes)"
	// The line of an error that is said whole.
	whole := len(`{"ok": false, "error": ""}`) + len(said)
	for _, limit := range []int{whole, whole - 1, 27} {
		want := "python transform failed: " + said[:min(len(said), limit-(whole-len(said)))]
		runs := []struct{ response, want string }{{"fill", want}, {"fill", want}}
		if limit == whole {
			// Which holds what is said of a text that could not be written.
			runs = append(runs, struct{ response, want string }{"raise", unwritten("ValueError", "MemoryError")}, runs[0])
		}
		c := workerCollector("filling-"+strconv.Itoa(limit), `
if response.text == "fill": held = bytearray(1 << 30)
if response.text == "raise": raise ValueError("x" * (72 << 20))
metric("m", value=1)
`)
		c.Limits.MaxOutputBytes = model.ByteSize(limit)
		c.Limits.MaxScriptMemory = 128 << 20
		for _, run := range runs {
			if _, err := failingRun(t, c, run.response); err == nil || err.Error() != run.want || !errors.Is(err, model.ErrScriptFailed) {
				t.Errorf("under %d bytes for an answer, %q fails with\n%.300v\nwant\n%s", limit, run.response, err, run.want)
			}
		}
		if now := PythonWorkers().Snapshot(c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != uint64(len(runs)) || len(now.Runs) != 1 {
			t.Errorf("under %d bytes for an answer: %d workers started, stopped %v, runs %v", limit, now.Starts, now.Stops, now.Runs)
		}
	}
	const characters = 48 << 20
	c := failingCollector("raising-much", 1<<20)
	c.Limits.MaxScriptMemory = 256 << 20
	for range 2 {
		_, err := failingRun(t, c, strconv.Itoa(characters))
		if err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("\nValueError: %s... (%d bytes)", strings.Repeat("x", 188), characters+len("ValueError: "))) {
			t.Fatalf("a script that raises %d characters under a memory limit of 256 MiB fails with %.700v", characters, err)
		}
	}
	if now := PythonWorkers().Snapshot(c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 2 {
		t.Errorf("after them: %d workers started, stopped %v, runs %v", now.Starts, now.Stops, now.Runs)
	}
}

// A worker that cuts a script's error holds the message no more often than
// the traceback module makes it: the exception's own and the line the
// module formats of it, twice its length with Python 3.12, and with a
// version that hands out a copy of that line three times. Written whole,
// it was held five times: joined, written as JSON and with its line
// break, and encoded. A message of halves of surrogate pairs, alone or the
// two of a pair apart, is held no more often than one of ASCII: the worker
// made all of it anew to show U+FFFD for a half and the character for two,
// which it now makes of the 200 bytes it shows, and counts the rest: under
// limits.max_script_memory of 128 MiB a message of 32 MiB with one half in
// it ended the worker so. And
// of a chain of exceptions with such a message each, which they hold, one
// more is held at a time and not all of them again.
func TestAnErrorThatIsCutIsHeldNoMoreOftenThanItIsMade(t *testing.T) {
	requirePython(t)
	const characters, halves = 4 << 20, 1 << 20
	c := failingCollector("holding", 1<<20)
	peak := func() int {
		set, err := failingRun(t, c, "peak")
		if err != nil {
			t.Fatal(err)
		}
		return int(workerMetricValue(t, set, "peak"))
	}
	peak()
	for _, message := range []struct {
		name, response string
		// memory is what Python holds the message in, and shown and bytes
		// what the exporter reads of it: its start and its length.
		memory, bytes int
		shown         string
	}{
		{"ASCII", strconv.Itoa(characters), characters, characters, strings.Repeat("x", 188)},
		{"halves of surrogate pairs, each alone", strconv.Itoa(halves) + ` '\udcff'`, 2 * halves, 3 * halves, strings.Repeat("\ufffd", 62)},
		{"the two halves of a pair, apart", strconv.Itoa(halves/2) + ` '\ud83d'+'\ude00'`, 2 * halves, 2 * halves, strings.Repeat("\U0001f600", 47)},
	} {
		_, err := failingRun(t, c, message.response)
		if want := fmt.Sprintf("\nValueError: %s... (%d bytes)", message.shown, message.bytes+len("ValueError: ")); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("%s: a script that raises %d bytes fails with\n%.700v\nwant it to end\n%s", message.name, message.memory, err, want)
		}
		if held := peak(); held > 3*message.memory+message.memory/2 {
			t.Errorf("%s: the worker held %d bytes over a message of %d, %.1f times its length; want three times at most, and a little", message.name, held, message.memory, float64(held)/float64(message.memory))
		}
	}
	chained := workerCollector("chaining", fmt.Sprintf(`
import tracemalloc
if not tracemalloc.is_tracing(): tracemalloc.start()
if response.text == "peak":
    metric("peak", value=tracemalloc.get_traced_memory()[1])
else:
    def chain(n):
        try:
            if n: chain(n - 1)
        finally:
            raise ValueError(str(n) * %d)
    chain(4)
`, characters))
	_, err := failingRun(t, chained, "chain")
	if err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("\nValueError: %s... (%d bytes)", strings.Repeat("4", 188), characters+len("ValueError: "))) || !strings.Contains(err.Error(), " lines)\n") {
		t.Fatalf("a chain of exceptions of %d characters each fails with %.900v", characters, err)
	}
	set, err := failingRun(t, chained, "peak")
	if err != nil {
		t.Fatal(err)
	}
	// Five messages, which the exceptions hold, and two more of one at most.
	if held := int(workerMetricValue(t, set, "peak")); held > 7*characters+characters/2 {
		t.Errorf("the worker held %d bytes over five messages of %d, %.1f times one; want seven times at most, and a little", held, characters, float64(held)/float64(characters))
	}
}

// alteredLauncher is pythonWorkerLauncher with was, which it has once,
// replaced by now.
func alteredLauncher(t *testing.T, was, now string) string {
	t.Helper()
	if strings.Count(pythonWorkerLauncher, was) != 1 {
		t.Fatalf("the launcher has %q %d times, not once", was, strings.Count(pythonWorkerLauncher, was))
	}
	return strings.Replace(pythonWorkerLauncher, was, now, 1)
}

// launchWorkersWith makes the test's pool start its workers with launcher.
func launchWorkersWith(t *testing.T, launcher string) {
	t.Helper()
	PythonWorkers().start = func(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
		return startPythonWorkerRunning(ctx, spec, launcher)
	}
}

// runningCollector is a collector whose transform runs the script its
// response is, so that one worker runs one script after another. The frames
// of that script are not shown: they are of a "<string>", as the worker's
// own are.
func runningCollector(name string, limit model.ByteSize) *model.Collector {
	c := workerCollector(name, "exec(response.text)")
	c.Limits.MaxOutputBytes = limit
	return c
}

// unwritten is the error of a script whose exception was of the type named
// when the worker could not write its text, of what.
func unwritten(named, fault string) string {
	return "python transform failed: " + named + ": (the text of this error could not be written: " + fault + ")"
}

// A worker in which cutting an error raises still answers that the script
// failed: with the type of the script's exception, that the text of the
// error could not be written and the type of what was raised over it, in
// place of the traceback. Whatever was raised there was raised with nothing
// around it: the interpreter ended, the run failed as a worker's that had
// exited, and the next run paid for another. Here the cut is stopped by a
// line the test puts into the launcher, and one worker serves every script.
//
// The type is named as a traceback names it, by its first 80 characters, so
// a class with a name of 100,000 characters makes no long line; a class
// whose module is no string is of "<unknown>"; and under a limit too small
// for the sentence the error is as many of its first characters as fit:
// one, under the 27 bytes a worker needs at least. An error of ordinary
// length is not cut, and is the traceback it was.
func TestAnErrorAWorkerCannotCutIsStillTheScriptsFailure(t *testing.T) {
	requirePython(t)
	launchWorkersWith(t, alteredLauncher(t, "        lines=0; own=0; skipping=False;", "        raise ZeroDivisionError('the test stops the cut here')\n        lines=0; own=0; skipping=False;"))
	c := runningCollector("uncut", 1<<20)
	for _, run := range []struct{ script, want string }{
		{"raise ValueError('x' * 20000)", unwritten("ValueError", "ZeroDivisionError")},
		{"fail('x' * 20000)", unwritten("RuntimeError", "ZeroDivisionError")},
		{"raise ValueError('ordinary')", "python transform failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    exec(response.text)\nValueError: ordinary"},
		{"class Odd(Exception): pass\nraise Odd('x' * 20000)", unwritten("__collector__.Odd", "ZeroDivisionError")},
		{"raise type('N' * 100000, (Exception,), {})('x' * 20000)", unwritten("__collector__."+strings.Repeat("N", 66), "ZeroDivisionError")},
		{"class Odd(Exception): pass\nOdd.__module__ = 5\nraise Odd('x' * 20000)", unwritten("<unknown>.Odd", "ZeroDivisionError")},
		{"raise type('\\u20ac' * 100, (Exception,), {'__module__': 'builtins'})('x' * 20000)", unwritten(strings.Repeat("\u20ac", 80), "ZeroDivisionError")},
	} {
		_, err := failingRun(t, c, run.script)
		if err == nil || err.Error() != run.want || !errors.Is(err, model.ErrScriptFailed) {
			t.Errorf("%s\nfails with\n%.700v\nwant\n%s", run.script, err, run.want)
		}
	}
	if set, err := failingRun(t, c, "metric('m', value=1)"); err != nil || len(set.Metrics) != 1 {
		t.Errorf("a metric after them: %v, %v", set, err)
	}
	if now := PythonWorkers().Snapshot(c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 7 || now.Runs[pythonRunOK] != 1 || len(now.Runs) != 2 {
		t.Errorf("after seven errors and a metric: %d workers started, stopped %v, runs %v", now.Starts, now.Stops, now.Runs)
	}
	least := runningCollector("uncut-least", 27)
	for range 2 {
		if _, err := failingRun(t, least, "raise ValueError('x' * 20000)"); err == nil || err.Error() != "python transform failed: V" {
			t.Errorf("under 27 bytes for an answer the error is\n%v\nwant its first character", err)
		}
	}
	if now := PythonWorkers().Snapshot(least.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 2 {
		t.Errorf("under 27 bytes for an answer: %d workers started, stopped %v, runs %v", now.Starts, now.Stops, now.Runs)
	}
}

// What cuts an error is compiled when an error first needs it, not when the
// worker starts: a worker whose launcher holds source there that is no
// Python starts, runs scripts and writes an error of ordinary length as
// every worker does, and fails a script whose error must be cut with the
// type of its exception and the SyntaxError the source is — each time, the
// source being compiled anew until it compiles, with nothing of a failed
// attempt kept — and carries on. Compiled at every start, it was a tenth of
// what starting a worker takes.
func TestWhatCutsAnErrorIsCompiledWhenAnErrorNeedsIt(t *testing.T) {
	requirePython(t)
	launchWorkersWith(t, alteredLauncher(t, "    import re,collections\n", "    import re,collections\n    this is no Python\n"))
	c := runningCollector("uncompiled", 1<<20)
	for _, run := range []struct{ script, want string }{
		{"raise ValueError('ordinary')", "python transform failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    exec(response.text)\nValueError: ordinary"},
		{"raise ValueError('x' * 20000)", unwritten("ValueError", "SyntaxError")},
		{"raise KeyError('x' * 20000)", unwritten("KeyError", "SyntaxError")},
	} {
		if _, err := failingRun(t, c, run.script); err == nil || err.Error() != run.want {
			t.Errorf("%s\nfails with\n%.700v\nwant\n%s", run.script, err, run.want)
		}
	}
	if set, err := failingRun(t, c, "metric('m', value=1)"); err != nil || len(set.Metrics) != 1 {
		t.Errorf("a metric after them: %v, %v", set, err)
	}
	if now := PythonWorkers().Snapshot(c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 3 || now.Runs[pythonRunOK] != 1 {
		t.Errorf("after three errors and a metric: %d workers started, stopped %v, runs %v", now.Starts, now.Stops, now.Runs)
	}
}

// An error the traceback module cannot make is still the script's failure,
// in the worker as it is: an exception of a class whose name cannot be
// asked for, which that module asks for, fails the run with "an exception"
// and the type of what asking raised, and a script that has taken the
// module's TracebackException away fails with the type of its exception
// and the AttributeError. Either ended the interpreter, and the run failed
// as "the interpreter exited". The worker runs the next script.
func TestAnErrorTheTracebackModuleCannotMakeIsStillTheScriptsFailure(t *testing.T) {
	requirePython(t)
	c := runningCollector("unmade", 1<<20)
	for _, run := range []struct{ script, want string }{
		{"class Unnamed(type):\n    def __getattribute__(cls, name):\n        if name == '__qualname__': raise LookupError('no name')\n        return super().__getattribute__(name)\nclass Odd(Exception, metaclass=Unnamed): pass\nraise Odd('short')", unwritten("an exception", "LookupError")},
		{"import traceback\nformer = traceback.TracebackException\ntraceback.TracebackException = None\ndef restore(): traceback.TracebackException = former\nbuiltins.restore = restore\nraise ValueError('short')", unwritten("ValueError", "AttributeError")},
		{"restore()\nraise ValueError('short')", "python transform failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    exec(response.text)\nValueError: short"},
	} {
		_, err := failingRun(t, c, run.script)
		if err == nil || err.Error() != run.want || !errors.Is(err, model.ErrScriptFailed) {
			t.Errorf("%s\nfails with\n%.700v\nwant\n%s", run.script, err, run.want)
		}
	}
	if now := PythonWorkers().Snapshot(c.Name); now.Starts != 1 || len(now.Stops) != 0 || now.Runs[pythonRunScriptError] != 3 || len(now.Runs) != 1 {
		t.Errorf("after three errors: %d workers started, stopped %v, runs %v", now.Starts, now.Stops, now.Runs)
	}
}

// What is shown of a script's error fits a limit of 16 KiB for an answer
// whatever its characters are, and no smaller limit is promised that
// (docs/PYTHON.md). A message of two words with 2,000 empty lines between
// them, raised five calls deep, has no character in it that JSON writes
// long, and what is shown of it is still a line of more than 2 KiB, each
// empty line being one byte of what is shown and two characters written:
// under a limit of exactly that line the error is the exporter's, and
// under 16 KiB too.
//
// The longest line there can be is of bytes that JSON writes as six
// characters each, as it writes a control character: six for each byte
// shownScriptError can show, and the brackets, quotes and commas of the
// nineteen parts they can be in, which is within 16 KiB; seven lines of
// control characters, which are shown by 200 bytes each, come near it.
func TestWhatIsShownOfAnErrorFitsALimitOfSixteenKiB(t *testing.T) {
	requirePython(t)
	const script = "def f(n, m):\n    if n == 0:\n        raise LookupError(m)\n    return f(n - 1, m)  # one more frame of the script, as long as a line of code is\nf(5, 'first' + '\\n' * 2000 + 'last')\n"
	c := &model.Collector{Name: "empty-lines"}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
	request, err := pythonRequest("metrics", script, &decode.Decoded{Kind: "text", Data: "value=7", Raw: r.Body}, r, c)
	if err != nil {
		t.Fatal(err)
	}
	under := func(limit int) (string, error) {
		line, _, err := PythonWorkers().run(t.Context(), pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: limit, Scripts: "empty-lines"}, request, time.Minute)
		if err != nil {
			t.Fatalf("under %d bytes: the run fails with %v", limit, err)
		}
		_, failure := pythonResult(c, "transform", time.Minute, line, nil)
		if failure == nil || !strings.Contains(failure.Error(), "\nLookupError: first\n") {
			t.Fatalf("under %d bytes: the answer %.200q is read as %v", limit, line, failure)
		}
		return string(line), failure
	}
	// 16 KiB hold the error whole, as it is written.
	wholeLine, failure := under(failureWhole)
	var whole pythonOutput
	if err := json.Unmarshal([]byte(wholeLine), &whole); err != nil || whole.Error == "" {
		t.Fatalf("under 16 KiB the answer is %.200q, %v", wholeLine, err)
	}
	shown := shownScriptError(strings.TrimSpace(whole.Error))
	want, exact := model.Errorf("python transform failed: %s", shown), shownLine(shown)
	if failure.Error() != want.Error() {
		t.Errorf("under 16 KiB the error is\n%s\nwant\n%s", failure, want)
	}
	if len(exact) <= 2048 || len(exact) > failureWhole {
		t.Fatalf("what is shown of the error is a line of %d bytes; want one that 2 KiB do not hold and 16 KiB do", len(exact))
	}
	if line, failure := under(len(exact)); line != exact || failure.Error() != want.Error() || model.SameFailureText(failure) != model.SameFailureText(want) {
		t.Errorf("under the %d bytes of what is shown the answer is\n%.300q\nwant\n%.300q\nand the error\n%s\nwant\n%s", len(exact), line, exact, failure, want)
	}
	// Every part between brackets, each text in quotes, a comma and a space
	// between two.
	const parts = 19
	const longest = 6*shownByWorkerBytes + len(`{"ok": false, "shown": []}`) + 2*(parts+1)/2 + 2*(parts-1)
	control := shownLine(shownScriptError(strings.Repeat(strings.Repeat("\x01", 250)+"\n", 20) + "last"))
	if longest > failureWhole || len(control) > longest || len(control) < failureWhole/2 {
		t.Errorf("what is shown of lines of control characters is a line of %d bytes, and the longest there can be one of %d; want them within 16 KiB, and the first more than half of that", len(control), longest)
	}
}

// formerShownByWorker is shownByWorker as it was: whatever the parts hold.
func formerShownByWorker(parts []any) model.QuotedValue {
	var said, same strings.Builder
	for _, part := range parts {
		switch part := part.(type) {
		case string:
			said.WriteString(part)
			same.WriteString(part)
		case json.Number:
			said.WriteString(part.String())
			same.WriteString(model.MovingMark)
		}
	}
	return model.ShownAs(said.String(), same.String())
}

// What the exporter makes of the parts a worker shows an error in is never
// longer than what it shows of an error itself. A script can write to the
// answers on its own, as it always could write an error there, and parts of
// a megabyte were an error of a megabyte, where the error a worker sent
// whole was cut here. Parts that come to more than a worker shows of any
// error (shownByWorkerBytes) are shown as an error's text of that length
// is: by its lines, each by its start. Parts within that, which are all
// that a worker writes, are the text they were, a number in its place and
// the mark where the failure is recognised; and a part that is neither a
// text nor a number — null, a boolean, a list, a dict — is passed over, as
// it was.
func TestWhatAWorkerShowsOfAnErrorIsNoLongerThanAnErrorIs(t *testing.T) {
	usePythonPool(t)
	c := &model.Collector{Name: "shown"}
	// The longest a worker shows: seven lines cut, one of a byte, and the
	// lines left out before them and after, each number of nineteen digits.
	const digits = "9223372036854775807"
	cut := `\n` + strings.Repeat("x", 200) + `... (", ` + digits + `, " bytes)`
	longest := `["... (", ` + digits + `, " lines)` + strings.Repeat(cut, 7) + `\na\n... (", ` + digits + `, " lines)"]`
	// A megabyte of parts, as the default limit for an answer holds them,
	// and enough of them under the race detector to be far past the bound.
	long, many, generated := alloctest.UnlessRaced(1<<20, 1<<14), alloctest.UnlessRaced(20000, 2000), alloctest.UnlessRaced(300, 60)
	random := rand.New(rand.NewPCG(38, 3))
	var within, past int
	shapes := []string{
		`[]`, `[""]`, `["a", 1, "b"]`, `["#", 5, "#"]`, `[1, 2]`, `["Traceback\n  File \"f\"\nE: m... (", 2000, " bytes)\n... (", 30, " lines)"]`,
		`[null]`, `[null, true, {}, [1, ["x"]], 1.5, -3, 1e999, 0.0000001, 123456789012345678901234567890]`, `[["nested"]]`, `[{"a": "b"}, false]`,
		`["\ud800", "\u0000", "\ud83d", "\ude00"]`, `[" \n spaced \n ", 7, " \n"]`,
		longest, strings.Replace(longest, `["`, `["x`, 1), strings.Replace(longest, digits, digits+"0", 1),
		`["` + strings.Repeat("x", long) + `"]`, `["` + strings.Repeat(`line\n`, many) + `"]`, `["  File \"f\"\n` + strings.Repeat("y", 3000) + `"]`,
		`[` + strings.Repeat(`1, `, many) + `1]`, `[` + strings.Repeat("7", many) + `]`, `[1e` + strings.Repeat("9", many) + `, -0.` + strings.Repeat("0", many) + `1]`,
		`["` + strings.Repeat(" ", many) + `"]`, `[` + strings.Repeat(`["x"], `, many) + `"y"]`,
	}
	for range generated {
		// Parts of every sort that come to around the bound.
		var shape []string
		for n := random.IntN(20); n > 0; n-- {
			switch random.IntN(6) {
			case 0:
				shape = append(shape, strconv.Itoa(random.IntN(1<<30)))
			case 1:
				shape = append(shape, []string{`null`, `true`, `[1]`, `{"a": 1}`, `-2.5`}[random.IntN(5)])
			default:
				shape = append(shape, strconv.Quote(strings.Repeat([]string{"x", "\u20ac", "line\n", "  File \"f\"\n", " "}[random.IntN(5)], random.IntN(120))))
			}
		}
		shapes = append(shapes, "["+strings.Join(shape, ", ")+"]")
	}
	for _, shape := range shapes {
		var parts []any
		decoder := json.NewDecoder(strings.NewReader(shape))
		decoder.UseNumber()
		if err := decoder.Decode(&parts); err != nil {
			t.Fatalf("%.60s: %v", shape, err)
		}
		was, shown := formerShownByWorker(parts), shownByWorker(parts)
		want := was
		if len(was.String()) > shownByWorkerBytes {
			past++
			want = shownScriptError(strings.TrimSpace(was.String()))
		} else {
			within++
		}
		if shown.String() != want.String() || shown.Same() != want.Same() || len(shown.String()) > shownByWorkerBytes || len(shown.Same()) > shownByWorkerBytes {
			t.Errorf("%.60s: the parts are shown as %d bytes, recognised by %d\n%.300q\nwant %d and %d\n%.300q", shape, len(shown.String()), len(shown.Same()), shown, len(want.String()), len(want.Same()), want)
		}
		// And so is the transform's error.
		_, err := pythonResult(c, "transform", time.Minute, []byte(`{"ok": false, "shown": `+shape+`}`), nil)
		if err == nil || err.Error() != "python transform failed: "+want.String() || model.SameFailureText(err) != "python transform failed: "+want.Same() || len(err.Error()) > model.MaxFailureBytes {
			t.Errorf("%.60s: the answer is read as an error of %d bytes: %.300v", shape, len(err.Error()), err)
		}
	}
	if within < generated/3 || past < generated/3 {
		t.Errorf("%d of the answers are within what a worker shows and %d past it; want %d of each", within, past, generated/3)
	}
	// The longest a worker shows is exactly the bound.
	var parts []any
	decoder := json.NewDecoder(strings.NewReader(longest))
	decoder.UseNumber()
	if err := decoder.Decode(&parts); err != nil || len(shownByWorker(parts).String()) != shownByWorkerBytes {
		t.Errorf("the longest a worker shows is %d bytes, %v; want %d", len(shownByWorker(parts).String()), err, shownByWorkerBytes)
	}
}
