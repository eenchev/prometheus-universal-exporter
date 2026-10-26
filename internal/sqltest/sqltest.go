// Package sqltest is a stand-in database for the tests of the sql request
// type: a database/sql/driver implementation that answers each statement
// with the rows a test gave it, and records what the exporter asked of it —
// the transactions it began and how, the statements it ran before the query,
// the values it bound, the connections it opened and closed, the pings. The
// exporter reaches it through the same pools and connectors as a real
// driver (fetch.UseSQLTestDriver), only the making of a connection being
// the test's.
package sqltest

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Result is what a statement returns: its columns, with their types as a
// driver names them, and its rows; or Err, which the query fails with.
// RowsErr, when set, is what reading the rows fails with after the last
// of them, as a server's error in the middle of a result is.
type Result struct {
	Columns []string
	Types   []string
	Rows    [][]driver.Value
	Err     error
	RowsErr error
}

// Query is a statement as it was run, with the values bound to it.
type Query struct {
	Statement string
	Args      []any
}

// DB is the stand-in database. The zero value answers no statement; a test
// sets Results, and the other fields, before its connections are made.
type DB struct {
	mu sync.Mutex
	// Results answers each statement by its text.
	Results map[string]Result
	// Block, when set, holds every query until the query's context ends or
	// Block is closed; Started is told each query that is held.
	Block   chan struct{}
	Started chan string
	// ConnectErr fails every connection, and BeginErr every transaction's
	// beginning.
	ConnectErr error
	BeginErr   error
	// DrainOnClose makes closing a result read the rows left in it, as
	// mysql's and pgx's do, until the query's context ends.
	DrainOnClose bool
	// Closes, when set, is told each connection closed, without waiting for
	// it to be read.
	Closes chan struct{}

	pingErr   error
	txOptions []driver.TxOptions
	executed  []string
	queries   []Query
	rolled    int
	opened    atomic.Int64
	closed    atomic.Int64
	pings     atomic.Int64
	rowsRead  atomic.Int64

	pingHang    bool
	pingStarted chan struct{}
	pinged      map[*conn]bool
	rollHang    bool
}

// Connect makes a connection to the database, as a driver's connector does.
func (db *DB) Connect(context.Context) (driver.Conn, error) {
	if db.ConnectErr != nil {
		return nil, db.ConnectErr
	}
	db.opened.Add(1)
	return &conn{db: db}, nil
}

// ConnectOver makes a connection to the database over nc, a network
// connection the test dialed, which the connection closes with it.
func (db *DB) ConnectOver(ctx context.Context, nc net.Conn) (driver.Conn, error) {
	c, err := db.Connect(ctx)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	c.(*conn).nc = nc
	return c, nil
}

// FailPings fails every ping from now on with err, or none with nil; it may
// be called while the exporter pings.
func (db *DB) FailPings(err error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.pingErr = err
}

// HangPings makes every ping from now on hang until its context ends, as one
// to a host that stopped answering does, and fail then with the context's
// error, when hang is true, and answer again when it is false; started,
// when it is not nil, is told each ping that hangs, without waiting for it
// to be read. It may be called while the exporter pings.
func (db *DB) HangPings(hang bool, started chan struct{}) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.pingHang, db.pingStarted = hang, started
}

// HangRollbacks makes the rollback of a transaction on a connection made
// over a network connection (ConnectOver) wait for the server's answer from
// now on, reading the network connection with no context and no deadline,
// as go-sql-driver/mysql's Rollback does (transaction.go, exec("ROLLBACK")):
// it fails, with driver.ErrBadConn, only when the network connection is
// closed or the server writes to it. False makes rollbacks answer again. It
// may be called while the exporter runs statements.
func (db *DB) HangRollbacks(hang bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.rollHang = hang
}

// Opened is how many connections were made.
func (db *DB) Opened() int64 { return db.opened.Load() }

// Closed is how many connections were closed.
func (db *DB) Closed() int64 { return db.closed.Load() }

// Pings is how many pings the database answered or failed.
func (db *DB) Pings() int64 { return db.pings.Load() }

// PingedConnections is how many connections were pinged, each counted once.
func (db *DB) PingedConnections() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.pinged)
}

