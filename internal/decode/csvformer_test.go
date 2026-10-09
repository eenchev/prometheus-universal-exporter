package decode

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The CSV decoder as it was before it refused a row with a value past the
// header's last column and trimmed the blanks after a quoted field, kept
// here to hold the decoder against: what it reads of every body that has
// neither is what this reads of it.

// formerReadCSV is readCSV as it was: only a tab, or another blank that is
// no space, had the blanks before a quote taken out, and nothing was taken
// out after one.
func formerReadCSV(body []byte, delimiter rune, trimSpace bool) ([][]string, error) {
	if !trimSpace || delimiter == ' ' || !unicode.IsSpace(delimiter) {
		return readCSVBody(body, delimiter, trimSpace)
	}
	body, removed := formerWithoutBlanksBeforeQuotes(body, delimiter)
	rows, err := readCSVBody(body, delimiter, false)
	return rows, originalColumn(err, body, removed)
}

func formerWithoutBlanksBeforeQuotes(body []byte, delimiter rune) ([]byte, []csvRemoved) {
	var (
		out     []byte
		removed []csvRemoved
		kept    int
	)
	for i := 0; i < len(body); {
		start := i
		for i < len(body) {
			r, width := utf8.DecodeRune(body[i:])
			if r == delimiter || r == '\n' || !unicode.IsSpace(r) {
				break
			}
			i += width
		}
		quoted := i < len(body) && body[i] == '"'
		if quoted && i > start {
			if out == nil {
				out = make([]byte, 0, len(body))
			}
			out = append(out, body[kept:start]...)
			kept = i
			removed = append(removed, csvRemoved{at: len(out), n: i - start})
		}
		if quoted {
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
			r, width := utf8.DecodeRune(body[i:])
			i += width
			if r == delimiter || r == '\n' {
				break
			}
		}
	}
	if out == nil {
		return body, nil
	}
	return append(out, body[kept:]...), removed
}

// formerDecodeCSV is decodeCSV as it was, of a body in UTF-8: the rows, and
// what the reader made of the body.
func formerDecodeCSV(body []byte, cfg model.CSVConfig) (data []any, read [][]string, err error) {
	delim := ','
	if cfg.Delimiter != "" {
		delim = []rune(cfg.Delimiter)[0]
	}
	rows, err := formerReadCSV(body, delim, cfg.TrimSpace)
	if err != nil {
		return nil, nil, fmt.Errorf("CSV decode: %w", err)
	}
	if len(rows) == 0 {
		return []any{}, rows, nil
	}
	out := []any{}
	if cfg.Header == nil || *cfg.Header {
		heads := slices.Clone(rows[0])
		column := map[string]int{}
		for i := range heads {
			if cfg.TrimSpace {
				heads[i] = strings.TrimSpace(heads[i])
			}
			if heads[i] == "" {
				if columnIsEmpty(rows[1:], i, cfg.TrimSpace) {
					continue
				}
				return nil, nil, fmt.Errorf("CSV header leaves column %d unnamed, and it holds values; name it, or set response.csv.header: false and read the columns by number", i+1)
			}
			if first, seen := column[heads[i]]; seen {
				return nil, nil, fmt.Errorf("CSV header names column %q twice, as columns %d and %d; rename one, or set response.csv.header: false and read the columns by number", heads[i], first+1, i+1)
			}
			column[heads[i]] = i
		}
		for _, row := range rows[1:] {
			m := map[string]any{}
			for i, k := range heads {
				if k == "" {
					continue
				}
				if i < len(row) {
					v := row[i]
					if cfg.TrimSpace {
						v = strings.TrimSpace(v)
					}
					m[k] = v
				} else {
					m[k] = ""
				}
			}
			out = append(out, m)
		}
		return out, rows, nil
	}
	for _, row := range rows {
		a := make([]any, len(row))
		for i, v := range row {
			if cfg.TrimSpace {
				v = strings.TrimSpace(v)
			}
			a[i] = v
		}
		out = append(out, a)
	}
	return out, rows, nil
}

// blankAfterQuote finds a quote with white space after it other than the
// end of its line, which every body with blanks after a quoted field has.
var blankAfterQuote = regexp.MustCompile(`"[\t\v\f \x{00a0}\x{2003}]|"\r+[^\n]`)

// csvDifference is what decoding a body gives now and gave before, held
// against each other.
type csvDifference int

const (
	// csvSame: the same rows, or the same error in the same words.
	csvSame csvDifference = iota
	// csvLongRow: the body decoded, with a header row, though a row of it
	// has a value past the header's last column; it is refused now, in the
	// words of that refusal.
	csvLongRow
	// csvClosingQuote: read under trim_space, the body has white space
	// after a quote, the end of a line apart; csvquotes_test.go holds what
	// is made of those.
	csvClosingQuote
	// csvCarriageReturn: a carriage return alone ends a record of the body,
	// which was read as one line with what follows;
	// csvcarriagereturn_test.go holds what is made of those.
	csvCarriageReturn
)

