//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
)

// checkServerAnswer checks that the driver named driverName recognises
// serverErr, the server's own error, by code, wrapped as a driver wraps it
// or not, and that marked (sqlServerAnswer) its text is the whole message
// while a probe's caller is told the code alone, the error still being what
// it was to errors.As.
func checkServerAnswer(t *testing.T, driverName string, serverErr error, code string) {
	t.Helper()
	d := sqlDriverTable[driverName]
	for _, err := range []error{serverErr, fmt.Errorf("the driver's words: %w", serverErr)} {
		got, ok := d.serverError(err)
		if !ok || got != code {
			t.Errorf("%s: %v recognised as %q, %v, want %q", driverName, err, got, ok, code)
		}
		marked := d.sqlServerAnswer(err)
		if marked.Error() != err.Error() {
			t.Errorf("%s: the marked error reads %q, want %q", driverName, marked.Error(), err.Error())
		}
		want := "the database answered " + code + "; its message is in the exporter's log"
		if caller := model.CallerText(marked); caller != want {
			t.Errorf("%s: the caller is answered %q, want %q", driverName, caller, want)
		}
		if again, ok := d.serverError(marked); !ok || again != code {
			t.Errorf("%s: the marked error is no longer the server's to errors.As", driverName)
		}
	}
}

// checkNotServerAnswer checks that err, which is no error of the server's,
// is not recognised as one and is left as it is.
func checkNotServerAnswer(t *testing.T, driverName string, err error) {
	t.Helper()
	d := sqlDriverTable[driverName]
	if code, ok := d.serverError(err); ok {
		t.Errorf("%s: %v recognised as the server's error %q", driverName, err, code)
	}
	if marked := d.sqlServerAnswer(err); marked != err { //nolint:errorlint // the very error, unchanged
		t.Errorf("%s: %v was changed to %v", driverName, err, marked)
	}
}

var sqlRefused = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}

// The built-in drivers tell the server's own error by the code the
// database documents: postgres's SQLSTATE, mysql's error number and
// SQLSTATE, SQL Server's error number; a network failure, or an error of
// the driver's own, is not the server's.
func TestTheBuiltInSQLDriversTellTheServersErrorByItsCode(t *testing.T) {
	checkServerAnswer(t, SQLDriverPostgres, &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: `password authentication failed for user "metrics"`}, "SQLSTATE 28P01")
	checkServerAnswer(t, SQLDriverMySQL, &mysql.MySQLError{Number: 1045, SQLState: [5]byte{'2', '8', '0', '0', '0'}, Message: "Access denied for user 'metrics'@'10.1.2.3' (using password: YES)"}, "error 1045 (SQLSTATE 28000)")
	checkServerAnswer(t, SQLDriverMySQL, &mysql.MySQLError{Number: 1146, Message: "Table 'secret_schema.jobs' doesn't exist"}, "error 1146")
	checkServerAnswer(t, SQLDriverSQLServer, mssql.Error{Number: 18456, State: 1, Class: 14, Message: "Login failed for user 'metrics'."}, "error 18456")
	checkServerAnswer(t, SQLDriverSQLServer, &mssql.Error{Number: 245, Message: "Conversion failed when converting the varchar value 'secret' to data type int."}, "error 245")
	for _, name := range sqlDefaultDrivers {
		checkNotServerAnswer(t, name, sqlRefused)
		checkNotServerAnswer(t, name, errors.New("tls: failed to verify certificate"))
		checkNotServerAnswer(t, name, nil)
	}
	checkNotServerAnswer(t, SQLDriverMySQL, mysql.ErrInvalidConn)
}

// What a probe's caller is told of a failure the server reported, wherever
// the sql request type met it — the login, the read-only transaction's
// beginning, the statement, the rows — is the step and the server's code;
// the error's text, which the log shows, still has the whole message. A
// connection the network refused is told as it is.
func TestASQLProbesCallerIsToldTheServersCodeAndNotItsMessage(t *testing.T) {
	message := `invalid input syntax for type integer: "row-secret-4711"`
	pgErr := &pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: message}
	for _, tc := range []struct {
		name   string
		fail   func(db *sqltest.DB)
		caller string
	}{
		{"connecting", func(db *sqltest.DB) { db.ConnectErr = pgErr },
			"connecting: connecting to postgres at db.example:5432: the database answered SQLSTATE 22P02; its message is in the exporter's log"},
		{"beginning", func(db *sqltest.DB) { db.BeginErr = pgErr },
			"running the statement: beginning the read-only transaction: the database answered SQLSTATE 22P02; its message is in the exporter's log (on db.example:5432)"},
		{"statement", func(db *sqltest.DB) { db.Results["q"] = sqltest.Result{Err: pgErr} },
			"running the statement: the database answered SQLSTATE 22P02; its message is in the exporter's log (on db.example:5432)"},
		{"rows", func(db *sqltest.DB) {
			db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}, RowsErr: pgErr}
		}, "reading the rows: the database answered SQLSTATE 22P02; its message is in the exporter's log (on db.example:5432)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSQLFixture(t)
			tc.fail(f.db)
			c := f.collector(t, "db", SQLDriverPostgres, "q")
			_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 10 * time.Second}, nil)
			if err == nil {
				t.Fatal("no failure")
			}
			if caller := model.CallerText(err); caller != tc.caller {
				t.Errorf("the caller is answered %q, want %q", caller, tc.caller)
			}
			if !strings.Contains(err.Error(), message) {
				t.Errorf("the error's text %q has not the server's message", err)
			}
			var got *pgconn.PgError
			if !errors.As(err, &got) {
				t.Errorf("%v is no longer the server's error to errors.As", err)
			}
		})
	}
	t.Run("named host", func(t *testing.T) {
		// The server's message names the server, as a foreign table's
		// does; the caller, who is not shown it, is still told the server.
		f := newSQLFixture(t)
		f.db.Results["q"] = sqltest.Result{Err: &pgconn.PgError{Severity: "ERROR", Code: "08001", Message: "could not connect to server db.example:5432 for the foreign table secret_jobs"}}
		_, err := FetchCollector(context.Background(), "db.example", f.collector(t, "db", SQLDriverPostgres, "q"), RequestOverrides{Timeout: 10 * time.Second}, nil)
		want := "running the statement: the database answered SQLSTATE 08001; its message is in the exporter's log (on db.example:5432)"
		if caller := model.CallerText(err); caller != want {
			t.Errorf("the caller is answered %q, want %q", caller, want)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f := newSQLFixture(t)
		f.db.ConnectErr = sqlRefused
		_, err := FetchCollector(context.Background(), "db.example", f.collector(t, "db", SQLDriverPostgres, "q"), RequestOverrides{Timeout: 10 * time.Second}, nil)
		if err == nil || model.CallerText(err) != err.Error() || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("a refused connection is answered %q, its text being %v", model.CallerText(err), err)
		}
	})
}

// The server's report that it ended the statement at the statement timeout
// the driver set, marked as the server's answer, is still told as the
// scrape's deadline coming first.
func TestAMarkedStatementTimeoutIsStillTheScrapesDeadline(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Err: &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to statement timeout"}}
	_, err := FetchCollector(context.Background(), "db.example", f.collector(t, "db", SQLDriverPostgres, "q"), RequestOverrides{Timeout: 10 * time.Second}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v is not the scrape's deadline", err)
	}
	want := "running the statement: the scrape's deadline came first (context deadline exceeded): the database answered SQLSTATE 57014; its message is in the exporter's log (on db.example:5432)"
	if caller := model.CallerText(err); caller != want {
		t.Errorf("the caller is answered %q, want %q", caller, want)
	}
}
