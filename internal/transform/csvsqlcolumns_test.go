package transform

import (
	"fmt"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transformSQLRows runs the csv transform over rows a sql statement
// returned with columns, as transformCSVRows runs it over rows alone.
func transformSQLRows(t *testing.T, rows []any, columns []string, rules ...model.MetricRule) csvRowsRun {
	t.Helper()
	c := model.Collector{Name: "rows", Transform: model.TransformConfig{Type: "csv"}, Metrics: slices.Clone(rules)}
	for i := range c.Metrics {
		c.Metrics[i].ErrorMode = model.ErrorModeLog
		c.Metrics[i].Type = model.GaugeMetricType
	}
	ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(t.Context()))
	ctx, failures := withRuleFailures(ctx)
	set, err := transformCSVColumns(ctx, rows, columns, c.Metrics, &c)
	failures.finish(&c, report)
	run := csvRowsRun{err: err}
	if set != nil {
		for _, m := range set.Metrics {
			run.series = append(run.series, seriesLine(m))
		}
	}
	for _, f := range report.Failures() {
		run.failures = append(run.failures, fmt.Sprintf("%s: %d failed, %d missing: %v", f.Metric, f.Failures, f.Missing, f.First))
	}
	return run
}

// A sql statement's column NULL in every row is in the response, though no
// row has it: a label read from it is left off every series, as one NULL in
// some rows is left off theirs, and a value read from it is an empty cell
// in each row, as NULL in some rows is in those. A column the statement did
// not return is still not in the response, which lists the NULL column
// among its columns.
func TestASQLColumnNullInEveryRowIsInTheResponse(t *testing.T) {
	columns := []string{"host", "n", "region"}
	labelled := model.MetricRule{Name: "m", Expression: "n", Labels: []model.LabelRule{{Name: "host", Expression: "host"}, {Name: "region", Expression: "region"}}}
	some := transformSQLRows(t, []any{map[string]any{"host": "a", "n": 1, "region": "eu"}, map[string]any{"host": "b", "n": 2}}, columns, labelled)
	some.holds(t, []string{`m{host="a",region="eu"} 1`, `m{host="b"} 2`})
	all := transformSQLRows(t, []any{map[string]any{"host": "a", "n": 1}, map[string]any{"host": "b", "n": 2}}, columns, labelled)
	all.holds(t, []string{`m{host="a"} 1`, `m{host="b"} 2`})
	value := model.MetricRule{Name: "v", Expression: "region"}
	empty := transformSQLRows(t, []any{map[string]any{"host": "a", "n": 1}}, columns, value)
	empty.holds(t, nil, `v: 1 failed, 1 missing: CSV column "region" is empty in row 1`)
	absent := transformSQLRows(t, []any{map[string]any{"host": "a", "n": 1}}, columns, model.MetricRule{Name: "w", Expression: "zone"})
	absent.holds(t, nil, `w: 1 failed, 1 missing: CSV column "zone" is not in the response, whose columns are "host", "n", "region"; column names are matched exactly`)
}
