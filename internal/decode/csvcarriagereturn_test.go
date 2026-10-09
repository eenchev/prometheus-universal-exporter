package decode

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// readCSVBeforeCarriageReturns is readCSV as it was before a carriage return
// alone ended a record: a line ended at a line feed, and a carriage return
// without one was a character of its field.
func readCSVBeforeCarriageReturns(body []byte, delimiter rune, trimSpace bool) ([][]string, error) {
	if !trimSpace {
		return readCSVBody(body, delimiter, false)
	}
	readerSkips := delimiter == ' ' || !unicode.IsSpace(delimiter)
	read, _ := takeBlanksAroundQuotes(body, delimiter, !readerSkips, false)
	rows, err := readCSVBody(read, delimiter, readerSkips)
	if err == nil || len(read) == len(body) {
		return rows, err
	}
	_, removed := withoutBlanksAroundQuotes(body, delimiter, !readerSkips)
	return rows, originalColumn(err, read, removed)
}

// csvFieldLineBeforeCarriageReturns is csvFieldLine as it was then.
func csvFieldLineBeforeCarriageReturns(body []byte, delimiter rune, trimSpace bool, record, field int) int {
	readerSkips := trimSpace
	if trimSpace {
		readerSkips = delimiter == ' ' || !unicode.IsSpace(delimiter)
		body, _ = takeBlanksAroundQuotes(body, delimiter, !readerSkips, false)
	}
	cr := csv.NewReader(bytes.NewReader(body))
	cr.Comma = delimiter
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = readerSkips
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	for i := 0; ; i++ {
		fields, err := cr.Read()
		if err != nil {
			return 0
		}
		if i == record {
			if field >= len(fields) {
				return 0
			}
			line, _ := cr.FieldPos(field)
			return line
		}
	}
}

// decodedCSV decodes a body with the csv decoder under the settings given.
func decodedCSV(t *testing.T, body string, cfg model.CSVConfig) (any, error) {
	t.Helper()
	c := model.Collector{Name: "lines", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: cfg}}
	d, err := Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: make(http.Header)}, &c)
	if err != nil {
		return nil, err
	}
	return csvDocument(d.Data), nil
}

