package decode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// decodeFailure is the error Decode refuses a body with, read by the decoder
// named, and the error the decoder made of it, before it was bounded.
func decodeFailure(t *testing.T, kind, body string, c model.Collector) (bounded, made error) {
	t.Helper()
	c.Decoder.Type = kind
	response := func() *fetch.HTTPResponse {
		return &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: http.Header{}}
	}
	_, bounded = Decode(response(), &c)
	_, made = decodeBody(response(), &c)
	if bounded == nil || made == nil {
		t.Fatalf("%s decodes %.100q: %v, %v", kind, body, bounded, made)
	}
	return bounded, made
}

// The error of a body that cannot be decoded is no longer than 2,000 bytes,
// whichever decoder read it and whatever the body holds: an element, an
// entity, a column's name, a sample's value, a label's name and its value, a
// metric's type, a carbon line and its value, a series' name and a point of
// a megabyte each were in the error whole, a megabyte and more of one line
// in the log and in the answer to the scraper. The error starts as it did,
// naming the problem, and ends with how long the whole was; it is recognised
// by a text no longer, which ends with the mark for the length and has the
// mark for the line, so the same mistake with a token half as long, on
// another line, is the same failure to the log. A JSON error, which names a
// character and where, is as short as it was.
func TestADecodeErrorIsNoLongerThanTwoThousandBytesWhateverTheBody(t *testing.T) {
	for name, tc := range map[string]struct {
		kind string
		body func(token string) string
		says string
	}{
		"xml, an element closed by another": {"xml", func(token string) string { return "<a><" + token + "></b></a>" }, "XML decode: XML syntax error on line 1: element <aaaa"},
		"xml, an entity":                    {"xml", func(token string) string { return "<a>&" + token + ";</a>" }, "XML decode: XML syntax error on line 1: invalid character entity &aaaa"},
		"csv, a column named twice":         {"csv", func(token string) string { return token + "," + token + "\n1,2\n" }, `CSV header names column "aaaa`},
		"prometheus, a value":               {"prometheus", func(token string) string { return "m " + token + "\n" }, `decoding Prometheus exposition: text format parsing error in line 1: expected float as value, got "aaaa`},
		"prometheus, a timestamp":           {"prometheus", func(token string) string { return "m 1 " + token + "\n" }, `decoding Prometheus exposition: text format parsing error in line 1: expected integer as timestamp, got "aaaa`},
		"prometheus, a label's name":        {"prometheus", func(token string) string { return "m{" + token + "} 1\n" }, `decoding Prometheus exposition: text format parsing error in line 1: expected '=' after label name "aaaa`},
		"prometheus, a label's value":       {"prometheus", func(token string) string { return "m{a=\"" + token + "\n" }, `decoding Prometheus exposition: text format parsing error in line 1: label value "aaaa`},
		"prometheus, a metric's type":       {"prometheus", func(token string) string { return "# TYPE m " + token + "\n" }, `decoding Prometheus exposition: text format parsing error in line 1: unknown metric type "aaaa`},
		"prometheus, a second HELP":         {"prometheus", func(token string) string { return "# HELP " + token + " x\n# HELP " + token + " y\n" }, `decoding Prometheus exposition: text format parsing error in line 2: second HELP line for metric name "aaaa`},
		"carbon, a line of too many fields": {"graphite", func(token string) string { return "a b c d " + token + "\n" }, `carbon line 1: "a b c d aaaa`},
		"carbon, a value":                   {"graphite", func(token string) string { return "a " + token + " 1\n" }, `carbon line 1: the value "aaaa`},
		"carbon, a tag":                     {"graphite", func(token string) string { return "a;" + token + " 1 1\n" }, `carbon line 1: the series "a;aaaa`},
		"graphite render, a series' name":   {"graphite", func(token string) string { return `[{"target": "` + token + `", "datapoints": [[1]]}]` }, `graphite render JSON: series "aaaa`},
		"graphite render, a point":          {"graphite", func(token string) string { return `[{"target": "t", "datapoints": [["` + token + `", 1]]}]` }, `graphite render JSON: series "t" point 0 is [aaaa`},
		// Ten problems, each of the start of its key, are over the bound
		// together where every byte of the key is written as four.
		"yaml, ten problems of keys of control characters": {"yaml", func(token string) string {
			return strings.Repeat(`? "`+strings.ReplaceAll(token[:len(token)/64], "a", `\x01`)+"\"\n: 1\n", 6)
		},
			"YAML decode: yaml: unmarshal errors:\n  line 3: mapping key \"\\x01\\x01"},
	} {
		long := strings.Repeat("a", 1<<20)
		err, made := decodeFailure(t, tc.kind, tc.body(long), model.Collector{})
		text, same := err.Error(), model.SameFailureText(err)
		mark := fmt.Sprintf("... (%d bytes)", len(made.Error()))
		if len(made.Error()) <= maxFailureBytes {
			t.Fatalf("%s: the decoder's error is %d bytes: %s", name, len(made.Error()), made)
		}
		if len(text) != maxFailureBytes || !strings.HasPrefix(text, tc.says) || !strings.HasSuffix(text, mark) || !strings.HasPrefix(made.Error(), strings.TrimSuffix(text, mark)) {
			t.Errorf("%s: the error is %d bytes, %.200q ... %q, want %d that start %q and end %q", name, len(text), text, text[max(0, len(text)-40):], maxFailureBytes, tc.says, mark)
		}
		if len(same) > maxFailureBytes || !strings.Contains(same, "... (# bytes)") || tc.kind != "yaml" && !strings.HasSuffix(same, "... (# bytes)") || strings.Contains(same, "line 1:") || strings.Contains(same, "line 3:") {
			t.Errorf("%s: the failure is recognised by %d bytes, %.200q ... %q", name, len(same), same, same[max(0, len(same)-40):])
		}
		moved := tc.body(long[:1<<19])
		if tc.kind == "prometheus" || tc.kind == "graphite" && !strings.HasPrefix(moved, "[") {
			moved = "\n" + moved
		}
		again, _ := decodeFailure(t, tc.kind, moved, model.Collector{})
		if got := model.SameFailureText(again); got != same || again.Error() == text {
			t.Errorf("%s: with a token half as long the error is %.200q ... %q, recognised by %.200q ... %q, want another length and the same failure", name, again, again.Error()[max(0, len(again.Error())-40):], got, got[max(0, len(got)-40):])
		}
	}
	long := strings.Repeat("a", 1<<20)
	for _, body := range []string{`{"a": ` + long + `}`, `{"a": "` + long, `{` + long + `: 1}`} {
		err, made := decodeFailure(t, "json", body, model.Collector{})
		if len(err.Error()) > 100 || err.Error() != made.Error() {
			t.Errorf("a JSON body is refused with %d bytes: %.200v", len(err.Error()), err)
		}
	}
}