// compareWithFormerCSV decodes a body in UTF-8 as it is decoded now and as
// it was, and fails the test unless the two agree or differ in one of the
// three ways they are meant to.
func compareWithFormerCSV(t *testing.T, body []byte, cfg model.CSVConfig) csvDifference {
	t.Helper()
	c := model.Collector{Name: "former", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: cfg}}
	var got any
	d, err := decodeCSV(&fetch.HTTPResponse{Body: bytes.Clone(body), Headers: make(http.Header)}, &c)
	if err == nil {
		got = csvDocument(d.Data)
	}
	want, read, wantErr := formerDecodeCSV(bytes.Clone(body), cfg)
	if cfg.TrimSpace && blankAfterQuote.Match(body) {
		return csvClosingQuote
	}
	delimiter := ','
	if cfg.Delimiter != "" {
		delimiter = []rune(cfg.Delimiter)[0]
	}
	if !bytes.Equal(carriageReturnsAsLineEnds(body, delimiter, cfg.TrimSpace), body) {
		return csvCarriageReturn
	}
	header := cfg.Header == nil || *cfg.Header
	long := 0
	if wantErr == nil && header && len(read) > 0 {
		for i, row := range read[1:] {
			if len(row) > len(read[0]) && firstValue(row[len(read[0]):], cfg.TrimSpace) < len(row)-len(read[0]) && long == 0 {
				long = i + 1
			}
		}
	}
	if long > 0 {
		if err == nil || !strings.Contains(err.Error(), "which the header does not name") {
			t.Fatalf("%q with %+v: row %d has a value past the header's last column, and the decode gave %v, %v", body, cfg, long, got, err)
		}
		return csvLongRow
	}
	if fmt.Sprint(err) != fmt.Sprint(wantErr) || err == nil && !reflect.DeepEqual(got, any(want)) {
		t.Fatalf("%q with %+v:\ngot  %v, %v\nwant %v, %v", body, cfg, got, err, want, wantErr)
	}
	return csvSame
}

