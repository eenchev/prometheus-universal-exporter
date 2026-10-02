package transform

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// jqValuesCollector is a jq collector of the rules given, bounded to limit
// series.
func jqValuesCollector(limit int, rules ...model.MetricRule) model.Collector {
	return model.Collector{Name: "values", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Limits: model.Limits{MaxMetrics: limit}, Metrics: rules}
}

// labelled renders a set as name{value of label} value, in order.
func labelled(set *model.MetricSet, label string) string {
	if set == nil {
		return "no set"
	}
	var out []string
	for _, m := range set.Metrics {
		out = append(out, m.Name+"{"+m.Labels[label]+"} "+strconv.FormatFloat(m.Value, 'g', -1, 64))
	}
	return strings.Join(out, "|")
}

// A jq rule without items stops at the first series past limits.max_metrics,
// as one with items does and with the same error, having made the labels of
// the series it kept and no others. It collected every value its expression
// gives and a set of labels for each before the first was counted: 1.8 GiB
// for a response of 10 MiB. Refusing a response of 100,000 values must cost
// a small part of what making their series does; it cost more than half.
func TestAJQRuleWithoutItemsStopsAtTheSeriesLimit(t *testing.T) {
	body := "[" + strings.TrimSuffix(repeated(`{"v":1,"id":"a"},`, manySeries), ",") + "]"
	rule := model.MetricRule{Name: "n", Type: model.GaugeMetricType, Expression: ".[].v",
		Labels: []model.LabelRule{{Name: "id", Expression: ".[].id"}, {Name: "site", Expression: `"a"`}, {Name: "kind", Value: "fixed"}}}
	withItems := model.MetricRule{Name: "n", Type: model.GaugeMetricType, Items: ".[]", Expression: ".v",
		Labels: []model.LabelRule{{Name: "id", Expression: ".id"}, {Name: "site", Expression: `"a"`}, {Name: "kind", Value: "fixed"}}}

	c := jqValuesCollector(manySeries, rule)
	d, r := decodedBody(t, c, "application/json", body)
	var set *model.MetricSet
	var err error
	all := allocated(func() { set, err = Transform(context.Background(), d, r, &c, "") })
	if err != nil || len(set.Metrics) != manySeries || set.Metrics[manySeries-1].Labels["id"] != "a" || set.Metrics[0].Labels["kind"] != "fixed" {
		t.Fatalf("at the limit: %d series, %v", len(set.Metrics), err)
	}

	c.Limits.MaxMetrics = seriesLimit
	limited := allocated(func() { _, err = Transform(context.Background(), d, r, &c, "") })
	if err == nil || err.Error() != wantLimitFailed || !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("err = %v, want %q marked as a limit", err, wantLimitFailed)
	}
	items := jqValuesCollector(seriesLimit, withItems)
	var itemsErr error
	limitedItems := allocated(func() { _, itemsErr = Transform(context.Background(), d, r, &items, "") })
	if itemsErr == nil || itemsErr.Error() != err.Error() {
		t.Fatalf("with items the error is %v, without %v", itemsErr, err)
	}
	t.Logf("all %d, limited %d, limited with items %d", all, limited, limitedItems)
	if limited > all/8 {
		t.Fatalf("refusing the scrape allocated %d bytes, making every series %d; it should stop at the 11th series", limited, all)
	}
}

// The limit counts series, not values: values that make no series — a null
// under required: false, text a rule under ignore cannot read as a number —
// take no room, so a rule of many values and few series passes.
func TestJQValuesThatMakeNoSeriesTakeNoRoom(t *testing.T) {
	optional := false
	c := jqValuesCollector(3,
		model.MetricRule{Name: "sparse", Expression: ".[].v", Required: &optional, ErrorMode: model.ErrorModeIgnore,
			Labels: []model.LabelRule{{Name: "id", Expression: ".[].id"}}})
	set, failures, err := transformWith(context.Background(), t, c, "application/json",
		`[{"v":null,"id":"a"},{"v":1,"id":"b"},{"v":"n/a","id":"c"},{"id":"d"},{"v":2,"id":"e"},{"v":"  ","id":"f"},{"v":3,"id":"g"}]`)
	if err != nil || labelled(set, "id") != "sparse{b} 1|sparse{e} 2|sparse{g} 3" {
		t.Fatalf("series %s, %v", labelled(set, "id"), err)
	}
	// The one value that is text and no number is the rule's only failure.
	if len(failures) != 1 || failures[0].Failures != 1 || failures[0].Missing != 0 {
		t.Fatalf("failures %+v", failures)
	}
}

