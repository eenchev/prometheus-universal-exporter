package transform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A script can leave one long string in an answer many times: as the items
// of a list, the values of a dict, a cell of every row, a label or the help
// of every metric. The list or the dict is there once, so nothing of what
// pythonshared_test.go is about holds it back, and the string is written
// each time: a thousand characters held a hundred thousand times are a
// hundred megabytes of JSON from a megabyte of memory. The worker made all
// of that in its memory before it wrote the first byte, for the exporter to
// read up to limits.max_output_bytes, refuse the answer and stop the worker.
// These tests leave such strings in data and in metrics, and want each
// answer refused by the worker itself, unwritten, in the words of the
// refusal of a list that is held too often, by a worker that then serves the
// next request; and they leave answers at the limit, just over it and under
// it, which end as they did.

// longStrings is a pre-script that leaves in data what the response's first
// line names, of as many copies of a text of a thousand characters, or of as
// many characters, as its second line says. Its interpreter counts the
// memory it holds, as sharedShapes' does, and the run that asks for the
// "peak" says how much that was at most since the run that asked last.
const longStrings = `
import tracemalloc
if not tracemalloc.is_tracing(): tracemalloc.start()
shape, size = response.text.split("\n")
size = int(size)
text = "x" * 1000
def texts(length):
    # Texts of a thousand characters and one of the rest, length together.
    return [text] * (length // 1000) + ["y" * (length % 1000)]
if shape == "peak":
    data = tracemalloc.get_traced_memory()[1]
    tracemalloc.stop()
    tracemalloc.start()
elif shape == "a text in a list":
    data = [text] * size
elif shape == "a text in a tuple":
    data = (text,) * size
elif shape == "a text as every value of a dict":
    data = dict.fromkeys(["k%d" % i for i in range(size)], text)
elif shape == "a text in every row":
    data = {"rows": [{"id": i, "cells": [text, text]} for i in range(size // 2)]}
elif shape == "a text in a list, beside a NaN":
    data = [float("nan")] + [text] * size
elif shape == "a text in a list, far down":
    data = [text] * size
    for _ in range(3000): data = [data]
elif shape == "one text alone":
    data = "x" * size
elif shape == "texts as long together":
    data = texts(size)
elif shape == "texts as long together, beside a NaN":
    data = [float("nan")] + texts(size)
elif shape == "texts as long together, far down":
    data = texts(size)
    for _ in range(3000): data = [data]
elif shape == "numbers":
    data = list(range(size))
else:
    fail("no shape " + shape)
`

// longStringsLimit is the limits.max_output_bytes of these tests'
// collectors, and longStringsEnvelope what a pre-script's answer is longer
// than its data written out: {"ok": true, "log": "", "data": } around it.
const (
	longStringsLimit    = 1 << 17
	longStringsEnvelope = len(`{"ok": true, "log": "", "data": }`)
)

// longStringsCollector is a collector whose pre-script is longStrings. Its
// memory limit is what ends a worker that writes out a hundred megabytes,
// so that such a run fails its test in seconds rather than filling the
// machine.
func longStringsCollector() *model.Collector {
	c := sharedCollector()
	c.Name, c.Transform.PreScript = "strings", longStrings
	c.Limits.MaxOutputBytes = longStringsLimit
	return c
}

// leaveStrings runs the collector's pre-script on a response that names a
// shape and its size, and returns the data it left.
func leaveStrings(t *testing.T, c *model.Collector, shape string, size int) (any, error) {
	t.Helper()
	return leaveShared(t, c, shape, size)
}

