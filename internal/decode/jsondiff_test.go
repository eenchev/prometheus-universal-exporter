package decode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The json decoder reads a body itself, in one pass (jsonvalue.go). What it
// replaced, encoding/json's Decoder and then model.Normalize, is kept here
// as the oracle, and these tests compare the two: on the same body they
// refuse alike, for the same kind of reason, and accept alike, with the same
// values of the same types.

// oracleDecodeJSON is the json decoder as it was.
func oracleDecodeJSON(body []byte) (any, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errJSONTrailingData
	}
	return model.Normalize(v), nil
}

// sameJSON says where two decoded values differ, or nothing when they are
// the same: of the same types throughout, a float64 bit for bit, so that -0
// is not 0 and a NaN would be itself, and a *big.Int by its value.
func sameJSON(old, got any, path string) string {
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
			if diff := sameJSON(value, other, path+"."+key); diff != "" {
				return diff
			}
		}
	case []any:
		y, ok := got.([]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
		for i := range x {
			if diff := sameJSON(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); diff != "" {
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
		return fmt.Sprintf("%s: the oracle made a %T", path, old)
	}
	return ""
}

// jsonRefusal names the kind of a refusal: an empty body, one that ends too
// early, data after the value, or a byte that cannot be where it is.
func jsonRefusal(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "ends too early"
	case errors.Is(err, io.EOF):
		return "empty"
	case errors.Is(err, errJSONTrailingData):
		return "trailing data"
	}
	return "invalid"
}

// compareJSON decodes body with the decoder and with the oracle, and
// reports any difference between the two.
func compareJSON(t *testing.T, body []byte) (accepted bool) {
	t.Helper()
	old, oldErr := oracleDecodeJSON(body)
	got, err := decodeJSON(body)
	if (err == nil) != (oldErr == nil) {
		t.Errorf("%q: err=%v, was %v", clip(body), err, oldErr)
		return false
	}
	if jsonRefusal(err) != jsonRefusal(oldErr) {
		t.Errorf("%q: refused as %s (%v), was as %s (%v)", clip(body), jsonRefusal(err), err, jsonRefusal(oldErr), oldErr)
	}
	if err != nil {
		return false
	}
	if diff := sameJSON(old, got, "$"); diff != "" {
		t.Errorf("%q: %s", clip(body), diff)
	}
	return true
}

// clip is the start of a body, for a failure's message.
func clip(body []byte) []byte { return body[:min(len(body), 300)] }

