package decode

import (
	"bytes"
	"encoding/csv"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// bareQuoteRow is a row that is read leniently: its last field has a quote
// and does not start with one.
const bareQuoteRow = "d0,4,a 5\" disk\n"

// A quoted field left open, or with a stray quote in it, fails the decode
// when another row has a quote in an unquoted field too. Read leniently for
// that row's sake, the broken field took the rows after it as its own text,
// and the scrape passed without them. The error is the one the same body
// gives without the lenient row, a line further down.
func TestCSVBrokenQuotedFieldsFailBesideABareQuote(t *testing.T) {
	for name, tc := range map[string]struct{ rows, want string }{
		"left open":                {"d1,5,ok\n\"d2,6,ok\nd3,7,ok\nd4,8,ok\n", `record on line 4; parse error on line 6, column 9: extraneous or missing " in quoted-field`},
		"left open, no final line": {"d1,5,ok\nd2,6,\"ok", `parse error on line 4, column 9: extraneous or missing " in quoted-field`},
		"stray quote":              {"d1,5,ok\n\"d2\"x,6,ok\nd3,7,ok\n", `parse error on line 4, column 4: extraneous or missing " in quoted-field`},
		"stray quote, later line":  {"d1,5,ok\n\"d2\r\nd\"x\",6,ok\nd3,7,\"ok\"\n", `record on line 4; parse error on line 5, column 2: extraneous or missing " in quoted-field`},
		"in the lenient row":       {"d1,5\" disk,\"o\"k\"\nd3,7,ok\n", `parse error on line 3, column 14: extraneous or missing " in quoted-field`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeAs(t, "csv", "", "name,size,note\n"+bareQuoteRow+tc.rows)
			if err == nil || err.Error() != "CSV decode: "+tc.want {
				t.Fatalf("beside a bare quote: %v\nwant %s", err, tc.want)
			}
			// Without the lenient row the strict reader refuses the same
			// field in the same words, one line up.
			_, strict := decodeAs(t, "csv", "", "name,size,note\n\n"+tc.rows)
			if name != "in the lenient row" && (strict == nil || strict.Error() != err.Error()) {
				t.Fatalf("the strict reader says %v, the lenient reading %v", strict, err)
			}
		})
	}
}

// Quoted fields written as the format requires are read beside a bare quote
// as they are without one: escaped quotes, delimiters and line breaks in
// them, CRLF line ends, leading blanks before the quote under trim_space, and
// a last line without a line end. No row is lost and none is joined to
// another.
func TestCSVQuotedFieldsAreReadBesideABareQuote(t *testing.T) {
	rows := []string{
		`plain,1,ok`,
		`"quoted",2,"say ""hi"""`,
		`"a,b",3,"two` + "\r\n" + `lines"`,
		`"",4,""""`,
		`x"y,5,"z"`,
		`last,6,"end"`,
	}
	want := [][]string{
		{"plain", "1", "ok"},
		{"quoted", "2", `say "hi"`},
		{"a,b", "3", "two\nlines"},
		{"", "4", `"`},
		{`x"y`, "5", "z"},
		{"last", "6", "end"},
	}
	for name, ending := range map[string]string{"LF": "\n", "CRLF": "\r\n"} {
		for _, final := range []string{ending, ""} {
			body := strings.Join(rows, ending) + final
			got, err := readCSV([]byte(body), ',', false)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(got) != len(want) {
				t.Fatalf("%s: %d rows, want %d: %q", name, len(got), len(want), got)
			}
			for i := range want {
				if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
					t.Errorf("%s row %d: %q, want %q", name, i+1, got[i], want[i])
				}
			}
		}
	}
	// A blank before a quote is the field's own without trim_space, where
	// the field is then unquoted and read as written, and no part of it
	// with trim_space, where it is a quoted field.
	got, err := readCSV([]byte("a\"b, \"c,d\"\n"), ',', true)
	if err != nil || len(got) != 1 || strings.Join(got[0], "|") != `a"b|c,d` {
		t.Fatalf("with trim_space: %q, %v", got, err)
	}
	got, err = readCSV([]byte("a\"b, \"c,d\"\n"), ',', false)
	if err != nil || len(got) != 1 || strings.Join(got[0], "|") != `a"b| "c|d"` {
		t.Fatalf("without trim_space: %q, %v", got, err)
	}
}

