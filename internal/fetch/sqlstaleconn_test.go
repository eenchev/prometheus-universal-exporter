//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
)

// A PostgreSQL server that ends an idle session, by idle_session_timeout or
// an administrator's pg_terminate_backend, leaves its last words on the
// socket, and pgx reads them as the answer to the next BEGIN: the scrape
// that took the connection from the pool failed with SQLSTATE 57P01 or
// 57P05, and with a timeout shorter than the scrape interval every other
// scrape did. A transaction that did not begin has run nothing, so the
// scrape now closes the connection and takes another, of one statement and
// of several; a new connection whose transaction does not begin fails the
// scrape as before, and so does a statement that fails once begun.
func TestAnIdleSQLConnectionTheServerEndedIsReplaced(t *testing.T) {
	ended := errors.New("FATAL: terminating connection due to idle-session timeout (SQLSTATE 57P05)")
	for name, change := range map[string]func(*model.RequestConfig){
		"one statement": func(*model.RequestConfig) {},
		"several statements": func(r *model.RequestConfig) {
			own := r.Own.(SQLConfig)
			own.Statement = ""
			own.Statements = []SQLStatement{{Name: "a", Statement: "SELECT 1"}, {Name: "b", Statement: "SELECT 1"}}
			r.Own = own
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSQLFixture(t)
			f.db.Results["SELECT 1"] = sqltest.Result{Columns: []string{"n"}, Rows: [][]driver.Value{{int64(1)}}}
			c := f.collector(t, "jobs", SQLDriverPostgres, "SELECT 1", change)
			scrape := func() error {
				_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
				return err
			}
			if err := scrape(); err != nil {
				t.Fatal(err)
			}
			f.db.EndSessions(ended)
			if err := scrape(); err != nil {
				t.Fatalf("the scrape that found the ended connection: %v", err)
			}
			if opened, closed := f.db.Opened(), f.db.Closed(); opened != 2 || closed != 1 {
				t.Errorf("%d connections made and %d closed, want the ended one closed and one made in its place", opened, closed)
			}
			if err := scrape(); err != nil || f.db.Opened() != 2 {
				t.Errorf("the next scrape: %v, %d connections made", err, f.db.Opened())
			}

			// Every connection fails to begin, a new one too: the scrape
			// fails with what the new one said, after the one it found.
			f.db.BeginErr = errors.New("the database is starting up")
			f.db.EndSessions(ended)
			if err := scrape(); err == nil || !strings.Contains(err.Error(), "the database is starting up") || strings.Contains(err.Error(), "57P05") {
				t.Errorf("with no transaction beginning: %v", err)
			}
			if opened := f.db.Opened(); opened != 3 {
				t.Errorf("%d connections made, want one more, tried once", opened)
			}
			f.db.BeginErr = nil

			// A statement that fails after its transaction began is not
			// run again.
			if err := scrape(); err != nil {
				t.Fatal(err)
			}
			before := len(f.db.Queries())
			f.db.Results["SELECT 1"] = sqltest.Result{Err: errors.New("relation does not exist")}
			if err := scrape(); err == nil || !strings.Contains(err.Error(), "relation does not exist") {
				t.Errorf("a statement that fails: %v", err)
			}
			if ran := len(f.db.Queries()) - before; ran != 1 {
				t.Errorf("the failing statement ran %d times", ran)
			}
		})
	}
}
