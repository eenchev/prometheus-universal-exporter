package transform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A script can leave one long string in an answer many times as a key: of
// every row of a list, of the cells of every row, as the name of a label of
// every metric. Each dict is there once and the string is no value, so
// nothing of what pythonshared_test.go and pythonstrings_test.go are about
// holds it back, and it is written each time: ten thousand characters as
// the key of two thousand rows are twenty megabytes of JSON from half a
// megabyte of memory, made whole in the worker's memory for the exporter
// to read up to limits.max_output_bytes, refuse and stop the worker over.
// To add up the keys of every dict cost an ordinary answer half again of
// its look-through, so the worker looks at the keys of a few dicts, the
// fourth value of a level and every sixty-first after it, lets those stand
// for the values around them, and adds all the keys up only where they
// then say the answer is longer than the limit. These tests leave such
// keys in data and in metrics, and want each answer refused by the worker
// itself, unwritten, by a worker that then serves the next request; they
// leave answers at the limit, which end as they did, and answers whose
// long keys the look does not pass, which do too; they leave mappings and
// keys that are not what they say they are, which are written wherever
// they stand; they compare the worker with what it was over generated
// answers; and they hold what the look costs an ordinary answer, and one
// whose keys the dicts looked at overstate.

// longKeys is a pre-script that leaves in data what the response's first
// line names: of as many rows as its second line says, each keyed by one
// text of ten thousand characters, or of rows keyed by as many characters
// together. Its interpreter counts the memory it holds from the first run
// that asks for the "peak", and each run that asks says how much that was
// at most since the run that asked last: a worker that is never asked
// does not count, which costs it most of its time.
const longKeys = `
import collections, tracemalloc
shape, size = response.text.split("\n")
size = int(size)
text = "k" * 10000
class Row(dict): pass
def rows(length):
    # Rows keyed by texts of a thousand characters, the last by one of the
    # rest, which is the empty key where nothing is left: length together.
    return [{"x" * 1000: 0} for _ in range(length // 1000)] + [{"y" * (length % 1000): 0}]
def far_down(leaf):
    for _ in range(3000): leaf = [leaf]
    return leaf
if shape == "peak":
    data = tracemalloc.get_traced_memory()[1] if tracemalloc.is_tracing() else 1
    tracemalloc.stop()
    tracemalloc.start()
elif shape == "a text as the key of every row":
    data = [{text: i} for i in range(size)]
elif shape == "a text as the key of every row of a dict":
    data = {"row %d" % i: {text: i} for i in range(size)}
elif shape == "a text as a key of the cells of every row":
    data = {"rows": [{"id": i, "cells": {text: i, "n": 1}} for i in range(size)]}
elif shape == "a text as the key of every row, beside a NaN":
    data = [float("nan")] + [{text: i} for i in range(size)]
elif shape == "a text as the key of every row, far down":
    data = far_down([{text: i} for i in range(size)])
elif shape == "a text as the key of every row, each a dict of the script's own":
    data = [Row({text: i}) for i in range(size)]
elif shape == "a text as the key of every row, each an OrderedDict":
    data = [collections.OrderedDict({text: i}) for i in range(size)]
elif shape == "a text as a key beside a number and None in every row":
    data = [{1: i, text: i, None: i} for i in range(size)]
elif shape == "a text that is no ASCII as the key of every row":
    text = "é" * 10000
    data = [{text: i} for i in range(size)]
elif shape == "keys as long together":
    data = rows(size)
elif shape == "keys as long together, beside a NaN":
    data = [float("nan")] + rows(size)
elif shape == "keys as long together, far down":
    data = far_down(rows(size))
elif shape == "a text as the key of the first three rows":
    data = [{"x" * size + str(i): i} for i in range(3)] + [{"id": i} for i in range(500)]
elif shape == "a number as the key of every row":
    data = [{i: 10 ** 150} for i in range(size)]
elif shape == "short keys":
    data = [{"id": i, "name": "row"} for i in range(size)]
else:
    fail("no shape " + shape)
`

// longKeysCollector is a collector whose pre-script is longKeys, with the
// limits of the collector of the tests of long strings.
func longKeysCollector() *model.Collector {
	c := longStringsCollector()
	c.Name, c.Transform.PreScript = "keys", longKeys
	return c
}

// longKeysEnvelope is how long the keys of a pre-script's answer are
// around its data: ok, log and data, which are counted with the data's own
// where keys are counted.
const longKeysEnvelope = len("ok") + len("log") + len("data")

// Data whose rows are keyed by one text of ten thousand characters, two
// thousand rows that are twenty megabytes written (three hundred, and
// three megabytes, under the race detector), is refused for its
// length by the worker, naming limits.max_output_bytes as the refusal of
// strings that are too long does: the rows of a list and of a dict, the
// cells of every row, beside a value that is no JSON, at the end of three
// thousand levels, rows that are dicts of the script's own kind or
// OrderedDicts, a key that stands beside a number and None, and one that
// is no ASCII. Nothing of it is written out or made: the worker holds
// what its script built and, where it copies an answer that is no JSON
// before it writes it, as much again, where it held all the answer is
// written as, twice. One worker refuses them all, as its scripts'
// failures, and then answers; each cost the exporter a worker before,
// stopped for an answer over the limit, or failed for its memory.
func TestDataWhoseKeysAreLongerThanTheLimitIsRefusedUnwritten(t *testing.T) {
	requirePython(t)
	c := longKeysCollector()
	want := "python pre-script failed: " + fmt.Sprintf(tooLong, "data")
	sharedPeak(t, c)
	// most is what the worker may hold over it: the rows the script built,
	// half a megabyte of dicts, and the worker's look through them or its
	// copy of them beside that. Under the race detector, which runs every
	// test twice, the rows are fewer: three megabytes written, which the
	// worker that wrote them out held twice, where it may hold two or
	// three megabytes.
	size := alloctest.UnlessRaced(2000, 300)
	shapes := []struct {
		name string
		most int
	}{
		{"a text as the key of every row", 2 << 20},
		{"a text as the key of every row of a dict", 2 << 20},
		{"a text as a key of the cells of every row", 2 << 20},
		{"a text as the key of every row, beside a NaN", 2 << 20},
		{"a text as the key of every row, far down", 3 << 20},
		{"a text as the key of every row, each a dict of the script's own", 2 << 20},
		{"a text as the key of every row, each an OrderedDict", 3 << 20},
		{"a text as a key beside a number and None in every row", 2 << 20},
		{"a text that is no ASCII as the key of every row", 2 << 20},
	}
	for _, shape := range shapes {
		data, err := leaveStrings(t, c, shape.name, size)
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s, %d rows: %.100v, %.300v, want the script's failure %q", shape.name, size, data, err, want)
		}
		if peak := sharedPeak(t, c); peak > shape.most {
			t.Fatalf("%s, %d rows: the worker held %d bytes to refuse it, want no more than %d", shape.name, size, peak, shape.most)
		}
	}
	data, err := leaveStrings(t, c, "a text as the key of every row", 5)
	if items, ok := data.([]any); err != nil || !ok || len(items) != 5 {
		t.Fatalf("after the refusals: %.100v, %v, want the worker to answer", data, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunScriptError] != uint64(len(shapes)) {
		t.Fatalf("%d workers started, %v stopped and the runs ended %v, want one worker to have refused the %d as its scripts' failures", pool.Starts, pool.Stops, pool.Runs, len(shapes))
	}
}

