package decode

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// valueCutCase is one error of a decoder that shows a value of the body: the
// decoder, the body that provokes it with a token in the value's place, the
// same further into the body by one line or one item, and the error, with
// the value as the error is to show it (quoted or bare) and with the
// positions it names as at gives them.
type valueCutCase struct {
	name, kind string
	header     http.Header
	body       func(token string, further bool) string
	want       func(quoted, bare func(string) string, token string, at func(int) string) string
}

// lineFurther is a body that is one line further into the response when it
// is to be.
func lineFurther(body func(token string) string) func(string, bool) string {
	return func(token string, further bool) string {
		if further {
			return "\n" + body(token)
		}
		return body(token)
	}
}

// valueCutCases are the errors of the exporter's own decoders that show a
// value of the body, each once.
func valueCutCases() []valueCutCase {
	const prom, open, carbon, render = "decoding Prometheus exposition: text format parsing error in line ", "decoding OpenMetrics exposition: text format parsing error in line ", "carbon line ", "graphite render JSON: "
	type show = func(string) string
	type place = func(int) string
	series := func(target, points string) func(string, bool) string {
		return func(token string, further bool) string {
			before := ""
			if further {
				before = `{"target": "ok", "datapoints": [[1, 2]]}, `
			}
			return "[" + before + `{"target": "` + strings.ReplaceAll(target, "T", token) + `", "datapoints": ` + strings.ReplaceAll(points, "T", token) + "}]"
		}
	}
	point := func(points string) func(string, bool) string {
		return func(token string, further bool) string {
			before := ""
			if further {
				before = "[1, 2], "
			}
			return `[{"target": "` + token + `", "datapoints": [` + before + strings.ReplaceAll(points, "T", token) + "]}]"
		}
	}
	zeros := func(token string) string {
		return strings.Repeat("0", len(token)-1) + strings.ReplaceAll(token[:1], "a", "0")
	}
	return []valueCutCase{
		{"a second HELP", "prometheus", nil, lineFurther(func(t string) string { return "# HELP " + t + " x\n# HELP " + t + " y\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(2) + ": second HELP line for metric name " + q(t)
			}},
		{"a second TYPE", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE " + t + " gauge\n# TYPE " + t + " gauge\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(2) + ": second TYPE line for metric name " + q(t) + ", or TYPE reported after samples"
			}},
		{"a metric's type", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE m " + t + "\n" }),
			func(q, _ show, t string, at place) string { return prom + at(1) + ": unknown metric type " + q(t) }},
		{"a sample's value", "prometheus", nil, lineFurther(func(t string) string { return "m " + t + "\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": expected float as value, got " + q(t)
			}},
		{"a sample's timestamp", "prometheus", nil, lineFurther(func(t string) string { return "m 1 " + t + "\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": expected integer as timestamp, got " + q(t)
			}},
		{"what follows a timestamp", "prometheus", nil, lineFurther(func(t string) string { return "m 1 2 " + t + "\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": spurious string after timestamp: " + q(t)
			}},
		{"a metric name that is no UTF-8", "prometheus", nil, lineFurther(func(t string) string { return "\"" + t + "\xff\" 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": invalid metric name " + q(t+"\xff")
			}},
		{"a bucket's bound", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE h histogram\nh_bucket{le=\"" + t + "\"} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(2) + ": expected float as value for 'le' label, got " + q(t)
			}},
		{"a quantile", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE s summary\ns{quantile=\"" + t + "\"} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(2) + ": expected float as value for 'quantile' label, got " + q(t)
			}},
		{"a count below nought", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE " + t + " histogram\n" + t + "_bucket{le=\"1\"} -1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(2) + ": expected a count from 0 to 2^64-1 for " + q(t) + ", got -1"
			}},
		{"a count that is a fraction", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE " + t + " summary\n" + t + "_count 1.5\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(2) + ": expected a whole number as the count for " + q(t) + ", got 1.5"
			}},
		{"a second _sum", "prometheus", nil, lineFurther(func(t string) string {
			return "# TYPE " + t + " histogram\n" + t + "_sum{" + t + "=\"" + t + "\"} 1\n" + t + "_sum{" + t + "=\"" + t + "\"} 1\n"
		}),
			func(q, b show, t string, at place) string {
				return prom + at(3) + ": second " + b(t) + "_sum sample for the histogram " + b(t) + "{" + b(t) + "=" + q(t) + "}"
			}},
		{"a second _count", "prometheus", nil, lineFurther(func(t string) string {
			return "# TYPE " + t + " summary\n" + t + "_count{a=\"" + t + "\"} 1\n" + t + "_count{a=\"" + t + "\"} 1\n"
		}),
			func(q, b show, t string, at place) string {
				return prom + at(3) + ": second " + b(t) + "_count sample for the summary " + b(t) + "{a=" + q(t) + "}"
			}},
		{"two buckets of one bound", "prometheus", nil, lineFurther(func(t string) string {
			return "# TYPE " + t + " histogram\n" + strings.Repeat(t+"_bucket{le=\"1\","+t+"=\""+t+"\"} 1\n", 2)
		}),
			func(q, b show, t string, at place) string {
				return "decoding Prometheus exposition: text format parsing error: the histogram " + b(t) + "{" + b(t) + "=" + q(t) + "}, which starts in line " + at(2) + ", has two buckets with the upper bound 1"
			}},
		{"two values of one quantile", "prometheus", nil, lineFurther(func(t string) string {
			return "# TYPE " + t + " summary\n" + strings.Repeat(t+"{quantile=\"0.5\"} 1\n", 2)
		}),
			func(_, b show, t string, at place) string {
				return "decoding Prometheus exposition: text format parsing error: the summary " + b(t) + ", which starts in line " + at(2) + ", has two values for the quantile 0.5"
			}},
		{"a label's name without a value", "prometheus", nil, lineFurther(func(t string) string { return "m{" + t + "} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": expected '=' after label name " + q(t)
			}},
		{"a second metric name", "prometheus", nil, lineFurther(func(t string) string { return "{" + t + "," + t + "} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": multiple metric names for metric " + q(t)
			}},
		{"a label's name that is no UTF-8", "prometheus", nil, lineFurther(func(t string) string { return "m{\"" + t + "\xff\"=\"1\"} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": invalid label name " + q(t+"\xff")
			}},
		{"a label's name written twice", "prometheus", nil, lineFurther(func(t string) string { return "m{" + t + "=\"1\"," + t + "=\"1\"} 1\n" }),
			func(q, _ show, t string, at place) string { return prom + at(1) + ": duplicate label name " + q(t) }},
		{"a label's value without quotes", "prometheus", nil, lineFurther(func(t string) string { return "m{" + t + "=1} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + `: expected '"' at start of the value of label ` + q(t)
			}},
		{"a label set that ends early", "prometheus", nil, lineFurther(func(t string) string { return "m{" + t + "=\"1\"\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": unexpected end of label set after label " + q(t)
			}},
		{"a label set that goes on", "prometheus", nil, lineFurther(func(t string) string { return "m{" + t + "=\"1\"x} 1\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + `: unexpected "x" after the value of label ` + q(t)
			}},
		{"a quoted name ending in a backslash", "prometheus", nil, lineFurther(func(t string) string { return "m{\"" + t + "\\\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": name " + q(t) + " ends in a lone backslash"
			}},
		{"a label's value ending in a backslash", "prometheus", nil, lineFurther(func(t string) string { return "m{a=\"" + t + "\\\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": label value " + q(t) + " ends in a lone backslash"
			}},
		{"a label's value that is not closed", "prometheus", nil, lineFurther(func(t string) string { return "m{a=\"" + t + "\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": label value " + q(t) + " contains unescaped new-line"
			}},
		{"a help text ending in a backslash", "prometheus", nil, lineFurther(func(t string) string { return "# HELP m " + t + "\\\n" }),
			func(q, _ show, t string, at place) string {
				return prom + at(1) + ": help text " + q(t) + " ends in a lone backslash"
			}},
		{"an OpenMetrics type", "prometheus", nil, lineFurther(func(t string) string { return "# TYPE m " + t + "\n# EOF\n" }),
			func(q, _ show, t string, at place) string { return open + at(1) + ": unknown metric type " + q(t) }},
		{"an OpenMetrics timestamp", "prometheus", nil, lineFurther(func(t string) string { return "m 1 " + t + "\n# EOF\n" }),
			func(q, _ show, t string, at place) string {
				return open + at(1) + ": expected a number of seconds as timestamp, got " + q(t)
			}},
		{"a carbon line of too many fields", "graphite", nil, lineFurther(func(t string) string { return "a b c d " + t + "\n" }),
			func(q, _ show, t string, at place) string {
				return carbon + at(1) + ": " + q("a b c d "+t) + " has 5 fields; want <path> <value> <timestamp>"
			}},
		{"a carbon value", "graphite", nil, lineFurther(func(t string) string { return "a " + t + " 1\n" }),
			func(q, _ show, t string, at place) string {
				return carbon + at(1) + ": the value " + q(t) + " is not a number"
			}},
		{"a carbon timestamp", "graphite", nil, lineFurther(func(t string) string { return "a 1 " + t + "\n" }),
			func(q, _ show, t string, at place) string {
				return carbon + at(1) + ": the timestamp " + q(t) + " is not a number of Unix seconds"
			}},
		{"a carbon timestamp in milliseconds", "graphite", nil, lineFurther(func(t string) string { return "a 1 " + zeros(t) + "1727000000000\n" }),
			func(q, _ show, t string, at place) string {
				return carbon + at(1) + ": the timestamp " + q(zeros(t)+"1727000000000") + " is in milliseconds, it seems; carbon lines take Unix seconds"
			}},
		{"a carbon series without a path", "graphite", nil, lineFurther(func(t string) string { return ";" + t + " 1 1\n" }),
			func(q, _ show, t string, at place) string {
				return carbon + at(1) + ": the series " + q(";"+t) + " has no path"
			}},
		{"a carbon tag", "graphite", nil, lineFurther(func(t string) string { return t + ";" + t + " 1 1\n" }),
			func(q, _ show, t string, at place) string {
				return carbon + at(1) + ": the series " + q(t+";"+t) + " has a tag " + q(t) + " that is not name=value"
			}},
		{"a render series without a path", "graphite", nil, series(";T", "[[1, 2]]"),
			func(q, _ show, t string, at place) string {
				return render + "series " + at(0) + ": the series " + q(";"+t) + " has no path"
			}},
		{"a render series' tag", "graphite", nil, series("T;T", "[[1, 2]]"),
			func(q, _ show, t string, at place) string {
				return render + "series " + at(0) + ": the series " + q(t+";"+t) + " has a tag " + q(t) + " that is not name=value"
			}},
		{"a render point of one element", "graphite", nil, point("[1]"),
			func(q, _ show, t string, at place) string {
				return render + "series " + q(t) + " point " + at(0) + " has 1 elements, not [value, timestamp]"
			}},
		{"a render point of text", "graphite", nil, point(`["T", 1]`),
			func(q, b show, t string, at place) string {
				return render + "series " + q(t) + " point " + at(0) + " is " + b("["+t+" 1]") + ", not [value, timestamp] as numbers"
			}},
		{"a render point of an object", "graphite", nil, point(`[1, {"T": ["T", null, true]}]`),
			func(q, b show, t string, at place) string {
				return render + "series " + q(t) + " point " + at(0) + " is " + b("[1 map["+t+":["+t+" <nil> true]]]") + ", not [value, timestamp] as numbers"
			}},
		// The columns are what the failure is, and are the same with a row more.
		{"a CSV column named twice", "csv", nil, func(t string, further bool) string {
			if further {
				return "x," + t + "," + t + "\n1,2,3\n4,5,6\n"
			}
			return "x," + t + "," + t + "\n1,2,3\n"
		},
			func(q, _ show, t string, _ place) string {
				return "CSV header names column " + q(t) + " twice, as columns 2 and 3; rename one, or set response.csv.header: false and read the columns by number"
			}},
	}
}

// decodeError is the error Decode refuses a body with, read by the decoder
// named, and the error the decoder made, before it was bounded.
func decodeError(t *testing.T, kind, body string, header http.Header) (bounded, made error) {
	t.Helper()
	c := model.Collector{}
	c.Decoder.Type = kind
	response := func() *fetch.HTTPResponse {
		headers := http.Header{}
		for name, values := range header {
			headers[name] = values
		}
		return &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: headers}
	}
	_, bounded = Decode(response(), &c)
	_, made = decodeBody(response(), &c)
	if bounded == nil || made == nil {
		t.Fatalf("%s decodes %.100q: %v, %v", kind, body, bounded, made)
	}
	return bounded, made
}

// longTokenBytes is how long the token is that the tests of a cut value give
// a decoder: 1 MiB, and 100 KiB under the race detector, where parsing a
// megabyte for each of some fifty messages took most of a minute of the ten
// a package's tests have in CI. Both lengths start with the digits 10, which
// the tests look for in the length an error names.
func longTokenBytes() int {
	if raceDetector {
		return 100 << 10
	}
	return 1 << 20
}

// Every error of the Prometheus, the Graphite and the CSV decoder that shows
// a value of the body shows no more than its first 64 bytes, with its length,
// and says after it what it said: with a token of 1 MiB for the value the
// error is the message whole, in a few hundred bytes, where it was the token
// whole and then, bounded, the first 2,000 bytes of that, without its end.
// It is the decoder's own error, not one cut at the bound. The failure is
// recognised by the message with the mark for the length and for where in
// the body it is, so a token half as long, a line or an item further, is the
// same failure, with its own length in its text, and a token that starts
// otherwise is another. A token of three bytes is shown whole, as it was, and
// one of 64 where it is the value: where the error shows it with more, as a
// carbon line with its fields, it is the value of over 64 bytes that is cut.
func TestADecoderShowsTheStartOfAValueAndKeepsWhatItSaysAfterIt(t *testing.T) {
	long := strings.Repeat("a", longTokenBytes())
	at := func(shift int) func(int) string {
		return func(place int) string { return strconv.Itoa(place + shift) }
	}
	mark := func(int) string { return model.MovingMark }
	quote, bare := model.QuoteValue, func(value string) string {
		if len(value) <= 64 {
			return value
		}
		return bareCut(value)
	}
	for _, tc := range valueCutCases() {
		err, made := decodeError(t, tc.kind, tc.body(long, false), tc.header)
		text, want := err.Error(), tc.want(quote, bare, long, at(0))
		if text != want || made.Error() != text || len(text) > 700 || strings.Count(text, "... (10") == 0 {
			t.Errorf("%s: a token of 1 MiB is refused in %d bytes, %.700q, want %q", tc.name, len(text), text, want)
			continue
		}
		same := model.SameFailureText(err)
		if want := lengthsMarked(tc.want(quote, bare, long, mark)); same != want {
			t.Errorf("%s: the failure is recognised by %q, want %q", tc.name, same, want)
		}
		half, _ := decodeError(t, tc.kind, tc.body(long[:len(long)/2], true), tc.header)
		if want := tc.want(quote, bare, long[:len(long)/2], at(1)); half.Error() != want || model.SameFailureText(half) != same {
			t.Errorf("%s: a token half as long, further into the body, is refused with %.700q, recognised by %q, want %q and the same failure", tc.name, half, model.SameFailureText(half), want)
		}
		if other, _ := decodeError(t, tc.kind, tc.body("b"+long[1:], false), tc.header); model.SameFailureText(other) == same {
			t.Errorf("%s: a token that starts otherwise is the same failure: %q", tc.name, same)
		}
		for _, token := range []string{long[:64], "aaa"} {
			short, made := decodeError(t, tc.kind, tc.body(token, false), tc.header)
			if want := tc.want(quote, bare, token, at(0)); short.Error() != want || made.Error() != want {
				t.Errorf("%s: a token of %d bytes is refused with %q, want %q", tc.name, len(token), short, want)
			}
			if want := lengthsMarked(tc.want(quote, bare, token, mark)); model.SameFailureText(short) != want {
				t.Errorf("%s: a token of %d bytes is recognised by %q, want %q", tc.name, len(token), model.SameFailureText(short), want)
			}
		}
		whole := func(value string) string { return value }
		if short, _ := decodeError(t, tc.kind, tc.body("aaa", false), tc.header); short.Error() != tc.want(strconv.Quote, whole, "aaa", at(0)) || model.SameFailureText(short) != tc.want(strconv.Quote, whole, "aaa", mark) {
			t.Errorf("%s: a token of three bytes is refused with %q, recognised by %q", tc.name, short, model.SameFailureText(short))
		}
	}
}

// The name of an encoding that is none is shown by its first 64 bytes and
// its length, and the error still says which names there are: named by the
// Content-Type of a response, where a name of 100 kB made an error of
// 100 kB, cut at the bound before its advice; by the XML declaration of a
// document; and by response.charset, which the configuration is refused
// with. A name of 64 bytes or fewer reads as it did, as %q quotes it.
func TestAnUnknownCharsetIsShownByItsStartAndStillSaysWhichNamesThereAre(t *testing.T) {
	const advice = "; use a name from the WHATWG Encoding Standard, such as utf-8, windows-1252, iso-8859-2, windows-1251, shift_jis or gbk"
	long := strings.Repeat("x", 100000)
	for _, name := range []string{long, long[:65], long[:64], "nope", "no pe", "n\"o", "\xff"} {
		want := "unsupported charset " + model.QuoteValue(name) + advice
		if len(name) <= 64 {
			want = fmt.Sprintf("unsupported charset %q", name) + advice
		}
		if err := CheckCharset(name); err == nil || err.Error() != want || model.SameFailureText(err) != lengthsMarked(want) {
			t.Errorf("response.charset of %d bytes is refused with %.300v, recognised by %.300q, want %.300q", len(name), err, model.SameFailureText(err), want)
		}
		if strings.ContainsAny(name, "\" ") || !utf8.ValidString(name) {
			continue
		}
		err, made := decodeError(t, "text", "x", http.Header{"Content-Type": {"text/plain; charset=" + name}})
		if err.Error() != want || made.Error() != want || len(want) > 300 {
			t.Errorf("a Content-Type naming a charset of %d bytes is refused with %.300v, want %.300q", len(name), err, want)
		}
		if len(name) > 1000 {
			// The declaration is read within the document's first kilobyte.
			continue
		}
		if err, _ := decodeError(t, "xml", `<?xml version="1.0" encoding="`+name+`"?><a/>`, nil); err.Error() != want {
			t.Errorf("an XML declaration naming an encoding of %d bytes is refused with %.300v, want %.300q", len(name), err, want)
		}
	}
	short, longer := CheckCharset(long), CheckCharset(long+long)
	if model.SameFailureText(short) != model.SameFailureText(longer) || short.Error() == longer.Error() || model.SameFailureText(CheckCharset("y"+long)) == model.SameFailureText(short) {
		t.Errorf("a longer name that starts the same is recognised by %.300q, and the name by %.300q", model.SameFailureText(longer), model.SameFailureText(short))
	}
}

// No text of the size of the value is made to refuse it: an exposition with
// a token of 1 MiB is parsed, refused, and its error read and asked what it
// is recognised by, in under 32 kB of allocations beyond what parsing the
// token itself copies of it, for each error the parser makes of a value,
// where reading the error alone made a megabyte and more each time; and so
// is a carbon line and a Graphite series' name of 1 MiB. The error of a
// point of the render API that holds 3 MiB of text is read in as little.
func TestRefusingALongValueMakesNoTextOfItsSize(t *testing.T) {
	if raceDetector {
		t.Skip("nothing can be said of allocations under the race detector")
	}
	const most = 32 << 10
	long := strings.Repeat("a", 1<<20)
	// parsing is how many copies of the token reading the body up to the
	// error makes, in whole MiB, which are the parse's and no part of the
	// error: a family's name, kept for the family; a series' labels, and the
	// signature a histogram's or a summary's series is found by; the text of
	// a TYPE line's type; a name, a value or a help text unescaped; and the
	// text strconv is given to read a number of, and the copy of it that
	// strconv's own error keeps.
	parsing := map[string]uint64{
		"a second HELP": 1, "a second TYPE": 1, "a metric's type": 1, "an OpenMetrics type": 1,
		"a sample's value": 2, "a sample's timestamp": 2, "an OpenMetrics timestamp": 2, "a bucket's bound": 2, "a quantile": 2,
		"a count below nought": 2, "a count that is a fraction": 2, "two values of one quantile": 2,
		"a second _sum": 11, "a second _count": 6, "two buckets of one bound": 11,
		"a quoted name ending in a backslash": 1, "a label's value ending in a backslash": 1, "a label's value that is not closed": 1, "a help text ending in a backslash": 6,
	}
	parsed := 0
	for _, tc := range valueCutCases() {
		if tc.kind != "prometheus" {
			continue
		}
		parsed++
		body := []byte(tc.body(long, false))
		options := promOptions{openMetrics: strings.HasSuffix(string(body), "# EOF\n")}
		var text, same string
		allocated := alloctest.BytesAtMost(1, most+parsing[tc.name]<<20, func() {
			_, err := parseExposition(body, options)
			if err == nil {
				t.Fatalf("%s: the exposition is accepted", tc.name)
			}
			text, same = err.Error(), model.SameFailureText(err)
		})
		if allocated > most+parsing[tc.name]<<20 || len(text) > 700 || len(same) > 700 {
			t.Errorf("%s: refusing a token of 1 MiB allocates %d bytes, of which %d MiB to parse it, for an error of %d bytes, recognised by %d", tc.name, allocated, parsing[tc.name], len(text), len(same))
		}
	}
	if parsed < 25 {
		t.Errorf("%d expositions were parsed: the table should hold every error the parser makes of a value", parsed)
	}
	now := time.Unix(1727000000, 0)
	fields, value, stamp, pathless, tagged := "a b c d "+long, "a "+long+" 1", "a 1 "+long, ";"+long, "a;"+long
	render := []byte(`[{"target": "t", "datapoints": [["` + long + `", {"` + long + `": ["` + long + `", null, true]}]]}]`)
	for name, tc := range map[string]struct {
		refuse  func() error
		parsing uint64
	}{
		"a carbon line of too many fields": {func() error { _, _, _, err := parseCarbonLine(fields, now); return err }, 0},
		// The copy strconv's own error keeps of the text it could not read.
		"a carbon value":          {func() error { _, _, _, err := parseCarbonLine(value, now); return err }, 1},
		"a carbon timestamp":      {func() error { _, _, _, err := parseCarbonLine(stamp, now); return err }, 1},
		"a series without a path": {func() error { _, _, err := parseGraphitePath(pathless); return err }, 0},
		"a series' tag":           {func() error { _, _, err := parseGraphitePath(tagged); return err }, 0},
	} {
		var text, same string
		allocated := alloctest.BytesAtMost(1, most+tc.parsing<<20, func() {
			err := tc.refuse()
			if err == nil {
				t.Fatalf("%s is accepted", name)
			}
			text, same = err.Error(), model.SameFailureText(err)
		})
		if allocated > most+tc.parsing<<20 || len(text) > 400 || len(same) > 400 {
			t.Errorf("%s of 1 MiB is refused in %d bytes of allocations, of which %d MiB to parse it, for an error of %d bytes, recognised by %d", name, allocated, tc.parsing, len(text), len(same))
		}
	}
	// Reading the JSON of an answer copies it several times over, so of a
	// point that is no point it is the error that is measured: its text and
	// what it is recognised by, which were the point written out, each time
	// either was asked for.
	_, err := parseGraphiteRender(render, &GraphiteReport{})
	if err == nil {
		t.Fatal("a point of text and an object is accepted")
	}
	var text, same string
	if allocated := alloctest.BytesAtMost(1, most, func() { text, same = err.Error(), model.SameFailureText(err) }); allocated > most || len(text) > 400 || len(same) > 400 {
		t.Errorf("the error of a point of 3 MiB is read in %d bytes of allocations: %d bytes, recognised by %d", allocated, len(text), len(same))
	}
}
