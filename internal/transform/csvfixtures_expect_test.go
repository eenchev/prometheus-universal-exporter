package transform

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// csvExpectation is one line of what docs/CONFIGURATION.md tells a reader to
// expect of CSV ("Reading CSV: what to expect"): a small body in the shape
// the line speaks of, the collector that reads it, and what comes of it —
// the decode's error for a body that fails to decode, the transform's for
// one a pre-script left in no shape the rules read, or else the series and
// each rule's failures, as csvFixtureResult writes them.
type csvExpectation struct {
	name     string
	response model.ResponseConfig
	pre      string
	body     string
	rules    []model.MetricRule
	decode   string
	failure  string
	series   []string
	failures []string
}

func (e csvExpectation) run(t *testing.T) {
	t.Helper()
	t.Run(e.name, func(t *testing.T) {
		if e.pre != "" {
			requirePython(t)
		}
		c := csvFixtureCollector(t, e.response, e.rules...)
		c.Transform.PreScript = e.pre
		if e.decode != "" {
			_, err := decode.Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(e.body), Headers: http.Header{}}, &c)
			if err == nil || !strings.Contains(err.Error(), e.decode) {
				t.Errorf("the decode gave %v, want it to fail with %q", err, e.decode)
			}
			return
		}
		result := transformCSVBody(t, c, "", []byte(e.body))
		if e.failure != "" {
			if result.err == nil || result.err.Error() != e.failure {
				t.Errorf("the transform gave %v, want it to fail with %q", result.err, e.failure)
			}
			return
		}
		result.holds(t, e.series, e.failures...)
	})
}

// The lines a file is made of. A carriage return alone ends a line as a line
// feed does, with a carriage return before it or without: a file written
// with one after each row, as a spreadsheet saves "CSV (Macintosh)", has its
// rows, a file may end some lines one way and some another, and inside a
// quoted field a carriage return is the field's text. A line of blanks is a
// row missing every value, with trim_space and without, where an empty line
// is no row at all.
func TestCSVExpectedOfTheLinesOfAFile(t *testing.T) {
	used := model.MetricRule{Name: "used", Expression: "used", Labels: columns("host", "host")}
	const blanks = "host,used\nweb01,72\n\n   \ndb1,5\n"
	for _, expectation := range []csvExpectation{
		{name: "a carriage return alone ends a line", body: "host,used\rweb01,72\rdb1,5\r", rules: []model.MetricRule{used},
			series: []string{`used{host="web01"} 72`, `used{host="db1"} 5`}},
		{name: "beside lines that end with a line feed", body: "host,used\rweb01,72\r\ndb1,5\nmail,9\r", rules: []model.MetricRule{used},
			series: []string{`used{host="web01"} 72`, `used{host="db1"} 5`, `used{host="mail"} 9`}},
		{name: "and is text inside a quoted field", body: "host,used\r\"web\r01\",72\rdb1,5\r", rules: []model.MetricRule{used},
			series: []string{"used{host=\"web\r01\"} 72", `used{host="db1"} 5`}},
		{name: "a line of blanks is a row missing every value", body: blanks, rules: []model.MetricRule{used},
			series:   []string{`used{host="web01"} 72`, `used{host="db1"} 5`},
			failures: []string{`used: 1 failed, 1 missing, logged: CSV column "used" is empty in row 2`}},
		{name: "under trim_space too", body: blanks, rules: []model.MetricRule{used},
			response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}},
			series:   []string{`used{host="web01"} 72`, `used{host="db1"} 5`},
			failures: []string{`used: 1 failed, 1 missing, logged: CSV column "used" is empty in row 2`}},
	} {
		expectation.run(t)
	}
}

