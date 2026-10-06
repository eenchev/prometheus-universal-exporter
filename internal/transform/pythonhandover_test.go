package transform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The whole hand-over, as it is and as it was (pythonoracle_test.go), with
// real workers: a script and a response are given to a worker running the
// launcher as it was, with the request json.Marshal wrote, and to one
// running the launcher as it is, with the request the encoder writes. The
// two requests are one line, the two answers are one line, byte for byte —
// so an answer is as long as it was, and passes or exceeds
// limits.max_output_bytes as it did — and the line is read into the series,
// the data or the error it was read into.

// handover is the two pools a script is run in, and the worker of each it
// runs in: one for all the scripts that leave the interpreter as they found
// it.
type handover struct {
	t        *testing.T
	old, now *PythonPool
	runs     int
}

func newHandover(t *testing.T) *handover {
	t.Helper()
	requirePython(t)
	h := &handover{t: t, old: oraclePythonPool(), now: newPythonPool()}
	t.Cleanup(h.old.shutdown)
	t.Cleanup(h.now.shutdown)
	return h
}

// handoverInput is a response as its decoder read it.
type handoverInput struct {
	name     string
	decoded  *decode.Decoded
	response *fetch.HTTPResponse
}

// handoverDecoded is body as the named decoder reads it.
func handoverDecoded(t *testing.T, kind, contentType, body string) handoverInput {
	t.Helper()
	c := &model.Collector{Name: "handover", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: kind}, Transform: model.TransformConfig{Type: "python"}}
	r := &fetch.HTTPResponse{StatusCode: 200, Target: "http://user:secret@target.example:8080/api?q=1", Body: []byte(body), Headers: http.Header{"Content-Type": {contentType}, "X-Mode": {"a", "b"}}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return handoverInput{name: kind, decoded: d, response: r}
}

// handoverInputs is a response of each decoder, each holding what the
// hand-over could get wrong: numbers of every form, keys out of order and
// written twice, text that is not UTF-8, and what JSON has no form for.
func handoverInputs(t *testing.T) []handoverInput {
	t.Helper()
	inputs := []handoverInput{
		handoverDecoded(t, "json", "application/json", `{"zeta": 1, "alpha": {"b": [1, 2.0, 2.5, 1e21, 1e-7, -0.0, 1e400, 123456789012345678901234567890, `+strings.Repeat("7", 5000)+`], "a": null, "B": true},
			"twice": 1, "twice": {"kept": "the later"}, "text": "quote \" backslash \\ <tag> & line\nbreak", "bytes": "caf`+"\xe9 \xff"+`", "empty": [{}, [], ""], "id": 1500000000000000001, "ratio": 0.30000000000000004}`),
		handoverDecoded(t, "json", "application/json", `[{"id": "a", "value": 1}, {"id": "b", "value": 2.5}]`),
		handoverDecoded(t, "yaml", "application/yaml", "when: 2024-06-01\nnan: .nan\ninf: .inf\nminus: -.inf\nlist: [1, 2.0, three, ~, true]\nnested:\n  z: 1\n  a: {k: v}\nbig: 123456789012345678901234567890\n"),
		handoverDecoded(t, "xml", "application/xml", `<?xml version="1.0"?><items><item id="a">1 &amp; 2</item><item id="b">3</item></items>`),
		handoverDecoded(t, "html", "text/html", "<!DOCTYPE html><html><body><table><tr><td>caf\xc3\xa9</td><td>1</td></tr></table></body></html>"),
		handoverDecoded(t, "csv", "text/csv", "id,region,value\na,eu,1\nb,us,\n\"c,d\",\"line\nbreak\",2.5\n"),
		handoverDecoded(t, "prometheus", "text/plain; version=0.0.4", "# HELP up Whether it is up.\n# TYPE up gauge\nup{job=\"a\"} 1 1700000000000\nup{job=\"b\"} NaN\n# TYPE lat histogram\nlat_bucket{le=\"0.5\"} 2\nlat_bucket{le=\"+Inf\"} 3\nlat_sum 1.5\nlat_count 3\n# TYPE q summary\nq{quantile=\"0.5\"} 0.25\nq_sum 2\nq_count 4\nplain 7\n"),
		handoverDecoded(t, "text", "text/plain", "Worker a CPU: 12%\nWorker b CPU: 34%\n\x00 and \xff\n"),
		handoverDecoded(t, "graphite", fetch.GraphiteContentType, fmt.Sprintf("servers.web01.cpu 0.5 %d\nservers.web02.cpu 0.25 %d\n", time.Now().Unix(), time.Now().Unix())),
	}
	headerless := &model.Collector{Name: "handover", Decoder: model.DecoderConfig{Type: "csv"}}
	no := false
	headerless.Response.CSV.Header = &no
	r := &fetch.HTTPResponse{NoStatus: true, Body: []byte("a,1\nb,2\n")}
	d, err := decode.Decode(r, headerless)
	if err != nil {
		t.Fatal(err)
	}
	return append(inputs, handoverInput{name: "csv without a header, from a file", decoded: d, response: r})
}

