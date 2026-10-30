//go:build !select_request_types || request_type_sql

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// debugPgValues is a sql collector of a postgres server whose statement takes
// a probe parameter with a default.
func debugPgValues(t *testing.T) string {
	return `  - name: pg_values
    request:
      type: sql
      driver: postgres
      username: postgres
      password_file: ` + testutil.WriteFile(t, "password", "pw\n") + `
      statement: SELECT state, jobs FROM jobs WHERE jobs >= $1::int
      args: ["{{param_min:0}}"]
    transform:
      type: jq
    metrics:
      - name: demo_jobs
        items: .[]
        expression: .jobs
        labels:
          - name: state
            expression: .state
`
}

// The debug page asks a run of a sql collector for the database server, with
// the driver's port, and its own probe parameter; of a pasted response, the
// rows as JSON alone, with no Content-Type, status or headers, which an
// answer of rows has none of.
func TestTheDebugPageAsksASQLCollectorForItsServerAndItsRows(t *testing.T) {
	server := debugFormServer(t, debugPgValues(t))
	page, form := debugFormPage(t, server, "pg_values")
	want := debugForm{
		RequestType: "sql", Answer: "Paste the rows as JSON, a list of objects of each row's columns by name, as the Response of a run shows them",
		TargetLabel: "Database server", TargetHint: "host or host:5432", Params: "param_min=0",
	}
	if form != want {
		t.Errorf("the form is %+v, want %+v", form, want)
	}
	requireDebugForm(t, page, form)
}

// A response pasted for a sql collector is its rows, as JSON, which its
// transform reads as it reads the rows a driver read; anything else is
// refused, saying what to paste.
func TestTheDebugPageReadsAPastedSQLResponseAsTheRows(t *testing.T) {
	server := debugFormServer(t, debugPgValues(t))
	post := func(body string) *httptest.ResponseRecorder {
		run, _ := json.Marshal(map[string]any{"configured": "pg_values", "body": body})
		request := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(run))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Debug-Run", "run")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	recorder := post(`[{"state": "open", "jobs": 3}, {"state": "done", "jobs": 40, "note": null}, {"state": "late", "jobs": true}]`)
	var answer debugAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK || !answer.OK {
		t.Fatalf("%d %v: %s", recorder.Code, err, recorder.Body)
	}
	if want := "# TYPE demo_jobs gauge\ndemo_jobs{state=\"open\"} 3\ndemo_jobs{state=\"done\"} 40\ndemo_jobs{state=\"late\"} 1\n"; answer.Series != want {
		t.Errorf("the series are %q, want %q", answer.Series, want)
	}
	for body, refusal := range map[string]string{
		`{"state": "open"}`:           "cannot unmarshal object",
		`[{"state": {"a": 1}}]`:       `row 1's state is a list or an object`,
		`[{"jobs": 1}] [{"jobs": 2}]`: "more follows the list",
		`[null]`:                      "row 1 is not an object",
	} {
		recorder := post(body)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "paste them as JSON, a list of objects") || !strings.Contains(recorder.Body.String(), refusal) {
			t.Errorf("%s: %d %s, want 400 with %q", body, recorder.Code, recorder.Body, refusal)
		}
	}
}
