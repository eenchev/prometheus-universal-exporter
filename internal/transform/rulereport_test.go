package transform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// reported is a rule's failure as a test compares it: the error by its text.
type reported struct {
	metric, expression, items string
	failures, missing         uint64
	first                     string
	logged                    bool
}

// reportedOf is what failures hold, each as reported.
func reportedOf(failures []RuleFailure) []reported {
	out := make([]reported, 0, len(failures))
	for _, f := range failures {
		out = append(out, reported{f.Metric, f.Expression, f.Items, f.Failures, f.Missing, f.First.Error(), f.Logged})
	}
	return out
}

// Two rules of one metric name are two rules to the report: each has an
// entry of its own, with its expression and its items, its own count of the
// series it carried on without and of the values that were missing, its own
// first error, and whether it is logged, in the order the rules first failed.
// Before, the two were one entry under the name, with the first rule's first
// error, so whatever the second failed with was told to nobody. Rules alike
// in name, expression and items are one rule, as they are to the transform.
func TestRulesOfOneMetricNameAreReportedEachByItself(t *testing.T) {
	static := func(name, value string) model.LabelRule { return model.LabelRule{Name: name, Value: value} }
	for name, tc := range map[string]struct {
		c           model.Collector
		contentType string
		body        string
		want        []reported
	}{
		"two XPaths": {
			c: model.Collector{Name: "disks", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
				{Name: "disk_bytes", Type: model.GaugeMetricType, Expression: "//disk/used", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{static("state", "used"), {Name: "disk", Expression: "../@name"}}},
				{Name: "disk_bytes", Type: model.GaugeMetricType, Expression: "//disk/free", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{static("state", "free"), {Name: "disk", Expression: "../@name"}}},
				{Name: "disks", Type: model.GaugeMetricType, Expression: "count(//disk)", ErrorMode: model.ErrorModeLog},
			}},
			contentType: "application/xml",
			body:        `<disks><disk name="a"><used>1</used><free>n/a</free></disk><disk name="b"><used></used><free>n/a</free></disk><disk name="c"><used></used><free>7</free></disk></disks>`,
			want: []reported{
				{"disk_bytes", "//disk/used", "", 2, 2, `metric "disk_bytes" value is missing for node 1: XPath "//disk/used" selected a node without a value`, true},
				{"disk_bytes", "//disk/free", "", 2, 0, `metric "disk_bytes" node 0: value "n/a" is not a number; map text to numbers with value_map`, false},
			},
		},
		"one expression over two items": {
			c: model.Collector{Name: "queues", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{
				{Name: "queue_jobs", Type: model.GaugeMetricType, Items: ".fast[]", Expression: ".jobs", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}}},
				{Name: "queue_jobs", Type: model.GaugeMetricType, Items: ".slow[]", Expression: ".jobs", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}}},
			}},
			contentType: "application/json",
			body:        `{"fast":[{"name":"a","jobs":null},{"name":"b","jobs":2}],"slow":[{"name":"c","jobs":"many"},{"name":"d","jobs":null}]}`,
			want: []reported{
				{"queue_jobs", ".jobs", ".fast[]", 1, 1, `metric "queue_jobs" value is missing for item 0`, true},
				{"queue_jobs", ".jobs", ".slow[]", 2, 1, `metric "queue_jobs" item 0: value "many" is not a number; map text to numbers with value_map`, true},
			},
		},
		"rules alike in name, expression and items": {
			c: model.Collector{Name: "twins", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"}, Metrics: []model.MetricRule{
				{Name: "used", Type: model.GaugeMetricType, Expression: "used", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{static("copy", "1"), {Name: "host", Expression: "host"}}},
				{Name: "used", Type: model.GaugeMetricType, Expression: "used", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{static("copy", "2"), {Name: "host", Expression: "host"}}},
			}},
			contentType: "text/csv",
			body:        "host,used\nh1,10\nh2,\n",
			want: []reported{
				{"used", "used", "", 2, 2, `CSV column "used" is empty in row 2`, true},
			},
		},
		"an XPath engine failure in the second rule": {
			c: model.Collector{Name: "cells", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
				{Name: "cells", Type: model.GaugeMetricType, Expression: "//a/v", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{static("kind", "value")}},
				{Name: "cells", Type: model.GaugeMetricType, Expression: "count(//a[contains(@x, 5)])", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{static("kind", "marked")}},
			}},
			contentType: "application/xml",
			body:        `<r><a x="5"><v></v></a></r>`,
			want: []reported{
				{"cells", "//a/v", "", 1, 1, `metric "cells" value is missing for node 0: XPath "//a/v" selected a node without a value`, true},
				{"cells", "count(//a[contains(@x, 5)])", "", 1, 0, `metric "cells" XPath "count(//a[contains(@x, 5)])" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`, true},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := LeaveRuleLoggingToCaller(context.Background())
			_, failures, err := transformWith(ctx, t, tc.c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if got := reportedOf(failures); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("the report is\n%+v\nwant\n%+v", got, tc.want)
			}

			// Two Transforms that report to one context add up rule by
			// rule, too.
			d, r := decodedBody(t, tc.c, tc.contentType, tc.body)
			ctx, report := WithRuleReport(ctx)
			for range 2 {
				if _, err := Transform(ctx, d, r, &tc.c, "python3"); err != nil {
					t.Fatal(err)
				}
			}
			twice := append([]reported(nil), tc.want...)
			for i := range twice {
				twice[i].failures, twice[i].missing = 2*twice[i].failures, 2*twice[i].missing
			}
			if got := reportedOf(report.Failures()); fmt.Sprint(got) != fmt.Sprint(twice) {
				t.Errorf("two transforms report\n%+v\nwant\n%+v", got, twice)
			}
		})
	}
}

