package transform

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The request to a worker is written by pythonEncoder (pythonrequest.go)
// rather than by json.Marshal. These tests compare the two: for every
// response and every data, the line is the line it was, byte for byte, or
// the error the error it was (oraclePythonRequest).

// requestTexts are strings of every kind a request holds, as a key, a value,
// a header, a body, a target or a script: what is written as it is, what is
// escaped, what is not UTF-8, and what is, or nearly is, the marker of a
// float that is not finite, which the request has always rewritten wherever
// it stood.
var requestTexts = []string{
	"", "plain", "id", "value", "a", "b", "items", "with space", `a "quoted" word and a \ backslash`, "<html>&amp;</html>",
	"line\nbreak\ttab\rreturn\bbell\ffeed", "\x00", "\x01\x02\x1f\x7f", "ünïcode 日本 \U0001F600", "\u2028 and \u2029", "\ufffd",
	"\xff\xfe not UTF-8", "\xe2\x82", "caf\xe9", "\xed\xa0\x80", "\xf4\x90\x80\x80", "a\xc3", "\xc3(",
	nonFiniteMarker + "NaN\x00", nonFiniteMarker + "+Inf\x00", nonFiniteMarker + "-Inf\x00",
	nonFiniteMarker + "NaN", nonFiniteMarker + "nan\x00", "x" + nonFiniteMarker + "NaN\x00", nonFiniteMarker + "NaN\x00x",
	`x"` + nonFiniteMarker + "NaN\x00", `"` + nonFiniteMarker + `+Inf` + "\x00" + `"`, `\u0000pue-nonfinite:NaN\u0000`, `"\u0000pue-nonfinite:NaN\u0000"`,
	"NaN", "Infinity", "-Infinity", "null", "true", "1e400", strings.Repeat("long ", 300),
}

// requestFloats are floats of every form json.Marshal writes: whole ones,
// which it writes without a point, those at the two ends where the exponent
// form starts, and those that are not finite.
var requestFloats = []float64{
	0, math.Copysign(0, -1), 1, -1, 1.5, 0.1, 100, 1e6, 123456789, 1e20, 1e21, 1.5e21, -1e21, 999999999999999900000, 1e-6, 0.000001234, 1e-7, 9.5e-7, 1e-9, 1.25e-10,
	1e22, 1e100, 1e-100, 1e300, 5e-324, math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, 9007199254740993, 1.7976931348623157e308,
	math.NaN(), math.Inf(1), math.Inf(-1), 0.30000000000000004, 2.5e-5, 123456.789e3,
}

// requestGenerator makes random data, as decoders make it and otherwise.
type requestGenerator struct {
	random *rand.Rand
	// foreign says values of types no decoder makes are among the data.
	foreign bool
}

func (g *requestGenerator) text() string {
	if g.random.IntN(4) == 0 {
		b := make([]byte, g.random.IntN(12))
		for i := range b {
			b[i] = byte(g.random.IntN(256))
		}
		return string(b)
	}
	return requestTexts[g.random.IntN(len(requestTexts))]
}

func (g *requestGenerator) value(depth int) any {
	kinds := 12
	if depth <= 0 {
		kinds = 8
	}
	switch kind := g.random.IntN(kinds); kind {
	case 0:
		return nil
	case 1:
		return g.random.IntN(2) == 0
	case 2:
		return []int{0, 1, -1, 255, 256, 1 << 31, math.MaxInt64, math.MinInt64, g.random.IntN(1 << 40)}[g.random.IntN(9)]
	case 3:
		if g.random.IntN(3) == 0 {
			return g.random.NormFloat64() * math.Pow(10, float64(g.random.IntN(60)-30))
		}
		return requestFloats[g.random.IntN(len(requestFloats))]
	case 4, 5:
		return g.text()
	case 6:
		switch g.random.IntN(4) {
		case 0:
			return (*big.Int)(nil)
		case 1:
			return new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), uint(g.random.IntN(300))))
		}
		return new(big.Int).Lsh(big.NewInt(int64(g.random.IntN(1000))), uint(g.random.IntN(200)))
	case 7:
		if !g.foreign {
			return g.random.IntN(100)
		}
		return []any{
			int64(7), uint64(math.MaxUint64), float32(1.5), json.Number("12.50"), json.Number(""), []string{"a", "<b>"}, []string(nil), map[string]string{"k": "v"},
			map[string]int(nil), []float64{1, math.NaN()}, map[any]any{1: "a"}, struct{ A int }{1}, []byte("bytes"), [2]int{1, 2}, &struct{}{}, int8(-3), uint(9),
		}[g.random.IntN(17)]
	case 8, 9:
		switch g.random.IntN(8) {
		case 0:
			return []any(nil)
		case 1:
			return []any{}
		}
		list := make([]any, g.random.IntN(5))
		for i := range list {
			list[i] = g.value(depth - 1)
		}
		return list
	default:
		switch g.random.IntN(8) {
		case 0:
			return map[string]any(nil)
		case 1:
			return map[string]any{}
		}
		object := make(map[string]any)
		for range g.random.IntN(6) {
			object[g.text()] = g.value(depth - 1)
		}
		return object
	}
}