// The worker refuses an answer for its keys only when they and its strings
// are longer together than limits.max_output_bytes, each key counted as
// long as it is and as often as it is written: that is less than the
// answer is written as, so no answer the exporter would take is refused.
// Rows whose keys are exactly as long as the limit, with the three keys
// of the answer around them, are not refused by the worker: the answer is
// longer, by its quotes, colons and commas, so it is written and the
// exporter refuses it, with the output limit's error and its worker, as it
// did; one character more and the worker refuses it itself, and stays. So
// it is in a plain list, beside a value that is no JSON, and three thousand
// levels down. Keys the worker's look does not pass, those of the first
// three rows of five hundred, are not counted however long (where the
// look passes is in the table of keysOracle), and keys that are numbers
// count nothing: those answers end as they did. And rows of short keys
// that are well within the limit are the rows they were.
func TestTheWorkerRefusesForItsKeysOnlyPastTheLimit(t *testing.T) {
	requirePython(t)
	c := longKeysCollector()
	refused := "python pre-script failed: " + fmt.Sprintf(tooLong, "data")
	const tooLarge = "python pre-script output exceeds limit"
	stopped := uint64(0)
	lost := func(shape string, size int) {
		t.Helper()
		_, err := leaveStrings(t, c, shape, size)
		if err == nil || err.Error() != tooLarge {
			t.Fatalf("%s %d: %.300v, want the output limit's error %q", shape, size, err, tooLarge)
		}
		stopped++
		if pool := PythonWorkers().PoolSnapshot(); pool.Stops[pythonStopOutputLimit] != stopped || pool.Runs[pythonRunOutputLimit] != stopped {
			t.Fatalf("%s %d: %v workers stopped and the runs ended %v, want the worker of the answer over the limit stopped for it", shape, size, pool.Stops, pool.Runs)
		}
	}
	atLimit := longStringsLimit - longKeysEnvelope
	for _, shape := range []string{"keys as long together", "keys as long together, beside a NaN", "keys as long together, far down"} {
		lost(shape, atLimit)
		starts := PythonWorkers().PoolSnapshot().Starts
		if _, err := leaveStrings(t, c, shape, atLimit+1); err == nil || err.Error() != refused || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s %d: %.300v, want the script's failure %q", shape, atLimit+1, err, refused)
		}
		// The worker that refused it answers the next request.
		if data, err := leaveStrings(t, c, shape, 2500); err != nil || data == nil {
			t.Fatalf("%s 2500: %.100v, %v, want the rows", shape, data, err)
		}
		if pool := PythonWorkers().PoolSnapshot(); pool.Starts != starts+1 || pool.Stops[pythonStopOutputLimit] != stopped {
			t.Fatalf("%s: %d workers started since the refusal and %v stopped, want the one that refused to have answered", shape, pool.Starts-starts, pool.Stops)
		}
	}
	// Three rows keyed by a hundred thousand characters each, before five
	// hundred of short keys: the look begins at the fourth value of a
	// level.
	lost("a text as the key of the first three rows", 100000)
	// Numbers as keys count nothing, as numbers do anywhere: 1,000 rows of
	// one, each with a number of 151 digits, are 160,000 bytes written,
	// which the exporter refuses.
	lost("a number as the key of every row", 1000)
	short := alloctest.UnlessRaced(2000, 200)
	data, err := leaveStrings(t, c, "short keys", short)
	if rows, ok := data.([]any); err != nil || !ok || len(rows) != short {
		t.Fatalf("%d rows of short keys: %.100v, %v, want the rows", short, data, err)
	}
}

// longKeyMetrics is a transform that emits what the response's first line
// names, of as many metrics as its second line says, each with a text of a
// thousand characters as a key.
const longKeyMetrics = `
shape, size = response.text.split("\n")
size = int(size)
text = "n" * 1000
def labelled(nan=False):
    for i in range(size): metric("m", value=float("nan") if nan and not i else i, labels={"id": str(i), text: "v"})
if shape == "a label name of every metric":
    labelled()
elif shape == "a label name of every metric, one of them NaN":
    labelled(True)
elif shape == "a label name of every metric, beside lists nested too deep":
    labelled()
    deep = [1]
    for _ in range(3000): deep = [deep]
    metrics[0]["deep"] = deep
elif shape == "a key of the script's own in every metric":
    for i in range(size): metric("m", value=i)
    for m in metrics: m[text] = 1
elif shape == "one dict of labels in every metric":
    labels = {text: "v"}
    for i in range(size): metrics.append({"name": "m", "value": i, "labels": labels})
else:
    fail("no shape " + shape)
`

// The metrics of a transform are held to the same: a label whose name is a
// thousand characters on five thousand metrics, five megabytes written (a
// tenth of them under the race detector, four times the limit),
// fails the scrape as the script's, naming limits.max_output_bytes and
// metrics, and the worker stays: where metric() made them, where a value
// among them is no JSON, where a list nested too deep for the worker's
// copy stands beside them, where the text is a key of the script's own in
// every metric, and where every metric holds the one dict of labels. A few
// such metrics, within the limit, are the series they were.
func TestMetricsWhoseLabelNamesAreLongerThanTheLimitAreRefusedUnwritten(t *testing.T) {
	requirePython(t)
	c := workerCollector("keys", longKeyMetrics)
	c.Limits.MaxOutputBytes = longStringsLimit
	c.Limits.MaxScriptMemory = 256 << 20
	run := func(shape string, size int) (*model.MetricSet, error) {
		t.Helper()
		body := shape + "\n" + strconv.Itoa(size)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
		ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
		defer cancel()
		return executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
	}
	want := "python transform failed: " + fmt.Sprintf(tooLong, "metrics")
	shapes := []string{"a label name of every metric", "a label name of every metric, one of them NaN", "a label name of every metric, beside lists nested too deep", "a key of the script's own in every metric", "one dict of labels in every metric"}
	size := alloctest.UnlessRaced(5000, 500)
	for _, shape := range shapes {
		set, err := run(shape, size)
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s, %d of them: %v, %.300v, want the script's failure %q", shape, size, set, err, want)
		}
	}
	for _, shape := range shapes[:2] {
		set, err := run(shape, 50)
		if err != nil || len(set.Metrics) != 50 || set.Metrics[49].Value != 49 || len(set.Metrics[49].Labels) != 2 {
			t.Fatalf("%s, 50 of them: %v, %.300v, want the 50 series", shape, set, err)
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunScriptError] != uint64(len(shapes)) {
		t.Fatalf("%d workers started, %v stopped and the runs ended %v, want one worker to have refused the %d as its scripts' failures", pool.Starts, pool.Stops, pool.Runs, len(shapes))
	}
}