// Whatever a body holds, reading it beside a bare quote gives the rows the
// strict reader gives for it alone, or the strict reader's refusal: the
// lenient reading never accepts a quoted field the strict one refuses.
func TestCSVLenientReadingAgreesWithTheStrictReader(t *testing.T) {
	fields := []string{`a`, ``, `"q"`, `"a,b"`, `"x""y"`, `"open`, `"st"ray"`, `"two` + "\n" + `lines"`, ` "sp"`, `tail"`}
	strict := func(body string) ([][]string, error) {
		cr := csv.NewReader(strings.NewReader(body))
		cr.FieldsPerRecord = -1
		return cr.ReadAll()
	}
	checked := 0
	for _, first := range fields {
		for _, second := range fields {
			for _, third := range fields {
				rows := first + "," + second + "\n" + third + ",z\n"
				want, wantErr := strict(rows)
				if wantErr != nil && strings.Contains(wantErr.Error(), csv.ErrBareQuote.Error()) {
					// The rows have a bare quote of their own, which the
					// strict reader stops at.
					continue
				}
				checked++
				got, err := readCSV([]byte(bareQuoteRow+rows), ',', false)
				if wantErr != nil {
					if err == nil {
						t.Fatalf("%q: read as %q, but the strict reader refuses it: %v", rows, got, wantErr)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%q: %v, but the strict reader reads it", rows, err)
				}
				if !slices.EqualFunc(got[1:], want, slices.Equal[[]string]) {
					t.Fatalf("%q: read as %q, the strict reader reads %q", rows, got[1:], want)
				}
			}
		}
	}
	if checked < 300 {
		t.Fatalf("only %d bodies were compared", checked)
	}
}

// trim_space trims both sides of every field without a header row too, where
// only the blanks leading a field were trimmed.
func TestCSVTrimSpaceWithoutAHeader(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
		Response: model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(false), TrimSpace: true}}}
	r := &fetch.HTTPResponse{Body: []byte(" web01 , 72 \n\"web02  \",31\t\n"), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, d.Data); got != `[["web01","72"],["web02","31"]]` {
		t.Fatalf("trimmed rows %s", got)
	}
	c.Response.CSV.TrimSpace = false
	if d, err = Decode(r, &c); err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, d.Data); got != `[[" web01 "," 72 "],["web02  ","31\t"]]` {
		t.Fatalf("untrimmed rows %s", got)
	}
}

// With a tab for a delimiter, or another blank that is no space, trim_space
// leaves an empty field where it stands: the tab after it is the next
// delimiter, not a blank leading the field after, which the reader took it
// for, moving the rest of the row a column to the left without a word. The
// fields are still trimmed on both sides.
func TestCSVTrimSpaceKeepsTheColumnsOfTabSeparatedValues(t *testing.T) {
	for _, delimiter := range []string{"\t", "\u00a0", "\v"} {
		body := strings.ReplaceAll("host|note|cpu\nweb01| x |72\nweb02||31\n|y|\n", "|", delimiter)
		for _, trim := range []bool{false, true} {
			c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
				Response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: delimiter, TrimSpace: trim}}}
			d, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: make(http.Header)}, &c)
			if err != nil {
				t.Fatalf("delimiter %q, trim_space %v: %v", delimiter, trim, err)
			}
			want := `[{"cpu":"72","host":"web01","note":" x "},{"cpu":"31","host":"web02","note":""},{"cpu":"","host":"","note":"y"}]`
			if trim {
				want = strings.Replace(want, `" x "`, `"x"`, 1)
			}
			if got := asJSON(t, d.Data); got != want {
				t.Errorf("delimiter %q, trim_space %v: rows %s, want %s", delimiter, trim, got, want)
			}
		}
	}
	// A comma is no blank: blanks around a field, and before a quoted one,
	// are trimmed as before.
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
		Response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte("host, note ,cpu\n web01 ,  \"x, y\", 72\nweb02, ,31\n"), Headers: make(http.Header)}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := asJSON(t, d.Data), `[{"cpu":"72","host":"web01","note":"x, y"},{"cpu":"31","host":"web02","note":""}]`; got != want {
		t.Errorf("comma-separated rows %s, want %s", got, want)
	}
}

