package decode

import (
	"bytes"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The files of testdata/csv are CSV in the shapes it comes in, each written
// as the tool it stands for writes it. These tests read them with the decoder
// alone, into rows; the csvfixtures tests of internal/transform and
// internal/exporter make series of them.

// csvFixtures says, for every file of testdata/csv, the shape it stands for.
// docs/DEVELOPMENT.md lists them too.
var csvFixtures = map[string]string{
	"status.csv":                   "the specification's own example: a header and two rows",
	"tickets-rfc4180.csv":          "a helpdesk's ticket export as RFC 4180 writes it: CRLF line ends, and fields with commas, doubled quotes and line breaks in quotes",
	"inventory-semicolon.csv":      "a stock list as a spreadsheet saves it with a German or Bulgarian locale: semicolons, and numbers with a decimal comma in quotes",
	"sensors.tsv":                  "a data logger's tab-separated readings, with empty fields in the middle of rows and at their end, and fields padded with spaces",
	"queues-pipe.txt":              "a query's result as psql -A prints it: fields separated by a pipe, and a footer counting the rows",
	"accounts-colon.txt":           "accounts in the form of /etc/passwd: no header, fields separated by a colon",
	"readings-noheader.csv":        "what a data logger appends to its file: no header, a reading a line",
	"cities-utf8-bom.csv":          "a list of cities with headers and values in Cyrillic, accented Latin, Greek, CJK and emoji, as a spreadsheet's \"CSV UTF-8\" saves it, with a byte order mark",
	"cities-utf16le-bom.csv":       "the same list as a spreadsheet's \"Unicode text\": UTF-16, little-endian, with a byte order mark",
	"cities-utf16be.csv":           "the same list in UTF-16, big-endian, without a byte order mark",
	"oblasti-utf8.csv":             "the Bulgarian part of such a list, in UTF-8 without a byte order mark",
	"oblasti-windows-1251.csv":     "the same Bulgarian list as an older system writes it, in windows-1251",
	"communes-iso-8859-1.csv":      "the French part of such a list in ISO 8859-1",
	"nodes-space-aligned.txt":      "a cluster tool's table: columns aligned with spaces, numbers to the right, a note of several words in quotes",
	"hosts-trailing-delimiter.csv": "an export that ends every line, the header's too, with the delimiter",
	"jobs-short-rows.csv":          "a scheduler's report whose writer stops a row at its last value: rows with fewer fields than the header",
	"jobs-blank-lines.csv":         "the same report with blank lines between the rows and after them, and one line of blanks",
	"jobs-no-final-newline.csv":    "the same report without a line end after its last row",
	"usage-duplicate-columns.csv":  "a capacity report whose header names two pairs of columns alike",
	"usage-unnamed-column.csv":     "a capacity report saved from a spreadsheet with an empty header cell above a column of values",
	"numbers.csv":                  "the ways exports write a number, and what they write in place of one",
	"backups-times.csv":            "a backup tool's report, each column's time written another way",
	"usgs-all-hour.csv":            "the USGS earthquake feed's all_hour.csv, in its documented columns",
	"service-status.csv":           "a fleet's status export: a row per service and host, its state in words, counters and gauges",
}

func readCSVFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/csv/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// decodeCSVFixture decodes a fixture as the csv decoder reads a response
// with the Content-Type given, if any, under the collector's response
// settings.
func decodeCSVFixture(t *testing.T, name, contentType string, response model.ResponseConfig) (*Decoded, error) {
	t.Helper()
	headers := make(http.Header)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	c := model.Collector{Name: "fixture", Decoder: model.DecoderConfig{Type: "csv"}, Response: response}
	return Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: readCSVFixture(t, name), Headers: headers}, &c)
}