// Data that holds one text of a thousand characters many times over, so
// that it is megabytes written, twenty of them for a list of twenty
// thousand, is refused for its length by the worker, naming
// limits.max_output_bytes as the refusal of a list held too often does: as
// the items of a list or a tuple, the values of a dict, the cells of every
// row, beside a value that is no JSON, at the end of three thousand levels,
// which the worker writes without json, and as one text alone. Nothing of
// it is written out or made: the worker holds about twice what its script
// built, where it held all the answer is written as, twice. One worker
// refuses them all, as its scripts' failures, and then answers; each cost
// the exporter a worker before, stopped for an answer over the limit.
func TestDataWhoseStringsAreLongerThanTheLimitIsRefusedUnwritten(t *testing.T) {
	requirePython(t)
	c := longStringsCollector()
	want := "python pre-script failed: " + fmt.Sprintf(tooLong, "data")
	sharedPeak(t, c)
	// Each is of the size it is under the race detector too: it is built
	// and refused in the worker, which the detector does not slow, and all
	// the exporter hands over and reads of one is the shape's name and the
	// refusal. most is what the worker may hold over it: about twice the
	// list, the dict or the rows the script built, which the worker's look
	// through them has beside the script's, and a part of what the answer
	// is written as, a thousand bytes for each copy, of which the worker
	// held two copies.
	shapes := []struct {
		name string
		size int
		most int
	}{
		{"a text in a list", 20000, 1 << 20},
		{"a text in a tuple", 20000, 1 << 20},
		{"a text as every value of a dict", 10000, 2 << 20},
		{"a text in every row", 10000, 4 << 20},
		{"a text in a list, beside a NaN", 20000, 1 << 20},
		{"a text in a list, far down", 20000, 2 << 20},
		{"one text alone", 4 << 20, 5 << 20},
	}
	for _, shape := range shapes {
		data, err := leaveStrings(t, c, shape.name, shape.size)
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s, %d of it: %.100v, %.300v, want the script's failure %q", shape.name, shape.size, data, err, want)
		}
		if peak := sharedPeak(t, c); peak > shape.most {
			t.Fatalf("%s, %d of it: the worker held %d bytes to refuse it, want no more than %d", shape.name, shape.size, peak, shape.most)
		}
	}
	data, err := leaveStrings(t, c, "a text in a list", 100)
	if items, ok := data.([]any); err != nil || !ok || len(items) != 100 {
		t.Fatalf("after the refusals: %.100v, %v, want the worker to answer", data, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunScriptError] != uint64(len(shapes)) {
		t.Fatalf("%d workers started, %v stopped and the runs ended %v, want one worker to have refused the %d as its scripts' failures", pool.Starts, pool.Stops, pool.Runs, len(shapes))
	}
}

// The worker refuses an answer for its strings only when they alone are
// longer than limits.max_output_bytes, each counted as long as it is and
// as often as it is there: that is the least the answer can be written as,
// so no answer the exporter would take is refused. Texts exactly as long
// together as the limit are not refused by the worker: the answer is longer
// than they are, by its quotes and commas, so it is written and the
// exporter refuses it, with the output limit's error and its worker, as it
// did; one character more and the worker refuses it itself, and stays. So
// it is in a plain list, beside a value that is no JSON, and three thousand
// levels down, where the limit is met nine characters sooner: what writes
// an answer nested that deep counts every key it writes with the strings,
// the answer's own ok, log and data among them (pythonkeys_test.go). And
// an answer is still taken when its line is exactly the limit, and refused
// by the exporter one byte past it.
func TestTheWorkerRefusesForItsStringsOnlyPastTheLimit(t *testing.T) {
	requirePython(t)
	c := longStringsCollector()
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
	for _, shape := range []string{"texts as long together", "texts as long together, beside a NaN", "texts as long together, far down"} {
		atLimit := longStringsLimit
		if shape == "texts as long together, far down" {
			atLimit -= longKeysEnvelope
		}
		lost(shape, atLimit)
		starts := PythonWorkers().PoolSnapshot().Starts
		if _, err := leaveStrings(t, c, shape, atLimit+1); err == nil || err.Error() != refused || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s %d: %.300v, want the script's failure %q", shape, atLimit+1, err, refused)
		}
		// The worker that refused it answers the next request.
		if data, err := leaveStrings(t, c, shape, 2500); err != nil || !strings.Contains(fmt.Sprint(data), strings.Repeat("y", 500)) {
			t.Fatalf("%s 2500: %.100v, %v, want the texts", shape, data, err)
		}
		if pool := PythonWorkers().PoolSnapshot(); pool.Starts != starts+1 || pool.Stops[pythonStopOutputLimit] != stopped {
			t.Fatalf("%s: %d workers started since the refusal and %v stopped, want the one that refused to have answered", shape, pool.Starts-starts, pool.Stops)
		}
	}
	// One text whose answer is exactly the limit, and one byte more.
	fits := longStringsLimit - longStringsEnvelope - len(`""`)
	data, err := leaveStrings(t, c, "one text alone", fits)
	if text, ok := data.(string); err != nil || !ok || len(text) != fits {
		t.Fatalf("one text of %d characters, an answer of %d bytes: %.100v, %v, want it taken", fits, longStringsLimit, data, err)
	}
	lost("one text alone", fits+1)
	// Numbers have no strings: 30,000 of them are 200,000 bytes written,
	// which the exporter refuses.
	lost("numbers", 30000)
}