// csvRows decodes a body with a header row under a delimiter and trim_space.
func csvRows(t *testing.T, delimiter string, trim bool, body string) (string, error) {
	t.Helper()
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
		Response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: delimiter, TrimSpace: trim}}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: make(http.Header)}, &c)
	if err != nil {
		return "", err
	}
	return asJSON(t, d.Data), nil
}

// Keeping the columns of tab-separated values cost them their quoted fields
// under trim_space: with the reader no longer skipping the blanks a field
// starts with, a quoted field written after spaces kept its quotes and was
// cut at a tab inside it, which made a column of the rest. The spaces
// between the start of a field and its quote are taken out before the body
// is read, so the field is a quoted one again, as it was read before the
// columns were kept, and an empty field still keeps its column. Blanks
// inside a quoted field, and a quote that does not start its field, are left
// as written, and without trim_space a blank before a quote is the field's
// own, as before.
func TestCSVTrimSpaceReadsAQuotedFieldAfterBlanksBetweenTabs(t *testing.T) {
	for _, delimiter := range []string{"\t", "\u00a0"} {
		body := strings.ReplaceAll("name|v|note\na|  \"q|1\"|x\nb||y\n c |3| \"z|z\"\n \t\"d\"|4|w\ne| \" in| \"\"side \"|5\" disk\n", "|", delimiter)
		if delimiter == "\t" {
			// A tab is the delimiter there, and no blank before a quote.
			body = strings.Replace(body, " \t\"d\"", "  \"d\"", 1)
		}
		want := strings.ReplaceAll(`[{"name":"a","note":"x","v":"q|1"},{"name":"b","note":"y","v":""},{"name":"c","note":"z|z","v":"3"},{"name":"d","note":"w","v":"4"},{"name":"e","note":"5\" disk","v":"in| \"side"}]`, "|", strings.Trim(asJSON(t, delimiter), `"`))
		got, err := csvRows(t, delimiter, true, body)
		if err != nil || got != want {
			t.Errorf("delimiter %q: %v, rows %s, want %s", delimiter, err, got, want)
		}
	}
	if got, err := csvRows(t, "\t", false, "name\tv\na\t \"q\"\n"); err != nil || got != `[{"name":"a","v":" \"q\""}]` {
		t.Errorf("without trim_space: %v, rows %s", err, got)
	}
}

// With a space for a delimiter trim_space reads as it always did: the
// reader skips the spaces a field starts with, so a run of spaces is one
// delimiter and columns aligned with spaces are read, a quoted field among
// them with its spaces kept. Keeping the columns of tab-separated values had
// turned that off for a space too, and every space of a run became a column
// of its own. An empty field cannot be written between spaces: the row is
// read a column short.
func TestCSVTrimSpaceReadsColumnsAlignedWithSpaces(t *testing.T) {
	got, err := csvRows(t, " ", true, "host   cpu  note\nweb01   72  \"two  words\"\n  web02   3  x\nweb03  y\n")
	if want := `[{"cpu":"72","host":"web01","note":"two  words"},{"cpu":"3","host":"web02","note":"x"},{"cpu":"y","host":"web03","note":""}]`; err != nil || got != want {
		t.Errorf("%v, rows %s, want %s", err, got, want)
	}
	// Without trim_space every space is a delimiter, as before.
	got, err = csvRows(t, " ", false, "host note cpu\nweb02  31\n")
	if want := `[{"cpu":"31","host":"web02","note":""}]`; err != nil || got != want {
		t.Errorf("without trim_space: %v, rows %s, want %s", err, got, want)
	}
}

