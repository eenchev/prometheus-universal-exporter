package decode

import (
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

// With a tab for a delimiter, or another blank, trim_space leaves an empty
// field where it stands: the tab after it is the next delimiter, not a blank
// leading the field after, which the reader took it for, moving the rest of
// the row a column to the left without a word. The fields are still trimmed
// on both sides.
func TestCSVTrimSpaceKeepsTheColumnsOfTabSeparatedValues(t *testing.T) {
	for _, delimiter := range []string{"\t", " "} {
		body := strings.ReplaceAll("host|note|cpu\nweb01|x|72\nweb02||31\n|y|\n", "|", delimiter)
		for _, trim := range []bool{false, true} {
			c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
				Response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: delimiter, TrimSpace: trim}}}
			d, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: make(http.Header)}, &c)
			if err != nil {
				t.Fatalf("delimiter %q, trim_space %v: %v", delimiter, trim, err)
			}
			if got, want := asJSON(t, d.Data), `[{"cpu":"72","host":"web01","note":"x"},{"cpu":"31","host":"web02","note":""},{"cpu":"","host":"","note":"y"}]`; got != want {
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
