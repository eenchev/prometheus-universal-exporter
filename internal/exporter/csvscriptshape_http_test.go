//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A csv pre-script that leaves something other than rows, or a row that is
// none, failed the transform as the script's doing, in words, and was not
// counted as one: http_exporter_script_errors_total stayed where it was,
// while the same mistake before a css rule moved it. Both are counted now,
// once a scrape, as a transform failure too.
func TestACSVPreScriptLeavingTheWrongShapeIsCountedAsAScriptError(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/html" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<p>1</p>"))
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("a,b\n1,2\n"))
	}))
	t.Cleanup(target.Close)
	scripted := func(name, kind, transform, path, script, expression string) model.Collector {
		return model.Collector{
			Name: name, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP, Path: path}, Decoder: model.DecoderConfig{Type: kind},
			Transform: model.TransformConfig{Type: transform, PreScript: script},
			Metrics:   []model.MetricRule{{Name: "m", Type: model.GaugeMetricType, Expression: expression}},
		}
	}
	server := verboseServer(t, false,
		scripted("csv_not_rows", "csv", "csv", "/csv", `data = {"a": 1}`, "a"),
		scripted("csv_not_a_row", "csv", "csv", "/csv", `data = [{"a": 1}, "x"]`, "a"),
		scripted("css_not_markup", "html", "css", "/html", `data = {"a": 1}`, "p"),
		scripted("csv_rows", "csv", "csv", "/csv", `data = [{"a": row["a"]} for row in data]`, "a"),
	)
	for collector, want := range map[string]string{
		"csv_not_rows":   "python pre-script of a csv transform left data as an object with 1 key; it must leave a list of rows",
		"csv_not_a_row":  `python pre-script of a csv transform left row 2 as "x"; a row must be a dict by column name or a list by column number`,
		"css_not_markup": "python pre-script of a css transform must leave data as a string of HTML",
		"csv_rows":       "m 1\n",
	} {
		answer := probeOnce(t, server, probePath(collector, target.URL, ""), nil)
		if (answer.Code == http.StatusOK) != (collector == "csv_rows") || !strings.Contains(answer.Body.String(), want) {
			t.Fatalf("%s: answered %d %q, want it to say %q", collector, answer.Code, answer.Body, want)
		}
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_script_errors_total{collector="csv_not_rows"}`:      1,
		`http_exporter_script_errors_total{collector="csv_not_a_row"}`:     1,
		`http_exporter_script_errors_total{collector="css_not_markup"}`:    1,
		`http_exporter_script_errors_total{collector="csv_rows"}`:          0,
		`http_exporter_transform_errors_total{collector="csv_not_rows"}`:   1,
		`http_exporter_transform_errors_total{collector="csv_not_a_row"}`:  1,
		`http_exporter_transform_errors_total{collector="css_not_markup"}`: 1,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}