// notWhatTheySay is a pre-script that leaves in data, after as many values
// as the response's last line says, of the kind its second line names, one
// dict that the worker has always written and that is not what it says it
// is, of the kind its first line names. A mapping that is a dict to
// isinstance and to nothing else: a weakref.proxy of a dict of the
// script's own class, or an object that answers __class__ with the class of
// what it stands for, as wrapt's ObjectProxy, werkzeug's LocalProxy and
// django's SimpleLazyObject do; the worker's copy takes either for a dict
// and goes through its items(). Or a dict whose one key is a number that
// says its class is str, or one whose class cannot be asked: json writes
// either as the number.
const notWhatTheySay = `
import weakref
kind, before, lead = response.text.split("\n")
class Row(dict): pass
class Lazy:
    def __init__(self, held): self.held = held
    __class__ = property(lambda self: type(self.held))
    def items(self): return self.held.items()
class Posing(int):
    __class__ = str
class Unasked(int):
    @property
    def __class__(self): raise ValueError("this key's class is not to be asked")
row = Row(a=1)
last = {"a proxy of a dict": lambda: weakref.proxy(row), "a mapping that says its class is dict": lambda: Lazy({"a": 1}),
    "a dict keyed by a number that says its class is str": lambda: {Posing(5): 1}, "a dict keyed by a number whose class raises": lambda: {Unasked(5): 1}}[kind]()
each = {"lists": list, "dicts": dict, "numbers": int, "dicts, and a NaN after it": dict, "a list and numbers": int}[before]
data = [each() for _ in range(int(lead))] + [last]
if before == "a list and numbers" and int(lead): data[0] = []
if before == "dicts, and a NaN after it": data.append(float("nan"))
`

