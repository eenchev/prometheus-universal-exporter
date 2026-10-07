package transform

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// limits.max_metrics is of the series a scrape keeps. A rule takes room for
// a series as it makes it, so that the scrape stops at the first series too
// many, and what it then drops must hold no room: a later rule would fail
// the scrape for series that were never exported. These tests go through
// every way a rule gives series up, in every engine, and want the room
// taken when the transform ends to be that of the series it returns, and a
// scrape to pass with a limit of the most series it has at one time.
//
// The transforms held to that already where a rule fails as a whole after
// it made series — a jq program failing part of the way, labels that do
// not pair, an XPath engine panic — and wherever a single item fails, which
// takes no room at all. The prometheus decoder did not: it stops at the
// limit itself, before the transform runs, and counted every series a rule
// matches, those the rule then carries on without among them.
//
// What the limit is for stays. A rule may not make more series than the
// limit has room for, also when it would go on to fail as a whole and give
// them all up: the scrape fails at the first series past the limit, before
// the rule reaches what fails it, rather than making every series the
// response describes to find out whether the rule keeps them. So the limit
// a scrape passes with is of the most series it has made and not given up
// at one time, which is more than it keeps only where a rule made series
// and then failed as a whole.

// roomCase is a response and the rules that read it, of which some give
// series up; how many series the scrape keeps; and the most it has at one
// time, made and not given up, where that is more.
type roomCase struct {
	name        string
	decoder     string
	transform   string
	contentType string
	body        string
	rules       []model.MetricRule
	kept        int
	most        int
}

