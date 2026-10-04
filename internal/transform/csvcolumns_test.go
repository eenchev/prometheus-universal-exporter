package transform

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// csvRowsRun is what a csv transform of rows gave, the rules' series each as
// its sample line and their failures as csvFixtureResult writes them, in the
// order they are reported.
type csvRowsRun struct {
	series, failures []string
	err              error
}

// transformCSVRows runs the csv transform over rows as Transform runs it,
// its rules gauges that log their failures unless they say otherwise.
func transformCSVRows(t *testing.T, rows any, preScript string, rules ...model.MetricRule) csvRowsRun {
	t.Helper()
	c := model.Collector{Name: "rows", Transform: model.TransformConfig{Type: "csv", PreScript: preScript}, Metrics: slices.Clone(rules)}
	for i := range c.Metrics {
		if c.Metrics[i].ErrorMode == "" {
			c.Metrics[i].ErrorMode = model.ErrorModeLog
		}
		c.Metrics[i].Type = model.GaugeMetricType
	}
	ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(t.Context()))
	ctx, failures := withRuleFailures(ctx)
	set, err := transformCSV(ctx, rows, c.Metrics, &c)
	failures.finish(&c, report)
	run := csvRowsRun{err: err}
	if set != nil {
		for _, m := range set.Metrics {
			run.series = append(run.series, seriesLine(m))
		}
	}
	for _, f := range report.Failures() {
		how := "not logged"
		if f.Logged {
			how = "logged"
		}
		run.failures = append(run.failures, fmt.Sprintf("%s: %d failed, %d missing, %s: %v", f.Metric, f.Failures, f.Missing, how, f.First))
	}
	return run
}

func (r csvRowsRun) holds(t *testing.T, series []string, failures ...string) {
	t.Helper()
	if r.err != nil || !slices.Equal(r.series, series) || !slices.Equal(r.failures, failures) {
		t.Errorf("series\n%s\nfailures\n%s\nerror %v\nwant the series\n%s\nand the failures\n%s", strings.Join(r.series, "\n"), strings.Join(r.failures, "\n"), r.err, strings.Join(series, "\n"), strings.Join(failures, "\n"))
	}
}

// A label that names a column the response does not have fails its rule,
// where it was left off every series without a word: a header in another
// case or a column that was renamed showed in nothing. The rule fails once
// for the response, however many rows it has, with the metric, the label,
// the column and the columns the response does have; it makes no series,
// and the other rules make theirs. It is a failure and no missing value, so
// a rule that is not required fails too, and a label that is required and
// one that is not fail alike. error_mode decides what follows, as for a
// label that cannot be read in the xpath transform: ignore counts it without
// a log line, and fail fails the transform with the error, as the rule's.
// A rule that finds no value in any row never reads its labels, and reports
// the value.
func TestCSVLabelOfAColumnTheResponseLacksFailsItsRule(t *testing.T) {
	optional := false
	rows := []any{
		map[string]any{"site": "ams", "host": "web01", "value": "72"},
		map[string]any{"site": "ams", "host": "web02", "value": ""},
		map[string]any{"site": "fra", "host": "db1", "value": "5"},
	}
	const absent = `metric "m" label "site": CSV column "Site" is not in the response, whose columns are "site", "host", "value"; column names are matched exactly`
	misnamed := model.MetricRule{Name: "m", Expression: "value", Labels: columns("host", "host", "site", "Site")}
	other := model.MetricRule{Name: "other", Expression: "value", Labels: columns("host", "host")}
	otherSeries := []string{`other{host="web01"} 72`, `other{host="db1"} 5`}
	otherFailure := `other: 1 failed, 1 missing, logged: CSV column "value" is empty in row 2`

	transformCSVRows(t, rows, "", misnamed, other).holds(t, otherSeries, "m: 1 failed, 0 missing, logged: "+absent, otherFailure)
	transformCSVRows(t, rows, "", other, misnamed).holds(t, otherSeries, "m: 1 failed, 0 missing, logged: "+absent, otherFailure)

	notRequired := misnamed
	notRequired.Required = &optional
	transformCSVRows(t, rows, "", notRequired, other).holds(t, otherSeries, "m: 1 failed, 0 missing, logged: "+absent, otherFailure)

	requiredLabel := misnamed
	requiredLabel.Labels = []model.LabelRule{{Name: "site", Expression: "Site", Required: true}}
	transformCSVRows(t, rows, "", requiredLabel).holds(t, nil, "m: 1 failed, 0 missing, logged: "+absent)

	ignored := misnamed
	ignored.ErrorMode = model.ErrorModeIgnore
	transformCSVRows(t, rows, "", ignored, other).holds(t, otherSeries, "m: 1 failed, 0 missing, not logged: "+absent, otherFailure)

	strict := misnamed
	strict.ErrorMode = model.ErrorModeFail
	run := transformCSVRows(t, rows, "", other, strict)
	var failure *MetricFailure
	if !errors.As(run.err, &failure) || failure.Metric != "m" || failure.Collector != "rows" || run.err.Error() != absent || errors.Is(run.err, model.ErrMissingValue) || len(run.series) != 0 {
		t.Errorf("under fail: %v with the series %q, want the transform failed for the rule m with %s", run.err, run.series, absent)
	}

	// The second label is the one that is not there: the first is read, and
	// the rule fails all the same.
	second := model.MetricRule{Name: "m", Expression: "value", Labels: columns("site", "Site", "host", "host")}
	transformCSVRows(t, rows, "", second).holds(t, nil, "m: 1 failed, 0 missing, logged: "+absent)

	// No row holds a value: the labels are never read.
	valueless := model.MetricRule{Name: "m", Expression: "Value", Labels: columns("site", "Site")}
	transformCSVRows(t, rows, "", valueless).holds(t, nil,
		`m: 3 failed, 3 missing, logged: CSV column "Value" is not in the response, whose columns are "value", "host", "site"; column names are matched exactly`)
}

