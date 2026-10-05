//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a rule of a python collector is for, end to end. The script makes
// every series, with the type and the help it gives them; a rule names one
// of them. The rule up, with its label note marked truncate: true, has the
// script's note cut to limits.max_label_value_length; the rule absent, for
// a series the script does not make, is listed in a debug probe's report
// among the rules that gave no series, and fails nothing; and each rule has
// its http_exporter_rule_failures_total series, which stays 0, since a rule
// that makes no series fails none. The script's jobs, which no rule names,
// is served as the script made it and has no such series. Everything else a
// rule could say there the load refuses.
func TestAPythonRuleNamesASeriesOfTheScript(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	scripted := func(rules ...model.MetricRule) model.Collector {
		c := pythonCollector("scripted", "metric(name=\"up\", value=1, labels={\"note\": \"a note of thirty-four bytes in all\"})\nmetric(name=\"jobs\", type=\"counter\", value=3, help=\"Jobs run.\")\n")
		c.Limits.MaxLabelValueLength = 20
		c.Metrics = rules
		return c
	}
	cut := []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}}
	server := modeServer(t, scripted(model.MetricRule{Name: "up", Labels: cut}, model.MetricRule{Name: "absent"}))
	server.SetProbeDebug(true)
	query := "collector=scripted&target=" + url.QueryEscape(target.URL)
	answer := probeOnce(t, server, "/probe?"+query, nil)
	if answer.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", answer.Code, answer.Body)
	}
	assertContains(t, answer.Body.String(), "# TYPE up gauge\n", "up{note=\"a note of thirty-…\"} 1\n", "# HELP jobs Jobs run.\n", "# TYPE jobs counter\n", "jobs 3\n")
	if strings.Contains(answer.Body.String(), "absent") {
		t.Errorf("the rule for a series the script does not make is in the answer:\n%s", answer.Body)
	}
	report := debugProbeGet(t, server, query+"&debug=true").Body.String()
	assertContains(t, report, "A probe would have answered 200 with 2 series", "Series by metric", "up: 1", "jobs: 1", "Rules that gave no series: absent\n")
	if strings.Contains(report, "Rules that carried on without some series") {
		t.Errorf("a python rule is reported as having failed:\n%s", report)
	}
	exposition := selfMetrics(t, server)
	for _, series := range []string{`http_exporter_rule_failures_total{collector="scripted",metric="up"}`, `http_exporter_rule_failures_total{collector="scripted",metric="absent"}`} {
		if got := seriesValue(t, exposition, series); got != 0 {
			t.Errorf("%s = %v, want 0", series, got)
		}
	}
	if strings.Contains(exposition, `metric="jobs"`) {
		t.Errorf("the script's series that no rule names has a rule's series:\n%s", exposition)
	}
	// Without the rule that cuts it, the scrape fails for the label.
	if answer := probeOnce(t, modeServer(t, scripted()), "/probe?"+query, nil); answer.Code != http.StatusBadGateway {
		t.Errorf("without a rule to cut the label the probe is answered %d: %s", answer.Code, answer.Body)
	}
	// What a rule could say besides is refused when the collector loads.
	yes := true
	for what, rule := range map[string]model.MetricRule{
		"sets type":                {Name: "up", Type: model.CounterMetricType},
		"sets description":         {Name: "up", Description: "Whether it is up."},
		"sets required":            {Name: "up", Required: &yes},
		"sets error_mode":          {Name: "absent", ErrorMode: model.ErrorModeFail},
		"has no name":              {Labels: cut},
		"does not set truncate":    {Name: "up", Labels: []model.LabelRule{{Name: "note", Expression: "note"}}},
		"sets value, which a pyth": {Name: "up", Labels: []model.LabelRule{{Name: "site", Value: "rack1"}}},
	} {
		if err := config.Validate(&model.Config{Collectors: []model.Collector{scripted(rule)}}); err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("a rule that %s: %v", what, err)
		}
	}
}
