package decode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A response whose decoder is chosen from its content was read twice when it
// was JSON: the detection parsed it with encoding/json to see whether it is
// JSON, threw the result away, and the json decoder then read it. The
// detection now reads it with the json decoder and Decode returns what that
// made (detectFormatKeeping, sniffJSON). The detection as it was is kept
// here as the oracle, and these tests compare the two: every body is taken
// for what it was taken for, decoded to what it was decoded to, and refused
// with the words it was refused with.

// oracleDetectFormat is detectFormat as it was.
func oracleDetectFormat(r *fetch.HTTPResponse) string {
	rawCT := strings.ToLower(r.Headers.Get("Content-Type"))
	ct := strings.TrimSpace(strings.Split(rawCT, ";")[0])
	switch {
	case ct == "application/json" || strings.HasSuffix(ct, "+json"):
		return "json"
	case ct == "application/yaml" || ct == "text/yaml" || ct == "application/x-yaml":
		return "yaml"
	case ct == "application/xml" || ct == "text/xml":
		return "xml"
	case ct == "text/csv":
		return "csv"
	case ct == "text/html":
		return "html"
	case ct == "application/openmetrics-text" || ct == "text/plain" && strings.Contains(rawCT, "version=0.0.4"):
		return "prometheus"
	case ct == fetch.GraphiteContentType:
		return "graphite"
	}
	b := bytes.TrimSpace(r.Body)
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		var x any
		if json.Unmarshal(b, &x) == nil {
			return "json"
		}
	}
	if bytes.Contains(b, []byte("# TYPE ")) || bytes.Contains(b, []byte("# HELP ")) {
		return "prometheus"
	}
	if looksLikeHTML(b) {
		return "html"
	}
	if bytes.HasPrefix(b, []byte("<")) {
		return "xml"
	}
	if looksLikeCarbon(b) {
		return "graphite"
	}
	return "text"
}

// sniffResponse is a response of body under contentType, which is no header
// at all when it is empty.
func sniffResponse(body []byte, contentType string) *fetch.HTTPResponse {
	headers := http.Header{}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	return &fetch.HTTPResponse{StatusCode: 200, Body: append([]byte(nil), body...), Headers: headers}
}

// oracleDecodeDetected is Decode as it was for a collector whose decoder is
// chosen from the response: the body converted to UTF-8, the decoder
// detected as it was detected, and the response then decoded by that
// decoder, which is what a collector naming the decoder has, whose path the
// detection is no part of.
func oracleDecodeDetected(body []byte, contentType string, c *model.Collector) (*Decoded, error) {
	converted := sniffResponse(body, contentType)
	if _, err := convertToUTF8(converted, c); err != nil {
		return nil, err
	}
	named := *c
	named.Decoder.Type = oracleDetectFormat(converted)
	return Decode(sniffResponse(body, contentType), &named)
}

// sameDecoded says where what Decode made of a body differs from what the
// oracle made of it, or nothing: the decoder, the error's text, the body
// kept beside the value and the value, which for a parsed XML or HTML
// document is compared by the body it was parsed from.
func sameDecoded(old *Decoded, oldErr error, got *Decoded, gotErr error) string {
	if (oldErr == nil) != (gotErr == nil) || oldErr != nil && oldErr.Error() != gotErr.Error() {
		return fmt.Sprintf("error %v, was %v", gotErr, oldErr)
	}
	if oldErr != nil {
		return ""
	}
	if old.Kind != got.Kind {
		return fmt.Sprintf("decoder %s, was %s", got.Kind, old.Kind)
	}
	if !bytes.Equal(old.Raw, got.Raw) {
		return fmt.Sprintf("body %q, was %q", clip(got.Raw), clip(old.Raw))
	}
	switch old.Kind {
	case "xml", "html":
		return ""
	case "json":
		return sameJSON(old.Data, got.Data, "$")
	}
	// The reports name the first line left out as an error, compared by
	// what it says.
	if !reflect.DeepEqual(old.Data, got.Data) || fmt.Sprintf("%+v %+v", old.Graphite, old.Prometheus) != fmt.Sprintf("%+v %+v", got.Graphite, got.Prometheus) {
		return fmt.Sprintf("value %#v, was %#v", got.Data, old.Data)
	}
	return ""
}

// sniffContentTypes are the headers a body is detected under: none, JSON's
// own, those that say nothing of the format, and those that say another.
var sniffContentTypes = []string{
	"", "application/json", "application/json; charset=utf-8", "application/problem+json",
	"text/plain", "text/plain; charset=utf-8", "application/octet-stream", "binary/octet-stream", "not a content type",
	"application/yaml", "text/xml", "text/csv", "text/html", "text/plain; version=0.0.4", "application/openmetrics-text",
}

