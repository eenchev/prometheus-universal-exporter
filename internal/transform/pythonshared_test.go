package transform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A script can leave one list or dict in an answer many times over, or in
// itself. Python keeps it once and JSON writes it out each time it is met,
// so a list that holds another twice, which holds a third twice, forty of
// them, is forty lists to the script and a million million values written,
// and one that holds itself twice doubles with every level and never ends.
// The worker went through such data as it is written: until its memory
// limit, a gigabyte and many seconds later, or without one until the
// script's time or the machine's memory ended it. These tests leave such
// values in data and in metrics, and want each refused at once, in the
// words of what is wrong with it, by a worker that then serves the next
// request; and they leave values that only look like them, which are
// written as they always were.

// sharedShapes is a script that leaves in data what the response's first
// line names, built of as many levels, or copies, as its second line says.
// Its interpreter counts the memory it holds (tracemalloc), which a worker
// takes for an answer after its script has ended, so the run that asks for
// the "peak" says how much that was at most since the run that asked last.
const sharedShapes = `
import collections, tracemalloc
if not tracemalloc.is_tracing(): tracemalloc.start()
shape, levels = response.text.split("\n")
levels = int(levels)
def built(leaf, step):
    for _ in range(levels): leaf = step(leaf)
    return leaf
if shape == "peak":
    data = tracemalloc.get_traced_memory()[1]
    tracemalloc.stop()
    tracemalloc.start()
elif shape == "a list in itself":
    data = [1]; data.append(data)
elif shape == "a list in itself twice":
    data = []; data.append(data); data.append(data)
elif shape == "a dict in itself twice":
    data = {}; data["x"] = data; data["y"] = data
elif shape == "two lists in each other twice":
    data = []; other = [data, data]; data.append(other); data.append(other)
elif shape == "a list in the tuples it holds":
    data = []; data.append((data, data)); data.append((data,))
elif shape == "an OrderedDict in itself twice, beside a NaN":
    held = collections.OrderedDict(); held["x"] = held; held["y"] = held
    data = [float("nan"), held]
elif shape == "a list in itself twice, far down":
    inner = []
    data = built(inner, lambda v: [v])
    inner.append(data); inner.append(data)
elif shape == "a list in itself twice, in a large document":
    data = {"items": [{"id": i, "tags": ["a", "b"]} for i in range(levels)]}
    data["items"].append(data["items"]); data["items"].append(data["items"])
elif shape == "doubled":
    data = built([1], lambda v: [v, v])
elif shape == "doubled dicts":
    data = built({"leaf": 1}, lambda v: {"a": v, "b": v})
elif shape == "doubled tuples":
    data = built((1.5,), lambda v: (v, v))
elif shape == "doubled, beside a NaN":
    data = [float("nan"), built([1], lambda v: [v, v])]
elif shape == "doubled, far down":
    data = [built([1], lambda v: [v, v])]
    for _ in range(2000): data = [data]
elif shape == "tenfold":
    data = built(["leaf"], lambda v: [v] * 10)
elif shape == "one list many times":
    data = [["x" * 1000, 1]] * levels
elif shape == "one long key many times":
    data = [{"k" * 1000: 1}] * levels
elif shape == "nested":
    data = built(7, lambda v: [v])
elif shape == "a pair twice":
    pair = [1, 2]; data = [pair, pair]
elif shape == "a pair many times":
    data = [[1, 2]] * levels
elif shape == "a pair many times, beside a NaN":
    data = [[1, 2]] * levels + [float("nan")]
elif shape == "a pair twice, far down":
    pair = [1, 2]
    data = built([pair, pair], lambda v: [v])
elif shape == "a pair in every row":
    pair = ("a", "b")
    data = [{"id": i, "tags": pair, "none": ()} for i in range(levels)]
elif shape == "numbers":
    data = list(range(levels))
else:
    fail("no shape " + shape)
`

