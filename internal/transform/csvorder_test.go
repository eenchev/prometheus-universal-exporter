package transform

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// formerTransformCSV is transformCSV as it was, adding every series as its
// row and its rule came, row by row: what the csv transform is held against
// (csvTransformsAsBefore).
func formerTransformCSV(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	rows, ok := data.([]any)
	if !ok {
		return nil, errors.New("CSV transform requires a header-based CSV response")
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
	if len(rows) > 0 {
		columns := len(rules)
		if first, named := rows[0].(map[string]any); named {
			columns = 0
			for i := range rules {
				if _, exists := first[rules[i].Expression]; exists {
					columns++
				}
			}
		}
		out.Metrics = growSeries(ctx, out.Metrics, len(rows)*columns)
	}
	for _, raw := range rows {
		if ctx.Err() != nil {
			return nil, interruptedAt(ctx, "")
		}
		row := csvRow(raw)
		for _, rule := range rules {
			value, exists := row[rule.Expression]
			if !exists || blankValue(value) {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("CSV column %q is missing", rule.Expression), model.ErrMissingValue)
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
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
		}
	}
	return noSeriesIsNil(out), nil
}

// csvTransformRun is what one run of a csv transform gave: its series, each
// as its sample line with the place of the rule that made it, the error, the
// rules' failures in the order they are reported, and what was logged.
type csvTransformRun struct {
	series   []string
	err      string
	failures []string
	logged   []string
}

// runCSVTransform runs a csv transform over decoded rows as Transform runs
// it: with the collector's series limit, and its rules' failures gathered
// and then reported and logged. Each rule's description is its place among
// the rules, which every series it makes then carries as its help.
func runCSVTransform(t *testing.T, transform func(context.Context, any, []model.MetricRule, *model.Collector) (*model.MetricSet, error), data any, c model.Collector) csvTransformRun {
	t.Helper()
	logs := testutil.CaptureLogs(t)
	c.Metrics = slices.Clone(c.Metrics)
	for i := range c.Metrics {
		c.Metrics[i].Description = strconv.Itoa(i)
	}
	ctx, report := WithRuleReport(t.Context())
	ctx = withSeriesBudget(ctx, c.Limits.MaxMetrics)
	ctx, failures := withRuleFailures(ctx)
	set, err := transform(ctx, data, c.Metrics, &c)
	failures.finish(&c, report)
	var run csvTransformRun
	if err != nil {
		run.err = err.Error()
		var failure *MetricFailure
		if errors.As(err, &failure) {
			run.err = "metric " + failure.Metric + " of " + failure.Collector + ": " + run.err
		}
	}
	if set != nil {
		if set.Metrics == nil {
			run.series = append(run.series, "no series")
		}
		for _, m := range set.Metrics {
			run.series = append(run.series, m.Help+" "+string(m.Type)+" "+seriesLine(m))
		}
	}
	for _, f := range report.Failures() {
		run.failures = append(run.failures, fmt.Sprintf("%s: %d failed, %d missing, logged %v: %v", f.Metric, f.Failures, f.Missing, f.Logged, f.First))
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		// Each line but for its time, which is its first field.
		if _, rest, found := strings.Cut(line, `,"level":`); found {
			run.logged = append(run.logged, rest)
		}
	}
	return run
}

// csvMissingNow is what the csv transform says of a value it cannot find:
// that the column is empty in a row, or that no row has it, with the columns
// the rows do have. It said `CSV column "x" is missing` of both.
var csvMissingNow = regexp.MustCompile(`(CSV column \\?"(?:[^"\\]|\\.)*?\\?") (?:is empty in row \d+|is not in the response, (?:which has no columns|whose longest row has \d+ columns?, read by number (?:as 1|from 1 to \d+)|whose (?:only column is|columns are) .*?; column names are matched exactly))`)

// asFormerCSVMessages is lines of a run with what the transform says of a
// missing value now put as it said it.
func asFormerCSVMessages(lines ...string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = csvMissingNow.ReplaceAllString(line, "$1 is missing")
	}
	return out
}

