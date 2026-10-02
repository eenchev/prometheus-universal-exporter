package transform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// jqOutcome is everything a run of a jq collector's transform gives: its
// series, its error and what kind it is, what its rules carried on without,
// and what it logged.
type jqOutcome struct {
	Series        []model.Metric
	Err           string
	Limit         bool
	Missing       bool
	FailedMetric  string
	RuleFailures  []string
	Logs          string
	SeriesWasNone bool
}

// jqOutcomeOf transforms body with c, its rules logging to a buffer without
// the time of day, so two runs can be compared line for line.
func jqOutcomeOf(t *testing.T, c model.Collector, body string) jqOutcome {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	set, failures, err := transformWith(WithRuleLogger(context.Background(), logger), t, c, "application/json", body)
	out := jqOutcome{Logs: logs.String(), SeriesWasNone: set == nil}
	if set != nil {
		out.Series = set.Metrics
	}
	if err != nil {
		out.Err, out.Limit, out.Missing = err.Error(), errors.Is(err, model.ErrLimitExceeded), errors.Is(err, model.ErrMissingValue)
		var failure *MetricFailure
		if errors.As(err, &failure) {
			out.FailedMetric = failure.Metric
		}
	}
	for _, f := range failures {
		out.RuleFailures = append(out.RuleFailures, fmt.Sprintf("%s: %d failed, %d missing, logged %v, first %v", f.Metric, f.Failures, f.Missing, f.Logged, f.First))
	}
	return out
}

// apart is c with the items expression of each rule made its own text, by
// blanks after it that mean nothing to jq, so that no two rules share one
// and each evaluates its own, as every rule did before items were shared.
func apart(c model.Collector) model.Collector {
	rules := make([]model.MetricRule, len(c.Metrics))
	for i, rule := range c.Metrics {
		if rule.Items != "" {
			rule.Items += strings.Repeat(" ", i+1)
		}
		rules[i] = rule
	}
	c.Metrics = rules
	return c
}

const sharedItemsBody = `{"items":[
	{"id":"a","region":"eu","value":1,"size":10},
	{"id":"b","region":"us","value":"n/a","size":20},
	{"id":"c","value":3,"size":null},
	{"id":"d","region":"eu","value":4,"size":40},
	7,
	{"id":"f","region":["x"],"value":6,"size":60}
],"none":[],"text":"not a list"}`

