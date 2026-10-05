package transform

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A response nested as deep as its decoder reads one is handed to a script
// and read back from it (decode.MaxDepth). It was not: a JSON body nested
// exactly 10,000 deep failed with "exceeded max depth", the request around
// it being one object deeper than json.Marshal writes; one nested 9,997 to
// 9,999 deep with the traceback of the worker's own json.loads, which the
// interpreter stops at about 10,000 calls in C; and what a pre-script left
// in data nested about 1,000 deep, with a RecursionError. These tests hand
// over the deepest documents of the JSON and YAML decoders, in both
// directions, and the first that are refused.

// deepJSON is a JSON body nested depth deep around the number 7: arrays,
// objects under the key "a", or the two in turn, the outermost an array.
func deepJSON(shape string, depth int) string {
	var open, shut strings.Builder
	for i := range depth {
		if shape == "arrays" || shape == "mixed" && i%2 == 0 {
			open.WriteString("[")
		} else {
			open.WriteString(`{"a":`)
		}
	}
	for i := depth - 1; i >= 0; i-- {
		if shape == "arrays" || shape == "mixed" && i%2 == 0 {
			shut.WriteString("]")
		} else {
			shut.WriteString("}")
		}
	}
	return open.String() + "7" + shut.String()
}

// deepShapes are the shapes of deepJSON.
var deepShapes = []string{"arrays", "objects", "mixed"}

// deepWalk is a script that goes down data, the first value of each list
// and the value of each dict's one key, and says how deep it went and what
// it found there.
const deepWalk = `
d = data
depth = 0
while isinstance(d, (list, dict)) and d:
    d = d[0] if isinstance(d, list) else next(iter(d.values()))
    depth += 1
metric("depth", value=depth, labels={"leaf": repr(d)})
`

// deepCollector is a collector of the named decoder whose scripts have a
// megabyte for an answer, which a document nested as deep as one decodes is
// well within.
func deepCollector(kind string) *model.Collector {
	return &model.Collector{Name: "deep", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: kind},
		Limits: model.Limits{ScriptTimeout: model.Duration(5 * time.Second), MaxOutputBytes: 1 << 20}}
}

// deepDecoded is body as the collector's decoder reads it, or its error.
func deepDecoded(c *model.Collector, body string) (*decode.Decoded, *fetch.HTTPResponse, error) {
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	return d, r, err
}