// run gives script the input in both pools, in the worker named state, and
// compares the two: the requests, the answers or the errors of the runs,
// and what the answer is read into under a limit of series. It returns the
// answer.
func (h *handover) run(mode, script string, input handoverInput, state string, maxOutput int, timeout time.Duration) []byte {
	h.t.Helper()
	h.runs++
	c := &model.Collector{Name: "handover"}
	want, wantErr := oraclePythonRequest(mode, script, input.decoded, input.response, c)
	request, err := pythonRequest(mode, script, input.decoded, input.response, c)
	if wantErr != nil || err != nil || string(request) != string(want) {
		h.t.Fatalf("%s, script %q: the request (%v) is not the request it was (%v):\n     %.400q\nwas  %.400q", input.name, script, err, wantErr, request, want)
	}
	spec := pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: maxOutput, Scripts: state}
	wasLine, _, wasErr := h.old.run(context.Background(), spec, want, timeout)
	line, _, err := h.now.run(context.Background(), spec, request, timeout)
	if (wasErr == nil) != (err == nil) || wasErr != nil && (wasErr.Error() != err.Error() || errors.Is(wasErr, errPythonTimeout) != errors.Is(err, errPythonTimeout) || errors.Is(wasErr, errPythonOutputTooLarge) != errors.Is(err, errPythonOutputTooLarge)) {
		h.t.Fatalf("%s, script %q: the run ended with %v, and with %v as it was", input.name, script, err, wasErr)
	}
	if string(line) != string(wasLine) {
		at := 0
		for at < len(line) && at < len(wasLine) && line[at] == wasLine[at] {
			at++
		}
		h.t.Fatalf("%s, script %q: the answers of %d and %d bytes differ at byte %d:\n     %.400q\nwas  %.400q", input.name, script, len(line), len(wasLine), at, line[max(0, at-60):], wasLine[max(0, at-60):])
	}
	if err == nil {
		for _, limit := range []int{0, 2} {
			compareAnswer(h.t, line, limit)
		}
	}
	return line
}

// A script that says what it was given: every value of data by its path,
// its type and its repr, in the order a script meets them, and the response,
// the target and the collector.
const handoverWalk = `
def walk(v, path):
    if isinstance(v, dict):
        for k in v:
            walk(v[k], path + "." + repr(k))
    elif isinstance(v, (list, tuple)):
        for i, x in enumerate(v):
            walk(x, path + "[%d]" % i)
    else:
        metric("leaf", value=len(metrics), labels={"path": path, "type": type(v).__name__, "repr": repr(v)[:300]})
walk(data, "data")
metric("response", value=len(response.text), labels={
    "status": repr(response.status_code), "headers": repr(list((response.headers or {}).items())),
    "one_string": str(response.text is response.body), "data_is_body": str(data is response.body),
    "target": target, "collector": collector, "content_type": repr(response.header("content-type")), "mode": repr(response.header("X-MODE", "none"))})
`

// What a script is given is what it was given, of every decoder: the walk
// names each value's type and repr, so an int that arrived as a float, a key
// in another place or a string that lost a byte would be another answer.
func TestPythonScriptIsGivenWhatItWasGiven(t *testing.T) {
	h := newHandover(t)
	for _, input := range handoverInputs(t) {
		line := h.run("metrics", handoverWalk, input, "", 1<<24, oracleTimeout)
		if !strings.Contains(string(line), `"ok": true`) || !strings.Contains(string(line), `"name": "leaf"`) {
			t.Fatalf("%s: %.300s", input.name, line)
		}
	}
}

