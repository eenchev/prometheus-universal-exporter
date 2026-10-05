package transform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A worker's answer is read by readPythonAnswer (pythonanswer.go) rather
// than by encoding/json into maps. These tests compare the two: for every
// line — an answer as a worker writes it, one with a metric that is not what
// metric(...) appends, one that is no answer, one cut short — the series
// are the series they were, in their order, the data is the data it was,
// and the error is the error it was (oraclePythonAnswer,
// oraclePythonSeries, oraclePythonData).

// pyDict is a dict as a script made it, its keys in their order, which may
// hold one twice, as no dict does and as a line that is not a worker's can.
type pyDict []pyPair

type pyPair struct {
	key   any
	value any
}

// pyRaw is text written into a line as it is.
type pyRaw string

// pyDumps writes v as Python's json.dumps writes it: ", " and ": " between
// things, everything but printable ASCII escaped, a float as repr gives it.
func pyDumps(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case pyRaw:
		b.WriteString(string(x))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case *big.Int:
		b.WriteString(x.String())
	case float64:
		switch abs := math.Abs(x); {
		case x == math.Trunc(x) && abs < 1e16:
			b.WriteString(strconv.FormatFloat(x, 'f', 1, 64))
		case abs >= 1e16 || abs < 1e-4:
			b.WriteString(strconv.FormatFloat(x, 'e', -1, 64))
		default:
			b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
		}
	case string:
		b.WriteByte('"')
		for _, r := range x {
			switch {
			case r == '"' || r == '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\r':
				b.WriteString(`\r`)
			case r == '\t':
				b.WriteString(`\t`)
			case r == '\b':
				b.WriteString(`\b`)
			case r == '\f':
				b.WriteString(`\f`)
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xffff:
				high, low := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, high, low)
			default:
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
		b.WriteByte('"')
	case []any:
		b.WriteByte('[')
		for i, element := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			pyDumps(b, element)
		}
		b.WriteByte(']')
	case pyDict:
		b.WriteByte('{')
		for i, pair := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			pyDumps(b, pair.key)
			b.WriteString(": ")
			pyDumps(b, pair.value)
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("pyDumps: %T", v))
	}
}

func pyLine(v any) []byte {
	var b strings.Builder
	pyDumps(&b, v)
	return []byte(b.String())
}

// answerTexts are strings of every kind an answer holds: plain ones, ones
// json.dumps escapes, markers of floats that are not finite, and numbers as
// text.
var answerTexts = []string{
	"", "up", "item_value", "gauge", "counter", "untyped", "histogram", "id", "region", "eu-1", "eu-2", "item-17", "a b", `q"uote`, `back\slash`, "line\nbreak\ttab",
	"\x00", "\x01\x1f\x7f", "ünïcode 日本", "\U0001F600", "\u2028", "\ufffd", strings.Repeat("long ", 20),
	nonFiniteMarker + "NaN\x00", nonFiniteMarker + "+Inf\x00", nonFiniteMarker + "-Inf\x00", nonFiniteMarker + "NaN", "\x00other",
	"12", " 12.5 ", "1e3", "0x10", "1_000", "NaN", "inf", "-Inf", "true", "null",
}

// answerNumbers are numbers as a line can write them: as json.dumps writes
// an int and a float, and otherwise, among them what no float64 holds, what
// no int64 holds and what JSON does not allow.
var answerNumbers = []pyRaw{
	"0", "1", "-1", "7", "1700000000000", "9007199254740993", "9223372036854775807", "9223372036854775808", "-9223372036854775809", "123456789012345678901234567890",
	"0.0", "-0.0", "1.0", "1.5", "5e-324", "1e+16", "1.5e-05", "1.7976931348623157e+308", "1E5", "1e5", "-2.5E-3", "0.1", "100.0", "1e400", "-1e400", "1e-400",
	"01", "+1", "1.", ".5", "1e", "-", "0x1", "1_0", "NaN", "Infinity", "-Infinity",
}

// answerGenerator writes random answers.
type answerGenerator struct {
	random *rand.Rand
}

func (g *answerGenerator) chance(n int) bool { return g.random.IntN(n) == 0 }

func (g *answerGenerator) text() string { return answerTexts[g.random.IntN(len(answerTexts))] }

