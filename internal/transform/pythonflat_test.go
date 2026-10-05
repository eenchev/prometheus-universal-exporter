package transform

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A metric is flat: a name, a type, a value, a help and a timestamp, each
// one value, and labels of one value each. A script can put a list or a dict
// where one value belongs, as an argument of metric(...) or in an entry it
// appends to metrics itself, and the scrape then fails saying which metric
// and which of its parts, and what stands there, by its kind and how many
// items it has. It said so only of a list or a dict nested a little: one
// nested about a thousand deep in an entry appended by hand, or holding
// itself, ended the worker's walk through the answer with the
// interpreter's own "RecursionError: maximum recursion depth exceeded", and
// one nested ten thousand deep as an argument of metric(...) ended the
// error that was to name it the same way. These tests put a list and a dict
// at every such place, nested a little and a lot, and want one failure of
// each place.

// flatPlace is a script that puts junk at the place the response's first
// line names: nested as deep as its second line says, of lists or of dicts
// under the key "a", or a list that holds itself, once or twice. A place
// that starts with "metric() " is an argument of metric(...), and any other
// a key of an entry appended to metrics by hand, after a metric that is
// what it should be.
const flatPlace = `
import collections
place, depth, shape = response.text.split("\n")
junk = 1
for _ in range(int(depth)):
    junk = {"lists": lambda v: [v], "dicts": lambda v: {"a": v}, "deques": lambda v: collections.deque([v]), "frozensets": lambda v: frozenset([v])}.get(shape, lambda v: v)(junk)
if shape == "itself":
    junk = [1]
    junk.append(junk)
if shape == "itself twice":
    junk = []
    junk.append(junk)
    junk.append(junk)
metric("before", value=1)
made = {"name": "m", "value": 1}
argument = place.startswith("metric() ")
if argument: place = place[9:]
if place == "entry": made = junk
elif place == "label": made["labels"] = {"l": junk, "other": "kept", "none": None}
elif place == "the metrics themselves": made = metrics
else: made[place] = junk
if argument: metric(**made)
else: metrics.append(made)
`

// flatCollector runs flatPlace under a memory limit, so that an answer gone
// through level after level, which for one holding itself twice doubles
// with each, ends there rather than filling the machine.
func flatCollector() *model.Collector {
	c := workerCollector("flat", flatPlace)
	c.Limits.MaxScriptMemory = 1 << 30
	return c
}

// runFlat runs the collector's script on a response of the given lines.
func runFlat(c *model.Collector, lines ...string) (*model.MetricSet, error) {
	body := strings.Join(lines, "\n")
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	return executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
}

// flatKinds is how the exporter names junk of each shape of flatPlace, and
// how metric(...) does: a list or a dict of one item whatever is nested in
// it, and a list of two that holds itself.
var flatKinds = []struct {
	shape, exporter, label, worker, python string
}{
	{"lists", "an array of 1 item", "an array of 1 values", "a list of 1 item", "list"},
	{"dicts", "an object with 1 key", "an object", "a dict of 1 item", "dict"},
	{"itself", "an array of 2 items", "an array of 2 values", "a list of 2 items", "list"},
	{"itself twice", "an array of 2 items", "an array of 2 values", "a list of 2 items", "list"},
}