// handoverScripts are transforms of every kind: what metric(...) takes and
// refuses, what a script appends to metrics itself, what it does to them
// afterwards, values JSON has no form for, containers and numbers of types
// of the script's own, and every way a script fails. What is answered
// otherwise than it was is not among them: a list or a dict where
// metric(...) takes one value, which its error names by its kind, and
// metrics that hold themselves (pythonflat_test.go). The largest makes 3,000
// metrics, and under the race detector, where reading them takes the test
// most of its time, 1,000: 6,000 values still, past the 4,096 from which a
// worker weighs what it writes (plain, in the launcher).
var handoverScripts = []string{
	`metric("up", value=1)`,
	`metric(name="jobs", type="counter", value=12.5, labels={"queue": "a", "state": "done"}, help="Jobs done.", timestamp=1700000000000)`,
	`
for i, item in enumerate([1, 2.5, True, False, "12", " 1.5 ", "1e3", float("nan"), float("inf"), float("-inf"), 10**30, -0.0, 5e-324, 1.7976931348623157e308]):
    metric("value", value=item, labels={"i": i})`,
	`metric("big", value=10**400)`, `metric("text", value="abc")`, `metric("none", value=None)`,
	`import decimal
metric("decimal", value=decimal.Decimal("1.5"))`,
	`
for i, v in enumerate(["text", 5, 0.5, 1e21, 1e-7, 1234567.0, -0.0, float("nan"), float("inf"), float("-inf"), True, False, None, 10**30, b"bytes", object, 1e20, 0.1 + 0.2]):
    metric("label", value=i, labels={"v": v, "i": str(i)})`,
	`metric("label", value=1, labels={"l": [1, 2]})`, `metric("label", value=1, labels={"d": {"a": 1}})`, `metric("label", value=1, labels={"t": (1, 2)})`, `metric("label", value=1, labels={"s": {1}})`,
	`metric("keys", value=1, labels={1: "int", None: "none", (1, 2): "tuple", 1.5: "float", True: "bool", "1": "text"})`, `metric("labels", value=1, labels=[("a", "b")])`, `metric("labels", value=1, labels="a=b")`,
	`
class Text(str): pass
import enum
class Level(enum.IntEnum):
    HIGH = 3
metric(Text("named"), value=Level.HIGH, labels={Text("k"): Text("v"), "level": Level.HIGH}, help=Text("help"), type=Text("counter"))`,
	`
import collections
metric("ordered", value=1, labels=collections.OrderedDict([("z", "1"), ("a", "2")]))
metric("default", value=2, labels=collections.defaultdict(str, b="x"))`,
	`metric("typed", type=None, value=1, help=None)`, `metric("typed", type="", value=1, help="")`, `metric("typed", type=5, value=1)`, `metric("typed", value=1, help=5)`, `metric("typed", type="histogram", value=1)`, `metric(5, value=1)`, `metric(None, value=1)`,
	`
for i, at in enumerate([None, 17, 17.9, "17", " 18 ", True, -5, 0, 1e15, 2**62]):
    metric("at", value=i, timestamp=at, labels={"i": i})`,
	`metric("at", value=1, timestamp=float("nan"))`, `metric("at", value=1, timestamp=float("inf"))`, `metric("at", value=1, timestamp=1e19)`, `metric("at", value=1, timestamp=-1e19)`, `metric("at", value=1, timestamp="soon")`,
	// Appended by hand.
	`metrics.append({"name": "a", "value": 1})`, `metrics.append({"name": "a", "value": 1, "labels": {"k": "v", "n": 5, "f": 0.5, "b": True, "none": None, "nan": float("nan"), "big": 10**30}, "type": "counter", "help": "By hand.", "timestamp": 17.5})`,
	`metrics.append({"name": "a"})`, `metrics.append({"value": 1})`, `metrics.append({})`, `metrics.append({"name": "a", "value": None})`, `metrics.append({"name": "a", "value": "x"})`, `metrics.append({"name": "a", "value": "12"})`,
	`metrics.append({"name": "a", "value": True})`, `metrics.append({"name": "a", "value": float("nan")})`, `metrics.append({"name": "a", "value": float("-inf"), "timestamp": float("nan")})`, `metrics.append({"name": "a", "value": [1, 2]})`,
	`metrics.append({"name": 5, "value": 1})`, `metrics.append({"name": 5.0, "value": 1})`, `metrics.append({"name": ["a"], "value": 1})`, `metrics.append({"name": "a", "value": 1, "labels": [1]})`, `metrics.append({"name": "a", "value": 1, "labels": {"l": [1]}})`,
	`metrics.append({"name": "a", "value": 1, "type": 5})`, `metrics.append({"name": "a", "value": 1, "help": {}})`, `metrics.append({"name": "a", "value": 1, "timestamp": "x"})`, `metrics.append({"name": "a", "value": 1, "timestamp": 10**30})`,
	`metrics.append({"name": "a", "value": 1, "extra": {"deep": [1, (2, 3), {"x": None}], "nan": float("nan")}})`, `metrics.append({"name": "a", "value": 1, 1: "int key", None: "none key", True: "bool key", 1.5: "float key"})`,
	`metrics.append({"name": "a", "value": 1, (1, 2): "tuple key"})`, `metrics.append({"name": "a", "value": 1, float("nan"): "nan key"})`, `metrics.append({"name": "a", "value": 1, "junk": {1, 2}})`, `metrics.append({"name": "a", "value": 1, "junk": b"bytes"})`,
	`metrics.append({"name": "a", "value": 1, "junk": object()})`, `metrics.append(5)`, `metrics.append("text")`, `metrics.append(None)`, `metrics.append([1, 2])`, `metrics.append(("name", 1))`, `metrics.append(True)`, `metrics.append(1.5)`, `metrics.append(float("nan"))`,
	`metrics.append({"name": "ok", "value": 1}); metrics.append(5); metrics.append({"name": 5})`, `metrics.extend([{"name": "a", "value": i} for i in range(5)])`,
	// Changed after metric(...) made them.
	`metric("a", value=1); metrics[0]["value"] = float("nan")`, `metric("a", value=1, labels={"k": "v"}); metrics[0]["labels"]["x"] = [1]`, `metric("a", value=1); metrics[0]["junk"] = {"s": {1}}`, `metric("a", value=1); del metrics[0]["name"]`,
	`metric("a", value=1); metric("b", value=2); metrics.reverse()`, `metric("a", value=1); metrics.clear()`, `metric("a", value=1); metrics[0]["labels"] = None`, `metric("a", value=1); metrics[0]["value"] = "2.5"`, `metric("a", value=1); metrics[0] = metrics[0]["labels"]`,
	`metric("a", value=1); metrics.append(metrics[0])`,
	// Containers and numbers of a script's own types.
	`
import collections
metrics.append(collections.OrderedDict([("value", 1), ("name", "ordered")]))
metrics.append(collections.defaultdict(list, name="default", value=2))
Point = collections.namedtuple("Point", "name value")
metrics.append(Point("p", 1))`,
	// Each alone, so that what one of them makes of the answer is not
	// hidden by another that sends it through wire.
	`
class Rows(list):
    def __iter__(self): return iter(["replaced"])
metrics.append({"name": "rows", "value": 1, "junk": Rows([1, 2, 3])})`,
	`
class Pair(tuple):
    def __iter__(self): return iter(["replaced"])
metrics.append({"name": "pair", "value": 1, "junk": Pair((1, 2))})`,
	`
class Half(dict):
    def items(self): return [("name", "half"), ("value", 0.5)]
metrics.append(Half(name="whole", value=1))`,
	`
class Odd(float): pass
metrics.append({"name": "odd", "value": Odd(1.5), "labels": {"v": Odd(2.5)}})`,
	`
class Odd(float): pass
metrics.append({"name": "odd", "value": Odd("inf"), "labels": {"v": Odd("nan")}})`,
	`
class Odd(float):
    def __repr__(self): return "odd"
metrics.append({"name": "odd", "value": Odd(1.5)})`,
	`
class Count(int):
    def __repr__(self): return "count"
    def __str__(self): return "count"
metrics.append({"name": "count", "value": Count(3), "labels": {"n": Count(4)}})`,
	`
import enum, fractions
class Level(enum.IntEnum):
    LOW = 1
metrics.append({"name": "level", "value": Level.LOW, "labels": {"l": Level.LOW}})
metrics.append({"name": "fraction", "value": fractions.Fraction(1, 2)})`,
	// Every way a script fails, and what it prints.
	"metric('a', value=1)\nfor x in range(3):\n    y = x +\n", `raise RuntimeError("no")`, `fail("the target said no")`, `fail(5)`, `import sys; sys.exit(3)`, `sys.exit("bye")`, `raise SystemExit`, `raise KeyboardInterrupt`, `assert False, "asserted"`,
	`
def rate(row):
    return row["requests"] / row["seconds"]
metric(name="rate", value=rate({"requests": 5, "seconds": 0}))`,
	`
def f(n):
    if n == 0:
        raise ValueError("at the bottom")
    return f(n - 1)
f(9)`,
	`
try:
    {}["missing"]
except KeyError as e:
    raise RuntimeError("wrapped") from e`,
	`
try:
    1 / 0
except ZeroDivisionError:
    metric("in", value="x")`,
	`
class Loud(Exception):
    def __str__(self): raise RuntimeError("no text")
raise Loud()`,
	`[metric("m", value=v) for v in [1, 2, "x"]]`, `raise ValueError("caf" + chr(233) + " " + chr(0x1F600) + chr(0))`, `def f(): return f()
f()`, `import socket`, `open("/etc/passwd")`, `import os; os.system("true")`,
	`print("printed"); metric("a", value=1)`, `print("x" * 5000); metric("a", value=1)`, `import sys; print("to stderr", file=sys.stderr); print(chr(233), chr(0), chr(0x1F600)); metric("a", value=1)`, `print("then failed"); raise RuntimeError("after printing")`,
	fmt.Sprintf(`for i in range(%d): metric("many", value=i, labels={"i": i, "mod": str(i %% 7)}, help="Many of them.")`, alloctest.UnlessRaced(3000, 1000)),
	`for i in range(500): metric("nan last", value=i)
metric("nan last", value=float("nan"))`,
}