func (g *answerGenerator) number() pyRaw { return answerNumbers[g.random.IntN(len(answerNumbers))] }

// anything is a value of any kind, nested depth deep at most.
func (g *answerGenerator) anything(depth int) any {
	kinds := 9
	if depth <= 0 {
		kinds = 6
	}
	switch g.random.IntN(kinds) {
	case 0:
		return nil
	case 1:
		return g.chance(2)
	case 2:
		return g.number()
	case 3:
		return g.random.NormFloat64() * math.Pow(10, float64(g.random.IntN(40)-20))
	case 4, 5:
		return g.text()
	case 6:
		list := make([]any, g.random.IntN(4))
		for i := range list {
			list[i] = g.anything(depth - 1)
		}
		return list
	default:
		dict := make(pyDict, g.random.IntN(4))
		for i := range dict {
			dict[i] = pyPair{g.text(), g.anything(depth - 1)}
		}
		return dict
	}
}

// labels is a metric's labels: strings under names, as metric(...) leaves
// them, and now and then something a script put there itself. At most one
// of them is a list or a dict, which is an error: of two, the one reported
// was never determined.
func (g *answerGenerator) labels() any {
	dict := make(pyDict, g.random.IntN(5))
	bad := false
	for i := range dict {
		dict[i] = pyPair{g.text(), g.text()}
		if g.chance(8) {
			dict[i].key = []string{"id", "region", "le", "id"}[g.random.IntN(4)]
		}
		if g.chance(10) {
			value := g.anything(1)
			switch value.(type) {
			case []any, pyDict:
				if bad {
					continue
				}
				bad = true
			}
			dict[i].value = value
		}
	}
	return dict
}

// asLabels is v as a script could leave a metric's labels and have one
// error for it: a dict among whose values at most one is a list or a dict.
func (g *answerGenerator) asLabels(key, v any) any {
	dict, ok := v.(pyDict)
	if !ok || key != "labels" {
		return v
	}
	bad := false
	for i := range dict {
		switch dict[i].value.(type) {
		case []any, pyDict:
			if bad {
				dict[i].value = g.text()
			}
			bad = true
		}
	}
	return dict
}

// metric is an entry of metrics: as metric(...) appends it, or with one or
// two things about it otherwise.
func (g *answerGenerator) metric() any {
	if g.chance(40) {
		return g.anything(2)
	}
	entry := pyDict{
		{"name", []string{"item_value", "up", "jobs", g.text()}[g.random.IntN(4)]},
		{"type", []string{"gauge", "counter", "untyped", ""}[g.random.IntN(4)]},
		{"value", g.random.NormFloat64() * math.Pow(10, float64(g.random.IntN(30)-10))},
		{"labels", g.labels()},
		{"help", []string{"", "", "Items in the queue.", g.text()}[g.random.IntN(4)]},
		{"timestamp", nil},
	}
	if g.chance(3) {
		entry[2].value = float64(g.random.IntN(100000))
	}
	if g.chance(4) {
		entry[5].value = []any{1700000000000, g.number(), 1.5e12}[g.random.IntN(3)]
	}
	for changes := g.random.IntN(8) - 5; changes > 0; changes-- {
		at := g.random.IntN(len(entry))
		switch g.random.IntN(8) {
		case 0:
			entry = append(entry[:at:at], entry[at+1:]...)
			if len(entry) == 0 {
				return entry
			}
		case 1:
			entry[at].value = g.asLabels(entry[at].key, g.anything(2))
		case 2:
			entry[at].value = nil
		case 3:
			entry[at].value = g.number()
		case 4:
			entry[at].value = g.text()
		case 5:
			key := []any{"extra", "name", "value", "labels", "Name", g.text(), pyRaw(`"na\u006de"`), pyRaw("7")}[g.random.IntN(8)]
			entry = append(entry, pyPair{key, g.asLabels(key, g.anything(2))})
		case 6:
			entry = append(entry, entry[at])
			entry[len(entry)-1].value = g.anything(1)
		default:
			g.random.Shuffle(len(entry), func(i, j int) { entry[i], entry[j] = entry[j], entry[i] })
		}
	}
	return entry
}