// The cases a JSON decoder gets wrong when it gets any wrong: every one is
// read as it was, or refused as it was.
func TestJSONDecoderAgreesWithTheOneItReplacedOnEdgeCases(t *testing.T) {
	deep := func(opening, closing string, depth int) string {
		return strings.Repeat(opening, depth) + strings.Repeat(closing, depth)
	}
	cases := []string{
		// Nothing, and whitespace.
		"", " ", "\n\t\r ", "\ufeff{}", "\x00", "\v1", "\f1", "\u00a01", " 1 ", "\n[\n]\n", "1\x00",
		// Scalars at the root.
		"null", "true", "false", `""`, `"a"`, "0", "nul", "nulL", "tru", "trux", "fals", "falsy", "True", "NULL", "nullx", "null null", "truefalse",
		// Numbers: what is whole, what is not, and what is no number.
		"-0", "-0.0", "0.0", "0e0", "-0e-0", "1.0", "1.5", "100", "1e2", "1E2", "1e+2", "1e-2", "1.0e2", "1.5e1", "12.50",
		"9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809",
		"999999999999999999", "1000000000000000000", "-999999999999999999", "-1000000000000000000",
		"18446744073709551615", "18446744073709551616", "123456789012345678901234567890", "-123456789012345678901234567890",
		"1e400", "-1e400", "1e-400", "1.7976931348623157e308", "1.7976931348623159e308", "4.9e-324", "2e-324", "1e309", "1E400", "0.1e400",
		"1." + strings.Repeat("0", 400), strings.Repeat("9", 400), "0." + strings.Repeat("0", 400) + "1", "1e" + strings.Repeat("9", 30), "1e-" + strings.Repeat("9", 30),
		"01", "-01", "00", "-", "+1", "+0", ".5", "1.", "1.e2", "1e", "1e+", "1e-", "-.5", "--1", "1-", "1e2.5", "1.2.3", "0x10", "1_000", "Infinity", "-Infinity", "NaN", "1e2e3", "0e", "-0.", "1 2", "1,", "0 ",
		"[01]", "[-]", "[1.]", "[1e]", "[.5]", "[+1]", "[1 2]", "[0.1.2]", "[-0,0,-0.0]", "[1e400,2]",
		// Strings: escapes, surrogates, control characters, what is not UTF-8.
		`"\"\\\/\b\f\n\r\t"`, `"\u0041\u00e9\u20ac"`, `"\u0000"`, `"\u001f"`, `"\uFFFF"`, `"\uffFE"`, `"\uD83D\uDE00"`, `"\ud83d\ude00"`,
		`"\uD83D"`, `"\uDE00"`, `"\uDE00\uD83D"`, `"\uD83D\uD83D\uDE00"`, `"\uD83Dx"`, `"\uD83D\n"`, `"\uD83D\u0041"`, `"\uD83D\u"`, `"\uD83D\uDE0"`, `"\uD83D\uDE0G"`, `"\uD83D\\uDE00"`, `"\uD800\uDC00"`, `"\uDBFF\uDFFF"`, `"\uD800\uDBFF"`, `"\uDC00\uDC00"`,
		`"\u12"`, `"\u12G4"`, `"\u"`, `"\U0041"`, `"\x41"`, `"\a"`, `"\'"`, `"\0"`, `"\ "`, `"\`, `"\"`, `"\\"`, `"\\\"`, `"abc`, `"`, `'a'`, `"a"b"`, `"\u00e9`, `"\u00`,
		"\"a\nb\"", "\"a\tb\"", "\"a\x00b\"", "\"a\x1fb\"", "\"\x7f\"", "\"a\rb\"",
		"\"\xff\"", "\"a\xc3\"", "\"\xc3\x28\"", "\"\xe2\x82\"", "\"\xe2\x82\xac\"", "\"\xed\xa0\x80\"", "\"\xf0\x9f\x98\x80\"", "\"\xf0\x9f\x98\"", "\"\xf4\x90\x80\x80\"", "\"\xc0\x80\"", "\"\xef\xbf\xbd\"", "\"\xff\\n\xfe\"", "\"\xe2\x82\\u00ac\"", "\"\xc3\\\"\"",
		"{\"\xff\":1,\"\xfe\":2}", "{\"\\uD800\":1,\"\\uDC00\":2}", `{"\u0061":1,"a":2}`, "\xff", "[\xff]", "{\xff:1}",
		// Arrays.
		"[]", "[ ]", "[1]", "[1,2,3]", "[ 1 , 2 ]", "[[],[[]],{}]", "[", "]", "[,]", "[1,]", "[,1]", "[1,,2]", "[1 2]", "[1", "[1,", "[1]]", "[[1]", `["a",]`, "[null,true,false]", "[1}", "[\n1\n,\n2\n]",
		// Objects, and a key written twice.
		"{}", "{ }", `{"a":1}`, `{ "a" : 1 , "b" : [ ] }`, `{"a":{"b":{"c":null}}}`, `{"":0}`, `{"a":1,"a":2}`, `{"a":1,"b":2,"a":3}`, `{"a":{"x":1},"a":[2]}`, `{"a":1,"a":{"b":1,"b":2}}`,
		"{", "}", `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`, `{"a":1,}`, `{,}`, `{"a" 1}`, `{"a":1 "b":2}`, `{a:1}`, `{1:1}`, `{"a":}`, `{"a"::1}`, `{"a":1}}`, `{"a":1]`, `{null:1}`, `{"a":1,,"b":2}`, `{"a",1}`, `{"a":1:2}`, `{'a':1}`,
		// Data after the value.
		"{}{}", "{} {}", "{}\n{}\n", "[][]", "1 1", `"a""b"`, "{}x", "{},", "[] ,", "null\nnull", "{} \xff", "[]\x00", "1 \v", "{}/**/", "{} //", "[1] 2", "true false",
		// Comments and other things JSON has not.
		"/**/1", "//\n1", "[1,/**/2]", "{\"a\":1,//\n}", "[1;2]", "(1)", "<a/>", "undefined", "[NaN]", "[Infinity]", "[-Infinity]", "[0x1]", "['a']", "{\"a\":01}",
		// Depth: 10,000 arrays or objects, one inside the other, and no more.
		deep("[", "]", 100), deep("[", "]", MaxDepth), deep("[", "]", MaxDepth+1), strings.Repeat("[", MaxDepth), strings.Repeat("[", MaxDepth+1), strings.Repeat("[", MaxDepth+5) + "x",
		deep(`{"a":`, "}", MaxDepth-1) + "", strings.Repeat(`{"a":`, MaxDepth-1) + "1" + strings.Repeat("}", MaxDepth-1), strings.Repeat(`{"a":`, MaxDepth) + "1" + strings.Repeat("}", MaxDepth), strings.Repeat(`{"a":`, MaxDepth+1) + "1" + strings.Repeat("}", MaxDepth+1),
		strings.Repeat(`{"a":[`, MaxDepth/2) + strings.Repeat("]}", MaxDepth/2), strings.Repeat(`{"a":[`, MaxDepth/2) + "[]" + strings.Repeat("]}", MaxDepth/2),
		"[" + strings.Repeat("[],", 20000) + "[]]",
		// Many keys, many items, long strings and keys.
		`{"items":[` + strings.Repeat(`{"id":"a","n":1},`, 3000) + `{"id":"b","n":2}]}`, `"` + strings.Repeat("x", 100000) + `"`, `{"` + strings.Repeat("k", 5000) + `":"` + strings.Repeat("\\n", 5000) + `"}`,
	}
	var object strings.Builder
	object.WriteString("{")
	for i := 0; i < 2000; i++ {
		// Every key twice, to different values, among 1000 different keys.
		fmt.Fprintf(&object, `"key%d":%d,`, i%1000, i)
	}
	object.WriteString(`"last":[]}`)
	cases = append(cases, object.String())
	accepted := 0
	for _, body := range cases {
		if compareJSON(t, []byte(body)) {
			accepted++
		}
		// And inside an array, an object, and whitespace.
		compareJSON(t, []byte("["+body+"]"))
		compareJSON(t, []byte(`{"k":`+body+`}`))
		compareJSON(t, []byte(" \n"+body+"\t\r\n"))
	}
	if accepted < 100 || accepted > len(cases)-100 {
		t.Fatalf("%d of %d cases are accepted: the table should hold many of both kinds", accepted, len(cases))
	}
	t.Logf("%d cases, %d of them accepted, each compared four ways", len(cases), accepted)
}

// jsonGenerator writes random JSON documents.
type jsonGenerator struct {
	random *rand.Rand
	out    []byte
}

func (g *jsonGenerator) pick(choices ...string) string {
	return choices[g.random.IntN(len(choices))]
}

func (g *jsonGenerator) space() {
	for g.random.IntN(6) == 0 {
		g.out = append(g.out, g.pick(" ", "\n", "\t", "\r", "  ")...)
	}
}

func (g *jsonGenerator) digits(n int) {
	for i := 0; i < n; i++ {
		g.out = append(g.out, byte('0'+g.random.IntN(10)))
	}
}

func (g *jsonGenerator) number() {
	if g.random.IntN(3) == 0 {
		g.out = append(g.out, '-')
	}
	switch g.random.IntN(8) {
	case 0:
		g.out = append(g.out, '0')
	case 1:
		// Around the ends of int64, and far beyond.
		g.out = append(g.out, g.pick("9223372036854775807", "9223372036854775808", "999999999999999999", "1000000000000000000", "18446744073709551616", "9223372036854775806")...)
		g.digits(g.random.IntN(3) * g.random.IntN(12))
	default:
		g.out = append(g.out, byte('1'+g.random.IntN(9)))
		g.digits(g.random.IntN(3) * g.random.IntN(11))
	}
	if g.random.IntN(4) == 0 {
		g.out = append(g.out, '.')
		g.digits(1 + g.random.IntN(2)*g.random.IntN(20))
		if g.random.IntN(3) == 0 {
			g.out = append(g.out, "000"[:1+g.random.IntN(3)]...)
		}
	}
	if g.random.IntN(6) == 0 {
		g.out = append(g.out, g.pick("e", "E", "e+", "e-", "E+", "E-")...)
		g.out = append(g.out, g.pick("0", "1", "2", "5", "10", "17", "22", "300", "308", "309", "323", "324", "400", "00", "007")...)
	}
}

var jsonWords = []string{"id", "name", "value", "items", "a", "b", "", "k", "status", "labels", "ünïcode", "日本", "with space", "a.b", "\U0001F600"}

func (g *jsonGenerator) text() {
	g.out = append(g.out, '"')
	for n := g.random.IntN(4); n > 0; n-- {
		switch g.random.IntN(14) {
		case 0:
			g.out = append(g.out, g.pick(`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`)...)
		case 1:
			g.out = append(g.out, g.pick(`\u0041`, `\u00e9`, `\u0000`, `\u001F`, `\u20AC`, `\uffff`, `\uFFFD`, `\u2028`)...)
		case 2:
			// Surrogates, whole pairs and halves, in either order.
			g.out = append(g.out, g.pick(`\uD83D\uDE00`, `\ud800\udc00`, `\uD83D`, `\uDE00`, `\uDBFF`, `\uDC00\uD800`, `\uD83D\uD83D\uDE00`, `\uD800\u0041`)...)
		case 3:
			// Bytes that are not UTF-8.
			g.out = append(g.out, g.pick("\xff", "\xc3", "\xe2\x82", "\xed\xa0\x80", "\xf0\x9f\x98", "\xc0\xaf", "\x80", "\xf8\x88\x80\x80\x80")...)
		case 4:
			g.out = append(g.out, g.pick("é", "€", "\U0001F600", "\ufffd", "\x7f", "日本語")...)
		default:
			g.out = append(g.out, jsonWords[g.random.IntN(len(jsonWords))]...)
			if g.random.IntN(3) == 0 {
				g.digits(1 + g.random.IntN(3))
			}
		}
	}
	g.out = append(g.out, '"')
}

func (g *jsonGenerator) value(depth int) {
	kind := g.random.IntN(10)
	if depth <= 0 && kind >= 6 {
		kind = g.random.IntN(6)
	}
	switch kind {
	case 0:
		g.out = append(g.out, g.pick("null", "true", "false")...)
	case 1, 2:
		g.text()
	case 3, 4, 5:
		g.number()
	case 6, 7:
		g.out = append(g.out, '[')
		g.space()
		for i, n := 0, g.random.IntN(5)*g.random.IntN(4); i < n; i++ {
			if i > 0 {
				g.out = append(g.out, ',')
				g.space()
			}
			g.value(depth - 1)
			g.space()
		}
		g.out = append(g.out, ']')
	default:
		g.out = append(g.out, '{')
		g.space()
		// Up to twenty members, so that some objects are more than the
		// eight a small map holds, and keys of few words, so that some
		// are written twice.
		for i, n := 0, g.random.IntN(5)*g.random.IntN(5); i < n; i++ {
			if i > 0 {
				g.out = append(g.out, ',')
				g.space()
			}
			if g.random.IntN(3) == 0 {
				g.text()
			} else {
				g.out = append(g.out, '"')
				g.out = append(g.out, jsonWords[g.random.IntN(len(jsonWords))]...)
				g.out = append(g.out, '"')
			}
			g.space()
			g.out = append(g.out, ':')
			g.space()
			g.value(depth - 1)
			g.space()
		}
		g.out = append(g.out, '}')
	}
}

// document writes one document, mostly an object or an array.
func (g *jsonGenerator) document() []byte {
	g.out = g.out[:0]
	g.space()
	g.value(1 + g.random.IntN(5))
	g.space()
	return bytes.Clone(g.out)
}

// corrupt returns body with one to three small changes: a byte removed,
// replaced or added, the body cut short, or a part of it written twice.
func (g *jsonGenerator) corrupt(body []byte) []byte {
	const alphabet = "{}[]\",:\\ \n\t-+.eE0123456789ntfu/u\x00\x1f\x7f\x80\xff\xc3\xed'#a"
	out := bytes.Clone(body)
	for n := 1 + g.random.IntN(3); n > 0; n-- {
		if len(out) == 0 {
			out = append(out, alphabet[g.random.IntN(len(alphabet))])
			continue
		}
		at := g.random.IntN(len(out))
		switch g.random.IntN(6) {
		case 0:
			out = append(out[:at], out[at+1:]...)
		case 1:
			out[at] = alphabet[g.random.IntN(len(alphabet))]
		case 2:
			out = append(out[:at], append([]byte{alphabet[g.random.IntN(len(alphabet))]}, out[at:]...)...)
		case 3:
			out = out[:at]
		case 4:
			end := min(len(out), at+1+g.random.IntN(8))
			out = append(out[:end], append(bytes.Clone(out[at:end]), out[end:]...)...)
		default:
			out = append(out, g.pick("}", "]", ",", " ", "\n{}", "x", "\"", "1", "null")...)
		}
	}
	return out
}

// 20,000 random documents, written with every kind of number, escape,
// surrogate, byte that is not UTF-8, key written twice and whitespace, and
// three corruptions of each, 80,000 bodies in all: the decoder accepts
// exactly those the one it replaced accepted, refuses the others for the
// same kind of reason, and reads what it accepts as the same values. Under
// the race detector 2,000 documents, 8,000 bodies: the first tenth, of which
// as large a share is to be accepted and refused.
func TestJSONDecoderAgreesWithTheOneItReplacedOnRandomDocuments(t *testing.T) {
	g := &jsonGenerator{random: rand.New(rand.NewPCG(2026, 1002))}
	documents := alloctest.UnlessRaced(20000, 2000)
	accepted, refused := 0, 0
	count := func(ok bool) {
		if ok {
			accepted++
		} else {
			refused++
		}
	}
	for i := 0; i < documents && !t.Failed(); i++ {
		body := g.document()
		if !compareJSON(t, body) {
			t.Fatalf("the generator wrote a document that is refused: %q", clip(body))
		}
		accepted++
		for j := 0; j < 3; j++ {
			count(compareJSON(t, g.corrupt(body)))
		}
	}
	if accepted < documents*3/2 || refused < documents*9/4 {
		t.Fatalf("%d bodies accepted and %d refused: the corruptions should leave more than %d and %d", accepted, refused, documents*3/2, documents*9/4)
	}
	t.Logf("%d bodies accepted, %d refused", accepted, refused)
}

// An error says where in the body the byte that cannot be there is, by line
// and column, and what was expected there; a body that ends too early, or
// is empty, or goes on after its value says that.
func TestJSONDecoderErrorsSayWhere(t *testing.T) {
	for body, want := range map[string]string{
		"{\"a\": 1,\n \"b\": x}":        `JSON decode: invalid character 'x' looking for beginning of value, at line 2, column 7`,
		`{"a" 1}`:                       `JSON decode: invalid character '1' after object key, at line 1, column 6`,
		`{"a":1 "b":2}`:                 `JSON decode: invalid character '"' after object key:value pair, at line 1, column 8`,
		`{a:1}`:                         `JSON decode: invalid character 'a' looking for beginning of object key string, at line 1, column 2`,
		"[1,\n2\n3]":                    `JSON decode: invalid character '3' after array element, at line 3, column 1`,
		"[\"a\tb\"]":                    `JSON decode: invalid character '\t' in string literal, at line 1, column 4`,
		`["\x"]`:                        `JSON decode: invalid character 'x' in string escape code, at line 1, column 4`,
		`["\u12G4"]`:                    `JSON decode: invalid character 'G' in \u hexadecimal character escape, at line 1, column 7`,
		`[trux]`:                        `JSON decode: invalid character 'x' in literal true (expecting 'e'), at line 1, column 5`,
		`[-x]`:                          `JSON decode: invalid character 'x' in numeric literal, at line 1, column 3`,
		`[1.x]`:                         `JSON decode: invalid character 'x' after decimal point in numeric literal, at line 1, column 4`,
		`[1ex]`:                         `JSON decode: invalid character 'x' in exponent of numeric literal, at line 1, column 4`,
		strings.Repeat("[", 10001):      `JSON decode: arrays and objects nested more than 10000 deep, at line 1, column 10001`,
		`{"a":`:                         `JSON decode: unexpected EOF`,
		`{"a":"b`:                       `JSON decode: unexpected EOF`,
		"":                              `JSON decode: EOF`,
		" \n":                           `JSON decode: EOF`,
		"{\"a\":1}\n{\"a\":2}":          `JSON decode: trailing data after the JSON value (NDJSON, one value per line, is not supported)`,
		"\xef\xbb\xbf{\"a\": 1,\n\"b\"": `JSON decode: unexpected EOF`,
	} {
		if _, err := decodeAs(t, "json", "", body); err == nil || err.Error() != want {
			t.Errorf("%q: err=%v, want %s", clip([]byte(body)), err, want)
		}
	}
}

// Whatever the body, the decoder and the one it replaced agree on it; the
// bodies here are the start of a search for one they do not agree on
// (go test -fuzz FuzzJSONDecoderAgreesWithTheOneItReplaced ./internal/decode/).
func FuzzJSONDecoderAgreesWithTheOneItReplaced(f *testing.F) {
	for _, body := range []string{
		`{"items":[{"id":"a","n":1,"ok":true,"none":null},{"id":"b","n":-2.5e3}]}`,
		`["\u00e9\ud83d\ude00\ud83d\n", 9223372036854775808, 1e400, -0, 0.0]`,
		"{\"a\":1,\"a\":[\"\xff\"]} ",
		"[[[[]]]]",
		"{\"a\":1}\n{\"a\":2}",
	} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		compareJSON(t, body)
	})
}

// A whole number of more digits than model.MaxWholeNumberDigits is read as
// its text, every digit kept, by the decoder and by model.Normalize alike:
// as a *big.Int it took time growing with the square of its length, two
// seconds for a million digits, which a response within the default size
// limit can hold and no deadline could interrupt. One of that many digits or
// fewer is still a *big.Int, and a number with a fraction or an exponent is
// read as before, whatever its length. That the long ones are text is what
// says no time was spent making integers of them: the test holds no clock,
// which a busy machine would fail.
func TestJSONWholeNumberOfManyDigitsIsReadAsItsText(t *testing.T) {
	digits := func(n int) string { return strings.Repeat("7", n) }
	for name, tc := range map[string]struct {
		number string
		text   bool
	}{
		"at the bound":             {digits(model.MaxWholeNumberDigits), false},
		"negative, at the bound":   {"-" + digits(model.MaxWholeNumberDigits-1), false},
		"one digit past it":        {digits(model.MaxWholeNumberDigits + 1), true},
		"negative, one past it":    {"-" + digits(model.MaxWholeNumberDigits), true},
		"two million digits":       {digits(2_000_000), true},
		"long, with a fraction":    {digits(5000) + ".5", true},
		"long, with an exponent":   {"1" + strings.Repeat("0", 5000) + "e-4990", false},
		"twenty digits":            {digits(20), false},
		"long, in an object value": {digits(9000), true},
	} {
		body := []byte(`{"id":` + tc.number + `}`)
		got, err := decodeJSON(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		value := got.(map[string]any)["id"]
		if _, isText := value.(string); isText != tc.text || isText && value != tc.number {
			t.Errorf("%s: read as %T, want text: %v", name, value, tc.text)
		}
		want, err := oracleDecodeJSON(body)
		if err != nil {
			t.Fatalf("%s: the decoder it replaced: %v", name, err)
		}
		if !reflect.DeepEqual(valueShape(value), valueShape(want.(map[string]any)["id"])) {
			t.Errorf("%s: read as %T, and by Normalize as %T", name, value, want.(map[string]any)["id"])
		}
	}
}

// valueShape is a number as it can be compared: a *big.Int by its digits.
func valueShape(v any) any {
	if n, ok := v.(*big.Int); ok {
		return "big:" + n.String()
	}
	return v
}