// Scripts of every kind answer what they answered, and their answers are
// read into what they were read into: the series, in their order, with
// their types, help, labels and timestamps, or the error, with its
// traceback.
func TestPythonTransformsAnswerWhatTheyAnswered(t *testing.T) {
	h := newHandover(t)
	input := handoverInputs(t)[0]
	failed, answered := 0, 0
	for _, script := range handoverScripts {
		if line := h.run("metrics", script, input, "", 1<<24, oracleTimeout); strings.HasPrefix(string(line), `{"ok": false`) {
			failed++
		} else {
			answered++
		}
	}
	if failed < 20 || answered < 40 {
		t.Fatalf("%d scripts failed and %d answered: the table should hold more of each", failed, answered)
	}
	// One worker of each pool served them all: a script that fails leaves
	// its worker in service, as it did.
	if old, now := h.old.PoolSnapshot(), h.now.PoolSnapshot(); old.Starts != 1 || now.Starts != 1 {
		t.Fatalf("%d workers started as they were and %d as they are, want 1 of each", old.Starts, now.Starts)
	}
}

// handoverPreScripts are pre-scripts: what they leave in data is what the
// transform then reads. The largest leaves 2,000 items, and under the race
// detector, where reading them back takes the test most of its time, 1,200:
// 4,800 values still, past the 4,096 from which a worker weighs what it
// writes (plain, in the launcher).
var handoverPreScripts = []string{
	`pass`, `data = None`, `del data`, `data = "text"`, `data = 5`, `data = 1.5`, `data = True`, `data = response.text`, `data = {"was": data}`, `data = [data, data]`,
	`data = [1, 2.0, "x", None, True, float("nan"), float("inf"), float("-inf"), 10**30, -(10**30), 2**63, 2**63 - 1, -(2**63), 2**53 + 1, 1.0, -0.0, 1e21, 1e-7, 5e-324, 0.1 + 0.2, 10**4000]`,
	`data = {"b": 1, "a": {"z": [], "y": {}, "t": ()}, 1: "int key", None: "none key", True: "bool key", 1.5: "float key", "1": "text key"}`, `data = (1, 2, (3, [4, (5,)]))`,
	`data = {"s": {1, 2}}`, `data = b"bytes"`, `data = {"k": object()}`, `data = {(1, 2): 3}`, `data = {float("nan"): 1}`, `data = 10**5000`, `import decimal; data = {"d": decimal.Decimal("1.5")}`,
	`data = {"text": "quote \" backslash \\ <tag> & " + chr(0) + chr(233) + chr(0x2028) + chr(0x1F600) + chr(0xD800), "marker": chr(0) + "pue-nonfinite:NaN" + chr(0)}`,
	`
import collections
data = collections.OrderedDict([("z", 1), ("a", collections.defaultdict(int, k=2))])`,
	`
class Rows(list):
    def __iter__(self): return iter(["replaced"])
data = {"rows": Rows([1, 2])}`,
	`
class Half(dict):
    def items(self): return [("half", 0.5)]
data = [Half(whole=1)]`,
	`
class Odd(float): pass
data = {"odd": Odd(2.5)}`,
	`
class Odd(float): pass
data = {"odd": Odd(2.5), "nan": Odd("nan")}`,
	`
if isinstance(data, dict):
    data["added"] = float("nan")
    data.pop("zeta", None)
elif isinstance(data, list):
    data.append({"added": float("-inf")})
else:
    data = {"text": data, "length": len(data)}`,
	`
if isinstance(data, dict) and "metrics" in data:
    for series in data["metrics"]:
        series["labels"]["site"] = "a"
        if "value" in series: series["value"] = series["value"] * 2
    data["metrics"].append({"name": "added", "value": 1})`,
	fmt.Sprintf(`data = [{"id": i, "v": i * 0.5, "tags": ["a", "b"]} for i in range(%d)]`, alloctest.UnlessRaced(2000, 1200)),
	`raise RuntimeError("no data")`, "data = {\n", `print("printed by the pre-script"); data = {"ok": 1}`,
}