// Every fixture of testdata/csv, read with every delimiter the fixtures
// use, with and without trim_space and a header row, and some six thousand
// small bodies of fields of every kind, rows shorter and longer than their
// header among them, decode as they did before rows longer than the header
// were refused and the blanks after a closing quote trimmed: the same rows,
// or the same error. The only bodies that differ are those with a value past
// the header's last column, read with a header row, which are refused, those
// with white space after a quote, read under trim_space, and the two
// fixtures in which a carriage return alone ends a record. Under the race
// detector every seventh of the small bodies is decoded, counted through
// the settings, which is still every field beside every other and after
// every header, with a seventh of each kind to find among them.
func TestCSVDecodesAsBeforeButForLongRowsAndBlanksAfterQuotes(t *testing.T) {
	no := false
	var settings []model.CSVConfig
	for _, delimiter := range []string{"", ";", "\t", "|", ":", " "} {
		for _, trim := range []bool{false, true} {
			settings = append(settings, model.CSVConfig{Delimiter: delimiter, TrimSpace: trim}, model.CSVConfig{Delimiter: delimiter, TrimSpace: trim, Header: &no})
		}
	}
	counts := map[csvDifference]int{}
	for _, name := range slices.Sorted(maps.Keys(csvFixtures)) {
		// The fixtures in other encodings are compared as the text they
		// are converted to.
		r := &fetch.HTTPResponse{Body: readCSVFixture(t, name), Headers: make(http.Header)}
		charset := map[string]string{"cities-utf16be.csv": "utf-16be", "oblasti-windows-1251.csv": "windows-1251", "communes-iso-8859-1.csv": "iso-8859-1"}[name]
		if _, err := convertToUTF8(r, &model.Collector{Response: model.ResponseConfig{Charset: charset}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, cfg := range settings {
			counts[compareWithFormerCSV(t, r.Body, cfg)]++
		}
	}
	if counts[csvSame] < 400 || counts[csvLongRow] < 10 || counts[csvCarriageReturn] < 20 || counts[csvCarriageReturn] > 48 {
		t.Errorf("of the fixtures' readings %d are as before, %d have a long row and in %d a carriage return ends a record: the fixtures and settings do not cover the three", counts[csvSame], counts[csvLongRow], counts[csvCarriageReturn])
	}

	fields := []string{`a`, ``, ` `, `"q"`, `"q" `, ` "a|b"`, `x"y`, "\"two\nlines\""}
	headers := []string{"h1|h2", "h1|h2|", "h1||h3", ` h1 |"h2" `}
	thirds := []string{"", "|", "| ", "|x", `|"q" `, "||y"}
	lasts := []string{"1|2", "1|2|", "1|2|| 4 ", "1"}
	generated := map[csvDifference]int{}
	bodies, every := 0, alloctest.UnlessRaced(1, 7)
	for _, cfg := range settings {
		// A comma and a tab, read every way, and spaces as columns
		// aligned with them are read.
		if aligned := cfg.Delimiter == " " && cfg.TrimSpace && cfg.Header == nil; cfg.Delimiter != "" && cfg.Delimiter != "\t" && !aligned {
			continue
		}
		delimiter := cfg.Delimiter
		if delimiter == "" {
			delimiter = ","
		}
		for _, header := range headers {
			for _, first := range fields {
				for _, second := range fields {
					for _, third := range thirds {
						for _, last := range lasts {
							if bodies++; bodies%every != 0 {
								continue
							}
							written := header + "\n" + first + "|" + second + third + "\n" + last + "\n"
							generated[compareWithFormerCSV(t, []byte(strings.ReplaceAll(written, "|", delimiter)), cfg)]++
						}
					}
				}
			}
		}
	}
	if generated[csvSame] < 20000/every || generated[csvLongRow] < 2000/every || generated[csvClosingQuote] < 5000/every {
		t.Errorf("of the generated bodies' readings %d are as before, %d have a long row and %d blanks after a quote: they do not cover all three", generated[csvSame], generated[csvLongRow], generated[csvClosingQuote])
	}
}

// What reading a body gives, the blanks after closing quotes apart, is what
// it gave: whatever a body of quoted and unquoted fields holds, under every
// delimiter and with trim_space on and off, a body the former reader read is
// read into the same fields, and one it refused is refused in the same
// words, at the same line and column, unless it is read under trim_space and
// has white space after a quote. A body in which a carriage return alone ends
// a record is left out: it was read as one line with what follows, and
// csvcarriagereturn_test.go holds what is made of it now. Under the race
// detector every seventh of the 87,000 bodies is read, counted through them
// all, which is still every field beside every other under every delimiter.
func TestCSVIsReadAsBeforeButForBlanksAfterQuotes(t *testing.T) {
	atoms := []string{`a`, `b c`, ` lead`, `trail `, ``, `"q"`, ` "q"`, `"q" `, "\"q\"\u00a0 ", "\"q\"\t", ` "a|b"  `, `"x""y"`, "\"two\nlines\"", "\"two\nlines\" ", `"open`, `"st"ray"`, `"st"ray" `, `5" disk`, `"q" x`, `"q" "r"`, `x "mid" `, "\"cr\"\r"}
	thirds := []string{`a`, ` "q" `, `"open`, `5" disk`, `"st"ray" `, "\"two\nlines\""}
	read, refused, differ, ended := 0, 0, 0, 0
	bodies, every := 0, alloctest.UnlessRaced(1, 7)
	for _, delimiter := range []rune{',', ';', '\t', ' ', '\u00a0'} {
		for _, trim := range []bool{false, true} {
			for _, first := range atoms {
				for _, second := range atoms {
					for _, third := range thirds {
						for _, ending := range []string{"\n", "\r\n", ""} {
							if bodies++; bodies%every != 0 {
								continue
							}
							body := []byte(strings.ReplaceAll(first+"|"+second+"\n"+third+"|"+second+ending, "|", string(delimiter)))
							if !bytes.Equal(carriageReturnsAsLineEnds(body, delimiter, trim), body) {
								ended++
								continue
							}
							want, wantErr := formerReadCSV(bytes.Clone(body), delimiter, trim)
							got, err := readCSV(bytes.Clone(body), delimiter, trim)
							switch {
							case wantErr == nil:
								read++
								if err != nil || !slices.EqualFunc(got, want, slices.Equal[[]string]) {
									t.Fatalf("delimiter %q, trim_space %v, %q: read as %q, %v; was %q", delimiter, trim, body, got, err, want)
								}
							case err != nil && err.Error() == wantErr.Error():
								refused++
							case trim && blankAfterQuote.Match(body):
								differ++
							default:
								t.Fatalf("delimiter %q, trim_space %v, %q: %q, %v; was refused: %v", delimiter, trim, body, got, err, wantErr)
							}
						}
					}
				}
			}
		}
	}
	if read < 5000/every || refused < 20000/every || differ < 2000/every || ended < 4000/every || ended > 6000/every {
		t.Errorf("%d bodies were read as before, %d refused as before, %d differ and in %d a carriage return ends a record: they do not cover all four", read, refused, differ, ended)
	}
}