// A carriage return alone ends a record, as a line feed does and a carriage
// return with a line feed after it: a body written with one after each line,
// which was read as a header and no rows, has its rows, with a header row
// and without one, whatever the delimiter, and with a carriage return at its
// end or without. A body may end some lines with it and others with a line
// feed, an empty line is no row whichever way it ends, and of two carriage
// returns before a line feed the first ends the row and the second belongs
// to the line feed. Inside a quoted field a carriage return is the field's
// text, as it was, and so is one in a field read under trim_space that
// starts with a quote after blanks. Under trim_space a carriage return ends
// the record before the blanks a field starts with are skipped, so it is
// never one of them: the field before it is empty, and a quoted field after
// it starts its own row.
func TestCSVCarriageReturnAloneEndsARecord(t *testing.T) {
	web := map[string]any{"host": "web01", "used": "72"}
	db := map[string]any{"host": "db1", "used": "5"}
	for _, tc := range []struct {
		name, body string
		cfg        model.CSVConfig
		want       []any
	}{
		{name: "after every line", body: "host,used\rweb01,72\rdb1,5\r", want: []any{web, db}},
		{name: "and none after the last", body: "host,used\rweb01,72\rdb1,5", want: []any{web, db}},
		{name: "without a header row", body: "web01,72\rdb1,5\r", cfg: model.CSVConfig{Header: boolPtr(false)},
			want: []any{[]any{"web01", "72"}, []any{"db1", "5"}}},
		{name: "with tabs", body: "host\tused\rweb01\t72\rdb1\t5\r", cfg: model.CSVConfig{Delimiter: "\t"}, want: []any{web, db}},
		{name: "with semicolons", body: "host;used\rweb01;72\rdb1;5\r", cfg: model.CSVConfig{Delimiter: ";"}, want: []any{web, db}},
		{name: "with a delimiter beyond ASCII", body: "host used\rweb01 72\rdb1 5\r", cfg: model.CSVConfig{Delimiter: " "}, want: []any{web, db}},
		{name: "beside line feeds and CRLF", body: "host,used\rweb01,72\r\ndb1,5\nmail,9\r",
			want: []any{web, db, map[string]any{"host": "mail", "used": "9"}}},
		{name: "an empty line is no row", body: "host,used\r\rweb01,72\r\n\r\rdb1,5\r\r", want: []any{web, db}},
		{name: "of two before a line feed the first ends the row", body: "host,used\r\r\nweb01,72\r\r\ndb1,5\r\r\n", want: []any{web, db}},
		{name: "inside a quoted field it is text", body: "host,used\r\"web\r01\",72\r\"db\r\n1\",5\r",
			want: []any{map[string]any{"host": "web\r01", "used": "72"}, map[string]any{"host": "db\n1", "used": "5"}}},
		{name: "and beside a doubled quote", body: "host,used\r\"a \"\"b\"\"\rc\",72\r",
			want: []any{map[string]any{"host": "a \"b\"\rc", "used": "72"}}},
		{name: "a quote inside a field opens nothing", body: "host,used\r5\" disk,72\rdb1,5\r",
			want: []any{map[string]any{"host": `5" disk`, "used": "72"}, db}},
		{name: "under trim_space", body: "host , used\r web01 , 72 \rdb1,5\r", cfg: model.CSVConfig{TrimSpace: true}, want: []any{web, db}},
		{name: "under trim_space, around quoted fields", body: "host,used\r  \"web01\"  , \"72\" \r\"db1\"\t,5\r", cfg: model.CSVConfig{TrimSpace: true}, want: []any{web, db}},
		{name: "under trim_space, inside a quoted field after blanks", body: "host,used\r  \"web\r01\" ,72\r", cfg: model.CSVConfig{TrimSpace: true},
			want: []any{map[string]any{"host": "web\r01", "used": "72"}}},
		{name: "under trim_space it is no blank before a quoted field", body: "host,used\rweb01,\r\"db1\",5\r", cfg: model.CSVConfig{TrimSpace: true},
			want: []any{map[string]any{"host": "web01", "used": ""}, db}},
		{name: "under trim_space, with tabs", body: "host\tused\r web01\t\r  \"db\t1\" \t5\r", cfg: model.CSVConfig{Delimiter: "\t", TrimSpace: true},
			want: []any{map[string]any{"host": "web01", "used": ""}, map[string]any{"host": "db\t1", "used": "5"}}},
		{name: "under trim_space, with spaces", body: "host  used\rweb01   72\r\"db 1\"  5\r", cfg: model.CSVConfig{Delimiter: " ", TrimSpace: true},
			want: []any{web, map[string]any{"host": "db 1", "used": "5"}}},
		{name: "without trim_space the blanks are kept", body: "host,used\r web01 , 72\r", want: []any{map[string]any{"host": " web01 ", "used": " 72"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodedCSV(t, tc.body, tc.cfg)
			if err != nil || !reflect.DeepEqual(got, any(tc.want)) {
				t.Errorf("%q decoded into %v, %v; want %v", tc.body, got, err, tc.want)
			}
		})
	}
}

// The lines an error names are the lines such a body has: a line that ends
// with a carriage return alone is counted like one that ends with a line
// feed, in what the reader refuses and in the row with a value past the
// header's last column, under trim_space too. A carriage return inside a
// quoted field is no line of its own, where a line feed there is one.
func TestCSVErrorsCountTheLinesACarriageReturnEnds(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		cfg        model.CSVConfig
		want       string
	}{
		{name: "a stray quote", body: "host,used\rweb01,72\r\"db\"1,5\r",
			want: `CSV decode: parse error on line 3, column 4: extraneous or missing " in quoted-field`},
		{name: "a quoted field left open", body: "host,used\rweb01,72\rdb1,\"5\r",
			want: `CSV decode: parse error on line 3, column 8: extraneous or missing " in quoted-field`},
		{name: "a stray quote beside a bare one", body: "host,used\r5\" disk,72\r\"db\"1,5\r",
			want: `CSV decode: parse error on line 3, column 4: extraneous or missing " in quoted-field`},
		{name: "blanks after a closing quote, without trim_space", body: "host;used\r\"web01\" ;72\r", cfg: model.CSVConfig{Delimiter: ";"},
			want: `CSV decode: parse error on line 2, column 7: extraneous or missing " in quoted-field`},
		{name: "a stray quote after blanks taken out", body: "host\tused\r \"web01\" \t72\r  \"db\"1\t5\r", cfg: model.CSVConfig{Delimiter: "\t", TrimSpace: true},
			want: `CSV decode: parse error on line 3, column 6: extraneous or missing " in quoted-field`},
		{name: "a value past the header's last column", body: "host,used\rweb01,72\rdb, 1,5\rmail,9\r",
			want: "CSV line 3 has a value in column 3, which the header does not name"},
		{name: "past it under trim_space, after a quoted field", body: "host,used\r \"web01\" ,72\r\n\"db\" , 1 ,5\r", cfg: model.CSVConfig{TrimSpace: true},
			want: "CSV line 3 has a value in column 3, which the header does not name"},
		{name: "after a carriage return inside a quoted field", body: "host,used\r\"web\r01\",72\rdb,1,5\r",
			want: "CSV line 3 has a value in column 3, which the header does not name"},
		{name: "after a line feed inside a quoted field", body: "host,used\r\"web\n01\",72\rdb,1,5\r",
			want: "CSV line 4 has a value in column 3, which the header does not name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodedCSV(t, tc.body, tc.cfg); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("%q failed with %v, want %q", tc.body, err, tc.want)
			}
		})
	}
}