// answer is a line: an answer of a transform or of a pre-script as a worker
// writes it, with now and then something about it otherwise.
func (g *answerGenerator) answer() []byte {
	var answer pyDict
	switch g.random.IntN(8) {
	case 0:
		answer = pyDict{{"ok", false}, {"error", "Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n" + g.text() + "\n"}}
	case 1, 2:
		answer = pyDict{{"ok", true}, {"log", g.text()}, {"data", g.anything(4)}}
	default:
		metrics := make([]any, g.random.IntN(6))
		for i := range metrics {
			metrics[i] = g.metric()
		}
		answer = pyDict{{"ok", true}, {"log", ""}, {"metrics", metrics}}
	}
	for changes := g.random.IntN(12) - 9; changes > 0; changes-- {
		at := g.random.IntN(len(answer))
		switch g.random.IntN(7) {
		case 0:
			answer = append(answer[:at:at], answer[at+1:]...)
			if len(answer) == 0 {
				return []byte("{}")
			}
		case 1:
			answer[at].value = g.anything(2)
		case 2:
			answer = append(answer, pyPair{[]any{"OK", "Log", "metrics", "data", "error", "ok", "started", g.text(), pyRaw(`"o\u006b"`)}[g.random.IntN(9)], g.anything(2)})
		case 3:
			answer = append(answer, answer[at])
		case 4:
			answer[at].value = nil
		default:
			g.random.Shuffle(len(answer), func(i, j int) { answer[i], answer[j] = answer[j], answer[i] })
		}
	}
	line := pyLine(answer)
	if g.chance(12) {
		// Written without the spaces of json.dumps, or with more.
		line = []byte(strings.NewReplacer(`", "`, `","`, `": `, `":`).Replace(string(line)))
	}
	if g.chance(12) {
		line = []byte(" \t" + strings.NewReplacer(`": `, "\" :\t ", `, "`, " ,\r\"").Replace(string(line)) + " ")
	}
	return line
}

// corrupt returns line with a small change: a byte removed, replaced or
// added, the line cut short, or something after it.
func (g *answerGenerator) corrupt(line []byte) []byte {
	const alphabet = "{}[]\",:\\ \t-+.eE019ntfu\x00\x1f\x7f\x80\xff"
	out := append([]byte(nil), line...)
	if len(out) == 0 {
		return []byte{alphabet[g.random.IntN(len(alphabet))]}
	}
	at := g.random.IntN(len(out))
	switch g.random.IntN(5) {
	case 0:
		out = append(out[:at], out[at+1:]...)
	case 1:
		out[at] = alphabet[g.random.IntN(len(alphabet))]
	case 2:
		out = append(out[:at], append([]byte{alphabet[g.random.IntN(len(alphabet))]}, out[at:]...)...)
	case 3:
		out = out[:at]
	default:
		out = append(out, []string{"}", " x", "{}", ",", "\n{\"ok\": true}", "]"}[g.random.IntN(6)]...)
	}
	return out
}

// sameSeries says where two sets of series differ, or nothing: a value by
// its bits, so that NaN is itself, and a timestamp by what it points to.
func sameSeries(old, got *model.MetricSet) string {
	if (old == nil) != (got == nil) {
		return fmt.Sprintf("set %v, was %v", got, old)
	}
	if old == nil {
		return ""
	}
	if len(old.Metrics) != len(got.Metrics) || (old.Metrics == nil) != (got.Metrics == nil) {
		return fmt.Sprintf("%d series, were %d", len(got.Metrics), len(old.Metrics))
	}
	for i, was := range old.Metrics {
		is := got.Metrics[i]
		sameTimestamp := (was.Timestamp == nil) == (is.Timestamp == nil) && (was.Timestamp == nil || *was.Timestamp == *is.Timestamp)
		sameLabels := len(was.Labels) == len(is.Labels) && (was.Labels == nil) == (is.Labels == nil)
		for name, value := range was.Labels {
			if other, has := is.Labels[name]; !has || other != value {
				sameLabels = false
			}
		}
		if was.Name != is.Name || was.Help != is.Help || was.Type != is.Type || math.Float64bits(was.Value) != math.Float64bits(is.Value) || !sameTimestamp || !sameLabels || is.Histogram != nil || is.Summary != nil || is.Created != was.Created {
			return fmt.Sprintf("series %d is %+v, was %+v", i, is, was)
		}
	}
	return ""
}