// roomCases are the ways a rule gives up series it made, or makes none of
// an item, by engine. Those of jq, xpath, css and regex have a second rule
// that keeps four series, after the one that gives some up: the rule that
// would fail the scrape if what the first dropped still held room.
func roomCases() []roomCase {
	const gauge, ignore = model.GaugeMetricType, model.ErrorModeIgnore
	numbers := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = strconv.Itoa(i + 1)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	objects := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf(`{"v":%d,"id":"i%d"}`, i+1, i+1)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	// cells are ten cells of a row, the seventh with an attribute the
	// engine panics on when a label reads it with contains().
	var cells strings.Builder
	cells.WriteString("<r>")
	for i := 1; i <= 10; i++ {
		if i == 7 {
			fmt.Fprintf(&cells, `<td x="5">%d</td>`, i)
		} else {
			fmt.Fprintf(&cells, `<td>%d</td>`, i)
		}
	}
	cells.WriteString("</r>")
	// exposition is twenty gauges, a dc label on every fourth, and seven
	// histograms of one series each.
	var exposition strings.Builder
	exposition.WriteString("# TYPE n gauge\n")
	for i := 1; i <= 20; i++ {
		dc := ""
		if i%4 == 0 {
			dc = `,dc="fra"`
		}
		fmt.Fprintf(&exposition, "n{i=\"%d\"%s} 1\n", i, dc)
	}
	for i := 1; i <= 7; i++ {
		fmt.Fprintf(&exposition, "# TYPE h%d histogram\nh%d_bucket{le=\"+Inf\"} 2\nh%d_sum 3\nh%d_count 2\n", i, i, i, i)
	}
	two := 2.0
	firstFour := model.MetricRule{Name: "b", Type: gauge, Expression: `.[:4][]`, Labels: []model.LabelRule{{Name: "i", Expression: ".[:4][]"}}}
	fourItems := model.MetricRule{Name: "b", Type: gauge, Items: `.[:4][]`, Expression: ".v", Labels: []model.LabelRule{{Name: "i", Expression: ".id"}}}
	fourCells := model.MetricRule{Name: "b", Type: gauge, Expression: "//td[position() < 5]", Labels: []model.LabelRule{{Name: "i", Expression: "string(.)"}}}
	return []roomCase{
		{"jq: the program fails after six values", "json", "jq", "application/json", numbers(10), []model.MetricRule{
			{Name: "a", Type: gauge, Expression: `.[] | if . > 6 then error("x") else . end`, ErrorMode: ignore}, firstFour}, 4, 6},
		{"jq: a label's values do not pair with ten series", "json", "jq", "application/json", numbers(10), []model.MetricRule{
			{Name: "a", Type: gauge, Expression: `.[]`, ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: ".[:6][]"}}}, firstFour}, 4, 6},
		{"jq: three of ten values are no numbers", "json", "jq", "application/json", numbers(10), []model.MetricRule{
			{Name: "a", Type: gauge, Expression: `.[] | if . % 3 == 0 then "bad" else . end`, ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: ".[]"}}}, firstFour}, 11, 0},
		{"jq items: the items fail after six", "json", "jq", "application/json", objects(10), []model.MetricRule{
			{Name: "a", Type: gauge, Items: `.[] | if .v > 6 then error("x") else . end`, Expression: ".v", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: ".id"}}}, fourItems}, 4, 6},
		{"jq items: two rules read items that fail after six", "json", "jq", "application/json", objects(10), []model.MetricRule{
			{Name: "a", Type: gauge, Items: `.[] | if .v > 6 then error("x") else . end`, Expression: ".v", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: ".id"}}},
			{Name: "a2", Type: gauge, Items: `.[] | if .v > 6 then error("x") else . end`, Expression: ".v", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: ".id"}}}, fourItems}, 4, 6},
		{"jq items: three of ten items are no numbers", "json", "jq", "application/json", objects(10), []model.MetricRule{
			{Name: "a", Type: gauge, Items: `.[]`, Expression: `if .v % 3 == 0 then "bad" else .v end`, ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: ".id"}}}, fourItems}, 11, 0},
		{"jq items: three of ten items lack a required label", "json", "jq", "application/json", objects(10), []model.MetricRule{
			{Name: "a", Type: gauge, Items: `.[]`, Expression: `.v`, ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: `if .v % 3 == 0 then null else .id end`, Required: true}}}, fourItems}, 11, 0},
		{"xpath: the engine fails at the seventh of ten nodes", "xml", "xpath", "application/xml", cells.String(), []model.MetricRule{
			{Name: "a", Type: gauge, Expression: "//td", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "kind", Expression: "contains(@x, 5)"}, {Name: "i", Expression: "string(.)"}}}, fourCells}, 4, 6},
		{"xpath: nine of ten nodes lack a required label", "xml", "xpath", "application/xml", cells.String(), []model.MetricRule{
			{Name: "a", Type: gauge, Expression: "//td", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "x", Expression: "@x", Required: true}}}, fourCells}, 5, 0},
		{"css: nine of ten items lack a required label", "html", "css", "text/html",
			"<html><body><table><tr>" + strings.Repeat("<td><b>1</b></td>", 5) + "<td><b>2</b><i>l</i></td>" + strings.Repeat("<td class=k><b>3</b></td>", 4) + "</tr></table></body></html>", []model.MetricRule{
				{Name: "a", Type: gauge, Items: "td", Expression: "b", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "x", Expression: "i", Required: true}}},
				{Name: "b", Type: gauge, Items: "td.k", Expression: "b"}}, 5, 0},
		{"regex: three of ten matches are no numbers", "text", "regex", "text/plain",
			"v=1 i=1\nv=2 i=2\nv=x i=3\nv=4 i=4\nv=5 i=5\nv=x i=6\nv=7 i=7\nv=8 i=8\nv=x i=9\nv=10 i=10\nw=1\nw=2\nw=3\nw=4\n", []model.MetricRule{
				{Name: "a", Type: gauge, Expression: `v=(\S+) i=(\d+)`, ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: "2"}}},
				{Name: "b", Type: gauge, Expression: `w=(\d+)`, Labels: []model.LabelRule{{Name: "i", Expression: "1"}}}}, 11, 0},
		{"csv: three of ten rows are no numbers", "csv", "csv", "text/csv", "v,w,i\n1,1,1\n2,2,2\nx,3,3\n4,4,4\n5,,5\nx,,6\n7,,7\n8,,8\nx,,9\n10,,10\n", []model.MetricRule{
			{Name: "a", Type: gauge, Expression: "v", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: "i"}}},
			{Name: "b", Type: gauge, Expression: "w", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: "i"}}}}, 11, 0},
		{"csv: a label's column is in no row", "csv", "csv", "text/csv", "v,w,i\n1,1,1\n2,2,2\n3,3,3\n4,4,4\n5,,5\n", []model.MetricRule{
			{Name: "a", Type: gauge, Expression: "v", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: "nope"}}},
			{Name: "b", Type: gauge, Expression: "w", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "i", Expression: "i"}}}}, 4, 0},
		{"prometheus: fifteen of twenty series lack a required label", "prometheus", "prometheus", "text/plain", exposition.String(), []model.MetricRule{
			{Name: "n", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "site", Expression: "dc", Required: true}}}}, 5, 0},
		{"prometheus: a type that seven histograms cannot take", "prometheus", "prometheus", "text/plain", exposition.String(), []model.MetricRule{
			{Expression: "^h", Type: gauge, ErrorMode: ignore},
			{Name: "n", Expression: "^n$", ErrorMode: ignore, Labels: []model.LabelRule{{Name: "site", Expression: "dc", Required: true}}}}, 5, 0},
		{"prometheus: a scale that seven histograms cannot take", "prometheus", "prometheus", "text/plain", exposition.String(), []model.MetricRule{
			{Expression: "^(h.*|n)$", Scale: &two, ErrorMode: ignore}}, 20, 0},
		{"prometheus: the type of a histogram, which twenty gauges cannot take", "prometheus", "prometheus", "text/plain", exposition.String(), []model.MetricRule{
			{Expression: ".", Type: model.HistogramMetricType, ErrorMode: ignore}}, 7, 0},
	}
}