// sniffTransforms are the transforms that leave the decoder to the response.
var sniffTransforms = []string{"jq", "yq", "xpath", "python"}

// compareDetected decodes body under each of the Content-Types, for each of
// the transforms and with the decoder left unset and set to auto, as Decode
// does and as it did, and reports any difference. It returns the decoders
// the body was read by.
func compareDetected(t *testing.T, body []byte, contentTypes, transforms []string) map[string]bool {
	t.Helper()
	kinds := map[string]bool{}
	for _, contentType := range contentTypes {
		// The detection by itself names the same decoder.
		if want, is := oracleDetectFormat(sniffResponse(body, contentType)), detectFormat(sniffResponse(body, contentType)); is != want {
			t.Fatalf("%q under %q: detected as %s, was %s", clip(body), contentType, is, want)
		}
		for _, transform := range transforms {
			for _, decoder := range []string{"", "auto"} {
				c := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: transform}}
				old, oldErr := oracleDecodeDetected(body, contentType, c)
				got, gotErr := Decode(sniffResponse(body, contentType), c)
				if diff := sameDecoded(old, oldErr, got, gotErr); diff != "" {
					t.Fatalf("%q under %q, transform %s, decoder %q: %s", clip(body), contentType, transform, decoder, diff)
				}
				if gotErr == nil {
					kinds[got.Kind] = true
				}
			}
		}
	}
	return kinds
}

