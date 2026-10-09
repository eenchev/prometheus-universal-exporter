package transform

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// csvColumnsBeforeRows and transformCSVBeforeRows are csvColumns and
// transformCSV as they were before the csv decoder gave its rows as
// decode.CSVRows, when it gave a list of maps of every column the header
// names, or of lists without one: what the transform makes of the decoder's
// rows now is held against what it made of those
// (TestCSVTransformOfTheDecodersRowsGivesWhatItGave).
type csvColumnsBeforeRows struct {
	rows []any
	// has is whether the response has a column, for each one asked after;
	// absent the error of each that it does not have.
	has    map[string]bool
	absent map[string]csvAbsentColumn
	// shown is what the response's columns are, as an error says it, made
	// when one first does, of names, the names of the rows read by name, in
	// order, and numbered, what the rows read by number have; listed says
	// those two were gathered.
	shown    string
	names    []string
	numbered string
	listed   bool
}

// inResponse reports whether any row has the column.
func (k *csvColumnsBeforeRows) inResponse(column string) bool {
	if has, asked := k.has[column]; asked {
		return has
	}
	// A row without a header row has its columns by number, from 1, written
	// as the transform names them (csvRow).
	number := 0
	if n, err := strconv.Atoi(column); err == nil && n > 0 && strconv.Itoa(n) == column {
		number = n
	}
	has := false
	for _, raw := range k.rows {
		switch row := raw.(type) {
		case map[string]any:
			_, has = row[column]
		case []any:
			has = number > 0 && number <= len(row)
		}
		if has {
			break
		}
	}
	if k.has == nil {
		k.has = map[string]bool{}
	}
	k.has[column] = has
	return has
}

// notInResponse is the error of a column no row has, which names the columns
// the rows do have: one error for each column, however many rows ask.
func (k *csvColumnsBeforeRows) notInResponse(column string) csvAbsentColumn {
	if absent, made := k.absent[column]; made {
		return absent
	}
	err := fmt.Errorf("CSV column %q is not in the response, %s", column, k.describe(column))
	absent := csvAbsentColumn{err: err, missing: model.MarkError(err, model.ErrMissingValue)}
	if k.absent == nil {
		k.absent = map[string]csvAbsentColumn{}
	}
	k.absent[column] = absent
	return absent
}

// describe says which columns the rows have, to a rule that asked for
// column: the names of rows read by name, the first csvColumnsShown of them,
// and how many columns the longest of the rows read by number has. The
// names are in order, but for those that are column except for the case of
// their letters or the blanks around them, which come first: the list is
// cut, and in a wide table the name that says what is wrong — used, to a
// rule that names Used — would be among those left out. The rows are gone
// through once, and the text of a column that has no such name is made
// once.
func (k *csvColumnsBeforeRows) describe(column string) string {
	if !k.listed {
		named, longest := map[string]struct{}{}, 0
		for _, raw := range k.rows {
			switch row := raw.(type) {
			case map[string]any:
				for name := range row {
					if _, seen := named[name]; !seen {
						named[name] = struct{}{}
						k.names = append(k.names, name)
					}
				}
			case []any:
				longest = max(longest, len(row))
			}
		}
		slices.Sort(k.names)
		switch {
		case longest == 1:
			k.numbered = "longest row has 1 column, read by number as 1"
		case longest > 1:
			k.numbered = fmt.Sprintf("longest row has %d columns, read by number from 1 to %d", longest, longest)
		}
		k.listed = true
	}
	for _, name := range k.names {
		if csvNearColumn(name, column) {
			return csvColumnsText(k.names, k.numbered, column)
		}
	}
	if k.shown == "" {
		k.shown = csvColumnsText(k.names, k.numbered, column)
	}
	return k.shown
}