// A cell that is empty in a column the response has leaves the label off its
// series, as it did: an empty text, a column a short row lacks, which the
// decoder gives it empty, a key only some of a pre-script's rows have, and
// None. So does a column number within the longest row and past the end of
// this one, read without a header row; a number past the longest row's end,
// and a name where the rows are read by number, are columns the response
// does not have, and fail the rule.
func TestCSVLabelOfAnEmptyCellIsLeftOff(t *testing.T) {
	rule := model.MetricRule{Name: "m", Expression: "value", Labels: columns("host", "host", "site", "site")}
	transformCSVRows(t, []any{
		map[string]any{"site": "ams", "host": "web01", "value": "72"},
		map[string]any{"site": "", "host": "web02", "value": "31"},
		map[string]any{"host": "web03", "value": "9"},
		map[string]any{"site": nil, "host": "db1", "value": "5"},
	}, "", rule).holds(t, []string{`m{host="web01",site="ams"} 72`, `m{host="web02"} 31`, `m{host="web03"} 9`, `m{host="db1"} 5`})

	// The first rows lack the key and a later one has it.
	transformCSVRows(t, []any{
		map[string]any{"host": "web01", "value": "72"},
		map[string]any{"host": "web02", "value": "31"},
		map[string]any{"site": "fra", "host": "db1", "value": "5"},
	}, "", rule).holds(t, []string{`m{host="web01"} 72`, `m{host="web02"} 31`, `m{host="db1",site="fra"} 5`})

	numbered := []any{[]any{"web01", "72"}, []any{"web02", "31", "ams"}, []any{"db1", "5"}}
	byNumber := func(site string) model.MetricRule {
		return model.MetricRule{Name: "m", Expression: "2", Labels: columns("host", "1", "site", site)}
	}
	transformCSVRows(t, numbered, "", byNumber("3")).holds(t, []string{`m{host="web01"} 72`, `m{host="web02",site="ams"} 31`, `m{host="db1"} 5`})
	const three = "whose longest row has 3 columns, read by number from 1 to 3"
	for _, column := range []string{"4", "0", "03", "-1", "+3", "site"} {
		rule := model.MetricRule{Name: "m", Expression: "2", Labels: []model.LabelRule{{Name: "site", Expression: column}}}
		transformCSVRows(t, numbered, "", rule).holds(t, nil, fmt.Sprintf(`m: 1 failed, 0 missing, logged: metric "m" label "site": CSV column %q is not in the response, %s`, column, three))
	}
}