// A decode error within the bound is the decoder's own, to the letter, in
// what it is recognised by, in its type and in being a limit that was
// exceeded or not, whichever decoder made it: the errors each decoder makes
// of a short mistaken body, and that of the series limit, are what they
// were before any was bounded.
func TestADecodeErrorWithinTheBoundIsTheDecodersOwn(t *testing.T) {
	limited := model.Collector{Transform: model.TransformConfig{Type: "prometheus"}, Limits: model.Limits{MaxMetrics: 1}}
	exceeded := 0
	for _, tc := range []struct {
		kind, body string
		c          model.Collector
	}{
		{"json", `{"a": nope}`, model.Collector{}}, {"json", `{"a": 1} {"b": 2}`, model.Collector{}}, {"json", `{"a": "`, model.Collector{}},
		{"yaml", "a: 1\na: 2\n", model.Collector{}}, {"yaml", "a: [1, 2\n", model.Collector{}}, {"yaml", "a: !!int nope\n", model.Collector{}}, {"yaml", "a: 1\n---\nb: 2\n", model.Collector{}},
		{"yaml", "? [a, b]\n: 1\nc: 2\n", model.Collector{}}, {"yaml", "a: 1\n2: 3\n<<: {[x]: 1}\n", model.Collector{}}, {"yaml", strings.Repeat("a: 1\n", 40), model.Collector{}},
		{"xml", "<a><b></c></a>", model.Collector{}}, {"xml", "<a>&nope;</a>", model.Collector{}}, {"xml", "<a", model.Collector{}},
		{"csv", "a,b\n1,\"2\"x\n", model.Collector{}}, {"csv", "a,a\n1,2\n", model.Collector{}}, {"csv", "a,\n1,2\n", model.Collector{}}, {"csv", "a,b\n1,2,3\n", model.Collector{}},
		{"prometheus", "m nope\n", model.Collector{}}, {"prometheus", "ok 1\nm{a=\"b} 1\n", model.Collector{}}, {"prometheus", "# TYPE m nope\n", model.Collector{}},
		{"prometheus", "# TYPE m histogram\nm_sum{a=\"b\"} 1\nm_sum{a=\"b\"} 1\n", model.Collector{}}, {"prometheus", "a 1\nb 2\nc 3\n", limited},
		{"graphite", "a b c d e\n", model.Collector{}}, {"graphite", "a nope 1\n", model.Collector{}}, {"graphite", "a;b 1 1\n", model.Collector{}},
		{"graphite", `[{"target": "t", "datapoints": [["x", 1]]}]`, model.Collector{}}, {"graphite", `[{"target": "t", "datapoints": [[1]]}]`, model.Collector{}},
		{"nope", "x", model.Collector{}},
	} {
		err, made := decodeFailure(t, tc.kind, tc.body, tc.c)
		if err.Error() != made.Error() || model.SameFailureText(err) != model.SameFailureText(made) || fmt.Sprintf("%T", err) != fmt.Sprintf("%T", made) || errors.Is(err, model.ErrLimitExceeded) != errors.Is(made, model.ErrLimitExceeded) {
			t.Errorf("%s refuses %q with %T %q, recognised by %q; the decoder made %T %q, recognised by %q", tc.kind, tc.body, err, err, model.SameFailureText(err), made, made, model.SameFailureText(made))
		}
		if bounded := boundedFailure(made); bounded != made { //nolint:errorlint // the very error, not one that wraps it
			t.Errorf("%s: the error %q is not the decoder's own once bounded: %q", tc.kind, made, bounded)
		}
		if errors.Is(err, model.ErrLimitExceeded) {
			exceeded++
		}
	}
	if exceeded != 1 {
		t.Errorf("%d of the errors are a limit that was exceeded, want the one of the series limit", exceeded)
	}
}

