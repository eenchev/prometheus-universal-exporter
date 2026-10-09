package decode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestDecodeJSONAutoDetectionAndMalformedInput(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}}
	r := &fetch.HTTPResponse{Body: []byte(`[{"value":7}]`), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "json" {
		t.Fatalf("detected kind=%q, want json", d.Kind)
	}
	values, ok := d.Data.([]any)
	if !ok || len(values) != 1 {
		t.Fatalf("decoded JSON array=%#v", d.Data)
	}
	row, ok := values[0].(map[string]any)
	if !ok || row["value"] != 7 {
		t.Fatalf("decoded JSON row=%#v", values[0])
	}

	c.Decoder.Type = "json"
	r.Body = []byte(`{"value":`)
	if _, err := Decode(r, &c); err == nil || !strings.Contains(err.Error(), "JSON decode") {
		t.Fatalf("malformed JSON error=%v", err)
	}
}

func TestDecodeCSVQuotedFieldsAndRowsWithoutHeader(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(true), Delimiter: ";", TrimSpace: true}}}
	r := &fetch.HTTPResponse{Body: []byte("server;note;cpu\n\"web;01\";\"up;ok\"; 72 \n"), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows := csvDocument(d.Data).([]any)
	row := rows[0].(map[string]any)
	if row["server"] != "web;01" || row["note"] != "up;ok" || row["cpu"] != "72" {
		t.Fatalf("decoded CSV row=%#v", row)
	}

	c.Response.CSV.Header = boolPtr(false)
	r.Body = []byte("web01;72\nweb02;31\n")
	d, err = Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows = csvDocument(d.Data).([]any)
	first := rows[0].([]any)
	if len(rows) != 2 || first[0] != "web01" || first[1] != "72" {
		t.Fatalf("decoded headerless CSV rows=%#v", rows)
	}
}

func TestDecodePrometheusPreservesTimestamp(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "prometheus"}}
	r := &fetch.HTTPResponse{Body: []byte("# TYPE vendor_value gauge\nvendor_value 42 1700000000000\n"), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set := d.Data.(model.MetricSet)
	if len(set.Metrics) != 1 || set.Metrics[0].Timestamp == nil || *set.Metrics[0].Timestamp != 1700000000000 {
		t.Fatalf("decoded Prometheus timestamp=%#v", set.Metrics)
	}
}

// A header naming one column twice fails the decode, naming it, rather than
// the later column overwriting the earlier; unnamed columns empty in every
// row, as a delimiter ending each line leaves, may repeat.
func TestCSVHeadersNamingAColumnTwiceAreRefused(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}}}
	r := &fetch.HTTPResponse{Body: []byte("server,cpu, cpu\nweb01,72,9\n"), Headers: make(http.Header)}
	if _, err := Decode(r, &c); err == nil || !strings.Contains(err.Error(), `names column "cpu" twice, as columns 2 and 3`) {
		t.Fatalf("%v", err)
	}
	r.Body = []byte("server,cpu,,\nweb01,72,,\n")
	if _, err := Decode(r, &c); err != nil {
		t.Fatalf("repeated empty names: %v", err)
	}
	c.Response.CSV.Header = boolPtr(false)
	r.Body = []byte("server,cpu,cpu\nweb01,72,9\n")
	if _, err := Decode(r, &c); err != nil {
		t.Fatalf("without a header: %v", err)
	}
}

// decodeAs decodes body with the decoder kind, auto included, and the
// Content-Type contentType, if any.
func decodeAs(t *testing.T, kind, contentType, body string) (*Decoded, error) {
	t.Helper()
	headers := make(http.Header)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: kind}}
	return Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: headers}, &c)
}

// A JSON body is one value: a second, as NDJSON has, or anything else after
// it fails the decode saying so, rather than the rest being dropped; trailing
// whitespace is fine.
func TestJSONTrailingDataIsRefused(t *testing.T) {
	for _, body := range []string{"{\"a\":1}\n{\"a\":2}\n", "{\"a\":1}\ngarbage", `{"a":1} {"a":2}`, `[1]]`} {
		for _, kind := range []string{"json", "auto"} {
			if _, err := decodeAs(t, kind, "application/json", body); err == nil || !strings.Contains(err.Error(), "trailing data after the JSON value (NDJSON, one value per line, is not supported)") {
				t.Errorf("%s %q: err=%v", kind, body, err)
			}
		}
	}
	d, err := decodeAs(t, "json", "", "{\"a\":1}\n \t\r\n")
	if err != nil || d.Data.(map[string]any)["a"] != 1 {
		t.Fatalf("trailing whitespace: %v %#v", err, d)
	}
}

