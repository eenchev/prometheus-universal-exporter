//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	mssql "github.com/microsoft/go-mssqldb"
)

// connRecorder records the calls database/sql makes of a fake driver
// connection, its transactions and statements, and what each was given.
type connRecorder struct {
	mu    sync.Mutex
	calls []string
	// stmt is what the connection's statements can do: "full" (with a
	// context, checking arguments, converting columns), "ctx" (with a
	// context) or "plain".
	stmt string
}

func (r *connRecorder) record(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *connRecorder) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// connSpecial is an argument only a fake's own argument checker takes.
type connSpecial struct{}

func connValues(args []driver.Value) string { return fmt.Sprint(args) }

func connNamed(args []driver.NamedValue) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = fmt.Sprintf("%s#%d=%v", a.Name, a.Ordinal, a.Value)
	}
	return strings.Join(parts, ",")
}

func (r *connRecorder) rows(what string) driver.Rows {
	return &connRows{values: []driver.Value{what}}
}

type connRows struct{ values []driver.Value }

func (*connRows) Columns() []string { return []string{"v"} }
func (*connRows) Close() error      { return nil }
func (r *connRows) Next(dest []driver.Value) error {
	if r.values == nil {
		return io.EOF
	}
	dest[0], r.values = r.values[0], nil
	return nil
}

// The capabilities a fake connection is made of, one method each.
type (
	capBase       struct{ r *connRecorder }
	capBeginTx    struct{ r *connRecorder }
	capPrepareCtx struct{ r *connRecorder }
	capExec       struct{ r *connRecorder }
	capExecCtx    struct{ r *connRecorder }
	capQuery      struct{ r *connRecorder }
	capQueryCtx   struct{ r *connRecorder }
	capPing       struct{ r *connRecorder }
	capReset      struct{ r *connRecorder }
	capValid      struct{ r *connRecorder }
	capCheck      struct{ r *connRecorder }
)

func (c capBase) Prepare(query string) (driver.Stmt, error) {
	c.r.record("Prepare %s", query)
	return c.r.newStmt(), nil
}
func (c capBase) Close() error { c.r.record("Close"); return nil }
func (c capBase) Begin() (driver.Tx, error) {
	c.r.record("Begin")
	return connTx(c), nil
}

func (c capBeginTx) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.r.record("BeginTx %+v", opts)
	return connTx(c), nil
}

func (c capPrepareCtx) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	c.r.record("PrepareContext %s", query)
	return c.r.newStmt(), nil
}

func (c capExec) Exec(query string, args []driver.Value) (driver.Result, error) {
	c.r.record("Exec %s %s", query, connValues(args))
	return driver.RowsAffected(1), nil
}

func (c capExecCtx) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.r.record("ExecContext %s %s", query, connNamed(args))
	return driver.RowsAffected(2), nil
}

func (c capQuery) Query(query string, args []driver.Value) (driver.Rows, error) {
	c.r.record("Query %s %s", query, connValues(args))
	return c.r.rows("conn query"), nil
}

func (c capQueryCtx) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.r.record("QueryContext %s %s", query, connNamed(args))
	return c.r.rows("conn query context"), nil
}

func (c capPing) Ping(context.Context) error { c.r.record("Ping"); return nil }

func (c capReset) ResetSession(context.Context) error { c.r.record("ResetSession"); return nil }

func (c capValid) IsValid() bool { c.r.record("IsValid"); return true }

func (c capCheck) CheckNamedValue(nv *driver.NamedValue) error {
	c.r.record("conn CheckNamedValue %T", nv.Value)
	if _, ok := nv.Value.(connSpecial); ok {
		nv.Value = "special by the connection"
		return nil
	}
	return driver.ErrSkip
}

type connTx struct{ r *connRecorder }

func (t connTx) Commit() error   { t.r.record("Commit"); return nil }
func (t connTx) Rollback() error { t.r.record("Rollback"); return nil }

// The statements a fake connection prepares.
type (
	stmtPlain struct{ r *connRecorder }
	stmtCtx   struct{ stmtPlain }
	stmtFull  struct{ stmtCtx }
)

