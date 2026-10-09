package decode

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// decodeCSVBeforeRows is decodeCSV as it was before the rows were CSVRows:
// each row a map of every column the header names, those a short row lacks
// empty, or without a header a list of the row's fields. What the decoder
// gives now is held against it (TestCSVRowsAreTheRowsTheDecoderGaveBefore).
func decodeCSVBeforeRows(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	cfg := c.Response.CSV
	delim := ','
	if cfg.Delimiter != "" {
		rr := []rune(cfg.Delimiter)
		if len(rr) != 1 {
			return nil, errors.New("CSV delimiter must be one character")
		}
		delim = rr[0]
	}
	rows, err := readCSV(r.Body, delim, cfg.TrimSpace)
	if err != nil {
		// The line and the column the reader names are where in the body
		// it stopped, which is no part of what the failure is to the log.
		var parse *csv.ParseError
		if errors.As(err, &parse) {
			err = model.SameFailureAs(err, strings.Replace(err.Error(), parse.Error(), "parse error: "+parse.Err.Error(), 1))
		}
		return nil, fmt.Errorf("CSV decode: %w", err)
	}
	if len(rows) == 0 {
		return &Decoded{Kind: "csv", Data: []any{}, Raw: r.Body}, nil
	}
	header := true
	if cfg.Header != nil {
		header = *cfg.Header
	}
	out := []any{}
	if header {
		heads := rows[0]
		// A header naming one column twice, or leaving two unnamed, would
		// have the later column overwrite the earlier in every row, without
		// a word. An unnamed column empty in every row, as a delimiter
		// ending each line leaves, holds nothing to lose and is left out.
		column := map[string]int{}
		for i := range heads {
			if cfg.TrimSpace {
				heads[i] = strings.TrimSpace(heads[i])
			}
			if heads[i] == "" {
				if columnIsEmpty(rows[1:], i, cfg.TrimSpace) {
					continue
				}
				return nil, fmt.Errorf("CSV header leaves column %d unnamed, and it holds values; name it, or set response.csv.header: false and read the columns by number", i+1)
			}
			if first, seen := column[heads[i]]; seen {
				return nil, model.Errorf("CSV header names column %s twice, as columns %d and %d; rename one, or set response.csv.header: false and read the columns by number", model.Quoted(heads[i]), first+1, i+1)
			}
			column[heads[i]] = i
		}
		for n, row := range rows[1:] {
			// A field past the header's last column has no name to be read
			// by, and went unseen: a delimiter the header's line was split
			// by and the rows' were not, or a quote read as text, showed
			// only as values in the wrong columns. Empty fields there are
			// what a delimiter ending the line leaves.
			if len(row) > len(heads) {
				if i := len(heads) + firstValue(row[len(heads):], cfg.TrimSpace); i < len(row) {
					return nil, model.Errorf("CSV line %d has a value in column %d, which the header does not name; name the column in the header, or set response.csv.header: false and read the columns by number; if the line is split where it should not be, check response.csv.delimiter and response.csv.trim_space", model.Position(csvFieldLine(r.Body, delim, cfg.TrimSpace, n+1, i)), model.Position(i+1))
				}
			}
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
	} else {
		for _, row := range rows {
			a := make([]any, len(row))
			for i, v := range row {
				// Both sides, as with a header: the reader itself trims
				// only what leads a field.
				if cfg.TrimSpace {
					v = strings.TrimSpace(v)
				}
				a[i] = v
			}
			out = append(out, a)
		}
	}
	return &Decoded{Kind: "csv", Data: out, Raw: r.Body}, nil
}

// decodeCSVBothWays decodes body under cfg with the decoder as it is and as
// it was, and returns what differs between the two, nothing when they agree:
// the same error, recognised by the same text, or rows that are the rows
// the decoder gave before (CSVRows.Document) and that json.Marshal writes as
// it wrote those. outcome says what the decode did, for the test to count.
func decodeCSVBothWays(body []byte, cfg model.CSVConfig) (diff, outcome string) {
	c := &model.Collector{Name: "rows", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: cfg}}
	was, wasErr := decodeCSVBeforeRows(&fetch.HTTPResponse{Body: bytes.Clone(body), Headers: http.Header{}}, c)
	now, err := decodeCSV(&fetch.HTTPResponse{Body: bytes.Clone(body), Headers: http.Header{}}, c)
	if (err == nil) != (wasErr == nil) || err != nil && (err.Error() != wasErr.Error() || model.SameFailureText(err) != model.SameFailureText(wasErr)) {
		return fmt.Sprintf("error %v, was %v", err, wasErr), ""
	}
	if err != nil {
		return "", "refused"
	}
	if now.Kind != was.Kind || !bytes.Equal(now.Raw, was.Raw) {
		return fmt.Sprintf("kind %s, was %s", now.Kind, was.Kind), ""
	}
	document := now.Data
	outcome = "no rows"
	if rows, ok := now.Data.(*CSVRows); ok {
		document = rows.Document()
		outcome = "read by number"
		if rows.Named() {
			outcome = "read by name"
			for i := range rows.Len() {
				if len(rows.Row(i)) < len(rows.column) {
					outcome = "read by name, with a short row"
				}
			}
		}
	}
	if !reflect.DeepEqual(document, was.Data) {
		return fmt.Sprintf("rows %#v, were %#v", document, was.Data), ""
	}
	nowJSON, nowErr := json.Marshal(now.Data)
	wasJSON, wasErr := json.Marshal(was.Data)
	if nowErr != nil || wasErr != nil || !bytes.Equal(nowJSON, wasJSON) {
		return fmt.Sprintf("written %s (%v), was %s (%v)", nowJSON, nowErr, wasJSON, wasErr), ""
	}
	return "", outcome
}