// A space as the delimiter. Without trim_space every space is a delimiter of
// its own, so columns aligned with spaces fail the decode at the header, for
// the column a run of spaces leaves unnamed. With it a run of spaces is one
// delimiter, and an empty field, which no blank can stand for there, is
// written as a pair of quotes.
func TestCSVExpectedOfASpaceAsTheDelimiter(t *testing.T) {
	used := model.MetricRule{Name: "used", Expression: "USED", Labels: columns("host", "HOST", "note", "NOTE")}
	const aligned = "HOST   NOTE         USED\nweb01  \"\"           72\ndb1    \"two words\"  5\n"
	for _, expectation := range []csvExpectation{
		// HOST, two columns without a name, USED; and the 72 is in the third.
		{name: "aligned columns without trim_space fail at the header", body: "HOST   USED\nweb01  72\n", rules: []model.MetricRule{used},
			response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: " "}},
			decode:   "CSV header leaves column 3 unnamed, and it holds values"},
		{name: "an empty field is a pair of quotes", body: aligned, rules: []model.MetricRule{used},
			response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: " ", TrimSpace: true}},
			series:   []string{`used{host="web01"} 72`, `used{host="db1",note="two words"} 5`}},
		{name: "between single spaces too, without trim_space", body: "HOST NOTE USED\nweb01 \"\" 72\ndb1 \"two words\" 5\n", rules: []model.MetricRule{used},
			response: model.ResponseConfig{CSV: model.CSVConfig{Delimiter: " "}},
			series:   []string{`used{host="web01"} 72`, `used{host="db1",note="two words"} 5`}},
	} {
		expectation.run(t)
	}
}

// Read without a header row, a file's first line is a row of data: one that
// has a header line fails every rule on that line, on every scrape, with the
// column's name for the text that is no number. There is no setting that
// skips lines: a pre-script drops the row.
func TestCSVExpectedOfAHeaderLineReadWithoutAHeader(t *testing.T) {
	headerless := false
	response := model.ResponseConfig{CSV: model.CSVConfig{Header: &headerless}}
	used := model.MetricRule{Name: "used", Expression: "2", Labels: columns("host", "1")}
	const body = "host,used\nweb01,72\ndb1,5\n"
	for _, expectation := range []csvExpectation{
		{name: "the header line is a row", response: response, body: body, rules: []model.MetricRule{used},
			series:   []string{`used{host="web01"} 72`, `used{host="db1"} 5`},
			failures: []string{`used: 1 failed, 0 missing, logged: value "used" is not a number; map text to numbers with value_map`}},
		{name: "which a pre-script drops", response: response, body: body, rules: []model.MetricRule{used},
			pre:    "data = data[1:]\n",
			series: []string{`used{host="web01"} 72`, `used{host="db1"} 5`}},
	} {
		expectation.run(t)
	}
}