// csvLabelColumnsAbsent is, for each rule, whether a label of it names a
// column no row of rows has: with any row at all, the rule then fails for
// that label once it has a value to give, where it left the label off.
func csvLabelColumnsAbsent(data any, rules []model.MetricRule) (absent []bool, some bool) {
	rows, _ := data.([]any)
	absent = make([]bool, len(rules))
	for i, rule := range rules {
		for _, label := range rule.Labels {
			if label.Static() || len(rows) == 0 {
				continue
			}
			if !slices.ContainsFunc(rows, func(row any) bool { _, has := csvRow(row)[label.Expression]; return has }) {
				absent[i], some = true, true
			}
		}
	}
	return absent, some
}

// csvTransformsAsBefore holds the csv transform of decoded rows against the
// transform as it was: the same error, the same failures of the same rules
// reported in the same order, the same lines logged, and the same series of
// every rule in the same order, the order of the rows. The former transform
// has the series row by row; ruleByRule says this one has them rule by rule,
// each rule's together, and reordered that the two orders differ.
//
// Two things are not as they were, and are held by csvcolumns_test.go. What
// is said of a missing value is compared as it was said (csvMissingNow). And
// a rule with a label whose column no row has fails, once, and makes no
// series: such a rule is held against the former transform reading, in its
// place, a rule that makes nothing and reports nothing, which leaves the
// error and the series of the other rules to compare, and nothing at all
// where such a rule is under fail, since it stops the transform.
// csvDocument is data as the csv decoder once gave its rows, a list of maps
// or of lists (decode.CSVRows.Document), and other data as it is.
func csvDocument(data any) any {
	if rows, ok := data.(*decode.CSVRows); ok {
		return rows.Document()
	}
	return data
}

func csvTransformsAsBefore(t *testing.T, data any, c model.Collector) (ruleByRule, reordered bool, run csvTransformRun) {
	t.Helper()
	former := c
	// The former transform read the rows as the decoder made them then.
	document := csvDocument(data)
	absent, labelFails := csvLabelColumnsAbsent(document, c.Metrics)
	if labelFails {
		optional := false
		former.Metrics = slices.Clone(c.Metrics)
		for i := range former.Metrics {
			if absent[i] {
				former.Metrics[i].Expression, former.Metrics[i].Required = "\x00 a column no row has", &optional
			}
		}
	}
	was := runCSVTransform(t, formerTransformCSV, document, former)
	now := runCSVTransform(t, transformCSV, data, c)
	// Each series starts with the place of its rule: a stable sort by it
	// has each rule's series together, in the order they were in.
	byRule := func(series []string) []string {
		sorted := slices.Clone(series)
		slices.SortStableFunc(sorted, func(a, b string) int {
			ruleA, _, _ := strings.Cut(a, " ")
			ruleB, _, _ := strings.Cut(b, " ")
			placeA, _ := strconv.Atoi(ruleA)
			placeB, _ := strconv.Atoi(ruleB)
			return placeA - placeB
		})
		return sorted
	}
	for i, rule := range c.Metrics {
		if absent[i] && rule.ErrorMode == model.ErrorModeFail {
			return slices.Equal(now.series, byRule(now.series)), false, now
		}
	}
	reported := labelFails || slices.Equal(asFormerCSVMessages(now.failures...), was.failures) && slices.Equal(asFormerCSVMessages(now.logged...), was.logged)
	if asFormerCSVMessages(now.err)[0] != was.err || !reported {
		t.Fatalf("%v with %+v:\nerror    %s\nfailures %q\nlogged   %q\nand they were\nerror    %s\nfailures %q\nlogged   %q", data, c.Metrics, now.err, now.failures, now.logged, was.err, was.failures, was.logged)
	}
	want := byRule(was.series)
	if !slices.Equal(byRule(now.series), want) {
		t.Fatalf("%v with %+v:\nseries\n%s\nwant every rule's as they were\n%s", data, c.Metrics, strings.Join(now.series, "\n"), strings.Join(was.series, "\n"))
	}
	return slices.Equal(now.series, want), !slices.Equal(now.series, was.series), now
}