// csvSettingsEvery is every setting of response.csv the decoder reads a
// body by: a header row or none, trim_space or not, and the delimiters the
// fixtures are written with.
func csvSettingsEvery() []model.CSVConfig {
	var settings []model.CSVConfig
	for _, header := range []*bool{nil, boolPtr(false)} {
		for _, trim := range []bool{false, true} {
			for _, delimiter := range []string{"", ";", "\t", " ", ":", "|"} {
				settings = append(settings, model.CSVConfig{Header: header, TrimSpace: trim, Delimiter: delimiter})
			}
		}
	}
	return settings
}

// The rows the csv decoder gives are the rows it gave when each was a map
// of every column the header names: the same rows, the same cells, an empty
// one for each column a short row lacks, the same lists without a header,
// and the same errors, by their text and by the text the failure log
// recognises them by. So they are of every fixture of testdata/csv and every
// CSV the examples read, under every setting of response.csv, and of tens
// of thousands of generated bodies: headers with named, unnamed, duplicate
// and padded columns, rows shorter than the header, as long and longer,
// with values and with only empty fields past its end, empty, blank and
// quoted cells, quotes with delimiters, line ends and doubled quotes in
// them, bare quotes, blank lines, line ends of LF, CRLF and CR alone, and a
// last line without one, read with a header and without, with trim_space
// and without, by four delimiters. json.Marshal writes them as it wrote
// them, which is what a script was handed when the encoder of its request
// did not write the data itself.
func TestCSVRowsAreTheRowsTheDecoderGaveBefore(t *testing.T) {
	counted := map[string]int{}
	var files []string
	fixtures, err := filepath.Glob("../../testdata/csv/*")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, fixtures...)
	examples, err := filepath.Glob("../../examples/*.csv")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, examples...)
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, cfg := range csvSettingsEvery() {
			diff, outcome := decodeCSVBothWays(body, cfg)
			if diff != "" {
				t.Fatalf("%s under %+v: %s", file, cfg, diff)
			}
			counted["fixture "+outcome]++
		}
	}
	random := rand.New(rand.NewSource(52))
	pick := func(of ...string) string { return of[random.Intn(len(of))] }
	bodies := alloctest.UnlessRaced(30000, 3000)
	for range bodies {
		delimiter := pick(",", ",", ";", "\t", " ")
		var lines []string
		width := random.Intn(6)
		if random.Intn(2) == 0 {
			heads := make([]string, width)
			for i := range heads {
				heads[i] = pick("a", "b", "c", "host", "used", "", "a", " b ", `"x,y"`, `"q""q"`, "B")
			}
			lines = append(lines, strings.Join(heads, delimiter))
		}
		for n := random.Intn(7); n > 0; n-- {
			if random.Intn(8) == 0 {
				lines = append(lines, pick("", " "))
				continue
			}
			cells := make([]string, random.Intn(width+3))
			for i := range cells {
				cells[i] = pick("1", "2.5", "", "", " ", "x", " 7 ", "\t", `"a,b"`, "\"l1\nl2\"", "\"r\rs\"", `"q""q"`, `5" disk`, `" 3 "`, `""`)
			}
			lines = append(lines, strings.Join(cells, delimiter))
		}
		end := pick("\n", "\n", "\r\n", "\r")
		body := strings.Join(lines, end)
		if random.Intn(3) > 0 {
			body += end
		}
		cfg := model.CSVConfig{TrimSpace: random.Intn(2) == 0}
		if delimiter != "," {
			cfg.Delimiter = delimiter
		}
		switch random.Intn(3) {
		case 0:
			cfg.Header = boolPtr(false)
		case 1:
			cfg.Header = boolPtr(true)
		}
		diff, outcome := decodeCSVBothWays([]byte(body), cfg)
		if diff != "" {
			t.Fatalf("%q under %+v: %s", body, cfg, diff)
		}
		counted[outcome]++
	}
	t.Logf("%v", counted)
	// Each kind of outcome among the generated bodies, a twentieth of them
	// at least: refusals, rows by name with a short row and without, rows by
	// number and none.
	for _, outcome := range []string{"refused", "read by name", "read by name, with a short row", "read by number", "no rows"} {
		if counted[outcome] < bodies/20 {
			t.Errorf("%d generated bodies were %s, fewer than %d: the bodies do not cover it", counted[outcome], outcome, bodies/20)
		}
	}
	if counted["fixture read by name, with a short row"] == 0 || counted["fixture refused"] == 0 {
		t.Errorf("the fixtures cover %v, without a short row read by name or without a refusal", counted)
	}
}