func transformCSVBeforeRows(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	rows, ok := data.([]any)
	if !ok {
		return nil, csvNotRows(c, data)
	}
	out := &model.MetricSet{}
	// A response without a row has no value for any rule, as a regex that
	// matched no text has none, and a jq items expression that selected
	// nothing: a required rule is missing its value, and its error mode
	// decides, rather than the scrape passing with nothing.
	if len(rows) == 0 {
		for _, rule := range rules {
			if !requiredRule(rule, c) {
				continue
			}
			missing := model.MarkError(fmt.Errorf("CSV column %q is missing: the response has no rows", rule.Expression), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return nil, ruleFailure(c, rule, missing)
		}
	}
	// Room for a series of every row for each rule whose column the response
	// has, which the first row tells of rows read by header name.
	//
	// The rows are read one after another, each by every rule, so what fails
	// first, what is logged and where the series limit stops are as the rows'
	// order makes them. The series are kept rule by rule, though, each rule's
	// together in the order of the rows: the exposition formats want a
	// metric's series together, and a set that has them apart costs the
	// writer a second pass on every probe (appendMetricSet in
	// internal/exporter). So with several rules each rule with a column has a
	// part of the room, as long as the rows are many, one part after another
	// in the rules' order: next[r] is where rule r's next series goes and
	// ends[r] where its part ends. The parts are closed up afterwards over
	// what the rows left empty. Without room for the parts within the series
	// limit there are none, and a rule without a part, whose column only a
	// later row has, as a pre-script may leave them, adds its series after
	// the parts; both leave the series in the order of the rows, as one rule
	// has them anyway.
	var next, ends []int
	parts := 0
	if len(rows) > 0 {
		columns := len(rules)
		first, named := rows[0].(map[string]any)
		if named {
			columns = 0
			for i := range rules {
				if _, exists := first[rules[i].Expression]; exists {
					columns++
				}
			}
		}
		if room := seriesRoom(ctx); len(rules) > 1 && columns > 0 && (room < 0 || room >= len(rows)*columns) {
			parts = len(rows) * columns
			out.Metrics = make([]model.Metric, parts)
			next, ends = make([]int, len(rules)), make([]int, len(rules))
			at := 0
			for i := range rules {
				next[i] = at
				if _, exists := first[rules[i].Expression]; exists || !named {
					at += len(rows)
				}
				ends[i] = at
			}
		} else {
			out.Metrics = growSeries(ctx, out.Metrics, len(rows)*columns)
		}
	}
	responseColumns := csvColumnsBeforeRows{rows: rows}
	// unreadable is the rules that have failed for a label whose column the
	// response does not have, which fail once and make no series.
	var unreadable []bool
	for at, raw := range rows {
		if ctx.Err() != nil {
			return nil, interruptedAt(ctx, "")
		}
		row := csvRow(raw)
		if row == nil {
			if _, named := raw.(map[string]any); !named {
				return nil, csvNotARow(c, at+1, raw)
			}
		}
		for r, rule := range rules {
			if unreadable != nil && unreadable[r] {
				continue
			}
			value, exists := row[rule.Expression]
			if !exists || blankValue(value) {
				if requiredRule(rule, c) {
					// A column no row has, or a cell that is empty in this
					// row, the header's line not counted among the rows.
					var missing error
					if !exists && !responseColumns.inResponse(rule.Expression) {
						missing = responseColumns.notInResponse(rule.Expression).missing
					} else {
						missing = model.MarkError(model.Errorf("CSV column %q is empty in row %d", rule.Expression, model.Position(at+1)), model.ErrMissingValue)
					}
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := ruleValue(rule, value)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			labels := make(map[string]string, len(rule.Labels))
			var labelErr error
			for _, label := range rule.Labels {
				if label.Static() {
					labels[label.Name] = label.Value
				} else if labelValue, exists := row[label.Expression]; exists && labelValue != nil {
					// A pre-script may leave numbers and None in a row:
					// a number is written as the other transforms write
					// one, and None leaves the label out.
					text, err := labelText(labelValue)
					if err != nil {
						labelErr = fmt.Errorf("metric %q label %q %w", rule.Name, label.Name, err)
						break
					}
					labels[label.Name] = text
				} else if !exists && !responseColumns.inResponse(label.Expression) {
					// A column no row has is the rule's failure, once for
					// the response, as a label that cannot be read is in
					// the xpath transform (unreadableXPathLabel), and not a
					// label left off every series without a word. A cell
					// that is empty leaves the label off.
					labelErr = fmt.Errorf("metric %q label %q: %w", rule.Name, label.Name, responseColumns.notInResponse(label.Expression).err)
					if unreadable == nil {
						unreadable = make([]bool, len(rules))
					}
					unreadable[r] = true
					break
				}
			}
			if labelErr != nil {
				if handleMetricError(ctx, c, rule, labelErr) {
					continue
				}
				return nil, ruleFailure(c, rule, labelErr)
			}
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			if err := takeSeries(ctx); err != nil {
				return nil, err
			}
			series := model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels}
			if parts > 0 && next[r] < ends[r] {
				out.Metrics[next[r]] = series
				next[r]++
				continue
			}
			out.Metrics = append(out.Metrics, series)
		}
	}
	if parts > 0 {
		// Each part's series moved down to where the part before it ended,
		// then the series added after the parts, and what is left over
		// cleared, so that it keeps no labels alive.
		filled, start := 0, 0
		for r := range next {
			if filled != start {
				copy(out.Metrics[filled:], out.Metrics[start:next[r]])
			}
			filled += next[r] - start
			start = ends[r]
		}
		if filled < parts {
			filled += copy(out.Metrics[filled:], out.Metrics[parts:])
			clear(out.Metrics[filled:])
			out.Metrics = out.Metrics[:filled]
		}
	}
	return noSeriesIsNil(out), nil
}

// csvBodies is CSV bodies to decode, each with the settings to decode it
// by: every fixture of testdata/csv and every CSV the examples read, under
// every setting of response.csv, and generated bodies of every shape the
// decoder reads — headers of named, unnamed, duplicate and padded columns,
// rows shorter than the header, as long and longer, empty, blank and quoted
// cells, blank lines, three kinds of line end — under random settings.
func csvBodies(t *testing.T, generated int) (bodies [][]byte, settings []model.CSVConfig) {
	t.Helper()
	files, err := filepath.Glob("../../testdata/csv/*")
	if err != nil {
		t.Fatal(err)
	}
	examples, err := filepath.Glob("../../examples/*.csv")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range append(files, examples...) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, header := range []*bool{nil, new(bool)} {
			for _, trim := range []bool{false, true} {
				for _, delimiter := range []string{"", ";", "\t", " ", ":", "|"} {
					bodies = append(bodies, body)
					settings = append(settings, model.CSVConfig{Header: header, TrimSpace: trim, Delimiter: delimiter})
				}
			}
		}
	}
	random := rand.New(rand.NewSource(125))
	pick := func(of ...string) string { return of[random.Intn(len(of))] }
	for range generated {
		var lines []string
		width := 1 + random.Intn(5)
		header := random.Intn(3) > 0
		if header {
			heads := make([]string, width)
			for i := range heads {
				heads[i] = pick("a", "b", "c", "host", "used", "", " b ", `"x,y"`, "B", "1", "2")
			}
			lines = append(lines, strings.Join(heads, ","))
		}
		for n := random.Intn(7); n > 0; n-- {
			if random.Intn(10) == 0 {
				lines = append(lines, pick("", " "))
				continue
			}
			cells := make([]string, random.Intn(width+2))
			for i := range cells {
				cells[i] = pick("1", "2.5", "", "", " ", "x", " 7 ", `"a,b"`, "\"l1\nl2\"", `"q""q"`, `5" disk`, `" 3 "`, "-1")
			}
			lines = append(lines, strings.Join(cells, ","))
		}
		end := pick("\n", "\r\n", "\r")
		cfg := model.CSVConfig{TrimSpace: random.Intn(2) == 0}
		if !header {
			cfg.Header = new(bool)
		}
		bodies = append(bodies, []byte(strings.Join(lines, end)+end))
		settings = append(settings, cfg)
	}
	return bodies, settings
}

// decodeCSVRows decodes body as the csv decoder does under cfg, and gives
// what it decoded and the same rows as the decoder gave them before
// (decode.CSVRows.Document); nil for a body it refuses.
func decodeCSVRows(body []byte, cfg model.CSVConfig) (now, was *decode.Decoded, r *fetch.HTTPResponse) {
	c := model.Collector{Name: "rows", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: cfg}}
	r = &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{"Content-Type": {"text/csv"}}, Target: "http://rows.example/"}
	now, err := decode.Decode(r, &c)
	if err != nil {
		return nil, nil, nil
	}
	document := now.Data
	if rows, ok := now.Data.(*decode.CSVRows); ok {
		document = rows.Document()
	}
	return now, &decode.Decoded{Kind: now.Kind, Data: document, Raw: now.Raw}, r
}

