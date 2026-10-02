package transform

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// absentCase is a required rule whose value the response does not hold in a
// way that left the rule unnoticed: a prometheus rule no series matches, and
// a csv rule over a response without a row. want is the missing value's
// wording.
type absentCase struct {
	name              string
	collector         model.Collector
	contentType, body string
	want              string
}

func absentCases() []absentCase {
	prometheus := func(rule model.MetricRule) model.Collector {
		return model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{rule}}
	}
	csv := model.Collector{Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"}, Metrics: []model.MetricRule{{Name: "cpu", Expression: "cpu"}}}
	const exposition = "# TYPE other gauge\nother 1\n"
	return []absentCase{
		{"prometheus rule by name", prometheus(model.MetricRule{Name: "service_up"}), "text/plain; version=0.0.4", exposition,
			`metric "service_up" is not in the response`},
		{"prometheus rule by expression", prometheus(model.MetricRule{Name: "up", Expression: "^service_"}), "text/plain; version=0.0.4", exposition,
			`metric "up" expression "^service_" matched no metric in the response`},
		{"prometheus rule without a name", prometheus(model.MetricRule{Expression: "^service_"}), "text/plain; version=0.0.4", exposition,
			`expression "^service_" matched no metric in the response`},
		{"prometheus rule over an empty exposition", prometheus(model.MetricRule{Name: "service_up"}), "text/plain; version=0.0.4", "",
			`metric "service_up" is not in the response`},
		{"csv with a header and no rows", csv, "text/csv", "server,cpu\n",
			`CSV column "cpu" is missing: the response has no rows`},
		{"csv with an empty body", csv, "text/csv", "",
			`CSV column "cpu" is missing: the response has no rows`},
	}
}

// A required rule that finds nothing to read is missing its value in every
// transform, and its error mode decides what follows. A prometheus rule that
// matched no series, and a csv rule over a response with no rows, were
// skipped without a word instead, so error_mode: fail never fired for the
// metric a scrape was meaningless without, and nothing was counted.
func TestARequiredRuleWithNothingToReadIsMissingItsValue(t *testing.T) {
	for _, tc := range absentCases() {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			c := tc.collector
			c.Name = "absent"

			// fail fails the scrape, as a missing value of that rule.
			c.Metrics[0].ErrorMode = model.ErrorModeFail
			set, failures, err := transformWith(context.Background(), t, c, tc.contentType, tc.body)
			var failure *MetricFailure
			if set != nil || !errors.As(err, &failure) || failure.Metric != c.Metrics[0].Name || !errors.Is(err, model.ErrMissingValue) || err.Error() != tc.want {
				t.Fatalf("fail: series %+v, err %v; want the rule's failure %q", set, err, tc.want)
			}
			if len(failures) != 0 {
				t.Fatalf("fail: counted as carried on: %+v", failures)
			}

			// log carries on, logs the rule once and counts a missing value.
			logs.Reset()
			c.Metrics[0].ErrorMode = model.ErrorModeLog
			set, failures, err = transformWith(context.Background(), t, c, tc.contentType, tc.body)
			if err != nil || len(set.Metrics) != 0 {
				t.Fatalf("log: series %+v, err %v", set, err)
			}
			if len(failures) != 1 || failures[0].Failures != 1 || failures[0].Missing != 1 || !failures[0].Logged || failures[0].First.Error() != tc.want {
				t.Fatalf("log: failures %+v", failures)
			}
			if records := testutil.AssertJSONLines(t, logs, 1); records[0]["error"] != tc.want || records[0]["collector"] != "absent" {
				t.Fatalf("log: logged %v", records)
			}

			// ignore carries on in silence, and counts it all the same.
			logs.Reset()
			c.Metrics[0].ErrorMode = model.ErrorModeIgnore
			set, failures, err = transformWith(context.Background(), t, c, tc.contentType, tc.body)
			if err != nil || len(set.Metrics) != 0 || logs.Len() != 0 {
				t.Fatalf("ignore: series %+v, err %v, logs %s", set, err, logs)
			}
			if len(failures) != 1 || failures[0].Missing != 1 || failures[0].Logged {
				t.Fatalf("ignore: failures %+v", failures)
			}
		})
	}
}

// What makes a rule optional elsewhere makes it optional here: with
// required: false, or a collector with allow_missing_keys, a rule with
// nothing to read is left out, under fail too, and nothing is counted.
func TestAnOptionalRuleWithNothingToReadIsLeftOut(t *testing.T) {
	optional := false
	for _, tc := range absentCases() {
		for name, relax := range map[string]func(*model.Collector){
			"required: false":    func(c *model.Collector) { c.Metrics[0].Required = &optional },
			"allow_missing_keys": func(c *model.Collector) { c.ErrorHandling.AllowMissingKeys = true },
		} {
			t.Run(tc.name+" with "+name, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				c := tc.collector
				c.Name = "absent"
				// Rules of its own, so one way of relaxing them is not
				// left in place for the other.
				c.Metrics = slices.Clone(c.Metrics)
				c.Metrics[0].ErrorMode = model.ErrorModeFail
				relax(&c)
				set, failures, err := transformWith(context.Background(), t, c, tc.contentType, tc.body)
				if err != nil || len(set.Metrics) != 0 || len(failures) != 0 || logs.Len() != 0 {
					t.Fatalf("series %+v, failures %+v, err %v, logs %s", set, failures, err, logs)
				}
			})
		}
	}
}

// A rule is missing its value only when nothing matched it: the rules beside
// it that did match pass their series on, a rule whose series were dropped
// for another reason is not missing, and a prometheus transform without
// rules, which passes on whatever there is, has nothing that can be missing.
func TestOnlyUnmatchedRulesAreMissing(t *testing.T) {
	testutil.CaptureLogs(t)
	const exposition = "# TYPE up gauge\nup 1\n# TYPE lat histogram\nlat_bucket{le=\"+Inf\"} 2\nlat_sum 3\nlat_count 2\n"
	c := model.Collector{Name: "mixed", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{
			{Name: "up", ErrorMode: model.ErrorModeFail},
			{Name: "absent", ErrorMode: model.ErrorModeLog},
			// Matched, and then refused: a histogram cannot become a gauge.
			{Name: "lat", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog},
		}}
	set, failures, err := transformWith(context.Background(), t, c, "text/plain; version=0.0.4", exposition)
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Name != "up" {
		t.Fatalf("series %+v, err %v", set, err)
	}
	if len(failures) != 2 || failures[0].Metric != "lat" || failures[0].Missing != 0 || failures[1].Metric != "absent" || failures[1].Missing != 1 {
		t.Fatalf("failures %+v", failures)
	}
	c.Metrics = nil
	set, failures, err = transformWith(context.Background(), t, c, "text/plain; version=0.0.4", "")
	if err != nil || len(set.Metrics) != 0 || len(failures) != 0 {
		t.Fatalf("passthrough of nothing: %+v, %+v, %v", set, failures, err)
	}
	// A csv rule over rows is read row by row, as before.
	rows := model.Collector{Name: "rows", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"},
		Metrics: []model.MetricRule{{Name: "cpu", Expression: "cpu", ErrorMode: model.ErrorModeFail}}}
	set, _, err = transformWith(context.Background(), t, rows, "text/csv", "server,cpu\nweb01,72\n")
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 72 {
		t.Fatalf("csv rows: %+v, %v", set, err)
	}
}