// mergedRuleFailure is RuleFailure as it was while the report had one entry
// per metric name.
type mergedRuleFailure struct {
	Metric            string
	Failures, Missing uint64
	First             error
	Logged            bool
}

// addMerged is RuleReport.add as it was then, the oracle of
// TestTheReportAddsUpByMetricNameToWhatItDid: the failures of every rule of
// a name were added to the name's one entry.
func addMerged(merged []mergedRuleFailure, metric string, failures, missing uint64, first error, logged bool) []mergedRuleFailure {
	for i := range merged {
		if merged[i].Metric == metric {
			merged[i].Failures += failures
			merged[i].Missing += missing
			if logged && !merged[i].Logged {
				merged[i].Logged, merged[i].First = true, first
			}
			return merged
		}
	}
	return append(merged, mergedRuleFailure{Metric: metric, Failures: failures, Missing: missing, First: first, Logged: logged})
}

// What the report holds of a metric name adds up to what it held of it
// before its rules were reported each by itself, over generated rules and
// sequences of their failures: the series that failed and the values that
// were missing, which is what the self-metrics count by the name; whether
// any of it is logged; and the names in the order they first failed. The
// first error the name's entry had is the first error of one of its rules.
// A name only one rule has is reported exactly as it was.
func TestTheReportAddsUpByMetricNameToWhatItDid(t *testing.T) {
	names, expressions, items := []string{"a", "b", "c", ""}, []string{"x", "y", ""}, []string{"", "i"}
	for seed := range uint64(300) {
		random := rand.New(rand.NewPCG(seed, 14))
		// Of the first names every rule has one expression and no items, so
		// the name has one rule; the others have several.
		alone := random.IntN(len(names) + 1)
		report := &RuleReport{}
		var merged []mergedRuleFailure
		for i := range 1 + random.IntN(40) {
			name := random.IntN(len(names))
			expression, item := expressions[0], items[0]
			if name >= alone {
				expression, item = expressions[random.IntN(len(expressions))], items[random.IntN(len(items))]
			}
			count := 1 + random.Uint64N(5)
			missing := random.Uint64N(count + 1)
			first, logged := fmt.Errorf("failure %d", i), random.IntN(2) == 0
			report.add(names[name], expression, item, count, missing, first, logged)
			merged = addMerged(merged, names[name], count, missing, first, logged)
		}

		type identity struct{ metric, expression, items string }
		rules := map[identity]bool{}
		var byName []mergedRuleFailure
		// firsts are, by name, the first errors of the name's logged rules,
		// and that of its first rule to fail.
		firsts, firstOfName := map[string][]string{}, map[string]string{}
		for _, f := range report.Failures() {
			if rules[identity{f.Metric, f.Expression, f.Items}] {
				t.Fatalf("seed %d: the rule of %+v has two entries", seed, f)
			}
			rules[identity{f.Metric, f.Expression, f.Items}] = true
			if f.Logged {
				firsts[f.Metric] = append(firsts[f.Metric], f.First.Error())
			}
			at := slices.IndexFunc(byName, func(known mergedRuleFailure) bool { return known.Metric == f.Metric })
			if at < 0 {
				byName, firstOfName[f.Metric] = append(byName, mergedRuleFailure{Metric: f.Metric, Failures: f.Failures, Missing: f.Missing, Logged: f.Logged}), f.First.Error()
				continue
			}
			byName[at].Failures += f.Failures
			byName[at].Missing += f.Missing
			byName[at].Logged = byName[at].Logged || f.Logged
		}
		if len(byName) != len(merged) {
			t.Fatalf("seed %d: %d names are reported, want %d", seed, len(byName), len(merged))
		}
		for i, was := range merged {
			if got := byName[i]; got.Metric != was.Metric || got.Failures != was.Failures || got.Missing != was.Missing || got.Logged != was.Logged {
				t.Errorf("seed %d: the rules of %q add up to %+v, want %+v", seed, was.Metric, got, was)
			}
			// The name's first error was that of its first logged rule, or
			// of its first rule to fail when none is logged.
			if was.Logged && !slices.Contains(firsts[was.Metric], was.First.Error()) || !was.Logged && firstOfName[was.Metric] != was.First.Error() {
				t.Errorf("seed %d: the first error %v the name %q had is not the first error of a rule of it: its logged rules have %v, its first rule %v", seed, was.First, was.Metric, firsts[was.Metric], firstOfName[was.Metric])
			}
		}
		for _, f := range report.Failures() {
			if !slices.Contains(names[:alone], f.Metric) {
				continue
			}
			for _, was := range merged {
				if was.Metric == f.Metric && (f.Failures != was.Failures || f.Missing != was.Missing || f.First.Error() != was.First.Error() || f.Logged != was.Logged) {
					t.Errorf("seed %d: the one rule of %q is reported as %+v, want as it was, %+v", seed, f.Metric, f, was)
				}
			}
		}
	}
}

