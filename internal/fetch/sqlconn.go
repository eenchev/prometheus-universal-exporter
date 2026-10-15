//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
)

// sqlGuardedConn is a driver's connection as the pool's connector hands it
// to database/sql (guardSQLConn): each call on it, and on the transactions
// and statements it makes, turns a panic of the driver into an error, the
// connection given up. database/sql calls a driver in goroutines of its own
// as well as the scrape's: its connection cleaner closes a connection past
// its lifetime or idle time, and a transaction whose context ends is rolled
// back by a goroutine that waits for that (sql.go connectionCleaner,
// Tx.awaitDone), where a panic would end the process.
//
// It is what the driver's connection is to database/sql, which asks each
// connection what it can do and acts on the answer. It runs a statement
// with arguments on the connection itself, the arguments checked without
// the statement, only when the connection says it can, and otherwise
// prepares the statement first and checks the arguments by it (sql.go
// execDC, queryDC); and it keeps the connection of a transaction whose
// context ended only when the connection both resets its session and says
// whether it is valid (sql.go beginDC). So a guarded connection runs
// statements (driver.ExecerContext, driver.QueryerContext), resets its
// session (driver.SessionResetter) and says whether it is valid
// (driver.Validator) exactly when the driver's connection does, one of
// sixteen types by those four (sqlGuardedKinds), running a statement on a
// connection that takes no context as database/sql would (ctxutil.go
// ctxDriverExec, ctxDriverQuery). The rest it always takes, and does as
// database/sql does without them when the driver's connection does not:
// asked to check an argument, it answers driver.ErrSkip, which has
// database/sql check it as it would have (convert.go
// driverArgsConnLocked); it pings as a connection that cannot ping is taken
// to, by doing nothing; it prepares and begins as database/sql does with a
// connection that takes no context (ctxutil.go ctxDriverPrepare,
// ctxDriverBegin). Its statements are the same (sqlGuardedStmt), and
// Conn.Raw's function finds the driver's connection by sqlDriverConn.
type sqlGuardedConn struct {
	conn       driver.Conn
	driverName string
}

// The methods a guarded connection has only when the driver's connection
// has them.
type (
	sqlGuardExec  struct{ c *sqlGuardedConn }
	sqlGuardQuery struct{ c *sqlGuardedConn }
	sqlGuardReset struct{ c *sqlGuardedConn }
	sqlGuardValid struct{ c *sqlGuardedConn }
)

// The guarded connections by what they have of those: E runs statements
// that return no rows, Q statements that do, R resets its session, V says
// whether it is valid.
type (
	sqlGuardedE struct {
		*sqlGuardedConn
		sqlGuardExec
	}
	sqlGuardedQ struct {
		*sqlGuardedConn
		sqlGuardQuery
	}
	sqlGuardedEQ struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardQuery
	}
	sqlGuardedR struct {
		*sqlGuardedConn
		sqlGuardReset
	}
	sqlGuardedER struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardReset
	}
	sqlGuardedQR struct {
		*sqlGuardedConn
		sqlGuardQuery
		sqlGuardReset
	}
	sqlGuardedEQR struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardQuery
		sqlGuardReset
	}
	sqlGuardedV struct {
		*sqlGuardedConn
		sqlGuardValid
	}
	sqlGuardedEV struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardValid
	}
	sqlGuardedQV struct {
		*sqlGuardedConn
		sqlGuardQuery
		sqlGuardValid
	}
	sqlGuardedEQV struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardQuery
		sqlGuardValid
	}
	sqlGuardedRV struct {
		*sqlGuardedConn
		sqlGuardReset
		sqlGuardValid
	}
	sqlGuardedERV struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardReset
		sqlGuardValid
	}
	sqlGuardedQRV struct {
		*sqlGuardedConn
		sqlGuardQuery
		sqlGuardReset
		sqlGuardValid
	}
	sqlGuardedEQRV struct {
		*sqlGuardedConn
		sqlGuardExec
		sqlGuardQuery
		sqlGuardReset
		sqlGuardValid
	}
)

