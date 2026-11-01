//go:build !select_request_types || request_type_sql

package exporter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The debug page shows a configured collector's folded block scalars as its
// file writes them: a statement written >- over two lines is shown >- over
// those two lines, a > with a blank line and a more indented line keeps
// them, and a literal | its own; read again, each is what the file's is.
func TestTheDebugPageShowsFoldedBlocksAsWritten(t *testing.T) {
	server := debugFormServer(t, `  - name: pg_databases
    request:
      type: sql
      driver: postgres
      username: postgres
      password_file: `+testutil.WriteFile(t, "password", "pw\n")+`
      statement: >-
        SELECT datname, numbackends, xact_commit, blks_hit
        FROM pg_stat_database WHERE datname IS NOT NULL
    transform:
      type: csv
    metrics:
      - name: pg_database_backends
        description: >
          Backends of each database,

            as pg_stat_database counts them
        expression: numbackends
        labels:
          - name: datname
            expression: datname
      - name: pg_database_xact_commit_total
        description: |
          Transactions committed,
          as pg_stat_database counts them
        type: counter
        expression: xact_commit
`)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/collector?name=pg_databases", nil))
	var answer debugCollectorAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{
		"  statement: >-\n    SELECT datname, numbackends, xact_commit, blks_hit\n    FROM pg_stat_database WHERE datname IS NOT NULL\n",
		"  - name: pg_database_backends\n    description: >\n      Backends of each database,\n\n        as pg_stat_database counts them\n    expression: numbackends\n",
		"    description: |\n      Transactions committed,\n      as pg_stat_database counts them\n",
	} {
		if !strings.Contains(answer.YAML, want) {
			t.Errorf("the YAML shown has no\n%s\nin\n%s", want, answer.YAML)
		}
	}
	if strings.Contains(answer.YAML, shownBlock) {
		t.Errorf("the YAML shown has a stand-in left in it:\n%s", answer.YAML)
	}
	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/debug?collector=pg_databases", nil))
	if !strings.Contains(page.Body.String(), "  statement: &gt;-\n    SELECT datname, numbackends, xact_commit, blks_hit\n    FROM pg_stat_database") {
		t.Errorf("the page does not open with the statement over its lines")
	}
	// Read as a collector, the lines withheld left out, it is the file's.
	shown := strings.Replace(answer.YAML, "  password_file: <redacted>\n", "", 1)
	cfg, err := config.ParseCollector([]byte(shown), nil)
	if err != nil {
		t.Fatalf("the YAML shown does not read as a collector: %v\n%s", err, shown)
	}
	got := &cfg.Collectors[0]
	followed := server.reconcile()
	configured := followed.collectorOf(followed.config, "pg_databases")
	for i := range configured.Metrics {
		if got.Metrics[i].Description != configured.Metrics[i].Description {
			t.Errorf("read again, metric %d's description is %q, want %q", i, got.Metrics[i].Description, configured.Metrics[i].Description)
		}
	}
	if a, b := got.Request.Own.(fetch.SQLConfig).Statement, configured.Request.Own.(fetch.SQLConfig).Statement; a != b {
		t.Errorf("read again, the statement is %q, want %q", a, b)
	}
}

// A statement, written as a folded block, and an argument that hold a
// credential are withheld whole, the statement no longer shown over its
// lines.
func TestTheDebugPageWithholdsAStatementHoldingACredential(t *testing.T) {
	requireFreeTextWithheld(t, `  - name: statement
    request:
      type: sql
      driver: postgres
      username: monitor
      statement: >-
        SELECT count(*) FROM sessions
        WHERE token = 'Bearer s3cret-statement'
    transform: {type: csv}
    metrics: [{name: sessions, expression: count}]
  - name: args
    request:
      type: sql
      driver: postgres
      username: monitor
      statement: SELECT count(*) FROM sessions WHERE note = $1
      args: ['password=s3cret-arg']
    transform: {type: csv}
    metrics: [{name: sessions, expression: count}]
`)
}
