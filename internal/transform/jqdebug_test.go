package transform

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// In a debug probe, whose context carries its report's logger, each value
// a jq expression gives debug or stderr is a line of the report: the
// function, the rule, the expression and the value as JSON, debug(message)
// telling its message. The series are what they are without it. A long
// value is cut, and past a hundred lines one line says the rest is left
// out. A scrape that is no debug probe writes nothing.
func TestDebugAndStderrTellADebugProbesReport(t *testing.T) {
	const body = `{"items":[{"name":"a","v":1,"tags":["x","y"]},{"name":"b","v":2,"tags":[]}],"long":"` + "LONG" + `"}`
	document := strings.Replace(body, "LONG", strings.Repeat("é", 2000), 1)
	c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{
		{Name: "v", Items: ".items[] | debug", Expression: `.v | debug("the value of \(.)") | . * 10`,
			Labels: []model.LabelRule{{Name: "name", Expression: ".name | stderr"}, {Name: "plain", Expression: ".name"}}},
		{Name: "", Expression: ".long | debug | length"},
		{Name: "many", Expression: "[range(150) | debug] | length"},
	}}
	var report bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&report, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	set, _, err := transformWith(WithRuleLogger(context.Background(), logger), t, c, "application/json", document)
	if err != nil || len(set.Metrics) != 4 || set.Metrics[0].Value != 10 || set.Metrics[1].Labels["name"] != "b" || set.Metrics[3].Value != 150 {
		t.Fatalf("series %+v, err %v", set, err)
	}
	lines := strings.Split(strings.TrimSpace(report.String()), "\n")
	want := []string{
		`level=INFO msg="jq debug" rule="metric \"v\"" expression=".items[] | debug" value="{\"name\":\"a\",\"tags\":[\"x\",\"y\"],\"v\":1}"`,
		`level=INFO msg="jq debug" rule="metric \"v\"" expression=".v | debug(\"the value of \\(.)\") | . * 10" value="\"the value of 1\""`,
		`level=INFO msg="jq stderr" rule="metric \"v\"" expression=".name | stderr" value="\"a\""`,
		`level=INFO msg="jq debug" rule="metric \"v\"" expression=".items[] | debug" value="{\"name\":\"b\",\"tags\":[],\"v\":2}"`,
		`level=INFO msg="jq debug" rule="metric \"v\"" expression=".v | debug(\"the value of \\(.)\") | . * 10" value="\"the value of 2\""`,
		`level=INFO msg="jq stderr" rule="metric \"v\"" expression=".name | stderr" value="\"b\""`,
	}
	for i, line := range want {
		if i >= len(lines) || lines[i] != line {
			t.Fatalf("line %d of the report is\n%s\nwant\n%s\nof\n%s", i, lines[min(i, len(lines)-1)], line, report.String())
		}
	}
	if long := lines[6]; !strings.HasPrefix(long, `level=INFO msg="jq debug" rule="metrics rule 2" expression=".long | debug | length" value="\"`+strings.Repeat("é", 100)) || !strings.HasSuffix(long, `... (4002 bytes)"`) || len(long) > 1300 {
		t.Errorf("the long value's line is %d bytes: %.200s ... %s", len(long), long, long[len(long)-40:])
	}
	if len(lines) != 101 || lines[99] != `level=INFO msg="jq debug" rule="metric \"many\"" expression="[range(150) | debug] | length" value=92` ||
		lines[100] != `level=INFO msg="the transform's expressions called debug or stderr more often; the rest is left out" shown=100` {
		t.Errorf("%d lines, ending\n%s\n%s", len(lines), lines[len(lines)-2], lines[len(lines)-1])
	}

	logs := testutil.CaptureLogs(t)
	if set, _, err := transformWith(context.Background(), t, c, "application/json", document); err != nil || len(set.Metrics) != 4 || logs.Len() != 0 {
		t.Errorf("a scrape: series %+v, err %v, logged %s", set, err, logs)
	}
}