// csvLineEnd is a way a line of a generated body ends, and how the body
// that the former reader is to read as the same lines ends it: a carriage
// return alone as a line feed.
type csvLineEnd struct{ written, former string }

// csvField is a field of a generated body, and what the former reader is to
// read in its place without trim_space when that is something else: a field
// that starts with a quote only after a space is a quoted field under
// trim_space alone, so without it a carriage return inside those quotes ends
// the record, unless the space is the delimiter.
type csvField struct{ written, untrimmed string }

func (f csvField) former(trim bool, delimiter rune) string {
	if trim || f.untrimmed == "" || delimiter == ' ' {
		return f.written
	}
	return f.untrimmed
}

// Whatever the fields of a body hold, a body whose lines end with a carriage
// return alone is read as the reader read the same body with line feeds in
// their place: the same rows, or the same error in the same words at the
// same line and column, and the same line for every field. That is so of
// lines that end either way in one body, of an empty line, and of a last line
// that ends with a carriage return, with a line feed, or with nothing; under
// every delimiter, with trim_space and without; and with fields in quotes
// that hold carriage returns, line feeds, delimiters and doubled quotes,
// fields with blanks around their quotes, a quote inside a field and text
// after a closing quote. A body with no carriage return that ends a record
// is the same body in both, so it is read as it was, and it is not copied.
// Under the race detector every seventh of the 45,000 bodies is read, counted
// through them all, which is still every field beside every other under
// every delimiter, and every field with every end of a line.
func TestCSVReadsCarriageReturnLinesAsItReadLineFeedLines(t *testing.T) {
	atoms := []csvField{{written: `a`}, {written: ``}, {written: ` b c `}, {written: `"q"`}, {written: ` "q"`}, {written: `"q" `}, {written: "\"c\rr\""}, {written: "\"c\rr|\r\n\" "},
		{written: " \"c\rr\"", untrimmed: " \"c\nr\""}, {written: "\"two\nlines\""}, {written: `"x""y"`}, {written: `5" disk`}, {written: `"q" x`}, {written: `"st"ray"`}, {written: "\t\"t\""}}
	ends := []csvLineEnd{{"\n", "\n"}, {"\r\n", "\r\n"}, {"\r", "\n"}, {"\r\r", "\n\n"}, {"\r\r\n", "\n\r\n"}}
	lasts := []csvLineEnd{{"\n", "\n"}, {"\r\n", "\r\n"}, {"\r", "\n"}, {"", ""}}
	read, refused, ended, same := 0, 0, 0, 0
	bodies, every := 0, alloctest.UnlessRaced(1, 7)
	for _, delimiter := range []rune{',', ';', '\t', ' ', ' '} {
		for _, trim := range []bool{false, true} {
			for _, first := range atoms {
				for _, second := range atoms {
					for _, end := range ends {
						for _, last := range lasts {
							if bodies++; bodies%every != 0 {
								continue
							}
							lines := func(first, second, end, last string) []byte {
								return []byte(strings.ReplaceAll("h|k"+end+first+"|"+second+end+second+"|z|"+first+last, "|", string(delimiter)))
							}
							body, former := lines(first.written, second.written, end.written, last.written), lines(first.former(trim, delimiter), second.former(trim, delimiter), end.former, last.former)
							want, wantErr := readCSVBeforeCarriageReturns(bytes.Clone(former), delimiter, trim)
							given := bytes.Clone(body)
							got, err := readCSV(given, delimiter, trim)
							if !bytes.Equal(given, body) {
								t.Fatalf("delimiter %q, trim_space %v: reading %q changed it to %q", delimiter, trim, body, given)
							}
							switch {
							case wantErr != nil:
								refused++
								if err == nil || err.Error() != wantErr.Error() {
									t.Fatalf("delimiter %q, trim_space %v, %q: %q, %v; with line feeds it was refused: %v", delimiter, trim, body, got, err, wantErr)
								}
							case err != nil || !slices.EqualFunc(got, want, slices.Equal[[]string]):
								t.Fatalf("delimiter %q, trim_space %v, %q: read as %q, %v; with line feeds it was %q", delimiter, trim, body, got, err, want)
							default:
								read++
							}
							for record, fields := range want {
								// The first and the last record say it of
								// the lines between them too.
								if record > 0 && record < len(want)-1 {
									continue
								}
								for _, field := range []int{0, len(fields) - 1, len(fields)} {
									line := csvFieldLineBeforeCarriageReturns(former, delimiter, trim, record, field)
									if got := csvFieldLine(body, delimiter, trim, record, field); got != line {
										t.Fatalf("delimiter %q, trim_space %v, %q: field %d of record %d is on line %d, and with line feeds it was on line %d", delimiter, trim, body, field, record, got, line)
									}
								}
							}
							// A body the two are one and the same for is
							// handed to the reader as it is.
							lineEnds := carriageReturnsAsLineEnds(body, delimiter, trim)
							if bytes.Equal(body, former) {
								same++
								if &lineEnds[0] != &body[0] {
									t.Fatalf("delimiter %q, trim_space %v: %q has no carriage return that ends a record, and was copied", delimiter, trim, body)
								}
							} else {
								ended++
							}
						}
					}
				}
			}
		}
	}
	if read < 25000/every || refused < 10000/every || ended < 25000/every || same < 10000/every {
		t.Errorf("%d bodies were read and %d refused, %d of them with a carriage return ending a record and %d without: they do not cover all four", read, refused, ended, same)
	}
}