// A csv collector's series are kept rule by rule: all the rows' series of
// the first rule, in the order of the rows, then those of the second, as
// the other transforms keep theirs and the text format wants them. They
// were kept row by row, a series of each rule for the first row and then
// for the second, which the text format has no place for: a metric's lines
// are one group there. A row a rule makes nothing of leaves no gap, and the
// failures are the same ones.
func TestCSVSeriesAreKeptRuleByRule(t *testing.T) {
	optional := false
	c := csvFixtureCollector(t, model.ResponseConfig{},
		model.MetricRule{Name: "host_cpu", Expression: "cpu", Labels: columns("host", "host")},
		model.MetricRule{Name: "host_mem", Expression: "mem", Required: &optional, Labels: columns("host", "host")},
		model.MetricRule{Name: "host_disk", Expression: "disk", Labels: columns("host", "host")},
	)
	result := transformCSVBody(t, c, "text/csv", []byte("host,cpu,mem,disk\nweb01,1,2,3\nweb02,4,,6\nweb03,x,8,9\nweb04,10,11,\n"))
	if result.err != nil {
		t.Fatal(result.err)
	}
	want := []string{
		`host_cpu{host="web01"} 1`, `host_cpu{host="web02"} 4`, `host_cpu{host="web04"} 10`,
		`host_mem{host="web01"} 2`, `host_mem{host="web03"} 8`, `host_mem{host="web04"} 11`,
		`host_disk{host="web01"} 3`, `host_disk{host="web02"} 6`, `host_disk{host="web03"} 9`,
	}
	if !slices.Equal(result.inOrder, want) {
		t.Errorf("series:\n%s\nwant\n%s", strings.Join(result.inOrder, "\n"), strings.Join(want, "\n"))
	}
	result.holds(t, want,
		`host_cpu: 1 failed, 0 missing, logged: value "x" is not a number; map text to numbers with value_map`,
		`host_disk: 1 failed, 1 missing, logged: CSV column "disk" is empty in row 4`,
	)
}