func (r *connRecorder) newStmt() driver.Stmt {
	switch r.stmt {
	case "full":
		return stmtFull{stmtCtx{stmtPlain{r}}}
	case "ctx":
		return stmtCtx{stmtPlain{r}}
	}
	return stmtPlain{r}
}

func (s stmtPlain) Close() error  { s.r.record("stmt Close"); return nil }
func (s stmtPlain) NumInput() int { return -1 }
func (s stmtPlain) Exec(args []driver.Value) (driver.Result, error) {
	s.r.record("stmt Exec %s", connValues(args))
	return driver.RowsAffected(3), nil
}
func (s stmtPlain) Query(args []driver.Value) (driver.Rows, error) {
	s.r.record("stmt Query %s", connValues(args))
	return s.r.rows("stmt query"), nil
}
func (s stmtCtx) ExecContext(_ context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.r.record("stmt ExecContext %s", connNamed(args))
	return driver.RowsAffected(4), nil
}
func (s stmtCtx) QueryContext(_ context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.r.record("stmt QueryContext %s", connNamed(args))
	return s.r.rows("stmt query context"), nil
}
func (s stmtFull) CheckNamedValue(nv *driver.NamedValue) error {
	s.r.record("stmt CheckNamedValue %T", nv.Value)
	if v, ok := nv.Value.(int); ok {
		nv.Value = int64(v) * 10
		return nil
	}
	return driver.ErrSkip
}
func (s stmtFull) ColumnConverter(idx int) driver.ValueConverter {
	s.r.record("stmt ColumnConverter %d", idx)
	return driver.DefaultParameterConverter
}

// The fake connections: one with each driver's capabilities, as its
// connection type has them (vendor: go-sql-driver/mysql connection.go,
// gosnowflake connection.go, clickhouse-go clickhouse_std.go, pgx stdlib
// sql.go, go-mssqldb mssql.go, go-ora connection.go, and verticaConn
// here), with the statements it prepares, one with nothing but what
// database/sql requires, and one that runs statements without a context.
type (
	fakeMySQL struct {
		capBase
		capBeginTx
		capPrepareCtx
		capExec
		capExecCtx
		capQuery
		capQueryCtx
		capPing
		capReset
		capValid
		capCheck
	}
	fakeSnowflake struct {
		capBase
		capBeginTx
		capPrepareCtx
		capExec
		capExecCtx
		capQuery
		capQueryCtx
		capPing
		capCheck
	}
	fakePgxOrClickHouse struct {
		capBase
		capBeginTx
		capPrepareCtx
		capExecCtx
		capQueryCtx
		capPing
		capReset
		capCheck
	}
	fakeSQLServer struct {
		capBase
		capBeginTx
		capPrepareCtx
		capPing
		capReset
		capValid
		capCheck
	}
	fakeOracle struct {
		capBase
		capBeginTx
		capPrepareCtx
		capExec
		capExecCtx
		capQueryCtx
		capPing
		capReset
		capCheck
	}
	fakeVertica struct {
		capBase
		capBeginTx
		capPrepareCtx
		capPing
		capReset
		capValid
	}
	fakeMinimal struct{ capBase }
	fakeLegacy  struct {
		capBase
		capExec
		capQuery
		capValid
	}
)