// Two rules alike in name, expression and items are one rule to the report,
// and it is logged when either has error_mode log, whichever comes first.
// With the rule under ignore first, the twin under log was not logged at
// all: the mode of the first to fail decided for both, though the report
// itself, adding two transforms up, has always taken log from either. This
// is older than the rules' being reported each by itself. Either way round
// the entry counts the failures of both, the transform's own line — written
// when the caller does not log the rules itself — is one, with error_mode
// log and both rules' failures, and twins both under ignore are counted and
// not logged.
func TestTwinRulesAreLoggedWhenEitherHasLog(t *testing.T) {
	static := func(value string) model.LabelRule { return model.LabelRule{Name: "copy", Value: value} }
	for name, tc := range map[string]struct {
		c           model.Collector
		contentType string
		body        string
		want        reported
	}{
		"csv": {
			c: model.Collector{Name: "twins", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"}, Metrics: []model.MetricRule{
				{Name: "used", Type: model.GaugeMetricType, Expression: "used", Labels: []model.LabelRule{static("1"), {Name: "host", Expression: "host"}}},
				{Name: "used", Type: model.GaugeMetricType, Expression: "used", Labels: []model.LabelRule{static("2"), {Name: "host", Expression: "host"}}},
			}},
			contentType: "text/csv",
			body:        "host,used\nh1,10\nh2,\n",
			want:        reported{"used", "used", "", 2, 2, `CSV column "used" is empty in row 2`, true},
		},
		"jq": {
			c: model.Collector{Name: "twins", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{
				{Name: "jobs", Type: model.GaugeMetricType, Items: ".queues[]", Expression: ".jobs", Labels: []model.LabelRule{static("1"), {Name: "queue", Expression: ".name"}}},
				{Name: "jobs", Type: model.GaugeMetricType, Items: ".queues[]", Expression: ".jobs", Labels: []model.LabelRule{static("2"), {Name: "queue", Expression: ".name"}}},
			}},
			contentType: "application/json",
			body:        `{"queues":[{"name":"a","jobs":null},{"name":"b","jobs":2},{"name":"c","jobs":null}]}`,
			want:        reported{"jobs", ".jobs", ".queues[]", 4, 4, `metric "jobs" value is missing for item 0`, true},
		},
		"xpath": {
			c: model.Collector{Name: "twins", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
				{Name: "used", Type: model.GaugeMetricType, Expression: "//disk/used", Labels: []model.LabelRule{static("1"), {Name: "disk", Expression: "../@name"}}},
				{Name: "used", Type: model.GaugeMetricType, Expression: "//disk/used", Labels: []model.LabelRule{static("2"), {Name: "disk", Expression: "../@name"}}},
			}},
			contentType: "application/xml",
			body:        `<disks><disk name="a"><used>1</used></disk><disk name="b"><used></used></disk></disks>`,
			want:        reported{"used", "//disk/used", "", 2, 2, `metric "used" value is missing for node 1: XPath "//disk/used" selected a node without a value`, true},
		},
	} {
		for _, modes := range [][2]string{
			{model.ErrorModeIgnore, model.ErrorModeLog},
			{model.ErrorModeLog, model.ErrorModeIgnore},
			{model.ErrorModeLog, model.ErrorModeLog},
			{model.ErrorModeIgnore, model.ErrorModeIgnore},
		} {
			t.Run(name+" under "+modes[0]+" then "+modes[1], func(t *testing.T) {
				c := tc.c
				c.Metrics = slices.Clone(tc.c.Metrics)
				c.Metrics[0].ErrorMode, c.Metrics[1].ErrorMode = modes[0], modes[1]
				want := tc.want
				want.logged = modes[0] == model.ErrorModeLog || modes[1] == model.ErrorModeLog
				var logs bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
				set, failures, err := transformWith(WithRuleLogger(context.Background(), logger), t, c, tc.contentType, tc.body)
				if err != nil {
					t.Fatal(err)
				}
				if got := reportedOf(failures); len(got) != 1 || got[0] != want {
					t.Errorf("the report is\n%+v\nwant the one entry\n%+v", got, want)
				}
				lines := strings.Count(logs.String(), `"msg":"metric extraction failed"`)
				if !want.logged && lines != 0 {
					t.Errorf("twins under ignore are logged:\n%s", &logs)
				}
				if line := fmt.Sprintf(`"metric":%q,"error_mode":"log","error":%q,"failures":%d`, want.metric, want.first, want.failures); want.logged && (lines != 1 || !strings.Contains(logs.String(), line)) {
					t.Errorf("the transform logged\n%swant one line with %s", &logs, line)
				}
				// The series are those of both rules, as they were.
				copies := map[string]int{}
				for _, m := range set.Metrics {
					copies[m.Labels["copy"]]++
				}
				if copies["1"] == 0 || copies["1"] != copies["2"] {
					t.Errorf("the twins made %v series", copies)
				}
			})
		}
	}
}