// Whatever the rows and the rules are, the csv transform gives what it gave
// when it kept its series row by row, but for their order: of some three
// thousand generated tables — rows with a header's columns or, without a
// header row, of their own lengths, cells that hold numbers, text, blanks
// and nothing, and rows as a pre-script leaves them, with numbers, None and
// columns only some rows have — read by one to four rules of every error
// mode, required and not, with value maps, labels of columns and required
// labels, two rules of one name, and a series limit below, at and above
// what the table makes, every one fails as it failed, under fail on the
// same cell of the same rule and at the series limit with the same count,
// reports the same failures in the same order, logs the same lines, and has
// the same series of every rule, in the order of the rows. What it says of a
// missing value apart, and but for the rules with a label whose column no
// row has, which fail where they left the label off: more than a thousand
// of the tables have one (csvTransformsAsBefore).
//
// The series are rule by rule, each rule's together, whenever the series
// limit has room for a series of every row for every rule and the rows all
// have the columns of the first, as decoded rows do. The rest, a table at
// its series limit and rows a pre-script left with columns of their own, may
// have them in the order of the rows, which the writer of an answer then
// puts together (appendMetricSet in internal/exporter).
func TestCSVTransformGivesWhatItGaveButForTheOrder(t *testing.T) {
	random := rand.New(rand.NewSource(8))
	pick := func(of ...string) string { return of[random.Intn(len(of))] }
	optional := false
	counted := map[string]int{}
	for table := 0; table < 3200; table++ {
		names := []string{"a", "b", "c"}
		kind := pick("named", "named", "numbered", "scripted")
		var rows []any
		for n := random.Intn(7); n > 0; n-- {
			switch kind {
			case "numbered":
				row := make([]any, random.Intn(4))
				for i := range row {
					row[i] = pick("1", "2.5", "", " ", "x", "7")
				}
				rows = append(rows, row)
			case "scripted":
				row := map[string]any{}
				for _, name := range names {
					switch pick("number", "number", "text", "none", "absent", "list") {
					case "number":
						row[name] = random.Intn(9)
					case "text":
						row[name] = pick("1", "x", "", "0.5")
					case "none":
						row[name] = nil
					case "list":
						row[name] = []any{1, 2}
					}
				}
				rows = append(rows, row)
			default:
				row := map[string]any{}
				for _, name := range names {
					row[name] = pick("1", "2.5", "", " ", "x", "7", "3")
				}
				rows = append(rows, row)
			}
		}
		var rules []model.MetricRule
		for n := 1 + random.Intn(4); n > 0; n-- {
			rule := model.MetricRule{Name: pick("m1", "m2", "m3", "m4"), Type: model.GaugeMetricType, ErrorMode: pick(model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail)}
			rule.Expression = pick("a", "b", "c", "d")
			label := model.LabelRule{Name: "l", Expression: pick("a", "b", "c", "d"), Required: random.Intn(4) == 0}
			if kind == "numbered" {
				rule.Expression = pick("1", "2", "3", "4")
				label.Expression = pick("1", "2", "3", "4")
			}
			rule.Labels = []model.LabelRule{label, {Name: "rule", Value: strconv.Itoa(len(rules))}}
			if random.Intn(3) == 0 {
				rule.Required = &optional
			}
			if random.Intn(4) == 0 {
				rule.ValueMap = map[string]float64{"x": -1}
			}
			rules = append(rules, rule)
		}
		c := model.Collector{Name: "generated", Transform: model.TransformConfig{Type: "csv"}, Metrics: rules}
		// No limit, or one around the number of series the table can make.
		if random.Intn(2) == 0 {
			c.Limits.MaxMetrics = 1 + random.Intn(len(rows)*len(rules)+2)
		}
		ruleByRule, reordered, run := csvTransformsAsBefore(t, rows, c)
		counted["tables"]++
		counted[kind]++
		if _, absent := csvLabelColumnsAbsent(rows, rules); absent {
			counted["with a label of a column no row has"]++
		} else {
			counted["without a label of a column no row has"]++
		}
		if reordered {
			counted["reordered"]++
		}
		uniform := true
		for _, row := range rows {
			named, isNamed := row.(map[string]any)
			uniform = uniform && (!isNamed || len(named) == len(rows[0].(map[string]any)))
			for name := range named {
				_, inFirst := rows[0].(map[string]any)[name]
				uniform = uniform && inFirst
			}
		}
		if room := c.Limits.MaxMetrics == 0 || c.Limits.MaxMetrics >= len(rows)*len(rules); uniform && room {
			counted["rule by rule"]++
			if !ruleByRule {
				t.Fatalf("%v with %+v and the series limit %d: the series are not rule by rule:\n%s", rows, rules, c.Limits.MaxMetrics, strings.Join(run.series, "\n"))
			}
		}
		switch {
		case strings.Contains(run.err, "exceeds limit"):
			counted["past the series limit"]++
		case run.err != "":
			counted["failed"]++
		case len(run.series) > 1:
			counted["answered"]++
		}
		if len(run.failures) > 1 {
			counted["failures of several rules"]++
		}
		if len(run.logged) > 0 {
			counted["logged"]++
		}
	}
	for what, least := range map[string]int{
		"named": 1000, "numbered": 400, "scripted": 400, "with a label of a column no row has": 1000, "without a label of a column no row has": 1000, "reordered": 200, "rule by rule": 1000, "past the series limit": 100, "failed": 300, "answered": 500, "failures of several rules": 300, "logged": 1000,
	} {
		if counted[what] < least {
			t.Errorf("%d of the %d tables are %s, want %d at least: the tables do not cover it", counted[what], counted["tables"], what, least)
		}
	}
}