// longStringMetrics is a transform that emits what the response's first
// line names, of as many metrics as its second line says, each with a text
// of a thousand characters.
const longStringMetrics = `
shape, size = response.text.split("\n")
size = int(size)
text = "x" * 1000
if shape == "a label of every metric":
    for i in range(size): metric("m", value=i, labels={"id": str(i), "blob": text})
elif shape == "the help of every metric":
    for i in range(size): metric("m", value=i, labels={"id": str(i)}, help=text)
elif shape == "a label of every metric, one of them NaN":
    for i in range(size): metric("m", value=i if i else float("nan"), labels={"id": str(i), "blob": text})
elif shape == "under a key of the script's own":
    metric("m", value=1)
    metrics[0]["mine"] = [[text] * size]
elif shape == "under a key of the script's own, beside lists nested too deep":
    metric("m", value=1)
    deep = [1]
    for _ in range(3000): deep = [deep]
    metrics[0]["mine"] = [[text] * size]
    metrics[0]["deep"] = deep
else:
    fail("no shape " + shape)
`

// The metrics of a transform are held to the same: a label or a help text
// of a thousand characters on thousands of metrics, written megabytes
// long, fails the scrape as the script's, naming limits.max_output_bytes
// and metrics, and the worker stays, also when a value among them is no
// JSON, and when the text is under a key of the script's own, which is
// written where the worker can follow what is there. A few such metrics,
// within the limit, are the series they were. And where what is under such
// a key is not written at all, because the worker cannot follow what
// stands beside it, the text is not counted either: the metric is the one
// series it was.
func TestMetricsWhoseStringsAreLongerThanTheLimitAreRefusedUnwritten(t *testing.T) {
	requirePython(t)
	c := workerCollector("strings", longStringMetrics)
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
	shapes := []string{"a label of every metric", "the help of every metric", "a label of every metric, one of them NaN", "under a key of the script's own"}
	for _, shape := range shapes {
		set, err := run(shape, 5000)
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Fatalf("%s, 5000 of them: %v, %.300v, want the script's failure %q", shape, set, err, want)
		}
	}
	for _, shape := range shapes[:2] {
		set, err := run(shape, 50)
		if err != nil || len(set.Metrics) != 50 || set.Metrics[49].Value != 49 {
			t.Fatalf("%s, 50 of them: %v, %.300v, want the 50 series", shape, set, err)
		}
	}
	set, err := run("under a key of the script's own, beside lists nested too deep", 5000)
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Name != "m" || set.Metrics[0].Value != 1 {
		t.Fatalf("a text 5000 times under a key that is not written: %v, %.300v, want the one series", set, err)
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunScriptError] != uint64(len(shapes)) {
		t.Fatalf("%d workers started, %v stopped and the runs ended %v, want one worker to have refused the %d as its scripts' failures", pool.Starts, pool.Stops, pool.Runs, len(shapes))
	}
}