// sharedCollector is a collector whose pre-script is sharedShapes, with
// 128 KiB for an answer. Its memory limit is what ends a worker that goes
// through all a value is written as, so that such a run fails the test in
// seconds rather than filling the machine.
func sharedCollector() *model.Collector {
	return &model.Collector{
		Name: "shared", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{PreScript: sharedShapes},
		Limits: model.Limits{ScriptTimeout: model.Duration(2 * time.Second), MaxOutputBytes: sharedLimit, MaxScriptMemory: 256 << 20},
	}
}

// sharedLimit is the limits.max_output_bytes of these tests' collectors.
const sharedLimit = 1 << 17

// sharedTime is how long a run of these tests may take before its probe's
// deadline ends it: many times what any takes, and far shorter than the
// minute a test's script has, so that a worker that walks a value without
// end fails its test and does not hang it.
const sharedTime = 15 * time.Second

// leaveShared runs the collector's pre-script on a response that names a
// shape and its levels, and returns the data it left.
func leaveShared(t *testing.T, c *model.Collector, shape string, levels int) (any, error) {
	t.Helper()
	body := shape + "\n" + strconv.Itoa(levels)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
	defer cancel()
	return executePythonPreScript(ctx, "python3", c.Transform.PreScript, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
}

// sharedPeak is the most memory the worker's interpreter held since the
// run that asked last, in bytes.
func sharedPeak(t *testing.T, c *model.Collector) int {
	t.Helper()
	data, err := leaveShared(t, c, "peak", 0)
	peak, ok := data.(int)
	if err != nil || !ok || peak <= 0 {
		t.Fatalf("the most memory the worker's interpreter held: %v, %v", data, err)
	}
	return peak
}

// sharedSmall is the most memory a worker may hold over a value it refuses
// where its walk first comes back to a list or a dict: a megabyte, half of
// it the script's own, where going through what the value is written as
// took the worker's whole memory limit. sharedBounded is the most over one
// it refuses only once it has counted as many values as an answer may have
// bytes, sharedLimit, which costs a multiple of that.
const (
	sharedSmall   = 1 << 20
	sharedBounded = 64 * sharedLimit
)

const (
	holdsItself = "python pre-script failed: RecursionError: data is nested more than 10000 deep, or a list or a dict in it holds itself; the exporter reads what a script leaves in data nested 10000 deep at most, as deep as it decodes a response"
	tooLong     = "OverflowError: what the script left in %s is longer than limits.max_output_bytes (131072 bytes) written out, a list or a dict that is there more than once being written each time; leave less there, or raise limits.max_output_bytes"
)

// Data that holds itself is refused saying so, where the worker's walk
// comes back to a list or a dict it is inside of: once or twice, a list, a
// dict, a tuple or a dict of another type, far down or beside values that
// are no JSON. Holding itself twice costs what holding itself once does,
// where it took the worker's memory limit and seconds: the levels of such
// a value double, each twice the one before. One worker refuses them all
// and then answers.
func TestDataThatHoldsItselfIsRefusedWhereTheWalkComesBackToIt(t *testing.T) {
	requirePython(t)
	c := sharedCollector()
	sharedPeak(t, c)
	peaks := map[string]int{}
	for _, shape := range []struct {
		name   string
		levels int
	}{
		{"a list in itself twice", 0}, {"a list in itself", 0}, {"a dict in itself twice", 0}, {"two lists in each other twice", 0}, {"a list in the tuples it holds", 0},
		{"an OrderedDict in itself twice, beside a NaN", 0}, {"a list in itself twice, far down", 300}, {"a list in itself twice, far down", 3000},
	} {
		data, err := leaveShared(t, c, shape.name, shape.levels)
		if err == nil || err.Error() != holdsItself || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s: %v, %.300v, want the script's failure %q", shape.name, data, err, holdsItself)
		}
		// One nested 3000 deep is weighed in a list as long.
		if peak := sharedPeak(t, c); peak > sharedSmall*(1+shape.levels/1000) {
			t.Fatalf("%s: the worker held %d bytes to refuse it, want no more than %d", shape.name, peak, sharedSmall*(1+shape.levels/1000))
		} else {
			peaks[shape.name] = peak
		}
	}
	if once, twice := peaks["a list in itself"], peaks["a list in itself twice"]; twice > once+once/4 {
		t.Fatalf("a list in itself took the worker %d bytes to refuse and one in itself twice %d, want the same but for a quarter", once, twice)
	}
	// Around it, a document of 20,000 values: the walk has counted as many
	// values as an answer may have bytes before the whole is weighed, and
	// no more, so what it costs is a multiple of that limit. The document
	// is as large under the race detector: it is built, walked and refused
	// in the worker, which the detector does not slow, and all the exporter
	// hands over and reads of it is the shape's name and the refusal.
	if data, err := leaveShared(t, c, "a list in itself twice, in a large document", 5000); err == nil || err.Error() != holdsItself {
		t.Fatalf("a list in itself twice, in a large document: %v, %.300v, want %q", data, err, holdsItself)
	}
	if peak := sharedPeak(t, c); peak > sharedBounded {
		t.Fatalf("a list in itself twice, in a large document: the worker held %d bytes to refuse it, want no more than %d under an output limit of %d", peak, sharedBounded, sharedLimit)
	}
	if data, err := leaveShared(t, c, "a pair twice", 0); err != nil || fmt.Sprint(data) != "[[1 2] [1 2]]" {
		t.Fatalf("after the refusals: %v, %v, want the worker to answer", data, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered them all", pool.Starts, pool.Stops)
	}
}

// Data that holds one list twice, which holds another twice, and so on, is
// no data that holds itself: it has a JSON form, of a million values at
// twenty levels and a million million at forty. It is refused for its
// length, naming limits.max_output_bytes, without being written out or
// gone through: twenty, thirty and forty levels cost the worker the same,
// what its script costs, where they took its memory limit; and thousands
// of levels, or one list of a long text held eighty thousand times, cost a
// multiple of the output limit. One worker refuses them all and then
// answers.
func TestDataThatHoldsOneListManyTimesOverIsRefusedForItsLength(t *testing.T) {
	requirePython(t)
	c := sharedCollector()
	want := "python pre-script failed: " + fmt.Sprintf(tooLong, "data")
	sharedPeak(t, c)
	peaks := map[int]int{}
	// The last four are of the sizes they are under the race detector too.
	// Each is built and weighed in the worker, which the detector does not
	// slow, and all the exporter hands over and reads of one is the shape's
	// name and the refusal. Each is there to be refused only once the walk
	// has counted more values than half of sharedLimit and the whole is
	// weighed: lists doubled thousands of times are more lists than the
	// worker's looks at a part of the answer reach before that, and one
	// list or dict held 80,000 times is that many values at once. A quarter
	// as many, fewer with what they hold than that half, would be written
	// out for the exporter to refuse, and the worker stopped.
	shapes := []struct {
		name   string
		levels int
		most   int
	}{
		{"doubled", 20, sharedSmall}, {"doubled", 30, sharedSmall}, {"doubled", 40, sharedSmall}, {"doubled dicts", 40, sharedSmall}, {"doubled tuples", 40, sharedSmall},
		{"doubled, beside a NaN", 40, sharedSmall}, {"doubled, far down", 40, sharedSmall}, {"tenfold", 12, sharedSmall},
		{"doubled", 2000, sharedBounded}, {"one list many times", 80000, sharedBounded},
		{"doubled", 9000, sharedBounded}, {"one long key many times", 80000, sharedBounded},
	}
	for _, shape := range shapes {
		data, err := leaveShared(t, c, shape.name, shape.levels)
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s of %d levels: %v, %.300v, want the script's failure %q", shape.name, shape.levels, data, err, want)
		}
		peak := sharedPeak(t, c)
		if peak > shape.most {
			t.Fatalf("%s of %d levels: the worker held %d bytes to refuse it, want no more than %d", shape.name, shape.levels, peak, shape.most)
		}
		if shape.name == "doubled" {
			peaks[shape.levels] = peak
		}
	}
	for _, levels := range []int{30, 40} {
		if peaks[levels] > peaks[20]+peaks[20]/4 {
			t.Fatalf("lists doubled 20 times took the worker %d bytes to refuse and lists doubled %d times %d, want the same but for a quarter", peaks[20], levels, peaks[levels])
		}
	}
	if data, err := leaveShared(t, c, "a pair twice", 0); err != nil || fmt.Sprint(data) != "[[1 2] [1 2]]" {
		t.Fatalf("after the refusals: %v, %v, want the worker to answer", data, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunScriptError] != uint64(len(shapes)) {
		t.Fatalf("%d workers started, %v stopped and the runs ended %v, want one worker to have refused the %d as its scripts' failures", pool.Starts, pool.Stops, pool.Runs, len(shapes))
	}
}

// What only looks like such data is written as it was: a list that is in
// data twice is written twice, also five thousand times, beside a value
// that is no JSON, at the end of a thousand levels, and in every row of a
// document; lists nested ten thousand deep are carried; and lists doubled
// twelve times are the four thousand values they are. And data that holds
// nothing twice but is longer than limits.max_output_bytes ends as it did,
// with the output limit's error and its worker.
func TestDataThatHoldsAListMoreThanOnceWithinTheLimitIsWrittenAsItWas(t *testing.T) {
	requirePython(t)
	c := sharedCollector()
	list := func(v any) []any {
		t.Helper()
		items, ok := v.([]any)
		if !ok {
			t.Fatalf("%.200v is no list", v)
		}
		return items
	}
	data, err := leaveShared(t, c, "a pair twice", 0)
	if err != nil || fmt.Sprint(data) != "[[1 2] [1 2]]" {
		t.Fatalf("a pair that is in data twice: %v, %v, want it written twice", data, err)
	}
	for _, shape := range []string{"a pair many times", "a pair many times, beside a NaN"} {
		data, err = leaveShared(t, c, shape, 5000)
		if items := list(data); err != nil || len(items) < 5000 || fmt.Sprint(items[0]) != "[1 2]" || fmt.Sprint(items[4999]) != "[1 2]" {
			t.Fatalf("%s: %.100v, %v, want the pair written 5000 times", shape, data, err)
		}
	}
	data, err = leaveShared(t, c, "a pair twice, far down", 1000)
	if got := nestedDepth(data); err != nil || got != 1002 {
		t.Fatalf("a pair twice, a thousand levels down: read back nested %d deep, %.200v, want 1002", got, err)
	}
	data, err = leaveShared(t, c, "a pair in every row", 2000)
	if items := list(data); err != nil || len(items) != 2000 || fmt.Sprint(items[1999]) != "map[id:1999 none:[] tags:[a b]]" {
		t.Fatalf("a pair in every row of 2000: %.100v, %v", data, err)
	}
	deepest := decode.MaxDepth
	if raceDetector {
		deepest = 2000
	}
	data, err = leaveShared(t, c, "nested", deepest)
	if got := nestedDepth(data); err != nil || got != deepest {
		t.Fatalf("lists nested %d deep: read back nested %d deep, %.200v", deepest, got, err)
	}
	data, err = leaveShared(t, c, "doubled", 12)
	leaves := 0
	var count func(v any)
	count = func(v any) {
		if items, ok := v.([]any); ok {
			for _, item := range items {
				count(item)
			}
			return
		}
		leaves++
	}
	count(data)
	if err != nil || leaves != 1<<12 {
		t.Fatalf("lists doubled 12 times: %d values, %v, want %d", leaves, err, 1<<12)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered them all", pool.Starts, pool.Stops)
	}
	// 30,000 numbers are 200,000 bytes written, of nothing held twice.
	_, err = leaveShared(t, c, "numbers", 30000)
	if err == nil || err.Error() != "python pre-script output exceeds limit" {
		t.Fatalf("30000 numbers under an output limit of %d: %.300v, want the output limit's error", sharedLimit, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Stops[pythonStopOutputLimit] != 1 || pool.Runs[pythonRunOutputLimit] != 1 {
		t.Fatalf("%v workers stopped and the runs ended %v, want the worker of the answer over the limit stopped for it", pool.Stops, pool.Runs)
	}
}

// sharedMetrics is a transform that appends a metric with junk at the
// place the response's first line names: lists doubled as many times as
// its second line says, or one entry of a thousand keys as many times. It
// says how much memory its interpreter held at most, as sharedShapes does.
const sharedMetrics = `
import tracemalloc
if not tracemalloc.is_tracing(): tracemalloc.start()
place, levels = response.text.split("\n")
junk = [1]
for _ in range(int(levels)): junk = [junk, junk]
metric("before", value=1)
if place == "peak":
    metric("peak_bytes", value=tracemalloc.get_traced_memory()[1])
    tracemalloc.stop()
    tracemalloc.start()
elif place == "entry": metrics.append(junk)
elif place == "label": metrics.append({"name": "m", "value": 1, "labels": {"l": junk, "other": "kept"}})
elif place == "labels": metrics.append({"name": "m", "value": 1, "labels": junk})
elif place == "mine": metrics.append({"name": "m", "value": float("inf"), "mine": junk})
elif place == "one entry many times":
    wide = {"name": "wide", "value": 2}
    wide.update(("key%d" % i, "text") for i in range(1000))
    metrics.extend([wide] * int(levels))
else: metrics.append({"name": "m", "value": 1, place: junk})
`

// Lists doubled forty times where a metric has one value fail the scrape
// as three numbers there do, naming the metric and its part, and under a
// key of the script's own leave the metric what it is: the worker writes
// the metrics without them, as it does those it cannot follow for their
// depth, and no longer copies out the million million values they are
// written as, which took its memory limit. An entry that is in metrics so
// often that they are longer than limits.max_output_bytes is not written
// out either: the scrape fails as the script's, naming the limit, and the
// worker stays. A few times, it is the metric it is each time.
func TestListsDoubledInAMetricFailAsAShortListThere(t *testing.T) {
	requirePython(t)
	c := workerCollector("doubled", sharedMetrics)
	c.Limits.MaxOutputBytes = sharedLimit
	c.Limits.MaxScriptMemory = 64 << 20
	run := func(place string, levels int) (*model.MetricSet, error) {
		t.Helper()
		body := place + "\n" + strconv.Itoa(levels)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
		ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
		defer cancel()
		return executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
	}
	peak := func(what string) {
		t.Helper()
		set, err := run("peak", 0)
		if err != nil || len(set.Metrics) != 2 || set.Metrics[1].Value <= 0 || set.Metrics[1].Value > sharedSmall {
			t.Fatalf("%s: the most memory the worker's interpreter held: %v, %v, want no more than %d bytes", what, set, err, sharedSmall)
		}
	}
	peak("at first")
	for place, want := range map[string]string{
		"entry":     "metrics[1] is an array of 2 items, not a metric; call metric(...), or append a mapping with a name and a value",
		"label":     `metric "m" label "l" is an array of 2 values, not a single value; select one, or join them with join(",")`,
		"labels":    `metric "m" labels are an array of 2 items, not a mapping of label names to values`,
		"value":     `metric "m" value an array of 2 items is not a number`,
		"help":      `metric "m" help an array of 2 items is not a string`,
		"timestamp": `metric "m" timestamp an array of 2 items is not a number of milliseconds`,
	} {
		for _, levels := range []int{1, 40} {
			set, err := run(place, levels)
			if err == nil || err.Error() != "python transform: "+want || !errors.Is(err, model.ErrScriptFailed) {
				t.Fatalf("lists doubled %d times as the %s: %v, %.300v, want the script's failure %q", levels, place, set, err, "python transform: "+want)
			}
			peak("lists doubled as the " + place)
		}
	}
	set, err := run("mine", 40)
	if err != nil || len(set.Metrics) != 2 || set.Metrics[1].Name != "m" || !math.IsInf(set.Metrics[1].Value, 1) {
		t.Fatalf("lists doubled 40 times under a key of the script's own: %v, %.300v, want the two metrics", set, err)
	}
	peak("lists doubled under a key of the script's own")
	set, err = run("one entry many times", 5)
	if err != nil || len(set.Metrics) != 6 || set.Metrics[5].Name != "wide" || set.Metrics[5].Value != 2 {
		t.Fatalf("one entry five times: %v, %.300v, want it five times after the first metric", set, err)
	}
	set, err = run("one entry many times", 200)
	if want := "python transform failed: " + fmt.Sprintf(tooLong, "metrics"); err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
		t.Fatalf("one entry of a thousand keys 200 times: %v, %.300v, want the script's failure %q", set, err, want)
	}
	if set, err = run("help", 0); err == nil || !strings.Contains(err.Error(), `metric "m" help an array of 1 item is not a string`) {
		t.Fatalf("after them all: %v, %v, want the worker to answer", set, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered them all", pool.Starts, pool.Stops)
	}
}

// sharedOracle is a transform that writes answers as the worker wrote them
// before it looked for a list or a dict it had met, and as it writes them
// now, and says how many it compared and which differ. The answer as it
// was is here, as the oracle: plain, wire, flat and answer, word for word.
// The answers are made from the seed the response gives, of every kind of
// value a script leaves, as data and as metrics: plain and not, of types
// of the script's own, nested a little and a lot, holding nothing twice
// and holding lists and dicts many times within what an answer may be.
// None holds itself and none is longer than the limit, which is where the
// two are meant to differ. It also holds the worker's measure to what it
// is for: weigh is no more than data is long when it is written, and
// nothing where nothing is held twice.
const sharedOracle = `
import __main__, collections, json, sys
def was_plain(document,levels=1<<30):
    scalars=frozenset((int,bool,type(None)))
    level=[document]
    for _ in range(min(levels,sys.getrecursionlimit()//2-10)):
        below=[]
        extend=below.extend
        for v in level:
            t=type(v)
            if t is str: continue
            if t is dict: extend(v.values())
            elif t is float:
                if v-v!=0: return False
            elif t is list or t is tuple: extend(v)
            elif t not in scalars: return False
        if not below: return True
        level=below
    return False
def was_wire(v):
    if isinstance(v,float) and (v!=v or v in (float('inf'),float('-inf'))):
        return '\x00pue-nonfinite:'+('NaN' if v!=v else '+Inf' if v>0 else '-Inf')+'\x00'
    if isinstance(v,dict): return {k:was_wire(x) for k,x in v.items()}
    if isinstance(v,(list,tuple)): return [was_wire(x) for x in v]
    return v
def was_flat(metrics):
    def cut(v):
        if isinstance(v,dict): return dict.fromkeys(v)
        if isinstance(v,(list,tuple)): return [None]*len(v)
        return v
    def entry(m):
        if not isinstance(m,dict): return cut(m)
        return {k:{n:cut(x) for n,x in v.items()} if k=='labels' and isinstance(v,dict) else cut(v) for k,v in m.items()}
    return [entry(m) for m in metrics]
def was_answer(document):
    text=None
    try:
        if was_plain(document,5) if 'metrics' in document else was_plain(document): text=json.dumps(document,allow_nan=False)
    except Exception: text=None
    if text is None:
        try: text=json.dumps(was_wire(document),allow_nan=False)
        except RecursionError:
            if 'data' not in document and 'metrics' not in document: raise
    if text is None:
        if 'metrics' in document: document=dict(document,metrics=was_flat(document['metrics']))
        text=__main__.deep_dumps(document)
    return text+'\n'

class Kept:
    def write(self, text): self.text = text
    def flush(self): pass
def now_answer(document):
    kept, answers = Kept(), __main__.answers
    __main__.answers = kept
    try: __main__.answer(document)
    finally: __main__.answers = answers
    return kept.text
def written(answer, document):
    # As deep in calls as the other, so that both meet the recursion limit
    # at one depth.
    try: return answer(document)
    except BaseException as e: return "%s: %s" % (type(e).__name__, e)

seed = [int(response.text)]
def pick(n):
    seed[0] = (seed[0] * 6364136223846793005 + 1442695040888963407) % (1 << 64)
    return (seed[0] >> 33) % n
class Rows(list): pass
class Bag(dict): pass
class Odd(float): pass
Pair = collections.namedtuple("Pair", "a b")
finite = [0, 1, -7, 10**30, 2**63, 1.5, -0.0, 1e21, 5e-324, True, False, None, "", "text", "café   \"q\" \\ \x00", "x" * 40]
other = finite + [float("nan"), float("inf"), float("-inf"), Odd(2.5), Odd("nan"), 10**400]
def key(i, odd):
    return [1, None, True, 1.5][pick(4)] if odd and not pick(6) else "key %d" % (i if pick(8) else 0)
def value(depth, odd, held):
    # held is the lists and dicts made so far, of which one is taken again
    # now and then: so none holds itself.
    # Now and then what json refuses, which fails the answer.
    if odd and not pick(150): return [{1, 2}, b"bytes", 2j][pick(3)]
    if depth <= 0 or pick(10) < 4: return (other if odd else finite)[pick(len(other if odd else finite))]
    if held is not None and held and not pick(6): return held[pick(len(held))]
    below = lambda: value(depth - 1, odd, held)
    n, kind = pick(5), pick(9 if odd else 3)
    if kind == 0: made = [below() for _ in range(n)]
    elif kind == 1: made = tuple(below() for _ in range(n))
    elif kind == 2: made = {key(i, odd): below() for i in range(n)}
    elif kind == 3: made = collections.OrderedDict((key(i, odd), below()) for i in range(n))
    elif kind == 4: made = Rows(below() for _ in range(n))
    elif kind == 5: made = Pair(below(), below())
    elif kind == 6: made = collections.defaultdict(list, {key(i, False): below() for i in range(n)})
    elif kind == 7: made = Bag((key(i, False), below()) for i in range(n))
    else: made = [below() for _ in range(n)]
    if held is not None: held.append(made)
    return made
def values(v, limit, seen):
    # How many values v is written as, counted to limit, and whether a
    # list or a dict is in it twice.
    n, left = 0, [v]
    while left and n <= limit:
        x = left.pop()
        n += 1
        if isinstance(x, (dict, list, tuple)):
            if id(x) in seen: seen[None] = True
            seen[id(x)] = x
            left.extend(x.values() if isinstance(x, dict) else x)
    return n
def entry(odd, held):
    made = {"name": "m%d" % pick(5), "value": pick(100)}
    for _ in range(pick(4)):
        made[["labels", "help", "type", "timestamp", "value", "mine", "name"][pick(7)]] = value(3, odd, held)
    if not pick(3): made["labels"] = {"l%d" % i: value(1 if pick(4) else 3, odd, held) for i in range(pick(4))}
    return made if pick(8) else value(3, odd, held)
def nested(depth, leaf):
    for _ in range(depth): leaf = [leaf]
    return leaf

pair, labels, wide = [1, 2], {"site": "a", "none": None}, [[1.5, "leaf"]] * 3
documents = [("data", [pair, pair]), ("data", [pair] * 5000), ("data", [pair] * 5000 + [float("nan")]), ("data", [{"id": i, "tags": pair, "none": ()} for i in range(3000)]),
    ("data", [{"id": i, "v": i * 0.5, "tags": ["a", "b"]} for i in range(3000)]), ("data", {"wide": wide, "again": [wide, wide, (wide,)]}),
    ("metrics", [{"name": "m", "value": i, "labels": labels} for i in range(3000)]), ("metrics", [{"name": "m", "value": float("nan"), "labels": labels, "mine": [wide, wide]}] * 50)]
doubled = [1.5, "leaf"]
for _ in range(11): doubled = [doubled, doubled]
documents += [("data", doubled), ("data", [float("nan"), doubled]), ("metrics", [{"name": "m", "value": 1, "mine": doubled}])]
for depth in (300, 600, 1100, 3000):
    for leaf in (7, [pair, pair], [wide, float("inf"), wide]):
        documents += [("data", nested(depth, leaf)), ("metrics", [{"name": "m", "value": 1, "mine": nested(depth, leaf)}, {"name": "n", "value": 2, "labels": {"l": nested(depth, leaf)}}])]
for i in range(int(response.headers["Generated"][0])):
    odd, held = i % 2 == 1, [] if i % 3 else None
    if i % 4 < 2: documents.append(("data", value(6, odd, held)))
    else: documents.append(("metrics", [entry(odd, held) for _ in range(pick(6))]))

compared = twice = cut = failed = 0
differ = []
for what, left in documents:
    seen = {}
    if values(left, 20000, seen) > 20000: continue
    document = {"ok": True, "log": "", what: left}
    was, now = written(was_answer, document), written(now_answer, document)
    compared += 1
    twice += None in seen
    cut += what == "metrics" and ": null" in was and not was_plain(document, 5)
    failed += not was.endswith("\n")
    weight = __main__.weigh(document)
    if was != now: differ.append("%s %.200r is answered %.300r, and was %.300r" % (what, left, now, was))
    elif __main__.plain(document, 5 if what == "metrics" else 1 << 30) is not was_plain(document, 5 if what == "metrics" else 1 << 30): differ.append("%s %.200r: plain says otherwise than it did" % (what, left))
    elif what == "data" and was.endswith("\n") and weight > len(was) - 1: differ.append("%s %.200r is %d long and weighs %d" % (what, left, len(was) - 1, weight))
    elif (weight != 0) != (None in seen): differ.append("%s %.200r weighs %d" % (what, left, weight))
    elif what == "metrics" and repr(__main__.flat(left)) != repr(was_flat(left)): differ.append("%.200r is cut otherwise than it was" % (left,))
metric("compared", value=compared)
metric("held twice", value=twice)
metric("cut", value=cut)
metric("failed", value=failed)
metric("differ", value=len(differ), labels={"first": differ[0] if differ else ""})
`

// An answer that holds nothing in itself, and no list or dict so often
// that it is longer than the limit, is the line it was, byte for byte, or
// fails as it failed: for hundreds of generated answers and for those a
// worker's walks could take for the others — a list held five thousand
// times, in every row of a document, at the end of three thousand levels,
// under a metric's label — the worker's answer is the oracle's, plain says
// of it what it said, and what is cut of a metric is what was cut.
func TestAnswersThatHoldNothingTooOftenAreTheLinesTheyWere(t *testing.T) {
	requirePython(t)
	c := workerCollector("oracle", sharedOracle)
	generated, seeds := 400, 2
	if raceDetector {
		generated, seeds = 150, 1
	}
	for seed := 1; seed <= seeds; seed++ {
		body := strconv.Itoa(seed)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Generated": {strconv.Itoa(generated)}}}
		ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
		set, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
		cancel()
		if err != nil || len(set.Metrics) != 5 {
			t.Fatalf("seed %d: %v, %.600v", seed, set, err)
		}
		compared, twice, cut, failed, differ := set.Metrics[0].Value, set.Metrics[1].Value, set.Metrics[2].Value, set.Metrics[3].Value, set.Metrics[4]
		if differ.Value != 0 {
			t.Fatalf("seed %d: %v of %v answers are not what they were, the first: %s", seed, differ.Value, compared, differ.Labels["first"])
		}
		if compared < float64(generated)*9/10 || twice < float64(generated)/8 || cut < 5 || failed < 5 {
			t.Fatalf("seed %d: %v answers compared, %v holding a list or a dict twice, %v with a metric cut and %v that failed: the table should hold many of each", seed, compared, twice, cut, failed)
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered", pool.Starts, pool.Stops)
	}
}