// fakeConns are the fake connections by name, each made with a recorder.
var fakeConns = []struct {
	name string
	stmt string
	make func(r *connRecorder) driver.Conn
}{
	{"mysql", "full", func(r *connRecorder) driver.Conn {
		return &fakeMySQL{capBase{r}, capBeginTx{r}, capPrepareCtx{r}, capExec{r}, capExecCtx{r}, capQuery{r}, capQueryCtx{r}, capPing{r}, capReset{r}, capValid{r}, capCheck{r}}
	}},
	{"snowflake", "ctx", func(r *connRecorder) driver.Conn {
		return &fakeSnowflake{capBase{r}, capBeginTx{r}, capPrepareCtx{r}, capExec{r}, capExecCtx{r}, capQuery{r}, capQueryCtx{r}, capPing{r}, capCheck{r}}
	}},
	{"postgres, clickhouse", "ctx", func(r *connRecorder) driver.Conn {
		return &fakePgxOrClickHouse{capBase{r}, capBeginTx{r}, capPrepareCtx{r}, capExecCtx{r}, capQueryCtx{r}, capPing{r}, capReset{r}, capCheck{r}}
	}},
	{"sqlserver", "full", func(r *connRecorder) driver.Conn {
		return &fakeSQLServer{capBase{r}, capBeginTx{r}, capPrepareCtx{r}, capPing{r}, capReset{r}, capValid{r}, capCheck{r}}
	}},
	{"oracle", "ctx", func(r *connRecorder) driver.Conn {
		return &fakeOracle{capBase{r}, capBeginTx{r}, capPrepareCtx{r}, capExec{r}, capExecCtx{r}, capQueryCtx{r}, capPing{r}, capReset{r}, capCheck{r}}
	}},
	{"vertica", "ctx", func(r *connRecorder) driver.Conn {
		return &fakeVertica{capBase{r}, capBeginTx{r}, capPrepareCtx{r}, capPing{r}, capReset{r}, capValid{r}}
	}},
	{"minimal", "plain", func(r *connRecorder) driver.Conn { return &fakeMinimal{capBase{r}} }},
	{"without contexts", "full", func(r *connRecorder) driver.Conn {
		return &fakeLegacy{capBase{r}, capExec{r}, capQuery{r}, capValid{r}}
	}},
}

// fakeConnector makes connections with make, guarded or as they are.
type fakeConnector struct {
	make    func() driver.Conn
	guarded bool
}

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	if c.guarded {
		return guardSQLConn("fake", c.make()), nil
	}
	return c.make(), nil
}

func (fakeConnector) Driver() driver.Driver { return sqlDriverStub{} }

// exerciseConn runs, on db, what database/sql does with a connection, and
// returns what each step gave.
func exerciseConn(t *testing.T, db *sql.DB, r *connRecorder) []string {
	t.Helper()
	var out []string
	note := func(step string, v any, err error) {
		out = append(out, fmt.Sprintf("%s: %v %v", step, v, err))
	}
	ctx := context.Background()
	readRows := func(rows *sql.Rows, err error) string {
		if err != nil {
			return "error " + err.Error()
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return "scan " + err.Error()
			}
			got = append(got, v)
		}
		return strings.Join(got, ";")
	}
	argSets := [][]any{nil, {1, "a"}, {sql.Named("n", 2)}, {connSpecial{}}, {struct{}{}}}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range argSets {
		note("query", readRows(conn.QueryContext(ctx, "SELECT q", args...)), nil)
		result, err := conn.ExecContext(ctx, "UPDATE e", args...)
		var n int64
		if err == nil {
			n, _ = result.RowsAffected()
		}
		note("exec", n, err)
		stmt, err := conn.PrepareContext(ctx, "SELECT p")
		if err != nil {
			note("prepare", nil, err)
			continue
		}
		note("stmt query", readRows(stmt.QueryContext(ctx, args...)), nil)
		result, err = stmt.ExecContext(ctx, args...)
		n = 0
		if err == nil {
			n, _ = result.RowsAffected()
		}
		note("stmt exec", n, err)
		note("stmt close", nil, stmt.Close())
	}
	note("ping", nil, conn.PingContext(ctx))
	var raw string
	note("raw", nil, conn.Raw(func(dc any) error {
		raw = fmt.Sprintf("%T", sqlDriverConn(dc))
		return nil
	}))
	note("raw conn", raw, nil)
	for _, opts := range []*sql.TxOptions{nil, {ReadOnly: true}, {Isolation: sql.LevelSerializable}} {
		tx, err := conn.BeginTx(ctx, opts)
		if err != nil {
			note("begin", opts, err)
			continue
		}
		note("tx query", readRows(tx.QueryContext(ctx, "SELECT t", 1)), nil)
		note("commit", nil, tx.Commit())
		tx, err = conn.BeginTx(ctx, opts)
		if err != nil {
			note("begin again", opts, err)
			continue
		}
		note("rollback", nil, tx.Rollback())
	}
	note("conn close", nil, conn.Close())
	// A connection taken again is reset and asked whether it is valid.
	conn, err = db.Conn(ctx)
	note("again", nil, err)
	if err == nil {
		note("again ping", nil, conn.PingContext(ctx))
		note("again close", nil, conn.Close())
	}
	// A transaction whose context ends is rolled back by database/sql,
	// which keeps the connection only when it resets its session and says
	// whether it is valid.
	txCtx, cancel := context.WithCancel(ctx)
	tx, err := db.BeginTx(txCtx, nil)
	note("cancelled begin", nil, err)
	if err == nil {
		rollbacks := func() int {
			n := 0
			for _, call := range r.log() {
				if call == "Rollback" {
					n++
				}
			}
			return n
		}
		before := rollbacks()
		cancel()
		for rollbacks() == before || db.Stats().InUse != 0 {
			runtime.Gosched()
		}
		note("after the cancelled transaction", db.Stats().OpenConnections, tx.Commit())
	}
	cancel()
	return out
}