// Values are taken as the program gives them, so a failure of the rule as a
// whole can come after series were made: the program failing part of the
// way, or a label whose values do not pair with the series, known once both
// have ended. The rule's series are then dropped and their room given back,
// so the rules after it have the whole limit, and the label's error counts
// values and series as it did.
func TestAJQRuleThatFailsPartOfTheWayDropsItsSeries(t *testing.T) {
	const body = `[{"v":1,"who":"a"},{"v":2,"who":"b"},{"v":3},{"v":4}]`
	for name, tc := range map[string]struct {
		rule model.MetricRule
		want string
	}{
		"the program fails": {model.MetricRule{Name: "broken", Expression: `.[] | if .v == 3 then error("no third") else .v end`},
			`no third`},
		"a label has too few values": {model.MetricRule{Name: "broken", Expression: ".[].v", Labels: []model.LabelRule{{Name: "who", Expression: ".[].who | select(. != null)"}}},
			`label "who" gave 2 values for 4 series, so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element`},
		"a label has too many values": {model.MetricRule{Name: "broken", Expression: ".[].v | select(. < 3)", Labels: []model.LabelRule{{Name: "who", Expression: ".[].v"}}},
			`label "who" gave 4 values for 2 series, so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element`},
		"a label has several values for one series": {model.MetricRule{Name: "broken", Expression: ".[0].v", Labels: []model.LabelRule{{Name: "who", Expression: ".[].v"}}},
			`label "who" gave 4 values for 1 series, so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element`},
		"a label's program fails": {model.MetricRule{Name: "broken", Expression: ".[].v", Labels: []model.LabelRule{{Name: "who", Expression: `.[] | if .v == 3 then error("no third") else .v end`}}},
			`label "who": error: no third`},
		"a label is an object": {model.MetricRule{Name: "broken", Expression: ".[].v", Labels: []model.LabelRule{{Name: "who", Expression: `.[] | if .v == 3 then {} else .v end`}}},
			`label "who" value 2 is an object, not a single value; select one of its fields`},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.CaptureLogs(t)
			after := model.MetricRule{Name: "after", Expression: ".[].v", ErrorMode: model.ErrorModeFail}
			tc.rule.ErrorMode = model.ErrorModeLog
			// The limit has room for the second rule's four series only if
			// the first gave back the ones it made before it failed.
			c := jqValuesCollector(4, tc.rule, after)
			set, failures, err := transformWith(context.Background(), t, c, "application/json", body)
			if err != nil || labelled(set, "who") != "after{} 1|after{} 2|after{} 3|after{} 4" {
				t.Fatalf("series %s, %v", labelled(set, "who"), err)
			}
			if len(failures) != 1 || failures[0].Metric != "broken" || failures[0].Failures != 1 || !strings.Contains(failures[0].First.Error(), tc.want) {
				t.Fatalf("failures %+v, want one of %q", failures, tc.want)
			}
			// Under fail the same error fails the scrape, as the rule's.
			tc.rule.ErrorMode = model.ErrorModeFail
			c = jqValuesCollector(4, tc.rule, after)
			_, _, err = transformWith(context.Background(), t, c, "application/json", body)
			var failure *MetricFailure
			if !errors.As(err, &failure) || failure.Metric != "broken" || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fail: %v, want %q", err, tc.want)
			}
		})
	}
}