// A column the response does not have and a cell that is empty are told
// apart. The first names the columns the response does have, and has causes
// that are no fault of the rule's, each of which shows in those names: a
// delimiter that is not the file's, a header with blanks after its
// delimiters read without trim_space, a name in another case, a sep= line
// before the header. The second names the row, counted from 1 without the
// header's line. Both are the rule's missing value, which required: false
// silences. A label that names a column the response does not have fails its
// rule, once and whether the rule is required or not, where an empty cell
// leaves the label off its series. A sep= line read with the delimiter it
// names fails the decode instead, the header's second column being unnamed.
func TestCSVExpectedOfAColumnThatIsMissing(t *testing.T) {
	optional := false
	semicolons := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: ";"}}
	used := model.MetricRule{Name: "used", Expression: "used", Labels: columns("host", "host")}
	absent := func(count int, columns string) []string {
		return []string{fmt.Sprintf(`used: %d failed, %d missing, logged: CSV column "used" is not in the response, %s; column names are matched exactly`, count, count, columns)}
	}
	for _, expectation := range []csvExpectation{
		{name: "an empty cell and an absent column are told apart", body: "host,used\nweb01,\ndb1,5\n",
			rules:  []model.MetricRule{used, {Name: "free", Expression: "free", Labels: columns("host", "host")}},
			series: []string{`used{host="db1"} 5`},
			failures: []string{
				`used: 1 failed, 1 missing, logged: CSV column "used" is empty in row 1`,
				`free: 2 failed, 2 missing, logged: CSV column "free" is not in the response, whose columns are "host", "used"; column names are matched exactly`,
			}},
		{name: "required: false says nothing of either", body: "host,used\nweb01,\ndb1,5\n",
			rules: []model.MetricRule{
				{Name: "used", Expression: "used", Required: &optional, Labels: columns("host", "host")},
				{Name: "free", Expression: "free", Required: &optional, Labels: columns("host", "host")},
			},
			series: []string{`used{host="db1"} 5`}},
		{name: "a label of an absent column fails its rule, once", body: "host,zone,used\nweb01,,72\ndb1,eu,5\n",
			rules: []model.MetricRule{
				{Name: "used", Expression: "used", Labels: columns("host", "host", "zone", "zone")},
				{Name: "used_by_rack", Expression: "used", Required: &optional, Labels: columns("host", "host", "rack", "Rack")},
			},
			series:   []string{`used{host="web01"} 72`, `used{host="db1",zone="eu"} 5`},
			failures: []string{`used_by_rack: 1 failed, 0 missing, logged: metric "used_by_rack" label "rack": CSV column "Rack" is not in the response, whose columns are "host", "used", "zone"; column names are matched exactly`}},
		{name: "a delimiter that is not the file's", body: "host;used\nweb01;72\n", rules: []model.MetricRule{used},
			failures: []string{`used: 1 failed, 1 missing, logged: CSV column "used" is not in the response, whose only column is "host;used"; column names are matched exactly`}},
		{name: "a header with blanks after its commas", body: "host, used\nweb01, 72\n", rules: []model.MetricRule{used}, failures: absent(1, `whose columns are " used", "host"`)},
		{name: "which trim_space reads", body: "host, used\nweb01, 72\n", rules: []model.MetricRule{used},
			response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: true}},
			series:   []string{`used{host="web01"} 72`}},
		{name: "a name in another case", body: "Host,Used\nweb01,72\n", rules: []model.MetricRule{used}, failures: absent(1, `whose columns are "Used", "Host"`)},
		{name: "a sep= line before the header", body: "sep=;\nhost;used\nweb01;72\n", rules: []model.MetricRule{used}, failures: absent(2, `whose only column is "sep=;"`)},
		{name: "a sep= line read with its delimiter", response: semicolons, body: "sep=;\nhost;used\nweb01;72\n", rules: []model.MetricRule{used},
			decode: "CSV header leaves column 2 unnamed, and it holds values"},
	} {
		expectation.run(t)
	}
}

// csvLeftByScript is what the csv transform says of a pre-script that left
// in data something other than a list of rows: what it left, and what the
// transform reads.
func csvLeftByScript(left string) string {
	return "python pre-script of a csv transform left data as " + left + "; it must leave a list of rows, each row a dict by column name or a list by column number"
}

