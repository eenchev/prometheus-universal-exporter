package decode

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// fluxAnswer is InfluxDB's annotated CSV of a Flux query that keeps two
// columns, as /api/v2/query answers it: three annotation lines, a header
// whose first column has no name, and two tables of one schema, which share
// the header.
const fluxAnswer = "#group,false,false,true,false\r\n" +
	"#datatype,string,long,string,double\r\n" +
	"#default,_result,,,\r\n" +
	",result,table,host,_value\r\n" +
	",,0,web01,12.5\r\n" +
	",,1,web02,40\r\n" +
	"\r\n"

// With response.csv.comment, the lines that begin with it are left out:
// annotated CSV is read as its header and its rows, the unnamed first column
// left out as an empty one always is. Without it, the first annotation line
// is the header, and its columns are what the rows are read by.
func TestCSVCommentLinesAreLeftOut(t *testing.T) {
	got, err := decodedCSV(t, fluxAnswer, model.CSVConfig{Comment: "#"})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"result": "", "table": "0", "host": "web01", "_value": "12.5"},
		map[string]any{"result": "", "table": "1", "host": "web02", "_value": "40"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got, err = decodedCSV(t, fluxAnswer, model.CSVConfig{})
	if err == nil {
		if rows, _ := got.([]any); len(rows) > 0 {
			if _, named := rows[0].(map[string]any)["host"]; named {
				t.Fatalf("without a comment character the annotation lines were left out: %v", got)
			}
		}
	}
}

// A comment line is one that begins with the character where a record
// begins: a line of a quoted field that begins with it is the field's text,
// a line that begins with a blank before it is a row, and a quote in a
// comment line opens no field, whatever line ends the body has and with
// trim_space or without.
func TestCSVCommentLinesAreOnlyWholeLines(t *testing.T) {
	for _, end := range []string{"\n", "\r\n", "\r"} {
		for _, trim := range []bool{false, true} {
			body := strings.Join([]string{
				`# a quote "opens nothing`,
				"host,note",
				`web01,"two` + end + `#lines"`,
				"#web02,skipped",
				"web03,last",
			}, end) + end
			got, err := decodedCSV(t, body, model.CSVConfig{Comment: "#", TrimSpace: trim})
			if err != nil {
				t.Fatalf("%q, trim %v: %v", end, trim, err)
			}
			want := []any{
				map[string]any{"host": "web01", "note": "two\n#lines"},
				map[string]any{"host": "web03", "note": "last"},
			}
			if end == "\r" {
				// A carriage return alone is the text of a quoted field.
				want[0].(map[string]any)["note"] = "two\r#lines"
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q, trim %v: got %q, want %q", end, trim, got, want)
			}
		}
	}
	got, err := decodedCSV(t, "host;cpu\n #web01;1\n", model.CSVConfig{Comment: "#", Delimiter: ";"})
	if err != nil || !reflect.DeepEqual(got, []any{map[string]any{"host": " #web01", "cpu": "1"}}) {
		t.Fatalf("a line that begins with a blank: %v, %v", got, err)
	}
}

// The lines that an error names are the body's own: the comment lines
// before a row are counted as the file has them.
func TestCSVCommentLinesKeepTheLineNumbers(t *testing.T) {
	body := "#one\n#two\nhost,cpu\n#three\nweb01,1,extra\n"
	_, err := decodedCSV(t, body, model.CSVConfig{Comment: "#"})
	if err == nil || !strings.Contains(err.Error(), "CSV line 5 has a value in column 3") {
		t.Fatalf("error %v, want it to name line 5", err)
	}
	_, err = decodedCSV(t, "#one\nhost,cpu\n\"open,1\n", model.CSVConfig{Comment: "#"})
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("error %v, want it to name line 3", err)
	}
}

// A body without the comment character, or with it in no comment line, is
// read as it is, without a copy.
func TestCSVBodyWithoutCommentLinesIsNotCopied(t *testing.T) {
	for _, body := range []string{"host,cpu\nweb01,1\n", "host,cpu\nweb#01,1\n"} {
		in := []byte(body)
		if out, _ := withoutCommentLines(in, '#', ',', false); &out[0] != &in[0] || string(out) != body {
			t.Errorf("%q was copied or changed: %q", body, out)
		}
	}
}

// A table of annotated CSV that follows an empty line and comment lines has
// a header of its own. When it names the first header's columns it is that
// header written again and is left out; when it names others, the decode
// fails naming its line, rather than its rows being read by the first
// header's names. Comment lines between rows without an empty line before
// them begin no table, and a row after a header left out is still named by
// its own line.
func TestCSVTablesAfterCommentLines(t *testing.T) {
	second := "\n#group,false,false,true,false\n#datatype,string,long,string,double\n#default,_result,,,\n"
	same := strings.TrimSuffix(fluxAnswer, "\r\n") + second + ",result,table,host,_value\n,,2,web03,7\n"
	got, err := decodedCSV(t, same, model.CSVConfig{Comment: "#"})
	if err != nil {
		t.Fatal(err)
	}
	if rows := got.([]any); len(rows) != 3 || rows[2].(map[string]any)["host"] != "web03" {
		t.Fatalf("got %v, want the rows of both tables and no header among them", got)
	}
	other := strings.TrimSuffix(fluxAnswer, "\r\n") + second + ",result,table,region,_value\n,,2,eu,7\n"
	if _, err := decodedCSV(t, other, model.CSVConfig{Comment: "#"}); err == nil || !strings.Contains(err.Error(), "CSV line 11 is the header of a new table, after comment lines, and names other columns than the first header") {
		t.Fatalf("error %v, want it to name line 11", err)
	}
	got, err = decodedCSV(t, "host,cpu\nweb01,1\n# a note\nweb02,2\n", model.CSVConfig{Comment: "#"})
	if err != nil || len(got.([]any)) != 2 {
		t.Fatalf("a comment between rows: %v, %v", got, err)
	}
	extra := same + ",,3,web04,8,surplus\n"
	if _, err := decodedCSV(t, extra, model.CSVConfig{Comment: "#"}); err == nil || !strings.Contains(err.Error(), "CSV line 13 has a value in column 6") {
		t.Fatalf("error %v, want it to name line 13", err)
	}
}