// An answer the worker wrote before it looked at keys is written wherever
// what it holds stands: a mapping that is a dict by what it says its class
// is, which the worker's copy has always gone through by its items(), and
// a dict keyed by a number that says its class is str, or that raises when
// its class is asked, which json has always written as the number. Each
// is left after no values and after up to sixty-five, lists, dicts or
// numbers, so that it is, in turn, every one of the first values of a
// level and of the first lists and dicts the worker's copy goes through,
// the one the look at keys begins with among them, and the one it comes to
// next, sixty-one on. The look runs nothing of the script's and asks no
// value what it is: it looks at the dicts the copy made, which are dicts
// whatever they are copies of, and takes a key for a string by its type.
// The worker that asked isinstance, and dict.keys of what answered, failed
// the mapping that was the fourth list or dict of an answer, and the
// sixty-fifth, with a TypeError, and the dict keyed by such a number where
// it stood fourth.
func TestWhatIsADictOrAStringOnlyByItsClassIsWrittenWhereverItStands(t *testing.T) {
	requirePython(t)
	c := longKeysCollector()
	c.Name, c.Transform.PreScript = "not what they say", notWhatTheySay
	kinds := []struct{ name, key string }{
		{"a proxy of a dict", "a"}, {"a mapping that says its class is dict", "a"},
		{"a dict keyed by a number that says its class is str", "5"}, {"a dict keyed by a number whose class raises", "5"},
	}
	// Under the race detector, the four that the look passes: the fourth
	// and the sixty-fifth of the lists and dicts of an answer, and of the
	// values of a level and of the dicts of a copy.
	leads := []int{1, 3, 62, 64, 0, 2, 4, 61, 63, 65}
	leads = leads[:alloctest.UnlessRaced(len(leads), 4)]
	for _, kind := range kinds {
		for _, before := range []string{"lists", "dicts", "numbers", "a list and numbers", "dicts, and a NaN after it"} {
			for _, lead := range leads {
				data, err := leaveStrings(t, c, kind.name+"\n"+before, lead)
				items, _ := data.([]any)
				want := lead + 1
				if before == "dicts, and a NaN after it" {
					want++
				}
				if err != nil || len(items) != want {
					t.Errorf("%s after %d %s: %.80v, %v, want the %d items", kind.name, lead, before, data, err, want)
					continue
				}
				if row, ok := items[lead].(map[string]any); !ok || len(row) != 1 || row[kind.key] == nil {
					t.Errorf("%s after %d %s: it is written as %v, want the one key %q", kind.name, lead, before, items[lead], kind.key)
				}
			}
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered", pool.Starts, pool.Stops)
	}
}

// madeWhenAsked is a pre-script that leaves one answer, whatever the
// response: seventeen mappings that make their rows when they are asked
// for them, dicts of the script's own class whose items() builds them, a
// seventh of the rows keyed by a text of four thousand characters, 560,000
// bytes written. Before that it makes as many small dicts as the response
// says and lets every second one go, as a script does that read a response
// of another size: what the interpreter's allocator then has free, and
// gives the rows made for the copy, differs with the count.
const madeWhenAsked = `
long = "k" * 4000
class Rows(dict):
    def items(self):
        return [(i, {(long if i % 7 == 3 else "id"): i}) for i in range(self["n"])]
junk = [{} for _ in range(int(response.text.split("\n")[1]))]
del junk[::2]
data = [Rows(n=n) for n in range(1, 120, 7)]
`

// The same answer ends the same way each time, whatever the interpreter's
// allocator holds, after fifty counts of dicts made and dropped (ten under
// the race detector): which dicts the worker looks at for their keys goes
// by where they stand in the answer. Rows that are made for the worker's copy
// and let go of as it goes on are given one another's place in memory, so
// a look that took the fourth and every sixty-first of the lists and dicts
// it had not met, by a set of where they lie, passed other rows after
// another count of dicts made and dropped before: the one answer was then
// refused by the worker, or written and refused by the exporter, which
// stopped the worker over it. It is refused by the worker every time now,
// by one worker.
func TestTheSameAnswerEndsTheSameWayWhateverTheAllocatorHolds(t *testing.T) {
	requirePython(t)
	c := longKeysCollector()
	c.Name, c.Transform.PreScript = "made when asked", madeWhenAsked
	want := "python pre-script failed: " + fmt.Sprintf(tooLong, "data")
	ends := map[string][]int{}
	for dropped := 0; dropped <= 400; dropped += alloctest.UnlessRaced(8, 40) {
		_, err := leaveStrings(t, c, "dicts made and dropped", dropped)
		end := fmt.Sprintf("%.90v", err)
		if err != nil && err.Error() == want {
			end = "the worker's refusal"
		}
		ends[end] = append(ends[end], dropped)
	}
	if len(ends) != 1 || len(ends["the worker's refusal"]) == 0 {
		for end, dropped := range ends {
			t.Errorf("the one answer ended as %q after this many dicts were made and dropped: %v; want the worker's refusal after each", end, dropped)
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have refused them all", pool.Starts, pool.Stops)
	}
}

// keysOfAnotherClass is a pre-script that leaves as many rows as the
// response's last line says, each keyed by one text of ten thousand
// characters: a str, a str of the script's own class, one of a class that
// says its length is one, or a member of an Enum of strings, in a plain
// list, beside a value that is no JSON, or three thousand levels down.
const keysOfAnotherClass = `
import enum
kind, where, size = response.text.split("\n")
class Text(str): pass
class Short(str):
    def __len__(self): return 1
class Name(str, enum.Enum):
    LONG = "n" * 10000
key = {"a str": "k" * 10000, "a str of the script's own class": Text("k" * 10000), "a str of a class that says it is one character": Short("k" * 10000), "a member of an Enum of strings": Name.LONG}[kind]
data = [{key: i} for i in range(int(size))]
if where == "beside a NaN": data.append(float("nan"))
if where == "far down":
    for _ in range(3000): data = [data]
`

// A key counts when it is a string, of the type str or of a class made of
// it, as long as it is in characters whatever its class says of its
// length, wherever the worker counts keys: where it looks through a plain
// answer, where it looks at the dicts of its copy, and where it writes an
// answer nested too deep for the copy, key by key. Two thousand rows keyed
// by ten thousand characters, twenty megabytes written (two hundred, and
// two megabytes, under the race detector), are refused by the worker in
// each of the three, the key being a str, a str of the script's
// own class, one of a class that says its length is one, or a member of
// an Enum of strings. What writes an answer nested too deep counted a key
// only when its class was exactly str, and wrote the twenty megabytes of
// the other three.
func TestKeysThatAreStringsOfAnotherClassCountWhereverTheWorkerCountsKeys(t *testing.T) {
	requirePython(t)
	c := longKeysCollector()
	c.Name, c.Transform.PreScript = "keys of another class", keysOfAnotherClass
	want := "python pre-script failed: " + fmt.Sprintf(tooLong, "data")
	size := alloctest.UnlessRaced(2000, 200)
	for _, kind := range []string{"a str", "a str of the script's own class", "a str of a class that says it is one character", "a member of an Enum of strings"} {
		for _, where := range []string{"in a list", "beside a NaN", "far down"} {
			if _, err := leaveStrings(t, c, kind+"\n"+where, size); err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
				t.Errorf("%d rows keyed by %s of 10,000 characters, %s: %.200v, want the script's failure %q", size, kind, where, err, want)
			}
			// Twelve such rows are within the limit, and are the rows they
			// were, each with its one key.
			data, err := leaveStrings(t, c, kind+"\n"+where, 12)
			for range 3000 {
				inner, ok := data.([]any)
				if where != "far down" || !ok || len(inner) != 1 {
					break
				}
				data = inner[0]
			}
			rows, _ := data.([]any)
			if err != nil || len(rows) < 12 {
				t.Errorf("12 rows keyed by %s of 10,000 characters, %s: %.80v, %v, want the rows", kind, where, data, err)
				continue
			}
			if row, ok := rows[11].(map[string]any); !ok || len(row) != 1 {
				t.Errorf("12 rows keyed by %s of 10,000 characters, %s: the last is %.80v, want a row of one key", kind, where, rows[11])
			}
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have refused and answered", pool.Starts, pool.Stops)
	}
}

// keysWas is the worker's writing of an answer as it was before it looked
// at keys, for a script that compares the worker with it: wire, plain,
// what writes an answer nested too deep and answer, word for word, with
// the worker's own weigh, flat and errors, which are as they were. An
// answer is written, or fails, by was_answer as it was and by now_answer
// as it is now, each as deep in calls as the other, so that both meet the
// recursion limit at one depth.
const keysWas = `
import __main__, collections, json, sys
most, deepest, weigh, flat, too_deep, too_long = __main__.most, __main__.deepest, __main__.weigh, __main__.flat, __main__.too_deep, __main__.too_long
class Unwritable(Exception): pass
whole=[None,None,0]
met=set()
def was_wire(v):
    if type(v) is str: whole[2]+=len(v); return v
    if isinstance(v,float) and (v!=v or v in (float('inf'),float('-inf'))):
        return '\x00pue-nonfinite:'+('NaN' if v!=v else '+Inf' if v>0 else '-Inf')+'\x00'
    if isinstance(v,(dict,list,tuple)):
        i=id(v)
        if i not in met: met.add(i)
        elif whole[1] is None:
            try: again()
            except RecursionError: pass
        if isinstance(v,dict): return {k:was_wire(x) for k,x in v.items()}
        return [was_wire(x) for x in v]
    return v
def sized():
    if whole[1] is None: whole[1]=weigh(whole[0])
    return whole[1]
def unwritable():
    n=sized()
    return n<0 or n>most
def again():
    if unwritable(): raise Unwritable
def was_plain_check():
    recursion_limit=sys.getrecursionlimit
    scalars=frozenset((int,bool,type(None)))
    def plain(document,levels=1<<30):
        level=[document]; room=half=most//2; look=4096; s=0
        for _ in range(min(levels,recursion_limit()//2-10)):
            below=[]
            extend=below.extend
            for v in level:
                t=type(v)
                if t is str: s+=len(v); continue
                if t is dict:
                    extend(v.values())
                    if len(below)>room:
                        if unwritable(): return None
                        room=look=1<<62
                elif t is float:
                    if v-v!=0: return False
                elif t is list or t is tuple:
                    extend(v)
                    if len(below)>room:
                        if unwritable(): return None
                        room=look=1<<62
                elif t not in scalars: return False
            if not below: whole[2]=s; return True
            room-=len(below)
            if half-room>look:
                look=(half-room)*4
                n=whole[1]=weigh(document,look>>8)
                if n is not None:
                    if n<0 or n>most: return None
                    look=1<<62
            level=below
        return False
    return plain
was_plain=was_plain_check()
def was_deep_check(deepest):
    quote=json.encoder.encode_basestring_ascii
    def key_text(k):
        if k.__class__ is str: return quote(k)
        return json.dumps({k:None},allow_nan=False)[1:-7]
    def dumps(document):
        text=[]; put=text.append; inside=[]; none=inside
        v=document
        while True:
            if isinstance(v,dict): put('{'); inside.append([iter(v.items()),'}',''])
            elif isinstance(v,(list,tuple)): put('['); inside.append([iter(v),']',''])
            else:
                put(json.dumps(was_wire(v),allow_nan=False))
                if whole[2]>most: raise too_long('metrics' if 'metrics' in document else 'data')
            if len(inside)>deepest+1: raise too_deep()
            while inside:
                at=inside[-1]
                v=next(at[0],none)
                if v is not none: break
                put(at[1]); inside.pop()
            else: return ''.join(text)
            put(at[2]); at[2]=', '
            if at[1]=='}': put(key_text(v[0])); put(': '); v=v[1]
    return dumps
was_deep_dumps=was_deep_check(deepest)
def was_answer(document):
    left='metrics' if 'metrics' in document else 'data' if 'data' in document else None
    text=None; follow=False; whole[0]=document; whole[1]=None; whole[2]=0
    try:
        follow=was_plain(document,5) if left=='metrics' else was_plain(document)
        if follow and (whole[2]<=most or not left): text=json.dumps(document,allow_nan=False)
    except Exception: text=None; follow=False
    if text is None and follow is False:
        whole[2]=0
        try:
            copy=was_wire(document)
            if whole[2]<=most or not left: text=json.dumps(copy,allow_nan=False)
        except Unwritable: whole[2]=0
        except RecursionError:
            if not left: raise
            whole[2]=0
        finally: met.clear()
    if text is None:
        if whole[2]>most: raise too_long(left)
        if left=='metrics':
            document=dict(document,metrics=flat(document['metrics']))
            if weigh(document)>most: raise too_long('metrics')
        else:
            n=sized()
            if n<0: raise too_deep()
            if n>most: raise too_long('data')
        text=was_deep_dumps(document)
    kept.write(text+'\n'); kept.flush()

class Kept:
    def write(self, text): self.text = text
    def flush(self): pass
kept = Kept()
def now_answer(document):
    answers = __main__.answers
    __main__.answers = kept
    try: __main__.answer(document)
    finally: __main__.answers = answers
def was_written(document):
    # As deep in calls as now_answer.
    was_answer(document)
def written(answer, document):
    kept.text = None
    try: answer(document)
    except BaseException as e: return "%s: %s" % (type(e).__name__, e)
    return kept.text
`

// keysOracle is, after keysWas, a transform that writes answers as the
// worker wrote them and as it writes them now, and says how each pair
// compares.
//
// The answers are made from the seed the response gives, of every kind of
// value a script leaves, as data and as metrics: plain and not, of types of
// the script's own, nested a little and up to where the worker's walks
// end, holding lists and dicts many times and in themselves, with keys
// that are empty, short, no ASCII, full of what JSON escapes, numbers,
// None, strings of a class of the script's own, one of which lies about
// its length, and texts of up to twenty-five hundred characters, in one
// dict and in many, so that many answers are within the limit of the
// test's collector, many over it by their keys and strings, and many over
// it only by what is around them.
//
// What must hold of each pair. An answer that was written is the same
// line now, byte for byte, or is refused with the error of a list held too
// often; and it is refused only when its keys and its strings, read back
// from the line itself, each as long as it is, are longer together than
// the limit, and, for data, only when the keys that are strings and the
// strings the script left are, counted here as often as they are written.
// That line was longer than the limit, so the exporter refused it and
// stopped its worker. An answer that failed fails with the same error;
// but where its keys and strings are over the limit it may fail with the
// refusal for them instead, which is counted apart (otherwise). The
// answers the table knows the end of end so: those the worker's look
// passes the long keys of are refused past the limit and the lines they
// were at it. An answer over the limit by its keys that is written as it
// was is one the look did not pass the keys of (missed).
const keysOracle = `
def weight(v):
    # How long the strings and the keys that are strings of what a script
    # left are together, each as often as it is written, or None for more
    # than a script of this test leaves.
    n, left, steps = 0, [v], 0
    while left:
        x = left.pop()
        steps += 1
        if steps > 100000: return None
        if type(x) is str: n += len(x)
        elif isinstance(x, dict):
            for k, y in x.items():
                if isinstance(k, str): n += str.__len__(k)
                left.append(y)
        elif isinstance(x, (list, tuple)): left.extend(x)
    return n
def weight_written(line):
    # The strings alone, and with them the keys, of an answer's line, read
    # back: the markers of the floats that are no JSON left out. A line
    # nested deeper than json reads one is read as the worker reads such a
    # request.
    try: read = json.loads(line)
    except RecursionError: read = __main__.deep_loads(line)
    strings, keys, left = 0, 0, [read]
    while left:
        x = left.pop()
        if type(x) is str:
            if not x.startswith('\x00pue-nonfinite:'): strings += len(x)
        elif type(x) is dict:
            keys += sum(map(len, x)); left.extend(x.values())
        elif type(x) is list: left.extend(x)
    return strings, strings + keys

seed = [int(response.text)]
first = seed[0] == 1
def pick(n):
    seed[0] = (seed[0] * 6364136223846793005 + 1442695040888963407) % (1 << 64)
    return (seed[0] >> 33) % n
class Rows(list): pass
class Bag(dict): pass
class Odd(float): pass
class Name(str): pass
class Liar(str):
    def __len__(self): return 1 << 40
Pair = collections.namedtuple("Pair", "a b")
texts = ["", "text", "café   \"q\" \\ \x00", "x" * 40, "y" * 300, "z" * 2500]
names = ["", "name", "n" * 40, "é" * 300, "q\"\\\n\x00" * 60, "K" * 700, "W" * 2500, Name("N" * 700), Liar("liar"), "😀" * 200]
finite = [0, 1, -7, 10**30, 1.5, -0.0, 5e-324, True, False, None] + texts
other = finite + [float("nan"), float("inf"), float("-inf"), Odd(2.5), Odd("nan"), 10**400]
def name():
    # A key of the table's, the long ones less often.
    return names[pick(len(names)) if pick(3) else pick(5)]
def key(i, odd):
    if odd and not pick(6): return [1, None, True, 1.5][pick(4)]
    if not pick(4): return name()
    return "key %d" % (i if pick(8) else 0)
def text():
    return texts[pick(len(texts)) if pick(4) else pick(4)]
def value(depth, odd, held):
    if odd and not pick(150): return [{1, 2}, b"bytes", 2j][pick(3)]
    if depth <= 0 or pick(10) < 4: return (other if odd else finite)[pick(len(other if odd else finite))] if pick(3) else text()
    if held is not None and held and not pick(6): return held[pick(len(held))]
    below = lambda: value(depth - 1, odd, held)
    n, kind = pick(5), pick(13 if odd else 7)
    if kind == 0: made = [below() for _ in range(n)]
    elif kind == 1: made = tuple(below() for _ in range(n))
    elif kind == 2: made = {key(i, odd): below() for i in range(n)}
    elif kind == 3: made = [text()] * pick(12)
    elif kind == 4: made = dict.fromkeys(["k%d" % i for i in range(pick(12))], text())
    elif kind == 5:
        # Rows keyed alike, by one key of the table's and one of their own.
        k = name()
        made = [{k: value(0, odd, held), "id": i} for i in range(pick(70))]
    elif kind == 6:
        k = name()
        made = {"r%d" % i: {k: i} for i in range(pick(70))}
    elif kind == 7: made = collections.OrderedDict((key(i, odd), below()) for i in range(n))
    elif kind == 8: made = Rows(below() for _ in range(n))
    elif kind == 9: made = Pair(below(), below())
    elif kind == 10: made = collections.defaultdict(list, {key(i, False): below() for i in range(n)})
    elif kind == 11:
        k = name()
        made = [Bag({k: value(0, False, held), 1: i}) for i in range(pick(70))]
    else: made = Bag((key(i, False), below()) for i in range(n))
    if held is not None: held.append(made)
    return made
def entry(odd, held):
    made = {"name": "m%d" % pick(5), "value": pick(100)}
    for _ in range(pick(4)):
        made[["labels", "help", "type", "timestamp", "value", "mine", "name"][pick(7)]] = value(3, odd, held)
    if not pick(3): made["labels"] = {key(i, odd): value(1 if pick(4) else 3, odd, held) for i in range(pick(4))}
    if not pick(4): made["help"] = text()
    if not pick(6): made[name()] = pick(9)
    return made if pick(8) else value(3, odd, held)
def entries(odd, held):
    if pick(4): return [entry(odd, held) for _ in range(pick(6))]
    # Many metrics of one label name, as metric() makes them.
    k, nan = name(), odd and not pick(3)
    return [{"name": "m", "type": "gauge", "value": float("nan") if nan and not i else float(i), "labels": {k: "v", "id": str(i)}, "help": "", "timestamp": None} for i in range(pick(120))]
def nested(depth, leaf):
    for _ in range(depth): leaf = [leaf]
    return leaf
def rows(n, width=500):
    # Rows keyed by texts of width characters, the last by one of the rest:
    # n together.
    return [{"x" * width: 0} for _ in range(n // width)] + [{"y" * (n % width): 0}]

# The answers the table knows the end of: True for one the worker must
# refuse now, False for one that must be as it was, None for one that may
# be either. They are the same for every seed, and are compared with the
# first.
K, short = "K" * 700, {"site": "a", "none": None}
documents = [("data", [{K: i} for i in range(200)], True), ("data", [{K: 1}] * 200, True), ("data", [[{K: 1}] * 3] * 70, True), ("data", {"r%d" % i: {K: i} for i in range(200)}, True),
    ("data", [{K: i} for i in range(3)] + [{"id": i} for i in range(100)], None), ("data", [{i: 0} for i in range(6000)], False), ("data", [{"": i} for i in range(3000)], False),
    ("data", [{"id": i, "name": "row"} for i in range(100)], False), ("data", [{"id": i, "name": "row"} for i in range(1000)], False), ("data", [{K: 1}] * 5000, None),
    ("metrics", [{"name": "m", "value": i, "labels": short} for i in range(40)], False), ("metrics", [{"name": "m", "value": i, "labels": short} for i in range(400)], False),
    ("metrics", [{"name": "m", "type": "gauge", "value": float(i), "labels": {K: "v", "id": str(i)}, "help": "", "timestamp": None} for i in range(100)], True),
    ("metrics", [{"name": "m", "type": "gauge", "value": float(i) if i else float("nan"), "labels": {K: "v", "id": str(i)}, "help": "", "timestamp": None} for i in range(100)], True),
    ("metrics", [{"name": "m", "value": i, "labels": {K: "v"}} for i in range(100)], True), ("metrics", [{"name": "m", "value": i, K: 1} for i in range(100)], True),
    ("metrics", [{"name": "m", "value": i, "labels": {K: "v"}} for i in range(5)], False)]
shared = {K: "v"}
documents += [("metrics", [{"name": "m", "value": i, "labels": shared} for i in range(100)], True)]
itself = {K: 1}
itself["itself"] = itself
documents += [("data", itself, None), ("data", [itself, 1], None)]
# Keys that are strings of a class of the script's own count as long as
# they are where the worker writes an answer nested too deep for its copy,
# whatever the class says of its length.
documents += [("data", nested(3000, [{Name(K): i} for i in range(200)]), True), ("data", nested(3000, [{Liar("liar"): i} for i in range(200)]), False)]
# Where the look passes, and where it does not. Of a level of a plain
# answer it passes the fourth value and every sixty-first after it, and of
# the dicts the copy of any other answer is made of, the fourth the copy
# finished and every sixty-first after that: one key twice as long as the
# limit in the fourth of a hundred rows, plain and beside a NaN, a key of
# seven hundred characters in the fourth of the sixty-one values of each
# of forty rows, and one dict of keys twice as long as the limit together
# that is the fourth value of its level, or the fourth dict of a copy, are
# refused. It does not pass the first three values of a level, the first
# three dicts of a copy, or a level of fewer than four values, and of rows
# of sixty-one values it passes the fourth value of each and no other, the
# sixty-first after it being the fourth of the next row: the same keys in
# the first three rows, in three rows that are all there are, in the first
# value of each of the forty rows, and in a dict that is the third value of
# its level or the second dict of a copy, are written as they were, longer
# than the limit. And what it passes stands for the values around it and no
# more: one dict of keys a fifth of the limit together is the one value
# looked at of the four of its level and is taken four times, or the one
# dict looked at of the six of a copy, where the keys of all six are then
# added up, and is the line it was.
far, few = "x" * (2 * most), [{"id": i} for i in range(100)]
def wide(length):
    return {"id %040d" % i: i for i in range(length // 43)}
def cells(at, i):
    row = [("c%d" % j, j) for j in range(60)]
    row.insert(at, ("cells", {K: i}))
    return dict(row)
documents += [("data", few[:3] + [{far: 3}] + few, True), ("data", [float("nan")] + few[:3] + [{far: 3}] + few, True),
    ("data", [{far + str(i): i} for i in range(3)] + few, False), ("data", [float("nan")] + [{far + str(i): i} for i in range(3)] + few, False), ("data", [{far + str(i): i} for i in range(3)], False),
    ("data", [cells(3, i) for i in range(40)], True), ("data", [cells(0, i) for i in range(40)], False),
    ("data", {"a": 1, "b": 2, "c": 3, "by id": wide(2 * most)}, True), ("data", {"a": 1, "b": 2, "by id": wide(2 * most), "c": 3}, False),
    ("data", {"a": float("nan"), "b": {}, "c": {}, "d": {}, "by id": wide(2 * most)}, True), ("data", {"a": float("nan"), "b": {}, "by id": wide(2 * most)}, False),
    ("data", {"a": 1, "b": 2, "c": 3, "by id": wide(most // 5)}, False), ("data", {"a": float("nan"), "b": {}, "c": {}, "d": {}, "by id": wide(most // 5)}, False)]
envelope = len("ok") + len("log") + len("data")
for n in (most - envelope - 1, most - envelope, most - envelope + 1):
    over = n + envelope > most
    documents += [("data", rows(n), over), ("data", [float("nan")] + rows(n), over), ("data", nested(3000, rows(n)), over), ("data", (rows(n),), over)]
# Rows that are every one keyed alike, so that the rows looked at say how
# long the keys of all are: with the answer's own three keys, which the
# look does not pass, they are one character over the limit of 16 KiB, or
# exactly at it.
documents += [("data", [{"u" * 184: i} for i in range(89)], 89 * 184 + envelope > most), ("data", [{"u" * 125: i} for i in range(131)], 131 * 125 + envelope > most)]
# Nested as deep as the worker's walks go, and a little less and more: the
# look through an answer level by level, half the recursion limit, and its
# copy, the recursion limit or half of it. The last of the three has
# seventy dicts and seventy more in them at its deepest, which the copy
# keeps and then looks at the keys of. The response says how far apart the
# depths are.
limit, apart = sys.getrecursionlimit(), int(response.headers["Depths"][0])
for depth in [300, 700, 1500] + list(range(limit // 2 - 18, limit // 2 + 2, apart)) + list(range(limit - 14, limit - 2, apart)):
    documents += [("data", nested(depth, rows(3 * most, 100)), True), ("data", nested(depth, [{"a": 1}, "leaf"]), False), ("data", nested(depth, [float("nan")] + [{"a": {}} for _ in range(70)]), False)]
if not first: documents = []
for i in range(int(response.headers["Generated"][0])):
    odd, held = i % 2 == 1, [] if i % 3 else None
    if i % 4 < 2: documents.append(("data", value(6, odd, held), None))
    else: documents.append(("metrics", entries(odd, held), None))

compared = same = long = refused = failed = otherwise = missed = 0
differ = []
for what, left, must in documents:
    document = {"ok": True, "log": "", what: left}
    held = weight(document)
    was, now = written(was_written, document), written(now_answer, document)
    compared += 1
    refusal = "OverflowError: %s" % too_long(what)
    if must is not None and (now == refusal) != must: differ.append("%s %.200r is answered %.300r, and should %s" % (what, left, now, "be refused" if must else "not be"))
    elif was.endswith("\n"):
        strings, both = weight_written(was)
        if now == was:
            if strings > most: differ.append("%s %.200r: its strings are longer than the limit and it is written" % (what, left))
            elif len(was) - 1 <= most: same += 1
            elif held is not None and held > most: missed += 1
            else: long += 1
        elif now != refusal: differ.append("%s %.200r is answered %.300r, and was %.300r" % (what, left, now, was))
        elif len(was) - 1 <= most or both <= most: differ.append("%s %.200r is refused, and its line was %d bytes, %d of them keys and strings" % (what, left, len(was) - 1, both))
        elif what == "data" and (held is None or held <= most): differ.append("%s %.200r is refused, and the keys and strings left in it are %r" % (what, left, held))
        else: refused += 1
    elif now == was: failed += 1
    elif now == refusal and (held is None or held > most): otherwise += 1
    else: differ.append("%s %.200r fails with %.300r, and failed with %.300r" % (what, left, now, was))
metric("compared", value=compared)
metric("same", value=same)
metric("long", value=long)
metric("refused", value=refused)
metric("failed", value=failed)
metric("otherwise", value=otherwise)
metric("missed", value=missed)
metric("differ", value=len(differ), labels={"first": differ[0] if differ else ""})
`

// An answer the exporter took before is the line it was, byte for byte; one
// that was written longer than the limit is refused by the worker now only
// when its keys and strings are longer than the limit, and is otherwise
// written as it was; and one that failed fails as it did: for hundreds of
// generated answers, with keys of every kind in them, in one dict and in
// many, for rows whose keys are at the limit, one character under and one
// over it, in a plain answer, in one that goes through the worker's copy
// and in one nested too deep for it, and for answers nested as deep as
// the worker's walks go. The answers are fewer under the race detector,
// which runs every test twice and does not slow the worker that makes and
// compares them: a sixth as many are generated, the floors of each kind
// being parts of how many there are, and of the thirty-two depths around
// where the worker's walks end every fourth is left.
func TestAnswersWithinTheLimitByTheirKeysAreTheLinesTheyWere(t *testing.T) {
	requirePython(t)
	c := workerCollector("keys oracle", keysWas+keysOracle)
	c.Limits.MaxOutputBytes = 1 << 14
	generated := alloctest.UnlessRaced(1200, 200)
	seeds := alloctest.UnlessRaced(2, 1)
	depths := strconv.Itoa(alloctest.UnlessRaced(1, 4))
	for seed := 1; seed <= seeds; seed++ {
		body := strconv.Itoa(seed)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Generated": {strconv.Itoa(generated)}, "Depths": {depths}}}
		ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
		set, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
		cancel()
		if err != nil || len(set.Metrics) != 8 {
			t.Fatalf("seed %d: %v, %.600v", seed, set, err)
		}
		count := func(i int) float64 { return set.Metrics[i].Value }
		compared, same, long, refused, failed, otherwise, missed, differ := count(0), count(1), count(2), count(3), count(4), count(5), count(6), set.Metrics[7]
		if differ.Value != 0 {
			t.Fatalf("seed %d: %v of %v answers are not what they should be, the first: %s", seed, differ.Value, compared, differ.Labels["first"])
		}
		t.Logf("seed %d: %v answers compared: %v the lines they were, %v written longer than the limit as they were, %v of those over it by their keys and strings, %v refused for their keys, %v failed as they did, %v failed for their keys instead", seed, compared, same, long+missed, missed, refused, failed, otherwise)
		floor := float64(generated)
		if compared < floor || same < floor/3 || long < floor/40 || refused < floor/20 || failed < floor/150 {
			t.Fatalf("seed %d: %v answers compared, %v the lines they were, %v longer than the limit as they were, %v refused for their keys and %v that failed: the table should hold many of each", seed, compared, same, long, refused, failed)
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered", pool.Starts, pool.Stops)
	}
}

// keysCost is, after keysWas, a transform that counts what the worker does
// to write an answer, as it was and as it is now: the calls it makes, of
// its own functions and of the interpreter's, which a clock does not
// measure the same twice and a count does; of those the calls of the
// function that adds up the keys of one dict it looks at (key_size); and
// the calls of the one that adds up the keys of all of them (keyed). The
// counts are of the worker's own answer(), with nothing of it copied here.
//
// The answers are of as many metrics as the response says, as metric()
// makes them, the same with a value that is no JSON, which the worker
// copies before it writes, and as many rows of seven short keys: ordinary
// answers. And answers within the limit in which what the look passes is
// not as the rest: one dict of five times as many keys as the fourth value
// of its level, a third of a megabyte of keys at most; one key of twenty
// thousand characters in the fourth of the rows, alone and beside a value
// that is no JSON; and a label named by twenty thousand characters on the
// first metric, whose labels are the fourth value of their level.
const keysCost = `
def calls(run, *arguments):
    n = [0, 0, 0]
    def profile(frame, event, argument):
        if event == "c_call": n[0] += 1
        elif event == "call":
            n[0] += 1
            if frame.f_code.co_name == "key_size": n[1] += 1
            elif frame.f_code.co_name == "keyed": n[2] += 1
    sys.setprofile(profile)
    try: run(*arguments)
    finally: sys.setprofile(None)
    return n
size = int(response.text)
long = "n" * 20000
def made(nan=False, name=None):
    for i in range(size): metric("requests_total", "counter", float("nan") if nan and not i else i, {"method": "GET", "code": "200", "site": "s%d" % (i % 7), "id": str(i)}, "Requests served.")
    entries = list(metrics)
    del metrics[:]
    if name: entries[0]["labels"][name] = "v"
    return entries
def rows(key=None):
    left = [{"key_%d" % j: (i if j % 2 else "v%d" % j) for j in range(7)} for i in range(size)]
    if key: left.insert(3, {key: 3})
    return left
cases = [("metrics", "metrics", made()), ("metrics, one NaN", "metrics", made(True)), ("rows", "data", rows()),
    ("one dict of many keys as the fourth value", "data", {"a": 1, "b": 2, "c": 3, "by id": {"id %06d" % i: i for i in range(5 * size)}}),
    ("rows, the fourth keyed by a long text", "data", rows(long)), ("rows, the fourth keyed by a long text, and a NaN", "data", rows(long) + [float("nan")]),
    ("metrics, a label of the first named by a long text", "metrics", made(name=long))]
for name, what, left in cases:
    document = {"ok": True, "log": "", what: left}
    was, now = written(was_written, document), written(now_answer, document)
    if was != now or not was.endswith("\n"): fail("%s: the answer is %.200r, and was %.200r" % (name, now, was))
    counted = calls(written, now_answer, document)
    metric(name, value=len(left), labels={"was": calls(written, was_written, document)[0], "now": counted[0], "looked": counted[1], "keyed": counted[2]})
`

// The look at keys costs an ordinary answer next to nothing: of two
// thousand metrics as metric() makes them and of two thousand rows of
// seven short keys, the worker makes no more than a twentieth more calls
// than it did, of its own functions and of the interpreter's, to look
// through the answer and write it; adding up the keys of every dict,
// which would refuse every answer that is over the limit by its keys, is
// a sixth more calls there, and was half again of the time of the look.
// Of the same metrics with a value that is no JSON, which the worker
// copies, it makes no more than a tenth more: one call for each dict the
// copy makes, to keep it. The keys of one dict are added up for every
// hundred metrics or rows at least, the look being there, and for every
// twenty at most, or every ten where the worker looked through the
// metrics before it met the value it copies them for; and those of all
// the answer's dicts are not added up at all.
//
// Nor does it cost an answer more where what the look passes is unlike
// the rest, which a count of calls for the answers above does not show:
// the worker that took each dict it looked at for sixty-one added up all
// the keys of an answer, in a second walk through it of four calls for
// every dict, whenever one dict of many keys or one long key stood where
// the look passes, and such an answer then cost up to three times its
// look, every scrape. One dict of ten thousand keys that is the fourth
// value of its level, ninety thousand characters of keys under a limit of
// a megabyte, stands for the four values of its level and no more: the
// keys of all dicts are not added up for it. One key of twenty thousand
// characters in the fourth of two thousand rows, or naming a label of the
// first of two thousand metrics, stands for sixty-one, which is more than
// the limit: the keys of all dicts are added up then, once, in the
// interpreter's own loops, for no more than a twentieth more calls than
// the worker made before it looked at keys, and a tenth where it copies
// the answer. The counts are of the worker's interpreter, which the race
// detector does not slow, and are the same for every run.
func TestTheLookAtKeysCostsAnAnswerWithinTheLimitNoMoreThanATwentieth(t *testing.T) {
	requirePython(t)
	c := workerCollector("keys cost", keysWas+keysCost)
	c.Limits.MaxOutputBytes = 1 << 20
	// A quarter as many under the race detector: the long text is then
	// taken as often as there are rows or metrics for each one looked at
	// just the same, sixty-one times, and the counts are parts of the size.
	size := alloctest.UnlessRaced(2000, 500)
	body := strconv.Itoa(size)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
	defer cancel()
	set, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
	// What each answer may cost over what it did, as a part of that; how
	// many dicts the look adds up the keys of, at least and at most; and
	// how often the keys of all dicts are added up.
	cases := map[string]struct{ part, least, most, keyed int }{
		"metrics":          {20, size / 100, size / 20, 0},
		"metrics, one NaN": {10, size / 100, size / 10, 0},
		"rows":             {20, size / 100, size / 20, 0},
		"one dict of many keys as the fourth value":          {20, 1, 1, 0},
		"rows, the fourth keyed by a long text":              {20, size / 100, size / 20, 1},
		"rows, the fourth keyed by a long text, and a NaN":   {10, size / 100, size / 20, 1},
		"metrics, a label of the first named by a long text": {20, size / 100, size / 20, 1},
	}
	if err != nil || len(set.Metrics) != len(cases) {
		t.Fatalf("%v, %.600v, want the counts of %d answers", set, err, len(cases))
	}
	for i := range set.Metrics {
		m := &set.Metrics[i]
		want, known := cases[m.Name]
		was, wasErr := strconv.Atoi(m.Labels["was"])
		now, nowErr := strconv.Atoi(m.Labels["now"])
		looked, lookedErr := strconv.Atoi(m.Labels["looked"])
		keyed, keyedErr := strconv.Atoi(m.Labels["keyed"])
		// A call for each metric or row at least, where the answer has as
		// many: a count that is less did not count the walk.
		if !known || wasErr != nil || nowErr != nil || lookedErr != nil || keyedErr != nil || (int(m.Value) >= size && was < size) {
			t.Fatalf("%s: the worker made %q calls, %q of them to add up the keys of a dict and %q those of all, and made %q, want counts of the walk", m.Name, m.Labels["now"], m.Labels["looked"], m.Labels["keyed"], m.Labels["was"])
		}
		t.Logf("%s: the worker makes %d calls, %d of them to add up the keys of a dict and %d those of all, and made %d: %+.1f%%", m.Name, now, looked, keyed, was, 100*float64(now-was)/float64(was))
		// Thirty-two calls are what an answer of any size costs: a level
		// kept, a dict looked at.
		if (now-was-32)*want.part > was {
			t.Errorf("%s: the worker makes %d calls, and made %d: more than one in %d more", m.Name, now, was, want.part)
		}
		if looked < want.least || looked > want.most {
			t.Errorf("%s: the worker adds up the keys of %d dicts it looks at, want %d at least and no more than %d", m.Name, looked, want.least, want.most)
		}
		if keyed != want.keyed {
			t.Errorf("%s: the worker adds up the keys of all the answer's dicts %d times, want %d", m.Name, keyed, want.keyed)
		}
	}
}