// Pre-scripts leave what they left, for the response of every decoder: the
// answer is the answer it was, and data is read back into the values it
// was, an int an int and a float a float. Under the race detector each
// response is given to a third of the pre-scripts and each pre-script a
// third of the responses (pairTaken).
func TestPythonPreScriptsLeaveWhatTheyLeft(t *testing.T) {
	h := newHandover(t)
	for i, input := range handoverInputs(t) {
		for j, script := range handoverPreScripts {
			if !pairTaken(i, j, 3) {
				continue
			}
			h.run("data", script, input, "", 1<<24, oracleTimeout)
		}
	}
	if old, now := h.old.PoolSnapshot(), h.now.PoolSnapshot(); old.Starts != 1 || now.Starts != 1 {
		t.Fatalf("%d workers started as they were and %d as they are, want 1 of each", old.Starts, now.Starts)
	}
}

// runDeep gives a script the input in both pools, as run does, and
// compares the two but where the worker as it was failed for the depth of
// what the script left in data or appended to metrics, with a
// RecursionError: there the worker carries the data, or says how deep data
// may nest, and writes the metrics without what no metric has, and was says
// the line is not compared. It returns the answer.
func (h *handover) runDeep(mode, script string, input handoverInput, state string) (line []byte, was bool) {
	h.t.Helper()
	c := &model.Collector{Name: "handover"}
	request, err := pythonRequest(mode, script, input.decoded, input.response, c)
	if err != nil {
		h.t.Fatal(err)
	}
	spec := pythonSpec{Path: "python3", Collector: c.Name, MaxOutput: 1 << 24, Scripts: state}
	wasLine, _, wasErr := h.old.run(context.Background(), spec, request, oracleTimeout)
	line, _, err = h.now.run(context.Background(), spec, request, oracleTimeout)
	if wasErr != nil || err != nil {
		h.t.Fatalf("%s, script %q: the run ended with %v, and with %v as it was", input.name, script, err, wasErr)
	}
	if strings.HasPrefix(string(wasLine), `{"ok": false, "error": "RecursionError: maximum recursion depth exceeded`) {
		return line, true
	}
	if string(line) != string(wasLine) {
		h.t.Fatalf("%s, script %q: the answer is not the answer it was:\n     %.300q\nwas  %.300q", input.name, script, line, wasLine)
	}
	compareAnswer(h.t, line, 0)
	return line, false
}