// sameDeep reports whether two decoded values are one: a float by its bits,
// so that NaN is itself. It says nothing of where they differ, which for a
// value nested ten thousand deep would be a path of as many steps at each.
func sameDeep(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, value := range x {
			other, has := y[key]
			if !has || !sameDeep(value, other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameDeep(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case *big.Int:
		y, ok := b.(*big.Int)
		return ok && x.Cmp(y) == 0
	}
	return a == b
}

// nestedDepth is how deep v is nested along the first value of each list
// and the one value of each mapping.
func nestedDepth(v any) int {
	for depth := 0; ; depth++ {
		switch x := v.(type) {
		case []any:
			if len(x) == 0 {
				return depth + 1
			}
			v = x[0]
		case map[string]any:
			if len(x) == 0 {
				return depth + 1
			}
			for _, value := range x {
				v = value
			}
		default:
			return depth
		}
	}
}

// deepestNesting is how deep v is nested at its deepest.
func deepestNesting(v any) int {
	deepest := 0
	switch x := v.(type) {
	case []any:
		for _, value := range x {
			deepest = max(deepest, deepestNesting(value))
		}
		return deepest + 1
	case map[string]any:
		for _, value := range x {
			deepest = max(deepest, deepestNesting(value))
		}
		return deepest + 1
	}
	return 0
}

// handOverDeep gives the decoded document to a python transform, which
// emits one metric of how deep it is and what its innermost value is; to a
// pre-script that hands it on to a jq rule, which reads that value; and to a
// pre-script that leaves data as it is, whose answer must be the document.
func handOverDeep(t *testing.T, name string, c *model.Collector, d *decode.Decoded, r *fetch.HTTPResponse, depth int, leaf string, leafValue float64) {
	t.Helper()
	ctx := context.Background()
	transform := *c
	transform.Transform = model.TransformConfig{Type: "python", Script: deepWalk}
	set, err := Transform(ctx, d, r, &transform, "python3")
	if err != nil {
		t.Fatalf("%s, a python transform: %.300v", name, err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != float64(depth) || set.Metrics[0].Labels["leaf"] != leaf {
		t.Fatalf("%s, a python transform: the script found %v at depth %v, want %s at %d", name, set.Metrics[0].Labels, set.Metrics[0].Value, leaf, depth)
	}
	rules := *c
	rules.Transform = model.TransformConfig{Type: "jq", PreScript: "data = data\n"}
	rules.Metrics = []model.MetricRule{{Name: "innermost", Type: model.GaugeMetricType, Expression: "last(..)"}}
	set, err = Transform(ctx, d, r, &rules, "python3")
	if err != nil {
		t.Fatalf("%s, a pre-script and a jq rule: %.300v", name, err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != leafValue && !(math.IsNaN(leafValue) && math.IsNaN(set.Metrics[0].Value)) {
		t.Fatalf("%s, a pre-script and a jq rule: %v, want one series of %v", name, set.Metrics, leafValue)
	}
	left := *c
	left.Transform = model.TransformConfig{Type: "jq", PreScript: "pass\n"}
	out, err := applyPreScript(ctx, d, r, &left, "python3")
	if err != nil {
		t.Fatalf("%s, a pre-script that leaves data: %.300v", name, err)
	}
	if !sameDeep(d.Data, out.Data) || nestedDepth(out.Data) != depth {
		t.Fatalf("%s, a pre-script that leaves data: what it left, nested %d deep, is not the document it was given, nested %d deep", name, nestedDepth(out.Data), depth)
	}
}

// A JSON body nested 9,998, 9,999 and 10,000 deep — the deepest the decoder
// reads — in arrays, in objects and in the two in turn, reaches a python
// transform, goes through a pre-script to a jq rule, and comes back from a
// pre-script as the document it was; one nested 10,001 deep is refused by
// the decoder, as it always was. One worker serves every body of a
// collector, the deepest among them, and is there for the next.
func TestAJSONBodyNestedAsDeepAsItDecodesIsHandedToItsScripts(t *testing.T) {
	requirePython(t)
	c := deepCollector("json")
	depths, shapes := []int{9998, 9999, 10000}, deepShapes
	if raceDetector {
		// The deepest alone, of the shape that is both: the walks through a
		// document nested this deep take seconds under the race detector.
		depths, shapes = []int{10000}, []string{"mixed"}
	}
	for _, shape := range shapes {
		for _, depth := range depths {
			d, r, err := deepDecoded(c, deepJSON(shape, depth))
			if err != nil {
				t.Fatalf("%s nested %d deep: %v", shape, depth, err)
			}
			handOverDeep(t, fmt.Sprintf("%s nested %d deep", shape, depth), c, d, r, depth, "7", 7)
		}
		_, _, err := deepDecoded(c, deepJSON(shape, 10001))
		if err == nil || !strings.Contains(err.Error(), "arrays and objects nested more than 10000 deep") {
			t.Fatalf("%s nested 10001 deep: %v, want the decoder to refuse the body for its depth", shape, err)
		}
	}
	// A document of every kind of value at every level — text with escapes
	// and bytes that are not UTF-8, numbers of every form, a number kept as
	// its text, null, an empty list and an empty object beside the next
	// level — nested 10,000 deep comes back from a pre-script as the
	// document it was: what reads a deep request and writes a deep answer
	// loses nothing of it.
	const levels = 4999
	rich := strings.Repeat(`[1, -2.5, 1e21, 1e400, 123456789012345678901234567890, "quote \" and \u00e9 and caf`+"\xe9"+`\n", null, true, [], {}, {"z": "after", "a": `, levels) + `{"twice": 1, "twice": [0.30000000000000004, 1500000000000000001]}` + strings.Repeat(`, "": false}]`, levels)
	d, r, err := deepDecoded(c, rich)
	if err != nil || deepestNesting(d.Data) != 2*levels+2 {
		t.Fatalf("a document of every kind of value: decoded nested %d deep (%v), want %d", deepestNesting(d.Data), err, 2*levels+2)
	}
	left := *c
	left.Transform = model.TransformConfig{Type: "jq", PreScript: "pass\n"}
	out, err := applyPreScript(context.Background(), d, r, &left, "python3")
	if err != nil || !sameDeep(d.Data, out.Data) || deepestNesting(out.Data) != 2*levels+2 {
		t.Fatalf("a document of every kind of value nested %d deep: what a pre-script left is not the document it was given (%.300v)", 2*levels+2, err)
	}
	// Three scripts ran, each in a worker of its own that took every body.
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 3 || len(pool.Stops) != 0 || pool.Runs[pythonRunOK] != uint64(3*len(shapes)*len(depths)+1) {
		t.Fatalf("%d workers started, %v stopped and the runs ended %v, want 3 workers for %d runs and none stopped", pool.Starts, pool.Stops, pool.Runs, 3*len(shapes)*len(depths)+1)
	}
}

// The deepest YAML documents, nested 10,000 deep as the deepest JSON ones
// are: sequences by indentation around sequences in brackets, which the
// parser bounds apart and which were handed over nested 20,000 deep while
// nothing else bounded them; the same around a NaN, which the worker writes
// back as a marker; mappings in brackets; and mappings each with a sequence
// written under its key, two levels for one of indentation. Each is handed
// to its scripts and read back. One level more is the decoder's to refuse,
// whichever way the document is nested, so no script is given, and none can
// leave, a value nested deeper than another decoder makes one.
func TestAYAMLDocumentNestedAsDeepAsItDecodesIsHandedToItsScripts(t *testing.T) {
	requirePython(t)
	c := deepCollector("yaml")
	// The depth as a document says it, and not as the decoders state it: the
	// one limit is theirs to keep.
	const most = 10000
	const each = most / 2
	indented := strings.Repeat("- ", each)
	bracketed := func(levels int, leaf string) string {
		return strings.Repeat("[", levels) + leaf + strings.Repeat("]", levels) + "\n"
	}
	const levels = 300
	lines := []string{"k:"}
	for i := range levels {
		lines = append(lines, strings.Repeat("  ", i)+"- k:")
	}
	underKeys := strings.Join(lines, "\n") + "\n" + strings.Repeat("  ", levels) + "- "
	documents := []struct {
		name, body, leaf string
		value            float64
	}{
		{"sequences by indentation around sequences in brackets", indented + bracketed(each, "7"), "7", 7},
		{"the same around a NaN", indented + bracketed(each, ".nan"), "nan", math.NaN()},
		{"mappings in brackets", strings.Repeat("{a: ", most) + "7" + strings.Repeat("}", most) + "\n", "7", 7},
		{"sequences under keys around sequences in brackets", underKeys + bracketed(most-2*(levels+1), "7"), "7", 7},
	}
	if raceDetector {
		documents = documents[1:2]
	}
	for _, document := range documents {
		d, r, err := deepDecoded(c, document.body)
		if err != nil {
			t.Fatalf("%s: %v", document.name, err)
		}
		if got := nestedDepth(d.Data); got != most {
			t.Fatalf("%s: decoded nested %d deep, want %d", document.name, got, most)
		}
		handOverDeep(t, document.name, c, d, r, most, document.leaf, document.value)
	}
	// One level more is the decoder's to refuse, in the same words whether
	// the parser's two bounds let the document through or one of them, where
	// the document is nested one way, does not.
	const refused = "YAML decode: yaml: the document is nested more than 10000 deep, counting what its aliases stand for; a response may nest 10000 deep at most"
	for name, body := range map[string]string{
		"one more level of indentation":               "- " + indented + bracketed(each, "7"),
		"one more level of brackets":                  indented + bracketed(each+1, "7"),
		"10,000 levels of each":                       indented + indented + bracketed(2*each, "7"),
		"one more level of brackets, under keys":      underKeys + bracketed(most-2*(levels+1)+1, "7"),
		"one more level of brackets, beside an alias": "a: &a 1\nb: *a\nc:\n  " + indented + bracketed(each, "7"),
		"one more level of indentation, one way":      indented + indented + "- 7\n",
		"one more level of brackets, one way":         bracketed(most+1, "7"),
	} {
		if _, _, err := deepDecoded(c, body); err == nil || err.Error() != refused {
			t.Fatalf("%s: %.200v, want the decoder to refuse the document for its depth with %q", name, err, refused)
		}
	}
	if pool := PythonWorkers().PoolSnapshot(); pool.Starts != 3 || len(pool.Stops) != 0 {
		t.Fatalf("%d workers started and %v stopped, want 3 workers, one for each script, and none stopped", pool.Starts, pool.Stops)
	}
}

// deepBuild is a pre-script that leaves a value nested as deep as the
// response's first line says, of the shape its second line names, around
// the value of its third.
const deepBuild = `
depth, shape, leaf = response.text.split("\n")
value = {"seven": 7, "nan": float("nan"), "empty": [], "itself": None}[leaf]
for i in range(int(depth) - 1, -1, -1):
    if shape == "lists" or shape == "mixed" and i % 2 == 0:
        value = [value]
    elif shape == "tuples":
        value = (value,)
    elif shape == "numbered":
        value = {i: value}
    else:
        value = {"a": value}
if leaf == "itself":
    value = [value]
    value.append(value)
data = value
`

// What a pre-script leaves in data is read back nested as deep as a decoder
// makes a value, whatever its shape: lists, tuples, dicts, dicts with
// numbers for keys and the innermost a NaN, which takes the worker's walk
// for what JSON has no form for. One level deeper it fails as the script's
// failure, saying how deep data may nest, and so does data that holds
// itself; the failure names no depth but the limit, so it is one failure in
// the log however deep the script went. One worker answers all of them, the
// deepest it writes and the first it refuses, and the next request after
// them.
func TestWhatAPreScriptLeavesIsReadBackAsDeepAsADecodedValue(t *testing.T) {
	requirePython(t)
	c := deepCollector("text")
	c.Transform = model.TransformConfig{Type: "jq", PreScript: deepBuild}
	leave := func(depth int, shape, leaf string) (any, error) {
		body := fmt.Sprintf("%d\n%s\n%s", depth, shape, leaf)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}
		out, err := applyPreScript(context.Background(), &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, c, "python3")
		if err != nil {
			return nil, err
		}
		return out.Data, nil
	}
	shapes := []string{"lists", "dicts", "mixed", "tuples", "numbered"}
	depths := []int{400, 1200, 9997, decode.MaxDepth - 1, decode.MaxDepth}
	if raceDetector {
		shapes, depths = []string{"mixed", "numbered"}, []int{1200, decode.MaxDepth}
	}
	runs := 0
	for _, shape := range shapes {
		for _, depth := range depths {
			for _, leaf := range []string{"seven", "nan"} {
				data, err := leave(depth, shape, leaf)
				runs++
				if err != nil {
					t.Fatalf("%s nested %d deep around %s: %.300v", shape, depth, leaf, err)
				}
				innermost := data
				for range depth {
					switch x := innermost.(type) {
					case []any:
						innermost = x[0]
					case map[string]any:
						for _, value := range x {
							innermost = value
						}
					}
				}
				if got := nestedDepth(data); got != depth {
					t.Fatalf("%s nested %d deep around %s: read back nested %d deep", shape, depth, leaf, got)
				}
				if number, isFloat := innermost.(float64); leaf == "seven" && innermost != 7 || leaf == "nan" && !(isFloat && math.IsNaN(number)) {
					t.Fatalf("%s nested %d deep around %s: the innermost value read back is %v", shape, depth, leaf, innermost)
				}
			}
		}
		// A list that is empty is one level too: the deepest is deepest
		// whatever it holds, and one more is refused.
		if data, err := leave(decode.MaxDepth-1, shape, "empty"); err != nil || nestedDepth(data) != decode.MaxDepth {
			t.Fatalf("%s nested %d deep around an empty list: %.300v", shape, decode.MaxDepth-1, err)
		}
		runs++
		const refused = "python pre-script failed: RecursionError: data is nested more than 10000 deep, or a list or a dict in it holds itself; the exporter reads what a script leaves in data nested 10000 deep at most, as deep as it decodes a response"
		for _, too := range []struct {
			depth int
			leaf  string
		}{{decode.MaxDepth + 1, "seven"}, {decode.MaxDepth, "empty"}, {decode.MaxDepth + 5000, "nan"}, {3, "itself"}} {
			_, err := leave(too.depth, shape, too.leaf)
			runs++
			if err == nil || err.Error() != refused {
				t.Fatalf("%s nested %d deep around %s: %.400v, want the failure %q", shape, too.depth, too.leaf, err, refused)
			}
		}
		// And the worker is there for what comes next.
		if data, err := leave(2, shape, "seven"); err != nil || nestedDepth(data) != 2 {
			t.Fatalf("%s nested 2 deep, after the deepest: %v, %v", shape, data, err)
		}
		runs++
	}
	pool := PythonWorkers().PoolSnapshot()
	if pool.Starts != 1 || len(pool.Stops) != 0 || pool.Runs[pythonRunOK]+pool.Runs[pythonRunScriptError] != uint64(runs) || pool.Runs[pythonRunScriptError] != uint64(4*len(shapes)) {
		t.Fatalf("%d workers started, %v stopped and %d runs ended %v, want one worker for them all, none stopped, and %d refused for their depth", pool.Starts, pool.Stops, runs, pool.Runs, 4*len(shapes))
	}
}

// deepDifferential is a script that reads and writes values both ways, by
// json and by what the worker reads and writes a deep value with, and fails
// at the first difference: every line of the response, which are requests
// as the exporter writes them and JSON of every form; a table of values and
// of what json refuses; and values made at random, written in three
// layouts.
const deepDifferential = `
import __main__ as worker
import collections, enum, random

def written(write, value):
    try:
        return write(value)
    except Exception as e:
        return "%s: %s" % (type(e).__name__, e)

def by_json(value): return json.dumps(worker.wire(value), allow_nan=False)

compared = 0
def compare(value):
    global compared
    compared += 1
    want, got = written(by_json, value), written(worker.deep_dumps, value)
    if want != got: fail("%r is written as %s, and by json as %s" % (value, got, want))

def read(text):
    global compared
    compared += 1
    want, got = json.loads(text), worker.deep_loads(text)
    if repr(want) != repr(got): fail("%s is read as %r, and by json as %r" % (text, got, want))
    return want

for line in response.text.split("\n"):
    compare(read(line))

class Text(str): pass
class Level(enum.IntEnum):
    HIGH = 3
class Pair(collections.namedtuple("Pair", "a b")): pass
nan, inf = float("nan"), float("inf")
for value in [
    None, True, False, 0, -1, 10**30, -10**30, 0.0, -0.0, 1.5, 1e21, 1e-7, 5e-324, 1.7976931348623157e308, nan, inf, -inf, "", "text", "café \U0001F600  ", "\x00pue-nonfinite:NaN\x00", "quote \" backslash \\ line\n",
    [], {}, (), [[]], [{}], {"a": []}, ([], {}), [1, [2, [3, [4]]], {"k": (5, 6)}], {"z": 1, "a": 2, "": 3}, {"a": nan, "b": [inf, -inf, {"c": (nan,)}]},
    {1: "int", 2.5: "float", True: "bool", None: "none", "1": "text"}, {False: 0, 0: 1}, {10**30: 1, -0.0: 2, 1e21: 3},
    {(1, 2): "tuple key"}, {nan: "nan key"}, {inf: "inf key"}, {b"bytes": 1}, {"a": {(1,): 2}}, {1, 2}, [1, {2}], {"a": b"bytes"}, object(), [object], {"k": [1, 2, {"deep": {3}}]},
    Text("named"), {Text("k"): Text("v")}, Level.HIGH, {"level": Level.HIGH, Level.HIGH: "key"}, Pair(1, [2]), collections.OrderedDict([("z", 1), ("a", (2,))]), collections.defaultdict(list, b=[nan]),
    [1.0, 2, "3", None, True], {"metrics": [{"name": "a", "value": 1.0, "labels": {"k": "v"}, "help": "", "timestamp": None}]},
]:
    compare(value)

chance = random.Random(2026)
def text():
    return "".join(chance.choice(["a", "b", " ", "\"", "\\", "\n", "\x00", "é", " ", "\U0001F600", "<", "&"]) for _ in range(chance.randrange(6)))
def value(depth):
    kind = chance.randrange(14 if depth > 0 else 8)
    if kind == 0: return None
    if kind == 1: return chance.choice([True, False])
    if kind == 2: return chance.randrange(-10**chance.randrange(1, 30), 10**chance.randrange(1, 30))
    if kind == 3: return chance.choice([0.0, -0.0, 1.5, 1e21, 1e-7, 5e-324, nan, inf, -inf, chance.random(), chance.uniform(-1e9, 1e9), float(chance.randrange(10**18))])
    if kind < 8: return text()
    if kind < 10: return [value(depth - 1) for _ in range(chance.randrange(4))]
    if kind == 10: return tuple(value(depth - 1) for _ in range(chance.randrange(4)))
    if kind < 13: return {text(): value(depth - 1) for _ in range(chance.randrange(4))}
    return {chance.choice([1, 2.5, True, None, 10**20, text()]): value(depth - 1) for _ in range(chance.randrange(4))}
for _ in range(int(target)):
    made = value(6)
    compare(made)
    # As json writes it with what JSON has no form for, in three layouts.
    for layout in ({}, {"separators": (",", ":"), "ensure_ascii": False}, {"indent": 1}):
        compare(read(json.dumps(made, **layout)))
metric("compared", value=compared)
`

// What the worker reads a request with, and writes an answer with, where
// json refuses for the depth is what json reads and writes of everything
// json does not refuse: the requests of every decoder, texts of every kind,
// numbers of every form, a table of what a script can leave — tuples, keys
// that are no strings, types of its own, what JSON has no form for and
// what json refuses, with json's own words — and thousands of values made
// at random.
func TestWhatReadsAndWritesADeepValueReadsAndWritesAsJSONDoes(t *testing.T) {
	requirePython(t)
	c := &model.Collector{Name: "request"}
	var lines []string
	for _, input := range handoverInputs(t) {
		for _, mode := range []string{"metrics", "data"} {
			line, err := pythonRequest(mode, handoverWalk, input.decoded, input.response, c)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(line))
		}
	}
	for _, text := range requestTexts {
		// As a value and not as a key, and whole or not at all: the marker
		// of a float that is not finite is rewritten wherever it stands,
		// and as a key or inside a longer string that is a request no
		// worker ever read.
		if _, whole := nonFiniteValues[text]; strings.Contains(text, "pue-nonfinite") && !whole {
			continue
		}
		r := &fetch.HTTPResponse{StatusCode: 200, Target: text, Body: []byte(text), Headers: http.Header{"X-Text": {text, "other"}}}
		line, err := pythonRequest("metrics", text, &decode.Decoded{Kind: "json", Data: map[string]any{"a": text, "z": []any{text, map[string]any{"k": text}}}, Raw: r.Body}, r, &model.Collector{Name: text})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	for _, f := range requestFloats {
		line, err := pythonRequest("data", "pass", &decode.Decoded{Kind: "json", Data: []any{f, -f, map[string]any{"v": f}}, Raw: []byte("{}")}, &fetch.HTTPResponse{Body: []byte("{}")}, c)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	lines = append(lines, "0", "-0", "7", "-12", "1e5", "1E+5", "-1.5e-3", "0.0", "-0.0", "1.0", "123456789012345678901234567890", "1e400", "-1e400", "NaN", "Infinity", "-Infinity", "true", "false", "null",
		`""`, `"é😀\n\"\\\/\b\f\r\t"`, `"\ud800"`, "[]", "{}", "[[]]", `{"a":{}}`, " [ 1 ,\t2 ,\r[ ] ] ", `{"a": 1, "a": 2, "b": {"a": 3, "a": [4]}}`, `{"": {"": ""}}`, `[{"a":[{"b":[1,2,{"c":null}]}]},"x",[[],{}]]`,
		`{"k": "a string that is a key", "a string that is a key": "k", "n": {"k": "k"}}`, strings.Repeat("[", 400)+`{"a":1.5}`+strings.Repeat("]", 400))
	generated := 3000
	if raceDetector || testing.Short() {
		generated = 300
	}
	body := strings.Join(lines, "\n")
	r := &fetch.HTTPResponse{StatusCode: 200, Target: strconv.Itoa(generated), Body: []byte(body), Headers: http.Header{}}
	run := workerCollector("differential", deepDifferential)
	set, err := executePython(context.Background(), "python3", deepDifferential, &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}, r, run)
	if err != nil {
		t.Fatalf("%.2000v", err)
	}
	// Each line is read and written, each of the table's values, more than
	// fifty, written, and each value made written, and read and written in
	// three layouts.
	if want := float64(2*len(lines) + 50 + 7*generated); len(set.Metrics) != 1 || set.Metrics[0].Value < want {
		t.Fatalf("%v, want at least %v values compared", set.Metrics, want)
	}
}

// The worker writes a deep answer as the worker that walked every answer
// wrote it where it could: given a recursion limit high enough for its walk
// and for json, it answered with data nested a few thousand deep, and that
// line is the line a worker writes now without any such limit, byte for
// byte, of lists, dicts, tuples, keys that are numbers, and a NaN inside.
func TestADeepAnswerIsTheLineAWorkerWithAHigherRecursionLimitWrote(t *testing.T) {
	h := newHandover(t)
	c := &model.Collector{Name: "handover"}
	input := handoverInputs(t)[1]
	compared := 0
	for _, shape := range []string{"[junk]", `{"k": junk}`, "(junk, 1.5)", `{7: [junk, "x"], None: float("nan")}`, `[float("inf"), {"a": (junk,), "b": []}]`} {
		for _, depth := range []int{300, 600, 1100, 2500} {
			if raceDetector && depth != 1100 {
				continue
			}
			build := fmt.Sprintf("junk = 1\nfor _ in range(%d):\n    junk = %s\ndata = junk\n", depth, shape)
			request, err := pythonRequest("data", build, input.decoded, input.response, c)
			if err != nil {
				t.Fatal(err)
			}
			raised, err := pythonRequest("data", "import sys\nsys.setrecursionlimit(30000)\n"+build, input.decoded, input.response, c)
			if err != nil {
				t.Fatal(err)
			}
			was, _, err := h.old.run(context.Background(), pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: 1 << 24, Scripts: "raised"}, raised, oracleTimeout)
			if err != nil {
				t.Fatal(err)
			}
			line, _, err := h.now.run(context.Background(), pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: 1 << 24}, request, oracleTimeout)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(was), `{"ok": true`) {
				t.Fatalf("%s nested %d times: the worker as it was answered %.200s even under a higher recursion limit", shape, depth, was)
			}
			if string(line) != string(was) {
				t.Fatalf("%s nested %d times: the answer is not the line it was under a higher recursion limit:\n     %.200s\nwas  %.200s", shape, depth, line, was)
			}
			compared++
		}
	}
	if now := h.now.PoolSnapshot(); now.Starts != 1 || len(now.Stops) != 0 || compared == 0 {
		t.Fatalf("%d workers started and %v stopped for %d answers, want one worker", now.Starts, now.Stops, compared)
	}
}

// A request nested deeper than any decoder makes a value, which no response
// is, is left to json.Marshal as it was: only what a decoder makes is
// carried deeper than JSON follows it. (What a script nests in its metrics
// is not carried either, and fails no walk: pythonflat_test.go.)
func TestARequestNestedDeeperThanADecodedValueIsLeftToJSON(t *testing.T) {
	c := workerCollector("junk", "pass")
	var data any = "leaf"
	for range decode.MaxDepth + 1 {
		data = []any{data}
	}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("{}")}
	line, err := pythonRequest("metrics", "pass", &decode.Decoded{Kind: "json", Data: data, Raw: r.Body}, r, c)
	want, wantErr := oraclePythonRequest("metrics", "pass", &decode.Decoded{Kind: "json", Data: data, Raw: r.Body}, r, c)
	if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() || string(line) != string(want) {
		t.Fatalf("data nested %d deep, deeper than a decoder makes it: error %v, was %v, and lines of %d and %d bytes", decode.MaxDepth+1, err, wantErr, len(line), len(want))
	}
}