// Rules that share an items expression give exactly what they gave when
// each evaluated it for itself: the same series in the same order, the same
// error for the scrape, and for each rule the same failures counted and
// logged, whatever its error mode. Each collector is transformed as it is
// written, its rules sharing, and with every rule's items made a text of
// its own, which is how every rule was evaluated before, and the two are
// compared whole. The cases are the ways a rule's items end: read through,
// failing part of the way or at once, selecting nothing, items a value
// cannot be read from, and the series limit met in the first rule, in a
// later one, and not at all by a response with more items than it has room
// for series.
func TestRulesSharingItemsGiveWhatTheyGaveApart(t *testing.T) {
	labels := []model.LabelRule{{Name: "id", Expression: ".id"}, {Name: "region", Expression: ".region", Required: true}}
	value := func(name, items, expression, mode string) model.MetricRule {
		return model.MetricRule{Name: name, Type: model.GaugeMetricType, Items: items, Expression: expression, ErrorMode: mode, Labels: labels}
	}
	optional := false
	failing := `.items[] | if type == "number" then error("boom") else . end`
	for name, tc := range map[string]struct {
		rules []model.MetricRule
		limit int
	}{
		"read through":                                         {rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("s", ".items[]", ".size", model.ErrorModeLog), value("again", ".items[]", ".value", model.ErrorModeLog)}},
		"a rule failing an item under fail":                    {rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("s", ".items[]", ".size", model.ErrorModeFail)}},
		"the first rule failing an item under fail":            {rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeFail), value("s", ".items[]", ".size", model.ErrorModeIgnore)}},
		"items failing part of the way":                        {rules: []model.MetricRule{value("v", failing, ".value", model.ErrorModeIgnore), value("s", failing, ".size", model.ErrorModeLog), {Name: "n", Expression: ".items | length"}}},
		"items failing part of the way, the second under fail": {rules: []model.MetricRule{value("v", failing, ".value", model.ErrorModeLog), value("s", failing, ".size", model.ErrorModeFail)}},
		"items failing part of the way, the first under fail":  {rules: []model.MetricRule{value("v", failing, ".value", model.ErrorModeFail), value("s", failing, ".size", model.ErrorModeLog)}},
		"items failing at once":                                {rules: []model.MetricRule{value("v", ".text[]", ".value", model.ErrorModeLog), value("s", ".text[]", ".size", model.ErrorModeIgnore), value("t", ".text[]", ".size", model.ErrorModeLog)}},
		"items selecting nothing":                              {rules: []model.MetricRule{value("v", ".none[]", ".value", model.ErrorModeLog), value("s", ".none[]", ".size", model.ErrorModeIgnore), {Name: "o", Items: ".none[]", Expression: ".", Required: &optional}, value("f", ".none[]", ".size", model.ErrorModeFail)}},
		"items that do not compile":                            {rules: []model.MetricRule{value("v", ".items[", ".value", model.ErrorModeLog), value("s", ".items[", ".size", model.ErrorModeIgnore)}},
		"two lists, their rules interleaved":                   {rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("w", ".items[1:][]", ".value", model.ErrorModeIgnore), {Name: "n", Expression: ".items | length"}, value("s", ".items[]", ".size", model.ErrorModeIgnore), value("x", ".items[1:][]", ".size", model.ErrorModeIgnore)}},
		"items made by the program":                            {rules: []model.MetricRule{value("v", ".items[] | objects | {id, region, value: (.value | numbers)}", ".value", model.ErrorModeLog), value("s", ".items[] | objects | {id, region, value: (.value | numbers)}", ".value * 2", model.ErrorModeLog)}},
		"the limit met in the first rule":                      {limit: 1, rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("s", ".items[]", ".size", model.ErrorModeIgnore)}},
		"the limit met in the second rule":                     {limit: 3, rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("s", ".items[]", ".size", model.ErrorModeIgnore)}},
		"the limit met exactly":                                {limit: 4, rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("s", ".items[]", ".size", model.ErrorModeIgnore)}},
		"more items than the limit has room for":               {limit: 5, rules: []model.MetricRule{value("v", ".items[]", ".value", model.ErrorModeIgnore), value("s", ".items[]", ".size", model.ErrorModeIgnore), value("t", ".items[]", ".none", model.ErrorModeIgnore)}},
		"more items than the limit has room for, and failing":  {limit: 4, rules: []model.MetricRule{value("v", failing, ".value", model.ErrorModeLog), value("s", failing, ".size", model.ErrorModeLog)}},
	} {
		t.Run(name, func(t *testing.T) {
			c := model.Collector{Name: "shared", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: tc.rules, Limits: model.Limits{MaxMetrics: tc.limit}}
			shared, each := jqOutcomeOf(t, c, sharedItemsBody), jqOutcomeOf(t, apart(c), sharedItemsBody)
			if !reflect.DeepEqual(shared, each) {
				t.Fatalf("sharing items:\n%+v\neach rule its own:\n%+v", shared, each)
			}
			if len(shared.Series) == 0 && shared.Err == "" && len(shared.RuleFailures) == 0 {
				t.Fatalf("the case shows nothing: %+v", shared)
			}
		})
	}
}

// The rule after one with the same items expression reads the items that
// rule's program gave, and does not run the program again: with the kept
// list changed between the two, the second rule's series are of the changed
// items. A rule with another expression runs its own, and a rule the only
// one with its expression keeps nothing.
func TestSharedItemsAreEvaluatedOnce(t *testing.T) {
	data := map[string]any{"items": []any{map[string]any{"v": 1.0}, map[string]any{"v": 2.0}}}
	rules := []model.MetricRule{
		{Name: "first", Items: ".items[]", Expression: ".v"},
		{Name: "second", Items: ".items[]", Expression: ".v"},
		{Name: "other", Items: ".items[1:][]", Expression: ".v"},
	}
	c := &model.Collector{Name: "once"}
	out := &model.MetricSet{}
	var sharing jqSharing
	ctx := context.Background()
	if err := transformJQItems(ctx, out, data, rules[0], c, &sharing, rules[1:]); err != nil {
		t.Fatal(err)
	}
	if len(sharing.lists) != 1 || !sharing.lists[0].complete || len(sharing.lists[0].items) != 2 {
		t.Fatalf("after the first rule: %+v", sharing.lists)
	}
	sharing.lists[0].items[1] = map[string]any{"v": 40.0}
	if err := transformJQItems(ctx, out, data, rules[1], c, &sharing, rules[2:]); err != nil {
		t.Fatal(err)
	}
	if err := transformJQItems(ctx, out, data, rules[2], c, &sharing, nil); err != nil {
		t.Fatal(err)
	}
	var values []float64
	for _, m := range out.Metrics {
		values = append(values, m.Value)
	}
	if want := []float64{1, 2, 1, 40, 2}; !reflect.DeepEqual(values, want) {
		t.Fatalf("values %v, want %v: the second rule reads the first's items, the third its own", values, want)
	}
	if len(sharing.lists) != 1 {
		t.Fatalf("a rule alone with its expression kept its items: %+v", sharing.lists)
	}
}