// A transform's answer nested deeper than a worker could walk failed there,
// with a RecursionError: what is nested deeper than the worker looks
// through is left to the walk it replaced, under the recursion limit as it
// is, also when a script changed it. Where that walk answered, the answer
// is the line it was, and where it failed for the depth, the answer is the
// metrics without what no metric has, the items of a list under a key of
// the script's own. What a pre-script leaves in data ended there too, and
// is carried instead: where the worker as it was answered, the answer is
// the line it was, and where it failed for the depth, the answer is the
// data, as deep as it is. Data that holds itself fails saying how deep data
// may nest, where it failed as a recursion without end, and data whose
// values are many times the same list is the line it was.
func TestPythonAnswersNestedDeepEndAsTheyEndedButForData(t *testing.T) {
	h := newHandover(t)
	input := handoverInputs(t)[1]
	nested := `
junk = %[2]s
for _ in range(%[1]d):
    junk = %[3]s
`
	failed, answered, carried := 0, 0, 0
	// metrics runs a transform that appends a metric with junk under a key
	// of its own, and counts it failed where the worker as it was failed for
	// its depth: there the answer is the metric with the items of junk, a
	// list or a dict of one, left out.
	metrics := func(build, state, cut string) {
		t.Helper()
		line, was := h.runDeep("metrics", build+`metrics.append({"name": "deep", "value": 1, "junk": junk})`, input, state)
		if !was {
			answered++
			return
		}
		if want := `{"ok": true, "log": "", "metrics": [{"name": "deep", "value": 1, "junk": ` + cut + `}]}`; string(line) != want {
			t.Fatalf("a metric with junk the worker as it was failed on for its depth is answered %.300s, want %s", line, want)
		}
		failed++
	}
	// data runs a pre-script that leaves junk, which is nested deep times,
	// and counts it carried where the worker as it was failed for its depth.
	data := func(build, state string, deep int) {
		t.Helper()
		line, was := h.runDeep("data", build+`data = junk`, input, state)
		if !was {
			answered++
			return
		}
		out, err := pythonResult(&model.Collector{Name: "handover"}, "pre-script", oracleTimeout, line, nil)
		if err != nil {
			t.Fatalf("data nested %d deep, which failed for its depth: %.300v", deep, err)
		}
		if got := nestedDepth(pythonData(out)); got != deep {
			t.Fatalf("data nested %d deep is read back nested %d deep", deep, got)
		}
		carried++
	}
	// The depths lie around the two that matter under the recursion limit
	// of 1000 an interpreter starts with: the one up to which the worker
	// looks through an answer itself, 490, and the one at which wire fails,
	// a little under 500 or under 1000 by the interpreter's release. The
	// third shape nests two deep for each step, around an empty list.
	single := []int{1, 200, 480, 487, 488, 489, 490, 491, 492, 494, 496, 498, 500, 600, 900, 994, 995, 996, 997, 1200}
	double := []int{1, 100, 240, 243, 244, 245, 246, 247, 249, 250, 300, 450, 496, 497, 498, 499, 600}
	for _, shape := range []struct {
		leaf, step, cut string
		depths          []int
		levels          func(int) int
	}{
		{"1", "[junk]", "[null]", single, func(steps int) int { return steps }}, {"1.5", `{"k": junk}`, `{"k": null}`, single, func(steps int) int { return steps }},
		{"[]", `[(junk,)]`, "[null]", double, func(steps int) int { return 2*steps + 1 }},
	} {
		for _, depth := range shape.depths {
			build := fmt.Sprintf(nested, depth, shape.leaf, shape.step)
			metrics(build, "", shape.cut)
			data(build, "", shape.levels(depth))
		}
	}
	if failed < 4 || answered < 40 || carried < 4 {
		t.Fatalf("%d transforms were answered where they failed for their depth, %d answers were as they were and %d were data carried where it failed: the depths should lie on both sides of the limit", failed, answered, carried)
	}
	// Under a recursion limit a script set, which lasts as long as its
	// worker: each limit in a worker of its own.
	for _, limit := range []int{60, 100, 3000} {
		failed, answered, carried = 0, 0, 0
		state := fmt.Sprintf("recursion limit %d", limit)
		set := fmt.Sprintf("import sys\nsys.setrecursionlimit(%d)\n", limit)
		for _, depth := range []int{2, limit/2 - 13, limit/2 - 12, limit/2 - 11, limit/2 - 10, limit/2 - 9, limit / 2, limit - 6, limit - 5, limit - 4, limit - 3, limit + 10} {
			build := set + fmt.Sprintf(nested, depth, "1", "[junk]")
			data(build, state, depth)
			if depth < limit/2 {
				metrics(build, state, "[null]")
			}
		}
		if carried == 0 || answered == 0 {
			t.Fatalf("under a recursion limit of %d, %d answers were data carried where it failed for its depth and %d were as they were", limit, carried, answered)
		}
	}
	for _, script := range []string{`a = []; a.append(a); data = a`, `d = {}; d["self"] = d; data = {"d": d}`} {
		line, was := h.runDeep("data", script, input, "")
		if want := fmt.Sprintf(`{"ok": false, "error": "RecursionError: data is nested more than %d deep, or a list or a dict in it holds itself;`, decode.MaxDepth); !was || !strings.HasPrefix(string(line), want) {
			t.Fatalf("script %q: %.300s, want the answer to start %s where it was a recursion without end", script, line, want)
		}
	}
	for _, script := range []string{
		`a = [1]; data = (a, a, [a, a])`,
		"x = [1.5, 'leaf']\nfor _ in range(12):\n    x = [x, x]\ndata = x", "x = [float('nan')]\nfor _ in range(10):\n    x = [x, x]\ndata = x",
	} {
		h.run("data", script, input, "", 1<<24, oracleTimeout)
	}
}

