//go:build !select_request_types || request_type_localfile

package exporter

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The rules of a directory's collector are logged for each file by
// themselves, and so are prometheus rules without a name, which have no
// metric to be told by: their lines carry an empty metric, as they did, and
// the expression, since the collector has two of them. A rule that starts to
// fail on a file beside one that has been failing on it is logged in full,
// each recovers by itself, and what another file's rules do changes nothing
// of it. The values the rules missed are counted as they were.
func TestADirectorysRulesWithoutANameAreLoggedEachByItself(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "app_jobs_total 7\napp_workers 2\n")
	testutil.WriteIn(t, root, "b.prom", "app_queue_length 3\napp_workers 1\n")
	c := dirCollector("dir", root, "*.prom")
	c.Metrics = []model.MetricRule{
		{Expression: "^app_jobs", ErrorMode: model.ErrorModeLog},
		{Expression: "^app_queue", ErrorMode: model.ErrorModeLog},
		{Name: "workers", Expression: "^app_workers$", ErrorMode: model.ErrorModeLog},
	}
	server := fileServer(t, c)
	logs := debugLogs(server)
	for _, step := range []struct {
		a    string
		want []string
	}{
		{"app_jobs_total 7\napp_workers 2\n", []string{
			`WARN metric extraction failed a.prom "" ^app_queue`,
			`WARN metric extraction failed b.prom "" ^app_jobs`,
		}},
		{"app_workers 2\n", []string{
			`WARN metric extraction failed a.prom "" ^app_jobs`,
			`DEBUG metric extraction failed a.prom "" ^app_queue`,
			`DEBUG metric extraction failed b.prom "" ^app_jobs`,
		}},
		{"app_jobs_total 7\napp_queue_length 0\n", []string{
			`WARN metric extraction failed a.prom "workers" <nil>`,
			`INFO metric extraction recovered a.prom "" ^app_jobs`,
			`INFO metric extraction recovered a.prom "" ^app_queue`,
			`DEBUG metric extraction failed b.prom "" ^app_jobs`,
		}},
	} {
		if err := os.WriteFile(filepath.Join(root, "a.prom"), []byte(step.a), 0o600); err != nil {
			t.Fatal(err)
		}
		logs.Reset()
		probeFile(t, server, "collector=dir").must(t, http.StatusOK)
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("the log line %q is not JSON: %v", line, err)
			}
			if message, _ := record["msg"].(string); strings.HasPrefix(message, "metric extraction ") {
				metric, _ := json.Marshal(record["metric"])
				got = append(got, strings.Join([]string{record["level"].(string), message, record["file"].(string), string(metric)}, " ")+" "+toText(record["expression"]))
			}
		}
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("with a.prom %q the rules' lines are\n%s\nwant\n%s\nthe log:\n%s", step.a, strings.Join(got, "\n"), strings.Join(step.want, "\n"), logs)
		}
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_rule_failures_total{collector="dir",metric="workers"}`: 1,
		`http_exporter_missing_keys_total{collector="dir"}`:                   7,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if strings.Contains(exposition, `http_exporter_rule_failures_total{collector="dir",metric=""}`) {
		t.Errorf("the rules without a name have a series of their failures:\n%s", exposition)
	}
}

// toText is a line's attribute as a test names it: its text, or <nil> when
// the line has none.
func toText(attribute any) string {
	if text, ok := attribute.(string); ok {
		return text
	}
	return "<nil>"
}