// csvRulesReading is one to four generated rules of a csv transform that
// read columns, of names and of names no row has, by name and by number,
// with labels of columns, required and not, and value maps, under every
// error mode.
func csvRulesReading(random *rand.Rand, names []string) []model.MetricRule {
	columns := append(slices.Clone(names), "nope", "Host", "1", "2", "3", "01", "")
	pick := func(of []string) string { return of[random.Intn(len(of))] }
	optional := false
	var rules []model.MetricRule
	for n := 1 + random.Intn(4); n > 0; n-- {
		rule := model.MetricRule{Name: pick([]string{"m1", "m2", "m3"}), Type: model.GaugeMetricType, Expression: pick(columns), ErrorMode: pick([]string{model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail})}
		rule.Labels = []model.LabelRule{{Name: "l", Expression: pick(columns), Required: random.Intn(4) == 0}, {Name: "rule", Value: strconv.Itoa(len(rules))}}
		if random.Intn(3) == 0 {
			rule.Required = &optional
		}
		if random.Intn(4) == 0 {
			rule.ValueMap = map[string]float64{"x": -1, "q\"q": 2}
		}
		rules = append(rules, rule)
	}
	return rules
}

// What a csv transform makes of the rows the csv decoder gives is what it
// made of them when each was a map of every column the header names, or a
// list without a header: the same series in the same order, the same error,
// the same failures of the same rules reported in the same order, and the
// same lines logged, word for word — a column no row has named as before,
// with the columns the rows do have, and a cell a short row lacks empty in
// it. So it is of every fixture of testdata/csv and every CSV the examples
// read, under every setting of response.csv, and of thousands of generated
// bodies, each read by generated rules of the columns its header names, of
// columns it does not, and of numbers, with labels, value maps, every error
// mode and a series limit below and above what the rows make. The rows a
// pre-script leaves, which are lists of dicts and lists still, are read as
// before too: by name, by number, with numbers, None and lists in them,
// keys only some rows have, rows that are no row and data that is no list.
func TestCSVTransformOfTheDecodersRowsGivesWhatItGave(t *testing.T) {
	random := rand.New(rand.NewSource(34))
	counted := map[string]int{}
	compare := func(what string, now, was any, c model.Collector) {
		t.Helper()
		gotRun := runCSVTransform(t, transformCSV, now, c)
		wantRun := runCSVTransform(t, transformCSVBeforeRows, was, c)
		if !reflect.DeepEqual(gotRun, wantRun) {
			t.Fatalf("%s with %+v and the series limit %d:\n%+v\nwas\n%+v", what, c.Metrics, c.Limits.MaxMetrics, gotRun, wantRun)
		}
		switch {
		case gotRun.err != "":
			counted["failed"]++
		case len(gotRun.failures) > 0:
			counted["with failures"]++
		default:
			counted["clean"]++
		}
		for _, line := range append(gotRun.failures, gotRun.err) {
			if strings.Contains(line, "is not in the response") {
				counted["a column not in the response"]++
			}
			if strings.Contains(line, "is empty in row") {
				counted["an empty cell"]++
			}
		}
	}
	bodies, settings := csvBodies(t, alloctest.UnlessRaced(2500, 250))
	for i, body := range bodies {
		now, was, _ := decodeCSVRows(body, settings[i])
		if now == nil {
			continue
		}
		var names []string
		if rows, ok := now.Data.(*decode.CSVRows); ok {
			for _, column := range rows.Columns() {
				names = append(names, column.Name)
			}
		}
		for range 2 {
			c := model.Collector{Name: "rows", Transform: model.TransformConfig{Type: "csv"}, Metrics: csvRulesReading(random, names)}
			if random.Intn(3) == 0 {
				c.Limits.MaxMetrics = 1 + random.Intn(12)
			}
			compare(fmt.Sprintf("%q under %+v", body, settings[i]), now.Data, was.Data, c)
			counted["decoded rows"]++
		}
	}
	pick := func(of ...string) string { return of[random.Intn(len(of))] }
	for range alloctest.UnlessRaced(2000, 200) {
		var rows []any
		for n := random.Intn(6); n > 0; n-- {
			switch pick("named", "numbered", "scripted", "no row") {
			case "named":
				row := map[string]any{}
				for _, name := range []string{"a", "b", "c"} {
					if random.Intn(4) > 0 {
						row[name] = pick("1", "2.5", "", " ", "x")
					}
				}
				rows = append(rows, row)
			case "numbered":
				row := make([]any, random.Intn(4))
				for i := range row {
					row[i] = pick("1", "", "x", "7")
				}
				rows = append(rows, row)
			case "scripted":
				rows = append(rows, map[string]any{"a": random.Intn(9), "b": nil, "c": []any{1}})
			default:
				rows = append(rows, pick("db1", "7"))
			}
		}
		c := model.Collector{Name: "script", Transform: model.TransformConfig{Type: "csv"}, Metrics: csvRulesReading(random, []string{"a", "b", "c"})}
		if random.Intn(2) == 0 {
			c.Transform.PreScript = "pass"
		}
		var data any = rows
		if random.Intn(20) == 0 {
			data = map[string]any{"a": "1"}
		}
		compare(fmt.Sprintf("%#v", data), data, data, c)
		counted["rows a pre-script left"]++
	}
	t.Logf("%v", counted)
	for _, outcome := range []string{"failed", "with failures", "clean", "a column not in the response", "an empty cell"} {
		if counted[outcome] < counted["decoded rows"]/50 {
			t.Errorf("%d transforms were %s, fewer than %d: the rows and rules do not cover it", counted[outcome], outcome, counted["decoded rows"]/50)
		}
	}
}