// sqlGuardedKinds makes each kind of guarded connection, by its bits: 1 for
// E, 2 for Q, 4 for R and 8 for V.
var sqlGuardedKinds = [16]func(c *sqlGuardedConn) driver.Conn{
	func(c *sqlGuardedConn) driver.Conn { return c },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedE{c, sqlGuardExec{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedQ{c, sqlGuardQuery{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedEQ{c, sqlGuardExec{c}, sqlGuardQuery{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedR{c, sqlGuardReset{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedER{c, sqlGuardExec{c}, sqlGuardReset{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedQR{c, sqlGuardQuery{c}, sqlGuardReset{c}} },
	func(c *sqlGuardedConn) driver.Conn {
		return &sqlGuardedEQR{c, sqlGuardExec{c}, sqlGuardQuery{c}, sqlGuardReset{c}}
	},
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedV{c, sqlGuardValid{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedEV{c, sqlGuardExec{c}, sqlGuardValid{c}} },
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedQV{c, sqlGuardQuery{c}, sqlGuardValid{c}} },
	func(c *sqlGuardedConn) driver.Conn {
		return &sqlGuardedEQV{c, sqlGuardExec{c}, sqlGuardQuery{c}, sqlGuardValid{c}}
	},
	func(c *sqlGuardedConn) driver.Conn { return &sqlGuardedRV{c, sqlGuardReset{c}, sqlGuardValid{c}} },
	func(c *sqlGuardedConn) driver.Conn {
		return &sqlGuardedERV{c, sqlGuardExec{c}, sqlGuardReset{c}, sqlGuardValid{c}}
	},
	func(c *sqlGuardedConn) driver.Conn {
		return &sqlGuardedQRV{c, sqlGuardQuery{c}, sqlGuardReset{c}, sqlGuardValid{c}}
	},
	func(c *sqlGuardedConn) driver.Conn {
		return &sqlGuardedEQRV{c, sqlGuardExec{c}, sqlGuardQuery{c}, sqlGuardReset{c}, sqlGuardValid{c}}
	},
}

// guardSQLConn is conn guarded against the driver's panics.
func guardSQLConn(driverName string, conn driver.Conn) driver.Conn {
	kind := 0
	if _, ok := conn.(driver.ExecerContext); ok {
		kind |= 1
	} else if _, ok := conn.(driver.Execer); ok { //nolint:staticcheck // SA1019: database/sql still runs a statement on such a connection
		kind |= 1
	}
	if _, ok := conn.(driver.QueryerContext); ok {
		kind |= 2
	} else if _, ok := conn.(driver.Queryer); ok { //nolint:staticcheck // SA1019: database/sql still runs a statement on such a connection
		kind |= 2
	}
	if _, ok := conn.(driver.SessionResetter); ok {
		kind |= 4
	}
	if _, ok := conn.(driver.Validator); ok {
		kind |= 8
	}
	return sqlGuardedKinds[kind](&sqlGuardedConn{conn: conn, driverName: driverName})
}

// sqlDriverConn is the driver's connection of dc, what Conn.Raw hands its
// function: the connection guardSQLConn guards, or dc itself.
func sqlDriverConn(dc any) any {
	if g, ok := dc.(interface{ driverConn() driver.Conn }); ok {
		return g.driverConn()
	}
	return dc
}

func (c *sqlGuardedConn) driverConn() driver.Conn { return c.conn }

// recovered, deferred, turns a panic of the driver into the error err
// points to: the driver's fault, joined with driver.ErrBadConn, so that
// database/sql closes the connection rather than use it again.
func (c *sqlGuardedConn) recovered(err *error) {
	if r := recover(); r != nil {
		*err = errors.Join(sqlDriverPanic(c.driverName, r), driver.ErrBadConn)
	}
}

func (c *sqlGuardedConn) Close() (err error) {
	defer c.recovered(&err)
	return c.conn.Close()
}

func (c *sqlGuardedConn) Prepare(query string) (stmt driver.Stmt, err error) {
	defer c.recovered(&err)
	return c.guardStmt(c.conn.Prepare(query))
}

func (c *sqlGuardedConn) PrepareContext(ctx context.Context, query string) (stmt driver.Stmt, err error) {
	defer c.recovered(&err)
	if pc, ok := c.conn.(driver.ConnPrepareContext); ok {
		return c.guardStmt(pc.PrepareContext(ctx, query))
	}
	si, err := c.conn.Prepare(query)
	if err == nil {
		select {
		default:
		case <-ctx.Done():
			_ = si.Close()
			return nil, ctx.Err()
		}
	}
	return c.guardStmt(si, err)
}

func (c *sqlGuardedConn) Begin() (tx driver.Tx, err error) {
	defer c.recovered(&err)
	return c.guardTx(c.conn.Begin()) //nolint:staticcheck // SA1019: the driver's Begin, for a caller of the guard's
}

func (c *sqlGuardedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (tx driver.Tx, err error) {
	defer c.recovered(&err)
	if bc, ok := c.conn.(driver.ConnBeginTx); ok {
		return c.guardTx(bc.BeginTx(ctx, opts))
	}
	if opts.Isolation != driver.IsolationLevel(0) {
		return nil, errors.New("sql: driver does not support non-default isolation level")
	}
	if opts.ReadOnly {
		return nil, errors.New("sql: driver does not support read-only transactions")
	}
	if ctx.Done() == nil {
		return c.guardTx(c.conn.Begin()) //nolint:staticcheck // SA1019: as database/sql begins with a driver that takes no context
	}
	txi, err := c.conn.Begin() //nolint:staticcheck // SA1019: as database/sql begins with a driver that takes no context
	if err == nil {
		select {
		default:
		case <-ctx.Done():
			_ = txi.Rollback()
			return nil, ctx.Err()
		}
	}
	return c.guardTx(txi, err)
}

func (g sqlGuardExec) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (result driver.Result, err error) {
	c := g.c
	defer c.recovered(&err)
	if ec, ok := c.conn.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	e := c.conn.(driver.Execer) //nolint:staticcheck // SA1019: as database/sql runs a statement on a driver that takes no context
	values, err := sqlDriverValues(ctx, args)
	if err != nil {
		return nil, err
	}
	return e.Exec(query, values)
}

func (g sqlGuardQuery) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (rows driver.Rows, err error) {
	c := g.c
	defer c.recovered(&err)
	if qc, ok := c.conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	q := c.conn.(driver.Queryer) //nolint:staticcheck // SA1019: as database/sql runs a statement on a driver that takes no context
	values, err := sqlDriverValues(ctx, args)
	if err != nil {
		return nil, err
	}
	return q.Query(query, values)
}

func (c *sqlGuardedConn) Ping(ctx context.Context) (err error) {
	defer c.recovered(&err)
	if p, ok := c.conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *sqlGuardedConn) CheckNamedValue(nv *driver.NamedValue) (err error) {
	defer c.recovered(&err)
	if checker, ok := c.conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (g sqlGuardReset) ResetSession(ctx context.Context) (err error) {
	defer g.c.recovered(&err)
	return g.c.conn.(driver.SessionResetter).ResetSession(ctx)
}

// IsValid is the driver's answer, or false when it panicked: database/sql
// then closes the connection.
func (g sqlGuardValid) IsValid() (valid bool) {
	var err error
	defer func() {
		if err != nil {
			valid = false
		}
	}()
	defer g.c.recovered(&err)
	return g.c.conn.(driver.Validator).IsValid()
}

// sqlDriverValues is args as a driver that takes no context is given them,
// or the error database/sql gives in their place (ctxutil.go ctxDriverExec,
// namedValueToValue).
func sqlDriverValues(ctx context.Context, args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for n, arg := range args {
		if len(arg.Name) > 0 {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		values[n] = arg.Value
	}
	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return values, nil
}

// guardTx is the driver's transaction, guarded.
func (c *sqlGuardedConn) guardTx(tx driver.Tx, err error) (driver.Tx, error) {
	if tx == nil {
		return nil, err
	}
	return &sqlGuardedTx{tx: tx, conn: c}, err
}

// sqlGuardedTx is a transaction of a guarded connection.
type sqlGuardedTx struct {
	tx   driver.Tx
	conn *sqlGuardedConn
}

func (t *sqlGuardedTx) Commit() (err error) {
	defer t.conn.recovered(&err)
	return t.tx.Commit()
}

func (t *sqlGuardedTx) Rollback() (err error) {
	defer t.conn.recovered(&err)
	return t.tx.Rollback()
}

// guardStmt is the driver's statement, guarded: a sqlGuardedStmtCC when it
// converts its columns' arguments itself (driver.ColumnConverter), as
// database/sql asks, and a sqlGuardedStmt otherwise.
func (c *sqlGuardedConn) guardStmt(stmt driver.Stmt, err error) (driver.Stmt, error) {
	if stmt == nil {
		return nil, err
	}
	g := sqlGuardedStmt{stmt: stmt, conn: c}
	if _, ok := stmt.(driver.ColumnConverter); ok { //nolint:staticcheck // SA1019: database/sql still asks for it
		return &sqlGuardedStmtCC{g}, err
	}
	return &g, err
}

// sqlGuardedStmt is a statement of a guarded connection. It runs with a
// context as database/sql runs a statement that takes none (ctxutil.go
// ctxDriverStmtExec, ctxDriverStmtQuery), and checks an argument as its
// statement does, or, when the statement does not, as its connection does,
// which is whom database/sql asks then (convert.go driverArgsConnLocked),
// or answers driver.ErrSkip.
type sqlGuardedStmt struct {
	stmt driver.Stmt
	conn *sqlGuardedConn
}

// sqlGuardedStmtCC is a guarded statement that converts its columns'
// arguments.
type sqlGuardedStmtCC struct{ sqlGuardedStmt }

func (s *sqlGuardedStmt) Close() (err error) {
	defer s.conn.recovered(&err)
	return s.stmt.Close()
}

// NumInput is the driver's count of placeholders, or -1, which has
// database/sql leave the count to the driver, when it panicked.
func (s *sqlGuardedStmt) NumInput() (n int) {
	var err error
	defer func() {
		if err != nil {
			n = -1
		}
	}()
	defer s.conn.recovered(&err)
	return s.stmt.NumInput()
}

func (s *sqlGuardedStmt) Exec(args []driver.Value) (result driver.Result, err error) {
	defer s.conn.recovered(&err)
	return s.stmt.Exec(args) //nolint:staticcheck // SA1019: the driver's Exec, for a caller of the guard's
}

func (s *sqlGuardedStmt) Query(args []driver.Value) (rows driver.Rows, err error) {
	defer s.conn.recovered(&err)
	return s.stmt.Query(args) //nolint:staticcheck // SA1019: the driver's Query, for a caller of the guard's
}

func (s *sqlGuardedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (result driver.Result, err error) {
	defer s.conn.recovered(&err)
	if ec, ok := s.stmt.(driver.StmtExecContext); ok {
		return ec.ExecContext(ctx, args)
	}
	values, err := sqlDriverValues(ctx, args)
	if err != nil {
		return nil, err
	}
	return s.stmt.Exec(values) //nolint:staticcheck // SA1019: as database/sql runs a statement that takes no context
}

func (s *sqlGuardedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (rows driver.Rows, err error) {
	defer s.conn.recovered(&err)
	if qc, ok := s.stmt.(driver.StmtQueryContext); ok {
		return qc.QueryContext(ctx, args)
	}
	values, err := sqlDriverValues(ctx, args)
	if err != nil {
		return nil, err
	}
	return s.stmt.Query(values) //nolint:staticcheck // SA1019: as database/sql runs a statement that takes no context
}

func (s *sqlGuardedStmt) CheckNamedValue(nv *driver.NamedValue) (err error) {
	defer s.conn.recovered(&err)
	if checker, ok := s.stmt.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(nv)
	}
	if checker, ok := s.conn.conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// ColumnConverter is the statement's converter of the column at idx, or,
// when the driver panicked, one that fails with the driver's fault.
func (s *sqlGuardedStmtCC) ColumnConverter(idx int) (converter driver.ValueConverter) {
	var err error
	defer func() {
		if err != nil {
			converter = sqlFailedConverter{err}
		}
	}()
	defer s.conn.recovered(&err)
	return s.stmt.(driver.ColumnConverter).ColumnConverter(idx) //nolint:staticcheck // SA1019: database/sql still asks for it
}

// sqlFailedConverter is the converter of a statement whose driver
// panicked giving its own.
type sqlFailedConverter struct{ err error }

func (c sqlFailedConverter) ConvertValue(any) (driver.Value, error) { return nil, c.err }