// An error of exactly 2,000 bytes is left as it is and one of 2,001 is cut
// to 2,000, its length said. The cut error is a new one: it is neither the
// long error nor one that wraps it or what that wrapped, so nothing holds
// the long text; that it is a limit that was exceeded, which the probe
// counts apart, is kept. What the failure is recognised by is cut where it
// is itself over 2,000 bytes, whatever the error's length: an error that
// names a line and is over the bound only by the line's digits is recognised
// by its text whole, with the mark for the line, as it is on a line of
// fewer digits. No error is no error.
func TestAnErrorOverTheBoundIsCutAndLetGoOf(t *testing.T) {
	if boundedFailure(nil) != nil {
		t.Error("no error is bounded to an error")
	}
	exact := errors.New(strings.Repeat("e", maxFailureBytes))
	if got := boundedFailure(exact); got != exact { //nolint:errorlint // the very error
		t.Errorf("an error of %d bytes is not left as it is: %.100v", maxFailureBytes, got)
	}
	sentinel := errors.New("the target said so")
	over := fmt.Errorf("%s: %w", strings.Repeat("e", maxFailureBytes-len(sentinel.Error())-1), sentinel)
	got := boundedFailure(over)
	if text, want := got.Error(), over.Error()[:maxFailureBytes-len("... (2001 bytes)")]+"... (2001 bytes)"; len(over.Error()) != maxFailureBytes+1 || text != want {
		t.Errorf("an error of %d bytes is cut to %d: %.100q ... %q", len(over.Error()), len(text), text, text[max(0, len(text)-40):])
	}
	if same, want := model.SameFailureText(got), over.Error()[:maxFailureBytes-len("... (# bytes)")]+"... (# bytes)"; same != want {
		t.Errorf("the cut error is recognised by %d bytes: %.100q ... %q", len(same), same, same[max(0, len(same)-40):])
	}
	if errors.Is(got, over) || errors.Is(got, sentinel) || errors.Is(got, model.ErrLimitExceeded) {
		t.Errorf("the cut error still is, or wraps, the long one: %.100v", got)
	}
	limit := boundedFailure(model.MarkError(over, model.ErrLimitExceeded))
	if !errors.Is(limit, model.ErrLimitExceeded) || limit.Error() != got.Error() || model.SameFailureText(limit) != model.SameFailureText(got) || errors.Is(limit, sentinel) {
		t.Errorf("a limit that was exceeded, cut, is %.100v, recognised by %.100q", limit, model.SameFailureText(limit))
	}
	// Over the bound by the digits of its line alone.
	said := strings.Repeat("e", maxFailureBytes-len("line 7: "))
	short, long := boundedFailure(model.Errorf("line %d: %s", model.Position(7), said)), boundedFailure(model.Errorf("line %d: %s", model.Position(123456), said))
	if short.Error() != "line 7: "+said || !strings.HasSuffix(long.Error(), "... (2005 bytes)") || len(long.Error()) != maxFailureBytes {
		t.Errorf("the errors are %d and %d bytes, ending %q and %q", len(short.Error()), len(long.Error()), short.Error()[len(short.Error())-30:], long.Error()[len(long.Error())-30:])
	}
	if same := model.SameFailureText(long); same != model.SameFailureText(short) || same != "line #: "+said {
		t.Errorf("on a line of more digits the failure is recognised by %d bytes, ending %q, want the text whole with the mark for the line", len(same), same[max(0, len(same)-30):])
	}
}