// What a row read by name has, by the CSVRows the decoder gives: every
// column the header names, the cell "" where the row is short of it, and
// no column it does not name, also the one an unnamed empty column has; a
// row read by number has the columns from "1" to its length, written as a
// number is written. Any row has a column when one has it.
func TestCSVRowsHaveTheColumnsOfTheirHeader(t *testing.T) {
	d, err := decodeCSVBody([]byte("host,used,,free\nweb01,72\ndb1,5,,9\n"))
	if err != nil {
		t.Fatal(err)
	}
	rows := d.Data.(*CSVRows)
	for _, tc := range []struct {
		row          int
		column, want string
		has          bool
	}{
		{0, "host", "web01", true}, {0, "used", "72", true}, {0, "free", "", true},
		{1, "free", "9", true}, {1, "", "", false}, {1, "3", "", false}, {1, "nope", "", false},
	} {
		if got, has := rows.Cell(tc.row, tc.column); got != tc.want || has != tc.has {
			t.Errorf("row %d column %q is %q, %v; want %q, %v", tc.row, tc.column, got, has, tc.want, tc.has)
		}
	}
	if !rows.Has("free") || rows.Has("") || rows.Has("1") {
		t.Errorf("the rows have free %v, \"\" %v, 1 %v; want true, false, false", rows.Has("free"), rows.Has(""), rows.Has("1"))
	}
	c := &model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(false)}}}
	d, err = Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte("a,b,c\nd\n"), Headers: http.Header{}}, c)
	if err != nil {
		t.Fatal(err)
	}
	rows = d.Data.(*CSVRows)
	if got, has := rows.Cell(0, "3"); got != "c" || !has {
		t.Errorf("row 1 column 3 is %q, %v", got, has)
	}
	if _, has := rows.Cell(1, "2"); has || !rows.Has("3") || rows.Has("4") || rows.Has("03") || rows.Has("+1") {
		t.Errorf("the rows by number have the columns %v of row 2, 3 %v, 4 %v, 03 %v, +1 %v", has, rows.Has("3"), rows.Has("4"), rows.Has("03"), rows.Has("+1"))
	}
}

// A header of k named columns over k rows of one field each is decoded at
// a cost of the cells the body holds, not k times k of them: a row is not
// given an empty cell for every column it is short of. At k = 2,000, a body
// of 15 kB, the decode allocates under 1.5 MB, a hundred bytes for each of
// the body's (it is 0.7 MB), where it allocated 649 MB, and a 39 kB body of
// 5,000 columns took the exporter to 2 GB; at 4,000 columns, a body of 31
// kB, the bound is twice as large, where the decode allocated 2.6 GB. This
// happens before the transform, so limits.max_metrics never stood in the
// way of it.
func TestCSVShortRowsUnderAWideHeaderCostTheCellsTheyHold(t *testing.T) {
	for _, k := range []int{2000, 4000} {
		body := csvWideHeaderShortRows(k)
		most := uint64(100 * len(body))
		var decodeErr error
		allocated := alloctest.BytesAtMost(1, most, func() { _, decodeErr = decodeCSVBody(body) })
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if allocated > most {
			t.Errorf("a header of %d columns over %d short rows, %d bytes, allocated %d bytes to decode, more than %d", k, k, len(body), allocated, most)
		}
	}
}

// The rows of a CSV of one short column hold a small multiple of the body:
// of a body of 2 MiB, a header and lines of "1", the decoded rows, a
// collection later, hold under 32 times its size (about 23, the reader's
// own slice of fields and string for each line and its place in the list
// of rows: some 48 bytes for a line of two), where a map for each row held
// 184 times its size, 1.9 GB for a body of 10 MiB that is 80 MB as a JSON
// array.
func TestCSVRowsOfOneColumnHoldASmallMultipleOfTheBody(t *testing.T) {
	body := csvOneColumn(alloctest.UnlessRaced(2<<20, 256<<10))
	// The least of three, since the heap of a test holds what other code
	// left on it, each run's rows let go of before the next.
	held, rows := ^uint64(0), 0
	for range 3 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		d, err := decodeCSVBody(body)
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(d)
		grown := uint64(0)
		if after.HeapAlloc > before.HeapAlloc {
			grown = after.HeapAlloc - before.HeapAlloc
		}
		held, rows = min(held, grown), len(csvDocument(d.Data).([]any))
	}
	t.Logf("the rows of a body of %d bytes hold %d bytes", len(body), held)
	if most := uint64(32 * len(body)); held > most {
		t.Errorf("the rows of a body of %d bytes hold %d bytes, more than %d", len(body), held, most)
	}
	if rows != len(body)/2-1 {
		t.Errorf("%d rows, want %d", rows, len(body)/2-1)
	}
}