// response is a response of some kind: with no headers, none, or several,
// each of some values, a status of every kind, and a body.
func (g *requestGenerator) response() *fetch.HTTPResponse {
	r := &fetch.HTTPResponse{Target: g.text(), Body: []byte(g.text())}
	switch g.random.IntN(5) {
	case 0:
		r.StatusCode = 200
	case 1:
		r.NoStatus = true
	case 2:
		code := g.random.IntN(17)
		r.GRPCCode = &code
	case 3:
		r.StatusCode = 100 + g.random.IntN(500)
	}
	switch g.random.IntN(4) {
	case 0:
		r.Headers = http.Header{}
	case 1, 2:
		r.Headers = http.Header{}
		for range g.random.IntN(4) {
			var values []string
			for range g.random.IntN(3) {
				values = append(values, g.text())
			}
			r.Headers[g.text()] = values
		}
	}
	return r
}

// compareRequest writes the request for d and r with a new encoder, with
// one that has written requests before, and as it was written, and reports
// any difference.
func compareRequest(t *testing.T, used *pythonEncoder, mode, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) {
	t.Helper()
	want, wantErr := oraclePythonRequest(mode, script, d, r, c)
	got, gotErr := pythonRequest(mode, script, d, r, c)
	again, againErr := used.request(mode, script, d, r, c)
	for _, is := range []struct {
		line []byte
		err  error
	}{{got, gotErr}, {again, againErr}} {
		if (wantErr == nil) != (is.err == nil) || wantErr != nil && wantErr.Error() != is.err.Error() {
			t.Fatalf("data %.300s of kind %s: error %v, was %v", fmt.Sprintf("%#v", d.Data), d.Kind, is.err, wantErr)
		}
		if string(is.line) != string(want) {
			at := 0
			for at < len(is.line) && at < len(want) && is.line[at] == want[at] {
				at++
			}
			t.Fatalf("data %.300s of kind %s: the lines of %d and %d bytes differ at byte %d:\n     %.300q\nwas  %.300q", fmt.Sprintf("%#v", d.Data), d.Kind, len(is.line), len(want), at, is.line[max(0, at-40):], want[max(0, at-40):])
		}
	}
}