// What a script is handed of the rows the csv decoder gives is what it was
// handed when each was a map of every column the header names: the request
// line to the worker is the same, byte for byte, its data a list of dicts
// by the header's names, keys in the order of their bytes, the cells a row
// is short of "", or of lists without a header — written by the encoder,
// and by json.Marshal where the encoder hands a request over to it — for
// every fixture, every CSV the examples read and thousands of generated
// bodies, under every setting.
func TestAScriptIsHandedTheCSVRowsItWasHanded(t *testing.T) {
	bodies, settings := csvBodies(t, alloctest.UnlessRaced(2500, 250))
	decoded := 0
	for i, body := range bodies {
		now, was, r := decodeCSVRows(body, settings[i])
		if now == nil {
			continue
		}
		decoded++
		c := &model.Collector{Name: "rows"}
		line, err := pythonRequest("data", "pass", now, r, c)
		if err != nil {
			t.Fatal(err)
		}
		line = bytes.Clone(line)
		want, err := pythonRequest("data", "pass", was, r, c)
		if err != nil {
			t.Fatal(err)
		}
		marshalled, err := pythonRequestMarshalled("data", "pass", now, r, c)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(line, want) || !bytes.Equal(marshalled, want) {
			t.Fatalf("%q under %+v: the request is\n%s\nand marshalled\n%s\nwas\n%s", body, settings[i], line, marshalled, want)
		}
	}
	if decoded < len(bodies)/3 {
		t.Errorf("%d of %d bodies decoded: the bodies do not cover the rows", decoded, len(bodies))
	}
}