// An error of a tab-separated body read under trim_space names the line and
// the column the body has it in, although the blanks before a quote were
// taken out before it was read: the same line and column the reader gave
// when it skipped those blanks itself. That holds for a quoted field left
// open or with a stray quote, on the line of the blanks and on a later one,
// after one run of blanks on the line and after two, and beside a row with
// a bare quote, which is read leniently.
func TestCSVErrorsOfTabSeparatedValuesNameTheBodysColumns(t *testing.T) {
	for body, want := range map[string]string{
		"a\t  \"st\"ray\"\tz\n":                  `parse error on line 1, column 8: extraneous or missing " in quoted-field`,
		"  \"a\"\t \"b\"\t   \"st\"ray\"\n":      `parse error on line 1, column 18: extraneous or missing " in quoted-field`,
		"a\t \"b\"\nc\t  \"open\n":               `parse error on line 2, column 11: extraneous or missing " in quoted-field`,
		" \"two\n lines\"\t  \"st\"ray\"\n":      `record on line 1; parse error on line 2, column 14: extraneous or missing " in quoted-field`,
		"5\" disk\tx\n  \"a\"\t \"st\"ray\"\n":   `parse error on line 2, column 11: extraneous or missing " in quoted-field`,
		"5\" disk\tx\n \"a\"\t  \"open\ny\tz\n":  `record on line 2; parse error on line 3, column 5: extraneous or missing " in quoted-field`,
		"a\t  \"q\" x\tz\n":                      `parse error on line 1, column 7: extraneous or missing " in quoted-field`,
		"a\tb\n\"q\"\tx\n \"r\"\t \"s\" \"t\"\n": `parse error on line 3, column 9: extraneous or missing " in quoted-field`,
	} {
		_, err := readCSV([]byte(body), '\t', true)
		_, was := readCSVBody([]byte(body), '\t', true)
		if err == nil || err.Error() != want || was == nil || was.Error() != want {
			t.Errorf("%q: err=%v, with the reader skipping the blanks %v, want %s", body, err, was, want)
		}
	}
}

// Whatever a tab-separated body holds, trim_space reads it as the reader
// read it when it skipped the blanks fields start with (readCSVBody with
// the reader's own trimming, which is how readCSV read every body before the
// columns were kept), fields trimmed, or refuses it with the same error at
// the same line and column — but for the two cases they are meant to differ
// in. A body with an empty field is left out here: one where a field starts,
// after blanks or without, with the delimiter. And a quoted field with
// blanks after its closing quote, which the reader refuses, is read as the
// reader reads the body without those blanks, an error's column counted in
// the body as it is written. Under the race detector every seventh of the
// 64,000 bodies is read, counted through them all, which is still every
// field beside every other and before every third.
func TestCSVTrimSpaceReadsTabSeparatedValuesAsTheReaderDidButForEmptyFields(t *testing.T) {
	atoms := []string{`a`, `b c`, ` lead`, `trail `, `"q"`, ` "q"`, `  "a|b"`, `"x""y"`, " \u00a0\"x \"\" y\"", "\"two\nlines\"", " \"two\n \"\"l|nes\"", `"open`, ` "open`, `"st"ray"`, ` "st"ray"`, `5" disk`, ` 5" disk`, `"sp" `, `x "mid"`, ` "a| |""b"`, "\f\"ff\"", ` " "`}
	// without is an atom as the reader is given it: `"sp" ` is the one atom
	// with blanks after a closing quote, and a delimiter or a line end
	// follows every atom.
	without := func(atom string) string {
		if atom == `"sp" ` {
			return `"sp"`
		}
		return atom
	}
	compared, refused, closed := 0, 0, 0
	bodies, every := 0, alloctest.UnlessRaced(1, 7)
	for _, delimiter := range []rune{'\t', '\u2003'} {
		for _, first := range atoms {
			for _, second := range atoms {
				for _, third := range atoms {
					for _, ending := range []string{"\n", "\r\n", ""} {
						if bodies++; bodies%every != 0 {
							continue
						}
						body := []byte(strings.ReplaceAll(first+"|"+second+"\n"+third+"|z"+ending, "|", string(delimiter)))
						clean := []byte(strings.ReplaceAll(without(first)+"|"+without(second)+"\n"+without(third)+"|z"+ending, "|", string(delimiter)))
						if len(clean) != len(body) {
							closed++
						}
						want, wantErr := readCSVBody(clean, delimiter, true)
						got, err := readCSV(bytes.Clone(body), delimiter, true)
						compared++
						if wantErr != nil {
							refused++
							if wantErr = errorInWrittenBody(wantErr, body, clean); err == nil || err.Error() != wantErr.Error() {
								t.Fatalf("delimiter %q, %q: err=%v, was %v", delimiter, body, err, wantErr)
							}
							continue
						}
						if err != nil || !slices.EqualFunc(trimmedFields(got), trimmedFields(want), slices.Equal[[]string]) {
							t.Fatalf("delimiter %q, %q: %v, read as %q, was %q", delimiter, body, err, got, want)
						}
					}
				}
			}
		}
	}
	if compared < 60000/every || refused < 10000/every || refused > compared-10000/every || closed < 5000/every {
		t.Fatalf("%d bodies were compared, %d of them refused and %d with blanks after a closing quote", compared, refused, closed)
	}
}