// Every fixture of testdata/csv is read as it was, under every delimiter the
// fixtures use, with trim_space and without: none of them ends a record with
// a carriage return alone but the two that are written to. Those two, with
// their carriage returns written as line feeds, are read as the former
// reader read that.
func TestCSVFixturesAreReadAsBeforeCarriageReturnsEndedRecords(t *testing.T) {
	// The two, with the delimiter each is written with: read with another,
	// their quoted fields are no quoted fields, and the carriage return
	// inside one of them ends a record like the others.
	carriageReturns := map[string]rune{"volumes-cr.csv": ',', "scale-mixed-line-ends.txt": ';'}
	compared := 0
	for name := range csvFixtures {
		r := &fetch.HTTPResponse{Body: readCSVFixture(t, name), Headers: make(http.Header)}
		charset := map[string]string{"cities-utf16be.csv": "utf-16be", "oblasti-windows-1251.csv": "windows-1251", "communes-iso-8859-1.csv": "iso-8859-1"}[name]
		if _, err := convertToUTF8(r, &model.Collector{Response: model.ResponseConfig{Charset: charset}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		written, ends := carriageReturns[name]
		for _, delimiter := range []rune{',', ';', '\t', '|', ':', ' '} {
			for _, trim := range []bool{false, true} {
				if copied := &carriageReturnsAsLineEnds(r.Body, delimiter, trim)[0] != &r.Body[0]; copied != ends {
					t.Errorf("%s with delimiter %q and trim_space %v: copied for its carriage returns: %v", name, delimiter, trim, copied)
				}
				former := r.Body
				if ends {
					if delimiter != written {
						continue
					}
					// Every carriage return alone as a line feed, but for
					// the one inside a quoted field.
					former = bytes.ReplaceAll(bytes.ReplaceAll(bytes.ReplaceAll(r.Body, []byte("\r\n"), []byte("\x00")), []byte("\r"), []byte("\n")), []byte("\x00"), []byte("\r\n"))
					former = bytes.ReplaceAll(former, []byte("full soon\nsee"), []byte("full soon\rsee"))
				}
				want, wantErr := readCSVBeforeCarriageReturns(bytes.Clone(former), delimiter, trim)
				got, err := readCSV(bytes.Clone(r.Body), delimiter, trim)
				if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() || !slices.EqualFunc(got, want, slices.Equal[[]string]) {
					t.Fatalf("%s with delimiter %q and trim_space %v: read as %q, %v; it was %q, %v", name, delimiter, trim, got, err, want, wantErr)
				}
				compared++
			}
		}
	}
	if compared < 300 {
		t.Errorf("%d readings compared", compared)
	}
}

// Looking for a carriage return that ends a record costs a body without one
// nothing but the look: no allocation, for a body with line feeds, with
// CRLF, with a carriage return as its last byte, and with carriage returns
// only inside quoted fields and at its end, each handed back as it is. A body with one is
// copied once, whether it has quotes to follow or not, and reading a body
// of CRLF lines allocates what it allocated.
func TestCSVCarriageReturnsCostABodyWithoutThemNothing(t *testing.T) {
	const rows = 2000
	bodies := map[string]struct {
		body   []byte
		copies float64
	}{
		"line feeds":                         {body: bytes.Repeat([]byte("web01,eu west,72\n"), rows)},
		"CRLF":                               {body: bytes.Repeat([]byte("web01,\"eu west\",72\r\n"), rows)},
		"a carriage return as the last byte": {body: append(bytes.Repeat([]byte("web01,eu west,72\n"), rows), "web02,eu,5\r"...)},
		"carriage returns in quoted fields":  {body: bytes.Repeat([]byte("web01,\"eu\rwest\",72\r\n"), rows)},
		"and one as the last byte besides":   {body: append(bytes.Repeat([]byte("web01,\"eu\rwest\",72\r\n"), rows), "web02,eu,5\r"...)},
		"carriage returns":                   {body: bytes.Repeat([]byte("web01,eu west,72\r"), rows), copies: 1},
		"carriage returns and quoted fields": {body: bytes.Repeat([]byte("web01,\"eu\rwest\",72\r"), rows), copies: 1},
	}
	for name, tc := range bodies {
		for _, trim := range []bool{false, true} {
			var out []byte
			allocations := alloctest.AllocsAtMost(5, tc.copies, func() { out = carriageReturnsAsLineEnds(tc.body, ',', trim) })
			if allocations != tc.copies {
				t.Errorf("%s, trim_space %v: %v allocations, want %v", name, trim, allocations, tc.copies)
			}
			if copied := &out[0] != &tc.body[0]; copied != (tc.copies > 0) {
				t.Errorf("%s, trim_space %v: copied %v", name, trim, copied)
			}
		}
	}
	crlf := bodies["CRLF"].body
	for _, trim := range []bool{false, true} {
		was, _ := alloctest.Allocations(3, func() { _, _ = readCSVBeforeCarriageReturns(crlf, ',', trim) })
		now := alloctest.AllocsAtMost(3, was, func() { _, _ = readCSV(crlf, ',', trim) })
		if now != was {
			t.Errorf("trim_space %v: reading a body of CRLF lines takes %v allocations, and took %v", trim, now, was)
		}
	}
}