// RowsRead is how many rows were read from results, by the exporter or by
// closing them (DrainOnClose).
func (db *DB) RowsRead() int64 { return db.rowsRead.Load() }

// TxOptions is the options of every transaction begun, in order.
func (db *DB) TxOptions() []driver.TxOptions {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]driver.TxOptions(nil), db.txOptions...)
}

// Executed is every statement run that returns no rows, in order, such as
// SET LOCAL statement_timeout.
func (db *DB) Executed() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]string(nil), db.executed...)
}

// Queries is every query run, in order.
func (db *DB) Queries() []Query {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]Query(nil), db.queries...)
}

// RolledBack is how many transactions were rolled back.
func (db *DB) RolledBack() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.rolled
}

// conn is one connection.
type conn struct {
	db     *DB
	closed bool
	// nc is the network connection of a connection made by ConnectOver.
	nc net.Conn
}

var errNotSupported = errors.New("sqltest: not supported")

func (c *conn) Prepare(string) (driver.Stmt, error) { return nil, errNotSupported }

func (c *conn) Close() error {
	if !c.closed {
		c.closed = true
		c.db.closed.Add(1)
		select {
		case c.db.Closes <- struct{}{}:
		default:
		}
		if c.nc != nil {
			_ = c.nc.Close()
		}
	}
	return nil
}

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *conn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	c.db.txOptions = append(c.db.txOptions, opts)
	if c.db.BeginErr != nil {
		return nil, c.db.BeginErr
	}
	return tx{db: c.db, c: c}, nil
}

func (c *conn) Ping(ctx context.Context) error {
	c.db.pings.Add(1)
	c.db.mu.Lock()
	if c.db.pinged == nil {
		c.db.pinged = map[*conn]bool{}
	}
	c.db.pinged[c] = true
	hang, started, err := c.db.pingHang, c.db.pingStarted, c.db.pingErr
	c.db.mu.Unlock()
	if hang {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return driver.ErrBadConn
	}
	return nil
}

func (c *conn) ExecContext(_ context.Context, statement string, _ []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	c.db.executed = append(c.db.executed, statement)
	return driver.RowsAffected(0), nil
}

func (c *conn) QueryContext(ctx context.Context, statement string, named []driver.NamedValue) (driver.Rows, error) {
	args := make([]any, len(named))
	for i, v := range named {
		args[i] = v.Value
	}
	c.db.mu.Lock()
	c.db.queries = append(c.db.queries, Query{Statement: statement, Args: args})
	result, ok := c.db.Results[statement]
	block, started, drain := c.db.Block, c.db.Started, c.db.DrainOnClose
	c.db.mu.Unlock()
	if block != nil {
		if started != nil {
			started <- statement
		}
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !ok {
		return nil, errors.New("sqltest: no result for the statement " + statement)
	}
	if result.Err != nil {
		return nil, result.Err
	}
	return &rows{db: c.db, ctx: ctx, result: result, drain: drain}, nil
}

// tx is a transaction, which a rollback ends.
type tx struct {
	db *DB
	c  *conn
}

func (tx) Commit() error { return nil }

func (t tx) Rollback() error {
	t.db.mu.Lock()
	hang := t.db.rollHang && t.c.nc != nil
	t.db.mu.Unlock()
	if hang {
		_, _ = t.c.nc.Read(make([]byte, 1))
		return driver.ErrBadConn
	}
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.rolled++
	return nil
}

// rows reads a result, within the context of its query.
type rows struct {
	db     *DB
	ctx    context.Context
	result Result
	next   int
	drain  bool
}

func (r *rows) Columns() []string { return r.result.Columns }

// Close reads the rows left with DrainOnClose, until the query's context
// ends, which a driver that drains a result stops at.
func (r *rows) Close() error {
	for r.drain && r.next < len(r.result.Rows) {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		r.next++
		r.db.rowsRead.Add(1)
	}
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if r.next >= len(r.result.Rows) {
		if r.result.RowsErr != nil {
			return r.result.RowsErr
		}
		return io.EOF
	}
	copy(dest, r.result.Rows[r.next])
	r.next++
	r.db.rowsRead.Add(1)
	return nil
}

// ColumnTypeDatabaseTypeName is a column's type, as Result.Types gives it.
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	if i < len(r.result.Types) {
		return r.result.Types[i]
	}
	return ""
}