// sameData says where what two readings of a pre-script's data made differ,
// or nothing: of the same types throughout.
func sameData(old, got any, path string) string {
	switch x := old.(type) {
	case map[string]any:
		y, ok := got.(map[string]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
		for key, value := range x {
			other, has := y[key]
			if !has {
				return fmt.Sprintf("%s: no key %q", path, key)
			}
			if diff := sameData(value, other, path+"."+key); diff != "" {
				return diff
			}
		}
	case []any:
		y, ok := got.([]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
		for i := range x {
			if diff := sameData(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); diff != "" {
				return diff
			}
		}
	case float64:
		if y, ok := got.(float64); !ok || math.Float64bits(x) != math.Float64bits(y) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
	case *big.Int:
		if y, ok := got.(*big.Int); !ok || x.Cmp(y) != 0 {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
	case int, string, bool, nil:
		if old != got {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
	default:
		return fmt.Sprintf("%s: a %T, which no answer holds", path, old)
	}
	return ""
}

// answerOutcome is how a line was read: its series or its data, and the
// error.
type answerOutcome int

const (
	answerNoAnswer answerOutcome = iota
	answerScriptFailed
	answerRefused
	answerSeries
)

// compareAnswer reads line as an answer is read and as it was read, as a
// transform's and as a pre-script's, under a limit of series, and reports
// any difference. It returns how the line was read.
func compareAnswer(t *testing.T, line []byte, limit int) answerOutcome {
	t.Helper()
	c := &model.Collector{Name: "answer"}
	for _, what := range []string{"transform", "pre-script"} {
		old, oldErr := oraclePythonAnswer(what, line)
		runs := PythonWorkers().Snapshot(c.Name).Runs
		got, gotErr := pythonResult(c, what, oracleTimeout, line, nil)
		if (oldErr == nil) != (gotErr == nil) || oldErr != nil && oldErr.Error() != gotErr.Error() {
			t.Fatalf("%q as a %s's: error %v, was %v", line, what, gotErr, oldErr)
		}
		// The run is counted as what it was: an answer, a script's error,
		// or a line that is no answer.
		outcome, result := pythonRunOK, answerSeries
		switch {
		case oldErr != nil && strings.Contains(oldErr.Error(), " output: "):
			outcome, result = pythonRunFailed, answerNoAnswer
		case oldErr != nil:
			outcome, result = pythonRunScriptError, answerScriptFailed
		}
		if now := PythonWorkers().Snapshot(c.Name).Runs; now[outcome] != runs[outcome]+1 {
			t.Fatalf("%q: the run is counted as %v, after %v, want one more %s", line, now, runs, outcome)
		}
		if oldErr != nil {
			if what == "pre-script" {
				return result
			}
			continue
		}
		if old.Log != got.Log {
			t.Fatalf("%q: log %q, was %q", line, got.Log, old.Log)
		}
		if what == "pre-script" {
			if diff := sameData(oraclePythonData(old), pythonData(got), "data"); diff != "" {
				t.Fatalf("%q: %s", line, diff)
			}
			continue
		}
		oldContext, gotContext := withSeriesBudget(context.Background(), limit), withSeriesBudget(context.Background(), limit)
		oldSet, oldErr := oraclePythonSeries(oldContext, old)
		gotSet, gotErr := pythonSeries(gotContext, got)
		if (oldErr == nil) != (gotErr == nil) || oldErr != nil && (oldErr.Error() != gotErr.Error() || errors.Is(oldErr, model.ErrScriptFailed) != errors.Is(gotErr, model.ErrScriptFailed) || errors.Is(oldErr, model.ErrLimitExceeded) != errors.Is(gotErr, model.ErrLimitExceeded)) {
			t.Fatalf("%q under a limit of %d: error %v, was %v", line, limit, gotErr, oldErr)
		}
		if diff := sameSeries(oldSet, gotSet); diff != "" {
			t.Fatalf("%q: %s", line, diff)
		}
		if seriesRoom(oldContext) != seriesRoom(gotContext) {
			t.Fatalf("%q under a limit of %d: room for %d more series, was %d", line, limit, seriesRoom(gotContext), seriesRoom(oldContext))
		}
		if oldErr != nil {
			return answerRefused
		}
	}
	return answerSeries
}

// Answers as a worker writes them and as it does not: metrics of every form
// metric(...) appends and of every form a script can append itself, what a
// pre-script leaves, a script's error, and lines that are no answer. Each is
// read as it was.
func TestPythonAnswerIsReadAsItWas(t *testing.T) {
	usePythonPool(t)
	marker := func(value string) string { return `"\u0000pue-nonfinite:` + value + `\u0000"` }
	metric := func(rest string) string { return `{"ok": true, "log": "", "metrics": [` + rest + `]}` }
	lines := []string{
		// As a worker writes them.
		`{"ok": true, "log": "", "metrics": []}`,
		metric(`{"name": "up", "type": "gauge", "value": 1.0, "labels": {}, "help": "", "timestamp": null}`),
		metric(`{"name": "jobs", "type": "counter", "value": 12.5, "labels": {"queue": "a", "state": "done"}, "help": "Jobs.", "timestamp": 1700000000000}`),
		metric(`{"name": "a", "type": "gauge", "value": 1.0, "labels": {"id": "1"}, "help": "", "timestamp": null}, {"name": "a", "type": "gauge", "value": 2.0, "labels": {"id": "2"}, "help": "", "timestamp": null}`),
		metric(`{"name": "nan", "type": "gauge", "value": ` + marker("NaN") + `, "labels": {}, "help": "", "timestamp": null}, {"name": "inf", "type": "gauge", "value": ` + marker("+Inf") + `, "labels": {"v": ` + marker("-Inf") + `}, "help": "", "timestamp": null}`),
		metric(`{"name": "\u00fcn\u00efcode \ud83d\ude00", "type": "untyped", "value": 1e+16, "labels": {"k\"q": "line\nbreak", "\u65e5": "\u672c"}, "help": "a \\ b", "timestamp": -1}`),
		`{"ok": true, "log": "printed\n", "data": {"items": [{"id": 1500000000000000001, "v": 0.5, "big": 123456789012345678901234567890, "none": null, "nan": ` + marker("NaN") + `}], "n": 1.0}}`,
		`{"ok": true, "log": "", "data": null}`, `{"ok": true, "log": "", "data": "text"}`, `{"ok": true, "log": "", "data": [[], {}, [{}]]}`, `{"ok": true, "log": "", "data": 5}`,
		`{"ok": false, "error": "Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\nRuntimeError: no\n"}`,
		`{"ok": false, "error": ""}`, `{"started": true}`, `{"ok": true, "ready": true}`,
		// What a script appends itself, and what is wrong with it.
		metric(`5`), metric(`null`), metric(`"text"`), metric(`[1, 2]`), metric(`{}`), metric(`true`),
		metric(`{"name": "a"}`), metric(`{"value": 1}`), metric(`{"name": "a", "value": 1}`), metric(`{"name": "a", "value": 1, "labels": null, "type": null, "help": null, "timestamp": null}`),
		metric(`{"name": 5, "value": 1}`), metric(`{"name": 5.0, "value": 1}`), metric(`{"name": null, "value": 1}`), metric(`{"name": ["a"], "value": 1}`), metric(`{"name": {"a": 1}, "value": 1}`),
		metric(`{"name": "a", "value": "12"}`), metric(`{"name": "a", "value": " 1.5 "}`), metric(`{"name": "a", "value": "abc"}`), metric(`{"name": "a", "value": true}`), metric(`{"name": "a", "value": false}`),
		metric(`{"name": "a", "value": null}`), metric(`{"name": "a", "value": [1]}`), metric(`{"name": "a", "value": {"v": 1}}`), metric(`{"name": "a", "value": 1e400}`), metric(`{"name": "a", "value": -1e400}`),
		metric(`{"name": "a", "value": 123456789012345678901234567890}`), metric(`{"name": "a", "value": 1E5}`), metric(`{"name": "a", "value": -0.0}`), metric(`{"name": "a", "value": 5e-324}`), metric(`{"name": "a", "value": 1e-400}`),
		metric(`{"name": "a", "value": 1, "type": 5}`), metric(`{"name": "a", "value": 1, "type": ""}`), metric(`{"name": "a", "value": 1, "type": "histogram"}`), metric(`{"name": "a", "value": 1, "help": 5}`), metric(`{"name": "a", "value": 1, "help": ["x"]}`),
		metric(`{"name": "a", "value": 1, "labels": []}`), metric(`{"name": "a", "value": 1, "labels": [1]}`), metric(`{"name": "a", "value": 1, "labels": "x"}`), metric(`{"name": "a", "value": 1, "labels": 5}`),
		metric(`{"name": "a", "value": 1, "labels": {"n": 5, "f": 0.5, "big": 1234567, "e": 1e+21, "t": true, "none": null, "s": "x"}}`), metric(`{"name": "a", "value": 1, "labels": {"l": [1, 2]}}`), metric(`{"name": "a", "value": 1, "labels": {"d": {"a": 1}}}`),
		metric(`{"name": "a", "value": 1, "labels": {"none": null}}`), metric(`{"name": "a", "value": 1, "labels": {"a": "1", "a": "2"}}`), metric(`{"name": "a", "value": 1, "labels": {"a": "1", "a": null}}`), metric(`{"name": "a", "value": 1, "labels": {"a": null, "a": "2"}}`),
		metric(`{"name": "a", "value": 1, "labels": {"m": ` + marker("NaN") + `, "p": ` + marker("+Inf") + `, "x": "\u0000pue-nonfinite:NaN", "y": "\u0000"}}`),
		metric(`{"name": "a", "value": 1, "timestamp": 1.5}`), metric(`{"name": "a", "value": 1, "timestamp": "17"}`), metric(`{"name": "a", "value": 1, "timestamp": "soon"}`), metric(`{"name": "a", "value": 1, "timestamp": true}`),
		metric(`{"name": "a", "value": 1, "timestamp": 1e19}`), metric(`{"name": "a", "value": 1, "timestamp": -1e19}`), metric(`{"name": "a", "value": 1, "timestamp": 9223372036854775807}`), metric(`{"name": "a", "value": 1, "timestamp": 1e400}`),
		metric(`{"name": "a", "value": 1, "timestamp": ` + marker("NaN") + `}`), metric(`{"name": "a", "value": 1, "timestamp": [1]}`), metric(`{"name": "a", "value": 1, "timestamp": 0}`),
		metric(`{"name": "a", "value": 1, "extra": {"deep": [1, {"x": "}]"}]}}`), metric(`{"name": "a", "name": "b", "value": 1}`), metric(`{"name": "a", "value": 1, "value": "x"}`), metric(`{"na\u006de": "a", "value": 1}`), metric(`{"Name": "a", "value": 1}`),
		metric(`{"name": "ok", "value": 1}, 5, {"name": "never", "value": 1}`), metric(`{"name": "ok", "value": 1}, {"name": 5, "value": 1}, {"name": "a", "value": "x"}`), metric(`{"name": "a", "value": "x"}, {"name": 5}`),
		// Not as a worker writes them, and no answer at all.
		`{"ok":true,"log":"","metrics":[{"name":"a","type":"gauge","value":1.0,"labels":{"k":"v"},"help":"","timestamp":null}]}`,
		" {\t\"ok\" : true ,\r \"metrics\" : [ { \"name\" : \"a\" , \"value\" : 1 } ] } ", `{"metrics": [{"name": "a", "value": 1}], "ok": true}`, `{"ok": true, "metrics": null}`, `{"ok": true}`, `{"ok": true, "log": null}`,
		`{"ok": true, "metrics": [{"name": "a", "value": 1}], "metrics": []}`, `{"ok": true, "ok": false, "error": "twice"}`, `{"OK": true, "Metrics": [{"name": "a", "value": 1}]}`, `{"ok": true, "o\u006b": false}`,
		`{"ok": true, "metrics": {}}`, `{"ok": true, "metrics": "x"}`, `{"ok": "yes"}`, `{"ok": 1}`, `{"ok": null}`, `{"ok": true, "log": 5}`, `{"ok": false, "error": 5}`, `{"ok": false, "error": null}`, `{"ok": true, "extra": [1, 2]}`,
		`{"ok": true, "log": "", "metrics": [{"name": "a", "value": 1}]} trailing`, `{"ok": true, "log": "", "metrics": [{"name": "a", "value": 1}]}{"ok": false}`, `{"ok": true, "metrics": [{"name": "a", "value": 1}]`, `{"ok": true, "metrics": [{"name": "a", "value": 1}`,
		`{"ok": true, "metrics": [{"name": "a", "value": 1},]}`, `{"ok": true, "metrics": [,]}`, `{"ok": true, "metrics": [{"name": "a" "value": 1}]}`, `{"ok": true, "metrics": [{"name": "a", "value": 01}]}`, `{"ok": true, "metrics": [{"name": "a", "value": 1.}]}`,
		`{"ok": true, "metrics": [{"name": "a", "value": NaN}]}`, `{"ok": true, "metrics": [{"name": "a\x01", "value": 1}]}`, "{\"ok\": true, \"metrics\": [{\"name\": \"a\xff\", \"value\": 1, \"labels\": {\"k\xe9\": \"v\xff\"}}]}", `{"ok": true, "metrics": [{"name": "\ud800", "value": 1}]}`,
		`{"ok": true, "metrics": [{"name": "a", "value": 1, "junk": {]}]}`, `{"ok": true, "metrics": [{"name": "a", "value": 1}], }`, `{"ok": true,, "metrics": []}`, `{"ok": tru}`, `{"ok": truefalse}`, `{"ok": true, "data": }`, `{"ok": true, "data": [1,]}`,
		`{"ok": true, "data": {"a": 1, "a": 2}}`, `{"ok": true, "data": 1e400}`, `{"ok": true, "data": [` + strings.Repeat("9", 5000) + `]}`, `[]`, `"text"`, `5`, `null`, `{}`, ``, ` `, `{`, `{"ok"`, `{"ok":`, `not json`, "\x00",
	}
	// An answer nested as deep as encoding/json reads, and deeper, as data
	// and as a metric's key.
	outcomes := map[answerOutcome]int{}
	for _, depth := range []int{5, pythonAnswerDepth - 3, pythonAnswerDepth - 2, pythonAnswerDepth - 1, pythonAnswerDepth, pythonAnswerDepth + 1} {
		nested := strings.Repeat("[", depth) + strings.Repeat("]", depth)
		for _, line := range []string{`{"ok": true, "log": "", "data": ` + nested + `}`, metric(`{"name": "a", "value": 1, "junk": ` + nested + `}`), metric(nested)} {
			outcomes[compareAnswer(t, []byte(line), 0)]++
		}
	}
	for _, line := range lines {
		for _, limit := range []int{0, 1, 2} {
			outcomes[compareAnswer(t, []byte(line), limit)]++
		}
	}
	for outcome, name := range map[answerOutcome]string{answerNoAnswer: "is no answer", answerScriptFailed: "is a script's error", answerRefused: "has a metric that is refused", answerSeries: "is read"} {
		if outcomes[outcome] == 0 {
			t.Errorf("no line of the table %s", name)
		}
	}
}

// 5,000 random answers, each as it is and corrupted twice, 15,000 lines in
// all, under limits of series that some of them pass: each is read as it
// was. Most of the answers are read without encoding/json, and most of the
// corrupted ones are no answer.
func TestRandomPythonAnswersAreReadAsTheyWere(t *testing.T) {
	usePythonPool(t)
	answers := 5000
	if testing.Short() {
		answers = 2000
	}
	g := &answerGenerator{random: rand.New(rand.NewPCG(7, 2026))}
	outcomes := map[answerOutcome]int{}
	read := 0
	for range answers {
		line := g.answer()
		if _, ok := readPythonAnswer(line); ok {
			read++
		}
		limit := []int{0, 0, 3, 5}[g.random.IntN(4)]
		outcomes[compareAnswer(t, line, limit)]++
		for range 2 {
			outcomes[compareAnswer(t, g.corrupt(line), limit)]++
		}
	}
	t.Logf("%d of %d answers read without encoding/json; of all the lines %d are no answer, %d a script's error, %d refused for a metric or the limit, %d read", read, answers, outcomes[answerNoAnswer], outcomes[answerScriptFailed], outcomes[answerRefused], outcomes[answerSeries])
	if read < answers*3/4 || outcomes[answerNoAnswer] < answers/2 || outcomes[answerRefused] < answers/10 || outcomes[answerSeries] < answers/2 || outcomes[answerScriptFailed] < answers/20 {
		t.Fatal("the generator no longer writes lines of every kind")
	}
}

// What readPythonAnswer does not read it says it does not, and what it reads
// it reads without encoding/json: every answer a worker writes of metrics
// that metric(...) appended.
func TestPythonAnswerOfAWorkerIsReadWithoutEncodingJSON(t *testing.T) {
	for line, want := range map[string]bool{
		`{"ok": true, "log": "", "metrics": []}`: true,
		`{"ok": true, "log": "x", "metrics": [{"name": "a", "type": "gauge", "value": 1.0, "labels": {"k": "v", "n": null}, "help": "", "timestamp": 17}]}`: true,
		`{"ok": true, "log": "", "metrics": [{"name": "a", "value": "5"}]}`:                                                                                 true,
		`{"ok": true, "log": "", "data": {"a": [1, 2.5, "x", null]}}`:                                                                                       true,
		`{"ok": false, "error": "no"}`:                           true,
		`{"ok": true, "log": "", "metrics": [], "extra": 1}`:     false,
		`{"ok": true, "ok": true}`:                               false,
		`{"OK": true}`:                                           false,
		`{"ok": true, "metrics": [{"name": "a", "value": 1}]} x`: false,
		`{"ok": true, "metrics": [{"name": "a", "value": 1}`:     false,
		`{"ok": true, "metrics": [{"name": "a", "value": 01}]}`:  false,
		`{"ok": true, "data": [1,]}`:                             false,
		`{}`:                                                     false,
		``:                                                       false,
	} {
		out, read := readPythonAnswer([]byte(line))
		if read != want {
			t.Errorf("%s: read %v, want %v", line, read, want)
		}
		if read && out.Metrics != nil {
			t.Errorf("%s: read into %d maps", line, len(out.Metrics))
		}
	}
}

// An answer of two thousand series, each with two labels, one of them the
// same for many series, is read in three allocations a series: its labels'
// map, which is two, and the text of the label that no other series has.
// Read by encoding/json into a map of each metric and of its labels, with a
// string for every key and every number, and then into series, it took 26.
func TestPythonAnswerAllocatesLittleForEachSeries(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	usePythonPool(t)
	metrics := make([]any, boundSeries)
	for i := range metrics {
		metrics[i] = pyDict{{"name", "item_value"}, {"type", "gauge"}, {"value", float64(i)}, {"labels", pyDict{{"id", fmt.Sprintf("item-%d", i)}, {"region", fmt.Sprintf("eu-%d", i%7)}}}, {"help", "Value of an item."}, {"timestamp", nil}}
	}
	line := pyLine(pyDict{{"ok", true}, {"log", ""}, {"metrics", metrics}})
	c := &model.Collector{Name: "bounded"}
	var set *model.MetricSet
	read := func() {
		out, err := pythonResult(c, "transform", oracleTimeout, line, nil)
		if err == nil {
			set, err = pythonSeries(context.Background(), out)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	read()
	if len(set.Metrics) != boundSeries || set.Metrics[boundSeries-1].Labels["id"] != fmt.Sprintf("item-%d", boundSeries-1) || set.Metrics[9].Labels["region"] != "eu-2" || set.Metrics[3].Value != 3 || set.Metrics[3].Help != "Value of an item." {
		t.Fatalf("%d series; %+v", len(set.Metrics), set.Metrics[min(9, len(set.Metrics)-1)])
	}
	if each := alloctest.AllocsAtMost(5, 6*boundSeries, read) / boundSeries; each > 6 {
		t.Errorf("%.1f allocations for each series, want at most 6", each)
	}
	was, _ := alloctest.Allocations(2, func() {
		out, err := oraclePythonAnswer("transform", line)
		if err == nil {
			_, err = oraclePythonSeries(context.Background(), out)
		}
		if err != nil {
			t.Fatal(err)
		}
	})
	if was /= boundSeries; was < 20 {
		t.Errorf("encoding/json read the answer in %.1f allocations a series: the bound above compares with nothing", was)
	}
}