// Every file of testdata/csv is one of the fixtures listed here, with the
// shape it stands for, and docs/DEVELOPMENT.md names each: a fixture added
// without a word of what it is for, or left behind by a test that no longer
// reads it, fails this.
func TestEveryCSVFixtureIsListedAndDocumented(t *testing.T) {
	entries, err := os.ReadDir("../../testdata/csv")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		files = append(files, entry.Name())
	}
	if listed := slices.Sorted(maps.Keys(csvFixtures)); !slices.Equal(files, listed) {
		t.Errorf("testdata/csv holds\n%s\nand the fixtures listed are\n%s", strings.Join(files, "\n"), strings.Join(listed, "\n"))
	}
	development, err := os.ReadFile("../../docs/DEVELOPMENT.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if !bytes.Contains(development, []byte("`"+name+"`")) {
			t.Errorf("docs/DEVELOPMENT.md does not name testdata/csv/%s", name)
		}
	}
}

// The fixtures are written the way their names say, which an editor that
// normalises line ends or encodings would undo: the RFC 4180 export ends
// every line with CRLF, the byte order marks are there, the lists in other
// encodings are the same text as their UTF-8 twins, one report has no final
// line end, and the padded fields keep their blanks.
func TestTheCSVFixturesAreWrittenAsTheirNamesSay(t *testing.T) {
	tickets := readCSVFixture(t, "tickets-rfc4180.csv")
	if crlf, lf := bytes.Count(tickets, []byte("\r\n")), bytes.Count(tickets, []byte("\n")); crlf != lf || crlf != 16 {
		t.Errorf("tickets-rfc4180.csv has %d CRLF line ends and %d line feeds, want 16 of each", crlf, lf)
	}
	for name, mark := range map[string]string{"cities-utf8-bom.csv": "\xef\xbb\xbf", "cities-utf16le-bom.csv": "\xff\xfe"} {
		if !bytes.HasPrefix(readCSVFixture(t, name), []byte(mark)) {
			t.Errorf("%s does not start with its byte order mark", name)
		}
	}
	if bytes.HasPrefix(readCSVFixture(t, "cities-utf16be.csv"), []byte("\xfe\xff")) {
		t.Error("cities-utf16be.csv starts with a byte order mark")
	}
	text := func(name, charset string) string {
		c := model.Collector{Decoder: model.DecoderConfig{Type: "text"}, Response: model.ResponseConfig{Charset: charset}}
		d, err := Decode(&fetch.HTTPResponse{Body: readCSVFixture(t, name), Headers: make(http.Header)}, &c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return d.Data.(string)
	}
	cities := text("cities-utf8-bom.csv", "")
	if !strings.HasPrefix(cities, "città,Land,température,влажност,状態,sky\nСофия,България,14.5,62,晴れ,☀️\n") {
		t.Errorf("cities-utf8-bom.csv starts %q", cities[:min(len(cities), 80)])
	}
	for name, charset := range map[string]string{"cities-utf16le-bom.csv": "", "cities-utf16be.csv": "utf-16be"} {
		if got := text(name, charset); got != cities {
			t.Errorf("%s is not the text of cities-utf8-bom.csv:\n%s", name, got)
		}
	}
	if got, want := text("oblasti-windows-1251.csv", "windows-1251"), text("oblasti-utf8.csv", ""); got != want {
		t.Errorf("oblasti-windows-1251.csv is not the text of oblasti-utf8.csv:\n%s", got)
	}
	if communes := readCSVFixture(t, "communes-iso-8859-1.csv"); !bytes.HasPrefix(communes, []byte("commune,d\xe9partement,temp\xe9rature,humidit\xe9,\xe9tat\n")) {
		t.Errorf("communes-iso-8859-1.csv starts %q", communes[:min(len(communes), 60)])
	}
	if report := readCSVFixture(t, "jobs-no-final-newline.csv"); !bytes.HasSuffix(report, []byte("\nrenew-certs,skipped,0,0")) {
		t.Errorf("jobs-no-final-newline.csv ends %q", report[max(0, len(report)-30):])
	}
	for name, padded := range map[string]string{
		"jobs-blank-lines.csv": "\n   \n",
		"numbers.csv":          "\nblanks around,  42  \n",
		"sensors.tsv":          "\nth-04\t office \t 21.5\t40 \t 12 \tbattery low\n",
	} {
		if !bytes.Contains(readCSVFixture(t, name), []byte(padded)) {
			t.Errorf("%s no longer holds %q", name, padded)
		}
	}
}

// Each fixture decodes into its rows, by header name or, without a header
// row, as lists: as many rows as the file has records, every row with the
// header's columns, and the rows that are hard to read holding exactly what
// the file says.
//
//   - RFC 4180: a comma, a doubled quote and a line break inside a quoted
//     field are the field's text, the CRLF of a line break read as a line
//     feed, and an empty field is an empty text.
//   - A delimiter other than a comma leaves commas in the fields.
//   - With a tab as the delimiter an empty field keeps its column, in the
//     middle of a row and at its end; trim_space takes the blanks around a
//     field and lets a quoted field start after blanks, and without it both
//     are read as written.
//   - A row with fewer fields than the header has empty text in the columns
//     it lacks, as the footer of a psql result has.
//   - Without a header row a row is the list of its fields, an empty one
//     included.
//   - A byte order mark is no part of the first column's name; a body in
//     UTF-16 or a legacy encoding is read by its byte order mark, by
//     response.charset, which a Content-Type naming another does not undo,
//     or by the Content-Type's charset.
//   - With a space as the delimiter and trim_space, a run of spaces is one
//     delimiter and a quoted field keeps its spaces.
//   - A delimiter ending every line leaves no column behind it; read without
//     a header row, it leaves a last, empty field.
//   - Blank lines are no rows, between the rows and after them; a line of
//     blanks is a row whose first column holds them; a last line without a
//     line end is read like any other.
func TestTheCSVFixturesDecodeIntoTheirRows(t *testing.T) {
	const jobsColumns = "job,state,duration_seconds,records"
	const citiesColumns = "città,Land,température,влажност,状態,sky"
	const oblastiColumns = "град,област,температура,влажност,състояние"
	sofia := map[string]any{"città": "София", "Land": "България", "température": "14.5", "влажност": "62", "状態": "晴れ", "sky": "☀️"}
	quebec := map[string]any{"città": "Québec, QC", "Land": "Canada", "température": "5", "влажност": "74", "状態": "雨", "sky": "🌧"}
	bansko := map[string]any{"град": "Банско, ски зона", "област": "Благоевград", "температура": "-2.5", "влажност": "91", "състояние": "сняг"}
	noHeader := boolPtr(false)
	for _, tc := range []struct {
		fixture, contentType string
		response             model.ResponseConfig
		// rows is how many rows the fixture decodes into; columns the
		// header's names, as the header writes them, or, without a header
		// row, how many fields every row has.
		rows    int
		columns string
		// want is what some of the rows hold, by their place from 0.
		want map[int]any
	}{
		{fixture: "status.csv", contentType: "text/csv", rows: 2, columns: "server,cpu,memory,status", want: map[int]any{
			0: map[string]any{"server": "web01", "cpu": "72", "memory": "61", "status": "up"},
			1: map[string]any{"server": "web02", "cpu": "31", "memory": "48", "status": "up"},
		}},
		{fixture: "tickets-rfc4180.csv", contentType: "text/csv", rows: 12, columns: "id,queue,subject,customer,priority,age_hours,replies", want: map[int]any{
			0:  map[string]any{"id": "10231", "queue": "billing", "subject": "Invoice 2026-0917 charged twice", "customer": "Acme, Inc.", "priority": "high", "age_hours": "52.5", "replies": "4"},
			2:  map[string]any{"id": "10233", "queue": "hardware", "subject": `Printer says "PC LOAD LETTER"`, "customer": "Initech", "priority": "low", "age_hours": "211.25", "replies": "7"},
			4:  map[string]any{"id": "10235", "queue": "network", "subject": "VPN drops every 30 min\n(since the firewall change)", "customer": "Initech", "priority": "high", "age_hours": "18", "replies": "3"},
			8:  map[string]any{"id": "10239", "queue": "software", "subject": "Crash on export: \"index out of range\"\nSteps:\n1. open report, 2. click \"Export\"", "customer": "Stark Industries", "priority": "high", "age_hours": "12", "replies": "2"},
			10: map[string]any{"id": "10241", "queue": "billing", "subject": `Quote for 5" tablets, 20 units`, "customer": "Initech", "priority": "low", "age_hours": "96", "replies": "1"},
			11: map[string]any{"id": "10242", "queue": "network", "subject": "", "customer": "Globex", "priority": "normal", "age_hours": "3", "replies": "0"},
		}},
		{fixture: "inventory-semicolon.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: ";"}}, rows: 12, columns: "Artikel;Lager;Bestand;Mindestbestand;Preis;Gewicht_kg;Auslastung", want: map[int]any{
			0:  map[string]any{"Artikel": "Schraube M4x20", "Lager": "Hamburg", "Bestand": "12500", "Mindestbestand": "2000", "Preis": "0,04", "Gewicht_kg": "0,003", "Auslastung": "62,5"},
			4:  map[string]any{"Artikel": "Unterlegscheibe 4,3", "Lager": "München", "Bestand": "44000", "Mindestbestand": "10000", "Preis": "0,01", "Gewicht_kg": "0,0005", "Auslastung": "88"},
			11: map[string]any{"Artikel": "Stahlträger IPE 100", "Lager": "Leipzig", "Bestand": "48", "Mindestbestand": "10", "Preis": "1.234,56", "Gewicht_kg": "48,6", "Auslastung": "4,8"},
		}},
		{fixture: "sensors.tsv", contentType: "text/tab-separated-values", response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "\t"}}, rows: 12, columns: "sensor\tlocation\ttemperature\thumidity\tbattery\tnote", want: map[int]any{
			1:  map[string]any{"sensor": "th-02", "location": "cold room 2", "temperature": "", "humidity": "68", "battery": "97", "note": ""},
			2:  map[string]any{"sensor": "th-03", "location": `  "dock, east"`, "temperature": "17.9", "humidity": "", "battery": "", "note": ""},
			3:  map[string]any{"sensor": "th-04", "location": " office ", "temperature": " 21.5", "humidity": "40 ", "battery": " 12 ", "note": "battery low"},
			5:  map[string]any{"sensor": "th-06", "location": "roof", "temperature": "-3.5", "humidity": "88", "battery": "64", "note": "north\tside"},
			6:  map[string]any{"sensor": "th-07", "location": "", "temperature": "19.0", "humidity": "45", "battery": "81", "note": ""},
			7:  map[string]any{"sensor": "th-08", "location": "basement", "temperature": "", "humidity": "", "battery": "", "note": "offline"},
			10: map[string]any{"sensor": "th-11", "location": "freezer", "temperature": "-18.4", "humidity": "52", "battery": "9", "note": " battery low "},
		}},
		{fixture: "sensors.tsv", contentType: "text/tab-separated-values", response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "\t", TrimSpace: true}}, rows: 12, columns: "sensor\tlocation\ttemperature\thumidity\tbattery\tnote", want: map[int]any{
			1:  map[string]any{"sensor": "th-02", "location": "cold room 2", "temperature": "", "humidity": "68", "battery": "97", "note": ""},
			2:  map[string]any{"sensor": "th-03", "location": "dock, east", "temperature": "17.9", "humidity": "", "battery": "", "note": ""},
			3:  map[string]any{"sensor": "th-04", "location": "office", "temperature": "21.5", "humidity": "40", "battery": "12", "note": "battery low"},
			5:  map[string]any{"sensor": "th-06", "location": "roof", "temperature": "-3.5", "humidity": "88", "battery": "64", "note": "north\tside"},
			6:  map[string]any{"sensor": "th-07", "location": "", "temperature": "19.0", "humidity": "45", "battery": "81", "note": ""},
			7:  map[string]any{"sensor": "th-08", "location": "basement", "temperature": "", "humidity": "", "battery": "", "note": "offline"},
			10: map[string]any{"sensor": "th-11", "location": "freezer", "temperature": "-18.4", "humidity": "52", "battery": "9", "note": "battery low"},
		}},
		{fixture: "queues-pipe.txt", contentType: "text/plain", response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "|"}}, rows: 13, columns: "queue|vhost|consumers|messages_ready|messages_unacked|state", want: map[int]any{
			0:  map[string]any{"queue": "orders", "vhost": "/shop", "consumers": "4", "messages_ready": "120", "messages_unacked": "3", "state": "running"},
			12: map[string]any{"queue": "(12 rows)", "vhost": "", "consumers": "", "messages_ready": "", "messages_unacked": "", "state": ""},
		}},
		{fixture: "accounts-colon.txt", contentType: "text/plain", response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: ":", Header: noHeader}}, rows: 13, columns: "7", want: map[int]any{
			0:  []any{"root", "x", "0", "0", "root", "/root", "/bin/bash"},
			8:  []any{"sshd", "x", "105", "65534", "", "/run/sshd", "/usr/sbin/nologin"},
			9:  []any{"postgres", "x", "112", "120", "PostgreSQL administrator,,,", "/var/lib/postgresql", "/bin/bash"},
			11: []any{"deploy", "x", "1001", "1001", `Deploy "bot" user`, "/srv/deploy", "/bin/sh"},
		}},
		{fixture: "readings-noheader.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{Header: noHeader}}, rows: 16, columns: "5", want: map[int]any{
			0:  []any{"2026-10-03T09:00:00Z", "th-01", "4.20", "71", "ok"},
			15: []any{"2026-10-03T09:15:00Z", "th-04", "21.83", "43", "low-battery"},
		}},
		{fixture: "cities-utf8-bom.csv", contentType: "text/csv", rows: 12, columns: citiesColumns, want: map[int]any{0: sofia, 11: quebec}},
		{fixture: "cities-utf8-bom.csv", contentType: "text/csv; charset=iso-8859-1", rows: 12, columns: citiesColumns, want: map[int]any{0: sofia, 11: quebec}},
		{fixture: "cities-utf16le-bom.csv", contentType: "text/csv", rows: 12, columns: citiesColumns, want: map[int]any{0: sofia, 11: quebec}},
		{fixture: "cities-utf16le-bom.csv", response: model.ResponseConfig{Charset: "windows-1251"}, rows: 12, columns: citiesColumns, want: map[int]any{0: sofia, 11: quebec}},
		{fixture: "cities-utf16be.csv", contentType: "text/csv; charset=utf-16be", rows: 12, columns: citiesColumns, want: map[int]any{0: sofia, 11: quebec}},
		{fixture: "cities-utf16be.csv", response: model.ResponseConfig{Charset: "utf-16be"}, rows: 12, columns: citiesColumns, want: map[int]any{0: sofia, 11: quebec}},
		{fixture: "oblasti-utf8.csv", contentType: "text/csv", rows: 10, columns: oblastiColumns, want: map[int]any{9: bansko}},
		{fixture: "oblasti-windows-1251.csv", contentType: "text/csv; charset=windows-1251", rows: 10, columns: oblastiColumns, want: map[int]any{9: bansko}},
		{fixture: "oblasti-windows-1251.csv", response: model.ResponseConfig{Charset: "windows-1251"}, rows: 10, columns: oblastiColumns, want: map[int]any{9: bansko}},
		{fixture: "oblasti-windows-1251.csv", contentType: "text/csv; charset=utf-8", response: model.ResponseConfig{Charset: "windows-1251"}, rows: 10, columns: oblastiColumns, want: map[int]any{9: bansko}},
		{fixture: "communes-iso-8859-1.csv", contentType: "text/csv; charset=ISO-8859-1", rows: 10, columns: "commune,département,température,humidité,état", want: map[int]any{
			6: map[string]any{"commune": "L'Haÿ-les-Roses", "département": "Val-de-Marne", "température": "12", "humidité": "72", "état": "nuageux"},
			9: map[string]any{"commune": "Sète, port", "département": "Hérault", "température": "22", "humidité": "61", "état": "ensoleillé"},
		}},
		{fixture: "nodes-space-aligned.txt", contentType: "text/plain", response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: " ", TrimSpace: true}}, rows: 13, columns: "NODE ROLE CPU MEM PODS AGE_DAYS STATUS NOTE", want: map[int]any{
			0: map[string]any{"NODE": "web01", "ROLE": "worker", "CPU": "72", "MEM": "61.5", "PODS": "14", "AGE_DAYS": "212", "STATUS": "Ready", "NOTE": "two words"},
			1: map[string]any{"NODE": "web02", "ROLE": "worker", "CPU": "31", "MEM": "48.0", "PODS": "9", "AGE_DAYS": "212", "STATUS": "Ready", "NOTE": "-"},
			6: map[string]any{"NODE": "batch01", "ROLE": "worker", "CPU": "0", "MEM": "4.0", "PODS": "0", "AGE_DAYS": "3", "STATUS": "NotReady", "NOTE": "kubelet stopped posting"},
		}},
		{fixture: "hosts-trailing-delimiter.csv", contentType: "text/csv", rows: 10, columns: "host,cpu_percent,memory_percent,disk_percent", want: map[int]any{
			0: map[string]any{"host": "web01", "cpu_percent": "72", "memory_percent": "61", "disk_percent": "40"},
		}},
		{fixture: "hosts-trailing-delimiter.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{Header: noHeader}}, rows: 11, columns: "5", want: map[int]any{
			0: []any{"host", "cpu_percent", "memory_percent", "disk_percent", ""},
			1: []any{"web01", "72", "61", "40", ""},
		}},
		{fixture: "jobs-short-rows.csv", contentType: "text/csv", rows: 10, columns: jobsColumns, want: map[int]any{
			0: map[string]any{"job": "backup-db", "state": "ok", "duration_seconds": "412.5", "records": "18250"},
			1: map[string]any{"job": "backup-files", "state": "ok", "duration_seconds": "1290", "records": ""},
			3: map[string]any{"job": "reindex", "state": "failed", "duration_seconds": "", "records": ""},
			7: map[string]any{"job": "send-reports", "state": "", "duration_seconds": "", "records": ""},
			9: map[string]any{"job": "renew-certs", "state": "skipped", "duration_seconds": "", "records": ""},
		}},
		{fixture: "jobs-blank-lines.csv", contentType: "text/csv", rows: 11, columns: jobsColumns, want: map[int]any{
			0:  map[string]any{"job": "backup-db", "state": "ok", "duration_seconds": "412.5", "records": "18250"},
			3:  map[string]any{"job": "reindex", "state": "failed", "duration_seconds": "7.5", "records": "0"},
			4:  map[string]any{"job": "   ", "state": "", "duration_seconds": "", "records": ""},
			5:  map[string]any{"job": "sync-ldap", "state": "ok", "duration_seconds": "12", "records": "830"},
			10: map[string]any{"job": "renew-certs", "state": "skipped", "duration_seconds": "0", "records": "0"},
		}},
		{fixture: "jobs-blank-lines.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}}, rows: 11, columns: jobsColumns, want: map[int]any{
			4: map[string]any{"job": "", "state": "", "duration_seconds": "", "records": ""},
		}},
		{fixture: "jobs-no-final-newline.csv", contentType: "text/csv", rows: 10, columns: jobsColumns, want: map[int]any{
			9: map[string]any{"job": "renew-certs", "state": "skipped", "duration_seconds": "0", "records": "0"},
		}},
		{fixture: "usage-duplicate-columns.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{Header: noHeader}}, rows: 11, columns: "5", want: map[int]any{
			0: []any{"host", "used", "free", "used", "free"},
			1: []any{"web01", "412", "612", "61", "39"},
		}},
		{fixture: "usage-unnamed-column.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{Header: noHeader}}, rows: 11, columns: "4", want: map[int]any{
			0: []any{"host", "", "used", "free"},
			7: []any{"batch01", "", "40", "984"},
		}},
		{fixture: "numbers.csv", contentType: "text/csv", rows: 55, columns: "form,value", want: map[int]any{
			11: map[string]any{"form": "blanks around", "value": "  42  "},
			12: map[string]any{"form": "quoted", "value": "42"},
			13: map[string]any{"form": "quoted with blanks", "value": " 42 "},
			26: map[string]any{"form": "thousands comma", "value": "1,234"},
			41: map[string]any{"form": "empty", "value": ""},
			42: map[string]any{"form": "blanks", "value": "   "},
			43: map[string]any{"form": "quoted empty", "value": ""},
		}},
		{fixture: "numbers.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}}, rows: 55, columns: "form,value", want: map[int]any{
			11: map[string]any{"form": "blanks around", "value": "42"},
			13: map[string]any{"form": "quoted with blanks", "value": "42"},
			42: map[string]any{"form": "blanks", "value": ""},
		}},
		{fixture: "backups-times.csv", contentType: "text/csv", rows: 12, columns: "job,started_local,finished,verified_de,next_run_us,snapshot,uploaded,expires,started_unix,finished_ms,size_bytes", want: map[int]any{
			0: map[string]any{"job": "db-main", "started_local": "2026-10-03 09:00:07", "finished": "2026-10-03T06:12:40Z", "verified_de": "03.10.2026 09:15", "next_run_us": "Oct 4, 2026 9:00 AM",
				"snapshot": "20261003T060007Z", "uploaded": "Sat, 03 Oct 2026 06:20:00 GMT", "expires": "2026-11-02", "started_unix": "1791007207", "finished_ms": "1791007960000", "size_bytes": "48318382080"},
			7: map[string]any{"job": "ci-cache", "started_local": "2026-10-03 05:00:00", "finished": "", "verified_de": "never", "next_run_us": "Oct 4, 2026 5:00 AM",
				"snapshot": "20261003T020000Z", "uploaded": "", "expires": "2026-10-04", "started_unix": "1790992800", "finished_ms": "", "size_bytes": "0"},
		}},
		{fixture: "usgs-all-hour.csv", contentType: "text/csv", response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}}, rows: 12,
			columns: "time,latitude,longitude,depth,mag,magType,nst,gap,dmin,rms,net,id,updated,place,type,horizontalError,depthError,magError,magNst,status,locationSource,magSource", want: map[int]any{
				2: map[string]any{"time": "2026-10-03T09:08:47.823Z", "latitude": "61.4375", "longitude": "-150.0462", "depth": "45.2", "mag": "1.6", "magType": "ml", "nst": "", "gap": "", "dmin": "", "rms": "0.42",
					"net": "ak", "id": "ak0269cx1f2k", "updated": "2026-10-03T09:10:55.412Z", "place": "11 km WNW of Susitna, Alaska", "type": "earthquake", "horizontalError": "", "depthError": "0.3", "magError": "",
					"magNst": "", "status": "automatic", "locationSource": "ak", "magSource": "ak"},
			}},
		{fixture: "service-status.csv", contentType: "text/csv", rows: 16, columns: "service,host,dc,env,state,restarts_total,requests_total,errors_total,latency_ms,queue_depth,version,internal_id", want: map[int]any{
			5: map[string]any{"service": "checkout", "host": "web03", "dc": "ams2", "env": "prod", "state": "down", "restarts_total": "22", "requests_total": "1804120", "errors_total": "90211", "latency_ms": "",
				"queue_depth": "340", "version": "5.2.0", "internal_id": "8c1102"},
		}},
	} {
		t.Run(strings.TrimSpace(tc.fixture+" "+tc.contentType+" "+responseSettings(tc.response)), func(t *testing.T) {
			d, err := decodeCSVFixture(t, tc.fixture, tc.contentType, tc.response)
			if err != nil {
				t.Fatal(err)
			}
			rows, ok := d.Data.([]any)
			if d.Kind != "csv" || !ok || len(rows) != tc.rows {
				t.Fatalf("decoded as %s into %d rows (%T), want %d rows of csv", d.Kind, len(rows), d.Data, tc.rows)
			}
			delimiter := tc.response.CSV.Delimiter
			if delimiter == "" {
				delimiter = ","
			}
			for i, row := range rows {
				switch row := row.(type) {
				case map[string]any:
					want := strings.Split(tc.columns, delimiter)
					slices.Sort(want)
					if got := slices.Sorted(maps.Keys(row)); !slices.Equal(got, want) {
						t.Errorf("row %d has the columns %q, want %q", i, got, want)
					}
				case []any:
					if want, err := strconv.Atoi(tc.columns); err != nil || len(row) != want {
						t.Errorf("row %d has %d fields, want %s", i, len(row), tc.columns)
					}
				default:
					t.Errorf("row %d is a %T", i, row)
				}
			}
			for i, want := range tc.want {
				if !reflect.DeepEqual(rows[i], want) {
					t.Errorf("row %d:\ngot  %q\nwant %q", i, rows[i], want)
				}
			}
		})
	}
}