// decodedRoom decodes the case's body for a collector of its rules with
// that limit, as a scrape does.
func (tc *roomCase) decoded(limit int) (*model.Collector, *decode.Decoded, *fetch.HTTPResponse, error) {
	c := &model.Collector{Name: "room", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: tc.rules, Limits: model.Limits{MaxMetrics: limit}}
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(tc.body), Headers: http.Header{"Content-Type": {tc.contentType}}}
	d, err := decode.Decode(r, c)
	return c, d, r, err
}

// The series a rule gives up hold no room. With a limit far above them,
// the room taken when the transform ends is that of the series it returns,
// in every engine and for every way a rule gives series up: failing as a
// whole after it made some, and failing on single items, which make none.
// And with the limit at exactly the series the scrape keeps, the scrape
// passes with them, the decode included: the prometheus decoder, which
// stops at the limit itself, refused the four scrapes whose rules carry
// on without most of the series they match. Where a rule made six series
// before it failed as a whole, the limit the scrape passes with is those
// six, which the rule after it is then given back. One series less and the
// scrape fails for the limit, as it did.
func TestSeriesARuleGivesUpHoldNoRoom(t *testing.T) {
	for _, tc := range roomCases() {
		t.Run(tc.name, func(t *testing.T) {
			const roomy = 1 << 20
			c, d, r, err := tc.decoded(roomy)
			if err != nil {
				t.Fatal(err)
			}
			ctx := withSeriesBudget(LeaveRuleLoggingToCaller(context.Background()), roomy)
			ctx, _ = withRuleFailures(ctx)
			set, _, err := transformMetrics(ctx, d, r, c, "")
			if err != nil || len(set.Metrics) != tc.kept {
				t.Fatalf("%d series, %v; want %d", len(set.Metrics), err, tc.kept)
			}
			if held := roomy - seriesRoom(ctx); held != tc.kept {
				t.Fatalf("the transform ended holding room for %d series, and returned %d", held, tc.kept)
			}
			// At the limit exactly, decoded with it.
			limit := max(tc.kept, tc.most)
			c, d, r, err = tc.decoded(limit)
			if err != nil {
				t.Fatalf("with a limit of %d, the decode: %v", limit, err)
			}
			exact, err := Transform(LeaveRuleLoggingToCaller(context.Background()), d, r, c, "")
			if err != nil {
				t.Fatalf("with a limit of %d: %v; want the %d series the scrape keeps", limit, err, tc.kept)
			}
			// The same series; a csv transform has them in another order
			// when the limit leaves no room to keep each rule's together.
			got, want := htmlSeries(exact), htmlSeries(set)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("with a limit of %d: the series %q, want those without one, %q", limit, got, want)
			}
			// One less, and the scrape fails for the limit, as it did.
			c, d, r, err = tc.decoded(limit - 1)
			if err == nil {
				_, err = Transform(LeaveRuleLoggingToCaller(context.Background()), d, r, c, "")
			}
			if want := fmt.Sprintf("metric count %d exceeds limit %d", limit, limit-1); err == nil || err.Error() != want || !errors.Is(err, model.ErrLimitExceeded) {
				t.Fatalf("with a limit of %d: %v, want %q", limit-1, err, want)
			}
		})
	}
}