// trimmedFields is rows with every field trimmed, as decodeCSV trims them
// under trim_space.
func trimmedFields(rows [][]string) [][]string {
	for _, row := range rows {
		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}
	}
	return rows
}

// errorInWrittenBody is the error the reader gave for clean, which is the
// body written without some of its blanks, with the column the place has in
// written: the bytes of clean are those of written in their order, so the
// byte the error is at is found by going through both. The lines are the
// same in the two.
func errorInWrittenBody(err error, written, clean []byte) error {
	var parse *csv.ParseError
	if !errors.As(err, &parse) {
		return err
	}
	lineStart := func(body []byte, line int) int {
		start := 0
		for ; line > 1; line-- {
			start += bytes.IndexByte(body[start:], '\n') + 1
		}
		return start
	}
	at := lineStart(clean, parse.Line) + parse.Column - 1
	i := 0
	for j := 0; j < at; i++ {
		if written[i] == clean[j] {
			j++
		}
	}
	// The blanks taken out just before the place are before it in the
	// written body too.
	for i < len(written) && (at == len(clean) || written[i] != clean[at]) {
		i++
	}
	moved := *parse
	moved.Column = i - lineStart(written, parse.Line) + 1
	return &moved
}

// Taking the blanks around quotes out of a body leaves one without any as
// it is, the same bytes and no copy, and says of the others what it took
// out and where: the blanks before a field's opening quote only when asked
// to, as for a tab, and those between a closing quote and the delimiter or
// the line's end always, the carriage return of a line's end left where it
// is.
func TestCSVBlanksAroundQuotesAreTakenOutOfACopy(t *testing.T) {
	for _, plain := range []string{
		"a\t\"q\"\t x \n\"b \"\"\t\"\tc\n",
		"a\tb \t c\n",
		"\"q\"\r\n\"r\"\r",
		"\"q\" x\t\"r\" \"s\"\n",
	} {
		body := []byte(plain)
		if out, removed := withoutBlanksAroundQuotes(body, '\t', true); &out[0] != &body[0] || len(out) != len(body) || removed != nil {
			t.Errorf("%q: %q %v", plain, out, removed)
		}
	}
	body := []byte("  \"a\"\t \"b \t \"\"c\"\n\u00a0\"d\"\tx \"e\"\t \n")
	before := bytes.Clone(body)
	out, removed := withoutBlanksAroundQuotes(body, '\t', true)
	if want := "\"a\"\t\"b \t \"\"c\"\n\"d\"\tx \"e\"\t \n"; string(out) != want || !bytes.Equal(body, before) {
		t.Fatalf("%q, want %q; the body is now %q", out, want, body)
	}
	if want := []csvRemoved{{at: 0, n: 2}, {at: 4, n: 1}, {at: 14, n: 2}}; !slices.Equal(removed, want) {
		t.Fatalf("%v, want %v", removed, want)
	}
	// After a closing quote, and before an opening one only when asked to.
	body = []byte(" \"a\" ,\"b\"\u00a0\r\n\"c \"\"\" \t,x \"d\" ,\"e\" \r")
	for leading, want := range map[bool]struct {
		out     string
		removed []csvRemoved
	}{
		false: {" \"a\",\"b\"\r\n\"c \"\"\",x \"d\" ,\"e\"\r", []csvRemoved{{at: 4, n: 1}, {at: 8, n: 2}, {at: 16, n: 2}, {at: 27, n: 1}}},
		true:  {"\"a\",\"b\"\r\n\"c \"\"\",x \"d\" ,\"e\"\r", []csvRemoved{{at: 0, n: 1}, {at: 3, n: 1}, {at: 7, n: 2}, {at: 15, n: 2}, {at: 26, n: 1}}},
	} {
		out, removed := withoutBlanksAroundQuotes(bytes.Clone(body), ',', leading)
		if string(out) != want.out || !slices.Equal(removed, want.removed) {
			t.Errorf("before opening quotes %v: %q %v, want %q %v", leading, out, removed, want.out, want.removed)
		}
	}
}