// A pre-script and a python transform that echo the rows the csv decoder
// gave, json.dumps of data, see what they saw when each row was a map of
// every column the header names: the same text, its keys in the same order,
// for every fixture of testdata/csv read with a header and without.
func TestAScriptEchoesTheCSVRowsItEchoed(t *testing.T) {
	requirePython(t)
	files, err := filepath.Glob("../../testdata/csv/*.csv")
	if err != nil {
		t.Fatal(err)
	}
	echoed := 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, header := range []*bool{nil, new(bool)} {
			now, was, r := decodeCSVRows(body, model.CSVConfig{Header: header})
			if now == nil {
				continue
			}
			for _, transform := range []model.TransformConfig{
				{Type: "csv", PreScript: "import json\ndata = [{'n': len(data), 'echo': json.dumps(data)}]\n"},
				{Type: "python", Script: "import json\nmetric(name='rows', value=len(data), labels={'echo': json.dumps(data)})\n"},
			} {
				c := model.Collector{Name: "echo", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: header}}, Transform: transform,
					Metrics: []model.MetricRule{{Name: "rows", Type: model.GaugeMetricType, Expression: "n", Labels: []model.LabelRule{{Name: "echo", Expression: "echo"}}}},
					Limits:  model.Limits{ScriptTimeout: model.Duration(10 * time.Second), MaxOutputBytes: 8 << 20}}
				echo := func(d *decode.Decoded) string {
					set, err := Transform(t.Context(), d, r, &c, "python3")
					if err != nil {
						return "error " + err.Error()
					}
					var lines []string
					for _, m := range set.Metrics {
						lines = append(lines, seriesLine(m))
					}
					return strings.Join(lines, "\n")
				}
				got, want := echo(now), echo(was)
				if got != want || !strings.Contains(got, `echo="[`) {
					t.Fatalf("%s with header %v and %s: the script echoed\n%.2000s\nand it echoed\n%.2000s", file, header == nil, transform.Type, got, want)
				}
				echoed++
			}
		}
	}
	if echoed < 2*len(files) {
		t.Errorf("%d echoes of %d fixtures", echoed, len(files))
	}
}