// responseSettings words the response settings of a case for its name.
func responseSettings(r model.ResponseConfig) string {
	var out []string
	if r.Charset != "" {
		out = append(out, "charset "+r.Charset)
	}
	if r.CSV.Delimiter != "" {
		out = append(out, "delimiter "+strconv.Quote(r.CSV.Delimiter))
	}
	if r.CSV.Header != nil && !*r.CSV.Header {
		out = append(out, "no header")
	}
	if r.CSV.TrimSpace {
		out = append(out, "trimmed")
	}
	return strings.Join(out, ", ")
}

// A fixture whose header cannot name its columns fails the decode, in words
// that name the column and say what to change: two columns of one name, a
// column of values under an empty header cell, and columns aligned with
// spaces read without trim_space, where every space is a delimiter of its
// own and the header's second run of them leaves a column of values unnamed.
// A charset the Content-Type names and the exporter does not know fails it
// too, naming it. Read by number, without a header row, the first two decode
// (TestTheCSVFixturesDecodeIntoTheirRows).
func TestCSVFixturesWhoseHeaderCannotNameTheColumnsFailTheDecode(t *testing.T) {
	const byNumber = "; name it, or set response.csv.header: false and read the columns by number"
	for _, tc := range []struct {
		fixture, contentType string
		csv                  model.CSVConfig
		want                 string
	}{
		{"usage-duplicate-columns.csv", "text/csv", model.CSVConfig{},
			`CSV header names column "used" twice, as columns 2 and 4; rename one, or set response.csv.header: false and read the columns by number`},
		{"usage-unnamed-column.csv", "text/csv", model.CSVConfig{}, "CSV header leaves column 2 unnamed, and it holds values" + byNumber},
		{"usage-unnamed-column.csv", "text/csv", model.CSVConfig{TrimSpace: true}, "CSV header leaves column 2 unnamed, and it holds values" + byNumber},
		{"nodes-space-aligned.txt", "text/plain", model.CSVConfig{Delimiter: " "}, "CSV header leaves column 3 unnamed, and it holds values" + byNumber},
		{"oblasti-windows-1251.csv", "text/csv; charset=cp-bulgarian", model.CSVConfig{},
			`unsupported charset "cp-bulgarian"; use a name from the WHATWG Encoding Standard, such as utf-8, windows-1252, iso-8859-2, windows-1251, shift_jis or gbk`},
	} {
		d, err := decodeCSVFixture(t, tc.fixture, tc.contentType, model.ResponseConfig{CSV: tc.csv})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s with %+v:\ngot  %v (decoded %v)\nwant %s", tc.fixture, tc.csv, err, d != nil, tc.want)
		}
	}
}