// A guarded connection is what the driver's connection is to database/sql:
// for each driver's kind of connection, one with nothing but what
// database/sql requires, and one that runs statements without a context,
// everything database/sql does with it, with every kind of argument, calls
// the driver's connection, its transactions and statements, in the same
// order with the same arguments, and gives the same results and errors, as
// it does with the driver's connection itself: statements run on the
// connection or prepared, arguments checked by the statement, the
// connection or the default converter, transactions begun with options or
// refused, and the connection of a transaction whose context ended kept or
// closed.
func TestAGuardedConnectionIsWhatTheDriversIsToDatabaseSQL(t *testing.T) {
	for _, fake := range fakeConns {
		var logs, outs [2][]string
		for i, guarded := range []bool{false, true} {
			r := &connRecorder{stmt: fake.stmt}
			db := sql.OpenDB(fakeConnector{make: func() driver.Conn { return fake.make(r) }, guarded: guarded})
			db.SetMaxOpenConns(1)
			outs[i] = exerciseConn(t, db, r)
			_ = db.Close()
			logs[i] = r.log()
		}
		if !slices.Equal(logs[0], logs[1]) {
			t.Errorf("%s: the driver was called\n%s\nguarded, and\n%s\nas it is", fake.name, strings.Join(logs[1], "\n"), strings.Join(logs[0], "\n"))
		}
		if !slices.Equal(outs[0], outs[1]) {
			t.Errorf("%s: database/sql gave\n%s\nguarded, and\n%s\nas it is", fake.name, strings.Join(outs[1], "\n"), strings.Join(outs[0], "\n"))
		}
		if len(logs[0]) < 40 {
			t.Errorf("%s: only %d calls of the driver", fake.name, len(logs[0]))
		}
	}
}