// A list is kept only while it has no more items than limits.max_metrics
// left room for series when its rule began: at the first item past that the
// list is forgotten, the rule reads on from its program, and the rules
// after it evaluate the expression themselves, so no more items are ever
// held for sharing than the limit allows series.
func TestSharedItemsAreNotKeptPastTheSeriesLimit(t *testing.T) {
	items := make([]any, 50)
	for i := range items {
		// Only every tenth item has a value, so the rule passes the limit
		// of 20 series while its items do not.
		item := map[string]any{}
		if i%10 == 0 {
			item["v"] = float64(i)
		}
		items[i] = item
	}
	data := map[string]any{"items": items}
	optional := false
	rules := []model.MetricRule{
		{Name: "first", Items: ".items[]", Expression: ".v", Required: &optional},
		{Name: "second", Items: ".items[]", Expression: ".v", Required: &optional},
	}
	c := &model.Collector{Name: "bounded"}
	for _, tc := range []struct {
		limit int
		kept  bool
	}{{20, false}, {49, false}, {50, true}, {0, true}} {
		out := &model.MetricSet{}
		var sharing jqSharing
		ctx := withSeriesBudget(context.Background(), tc.limit)
		if err := transformJQItems(ctx, out, data, rules[0], c, &sharing, rules[1:]); err != nil {
			t.Fatal(err)
		}
		list := sharing.lists[0]
		if list.complete != tc.kept || (!tc.kept && (list.items != nil || cap(list.items) != 0)) || (tc.kept && len(list.items) != 50) {
			t.Fatalf("limit %d: kept %d items, complete %v", tc.limit, len(list.items), list.complete)
		}
		// The room made for series is the limit's too, give or take what
		// the allocator rounds a size up by.
		if tc.limit > 0 && cap(out.Metrics) >= 2*tc.limit {
			t.Fatalf("limit %d: room made for %d series", tc.limit, cap(out.Metrics))
		}
		if err := transformJQItems(ctx, out, data, rules[1], c, &sharing, nil); err != nil {
			t.Fatal(err)
		}
		if len(out.Metrics) != 10 || out.Metrics[9].Value != 40 || out.Metrics[9].Name != "second" {
			t.Fatalf("limit %d: series %+v", tc.limit, out.Metrics)
		}
	}
}

// stoppingContext is a context that is done from the n-th time its error is
// asked for, as a deadline that passes while a rule reads its items is.
type stoppingContext struct {
	context.Context
	asked *int
	after int
	done  chan struct{}
}

func (c stoppingContext) Err() error {
	*c.asked++
	if *c.asked < c.after {
		return nil
	}
	if *c.asked == c.after {
		close(c.done)
	}
	return context.DeadlineExceeded
}

func (c stoppingContext) Done() <-chan struct{} { return c.done }

