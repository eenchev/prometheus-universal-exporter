//go:build !select_request_types || request_type_sql

package exporter

import (
	"context"
	"database/sql/driver"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe of a sql collector whose csv rule takes a label from a column
// NULL in every row answers its series with the label left off, as a
// column NULL in some rows leaves it off theirs: the statement returned the
// column, though no row has a value of it.
func TestASQLLabelColumnNullInEveryRowIsLeftOff(t *testing.T) {
	db := &sqltest.DB{Results: map[string]sqltest.Result{
		"SELECT state, n, note FROM jobs": {Columns: []string{"state", "n", "note"}, Rows: [][]driver.Value{{"open", int64(3), nil}, {"done", int64(40), nil}}},
	}}
	t.Cleanup(fetch.UseSQLTestDriver(func(ctx context.Context, _ fetch.SQLConnectInfo) (driver.Conn, error) { return db.Connect(ctx) }))
	t.Cleanup(fetch.CloseConnections)
	dir := t.TempDir()
	password := testutil.WriteIn(t, dir, "password", "s3cret\n")
	path := testutil.WriteIn(t, dir, "config.yaml", `collectors:
  - name: jobs
    request:
      type: sql
      driver: postgres
      username: metrics
      password_file: `+password+`
      statement: SELECT state, n, note FROM jobs
    transform:
      type: csv
    metrics:
      - name: jobs
        expression: n
        labels:
          - name: state
            expression: state
          - name: note
            expression: note
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := probeOnce(t, server, "/probe?collector=jobs&target=db.example", nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `jobs{state="open"} 3`) || !strings.Contains(body, `jobs{state="done"} 40`) {
		t.Errorf("%d %s", recorder.Code, body)
	}
}
