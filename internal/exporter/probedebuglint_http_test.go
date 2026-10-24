//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// lintBody is what lintCollector reads: a session with a UUID, and a count.
const lintBody = `{"sessions":[{"id":"5f0c6c2e-8a5b-4c1e-9d3f-2b7a1e0c9d48","n":2}],"jobs":7}`

// lintCollector makes a counter not named _total and a gauge labelled by
// session ids; lintOther makes the counter's name as a gauge.
func lintCollector() model.Collector {
	return model.Collector{
		Name:      "app",
		Request:   model.RequestConfig{Type: "http"},
		Decoder:   model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{
			{Name: "jobs", Type: model.CounterMetricType, Expression: ".jobs"},
			{Name: "sessions", Items: ".sessions[]", Expression: ".n", Labels: []model.LabelRule{{Name: "session", Expression: ".id"}}},
		},
	}
}

func lintOther() model.Collector {
	return model.Collector{
		Name: "other", Request: model.RequestConfig{Type: "http"}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "jobs", Expression: ".jobs"}},
	}
}

// A debug probe's report has a Lint section, after the logs and before the
// metrics, with the advice on the collector and on the series the run made,
// each finding by its rule and metric, and the probe's answer is the same
// with it as a probe without debug gives. lint.ignore takes its rules out
// and the section says which.
func TestADebugProbeReportsTheMetricLint(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lintBody))
	}))
	t.Cleanup(target.Close)
	query := "collector=app&target=" + url.QueryEscape(target.URL)

	server := modeServer(t, lintCollector(), lintOther())
	server.SetDebug(DebugFull)
	plain := debugProbeGet(t, server, query)
	if plain.Code != http.StatusOK {
		t.Fatalf("the probe answered %d: %s", plain.Code, plain.Body)
	}
	body := debugProbeGet(t, server, query+"&debug=true").Body.String()
	assertContains(t, body,
		"\nLint\n  [counter-total] metric \"jobs\": a counter whose name does not end in _total",
		"\n  [name-clash] metric \"jobs\": collector \"other\" makes it too, as a gauge, where this collector makes a counter",
		"\n  [unbounded-label] metric \"sessions\": the label session has a value shaped as an id or a time: \"5f0c6c2e-8a5b-4c1e-9d3f-2b7a1e0c9d48\" is a UUID",
	)
	logs, lint, metrics := strings.Index(body, "\nLogs\n"), strings.Index(body, "\nLint\n"), strings.Index(body, "\nMetrics a probe would have served\n")
	if logs < 0 || lint < logs || metrics < lint {
		t.Fatalf("the sections are out of order:\n%s", body)
	}
	if !strings.HasSuffix(body, "\nMetrics a probe would have served\n"+plain.Body.String()) {
		t.Fatalf("the report's metrics differ from the probe's answer %q:\n%s", plain.Body, body)
	}
	if !strings.Contains(body, "A probe would have answered 200 with 2 series.") {
		t.Fatalf("the verdict changed:\n%s", body)
	}

	silenced := lintCollector()
	silenced.Lint.Ignore = []string{model.LintCounterTotal, model.LintNameClash, model.LintUnboundedLabel}
	server = modeServer(t, silenced, lintOther())
	server.SetDebug(DebugFull)
	body = debugProbeGet(t, server, query+"&debug=true").Body.String()
	assertContains(t, body, "\nLint\n  none\n  Not applied, as lint.ignore asks: counter-total, name-clash, unbounded-label\n")
}

// The debug page answers the same advice as JSON, under lint, for a
// collector written on the page, compared with the configuration's others,
// and shows it.
func TestTheDebugPageShowsTheMetricLint(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{lintOther()}})
	written := "name: app\nrequest: {type: http}\ndecoder: {type: json}\ntransform: {type: jq}\nmetrics:\n  - {name: jobs, type: counter, expression: .jobs}\n"
	answer := postRun(t, server, pasted(written, lintBody), http.StatusOK)
	if !answer.OK || len(answer.Lint) != 2 {
		t.Fatalf("%+v", answer)
	}
	if f := answer.Lint[0]; f.Collector != "app" || f.Rule != model.LintCounterTotal || f.Metric != "jobs" || f.Message == "" {
		t.Errorf("%+v", f)
	}
	if f := answer.Lint[1]; f.Rule != model.LintNameClash || !strings.Contains(f.Message, `collector "other" makes it too`) {
		t.Errorf("%+v", f)
	}
	if !strings.Contains(answer.Report, "\nLint\n  [counter-total] metric \"jobs\"") {
		t.Errorf("report:\n%s", answer.Report)
	}
	page := getPath(server, http.MethodGet, "/debug").Body.String()
	if !strings.Contains(page, `<pre id="lint"></pre>`) || !strings.Contains(page, `answer.lint`) {
		t.Errorf("the page does not show the advice")
	}
}
