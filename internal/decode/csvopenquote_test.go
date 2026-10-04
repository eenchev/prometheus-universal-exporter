package decode

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// carriageReturnsAsLineEndsBeforeOpenQuotes is carriageReturnsAsLineEnds as
// it was before the carriage returns after a quoted field left open ended
// lines: every one after such a quote was the field's text.
func carriageReturnsAsLineEndsBeforeOpenQuotes(body []byte, delimiter rune, trimSpace bool) []byte {
	last := len(body) - 1
	first := loneCarriageReturn(body, 0)
	if first < 0 || first == last {
		return body
	}
	if bytes.IndexByte(body, '"') < 0 {
		out := bytes.Clone(body)
		for at := first; at >= 0 && at < last; at = loneCarriageReturn(body, at+1) {
			out[at] = '\n'
		}
		return out
	}
	alone := func(i int) bool {
		return body[i] == '\r' && (i+1 == len(body) || body[i+1] != '\n')
	}
	out, copied := body, false
	for i := 0; i < len(body); {
		if trimSpace {
			for i < len(body) {
				r, width := utf8.DecodeRune(body[i:])
				if r == delimiter || r == '\n' || !unicode.IsSpace(r) || alone(i) {
					break
				}
				i += width
			}
		}
		if i < len(body) && body[i] == '"' {
			for i++; i < len(body); i++ {
				if body[i] != '"' {
					continue
				}
				if i+1 < len(body) && body[i+1] == '"' {
					i++
					continue
				}
				i++
				break
			}
		}
		for i < len(body) {
			c := body[i]
			if c == '\r' && alone(i) {
				if i < last {
					if !copied {
						out, copied = bytes.Clone(body), true
					}
					out[i] = '\n'
				}
				i++
				break
			}
			if c < utf8.RuneSelf {
				i++
				if rune(c) == delimiter || c == '\n' {
					break
				}
				continue
			}
			r, width := utf8.DecodeRune(body[i:])
			i += width
			if r == delimiter {
				break
			}
		}
	}
	return out
}

// readCSVBeforeOpenQuotes is readCSV as it was then.
func readCSVBeforeOpenQuotes(body []byte, delimiter rune, trimSpace bool) ([][]string, error) {
	return readCSVBeforeCarriageReturns(carriageReturnsAsLineEndsBeforeOpenQuotes(body, delimiter, trimSpace), delimiter, trimSpace)
}

// loneCarriageReturnsAsLineFeeds is body from the byte at from on with every
// carriage return that no line feed follows written as a line feed.
func loneCarriageReturnsAsLineFeeds(body string, from int) string {
	rest := strings.ReplaceAll(body[from:], "\r\n", "\x00")
	return body[:from] + strings.ReplaceAll(strings.ReplaceAll(rest, "\r", "\n"), "\x00", "\r\n")
}