// What a pre-script of a csv transform is given and must leave: a list of
// rows, each a dict by the header's names, or without a header row a list of
// the row's fields. It may drop rows, change cells and add columns; a row it
// leaves as a list is read by number. Anything but a list in data — a dict,
// None, a string — fails the transform, saying what the script left, and so
// does a row that is neither a dict nor a list, by its number.
func TestCSVExpectedOfAPreScript(t *testing.T) {
	headerless := false
	const body = "host,used\nweb01,72\ndb1,5\n"
	byName := model.MetricRule{Name: "used", Expression: "used", Labels: columns("host", "host")}
	byNumber := model.MetricRule{Name: "used", Expression: "2", Labels: columns("host", "1")}
	for _, expectation := range []csvExpectation{
		{name: "with a header the rows are dicts", body: body, rules: []model.MetricRule{byName},
			pre:    "assert isinstance(data, list) and all(isinstance(row, dict) for row in data), data\nassert data[0] == {'host': 'web01', 'used': '72'}, data\n",
			series: []string{`used{host="web01"} 72`, `used{host="db1"} 5`}},
		{name: "without one they are lists", body: "web01,72\ndb1,5\n", rules: []model.MetricRule{byNumber},
			response: model.ResponseConfig{CSV: model.CSVConfig{Header: &headerless}},
			pre:      "assert isinstance(data, list) and all(isinstance(row, list) for row in data), data\nassert data[0] == ['web01', '72'], data\n",
			series:   []string{`used{host="web01"} 72`, `used{host="db1"} 5`}},
		{name: "a column the script adds is read", body: body,
			rules:  []model.MetricRule{{Name: "free", Expression: "free", Labels: columns("host", "host")}},
			pre:    "for row in data:\n    row['free'] = 100 - int(row['used'])\n",
			series: []string{`free{host="web01"} 28`, `free{host="db1"} 95`}},
		{name: "a row left as a list is read by number", body: body, rules: []model.MetricRule{byNumber},
			pre:    "data = [[row['host'], row['used']] for row in data]\n",
			series: []string{`used{host="web01"} 72`, `used{host="db1"} 5`}},
		{name: "a row that is neither fails the transform", body: body, rules: []model.MetricRule{byName},
			pre:     "data = [data[0], 'db1', 5]\n",
			failure: `python pre-script of a csv transform left row 2 as "db1"; a row must be a dict by column name or a list by column number`},
		{name: "a dict is no list of rows", body: body, rules: []model.MetricRule{byName}, pre: "data = {'rows': data}\n", failure: csvLeftByScript("an object with 1 key")},
		{name: "nor is None", body: body, rules: []model.MetricRule{byName}, pre: "data = None\n", failure: csvLeftByScript("null")},
		{name: "nor a string", body: body, rules: []model.MetricRule{byName}, pre: "data = 'web01,72'\n", failure: csvLeftByScript(`"web01,72"`)},
		{name: "nor a number", body: body, rules: []model.MetricRule{byName}, pre: "data = len(data)\n", failure: csvLeftByScript("2")},
	} {
		expectation.run(t)
	}
}

// Quoting is no setting: with a tab as the delimiter too, a field that
// starts with a quote is a quoted field, which runs to its closing quote
// over the tabs inside it, and one never closed fails the decode. A quote
// later in a field is text, and a text that starts with a quote is written
// as a quoted field with its quotes doubled.
func TestCSVExpectedOfAQuoteInTabSeparatedValues(t *testing.T) {
	tabs := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "\t"}}
	used := model.MetricRule{Name: "used", Expression: "used", Labels: columns("disk", "disk")}
	for _, expectation := range []csvExpectation{
		{name: "a field that starts with a quote is a quoted field", response: tabs, rules: []model.MetricRule{used},
			body:   "disk\tused\n\"rack\t7\"\t72\nsda\t5\n",
			series: []string{"used{disk=\"rack\t7\"} 72", `used{disk="sda"} 5`}},
		{name: "and left open fails the decode", response: tabs, rules: []model.MetricRule{used},
			body:   "disk\tused\n\"big disk\t72\nsda\t5\n",
			decode: `extraneous or missing " in quoted-field`},
		{name: "a quote later in a field is text", response: tabs, rules: []model.MetricRule{used},
			body:   "disk\tused\n5\" disk\t72\nsda\t5\n",
			series: []string{`used{disk="5\" disk"} 72`, `used{disk="sda"} 5`}},
		{name: "a text that starts with one is written in quotes, its own doubled", response: tabs, rules: []model.MetricRule{used},
			body:   "disk\tused\n\"\"\"big\"\" disk\"\t72\n",
			series: []string{`used{disk="\"big\" disk"} 72`}},
	} {
		expectation.run(t)
	}
}