// The prometheus decoder still stops at the limit wherever the transform
// is sure to make a series of what it keeps, a rule that sets a type or a
// scale and carries on among them: the gauges of a large exposition take
// the type of a gauge or of a counter and a scale, so the exposition is
// refused while it is read, without being held in memory. Where a rule
// may carry on without a series, requiring a label, the decoder keeps what
// the rule matches and the transform stops, at the first series it makes
// past the limit, with the same error.
func TestPrometheusDecoderStopsWhereARuleIsSureToKeepWhatItMatches(t *testing.T) {
	body := []byte(promSeries(manySeries))
	two := 2.0
	for name, rule := range map[string]model.MetricRule{
		"the metric's own type":  {Expression: "^n$", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog},
		"another type it takes":  {Expression: "^n$", Type: model.CounterMetricType, ErrorMode: model.ErrorModeIgnore},
		"a scale":                {Expression: "^n$", Scale: &two, ErrorMode: model.ErrorModeLog},
		"a label under fail":     {Expression: "^n$", ErrorMode: model.ErrorModeFail, Labels: []model.LabelRule{{Name: "copy", Expression: "i", Required: true}}},
		"beside a rule that may": {Name: "n", ErrorMode: model.ErrorModeLog},
	} {
		c := model.Collector{Name: "limited", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Limits: model.Limits{MaxMetrics: seriesLimit},
			Metrics: []model.MetricRule{rule, {Expression: "^n", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "copy", Expression: "j", Required: true}, {Name: "rule", Value: "second"}}}}}
		if name != "beside a rule that may" {
			c.Metrics = c.Metrics[:1]
		}
		r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
		var err error
		allocs := alloctest.AllocsAtMost(2, boundedAllocs, func() { _, err = decode.Decode(r, &c) })
		if err == nil || err.Error() != wantLimitFailed || !errors.Is(err, model.ErrLimitExceeded) {
			t.Errorf("%s: err = %v, want %q marked as a limit", name, err, wantLimitFailed)
		}
		if allocs > boundedAllocs {
			t.Errorf("%s: refusing the exposition took %.0f allocations", name, allocs)
		}
	}
	// A required label under log: every series has it, which only reading
	// each tells, so the decoder keeps them all and the transform stops.
	c := model.Collector{Name: "limited", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Limits: model.Limits{MaxMetrics: seriesLimit},
		Metrics: []model.MetricRule{{Expression: "^n$", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "copy", Expression: "j", Required: true}}}}}
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil || len(d.Data.(model.MetricSet).Metrics) != manySeries {
		t.Fatalf("a rule that requires a label: the decode kept %v, %v; want every series the rule matches", d, err)
	}
	if _, err = Transform(LeaveRuleLoggingToCaller(context.Background()), d, r, &c, ""); err == nil || err.Error() != wantLimitFailed || !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("a rule that requires a label: err = %v, want %q marked as a limit", err, wantLimitFailed)
	}
}

// roomFamilies are the families of a generated exposition, by type.
var roomFamilies = map[string]model.MetricType{
	"g0": model.GaugeMetricType, "g1": model.GaugeMetricType, "c0": model.CounterMetricType, "u0": model.UntypedMetricType,
	"h0": model.HistogramMetricType, "h1": model.HistogramMetricType, "s0": model.SummaryMetricType,
}