// The line a decoder leaves out of a body it decodes is reported, and
// logged as a warning the failure log remembers, in an error bounded like a
// decode error: a carbon line of a megabyte that is skipped under
// invalid_lines: skip, and a sample of a histogram whose name is a megabyte
// long and that is none of its samples, were reported in a megabyte and in
// five. A short line is reported as it was.
func TestTheFirstLineADecoderLeavesOutIsReportedInABoundedError(t *testing.T) {
	skipping := model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}}
	skipping.Response.Graphite.InvalidLines = "skip"
	exposition := model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}}
	stray := func(name string) string {
		return "# TYPE " + name + " histogram\n" + name + "_bucket{le=\"+Inf\"} 1\n" + name + "_sum 1\n" + name + "_count 1\n" + name + "{a=\"b\"} 1\n"
	}
	first := func(c model.Collector, body string) error {
		t.Helper()
		d, err := Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: http.Header{}}, &c)
		switch {
		case err != nil:
			t.Fatalf("%.100q is refused: %.300v", body, err)
		case d.Graphite != nil && d.Graphite.FirstSkipped != nil:
			return d.Graphite.FirstSkipped
		case d.Prometheus != nil && d.Prometheus.FirstLeftOut != nil:
			return d.Prometheus.FirstLeftOut
		}
		t.Fatalf("%.100q: no line is reported left out", body)
		return nil
	}
	long := strings.Repeat("a", 1<<20)
	for name, tc := range map[string]struct {
		c           model.Collector
		body        func(token string) string
		says, short string
	}{
		"a carbon line": {skipping, func(token string) string { return "a b c d " + token + "\nok 1 1\n" }, `carbon line 1: "a b c d aaaa`,
			`carbon line 1: "a b c d a" has 5 fields; want <path> <value> <timestamp>`},
		"a sample of a histogram": {exposition, stray, "line 5: expected aaaa",
			"line 5: expected a_bucket with an le label, a_sum or a_count as a sample of the histogram a, got a"},
	} {
		err := first(tc.c, tc.body(long))
		text, same := err.Error(), model.SameFailureText(err)
		if len(text) != maxFailureBytes || !strings.HasPrefix(text, tc.says) || !strings.HasSuffix(text, " bytes)") || len(same) > maxFailureBytes || !strings.HasSuffix(same, "... (# bytes)") {
			t.Errorf("%s of a megabyte is reported in %d bytes, %.100q ... %q, recognised by %d bytes", name, len(text), text, text[max(0, len(text)-40):], len(same))
		}
		if again := first(tc.c, "\n"+tc.body(long[:1<<19])); model.SameFailureText(again) != same || again.Error() == text {
			t.Errorf("%s half as long, a line further, is recognised by %.100q, want the same failure with another length", name, model.SameFailureText(again))
		}
		if err := first(tc.c, tc.body("a")); err.Error() != tc.short {
			t.Errorf("%s that is short is reported as %q, want %q", name, err, tc.short)
		}
	}
}