// Bodies of every kind a target answers with, under every kind of
// Content-Type: JSON objects, arrays and scalars; JSON after whitespace,
// after a byte order mark, and after and before the whitespace that is none
// to JSON; what is nearly JSON; YAML that is JSON too; markup, CSV,
// exposition, carbon lines and text; numbers no float64 holds, of which the
// detection made no JSON; keys written twice; nesting at the limit and past
// it; and bytes that are not UTF-8. Each is decoded by the decoder it was,
// to the value it was, or refused with the error it was.
func TestDetectedBodiesAreDecodedAsBefore(t *testing.T) {
	digits := func(n int) string { return strings.Repeat("9", n) }
	// The largest float64, written out, and the whole number after the
	// last one that still rounds to it.
	const largest = "179769313486231570814527423731704356798070567525844996598917476803157260780028538760589558632766878171540458953514382464234321326889464182768467546703537516986049910576551282076245490090389328944075868508455133942304583236903222948165808559332123348274797826204144723168738177180919299881250404026184124858368"
	const tooLarge = "179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792"
	bodies := []string{
		// JSON.
		`{}`, `[]`, `{"v":1}`, `[1,2,3]`, `{"items":[{"id":"a","value":1.5},{"id":"b","value":null}]}`,
		`{"help":"# HELP in a string","type":"# TYPE too"}`, `[{"a":"<html>"}]`, `{"v":1}` + "\n", "\n\t {\"v\":1}\r\n ",
		// Scalars start with no bracket, and are text.
		`1`, `"text"`, `true`, `null`, `-0.5e3`,
		// A byte order mark is removed first.
		"\xef\xbb\xbf" + `{"v":1}`, "\xef\xbb\xbf\n" + `[1]`, "\xff\xfe{\x00}\x00", "\xfe\xff\x00[\x001\x00]",
		// Whitespace the detection trims and JSON does not have.
		"\f" + `{"v":1}`, `{"v":1}` + "\v", "\u00a0" + `{"v":1}`, `[1]` + "\u0085", "\u2028[1]\u3000", "\xc2{\"v\":1}", "\x85[1]", "\x00[1]",
		// Nearly JSON.
		`{"v":1`, `{"v":1}}`, `{"v":1} {"v":2}`, `{"v":1}` + "\n" + `{"v":2}` + "\n", `[1,]`, `{'v':1}`, `{v:1}`, `[01]`, `[+1]`, `[1.]`, `[.5]`, `[1e]`,
		`{"v":NaN}`, `[Infinity]`, `{"v":"a` + "\n" + `b"}`, `{"v":"\x"}`, `{"v":"\ud800"}`, `{"v":tru}`, `{"a":1,}`, `[`, `{`, `[]]`, `{"a"}`, `{"a":}`, `{,}`, `[1 2]`,
		"[INFO] started\n[WARN] v=1\n", "{{ template }}", "[section]\nkey=value\n",
		// YAML that is JSON, and YAML that is not.
		`{"a": [1, 2], "b": {"c": "d"}}`, "{a: 1, b: [x, y]}", "[a, b]", "a: 1\nb:\n  - 2\n", "---\na: 1\n",
		// Markup.
		`<?xml version="1.0"?><r><v>1</v></r>`, "<r><v>1</v></r>", "<!DOCTYPE html><html><body><p>1</p></body></html>", "<html><body>1</body></html>", "<!-- c --><HTML></HTML>", "<",
		// CSV, exposition, carbon lines, text, nothing.
		"id,value\na,1\nb,2\n", "# HELP v A value.\n# TYPE v gauge\nv 1\n", "# TYPE v gauge\nv{a=\"b\"} 1 1700000000000\n", `{"note":"# TYPE v gauge"}` + "\n# TYPE v gauge\nv 1\n",
		"servers.web01.cpu 0.5 1700000000\nservers.web02.cpu 0.25 1700000000\n", "v=1", "plain text\nof two lines\n", "", " ", "\n\n",
		// Numbers at the edge of a float64, of an int64 and of what is read
		// as a number at all.
		`[1e308]`, `[1e309]`, `[-1e309]`, `[1e400]`, `{"v":-1e400}`, `[1e-400]`, `[0.0000001e400]`, `[1.7976931348623157e308]`, `[1.7976931348623159e308]`, `[1.797693134862315808e308]`,
		`[` + largest + `]`, `[-` + largest + `]`, `[` + tooLarge + `]`, `[-` + tooLarge + `]`, `[` + digits(308) + `]`, `[` + digits(309) + `]`, `[-` + digits(309) + `]`, `[1` + strings.Repeat("0", 308) + `]`, `[2` + strings.Repeat("0", 308) + `]`,
		`[` + digits(310) + `]`, `[` + digits(model.MaxWholeNumberDigits) + `]`, `[` + digits(model.MaxWholeNumberDigits+1) + `]`, `[` + digits(400) + `.5]`, `[` + digits(308) + `.5]`, `[` + digits(300) + `e9]`,
		`[9223372036854775807,9223372036854775808,-9223372036854775808,-9223372036854775809,18446744073709551616]`, `[1500000000000000001,0.1,-0,-0.0,1E2,1e+2,1.0]`,
		`{"ok":1,"big":1e999,"tail":[1,2,3]}`, `[1e400`, `[1e400,]`,
		// Keys written twice, and the same after an escape.
		`{"a":1,"a":2}`, `{"a":{"b":1},"a":{"b":2,"b":3}}`, `{"a":1,"a":2}`, `{"":1,"":2}`,
		// Bytes that are not UTF-8, and halves of surrogate pairs.
		"{\"v\":\"\xff\xfe\"}", "{\"\xc3\x28\":1}", "[\"\xe2\x82\"]", `["\ud83d\ude00","\ud83d","\ude00\ud83d"]`, "[\"a\x00b\"]", "{\"v\":\"caf\xe9\"}",
	}
	kinds := map[string]bool{}
	for _, body := range bodies {
		for kind := range compareDetected(t, []byte(body), sniffContentTypes, sniffTransforms) {
			kinds[kind] = true
		}
	}
	// Arrays and objects nested as deep as the decoder reads them, and one
	// deeper, which is no JSON to the detection and text to the decoder.
	for _, depth := range []int{2, 100, jsonMaxDepth - 1, jsonMaxDepth, jsonMaxDepth + 1} {
		for _, body := range []string{strings.Repeat("[", depth) + strings.Repeat("]", depth), strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth), strings.Repeat("[", depth)} {
			compareDetected(t, []byte(body), []string{"", "application/json"}, []string{"jq"})
			want := "text"
			if depth <= jsonMaxDepth && !strings.HasSuffix(body, "[") {
				want = "json"
			}
			if got := detectFormat(sniffResponse([]byte(body), "")); got != want {
				t.Errorf("%.8s nested %d deep is detected as %s, want %s", body, depth, got, want)
			}
		}
	}
	// The table reaches every decoder a detection can choose.
	for _, kind := range []string{"json", "yaml", "xml", "csv", "html", "prometheus", "graphite", "text"} {
		if !kinds[kind] {
			t.Errorf("no body of the table was decoded as %s", kind)
		}
	}
}