// Every value a decoder makes, at every place of a request: each text as the
// data, a key, a header, the body, the target, the script and the
// collector's name; each float; integers of every size; a list and an
// object that are nil and that are empty; the body as the data, which is
// sent once; and the data of every decoder.
func TestPythonRequestIsTheLineItWas(t *testing.T) {
	c := &model.Collector{Name: "request"}
	used := &pythonEncoder{}
	headers := http.Header{"Content-Type": {"application/json"}, "X-Mode": {"a", "b"}, "X-Empty": {}, "X-None": nil}
	for _, text := range requestTexts {
		for _, data := range []any{text, map[string]any{text: text, "z": []any{text}}, []any{text, map[string]any{"k": text}}} {
			r := &fetch.HTTPResponse{StatusCode: 200, Target: text, Body: []byte(text), Headers: http.Header{text: {text, "other"}}}
			compareRequest(t, used, "metrics", text, &decode.Decoded{Kind: "json", Data: data, Raw: r.Body}, r, &model.Collector{Name: text})
			compareRequest(t, used, "data", "data = 1", &decode.Decoded{Kind: "html", Data: "parsed", Raw: []byte(text)}, r, c)
			compareRequest(t, used, text, "pass", &decode.Decoded{Kind: "text", Data: text + "x", Raw: r.Body}, &fetch.HTTPResponse{Body: []byte(text + "x"), Headers: headers}, c)
		}
	}
	plain := &fetch.HTTPResponse{StatusCode: 200, Target: "http://target.example/api", Body: []byte(`{"a":1}`), Headers: headers}
	values := []any{
		nil, true, false, 0, -1, math.MaxInt64, math.MinInt64, "text", (*big.Int)(nil), big.NewInt(0), new(big.Int).Lsh(big.NewInt(1), 200), new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(3), 4000)),
		[]any(nil), []any{}, map[string]any(nil), map[string]any{}, []any{nil, []any{}, map[string]any{}}, map[string]any{"b": 1, "a": 2, "ab": 3, "B": 4, "": 5, "é": 6, "a\x00": 7, "\xff": 8},
		map[string]any{"items": []any{map[string]any{"id": "a", "value": 1.5, "tags": []any{"x", "y"}}, map[string]any{"id": "b", "value": nil}}},
		// What a decoder does not make is written as it was.
		int64(5), uint64(5), float32(2.5), json.Number("1.50"), []string{"a"}, map[string]string{"a": "b"}, []float64{math.NaN()}, map[string]any{"deep": []any{map[string]any{"x": int32(1)}}},
	}
	for _, f := range requestFloats {
		values = append(values, f, []any{f, -f}, map[string]any{"v": f})
	}
	for _, data := range values {
		compareRequest(t, used, "metrics", "pass", &decode.Decoded{Kind: "json", Data: data, Raw: plain.Body}, plain, c)
	}
	// Lists and objects nested as deep as the encoder follows them, and
	// deeper, where json.Marshal writes them, up to the depth at which a
	// Go release that bounds the nesting refuses them.
	for _, depth := range []int{100, pythonEncoderDepth - 1, pythonEncoderDepth, pythonEncoderDepth + 1, pythonEncoderDepth + 2, 1500, 9998, 9999, 10000, 10001, 12000} {
		var list, object any = "leaf", 1.0
		for range depth {
			list, object = []any{list}, map[string]any{"k": object}
		}
		compareRequest(t, used, "metrics", "pass", &decode.Decoded{Kind: "json", Data: list, Raw: plain.Body}, plain, c)
		compareRequest(t, used, "metrics", "pass", &decode.Decoded{Kind: "json", Data: object, Raw: plain.Body}, plain, c)
	}
	// The data of each decoder, and a status of each kind.
	code := 5
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "up", Type: model.GaugeMetricType, Help: "Up & <running>.", Value: 1, Labels: map[string]string{"job": "a", "instance": "b"}},
		{Name: "stale", Type: model.UntypedMetricType, Value: math.NaN(), Timestamp: new(int64)},
		{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 0.5, CumulativeCount: 2}, {UpperBound: math.Inf(1), CumulativeCount: 3}}, Sum: 1.5, Count: 3}},
		{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: math.Inf(-1)}}, NoSum: true, NoCount: true}},
	}}
	body := []byte("<r a=\"1\">text & more</r>\n")
	for _, r := range []*fetch.HTTPResponse{{StatusCode: 200, Body: body}, {NoStatus: true, Body: body, Headers: http.Header{}}, {GRPCCode: &code, Body: body, Headers: headers}, {Body: body}, {StatusCode: 404, Body: nil, Headers: headers}} {
		for _, d := range []*decode.Decoded{
			{Kind: "text", Data: string(r.Body), Raw: r.Body}, {Kind: "text", Data: "changed by a pre-script", Raw: r.Body}, {Kind: "xml", Data: "a parsed document", Raw: r.Body}, {Kind: "html", Data: 5, Raw: r.Body},
			{Kind: "prometheus", Data: set, Raw: r.Body}, {Kind: "prometheus", Data: model.MetricSet{}, Raw: r.Body}, {Kind: "prometheus", Data: "not a set", Raw: r.Body},
			{Kind: "csv", Data: []any{map[string]any{"id": "a", "value": "1"}, map[string]any{"id": "b", "value": ""}}, Raw: r.Body}, {Kind: "csv", Data: []any{[]any{"a", "1"}, []any{}}, Raw: r.Body},
			{Kind: "yaml", Data: map[string]any{"when": "2024-06-01", "n": 1, "f": math.Inf(1), "nan": math.NaN(), "list": []any{nil, true}}, Raw: r.Body},
			{Kind: "graphite", Data: map[string]any{"series": []any{map[string]any{"target": "a.b", "tags": map[string]any{"name": "a.b"}, "value": 0.5, "timestamp": 1700000000}}}, Raw: r.Body},
			{Kind: "json", Data: nil, Raw: r.Body},
		} {
			for _, mode := range []string{"metrics", "data"} {
				compareRequest(t, used, mode, "metric('a', value=1)\n", d, r, c)
			}
		}
	}
}