// trim_space trims the blanks after a quoted field too, between its closing
// quote and the delimiter or the end of the line, which failed the decode as
// a stray quote: with a comma, a semicolon and a tab as the delimiter, before
// a line feed, a CRLF and the end of the body, after a field with a doubled
// quote, a delimiter and a line break in it, a CRLF there read as a line
// feed, and in a header. The blanks
// inside the quotes are the field's until its text is trimmed. Without
// trim_space such a field fails the decode as it did.
func TestCSVTrimSpaceTrimsTheBlanksAfterAQuotedField(t *testing.T) {
	const rows = `[{"cpu":"1","host":"web01"},{"cpu":"2","host":"say \"hi\", twice"},{"cpu":"3","host":"two\nlines"},{"cpu":"x","host":"last"}]`
	for _, delimiter := range []string{",", ";", "\t", "|"} {
		for _, ending := range []string{"\n", "\r\n"} {
			for _, final := range []string{ending, ""} {
				body := strings.NewReplacer(",", delimiter, "\n", ending).Replace(
					"\"host\" ,cpu\n\"web01\" ,1\n\"say \"\"hi\"\", twice\"  ,\"2\"\u00a0\n\"two\nlines\"\u00a0 ,3\n last ,\" x \" ") + final
				want := rows
				if delimiter != "," {
					// The comma inside the quotes is text there, and the
					// delimiter's own would be too.
					want = strings.Replace(rows, `\"hi\", twice`, `\"hi\"`+delimiter+` twice`, 1)
					want = strings.ReplaceAll(want, "\t", `\t`)
				}
				got, err := csvRows(t, delimiter, true, body)
				if err != nil || got != want {
					t.Errorf("delimiter %q, line ends %q, last %q: %v\nrows %s\nwant %s", delimiter, ending, final, err, got, want)
				}
				if _, err := csvRows(t, delimiter, false, body); err == nil || !strings.Contains(err.Error(), `extraneous or missing " in quoted-field`) {
					t.Errorf("delimiter %q without trim_space: %v, want the stray quote of line 1", delimiter, err)
				}
			}
		}
	}
	// Without a header row too, and the repro of the bug: one blank.
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
		Response: model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(false), TrimSpace: true}}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte("host,cpu\n\"web01\" ,1\n"), Headers: make(http.Header)}, &c)
	if err != nil || asJSON(t, d.Data) != `[["host","cpu"],["web01","1"]]` {
		t.Errorf("without a header row: %v, %v", err, d)
	}
}