// A list or a dict in an entry a script appends to metrics, where a metric
// has one value, fails the scrape as the script's failure, with the same
// words whether it is nested twice, a thousand times, or more than ten
// thousand, deeper than any response decodes, and when it holds itself. The
// words are the exporter's, naming the metric and its part, and no
// RecursionError of the worker's. One worker answers them all.
func TestAListOrDictInAMetricAppendedByHandFailsTheSameHoweverDeep(t *testing.T) {
	requirePython(t)
	c := flatCollector()
	depths := []int{2, 40, 480, 990, 1100, 5000, 10001, 30000}
	if raceDetector {
		depths = []int{2, 1100, 10001}
	}
	const (
		oneValue = `, not a single value; select one, or join them with join(",")`
		notText  = ` is not a string`
	)
	runs := 0
	for _, kind := range flatKinds {
		object := kind.python == "dict"
		labelled := `metric "m" label "l" is ` + kind.label + oneValue
		labels := `metric "m" labels are ` + kind.exporter + ", not a mapping of label names to values"
		entry := "metrics[1] is " + kind.exporter + ", not a metric; call metric(...), or append a mapping with a name and a value"
		if object {
			// A dict of dicts as an entry is one without a name, and as labels
			// it is a label that is a dict.
			labelled = `metric "m" label "l" is an object, not a single value; select one of its fields`
			labels = `metric "m" label "a" is an object, not a single value; select one of its fields`
			entry = "metric name null is not a string"
		}
		for place, want := range map[string]string{
			"entry":     entry,
			"name":      "metric name " + kind.exporter + notText,
			"type":      `metric "m" type ` + kind.exporter + notText + `; give "gauge", "counter" or "untyped"`,
			"help":      `metric "m" help ` + kind.exporter + notText,
			"value":     `metric "m" value ` + kind.exporter + " is not a number",
			"timestamp": `metric "m" timestamp ` + kind.exporter + " is not a number of milliseconds",
			"labels":    labels,
			"label":     labelled,
		} {
			for _, depth := range depths {
				if strings.HasPrefix(kind.shape, "itself") && depth != depths[0] {
					continue
				}
				_, err := runFlat(c, place, strconv.Itoa(depth), kind.shape)
				runs++
				if err == nil || err.Error() != "python transform: "+want || !errors.Is(err, model.ErrScriptFailed) {
					t.Fatalf("%s of %s nested %d deep: %.400v, want the script's failure %q", place, kind.shape, depth, err, "python transform: "+want)
				}
			}
		}
	}
	// The metrics appended to themselves are a list of as many items as
	// there are metrics, and no metric.
	_, err := runFlat(c, "the metrics themselves", "1", "lists")
	runs++
	if want := "python transform: metrics[1] is an array of 2 items, not a metric; call metric(...), or append a mapping with a name and a value"; err == nil || err.Error() != want {
		t.Fatalf("the metrics appended to themselves: %.400v, want %q", err, want)
	}
	// And the worker is there for what comes next.
	set, err := runFlat(c, "help", "0", "lists")
	runs++
	if err == nil || !strings.Contains(err.Error(), `metric "m" help 1 is not a string`) || set != nil {
		t.Fatalf("a help that is a number, after the deepest: %v, %v", set, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunOK] != uint64(runs) {
		t.Fatalf("%d workers started, %v stopped and %d runs ended %v, want one worker to have answered them all", pool.Starts, pool.Stops, runs, pool.Runs)
	}
}

// A list that holds itself twice is not looked through level after level,
// each twice as long as the one before: it was, until the worker's memory
// limit ended that, a gigabyte and seven seconds later, and without a limit
// until the script's time or the machine's memory did. The script has its
// interpreter count the memory it holds (tracemalloc), and says in its next
// run how much that was at most: the worker's own memory is no measure, a
// process starting with the high-water mark of the one it was started from.
func TestAListThatHoldsItselfTwiceInAMetricIsNotGoneThroughLevelByLevel(t *testing.T) {
	requirePython(t)
	c := workerCollector("twice", `
import tracemalloc
if not tracemalloc.is_tracing(): tracemalloc.start()
if response.text == "peak": metric("peak_bytes", value=tracemalloc.get_traced_memory()[1])
else:
    junk = []
    junk.append(junk)
    junk.append(junk)
    metrics.append({"name": "m", "value": 1, "labels": {"l": junk}, "mine": junk})
`)
	c.Limits.MaxScriptMemory = 1 << 30
	_, err := runFlat(c, "twice")
	if want := `python transform: metric "m" label "l" is an array of 2 values, not a single value; select one, or join them with join(",")`; err == nil || err.Error() != want {
		t.Fatalf("a label that is a list holding itself twice: %.400v, want %q", err, want)
	}
	set, err := runFlat(c, "peak")
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value <= 0 || set.Metrics[0].Value > 64<<20 {
		t.Fatalf("the most memory the worker's interpreter held: %v, %v, want no more than 64 MiB", set, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered both", pool.Starts, pool.Stops)
	}
}

// Under a key no metric has, a script's own, the exporter reads nothing: a
// list there leaves the metric what it is, nested twice as nested ten
// thousand times and when it holds itself, where one nested about a
// thousand deep failed the scrape with a RecursionError. A metric that
// holds itself under such a key is the metric too.
func TestWhatAScriptKeepsUnderAKeyOfItsOwnIsNotReadHoweverDeep(t *testing.T) {
	requirePython(t)
	c := flatCollector()
	depths := []int{2, 480, 990, 1100, 10001, 30000}
	if raceDetector {
		depths = []int{2, 1100, 10001}
	}
	for _, kind := range flatKinds {
		for _, depth := range depths {
			set, err := runFlat(c, "junk", strconv.Itoa(depth), kind.shape)
			if err != nil || len(set.Metrics) != 2 || set.Metrics[1].Name != "m" || set.Metrics[1].Value != 1 || len(set.Metrics[1].Labels) != 0 {
				t.Fatalf("a key of the script's own holding %s nested %d deep: %v, %.300v, want the two metrics", kind.shape, depth, set, err)
			}
		}
	}
	itself := workerCollector("flat", `metric("a", value=1, labels={"k": "v"}); metrics[0]["self"] = metrics[0]`)
	set, err := runWorkerScript(t, itself)
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Name != "a" || set.Metrics[0].Labels["k"] != "v" {
		t.Fatalf("a metric that holds itself under a key of the script's own: %v, %v, want the metric", set, err)
	}
	// What the answer holds beside it is written as it is in any answer: a
	// NaN and an infinity as the floats they are, a tuple as a list.
	beside := workerCollector("flat", `
junk = 1
for _ in range(5000): junk = [junk]
metric("nan", value=float("nan"), labels={"l": float("inf")})
metrics.append({"name": "m", "value": float("-inf"), "labels": {"k": 1.5}, "mine": (junk, float("nan"))})`)
	set, err = runWorkerScript(t, beside)
	if err != nil || len(set.Metrics) != 2 || !math.IsNaN(set.Metrics[0].Value) || set.Metrics[0].Labels["l"] != "+Inf" || !math.IsInf(set.Metrics[1].Value, -1) || set.Metrics[1].Labels["k"] != "1.5" {
		t.Fatalf("a NaN and the infinities beside a list nested deep under a key of the script's own: %v, %v", set, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 3 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want one for each script and none stopped", pool.Starts, pool.Stops)
	}
}

// A list or a dict given to metric(...) where it takes one value fails the
// script naming the metric, the argument, and what was given by its kind
// and how many items it has — the same error for one nested twice and one
// nested deeper than the interpreter writes a value out, where the error
// ended in a RecursionError of its own, and no longer the whole of it
// written out, which for a list of a thousand items was an error of a
// thousand items. The traceback is of the script's line, as it was. A label
// that is one fails as it did, at every depth.
func TestAListOrDictGivenToMetricIsNamedTheSameHoweverDeep(t *testing.T) {
	requirePython(t)
	c := flatCollector()
	depths := []int{2, 990, 1100, 10001, 30000}
	if raceDetector {
		depths = []int{2, 10001}
	}
	runs := 0
	for _, kind := range flatKinds {
		labels := "ValueError: metric 'm' labels must be a mapping of label names to values, not a list"
		if kind.python == "dict" {
			labels = `ValueError: label 'a' is a dict, not a single value; pass one value, or join them with ",".join(...)`
		}
		for place, want := range map[string]string{
			"name":      "ValueError: metric name " + kind.worker + " is not a string",
			"type":      "ValueError: metric 'm' type " + kind.worker + ` is not a string; give "gauge", "counter" or "untyped"`,
			"help":      "ValueError: metric 'm' help " + kind.worker + " is not a string",
			"value":     "ValueError: metric 'm' value " + kind.worker + " is not a number",
			"timestamp": "ValueError: metric 'm' timestamp " + kind.worker + " is not a number",
			"labels":    labels,
			"label":     "ValueError: label 'l' is a " + kind.python + `, not a single value; pass one value, or join them with ",".join(...)`,
		} {
			// The one frame of the traceback is the script's line; an
			// interpreter may mark the call in it on a line of its own.
			const traceback = "python transform failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 22, in <module>\n    if argument: metric(**made)\n"
			for _, depth := range depths {
				if strings.HasPrefix(kind.shape, "itself") && depth != depths[0] {
					continue
				}
				_, err := runFlat(c, "metric() "+place, strconv.Itoa(depth), kind.shape)
				runs++
				if err == nil || !strings.HasPrefix(err.Error(), traceback) || !strings.HasSuffix(err.Error(), "\n"+want) || strings.Count(err.Error(), "\n") > 4 || !errors.Is(err, model.ErrScriptFailed) {
					t.Fatalf("metric(...) given %s nested %d deep as its %s: %.500v, want the script's failure %q after the traceback of its one line", kind.shape, depth, place, err, want)
				}
			}
		}
	}
	// A value of another kind that holds its own kind deeper than the
	// interpreter writes one out — a deque, or a frozenset as a label, which
	// is written as it writes itself — is named by its kind where it was to
	// be written: no RecursionError says of it what the script got wrong.
	// (An interpreter that writes it out names it whole, as it names one
	// nested a little.)
	for _, other := range []struct{ shape, place, want string }{
		{"deques", "help", "ValueError: metric 'm' help a deque nested too deep to be written is not a string"},
		{"deques", "value", "ValueError: metric 'm' value a deque nested too deep to be written is not a number"},
		{"deques", "label", "ValueError: label 'l' is a deque nested too deep to be written, not a single value; pass one value"},
		{"frozensets", "label", "ValueError: label 'l' is a frozenset nested too deep to be written, not a single value; pass one value"},
		{"frozensets", "help", "ValueError: metric 'm' help a frozenset of 1 item is not a string"},
	} {
		_, err := runFlat(c, "metric() "+other.place, "30000", other.shape)
		runs++
		if err != nil && strings.Contains(err.Error(), "RecursionError") || err != nil && !strings.HasSuffix(err.Error(), "\n"+other.want) && !strings.Contains(err.Error(), "deque([") && !strings.Contains(err.Error(), "frozenset({") {
			t.Fatalf("metric(...) given %s nested 30000 deep as its %s: %.500v, want %q", other.shape, other.place, err, other.want)
		}
	}
	pool := PythonWorkers().PoolSnapshot()
	if pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunOK]+pool.Runs[pythonRunScriptError] != uint64(runs) {
		t.Fatalf("%d workers started, %v stopped and %d runs ended %v, want one worker to have answered them all", pool.Starts, pool.Stops, runs, pool.Runs)
	}
}

// flatTable is a script that appends one entry to metrics: the value its
// table has at the index the response's second line gives, at the place
// its first line names. Asked to on the third line, it appends the entry as
// the worker writes one it could not follow, with None for the items of a
// list or a dict that stands where a metric has one value (flat).
const flatTable = `
import __main__, collections
place, index, how = response.text.split("\n")
nan = float("nan")
deep = 1
for i in range(60):
    deep = [deep, "beside"] if i % 2 else {"k": deep, "n": nan}
values = [
    [], [1], [1, 2, 3], {}, {"a": 1}, {"a": {"b": [1]}, "c": 2}, (), (1, 2), ((1,), [2], {"k": (3,)}), [[1], [2]], {1: 2, "1": 3, None: 4, True: 5, 1.5: 6}, [nan], {"n": nan}, [None, "x", 1.5, True], deep, [deep], {"deep": deep},
    collections.OrderedDict([("z", [1]), ("a", 2)]), collections.defaultdict(list, b=[nan]), collections.namedtuple("Pair", "a b")(1, [2]),
    5, 0, -1.5, "x", "", " 12 ", None, True, False, nan, float("inf"), 10**30, 17.9,
]
value = values[int(index)]
made = {"name": "m", "value": 1, "labels": {"kept": "v"}}
if place == "entry": made = value
elif place == "label": made["labels"] = {"l": value, "other": "kept", "none": None, "number": 5}
elif place == "nameless help": made = {"value": 1, "help": value}
elif place == "valueless label": made = {"name": "m", "labels": {"l": value}}
else: made[place] = value
metric("values", value=len(values))
metrics.append(made)
if how == "cut": metrics[:] = __main__.flat(metrics)
`

// What the worker writes in place of a list or a dict it could not follow
// is read by the exporter as the list or the dict itself is: for every kind
// of value at every place of an entry, the entry as it is — whose answer is
// the answer it was before the worker cut anything, byte for byte, and is
// read as it was — and the entry cut give the same series or the same
// failure. So an entry is refused in the same words, or read the same way,
// whether the worker could write all of it or not.
func TestAnEntryCutForItsDepthIsReadAsTheEntryItself(t *testing.T) {
	h := newHandover(t)
	text := func(lines ...string) handoverInput {
		body := strings.Join(lines, "\n")
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
		return handoverInput{name: body, decoded: &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, response: r}
	}
	c := &model.Collector{Name: "handover"}
	read := func(line []byte) (*model.MetricSet, error) {
		out, err := pythonResult(c, "transform", oracleTimeout, line, nil)
		if err != nil {
			return nil, err
		}
		return pythonSeries(withSeriesBudget(context.Background(), 0), out)
	}
	// The script says how many values its table has.
	counted, err := read(h.run("metrics", flatTable, text("junk", "0", "whole"), "", 1<<24, oracleTimeout))
	if err != nil || len(counted.Metrics) != 2 || counted.Metrics[0].Value < 30 {
		t.Fatalf("%v, %v, want the script to say its table has thirty values and more", counted, err)
	}
	values := int(counted.Metrics[0].Value)
	step := 1
	if raceDetector {
		step = 3
	}
	refused, taken := 0, 0
	for _, place := range []string{"entry", "name", "type", "help", "value", "timestamp", "labels", "label", "junk", "nameless help", "valueless label"} {
		for index := 0; index < values; index += step {
			whole := h.run("metrics", flatTable, text(place, strconv.Itoa(index), "whole"), "", 1<<24, oracleTimeout)
			entry := text(place, strconv.Itoa(index), "cut")
			request, err := pythonRequest("metrics", flatTable, entry.decoded, entry.response, c)
			if err != nil {
				t.Fatal(err)
			}
			cut, _, err := h.now.run(context.Background(), pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: 1 << 24}, request, oracleTimeout)
			if err != nil {
				t.Fatal(err)
			}
			wholeSet, wholeErr := read(whole)
			cutSet, cutErr := read(cut)
			if (wholeErr == nil) != (cutErr == nil) || wholeErr != nil && (wholeErr.Error() != cutErr.Error() || errors.Is(wholeErr, model.ErrScriptFailed) != errors.Is(cutErr, model.ErrScriptFailed)) {
				t.Fatalf("value %d as the %s: cut, the entry fails with %v, and whole with %v\ncut    %.300s\nwhole  %.300s", index, place, cutErr, wholeErr, cut, whole)
			}
			if diff := sameSeries(wholeSet, cutSet); diff != "" {
				t.Fatalf("value %d as the %s: cut, the entry is read otherwise than whole: %s\ncut    %.300s\nwhole  %.300s", index, place, diff, cut, whole)
			}
			if wholeErr != nil {
				refused++
			} else {
				taken++
			}
		}
	}
	if refused < 30/step*5 || taken < 30/step*2 {
		t.Fatalf("%d entries were refused and %d taken: the table should hold many of each", refused, taken)
	}
}

// What metric(...) says of an argument that is no list, dict, tuple or set
// is what it said, byte for byte, the worker as it was being the oracle
// (pythonoracle_test.go): a number, a text, None, bytes, a class, a value
// of a type of the script's own. And of one that is, it says what it said
// with the value named by its kind where it was written out, and nothing
// else changed.
func TestMetricNamesAnArgumentAsItDidUnlessItIsAListOrDict(t *testing.T) {
	h := newHandover(t)
	input := handoverInputs(t)[1]
	arguments := []string{"name", "type", "help", "value", "timestamp"}
	script := func(argument, literal string) string {
		return "given = " + literal + "\nmade = {\"name\": \"m\", \"value\": 1}\nmade[\"" + argument + "\"] = given\nmetric(**made)\n"
	}
	for _, literal := range []string{
		`5`, `-1.5`, `None`, `True`, `"text"`, `""`, `" 12 "`, `"caf" + chr(233)`, `"x" * 300`, `b"bytes"`, `object`, `float("nan")`, `float("inf")`, `10**30`, `1e19`, `range(3)`, `__import__("decimal").Decimal("1.5")`,
		`type("Odd", (), {"__repr__": lambda self: "odd"})()`, `type("Text", (str,), {})("sub")`, `type("Count", (int,), {"__repr__": lambda self: "count"})(3)`, `bytearray(b"ab")`,
	} {
		for _, argument := range arguments {
			h.run("metrics", script(argument, literal), input, "", 1<<24, oracleTimeout)
		}
	}
	c := &model.Collector{Name: "handover"}
	spec := pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: 1 << 24}
	for _, given := range []struct{ literal, written, named string }{
		{`[]`, `[]`, "a list of 0 items"}, {`[1]`, `[1]`, "a list of 1 item"}, {`[1, 2, 3]`, `[1, 2, 3]`, "a list of 3 items"}, {`[[1], {"k": (2,)}]`, `[[1], {'k': (2,)}]`, "a list of 2 items"},
		{`{}`, `{}`, "a dict of 0 items"}, {`{"a": 1}`, `{'a': 1}`, "a dict of 1 item"}, {`{"a": [1], 2: None}`, `{'a': [1], 2: None}`, "a dict of 2 items"},
		{`()`, `()`, "a tuple of 0 items"}, {`(1, 2)`, `(1, 2)`, "a tuple of 2 items"}, {`{1, 2}`, `{1, 2}`, "a set of 2 items"}, {`frozenset({1})`, `frozenset({1})`, "a frozenset of 1 item"},
		{`type("Bag", (dict,), {})(a=1)`, `{'a': 1}`, "a Bag of 1 item"}, {`type("Row", (list,), {})([1, 2])`, `[1, 2]`, "a Row of 2 items"},
	} {
		for _, argument := range arguments {
			request, err := pythonRequest("metrics", script(argument, given.literal), input.decoded, input.response, c)
			if err != nil {
				t.Fatal(err)
			}
			was, _, wasErr := h.old.run(context.Background(), spec, request, oracleTimeout)
			line, _, err := h.now.run(context.Background(), spec, request, oracleTimeout)
			if wasErr != nil || err != nil {
				t.Fatalf("%s as the %s: the run ended with %v, and with %v as it was", given.literal, argument, err, wasErr)
			}
			// As json writes the error: the two differ in no character it
			// escapes.
			want := strings.Replace(string(was), " "+given.written+" is not a ", " "+given.named+" is not a ", 1)
			if want == string(was) || string(line) != want {
				t.Fatalf("%s as the %s is answered\n     %s\nwas  %s\nwant what it was with %q for %q", given.literal, argument, line, was, given.named, given.written)
			}
		}
	}
	if old, now := h.old.PoolSnapshot(), h.now.PoolSnapshot(); old.Starts != 1 || now.Starts != 1 || len(now.Stops) != 0 {
		t.Fatalf("%d workers started as they were and %d as they are, %v stopped, want 1 of each and none stopped", old.Starts, now.Starts, now.Stops)
	}
}