// assertGuardedLike fails unless the guarded conn runs statements, resets
// its session and says whether it is valid exactly as conn does, and takes
// the rest, which it does as database/sql does without them.
func assertGuardedLike(t *testing.T, name string, conn driver.Conn) {
	t.Helper()
	guarded := guardSQLConn(name, conn)
	for _, tc := range []struct {
		what        string
		has, should bool
	}{
		{"runs statements that return no rows", implements[driver.ExecerContext](guarded), implements[driver.ExecerContext](conn) || implements[driver.Execer](conn)}, //nolint:staticcheck // SA1019: database/sql still runs a statement on such a connection
		{"runs statements that return rows", implements[driver.QueryerContext](guarded), implements[driver.QueryerContext](conn) || implements[driver.Queryer](conn)}, //nolint:staticcheck // SA1019: database/sql still runs a statement on such a connection
		{"resets its session", implements[driver.SessionResetter](guarded), implements[driver.SessionResetter](conn)},
		{"says whether it is valid", implements[driver.Validator](guarded), implements[driver.Validator](conn)},
		{"pings", implements[driver.Pinger](guarded), true},
		{"prepares with a context", implements[driver.ConnPrepareContext](guarded), true},
		{"begins with a context", implements[driver.ConnBeginTx](guarded), true},
		{"checks arguments", implements[driver.NamedValueChecker](guarded), true},
	} {
		if tc.has != tc.should {
			t.Errorf("%s %T: the guarded connection %s %t, want %t", name, conn, tc.what, tc.has, tc.should)
		}
	}
	if sqlDriverConn(guarded) != any(conn) {
		t.Errorf("%s: Conn.Raw's function would not find the driver's connection", name)
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

// The driver connections whose type is exported, and every fake one, are
// guarded by a connection that runs statements, resets its session and
// says whether it is valid exactly when they do.
func TestAGuardedConnectionRunsResetsAndValidatesAsTheDriversDoes(t *testing.T) {
	assertGuardedLike(t, SQLDriverPostgres, (*stdlib.Conn)(nil))
	assertGuardedLike(t, SQLDriverSQLServer, (*mssql.Conn)(nil))
	for _, fake := range fakeConns {
		assertGuardedLike(t, fake.name, fake.make(&connRecorder{}))
	}
}

// panicConn is a driver connection that panics in every call but Begin,
// whose transaction panics in its commit and rollback, with what its Close
// and Rollback were called on reported first.
type panicConn struct {
	closing   chan struct{}
	rollbacks chan struct{}
}

func (*panicConn) Prepare(string) (driver.Stmt, error) { panic("prepare: index out of range") }
func (c *panicConn) Close() error {
	c.closing <- struct{}{}
	panic("close: nil pointer dereference")
}
func (c *panicConn) Begin() (driver.Tx, error) { return panicTx{c}, nil }
func (*panicConn) Ping(context.Context) error  { panic("ping: index out of range") }
func (*panicConn) ResetSession(context.Context) error {
	panic("reset: index out of range")
}
func (*panicConn) IsValid() bool { panic("valid: index out of range") }
func (*panicConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	panic("query: index out of range")
}

type panicTx struct{ c *panicConn }

func (panicTx) Commit() error { panic("commit: index out of range") }
func (t panicTx) Rollback() error {
	t.c.rollbacks <- struct{}{}
	panic("rollback: index out of range")
}

// closePanicConn is a driver connection that panics closing, with what its
// Close was called on reported first.
type closePanicConn struct{ closing chan struct{} }

func (*closePanicConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (*closePanicConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }
func (c *closePanicConn) Close() error {
	c.closing <- struct{}{}
	panic("close: nil pointer dereference")
}

// panicPool is a database/sql pool of the pool's connector, whose driver
// makes panicConns.
func panicPool(t *testing.T, conn func() driver.Conn) *sql.DB {
	t.Helper()
	hook := func(context.Context, string, *sqlConnectSpec) (driver.Conn, error) { return conn(), nil }
	sqlConnectHook.Store(&hook)
	t.Cleanup(func() { sqlConnectHook.Store(nil) })
	pool := &sqlPool{driver: sqlDriverTable[SQLDriverSQLServer], key: sqlPoolKey{collector: "db", address: sqlAddress{host: "127.0.0.1", port: "1433", hostPort: "127.0.0.1:1433"}}}
	return sql.OpenDB(&sqlConnector{cache: newSQLPoolCache(), pool: pool})
}

// A driver that panics closing a connection that database/sql's cleaner
// closes, past its lifetime, in a goroutine of its own, no longer ends the
// process: the connection is closed, counted closed.
func TestADriverThatPanicsClosingAnExpiredConnectionDoesNotEndTheProcess(t *testing.T) {
	c := &closePanicConn{closing: make(chan struct{}, 1)}
	db := panicPool(t, func() driver.Conn { return c })
	t.Cleanup(func() { _ = db.Close() })
	db.SetConnMaxLifetime(time.Hour)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	// A shorter lifetime has the running cleaner look at once.
	db.SetConnMaxLifetime(time.Nanosecond)
	<-c.closing
	for db.Stats().OpenConnections != 0 {
		runtime.Gosched()
	}
	if closed := db.Stats().MaxLifetimeClosed; closed != 1 {
		t.Errorf("%d connections closed past their lifetime", closed)
	}
}

// A driver that panics rolling back a transaction whose context ended, which
// database/sql rolls back in a goroutine of its own, no longer ends the
// process: the connection is given up and closed.
func TestADriverThatPanicsRollingBackACancelledTransactionDoesNotEndTheProcess(t *testing.T) {
	c := &panicConn{closing: make(chan struct{}, 1), rollbacks: make(chan struct{}, 1)}
	db := panicPool(t, func() driver.Conn { return c })
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-c.rollbacks
	<-c.closing
	for db.Stats().OpenConnections != 0 {
		runtime.Gosched()
	}
	if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("committing the rolled-back transaction: %v", err)
	}
}

// Every call of a guarded connection, its transaction and statement whose
// driver panics fails with a sentence naming the driver, joined with
// driver.ErrBadConn so that database/sql closes the connection, and the
// connection asked whether it is valid says it is not.
func TestEveryCallOfAGuardedConnectionWhoseDriverPanicsFails(t *testing.T) {
	c := &panicConn{closing: make(chan struct{}, 1), rollbacks: make(chan struct{}, 1)}
	g := guardSQLConn(SQLDriverSQLServer, c)
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, driver.ErrBadConn) || !strings.Contains(err.Error(), "the sqlserver driver failed on what the server sent (it panicked: "+what) {
			t.Errorf("%s: %v", what, err)
		}
	}
	_, err := g.Prepare("q")
	check("prepare", err)
	_, err = g.(driver.ConnPrepareContext).PrepareContext(context.Background(), "q")
	check("prepare", err)
	_, err = g.(driver.QueryerContext).QueryContext(context.Background(), "q", nil)
	check("query", err)
	check("ping", g.(driver.Pinger).Ping(context.Background()))
	check("reset", g.(driver.SessionResetter).ResetSession(context.Background()))
	if g.(driver.Validator).IsValid() {
		t.Error("a connection whose driver panicked saying whether it is valid is valid")
	}
	tx, err := g.(driver.ConnBeginTx).BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check("commit", tx.Commit())
	check("rollback", tx.Rollback())
	<-c.rollbacks
	check("close", g.Close())
	<-c.closing
	stmt := (&sqlGuardedConn{conn: c, driverName: SQLDriverSQLServer}).guardStmtOf(panicStmt{})
	check("stmt close", stmt.Close())
	if n := stmt.NumInput(); n != -1 {
		t.Errorf("NumInput of a panicking statement is %d", n)
	}
	_, err = stmt.(driver.StmtQueryContext).QueryContext(context.Background(), nil)
	check("stmt query", err)
	_, err = stmt.(driver.StmtExecContext).ExecContext(context.Background(), nil)
	check("stmt exec", err)
	check("stmt check", stmt.(driver.NamedValueChecker).CheckNamedValue(&driver.NamedValue{}))
	_, err = stmt.(driver.ColumnConverter).ColumnConverter(0).ConvertValue(1) //nolint:staticcheck // SA1019: what database/sql asks
	check("stmt converter", err)
}

// panicStmt is a statement whose every call panics.
type panicStmt struct{}

func (panicStmt) Close() error  { panic("stmt close: index out of range") }
func (panicStmt) NumInput() int { panic("stmt input: index out of range") }
func (panicStmt) Exec([]driver.Value) (driver.Result, error) {
	panic("stmt exec: index out of range")
}
func (panicStmt) Query([]driver.Value) (driver.Rows, error) {
	panic("stmt query: index out of range")
}
func (panicStmt) CheckNamedValue(*driver.NamedValue) error {
	panic("stmt check: index out of range")
}
func (panicStmt) ColumnConverter(int) driver.ValueConverter {
	panic("stmt converter: index out of range")
}

// guardStmtOf is stmt guarded as a statement the connection prepared.
func (c *sqlGuardedConn) guardStmtOf(stmt driver.Stmt) driver.Stmt {
	s, _ := c.guardStmt(stmt, nil)
	return s
}