// The value of a rule tells a column the response does not have from a cell
// that is empty in one it has, which one message, `CSV column "x" is
// missing`, stood for. The first names the columns the response has, the
// same error for every row; the second names the row, counted from 1 as the
// rows are, the header's line not among them, whether the cell is empty,
// holds blanks or None, or the row is one that lacks a column other rows
// have. Both are the rule's missing value still: counted as missing, handled
// by error_mode, and nothing at all for a rule that is not required.
func TestCSVValueTellsAnAbsentColumnFromAnEmptyCell(t *testing.T) {
	optional := false
	rows := []any{
		map[string]any{"host": "web01", "used": "72", "free": "28"},
		map[string]any{"host": "web02", "used": "", "free": "  "},
		map[string]any{"host": "web03", "used": nil},
		map[string]any{"host": "db1", "used": "5", "free": "95"},
	}
	// The name that is the one asked for in another case comes first.
	const has = `whose columns are "free", "host", "used"; column names are matched exactly`
	const hasUsed = `whose columns are "used", "free", "host"; column names are matched exactly`
	transformCSVRows(t, rows, "",
		model.MetricRule{Name: "used", Expression: "used"},
		model.MetricRule{Name: "free", Expression: "free"},
		model.MetricRule{Name: "total", Expression: "Used"},
		model.MetricRule{Name: "ignored", Expression: "Free", ErrorMode: model.ErrorModeIgnore},
		model.MetricRule{Name: "optional", Expression: "Used", Required: &optional},
		model.MetricRule{Name: "optional_free", Expression: "free", Required: &optional},
	).holds(t, []string{"used 72", "used 5", "free 28", "free 95", "optional_free 28", "optional_free 95"},
		`total: 4 failed, 4 missing, logged: CSV column "Used" is not in the response, `+hasUsed,
		`ignored: 4 failed, 4 missing, not logged: CSV column "Free" is not in the response, `+has,
		`used: 2 failed, 2 missing, logged: CSV column "used" is empty in row 2`,
		`free: 2 failed, 2 missing, logged: CSV column "free" is empty in row 2`,
	)

	for name, rule := range map[string]model.MetricRule{
		`CSV column "Used" is not in the response, ` + hasUsed: {Name: "m", Expression: "Used", ErrorMode: model.ErrorModeFail},
		`CSV column "free" is empty in row 2`:                  {Name: "m", Expression: "free", ErrorMode: model.ErrorModeFail},
	} {
		run := transformCSVRows(t, rows, "", rule)
		var failure *MetricFailure
		if !errors.As(run.err, &failure) || failure.Metric != "m" || run.err.Error() != name || !errors.Is(run.err, model.ErrMissingValue) {
			t.Errorf("under fail: %v, want the rule's missing value, %s", run.err, name)
		}
	}

	numbered := []any{[]any{"web01", "72"}, []any{"web02"}, []any{"db1", ""}}
	transformCSVRows(t, numbered, "",
		model.MetricRule{Name: "used", Expression: "2"},
		model.MetricRule{Name: "free", Expression: "3"},
		model.MetricRule{Name: "named", Expression: "used"},
	).holds(t, []string{"used 72"},
		`free: 3 failed, 3 missing, logged: CSV column "3" is not in the response, whose longest row has 2 columns, read by number from 1 to 2`,
		`named: 3 failed, 3 missing, logged: CSV column "used" is not in the response, whose longest row has 2 columns, read by number from 1 to 2`,
		`used: 2 failed, 2 missing, logged: CSV column "2" is empty in row 2`,
	)
}