// A deadline that passes while a rule reads items another rule's program
// gave fails the transform as one that passes while its own program runs
// does: with the context's error, naming the rule it was at, nothing
// answered and nothing counted against the rule, whatever its error mode.
// Nothing in the rule would otherwise notice: its value is a field of the
// item, looked up without running a program.
func TestADeadlineWhileReadingSharedItemsFailsTheTransform(t *testing.T) {
	const body = `{"items":[{"value":1,"size":1},{"value":2,"size":2},{"value":3,"size":3},{"value":4,"size":4}]}`
	for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog, model.ErrorModeFail} {
		t.Run(mode, func(t *testing.T) {
			c := model.Collector{Name: "slow", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
				Metrics: []model.MetricRule{
					{Name: "first", Items: ".items[]", Expression: ".value", ErrorMode: mode},
					{Name: "second", Items: ".items[]", Expression: ".size", ErrorMode: mode},
				}}
			// The first rule never asks for the context's error, its
			// program watching the context's channel; the second asks
			// before each kept item, and the deadline passes at its third.
			asked := 0
			ctx := stoppingContext{Context: context.Background(), asked: &asked, after: 3, done: make(chan struct{})}
			set, failures, err := transformWith(ctx, t, c, "application/json", body)
			if set != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("series %+v, err %v; want the deadline's error and no series", set, err)
			}
			if want := `the transform was stopped at metric "second": context deadline exceeded`; err.Error() != want {
				t.Fatalf("err %q, want %q", err, want)
			}
			var failure *MetricFailure
			if errors.As(err, &failure) {
				t.Fatalf("the deadline is reported as the failure of rule %q", failure.Metric)
			}
			if len(failures) != 0 {
				t.Fatalf("counted against the rules: %+v", failures)
			}
			// With a context that stays live, both rules read every item.
			set, _, err = transformWith(context.Background(), t, c, "application/json", body)
			if err != nil || len(set.Metrics) != 8 {
				t.Fatalf("with a live context: %+v, %v", set, err)
			}
		})
	}
}

// jqIterationLength tells how many items an expression that is a path of
// fields and an iteration gives, and nothing of any other expression or of
// data that has no array or object there: it sizes what the items are
// gathered in, and a wrong answer would only size it badly, so what is
// checked is that it is right where it answers.
func TestJQIterationLength(t *testing.T) {
	data := map[string]any{"items": []any{1, 2, 3}, "data": map[string]any{"rows": []any{1}, "by_name": map[string]any{"a": 1, "b": 2}}, "text": "x", "nothing": nil}
	for expression, want := range map[string]int{".items[]": 3, ".data.rows[]": 1, ".data.by_name[]": 2, ".[]": 4} {
		if n, ok := jqIterationLength(data, expression); !ok || n != want {
			t.Errorf("%s: %d, %v; want %d", expression, n, ok, want)
		}
	}
	if n, ok := jqIterationLength([]any{1, 2}, ".[]"); !ok || n != 2 {
		t.Errorf(".[] of an array: %d, %v", n, ok)
	}
	for _, expression := range []string{".items", ".items[]?", ".items[] | .a", ".text[]", ".nothing[]", ".missing[]", ".data.rows.x[]", "items[]", ".items[0][]", `."items"[]`, "..[]", ".[][]", ".items []", "[]", "", ".1[]", ".data..rows[]"} {
		if n, ok := jqIterationLength(data, expression); ok {
			t.Errorf("%q: %d items known", expression, n)
		}
	}
}

// evaluateJQOne gives what it gave when it gathered every value of the
// program in a list first, as evaluateJQ does: the one value, nil for none,
// the program's error, and for more than one value the error that counts
// them.
func TestEvaluateJQOneAgreesWithEvaluateJQ(t *testing.T) {
	listed := func(input, root any, expression string) (any, error) {
		values, err := evaluateJQ(context.Background(), input, root, expression)
		if err != nil {
			return nil, err
		}
		switch len(values) {
		case 0:
			return nil, nil
		case 1:
			return values[0], nil
		default:
			return nil, fmt.Errorf("expression %q produced %d values for one item; it must produce at most one", expression, len(values))
		}
	}
	inputs := []any{nil, 1.0, "text", []any{1.0, 2.0, 3.0}, map[string]any{"a": 1.0, "b": map[string]any{"c": "x"}, "list": []any{1.0, 2.0}}, map[string]any{}}
	expressions := []string{".", ".a", ".b.c", ".b.missing.deeper", ".list[]", ".[]", "empty", ".a, .b", ".a, error(\"x\")", "error(\"first\")", ".list | length", "$root", ".a + 1", ".b | keys[]", "1, 2, 3", ".[0]", "not valid ["}
	compared := 0
	for _, input := range inputs {
		for _, expression := range expressions {
			want, wantErr := listed(input, input, expression)
			got, gotErr := evaluateJQOne(context.Background(), input, input, expression)
			if !reflect.DeepEqual(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("%s of %v: %v, %v; a list of its values gives %v, %v", expression, input, got, gotErr, want, wantErr)
			}
			compared++
		}
	}
	t.Logf("%d evaluations compared", compared)
}