// roomExposition writes an exposition of some of roomFamilies, each of a
// few series, of which some have a dc label.
func roomExposition(random *rand.Rand) string {
	var b strings.Builder
	for _, name := range []string{"g0", "g1", "c0", "u0", "h0", "h1", "s0"} {
		if random.IntN(3) == 0 {
			continue
		}
		typ := roomFamilies[name]
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, typ)
		for i, n := 0, random.IntN(9); i < n; i++ {
			labels := fmt.Sprintf(`i="%d"`, i)
			if random.IntN(3) == 0 {
				labels += `,dc="fra"`
			}
			switch typ {
			case model.HistogramMetricType:
				fmt.Fprintf(&b, "%[1]s_bucket{%[2]s,le=\"1\"} 1\n%[1]s_bucket{%[2]s,le=\"+Inf\"} 2\n%[1]s_sum{%[2]s} 3\n%[1]s_count{%[2]s} 2\n", name, labels)
			case model.SummaryMetricType:
				fmt.Fprintf(&b, "%[1]s{%[2]s,quantile=\"0.5\"} 1\n%[1]s_sum{%[2]s} 3\n%[1]s_count{%[2]s} 2\n", name, labels)
			default:
				fmt.Fprintf(&b, "%s{%s} %d\n", name, labels, i)
			}
		}
	}
	return b.String()
}

// roomRules are one to three rules over roomFamilies: by name or by
// expression, with a type, a scale and labels, required or not, and every
// error mode.
func roomRules(random *rand.Rand) []model.MetricRule {
	two := 2.0
	pick := func(of ...string) string { return of[random.IntN(len(of))] }
	rules := make([]model.MetricRule, 1+random.IntN(3))
	for i := range rules {
		rule := &rules[i]
		if random.IntN(3) == 0 {
			rule.Name = pick("g0", "g1", "c0", "u0", "h0", "s0", "absent")
		} else {
			rule.Expression = pick("^g", "^[gc]", "^h", "^[hs]", "0$", ".", "^u0$", "^nothing$")
			if random.IntN(4) == 0 {
				rule.Name = "renamed"
			}
		}
		rule.ErrorMode = pick(model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail)
		if random.IntN(3) == 0 {
			rule.Type = model.MetricType(pick("gauge", "counter", "untyped", "histogram", "summary"))
		}
		if random.IntN(4) == 0 {
			rule.Scale = &two
		}
		if random.IntN(3) == 0 {
			rule.Labels = append(rule.Labels, model.LabelRule{Name: "site", Expression: "dc", Required: random.IntN(2) == 0})
		}
		if random.IntN(4) == 0 {
			rule.Labels = append(rule.Labels, model.LabelRule{Name: "rule", Value: strconv.Itoa(i)})
		}
		if random.IntN(3) == 0 {
			optional := false
			rule.Required = &optional
		}
	}
	return rules
}

// roomResult is what a scrape came to: its series, its error and what its
// rules failed on.
type roomResult struct {
	series   []model.Metric
	err      error
	failures []RuleFailure
}

// same says where two results differ, or nothing when they do not: in
// their series, the text of their error and whether it is the limit's, and
// in what each rule failed on, how often and with what first.
func (was roomResult) same(got roomResult) string {
	text := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	switch {
	case text(was.err) != text(got.err) || errors.Is(was.err, model.ErrLimitExceeded) != errors.Is(got.err, model.ErrLimitExceeded):
		return fmt.Sprintf("err = %v, was %v", got.err, was.err)
	case !reflect.DeepEqual(was.series, got.series):
		return fmt.Sprintf("%d series, were %d: %v, were %v", len(got.series), len(was.series), got.series, was.series)
	case len(was.failures) != len(got.failures):
		return fmt.Sprintf("%d rules failed, were %d", len(got.failures), len(was.failures))
	}
	for i, w := range was.failures {
		g := got.failures[i]
		if w.Metric != g.Metric || w.Expression != g.Expression || w.Failures != g.Failures || w.Missing != g.Missing || text(w.First) != text(g.First) || w.Logged != g.Logged {
			return fmt.Sprintf("rule %q %q failed %d times, first %v; was %d times, first %v", g.Metric, g.Expression, g.Failures, g.First, w.Failures, w.First)
		}
	}
	return ""
}