// stringsOracle is a transform that writes answers as the worker wrote them
// before it counted their strings, and as it writes them now, and says how
// each pair compares. The answer as it was is here, as the oracle: plain,
// wire, answer and what writes an answer nested too deep, word for word,
// with the worker's own weigh, flat and errors, which are as they were.
//
// The answers are made from the seed the response gives, of every kind of
// value a script leaves, as data and as metrics: plain and not, of types of
// the script's own, nested a little and a lot, holding lists and dicts many
// times, and holding texts of up to nine thousand characters, alone and
// many times over, so that many are within the limit of the test's
// collector, many over it by their strings alone, and many over it only by
// what is around them.
//
// What must hold of each pair. An answer that was written is the same line
// now, byte for byte, unless the strings it is written with — read back
// from the line itself, each as long as it is, the markers of NaN and the
// infinities and the keys not among them — are longer together than the
// limit: then the worker refuses it, with the error of a list held too
// often. That line was longer than the limit, so the exporter refused it
// and stopped its worker. The worker may refuse it too where its strings
// are longer than the limit only with its keys, which it counts where its
// look at them says they may be long (pythonkeys_test.go, where that is
// compared): such a line was longer than the limit as well, and is counted
// with those that were. An answer that failed fails with the same error,
// the worker's own refusals among them; but where its strings, as the
// script left them, are over the limit, alone or with its keys, it may
// fail with the refusal for them instead of what failed it before, which
// is counted apart (otherwise): by this script's own count of them, the
// strings of what the script left and the keys that are strings, each as
// often as it is there, and the three keys of the answer around it.
const stringsOracle = `
import __main__, collections, json, sys
most, deepest, weigh, flat, too_deep, too_long = __main__.most, __main__.deepest, __main__.weigh, __main__.flat, __main__.too_deep, __main__.too_long
class Unwritable(Exception): pass
whole=[None,None]
met=set()
def was_wire(v):
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
def was_plain(document,levels=1<<30):
    scalars=frozenset((int,bool,type(None)))
    level=[document]; room=half=most//2; look=4096
    for _ in range(min(levels,sys.getrecursionlimit()//2-10)):
        below=[]
        extend=below.extend
        for v in level:
            t=type(v)
            if t is str: continue
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
        if not below: return True
        room-=len(below)
        if half-room>look:
            look=(half-room)*4
            n=whole[1]=weigh(document,look>>8)
            if n is not None:
                if n<0 or n>most: return None
                look=1<<62
        level=below
    return False
quote=json.encoder.encode_basestring_ascii
def key_text(k):
    if k.__class__ is str: return quote(k)
    return json.dumps({k:None},allow_nan=False)[1:-7]
def was_deep_dumps(document):
    text=[]; put=text.append; inside=[]; none=inside
    v=document
    while True:
        if isinstance(v,dict): put('{'); inside.append([iter(v.items()),'}',''])
        elif isinstance(v,(list,tuple)): put('['); inside.append([iter(v),']',''])
        else: put(json.dumps(was_wire(v),allow_nan=False))
        if len(inside)>deepest+1: raise too_deep()
        while inside:
            at=inside[-1]
            v=next(at[0],none)
            if v is not none: break
            put(at[1]); inside.pop()
        else: return ''.join(text)
        put(at[2]); at[2]=', '
        if at[1]=='}': put(key_text(v[0])); put(': '); v=v[1]
def was_written(document):
    text=None; follow=False; whole[0]=document; whole[1]=None
    try:
        follow=was_plain(document,5) if 'metrics' in document else was_plain(document)
        if follow: text=json.dumps(document,allow_nan=False)
    except Exception: text=None; follow=False
    if text is None and follow is not None:
        try: text=json.dumps(was_wire(document),allow_nan=False)
        except Unwritable: pass
        except RecursionError:
            if 'data' not in document and 'metrics' not in document: raise
        finally: met.clear()
    if text is None:
        if 'metrics' in document:
            document=dict(document,metrics=flat(document['metrics']))
            if weigh(document)>most: raise too_long('metrics')
        else:
            n=sized()
            if n<0: raise too_deep()
            if n>most: raise too_long('data')
        text=was_deep_dumps(document)
    return text+'\n'
def was_answer(document):
    # As deep in calls as now_answer, so that both meet the recursion
    # limit at one depth.
    return was_written(document)

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
    try: return answer(document)
    except BaseException as e: return "%s: %s" % (type(e).__name__, e)

def strings_left(v, keys=False):
    # How long the strings of what a script left are together, each as
    # often as it is there, with the keys that are strings where keys is
    # set, or None for more than a script of this test leaves: with the
    # keys or without them, the same number of steps.
    n, left, steps = 0, [v], 0
    while left:
        x = left.pop()
        steps += 1
        if steps > 300000: return None
        if type(x) is str: n += len(x)
        elif isinstance(x, dict):
            left.extend(x.values())
            if keys: n += sum(str.__len__(k) for k in x if isinstance(k, str))
        elif isinstance(x, (list, tuple)): left.extend(x)
    return n
def strings_written(line, keys=False):
    # The same of an answer's line, read back: of its values, the markers
    # of the floats that are no JSON left out, with its keys where keys is
    # set. A line nested deeper than json reads one is read as the worker
    # reads such a request.
    try: read = json.loads(line)
    except RecursionError: read = __main__.deep_loads(line)
    n, left = 0, [read]
    while left:
        x = left.pop()
        if type(x) is str:
            if not x.startswith('\x00pue-nonfinite:'): n += len(x)
        elif type(x) is dict:
            left.extend(x.values())
            if keys: n += sum(map(len, x))
        elif type(x) is list: left.extend(x)
    return n

seed = [int(response.text)]
def pick(n):
    seed[0] = (seed[0] * 6364136223846793005 + 1442695040888963407) % (1 << 64)
    return (seed[0] >> 33) % n
class Rows(list): pass
class Bag(dict): pass
class Odd(float): pass
Pair = collections.namedtuple("Pair", "a b")
texts = ["", "text", "café   \"q\" \\ \x00", "x" * 40, "y" * 300, "z" * 2500, "w" * 9000]
finite = [0, 1, -7, 10**30, 1.5, -0.0, 5e-324, True, False, None] + texts
other = finite + [float("nan"), float("inf"), float("-inf"), Odd(2.5), Odd("nan"), 10**400]
def key(i, odd):
    return [1, None, True, 1.5][pick(4)] if odd and not pick(6) else "key %d" % (i if pick(8) else 0)
def text():
    # A text, the long ones less often.
    return texts[pick(len(texts)) if pick(3) else pick(4)]
def value(depth, odd, held):
    if odd and not pick(150): return [{1, 2}, b"bytes", 2j][pick(3)]
    if depth <= 0 or pick(10) < 4: return (other if odd else finite)[pick(len(other if odd else finite))] if pick(3) else text()
    if held is not None and held and not pick(6): return held[pick(len(held))]
    below = lambda: value(depth - 1, odd, held)
    n, kind = pick(5), pick(11 if odd else 5)
    if kind == 0: made = [below() for _ in range(n)]
    elif kind == 1: made = tuple(below() for _ in range(n))
    elif kind == 2: made = {key(i, odd): below() for i in range(n)}
    elif kind == 3: made = [text()] * pick(12)
    elif kind == 4: made = dict.fromkeys(["k%d" % i for i in range(pick(12))], text())
    elif kind == 5: made = collections.OrderedDict((key(i, odd), below()) for i in range(n))
    elif kind == 6: made = Rows(below() for _ in range(n))
    elif kind == 7: made = Pair(below(), below())
    elif kind == 8: made = collections.defaultdict(list, {key(i, False): below() for i in range(n)})
    elif kind == 9: made = Bag((key(i, False), below()) for i in range(n))
    else: made = [below() for _ in range(n)]
    if held is not None: held.append(made)
    return made
def entry(odd, held):
    made = {"name": "m%d" % pick(5), "value": pick(100)}
    for _ in range(pick(4)):
        made[["labels", "help", "type", "timestamp", "value", "mine", "name"][pick(7)]] = value(3, odd, held)
    if not pick(3): made["labels"] = {"l%d" % i: value(1 if pick(4) else 3, odd, held) for i in range(pick(4))}
    if not pick(4): made["help"] = text()
    return made if pick(8) else value(3, odd, held)
def nested(depth, leaf):
    for _ in range(depth): leaf = [leaf]
    return leaf
def long_together(n):
    return ["x" * 1000] * (n // 1000) + ["y" * (n % 1000)]

pair, labels = [1, 2], {"site": "a", "none": None}
documents = [("data", [pair] * 3000), ("data", list(range(6000))), ("data", "x" * most), ("data", "x" * (most + 1)),
    ("metrics", [{"name": "m", "value": i, "labels": labels} for i in range(40)]), ("metrics", [{"name": "m", "value": i, "labels": labels} for i in range(400)]),
    ("metrics", [{"name": "m", "value": i, "labels": {"blob": "z" * 500}} for i in range(100)]),
    ("metrics", [{"name": "m", "value": float("nan"), "labels": {"blob": "z" * 500}} for i in range(100)]),
    ("metrics", [{"name": "m", "value": 1, "mine": [["z" * 500] * 100]}]), ("metrics", [{"name": "m", "value": 1, "mine": [["z" * 500] * 100], "deep": nested(3000, 1)}])]
for n in (most - 1, most, most + 1):
    documents += [("data", long_together(n)), ("data", [float("nan")] + long_together(n)), ("data", nested(3000, long_together(n))), ("data", {"rows": [{"cells": tuple(long_together(n))}]})]
doubled = ["leaf " * 20]
for _ in range(11): doubled = [doubled, doubled]
documents += [("data", doubled), ("data", [float("nan"), doubled])]
for i in range(int(response.headers["Generated"][0])):
    odd, held = i % 2 == 1, [] if i % 3 else None
    if i % 16 == 5: documents.append(("data", list(range(most // 3 + pick(most // 8))) + [text() for _ in range(pick(3))]))
    elif i % 4 < 2: documents.append(("data", value(6, odd, held)))
    else: documents.append(("metrics", [entry(odd, held) for _ in range(pick(6))]))

compared = same = long = refused = failed = otherwise = 0
differ = []
for what, left in documents:
    held = strings_left(left)
    if held is None: continue
    document = {"ok": True, "log": "", what: left}
    was, now = written(was_answer, document), written(now_answer, document)
    compared += 1
    refusal = "OverflowError: %s" % too_long(what)
    if was.endswith("\n"):
        over = strings_written(was) > most
        if over and len(was) - 1 <= most: differ.append("%s %.200r: its strings are longer than the limit and its line is not" % (what, left))
        elif not over and now == refusal and len(was) - 1 > most and strings_written(was, True) > most: long += 1
        elif now != (refusal if over else was): differ.append("%s %.200r is answered %.300r, and was %.300r" % (what, left, now, was))
        elif over: refused += 1
        elif len(was) - 1 > most: long += 1
        else: same += 1
    elif now == was: failed += 1
    elif now == refusal and (held > most or strings_left(left, True) + len("ok" + "log" + what) > most): otherwise += 1
    else: differ.append("%s %.200r fails with %.300r, and failed with %.300r" % (what, left, now, was))
metric("compared", value=compared)
metric("same", value=same)
metric("long", value=long)
metric("refused", value=refused)
metric("failed", value=failed)
metric("otherwise", value=otherwise)
metric("differ", value=len(differ), labels={"first": differ[0] if differ else ""})
`