// An answer is as long as it was, so limits.max_output_bytes is passed by
// the answers that passed it and exceeded by those that exceeded it: an
// answer of exactly the limit is taken, and one a byte longer ends the run
// with the output limit's error and its worker, as it did. And a script
// that does not end is ended by its timeout, as it was.
func TestPythonAnswerMeetsItsLimitsWhereItMetThem(t *testing.T) {
	h := newHandover(t)
	input := handoverInputs(t)[1]
	for _, script := range []string{
		`for i in range(40): metric("m", value=i, labels={"i": i})`,
		`print("x" * 6000); metrics.extend({"name": "m", "value": float("nan"), "junk": (i, [i])} for i in range(30))`,
	} {
		length := len(h.run("metrics", script, input, "", 1<<24, oracleTimeout))
		if length < 1000 {
			t.Fatalf("an answer of %d bytes", length)
		}
		for _, limit := range []int{length, length - 1} {
			spec := pythonSpec{Path: "python3", Collector: "handover", MaxOutput: limit, Scripts: fmt.Sprintf("limit %d of %d", limit, length)}
			request, err := pythonRequest("metrics", script, input.decoded, input.response, &model.Collector{Name: "handover"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, wasErr := h.old.run(context.Background(), spec, request, oracleTimeout)
			line, _, err := h.now.run(context.Background(), spec, request, oracleTimeout)
			if errors.Is(err, errPythonOutputTooLarge) != (limit < length) || errors.Is(wasErr, errPythonOutputTooLarge) != (limit < length) {
				t.Fatalf("an answer of %d bytes under a limit of %d: %v, and %v as it was (%.100s)", length, limit, err, wasErr, line)
			}
		}
	}
	// The script never ends, so its timeout is what ends each run, however
	// long the machine takes over starting the workers.
	spec := pythonSpec{Path: "python3", Collector: "handover", MaxOutput: 1 << 20, Scripts: "endless"}
	request, err := pythonRequest("metrics", "while True:\n    metric('a', value=1)\n    metrics.clear()\n", input.decoded, input.response, &model.Collector{Name: "handover"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, wasErr := h.old.run(context.Background(), spec, request, 300*time.Millisecond)
	_, _, err = h.now.run(context.Background(), spec, request, 300*time.Millisecond)
	if !errors.Is(err, errPythonTimeout) || !errors.Is(wasErr, errPythonTimeout) {
		t.Fatalf("%v, and %v as it was", err, wasErr)
	}
}

// A worker writes an answer that is plain — the metrics metric(...) made,
// the data a pre-script left, of lists, dicts, text and finite numbers —
// without walking it through wire, and an answer that is not, one with a
// NaN, through wire as before. The script counts the calls of wire by
// putting a counter in its place, in the worker's own module, and says in
// its next run how many the answer before took: walking every answer, the
// worker made 37,000 calls for five thousand metrics, which cost more than
// the script.
func TestPythonWorkerWalksOnlyAnswersThatNeedIt(t *testing.T) {
	requirePython(t)
	c := workerCollector("walks", `
import __main__
if not hasattr(__main__, "wire_calls"):
    __main__.wire_calls = 0
    walk = __main__.wire
    def counting(v):
        __main__.wire_calls += 1
        return walk(v)
    __main__.wire = counting
metric("wire_calls", value=__main__.wire_calls)
__main__.wire_calls = 0
for i in range(100):
    metric("m", value=float(data) if i == 99 else i, labels={"i": i, "mod": str(i % 7)})
`)
	calls := func(value string) float64 {
		t.Helper()
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(value), Headers: http.Header{}}
		set, err := executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: value, Raw: r.Body}, r, c)
		if err != nil || len(set.Metrics) != 101 {
			t.Fatalf("%v, %v", set, err)
		}
		return workerMetricValue(t, set, "wire_calls")
	}
	calls("1")
	if after := calls("nan"); after != 0 {
		t.Fatalf("a plain answer of 101 metrics, and the line that says its request was taken, took %v calls of wire, want none", after)
	}
	if after := calls("1"); after < 800 {
		t.Fatalf("an answer with a NaN took %v calls of wire, want one for each of its values, some 800", after)
	}
	if after := calls("1"); after != 0 {
		t.Fatalf("the plain answer after it took %v calls of wire, want none", after)
	}
	if PythonWorkers().Snapshot(c.Name).Starts != 1 {
		t.Fatal("the runs were not one worker's")
	}
}

// metric(...) asks label_text for the text of a label that is not a string,
// and takes one that is as it is, which is what label_text returns for it:
// the script counts the calls by putting a counter in its place. Asking for
// every label made a third of what a call of metric(...) costs. The label
// given as the empty string is one of those taken as it is, and is left off
// the series where the answer is read (pythonemptylabel_test.go).
func TestPythonMetricTakesAStringLabelAsItIs(t *testing.T) {
	requirePython(t)
	c := workerCollector("labels", `
import __main__
asked = []
text = __main__.label_text
def counting(name, v):
    asked.append(name)
    return text(name, v)
__main__.label_text = counting
try:
    class Text(str): pass
    metric("m", value=1, labels={"a": "x", "b": 5, "c": None, "d": True, "e": Text("sub"), "f": "", 7: "seven", "g": 0.5})
finally:
    __main__.label_text = text
metric("asked", value=len(asked), labels={"names": ",".join(sorted(map(str, asked)))})
`)
	set, err := runWorkerScript(t, c)
	if err != nil || len(set.Metrics) != 2 {
		t.Fatalf("%v, %v", set, err)
	}
	want := map[string]string{"a": "x", "b": "5", "d": "true", "e": "sub", "7": "seven", "g": "0.5"}
	if got := set.Metrics[0].Labels; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	if asked := set.Metrics[1]; asked.Value != 5 || asked.Labels["names"] != "b,c,d,e,g" {
		t.Fatalf("label_text was asked for %v labels, %q, want the 5 that are no plain string: b,c,d,e,g", asked.Value, asked.Labels["names"])
	}
}