// The words of what is refused are the words it was refused with, and what
// the detection takes for JSON is what it took for JSON: a body with a
// number no float64 holds is text, as it was, and JSON when the collector
// or the Content-Type says it is.
func TestDetectionTakesForJSONWhatItTook(t *testing.T) {
	jq := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "jq"}}
	for _, tc := range []struct {
		body, contentType, kind, failure string
	}{
		{`{"v":1}`, "", "json", ""},
		{` {"v":1} `, "text/plain", "json", ""},
		{`{"v":1e400}`, "", "text", ""},
		{`{"v":1e400}`, "application/json", "json", ""},
		{`{"v":` + strings.Repeat("9", 400) + `}`, "text/plain", "text", ""},
		{`{"v":1} {"v":2}`, "", "text", ""},
		{`{"v":1`, "", "text", ""},
		{`{"v":1`, "application/json", "", "JSON decode: unexpected EOF"},
		{"\f" + `{"v":1}`, "", "", `JSON decode: invalid character '\f' looking for beginning of value, at line 1, column 1`},
		{`{"v":1}` + "\u00a0", "", "", "JSON decode: trailing data after the JSON value (NDJSON, one value per line, is not supported)"},
		{`[1]` + "\v", "application/octet-stream", "", "JSON decode: trailing data after the JSON value (NDJSON, one value per line, is not supported)"},
	} {
		d, err := Decode(sniffResponse([]byte(tc.body), tc.contentType), jq)
		switch {
		case tc.failure != "":
			if err == nil || err.Error() != tc.failure {
				t.Errorf("%q under %q: error %v, want %s", tc.body, tc.contentType, err, tc.failure)
			}
		case err != nil || d.Kind != tc.kind:
			t.Errorf("%q under %q: %v, error %v, want decoder %s", tc.body, tc.contentType, d, err, tc.kind)
		}
	}
	// What was kept of the detection is the value the decoder makes.
	d, err := Decode(sniffResponse([]byte(` {"v":[1,2.5,"a",null,true,12345678901234567890]} `), ""), jq)
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeJSON([]byte(`{"v":[1,2.5,"a",null,true,12345678901234567890]}`))
	if err != nil {
		t.Fatal(err)
	}
	if diff := sameJSON(want, d.Data, "$"); diff != "" || string(d.Raw) != ` {"v":[1,2.5,"a",null,true,12345678901234567890]} ` {
		t.Fatalf("%s; body %q", diff, d.Raw)
	}
}

// 2,000 random documents, with every kind of number, escape, key written
// twice and whitespace the generator of jsondiff_test.go writes, a scalar
// put in an array so that each is one the detection reads; each as it is,
// between whitespace of every kind, and corrupted three times: 10,000
// bodies, each under no Content-Type and under text/plain, taken for what
// they were taken for and decoded to what they were decoded to.
func TestDetectedRandomDocumentsAreDecodedAsBefore(t *testing.T) {
	documents := 2000
	if testing.Short() {
		documents = 400
	}
	g := &jsonGenerator{random: rand.New(rand.NewPCG(11, 2026))}
	margins := []string{"", " ", "\n", "\t\r\n ", "\f", "\v", "\u00a0", "\u0085", "\u2003"}
	var asJSON, otherwise int
	for range documents {
		document := g.document()
		if first := bytes.TrimSpace(document)[0]; first != '{' && first != '[' {
			document = append(append([]byte("["), document...), ']')
		}
		bodies := [][]byte{document, []byte(g.pick(margins...) + string(document) + g.pick(margins...))}
		for range 3 {
			bodies = append(bodies, g.corrupt(document))
		}
		for _, body := range bodies {
			// The detection reads neither the transform nor whether the
			// decoder is unset or auto, which the table above crosses.
			if compareDetected(t, body, []string{"", "text/plain"}, []string{g.pick(sniffTransforms...)})["json"] {
				asJSON++
			} else {
				otherwise++
			}
		}
	}
	t.Logf("%d bodies decoded as JSON, %d as something else or refused", asJSON, otherwise)
	if asJSON < documents || otherwise < documents {
		t.Fatalf("%d bodies were decoded as JSON and %d as something else or not at all: the generator no longer writes both", asJSON, otherwise)
	}
}

// A JSON body is read once, whatever chose its decoder: detected from its
// content, under no Content-Type or under one that says nothing, it costs
// the allocations it costs a collector whose decoder is json, and a few for
// what the detection keeps. Read by encoding/json first, to see whether it is
// JSON, it cost 46 allocations for each of these items where it now costs
// 10.
func TestDetectedJSONIsReadOnce(t *testing.T) {
	const items = 1000
	var body strings.Builder
	body.WriteString(`{"items":[`)
	for i := range items {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"item-%d","region":"eu-%d","kind":"k%d","value":%d,"size":%d,"extra":{"a":1,"b":"two","c":[1,2,3]}}`, i, i%7, i%3, i, i*1024)
	}
	body.WriteString("]}")
	named := allocationsFor(t, model.Collector{Decoder: model.DecoderConfig{Type: "json"}}, "application/json", body.String(), 1)
	for _, contentType := range []string{"", "text/plain", "application/octet-stream"} {
		for _, decoder := range []string{"", "auto"} {
			detected := allocationsFor(t, model.Collector{Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: "jq"}}, contentType, body.String(), 1)
			if detected > named+8 {
				t.Errorf("under %q, decoder %q: %.0f allocations, and %.0f with the decoder json: the body is read more than once", contentType, decoder, detected, named)
			}
		}
	}
}