// addCountedAsItWas is ruleFailures.addCounted as it was while the first of
// two twins to fail decided whether both are logged, the oracle of
// TestFailuresAreGatheredAsTheyWereButForATwinUnderLog.
func (f *ruleFailures) addCountedAsItWas(rule model.MetricRule, first error, count, missing uint64, logged bool) {
	for _, known := range f.rules {
		if known.rule.Name == rule.Name && known.rule.Expression == rule.Expression && known.rule.Items == rule.Items {
			known.count += count
			known.missing += missing
			return
		}
	}
	f.rules = append(f.rules, &failedRule{rule: rule, first: first, count: count, missing: missing, logged: logged})
}

// What a transform gathers of its rules' failures is what it gathered, over
// generated sequences of failures of rules with twins among them: the same
// rules in the same order with the same counts, and, for every rule but one
// under ignore that a twin under log joined, the same first error, mode and
// whether it is logged. That one is now logged, as the rule under log, with
// the twin's first failure as the one to show, and so it is reported.
func TestFailuresAreGatheredAsTheyWereButForATwinUnderLog(t *testing.T) {
	names, expressions, items := []string{"a", "b", ""}, []string{"x", "y"}, []string{"", "i"}
	modes := []string{model.ErrorModeLog, model.ErrorModeIgnore}
	var same, upgraded int
	for seed := range uint64(300) {
		random := rand.New(rand.NewPCG(seed, 16))
		// Nothing is logged of them here: the caller's report is compared.
		ctx := LeaveRuleLoggingToCaller(context.Background())
		now, was := &ruleFailures{ctx: ctx}, &ruleFailures{ctx: ctx}
		// joined are, by rule, the first failure of a twin under log that
		// joined what one under ignore had gathered.
		type identity struct{ metric, expression, items string }
		joined, logs := map[identity]error{}, map[identity]bool{}
		for i := range 1 + random.IntN(30) {
			rule := model.MetricRule{Name: names[random.IntN(len(names))], Expression: expressions[random.IntN(len(expressions))], Items: items[random.IntN(len(items))], ErrorMode: modes[random.IntN(len(modes))]}
			count := 1 + random.Uint64N(5)
			missing, first, logged := random.Uint64N(count+1), fmt.Errorf("failure %d", i), rule.ErrorMode == model.ErrorModeLog
			id := identity{rule.Name, rule.Expression, rule.Items}
			if known, gathered := logs[id]; gathered && logged && !known {
				joined[id] = first
			}
			logs[id] = logs[id] || logged
			now.addCounted(rule, first, count, missing, logged)
			was.addCountedAsItWas(rule, first, count, missing, logged)
		}
		if len(now.rules) != len(was.rules) {
			t.Fatalf("seed %d: %d rules are gathered, and %d were", seed, len(now.rules), len(was.rules))
		}
		report := &RuleReport{}
		now.finish(nil, report)
		reported := report.Failures()
		for i, known := range was.rules {
			got, id := now.rules[i], identity{known.rule.Name, known.rule.Expression, known.rule.Items}
			if id != (identity{got.rule.Name, got.rule.Expression, got.rule.Items}) || got.count != known.count || got.missing != known.missing {
				t.Fatalf("seed %d: entry %d is %+v, and was %+v", seed, i, *got, *known)
			}
			if first, twin := joined[id]; twin {
				upgraded++
				if !got.logged || !errors.Is(got.first, first) || got.rule.ErrorMode != model.ErrorModeLog || known.logged {
					t.Errorf("seed %d: the rule %v a twin under log joined is gathered as %+v, want logged with the first error %v", seed, id, *got, first)
				}
				if !reported[i].Logged || !errors.Is(reported[i].First, first) || reported[i].Failures != known.count {
					t.Errorf("seed %d: the rule %v a twin under log joined is reported as %+v", seed, id, reported[i])
				}
				continue
			}
			same++
			if got.logged != known.logged || !errors.Is(got.first, known.first) || got.rule.ErrorMode != known.rule.ErrorMode {
				t.Errorf("seed %d: the rule %v is gathered as %+v, and was %+v", seed, id, *got, *known)
			}
		}
	}
	if same < 500 || upgraded < 100 {
		t.Fatalf("%d rules gathered as they were and %d a twin under log joined: the generator shows too little", same, upgraded)
	}
}