// pairingBody has four items: three values that are numbers or mapped, one
// that is neither, and names for labels to pair or fail to.
const pairingBody = `{"i": 7, "items": [{"s":"up","name":"a"},{"s":"x","name":"b"},{"s":"down","name":"c"},{"s":"weird","name":"d"}]}`

// A rule whose labels cannot be paired with its values fails as a whole,
// once, with the pairing mistake as its failure, as on main. Taking the
// values one at a time, the rule met its values that are no numbers before
// it knew the labels would not pair, and counted each beside the pairing
// failure, the first of them as what the rule failed of: three failures of
// `value "x" is neither in value_map nor a number` where main counted one,
// of the label. Under fail the scrape failed naming the value, which was
// not the mistake. Whatever fails the rule as a whole is its one failure,
// in the order main found them: the expression failing at any value, then
// the first label that fails anywhere, does not pair, or gives a series a
// value that is no label's.
func TestAJQRuleThatFailsAsAWholeFailsOnce(t *testing.T) {
	values := map[string]float64{"up": 1, "down": 0, "weird": 2}
	names := model.LabelRule{Name: "b", Expression: ".items[].name"}
	const cannotPair = ", so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element"
	for name, tc := range map[string]struct {
		expression string
		labels     []model.LabelRule
		// want is the rule's one failure, and part what the scrape's error
		// says it is the failure of.
		part, want string
	}{
		"a value is no number and a label has too many values": {".items[].s", []model.LabelRule{{Name: "a", Expression: ".items[] | .name, .name"}, names},
			"labels", `label "a" gave 8 values for 4 series` + cannotPair},
		"a value is no number and a label has too few values": {".items[].s", []model.LabelRule{{Name: "a", Expression: ".items[0:3][] | .name"}, names},
			"labels", `label "a" gave 3 values for 4 series` + cannotPair},
		"a value is no number and a later label has too few values": {".items[].s", []model.LabelRule{names, {Name: "a", Expression: ".items[0:2][].name"}},
			"labels", `label "a" gave 2 values for 4 series` + cannotPair},
		"the expression fails after a label ran out": {`.items[] | if .name == "d" then error("val") else .s end`, []model.LabelRule{{Name: "a", Expression: ".items[0:2][].name"}, names},
			"expression", `error: val`},
		"a label fails after a value was no number": {".items[].s", []model.LabelRule{{Name: "a", Expression: `.items[] | if .name == "c" then error("lbl") else .name end`}, names},
			"labels", `label "a": error: lbl`},
		"a label fails after a later one ran out": {".items[].s", []model.LabelRule{{Name: "a", Expression: `.items[] | if .name == "d" then error("lbl") else .name end`}, {Name: "b", Expression: ".items[0:2][].name"}},
			"labels", `label "a": error: lbl`},
		"a label fails after its last paired value": {".items[0:2][].s", []model.LabelRule{{Name: "a", Expression: `.items[] | if .name == "c" then error("lbl") else .name end`}},
			"labels", `label "a": error: lbl`},
		"a label does not pair and a later one fails": {".items[].s", []model.LabelRule{{Name: "a", Expression: ".items[0:2][].name"}, {Name: "b", Expression: `.items[] | if .name == "a" then error("lbl") else .name end`}},
			"labels", `label "a" gave 2 values for 4 series` + cannotPair},
		"a label is an object for the value that is no number": {".items[].s", []model.LabelRule{{Name: "a", Expression: `.items[] | if .name == "b" then {} else .name end`}},
			"labels", `label "a" value 1 is an object, not a single value; select one of its fields`},
		"a label is an object and does not pair": {".items[].s", []model.LabelRule{{Name: "a", Expression: `.items[0:3][] | if .name == "a" then {} else .name end`}},
			"labels", `label "a" gave 3 values for 4 series` + cannotPair},
		"a label fails and there are no values": {"empty", []model.LabelRule{{Name: "a", Expression: `.items[] | if .name == "c" then error("lbl") else .name end`}},
			"labels", `label "a": error: lbl`},
	} {
		for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail} {
			t.Run(name+" under "+mode, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				rule := model.MetricRule{Name: "m", Expression: tc.expression, ValueMap: values, ErrorMode: mode, Labels: tc.labels}
				c := jqValuesCollector(0, rule, model.MetricRule{Name: "after", Expression: ".i", ErrorMode: model.ErrorModeFail})
				set, failures, err := transformWith(context.Background(), t, c, "application/json", pairingBody)
				if mode == model.ErrorModeFail {
					want := `metric "m" ` + tc.part + `: ` + tc.want
					var failure *MetricFailure
					if !errors.As(err, &failure) || failure.Metric != "m" || err.Error() != want {
						t.Fatalf("err = %v, want %q", err, want)
					}
					if len(failures) != 0 {
						t.Fatalf("failures carried on from: %+v", failures)
					}
					testutil.AssertJSONLines(t, logs, 1)
					return
				}
				// No series of the rule, and the rule after it untouched.
				if err != nil || labelled(set, "a") != "after{} 7" {
					t.Fatalf("series %s, %v", labelled(set, "a"), err)
				}
				if len(failures) != 1 || failures[0].Metric != "m" || failures[0].Failures != 1 || failures[0].Missing != 0 || failures[0].First.Error() != tc.want {
					t.Fatalf("failures %+v, want one: %s", failures, tc.want)
				}
				lines := 0
				if mode == model.ErrorModeLog {
					lines = 1
				}
				for _, record := range testutil.AssertJSONLines(t, logs, lines) {
					if record["error"] != tc.want || record["failures"] != float64(1) {
						t.Fatalf("logged %v", record)
					}
				}
			})
		}
	}
}