// A quoted field left open in a body whose lines end with a carriage return
// alone fails with the error the same body gives with line feeds: the line
// the body ends on and the column there, where it named the field's own line
// and, as the column, how far the end of the body was from it. That is so
// with a carriage return as the body's last byte and without one, in a body
// whose only lone carriage return is its last byte, with a doubled quote
// after the open one, after a bare quote that has the body read leniently,
// under trim_space with blanks before the quote, and with a tab as the
// delimiter.
func TestCSVQuoteLeftOpenFailsAsWithLineFeeds(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		cfg        model.CSVConfig
		want       string
	}{
		{name: "lines after it", body: "h,k\ra,b\rc,\"d\re,f\rg,h\r",
			want: `CSV decode: record on line 3; parse error on line 5, column 5: extraneous or missing " in quoted-field`},
		{name: "and no carriage return at the end", body: "h,k\ra,b\rc,\"d\re,f\rg,h",
			want: `CSV decode: record on line 3; parse error on line 5, column 4: extraneous or missing " in quoted-field`},
		{name: "on the last line", body: "host,used\rweb01,72\rdb1,\"5\r",
			want: `CSV decode: parse error on line 3, column 8: extraneous or missing " in quoted-field`},
		{name: "the only carriage return is the last byte", body: "h,k\na,\"b\r",
			want: `CSV decode: parse error on line 2, column 6: extraneous or missing " in quoted-field`},
		{name: "a doubled quote after it closes nothing", body: "h,k\rc,\"d \"\"e\"\"\rf,g\r",
			want: `CSV decode: record on line 2; parse error on line 3, column 5: extraneous or missing " in quoted-field`},
		{name: "after a bare quote", body: "h,k\r5\" disk,b\rc,\"d\re,f\rg,h\r",
			want: `CSV decode: record on line 3; parse error on line 5, column 5: extraneous or missing " in quoted-field`},
		{name: "mixed with CRLF and line feeds", body: "h,k\r\na,b\rc,\"d\r\ne,f\ng,h\r",
			want: `CSV decode: record on line 3; parse error on line 5, column 5: extraneous or missing " in quoted-field`},
		{name: "under trim_space, after blanks", body: "h,k\ra,b\rc,  \"d\re,f\rg,h\r", cfg: model.CSVConfig{TrimSpace: true},
			want: `CSV decode: record on line 3; parse error on line 5, column 5: extraneous or missing " in quoted-field`},
		{name: "with tabs under trim_space", body: "h\tk\ra\tb\rc\t \"d\re\tf\rg\th\r", cfg: model.CSVConfig{Delimiter: "\t", TrimSpace: true},
			want: `CSV decode: record on line 3; parse error on line 5, column 5: extraneous or missing " in quoted-field`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodedCSV(t, tc.body, tc.cfg)
			if err == nil || err.Error() != tc.want {
				t.Errorf("%q failed with %v, want %s", tc.body, err, tc.want)
			}
			twin := loneCarriageReturnsAsLineFeeds(tc.body, 0)
			if _, twinErr := decodedCSV(t, twin, tc.cfg); twinErr == nil || err == nil || twinErr.Error() != err.Error() {
				t.Errorf("%q failed with %v, and with line feeds, %q, with %v", tc.body, err, twin, twinErr)
			}
		})
	}
}

// Whatever comes before a quoted field left open and after it, the body is
// refused as the reader as it was refuses the same body with line feeds in
// place of the lone carriage returns, those after the open quote among them
// and those inside the closed quoted fields before it not: the same words,
// lines and columns. The lines before it hold quoted fields with carriage
// returns, doubled quotes, blanks around their quotes and a bare quote; the
// field is open at the start of a line and after a delimiter, with blanks
// before its quote and with a doubled quote in it; what follows it has
// delimiters, doubled quotes and empty lines, ended four ways; under every
// delimiter, with trim_space and without.
func TestCSVQuoteLeftOpenIsRefusedAsItsLineFeedTwin(t *testing.T) {
	// A line before the open field, and the same with what the former
	// reader is to read in its place.
	type line struct{ written, former string }
	before := []line{{"", ""}, {"a|b\r", "a|b\n"}, {"a|b\r\n", "a|b\r\n"}, {"\"c\rr\"|x\r", "\"c\rr\"|x\n"}, {"\"x\"\"y\"|\"q\" \r", "\"x\"\"y\"|\"q\" \n"},
		{"5\" disk|b\r", "5\" disk|b\n"}, {"a|\"two\nlines\"\r\r", "a|\"two\nlines\"\n\n"}}
	open := []string{`"d`, `c|"d`, `c| "d`, `"d ""e""`, "\"d\r", "c|\"\r"}
	after := []string{"", "\r", "\n", "\r\n", "\re|f\rg|h", "\re|f\rg|h\r", "|x\r\r\"\"|y\r\n\"\"\"\"\r", "\r\"\"\r", " \r \r"}
	compared, moved := 0, 0
	for _, delimiter := range []rune{',', ';', '\t', ' ', '\u00a0'} {
		for _, trim := range []bool{false, true} {
			for _, first := range before {
				for _, second := range before {
					for _, field := range open {
						for _, rest := range after {
							written := func(s string) string { return strings.ReplaceAll(s, "|", string(delimiter)) }
							head, formerHead := written("h|k\r"+first.written+second.written), written("h|k\n"+first.former+second.former)
							body := head + written(field+rest)
							// A field whose quote follows a blank is a quoted
							// one under trim_space alone, unless the blank is
							// the delimiter.
							if strings.Contains(field, ` "`) && !trim && delimiter != ' ' {
								continue
							}
							twin := formerHead + loneCarriageReturnsAsLineFeeds(written(field+rest), 0)
							_, wantErr := readCSVBeforeCarriageReturns([]byte(twin), delimiter, trim)
							given := []byte(body)
							rows, err := readCSV(given, delimiter, trim)
							if string(given) != body {
								t.Fatalf("delimiter %q, trim_space %v: reading %q changed it to %q", delimiter, trim, body, given)
							}
							if wantErr == nil || err == nil || err.Error() != wantErr.Error() {
								t.Fatalf("delimiter %q, trim_space %v, %q: %q, %v; with line feeds, %q, it was refused: %v", delimiter, trim, body, rows, err, twin, wantErr)
							}
							compared++
							if _, was := readCSVBeforeOpenQuotes([]byte(body), delimiter, trim); was == nil {
								t.Fatalf("delimiter %q, trim_space %v, %q: read before", delimiter, trim, body)
							} else if was.Error() != err.Error() {
								moved++
							}
						}
					}
				}
			}
		}
	}
	if compared < 20000 || moved < 10000 {
		t.Errorf("%d bodies compared, %d of them with another error than before", compared, moved)
	}
}