// 10,000 random requests, half of them with values among their data that no
// decoder makes, which the encoder leaves to json.Marshal: each is the line
// it was. The encoder that writes them all has written every one before it,
// so nothing of a request is left in the next.
func TestPythonRequestIsTheLineItWasForRandomData(t *testing.T) {
	requests := 10000
	if testing.Short() {
		requests = 2000
	}
	g := &requestGenerator{random: rand.New(rand.NewPCG(9, 2026))}
	used := &pythonEncoder{}
	for i := range requests {
		g.foreign = i%2 == 1
		r := g.response()
		d := &decode.Decoded{Kind: "json", Data: g.value(4), Raw: r.Body}
		switch g.random.IntN(8) {
		case 0:
			d = &decode.Decoded{Kind: "text", Data: string(r.Body), Raw: r.Body}
		case 1:
			d.Kind = "xml"
		}
		compareRequest(t, used, []string{"metrics", "data"}[i%2], g.text(), d, r, &model.Collector{Name: g.text()})
	}
}

// A request for a document of two thousand items is written without an
// allocation by an encoder that has written one before, as each of a
// collector's scrapes after its first is, and with the few of a buffer that
// grows by one that has not. Written by json.Marshal it took 12 allocations
// for each item: a copy of every object and list to mark the floats that are
// not finite, and then the keys of every object, sorted, through reflection.
func TestPythonRequestAllocatesNothingForEachItem(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	var body strings.Builder
	body.WriteString(`{"items":[`)
	for i := range boundSeries {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"item-%d","region":"eu-%d","kind":"k%d","value":%d,"size":%d,"ratio":%d.5,"extra":{"a":1,"b":"two","c":[1,2,3]}}`, i, i%7, i%3, i, i*1024, i)
	}
	body.WriteString("]}")
	c := &model.Collector{Name: "bounded", Decoder: model.DecoderConfig{Type: "json"}}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body.String()), Headers: http.Header{"Content-Type": {"application/json"}}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	used := &pythonEncoder{}
	var line []byte
	write := func() { line, err = used.request("metrics", "pass", d, r, c) }
	write()
	if want, _ := oraclePythonRequest("metrics", "pass", d, r, c); err != nil || string(line) != string(want) {
		t.Fatalf("%v: %.200q", err, line)
	}
	if allocations := testing.AllocsPerRun(5, write); allocations > 4 {
		t.Errorf("%.0f allocations for a request of %d items by an encoder used before, want at most 4", allocations, boundSeries)
	}
	if allocations := testing.AllocsPerRun(5, func() { _, _ = pythonRequest("metrics", "pass", d, r, c) }); allocations > 64 {
		t.Errorf("%.0f allocations for a request of %d items by a new encoder, want at most 64", allocations, boundSeries)
	}
	if was := testing.AllocsPerRun(2, func() { _, _ = oraclePythonRequest("metrics", "pass", d, r, c) }); was < 8*boundSeries {
		t.Errorf("json.Marshal wrote the request in %.0f allocations: the bound above compares with nothing", was)
	}
}