// An answer the exporter took before is the line it was, byte for byte; one
// that was written longer than the limit is refused by the worker now
// exactly when its strings alone are longer than the limit, and is written
// as it was when they are not; and one that failed fails as it did: for
// hundreds of generated answers, with texts in them held once and many
// times, and for those at the limit, one character under and one over it,
// in a plain answer, in one that goes through the worker's copy and in one
// nested too deep for it. The answers are fewer under the race detector,
// which does not slow the worker that makes and compares them: the floors
// of each kind are parts of how many there are.
func TestAnswersWithinTheLimitByTheirStringsAreTheLinesTheyWere(t *testing.T) {
	requirePython(t)
	c := workerCollector("strings oracle", stringsOracle)
	c.Limits.MaxOutputBytes = 1 << 14
	generated := alloctest.UnlessRaced(1200, 400)
	seeds := alloctest.UnlessRaced(2, 1)
	for seed := 1; seed <= seeds; seed++ {
		body := strconv.Itoa(seed)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Generated": {strconv.Itoa(generated)}}}
		ctx, cancel := context.WithTimeout(context.Background(), sharedTime)
		set, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c)
		cancel()
		if err != nil || len(set.Metrics) != 7 {
			t.Fatalf("seed %d: %v, %.600v", seed, set, err)
		}
		count := func(i int) float64 { return set.Metrics[i].Value }
		compared, same, long, refused, failed, otherwise, differ := count(0), count(1), count(2), count(3), count(4), count(5), set.Metrics[6]
		if differ.Value != 0 {
			t.Fatalf("seed %d: %v of %v answers are not what they should be, the first: %s", seed, differ.Value, compared, differ.Labels["first"])
		}
		t.Logf("seed %d: %v answers compared: %v the lines they were, %v written longer than the limit as they were, %v refused for their strings, %v failed as they did, %v failed for their strings instead", seed, compared, same, long, refused, failed, otherwise)
		floor := float64(generated)
		if compared < floor*9/10 || same < floor/3 || long < floor/30 || refused < floor/20 || failed < floor/150 {
			t.Fatalf("seed %d: %v answers compared, %v the lines they were, %v longer than the limit as they were, %v refused for their strings and %v that failed: the table should hold many of each", seed, compared, same, long, refused, failed)
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 1 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want the one worker to have answered", pool.Starts, pool.Stops)
	}
}