// The columns an error lists are the response's: the names every row is
// read by, in order, the first twelve of them with the number of the rest;
// a name longer than 64 bytes cut, with its length; the one column a
// delimiter that is not the file's leaves; for rows read by number, how
// many the longest has; both, for rows of the two kinds, as a pre-script may
// leave them; and nothing for rows that hold nothing.
func TestCSVErrorListsTheColumnsTheResponseHas(t *testing.T) {
	wide := map[string]any{}
	for i := range 15 {
		wide["c"+strconv.Itoa(10+i)] = "1"
	}
	long := strings.Repeat("host;", 20)
	for name, tc := range map[string]struct {
		rows []any
		want string
	}{
		"more than twelve columns": {rows: []any{wide},
			want: `whose columns are "c10", "c11", "c12", "c13", "c14", "c15", "c16", "c17", "c18", "c19", "c20", "c21" and 3 more; column names are matched exactly`},
		"a long name": {rows: []any{map[string]any{long: "1"}},
			want: `whose only column is "` + long[:64] + `"... (100 bytes); column names are matched exactly`},
		"one column":           {rows: []any{map[string]any{"host;used": "web01;72"}}, want: `whose only column is "host;used"; column names are matched exactly`},
		"columns of some rows": {rows: []any{map[string]any{"b": "1"}, map[string]any{"a": "1", "b": "2"}}, want: `whose columns are "a", "b"; column names are matched exactly`},
		"one numbered column":  {rows: []any{[]any{"web01"}, []any{}}, want: "whose longest row has 1 column, read by number as 1"},
		"rows of both kinds": {rows: []any{map[string]any{"a": "1"}, []any{"1", "2"}},
			want: `whose only column is "a", and whose longest row has 2 columns, read by number from 1 to 2; column names are matched exactly`},
		"rows that hold nothing": {rows: []any{map[string]any{}, []any{}}, want: "which has no columns"},
	} {
		run := transformCSVRows(t, tc.rows, "", model.MetricRule{Name: "m", Expression: "absent"})
		if want := fmt.Sprintf(`m: %d failed, %d missing, logged: CSV column "absent" is not in the response, %s`, len(tc.rows), len(tc.rows), tc.want); run.err != nil || !slices.Equal(run.failures, []string{want}) {
			t.Errorf("%s: %q, %v; want %s", name, run.failures, run.err, want)
		}
	}
}

// A csv transform given something other than rows says what it was given
// and by whom, where it said `CSV transform requires a header-based CSV
// response` of every case, which blamed the response for what a pre-script
// left: a dict, None, text, a number. The csv decoder gives rows whatever
// the body holds, and a response another decoder read is refused before the
// transform; were the transform handed one all the same, it says what the
// decoded response is. A row that is neither a dict nor a list fails the
// transform too, by its number, counted from 1, and what it is: every column
// of it was missing, as if the response had none. The rows before it have
// been read; none of them is answered.
func TestCSVTransformSaysWhatItWasGivenInPlaceOfRows(t *testing.T) {
	rule := model.MetricRule{Name: "m", Expression: "value"}
	const script = "data = something_else\n"
	const leave = "; it must leave a list of rows, each row a dict by column name or a list by column number"
	const row = "; a row must be a dict by column name or a list by column number"
	for _, tc := range []struct {
		data        any
		pre, failed string
	}{
		{data: map[string]any{"rows": []any{}, "count": 2}, pre: script, failed: "python pre-script of a csv transform left data as an object with 2 keys" + leave},
		{data: nil, pre: script, failed: "python pre-script of a csv transform left data as null" + leave},
		{data: "web01,72", pre: script, failed: `python pre-script of a csv transform left data as "web01,72"` + leave},
		{data: 3, pre: script, failed: "python pre-script of a csv transform left data as 3" + leave},
		{data: true, pre: script, failed: "python pre-script of a csv transform left data as true" + leave},
		{data: map[string]any{"a": 1}, failed: "csv transform reads the rows the csv decoder gives, and the decoded response is an object with 1 key; set decoder.type to csv"},
		{data: "text", failed: `csv transform reads the rows the csv decoder gives, and the decoded response is "text"; set decoder.type to csv`},
		{data: []any{map[string]any{"value": "1"}, "web01", 72}, pre: script, failed: `python pre-script of a csv transform left row 2 as "web01"` + row},
		{data: []any{map[string]any{"value": "1"}, map[string]any{"value": "2"}, nil}, pre: script, failed: "python pre-script of a csv transform left row 3 as null" + row},
		{data: []any{7.5}, pre: script, failed: "python pre-script of a csv transform left row 1 as 7.5" + row},
		{data: []any{map[string]any{"value": "1"}, "web01"}, failed: `csv transform reads rows that are a mapping by column name or a list by column number, and row 2 of the decoded response is "web01"`},
	} {
		run := transformCSVRows(t, tc.data, tc.pre, rule)
		var failure *MetricFailure
		if run.err == nil || run.err.Error() != tc.failed || errors.As(run.err, &failure) || len(run.series) != 0 || len(run.failures) != 0 {
			t.Errorf("%v: %v, with the series %q and the failures %q; want the transform failed with %s", tc.data, run.err, run.series, run.failures, tc.failed)
		}
		// What a pre-script left is the script's failure, counted as one;
		// what a decoder gave is not.
		if scripted := tc.pre != ""; errors.Is(run.err, model.ErrScriptFailed) != scripted {
			t.Errorf("%v: the failure %v is marked as a script's: %v, want %v", tc.data, run.err, !scripted, scripted)
		}
	}
	// Rows that are dicts and lists, and none at all, are rows.
	transformCSVRows(t, []any{map[string]any{"value": "1"}, []any{"2"}, map[string]any{}, []any{}}, script, model.MetricRule{Name: "m", Expression: "value", Required: new(bool)}).holds(t, []string{"m 1"})
	transformCSVRows(t, []any{}, script, model.MetricRule{Name: "m", Expression: "value", Required: new(bool)}).holds(t, nil)
}