// What follows a closing quote other than blanks and then the delimiter or
// the line's end is still refused under trim_space, at the line and column
// the body has it in, the blanks taken out before it on its line counted:
// text after the blanks, a second quoted part, and a stray quote or an open
// field later on the line or on a later one. The column is the reader's: of
// the quote it stops at, or past the end of the line a field is left open
// on.
func TestCSVTrimSpaceStillRefusesTextAfterAClosingQuote(t *testing.T) {
	for _, tc := range []struct {
		delimiter rune
		body      string
		want      string
	}{
		{',', "a,b\n\"q\" x,1\n", `parse error on line 2, column 3: extraneous or missing " in quoted-field`},
		{',', "a,b\n\"q\" \"r\",1\n", `parse error on line 2, column 3: extraneous or missing " in quoted-field`},
		{',', "\"a\"  ,\"b\" ,\"st\"ray\"\n", `parse error on line 1, column 15: extraneous or missing " in quoted-field`},
		{';', "\"a\" ;b\n\"c\"\u00a0;\"d\"  ;  \"st\"ray\"\n", `parse error on line 2, column 18: extraneous or missing " in quoted-field`},
		{',', "\"a\" ,\"two\nlines\"  ,\"open\n", `record on line 1; parse error on line 2, column 16: extraneous or missing " in quoted-field`},
		{'\t', " \"a\" \t\"b\"  \t \"st\"ray\"\n", `parse error on line 1, column 17: extraneous or missing " in quoted-field`},
		{'\t', "5\" disk\t\"a\" \n\"b\" \t\"st\"ray\"\n", `parse error on line 2, column 9: extraneous or missing " in quoted-field`},
		{' ', "\"a\"\t \"b\"\t\"c\"\n", `parse error on line 1, column 8: extraneous or missing " in quoted-field`},
	} {
		if _, err := readCSV([]byte(tc.body), tc.delimiter, true); err == nil || err.Error() != tc.want {
			t.Errorf("delimiter %q, %q: err=%v, want %s", tc.delimiter, tc.body, err, tc.want)
		}
	}
}

// With a space as the delimiter the spaces after a closing quote are the
// delimiter, and a run of them one delimiter, as before: nothing is taken
// from them, and a quoted field at the end of a line with spaces after it is
// followed by an empty field, as an unquoted one is. A tab or another blank
// that is no space is trimmed there as everywhere.
func TestCSVTrimSpaceLeavesTheSpacesAfterAQuoteToTheSpaceDelimiter(t *testing.T) {
	for body, want := range map[string]string{
		"host note cpu\nweb01 \"two  words\"   72\n":             `[{"cpu":"72","host":"web01","note":"two  words"}]`,
		"host note cpu\n\"web01\"   \"two  words\" \"72\"\n":     `[{"cpu":"72","host":"web01","note":"two  words"}]`,
		"host note cpu\n\"web01\"\t \"x\"\t\t72\n":               `[{"cpu":"","host":"web01","note":"x\"\t\t72"}]`,
		"host note cpu\n\"web01\"\t \"two  words\"\u00a0\t 72\n": `[{"cpu":"72","host":"web01","note":"two  words"}]`,
		"host note cpu\n\"web01\" \"x\"\t\n":                     `[{"cpu":"","host":"web01","note":"x"}]`,
	} {
		got, err := csvRows(t, " ", true, body)
		if strings.Contains(body, "\t\t72") {
			// Blanks that the delimiter does not follow are no blanks after
			// the field: the reader refuses it as it did.
			if err == nil || !strings.Contains(err.Error(), `parse error on line 2, column 12: extraneous or missing " in quoted-field`) {
				t.Errorf("%q: %v, rows %s", body, err, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %v, rows %s, want %s", body, err, got, want)
		}
	}
	// A quoted last field with spaces after it: the same rows, and the same
	// refusal of the empty column those spaces make under an empty header
	// name, as before blanks after a closing quote were trimmed.
	for _, body := range []string{"a b\n\"1\" \"2\" \n", "a b \n1 \"2\"  \n"} {
		got, err := readCSV([]byte(body), ' ', true)
		was, wasErr := readCSVBody([]byte(body), ' ', true)
		if err != nil || wasErr != nil || !slices.EqualFunc(got, was, slices.Equal[[]string]) {
			t.Errorf("%q: read as %q, %v; the reader alone reads %q, %v", body, got, err, was, wasErr)
		}
	}
}