// A prometheus scrape is what it was but for the series a rule gives up:
// for thousands of generated expositions, rule sets and limits, the scrape
// as it is — decoded with its limit, then transformed — is compared with
// the scrape as the decoder counted before, which is here as the oracle:
// every series the decoder keeps counted, so more than the limit of them
// is the limit's error, and no more is the transform of them.
//
// Where the scrape was not refused for the limit, it is the same scrape:
// the same series, the same error to the letter, the same failures of its
// rules. So is every scrape of which no rule gives a series up, refused or
// not. Where it was refused for the limit and a rule gave series up, it is
// now what the series it keeps make it: the scrape without a limit when
// they fit, the series and the rules' failures alike, and the limit's
// error, naming one series past the limit as before, when they do not:
// that scrape may now be stopped by the transform, having counted what its
// rules failed on until then, where the decoder stopped it before any rule
// ran. A scrape that fails without a limit, for a rule under fail, fails
// with that or with the limit's error, whichever its series meet first.
func TestAPrometheusScrapeCountsTheSeriesItKeeps(t *testing.T) {
	random := rand.New(rand.NewPCG(2026, 1006))
	scrapes := alloctest.UnlessRaced(6000, 1000)
	asItWas, spared, stillRefused, gaveUp := 0, 0, 0, 0
	for range scrapes {
		body, rules, limit := roomExposition(random), roomRules(random), 1+random.IntN(24)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Content-Type": {"text/plain"}}}
		transformed := func(c *model.Collector, d *decode.Decoded) roomResult {
			ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(context.Background()))
			set, err := Transform(ctx, d, r, c, "")
			result := roomResult{err: err, failures: report.Failures()}
			if set != nil {
				result.series = set.Metrics
			}
			return result
		}
		collector := func(limit int) *model.Collector {
			return &model.Collector{Name: "room", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Metrics: rules, Limits: model.Limits{MaxMetrics: limit}}
		}
		// Without a limit: what the response and the rules come to.
		free := collector(0)
		all, err := decode.Decode(r, free)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		unlimited := transformed(free, all)
		// As the decoder counted: every series it keeps.
		c := collector(limit)
		var was roomResult
		if kept := len(all.Data.(model.MetricSet).Metrics); kept > limit {
			was = roomResult{err: model.MetricCountError(limit+1, limit)}
		} else {
			was = transformed(c, all)
		}
		// As it is.
		var got roomResult
		if d, err := decode.Decode(r, c); err != nil {
			got = roomResult{err: err}
		} else {
			got = transformed(c, d)
		}
		where := fmt.Sprintf("%.400q with %+v under a limit of %d", body, rules, limit)
		refusedBefore := errors.Is(was.err, model.ErrLimitExceeded)
		if len(unlimited.failures) > 0 {
			gaveUp++
		}
		switch {
		case !refusedBefore:
			if diff := was.same(got); diff != "" {
				t.Fatalf("%s: %s", where, diff)
			}
			asItWas++
		case unlimited.err != nil:
			// A rule under fail ends the scrape without a limit: with that
			// or with the limit's error, whichever its series meet first.
			was.failures, got.failures, unlimited.failures = nil, nil, nil
			if got.same(unlimited) != "" && got.same(was) != "" {
				t.Fatalf("%s: err = %v, want the limit's error or the scrape's without a limit, %v", where, got.err, unlimited.err)
			}
		case len(unlimited.failures) == 0:
			// No rule gave a series up: refused as it was.
			if diff := was.same(got); diff != "" {
				t.Fatalf("%s, of which no rule gives a series up: %s", where, diff)
			}
			asItWas++
		case len(unlimited.series) <= limit:
			if diff := unlimited.same(got); diff != "" {
				t.Fatalf("%s, which keeps %d series: %s", where, len(unlimited.series), diff)
			}
			spared++
		default:
			// Refused for the limit still, by the transform where it was
			// the decoder: with what its rules failed on before it stopped,
			// which a scrape the decoder refused never came to.
			was.failures, got.failures = nil, nil
			if diff := was.same(got); diff != "" {
				t.Fatalf("%s, which keeps %d series: %s", where, len(unlimited.series), diff)
			}
			stillRefused++
		}
	}
	t.Logf("%d scrapes: %d as they were, %d refused for the limit before that pass with the series they keep, %d refused still; in %d a rule gave series up", scrapes, asItWas, spared, stillRefused, gaveUp)
	if asItWas < scrapes/2 || spared < scrapes/50 || stillRefused < scrapes/200 || gaveUp < scrapes/4 {
		t.Fatalf("of %d scrapes, %d are as they were, %d pass that were refused, %d are refused still and in %d a rule gave series up: the generator should make many of each", scrapes, asItWas, spared, stillRefused, gaveUp)
	}
}