// Where nothing fails the rule as a whole, each value that fails is its own
// failure, as before: counted, the first of them logged once with how many
// there were, the other values' series exported; and under fail the first
// of them is the scrape's.
func TestJQValuesThatFailAreCountedWhereTheRuleDoesNotFailAsAWhole(t *testing.T) {
	const body = `{"items": [{"s":"x","name":"a"},{"s":1,"name":"b"},{"s":null,"name":"c"},{"s":"y","name":"d"},{"s":2}]}`
	labels := []model.LabelRule{{Name: "a", Expression: ".items[].name", Required: true}}
	logs := testutil.CaptureLogs(t)
	c := jqValuesCollector(0, model.MetricRule{Name: "m", Expression: ".items[].s", ErrorMode: model.ErrorModeLog, Labels: labels})
	set, failures, err := transformWith(context.Background(), t, c, "application/json", body)
	if err != nil || labelled(set, "a") != "m{b} 1" {
		t.Fatalf("series %s, %v", labelled(set, "a"), err)
	}
	// Two values that are no numbers, one that is missing, and one series
	// without its required label.
	const first = `value "x" is not a number; map text to numbers with value_map`
	if len(failures) != 1 || failures[0].Failures != 4 || failures[0].Missing != 2 || failures[0].First.Error() != first || !failures[0].Logged {
		t.Fatalf("failures %+v", failures)
	}
	if record := testutil.AssertJSONLines(t, logs, 1)[0]; record["error"] != first || record["failures"] != float64(4) {
		t.Fatalf("logged %v", record)
	}
	c.Metrics[0].ErrorMode = model.ErrorModeFail
	_, failures, err = transformWith(context.Background(), t, c, "application/json", body)
	if err == nil || err.Error() != `metric "m": `+first || len(failures) != 0 {
		t.Fatalf("under fail: %v, %+v", err, failures)
	}
	// Called without a Transform gathering them, as a test may, the held
	// failures are logged once all the same.
	logs.Reset()
	c.Metrics[0].ErrorMode = model.ErrorModeLog
	d, _ := decodedBody(t, c, "application/json", body)
	if _, err := transformJQ(withSeriesBudget(context.Background(), 0), d.Data, c.Metrics, &c); err != nil {
		t.Fatal(err)
	}
	if record := testutil.AssertJSONLines(t, logs, 1)[0]; record["error"] != first || record["failures"] != float64(4) {
		t.Fatalf("logged %v", record)
	}
}

