package decode

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
// the same line and column — but for a body with an empty field, the one
// case the two are meant to differ in, which is left out here: one where a
// field starts, after blanks or without, with the delimiter.
func TestCSVTrimSpaceReadsTabSeparatedValuesAsTheReaderDidButForEmptyFields(t *testing.T) {
	atoms := []string{`a`, `b c`, ` lead`, `trail `, `"q"`, ` "q"`, `  "a|b"`, `"x""y"`, " \u00a0\"x \"\" y\"", "\"two\nlines\"", " \"two\n \"\"l|nes\"", `"open`, ` "open`, `"st"ray"`, ` "st"ray"`, `5" disk`, ` 5" disk`, `"sp" `, `x "mid"`, ` "a| |""b"`, "\r\"cr\"", ` " "`}
	trimmed := func(rows [][]string) [][]string {
		for _, row := range rows {
			for i := range row {
				row[i] = strings.TrimSpace(row[i])
			}
		}
		return rows
	}
	compared, refused := 0, 0
	for _, delimiter := range []rune{'\t', '\u2003'} {
		for _, first := range atoms {
			for _, second := range atoms {
				for _, third := range atoms {
					for _, ending := range []string{"\n", "\r\n", ""} {
						body := []byte(strings.ReplaceAll(first+"|"+second+"\n"+third+"|z"+ending, "|", string(delimiter)))
						want, wantErr := readCSVBody(body, delimiter, true)
						got, err := readCSV(bytes.Clone(body), delimiter, true)
						compared++
						if wantErr != nil {
							refused++
							if err == nil || err.Error() != wantErr.Error() {
								t.Fatalf("delimiter %q, %q: err=%v, was %v", delimiter, body, err, wantErr)
							}
							continue
						}
						if err != nil || !slices.EqualFunc(trimmed(got), trimmed(want), slices.Equal[[]string]) {
							t.Fatalf("delimiter %q, %q: %v, read as %q, was %q", delimiter, body, err, got, want)
						}
					}
				}
			}
		}
	}
	if compared < 60000 || refused < 10000 || refused > compared-10000 {
		t.Fatalf("%d bodies were compared, %d of them refused", compared, refused)
	}
}

// Taking the blanks before quotes out of a body leaves one without any as
// it is, the same bytes and no copy, and says of the others what it took
// out and where.
func TestCSVBlanksBeforeQuotesAreTakenOutOfACopy(t *testing.T) {
	plain := []byte("a\t\"q\"\t x \n\"b \"\"\t\"\tc\n")
	if out, removed := withoutBlanksBeforeQuotes(plain, '\t'); &out[0] != &plain[0] || len(out) != len(plain) || removed != nil {
		t.Fatalf("%q %v", out, removed)
	}
	body := []byte("  \"a\"\t \"b \t \"\"c\"\n\u00a0\"d\"\tx \"e\"\t \n")
	before := bytes.Clone(body)
	out, removed := withoutBlanksBeforeQuotes(body, '\t')
	if want := "\"a\"\t\"b \t \"\"c\"\n\"d\"\tx \"e\"\t \n"; string(out) != want || !bytes.Equal(body, before) {
		t.Fatalf("%q, want %q; the body is now %q", out, want, body)
	}
	if want := []csvRemoved{{at: 0, n: 2}, {at: 4, n: 1}, {at: 14, n: 2}}; !slices.Equal(removed, want) {
		t.Fatalf("%v, want %v", removed, want)
	}
}