// Asking whether the response has a column costs a transform nothing where
// every row has the columns its rules name: the rows are gone through only
// for a column a row lacks, once for each such column, however many rows
// and rules ask, and the columns the response has are put into words once.
func TestCSVColumnsAreLookedForOnlyWhenARowLacksOne(t *testing.T) {
	var rows []any
	for i := range 200 {
		rows = append(rows, map[string]any{"host": "web" + strconv.Itoa(i), "used": "1", "free": "2"})
	}
	columns := csvColumns{rows: rows}
	if columns.has != nil || columns.absent != nil || columns.shown != "" {
		t.Fatalf("a csvColumns that was asked nothing holds %+v", columns)
	}
	if columns.inResponse("Used") || !columns.inResponse("used") || columns.inResponse("Used") {
		t.Error("the response has the column used, and no column Used")
	}
	first := columns.notInResponse("Used")
	if again := columns.notInResponse("Used"); again != first || !errors.Is(first.missing, model.ErrMissingValue) || errors.Is(first.err, model.ErrMissingValue) || first.missing.Error() != first.err.Error() {
		t.Error("the error of a column the response does not have was made twice")
	}
	if len(columns.has) != 2 || len(columns.absent) != 1 {
		t.Errorf("after two columns were asked after, %d answers and %d errors are kept", len(columns.has), len(columns.absent))
	}
	c := model.Collector{Name: "rows", Transform: model.TransformConfig{Type: "csv"}}
	rules := func(column, label string) []model.MetricRule {
		return []model.MetricRule{
			{Name: "used", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeIgnore, Expression: column, Labels: []model.LabelRule{{Name: "host", Expression: label}}},
			{Name: "free", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeIgnore, Expression: "free", Labels: []model.LabelRule{{Name: "host", Expression: "host"}}},
		}
	}
	// What is allocated is counted without the race detector, which
	// allocates of its own and now and then within the count.
	if raceDetector {
		return
	}
	cost := func(rules []model.MetricRule) float64 {
		return testing.AllocsPerRun(5, func() {
			if _, err := transformCSV(t.Context(), rows, rules, &c); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Two allocations a series, its labels and their map, and the series'
	// slice: nothing is kept of columns no row lacks.
	if present := cost(rules("used", "host")); present > float64(2*2*len(rows)+8) {
		t.Errorf("reading rows that have every column takes %v allocations", present)
	}
	// A value's column no row has: one error and what it is kept in, some
	// twenty allocations in all, not one for each row.
	if absent, present := cost(rules("Used", "host")), cost(rules("used", "host")); absent > present-float64(2*len(rows))+32 {
		t.Errorf("reading %d rows that lack a rule's column takes %v allocations, and reading rows that have it %v", len(rows), absent, present)
	}
	// A label's column no row has: the rule stops at the first row.
	if absent, present := cost(rules("used", "Host")), cost(rules("used", "host")); absent > present-float64(2*len(rows))+32 {
		t.Errorf("reading %d rows that lack a label's column takes %v allocations, and reading rows that have it %v", len(rows), absent, present)
	}
}