// Finding out whether the rule fails as a whole reads the rest of what its
// expressions give without keeping it: a rule of 20,000 values whose label
// runs out at the third, or whose first value is no number under fail,
// costs what running its programs does, a small part of what making its
// series costs, and is told how many values there were. The series limit
// still stops the rule at the first series past it, before a label that is
// one value short is known not to pair.
func TestFindingAJQRulesWholeFailureKeepsNoValues(t *testing.T) {
	const values = 20000
	body := `{"few": ["a", "b"], "items": [{"v":"x","id":"a"},` + strings.TrimSuffix(repeated(`{"v":1,"id":"a"},`, values-1), ",") + "]}"
	ids := model.LabelRule{Name: "id", Expression: ".items[].id"}
	run := func(c model.Collector) (set *model.MetricSet, failures []RuleFailure, bytes uint64, err error) {
		d, r := decodedBody(t, c, "application/json", body)
		ctx, report := WithRuleReport(context.Background())
		bytes = allocated(func() { set, err = Transform(ctx, d, r, &c, "") })
		return set, report.Failures(), bytes, err
	}
	testutil.CaptureLogs(t)
	set, failures, all, err := run(jqValuesCollector(0, model.MetricRule{Name: "n", Expression: ".items[].v", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{ids}}))
	if err != nil || len(set.Metrics) != values-1 || len(failures) != 1 || failures[0].Failures != 1 {
		t.Fatalf("every series: %d, %+v, %v", len(set.Metrics), failures, err)
	}

	short := model.MetricRule{Name: "n", Expression: ".items[].v", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{ids, {Name: "few", Expression: ".few[]"}}}
	set, failures, unpaired, err := run(jqValuesCollector(0, short))
	const want = `label "few" gave 2 values for 20000 series, so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element`
	if err != nil || len(set.Metrics) != 0 || len(failures) != 1 || failures[0].Failures != 1 || failures[0].First.Error() != want {
		t.Fatalf("a label that runs out: %+v, %+v, %v", set, failures, err)
	}

	failing := model.MetricRule{Name: "n", Expression: ".items[].v", ErrorMode: model.ErrorModeFail, Labels: []model.LabelRule{ids}}
	_, _, single, err := run(jqValuesCollector(0, failing))
	if err == nil || err.Error() != `metric "n": value "x" is not a number; map text to numbers with value_map` {
		t.Fatalf("a first value that is no number: %v", err)
	}
	failing.Labels = short.Labels
	_, _, both, err := run(jqValuesCollector(0, failing))
	if err == nil || err.Error() != `metric "n" labels: `+want {
		t.Fatalf("a first value that is no number and a label that runs out: %v", err)
	}
	t.Logf("every series %d bytes; a label that runs out %d, a first value that fails %d, both %d", all, unpaired, single, both)
	for name, bytes := range map[string]uint64{"a label that runs out": unpaired, "a first value that fails": single, "both": both} {
		// The programs allocate as they give each value; nothing is kept.
		if bytes > all/4 {
			t.Errorf("%s: finding the rule's failure allocated %d bytes, making every series %d", name, bytes, all)
		}
	}

	// Past the limit the rule stops there, as it did.
	oneShort := model.MetricRule{Name: "n", Expression: ".items[].v", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "id", Expression: ".items[1:][].id"}}}
	_, failures, _, err = run(jqValuesCollector(0, oneShort))
	if err != nil || len(failures) != 1 || failures[0].Failures != 1 || !strings.HasPrefix(failures[0].First.Error(), `label "id" gave 19999 values for 20000 series`) {
		t.Fatalf("a label one value short: %+v, %v", failures, err)
	}
	_, _, limited, err := run(jqValuesCollector(seriesLimit, oneShort))
	if err == nil || err.Error() != wantLimitFailed || !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("at the limit: %v", err)
	}
	if limited > all/8 {
		t.Errorf("stopping at the limit allocated %d bytes, making every series %d", limited, all)
	}
}