// A YAML body is one document: a second fails the decode saying multi-document
// YAML is not supported, while a leading ---, a trailing --- or ... and an
// empty body are fine.
func TestYAMLMultipleDocumentsAreRefused(t *testing.T) {
	for _, body := range []string{"a: 1\n---\na: 2\n", "---\na: 1\n---\n- 2\n", "a: 1\n--- ~\n"} {
		if _, err := decodeAs(t, "yaml", "", body); err == nil || !strings.Contains(err.Error(), "multi-document YAML is not supported") {
			t.Errorf("%q: err=%v", body, err)
		}
	}
	for _, body := range []string{"a: 1\n", "---\na: 1\n", "a: 1\n---\n", "---\na: 1\n...\n", "a: 1\n---\n# the end\n"} {
		d, err := decodeAs(t, "yaml", "", body)
		if err != nil || d.Data.(map[string]any)["a"] != 1 {
			t.Errorf("%q: %v %#v", body, err, d)
		}
	}
	if d, err := decodeAs(t, "yaml", "", ""); err != nil || d.Data != nil {
		t.Fatalf("empty: %v %#v", err, d)
	}
}

// A quote inside an unquoted CSV field is taken as written, while quoted
// fields, escaped quotes in them, are read as before and a quoted field left
// open still fails.
func TestCSVStrayQuotesInUnquotedFields(t *testing.T) {
	d, err := decodeAs(t, "csv", "", "name,size\n\"disk 5\"\" \",10\nbad 5\" disk,20\n\"a,b\",30\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, d.Data); got != `[{"name":"disk 5\" ","size":"10"},{"name":"bad 5\" disk","size":"20"},{"name":"a,b","size":"30"}]` {
		t.Fatalf("rows %s", got)
	}
	if _, err := decodeAs(t, "csv", "", "name,size\n\"open,10\nweb,20\n"); err == nil || !strings.Contains(err.Error(), "CSV decode") {
		t.Fatalf("an open quoted field: %v", err)
	}
}

// An unnamed CSV column that holds a value fails the decode naming its
// number, rather than every unnamed column overwriting the "" key; one empty
// in every row is left out of the rows.
func TestCSVUnnamedColumnsWithValuesAreRefused(t *testing.T) {
	for body, want := range map[string]string{
		"name,,\na,1,2\n":          "CSV header leaves column 2 unnamed",
		"name,cpu,\na,1,\nb,2,x\n": "CSV header leaves column 3 unnamed",
	} {
		if _, err := decodeAs(t, "csv", "", body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
	d, err := decodeAs(t, "csv", "", "name,cpu,\na,1,\nb,2\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, d.Data); got != `[{"cpu":"1","name":"a"},{"cpu":"2","name":"b"}]` {
		t.Fatalf("rows %s", got)
	}
}

// A row with a value past the header's last column fails the decode, naming
// the line the value is on and its column, where the value was dropped
// without a word: a row split by a delimiter the header's line has none of,
// and a quoted field of tab-separated values read without trim_space, showed
// only as values in the wrong columns. The line is the body's own, blank
// lines and the lines of a quoted field counted, also in a body read
// leniently for a bare quote and one read under trim_space. A header ending
// in a delimiter names no column there either, and its column of values is
// refused first, as it was.
func TestCSVRowsWithAValuePastTheHeaderAreRefused(t *testing.T) {
	const advice = ", which the header does not name; name the column in the header, or set response.csv.header: false and read the columns by number; " +
		"if the line is split where it should not be, check response.csv.delimiter and response.csv.trim_space"
	for _, tc := range []struct {
		delimiter string
		trim      bool
		body      string
		want      string
	}{
		{"", false, "a,b\n1,2,3\n", "CSV line 2 has a value in column 3" + advice},
		{"", false, "a,b\n1,2\n3,4,,x,y\n", "CSV line 3 has a value in column 4" + advice},
		{"", false, "a,b\n\n\"x\ny\",2\n\n1,2,3\n", "CSV line 6 has a value in column 3" + advice},
		{"", false, "a,b\n1,\"x\ny\",\"\",\"z\nw\"\n", "CSV line 3 has a value in column 4" + advice},
		{"", false, "a,b\n5\" disk,1\nx,2,3\n", "CSV line 3 has a value in column 3" + advice},
		{"", false, "a,b\n1,2, \n", "CSV line 2 has a value in column 3" + advice},
		{"", true, "a,b\n \"1\" ,2 , , 3 \n", "CSV line 2 has a value in column 4" + advice},
		{"", false, "host;cpu\nweb01;72,5\n", "CSV line 2 has a value in column 2" + advice},
		{"\t", false, "host\tnote\nweb01\tok\nweb02\t \"x\ty\"\n", "CSV line 3 has a value in column 3" + advice},
		{"\t", true, " \"host\" \t\"note\"\n\n \"web01\" \t \"x\ty\" \t7\n", "CSV line 3 has a value in column 3" + advice},
		{" ", true, "host  cpu\nweb01  72  two words\n", "CSV line 2 has a value in column 3" + advice},
		{"", false, "a,b,\n1,2,,4\n", "CSV line 2 has a value in column 4" + advice},
		{"", false, "a,b,\n1,2,3,4\n", "CSV header leaves column 3 unnamed, and it holds values; name it, or set response.csv.header: false and read the columns by number"},
	} {
		if got, err := csvRows(t, tc.delimiter, tc.trim, tc.body); err == nil || err.Error() != tc.want {
			t.Errorf("delimiter %q, trim_space %v, %q:\ngot  %v (rows %s)\nwant %s", tc.delimiter, tc.trim, tc.body, err, got, tc.want)
		}
	}
}

// Fields past the header's last column that hold nothing are what a
// delimiter ending a line leaves, and are left out as they were, one row
// having them and another not: empty ones, an empty quoted one, and under
// trim_space one of blanks. A row shorter than the header still has empty
// text in the columns it lacks. Without a header row there is no column a
// row may not have: rows of any lengths decode into lists as long as they
// are.
func TestCSVEmptyFieldsPastTheHeaderAndShortRowsAreRead(t *testing.T) {
	for _, tc := range []struct {
		trim       bool
		body, want string
	}{
		{false, "a,b\n1,2,\n3,4,,\n5,6\n7\n", `[{"a":"1","b":"2"},{"a":"3","b":"4"},{"a":"5","b":"6"},{"a":"7","b":""}]`},
		{false, "a,b\n1,2,\"\",\n", `[{"a":"1","b":"2"}]`},
		{true, "a,b\n1,2, ,\t\n3 , 4 ,\" \" \n", `[{"a":"1","b":"2"},{"a":"3","b":"4"}]`},
		{false, "a,b,\n1,2,,\n3,4\n", `[{"a":"1","b":"2"},{"a":"3","b":"4"}]`},
	} {
		if got, err := csvRows(t, "", tc.trim, tc.body); err != nil || got != tc.want {
			t.Errorf("trim_space %v, %q: %v, rows %s, want %s", tc.trim, tc.body, err, got, tc.want)
		}
	}
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"},
		Response: model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(false)}}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte("a,b\n1,2,3\n4\n5,6,,x,\n"), Headers: make(http.Header)}, &c)
	if err != nil {
		t.Fatalf("without a header row: %v", err)
	}
	if got, want := asJSON(t, d.Data), `[["a","b"],["1","2","3"],["4"],["5","6","","x",""]]`; got != want {
		t.Errorf("without a header row: rows %s, want %s", got, want)
	}
}

// Automatic detection recognises HTML after whitespace, comments and an XML
// declaration, in any case, and parses it as HTML, which a <br> would fail
// as XML; other markup is still XML.
func TestHTMLIsRecognisedAfterCommentsAndDeclarations(t *testing.T) {
	for _, body := range []string{
		"  \n<!DOCTYPE html>\n<html><p>1</p><br></html>",
		"\n\n<!-- generated -->\n<!DOCTYPE html><html><body><p>1</p><br></body></html>",
		"<?xml version=\"1.0\"?>\n<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML 1.0 Strict//EN\">\n<html><body><p>1</p><br></body></html>",
		"<!-- a --><!-- b -->\n<HTML lang=\"en\"><p>1</p><br></HTML>",
		"<html>",
	} {
		if d, err := decodeAs(t, "auto", "", body); err != nil || d.Kind != "html" {
			t.Errorf("%q: %v %#v", body, err, d)
		}
	}
	for _, body := range []string{"<?xml version=\"1.0\"?><!-- c --><htmlish>1</htmlish>", "<root><html>1</html></root>", "<!-- open <html>"} {
		if d, err := decodeAs(t, "auto", "", body); err == nil && d.Kind == "html" {
			t.Errorf("%q decoded as HTML", body)
		}
	}
}

// htmlText is the text of the elements selector picks in an HTML decode.
func htmlText(t *testing.T, d *Decoded, selector string) string {
	t.Helper()
	return d.Data.(*HTMLDecoded).Document.Find(selector).Text()
}

// A <meta charset> naming UTF-16 means UTF-8, as browsers read it, since the
// page it is in was read as ASCII, and x-user-defined means windows-1252; a
// Content-Type header naming UTF-16 is still taken as it says.
func TestHTMLMetaCharsetUTF16MeansUTF8(t *testing.T) {
	for _, charset := range []string{"utf-16", "UTF-16LE", "utf-16be"} {
		d, err := decodeAs(t, "auto", "text/html", `<html><head><meta charset="`+charset+`"></head><body><p id=v>42 café</p></body></html>`)
		if err != nil || htmlText(t, d, "#v") != "42 café" {
			t.Errorf("%s: %v %q", charset, err, htmlText(t, d, "#v"))
		}
	}
	d, err := decodeAs(t, "auto", "", "<html><head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=x-user-defined\"></head><body><p id=v>caf\xe9 \x80</p></body></html>")
	if err != nil || htmlText(t, d, "#v") != "café €" {
		t.Fatalf("x-user-defined: %v %q", err, htmlText(t, d, "#v"))
	}
	utf16 := []byte{}
	for _, c := range "<html><p id=v>42</p></html>" {
		utf16 = append(utf16, byte(c), 0)
	}
	d, err = decodeAs(t, "html", "text/html; charset=utf-16le", string(utf16))
	if err != nil || htmlText(t, d, "#v") != "42" {
		t.Fatalf("a UTF-16 header: %v %q", err, htmlText(t, d, "#v"))
	}
}