// What the reader accepts is read as it was: of the generated bodies of
// TestCSVReadsCarriageReturnLinesAsItReadLineFeedLines, with fields left
// open among them, and of every fixture, each is handed to the reader as it
// was handed before, byte for byte, unless the reader refuses it, before and
// now; and a body the reader accepted has the rows it had. So the line of
// every field of such a body is the line it was too.
func TestCSVBodiesReadBeforeQuotesLeftOpenEndedLinesAreReadTheSame(t *testing.T) {
	atoms := []string{`a`, ``, ` b c `, `"q"`, ` "q"`, `"q" `, "\"c\rr\"", "\"c\rr|\r\n\" ", " \"c\rr\"", "\"two\nlines\"", `"x""y"`, `5" disk`, `"q" x`, `"st"ray"`, "\t\"t\"",
		`"open`, ` "open`, `"open ""x""`, "\"open\r"}
	ends := []string{"\n", "\r\n", "\r", "\r\r", "\r\r\n"}
	lasts := []string{"\n", "\r\n", "\r", ""}
	accepted, refused, changed := 0, 0, 0
	compare := func(what string, body []byte, delimiter rune, trim bool) {
		t.Helper()
		was, now := carriageReturnsAsLineEndsBeforeOpenQuotes(body, delimiter, trim), carriageReturnsAsLineEnds(body, delimiter, trim)
		want, wantErr := readCSVBeforeOpenQuotes(bytes.Clone(body), delimiter, trim)
		got, err := readCSV(bytes.Clone(body), delimiter, trim)
		if (err == nil) != (wantErr == nil) || !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Fatalf("%s, delimiter %q, trim_space %v: read as %q, %v; it was %q, %v", what, delimiter, trim, got, err, want, wantErr)
		}
		if !bytes.Equal(was, now) {
			changed++
			if err == nil {
				t.Fatalf("%s, delimiter %q, trim_space %v: the reader accepts it, and is handed %q where it was handed %q", what, delimiter, trim, now, was)
			}
		}
		if err == nil {
			accepted++
		} else {
			refused++
		}
	}
	for _, delimiter := range []rune{',', ';', '\t', ' ', '\u00a0'} {
		for _, trim := range []bool{false, true} {
			for _, first := range atoms {
				for _, second := range atoms {
					for _, end := range ends {
						for _, last := range lasts {
							body := strings.ReplaceAll("h|k"+end+first+"|"+second+end+second+"|z|"+first+last, "|", string(delimiter))
							compare(body, []byte(body), delimiter, trim)
						}
					}
				}
			}
		}
	}
	for name := range csvFixtures {
		r := &fetch.HTTPResponse{Body: readCSVFixture(t, name), Headers: make(http.Header)}
		charset := map[string]string{"cities-utf16be.csv": "utf-16be", "oblasti-windows-1251.csv": "windows-1251", "communes-iso-8859-1.csv": "iso-8859-1"}[name]
		if _, err := convertToUTF8(r, &model.Collector{Response: model.ResponseConfig{Charset: charset}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, delimiter := range []rune{',', ';', '\t', '|', ':', ' '} {
			for _, trim := range []bool{false, true} {
				compare(name, r.Body, delimiter, trim)
			}
		}
	}
	if accepted < 25000 || refused < 25000 || changed < 3000 {
		t.Errorf("%d bodies accepted and %d refused, %d of them handed to the reader otherwise than before: they do not cover all three", accepted, refused, changed)
	}
}

// A body with a quoted field left open is copied once for its carriage
// returns, as any body with one that ends a record is, whether the field is
// open on the first line or after lines that were copied for already; and a
// body that ends its last line with the only lone carriage return it has,
// after quoted fields that are closed, is still handed on as it is.
func TestCSVQuoteLeftOpenCostsOneCopy(t *testing.T) {
	const rows = 2000
	for name, tc := range map[string]struct {
		body   []byte
		copies float64
	}{
		"open on the first line":        {body: append([]byte("\"open,"), bytes.Repeat([]byte("web01,eu west,72\r"), rows)...), copies: 1},
		"open after lines ended by one": {body: append(bytes.Repeat([]byte("web01,\"eu\rwest\",72\r"), rows), "web02,\"eu,5\rweb03,eu,6\r"...), copies: 1},
		"closed, and one as the last":   {body: append(bytes.Repeat([]byte("web01,\"eu\rwest\",72\r\n"), rows), "web02,\"eu\",5\r"...)},
	} {
		for _, trim := range []bool{false, true} {
			var out []byte
			allocations := testing.AllocsPerRun(5, func() { out = carriageReturnsAsLineEnds(tc.body, ',', trim) })
			if allocations != tc.copies {
				t.Errorf("%s, trim_space %v: %v allocations, want %v", name, trim, allocations, tc.copies)
			}
			if copied := &out[0] != &tc.body[0]; copied != (tc.copies > 0) {
				t.Errorf("%s, trim_space %v: copied %v", name, trim, copied)
			}
		}
	}
}

// A quoted field left open on line 3 of a body of carriage return lines and
// on line 7 of another is one failure to the log, though each error names
// its lines, the record's and the one the body ends on; a value past the
// header's last column is another.
func TestCSVQuoteLeftOpenIsOneFailureWhereverItIs(t *testing.T) {
	failure := func(body string) error {
		t.Helper()
		_, err := decodedCSV(t, body, model.CSVConfig{})
		if err == nil {
			t.Fatalf("%q decoded", body)
		}
		return err
	}
	a, b := failure("h,k\ra,b\rc,\"d\re,f\rg,h\r"), failure("h,k\ra,b\ra,b\ra,b\ra,b\ra,b\rc,\"d\re,f\r")
	other := failure("h,k\ra,b\rc,d,e\r")
	if !strings.Contains(a.Error(), "record on line 3; parse error on line 5, column 5") || !strings.Contains(b.Error(), "record on line 7; parse error on line 8, column 5") {
		t.Errorf("the failures are\n%v\n%v", a, b)
	}
	if model.SameFailureText(a) != model.SameFailureText(b) || model.SameFailureText(a) != `CSV decode: parse error: extraneous or missing " in quoted-field` {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant one text without a line", a, b, model.SameFailureText(a), model.SameFailureText(b))
	}
	if model.SameFailureText(other) == model.SameFailureText(a) {
		t.Errorf("%v and %v are recognised as one, by %s", a, other, model.SameFailureText(a))
	}
}