// The parts of the csv transform's room that the order of its series rests
// on, each on its own: a rule whose column no row has takes no room and
// makes no series; a rule whose column only later rows have, as a pre-script
// may leave them, has its series after the others'; rows more than the
// series limit has room for are read as they were, row by row, and fail at
// the limit or, when enough of them hold nothing, are answered; and without
// rows there are no series.
func TestCSVSeriesOfRulesWithoutRoomOfTheirOwn(t *testing.T) {
	optional := false
	rule := func(name, column string) model.MetricRule {
		return model.MetricRule{Name: name, Expression: column, Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog, Required: &optional, Labels: columns("row", "row")}
	}
	row := func(n int, cells ...string) any {
		out := map[string]any{"row": strconv.Itoa(n)}
		for i := 0; i < len(cells); i += 2 {
			out[cells[i]] = cells[i+1]
		}
		return out
	}
	for name, tc := range map[string]struct {
		rows  []any
		rules []model.MetricRule
		limit int
		want  []string
		err   string
	}{
		"a column no row has": {
			rows:  []any{row(1, "a", "1", "c", "3"), row(2, "a", "4", "c", "6")},
			rules: []model.MetricRule{rule("a", "a"), rule("b", "b"), rule("c", "c")},
			want:  []string{`a{row="1"} 1`, `a{row="2"} 4`, `c{row="1"} 3`, `c{row="2"} 6`},
		},
		"a column only later rows have": {
			rows:  []any{row(1, "a", "1", "c", "3"), row(2, "a", "4", "b", "5", "c", "6"), row(3, "b", "8", "c", "9")},
			rules: []model.MetricRule{rule("a", "a"), rule("b", "b"), rule("c", "c")},
			want:  []string{`a{row="1"} 1`, `a{row="2"} 4`, `c{row="1"} 3`, `c{row="2"} 6`, `c{row="3"} 9`, `b{row="2"} 5`, `b{row="3"} 8`},
		},
		"no column of the first row's": {
			rows:  []any{row(1), row(2, "a", "4", "b", "5")},
			rules: []model.MetricRule{rule("a", "a"), rule("b", "b")},
			want:  []string{`a{row="2"} 4`, `b{row="2"} 5`},
		},
		"more rows than the series limit has room for, some of them empty": {
			rows:  []any{row(1, "a", "1", "b", "2"), row(2, "a", "", "b", ""), row(3, "a", "5", "b", "")},
			rules: []model.MetricRule{rule("a", "a"), rule("b", "b")}, limit: 3,
			want: []string{`a{row="1"} 1`, `b{row="1"} 2`, `a{row="3"} 5`},
		},
		"more series than the series limit": {
			rows:  []any{row(1, "a", "1", "b", "2"), row(2, "a", "3", "b", "4")},
			rules: []model.MetricRule{rule("a", "a"), rule("b", "b")}, limit: 3,
			err: "metric count 4 exceeds limit 3",
		},
		"as many series as the series limit": {
			rows:  []any{row(1, "a", "1", "b", "2"), row(2, "a", "3", "b", "4")},
			rules: []model.MetricRule{rule("a", "a"), rule("b", "b")}, limit: 4,
			want: []string{`a{row="1"} 1`, `a{row="2"} 3`, `b{row="1"} 2`, `b{row="2"} 4`},
		},
		"no rows": {rules: []model.MetricRule{rule("a", "a"), rule("b", "b")}},
	} {
		c := model.Collector{Name: "room", Transform: model.TransformConfig{Type: "csv"}, Metrics: tc.rules, Limits: model.Limits{MaxMetrics: tc.limit}}
		_, _, run := csvTransformsAsBefore(t, tc.rows, c)
		var got []string
		for _, line := range run.series {
			// The sample line, after the rule's place and the type.
			if parts := strings.SplitN(line, " ", 3); len(parts) == 3 {
				got = append(got, parts[2])
			}
		}
		if run.err != tc.err || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, error %q; want %q, error %q", name, got, run.err, tc.want, tc.err)
		}
	}
}

// What is found in the finished set of a csv collector is found in the
// order its series are kept in, rule by rule, not in the order of the rows
// as the rules' own failures are: of two rules that each have a series twice,
// validation names the first rule's metric, though the second rule's pair is
// complete a row earlier; and of two rules that each have a label value that
// is not UTF-8, the warning's first metric is the first rule's, though the
// second rule's value is on an earlier row.
func TestCSVFinishedSetIsCheckedRuleByRule(t *testing.T) {
	c := csvFixtureCollector(t, model.ResponseConfig{},
		model.MetricRule{Name: "m_a", Expression: "a", Labels: columns("l", "la")},
		model.MetricRule{Name: "m_b", Expression: "b", Labels: columns("l", "lb")},
	)
	set, err := runBody(t, c, "text/csv", "la,lb,a,b\nx,p,1,1\ny,p,2,2\nx,q,3,3\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Validate(c.Limits); err == nil || err.Error() != `duplicate metric series "m_a"` {
		t.Errorf("validation: %v, want the first rule's duplicate series", err)
	}
	r := &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte("la,lb,a,b\nok,bad\xff,1,2\nbad\xfe,ok,3,4\n"), Headers: http.Header{"Content-Type": {"text/csv; charset=utf-8"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, report := WithRuleReport(t.Context())
	if _, err := Transform(ctx, d, r, &c, "python3"); err != nil {
		t.Fatal(err)
	}
	if repaired, first := report.UTF8Repairs(); repaired != 2 || first != "m_a" {
		t.Errorf("%d values repaired, the first of %q; want 2 and the first rule's m_a", repaired, first)
	}
}
