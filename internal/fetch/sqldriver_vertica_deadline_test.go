//go:build (!select_request_types || request_type_sql) && sql_driver_vertica

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// verticaStandIn is a connection as vertica-sql-go's is to database/sql,
// whose calls wait for hold, when set, and report the contexts they are
// given; closed is closed when it is closed.
type verticaStandIn struct {
	hold     chan struct{}
	panics   bool
	contexts chan context.Context
	closed   chan struct{}
}

func newVerticaStandIn() *verticaStandIn {
	return &verticaStandIn{contexts: make(chan context.Context, 16), closed: make(chan struct{})}
}

func (v *verticaStandIn) call(ctx context.Context) {
	v.contexts <- ctx
	if v.panics {
		panic("index out of range [3] with length 2")
	}
	if v.hold != nil {
		<-v.hold
	}
}

func (v *verticaStandIn) Prepare(query string) (driver.Stmt, error) {
	return v.PrepareContext(context.Background(), query)
}

func (v *verticaStandIn) PrepareContext(ctx context.Context, _ string) (driver.Stmt, error) {
	v.call(ctx)
	return &verticaStandInStmt{v}, nil
}

func (v *verticaStandIn) Begin() (driver.Tx, error) { return nil, errors.New("not used") }

func (v *verticaStandIn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	v.call(ctx)
	return verticaStandInTx{}, nil
}

func (v *verticaStandIn) Ping(ctx context.Context) error { v.call(ctx); return nil }

func (v *verticaStandIn) ResetSession(context.Context) error { return nil }

func (v *verticaStandIn) Close() error { close(v.closed); return nil }

type verticaStandInTx struct{}

func (verticaStandInTx) Commit() error   { return nil }
func (verticaStandInTx) Rollback() error { return nil }

type verticaStandInStmt struct{ v *verticaStandIn }

func (*verticaStandInStmt) Close() error  { return nil }
func (*verticaStandInStmt) NumInput() int { return -1 }
func (*verticaStandInStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.ResultNoRows, nil
}
func (*verticaStandInStmt) Query([]driver.Value) (driver.Rows, error) {
	return &verticaStandInRows{}, nil
}
func (s *verticaStandInStmt) ExecContext(ctx context.Context, _ []driver.NamedValue) (driver.Result, error) {
	s.v.call(ctx)
	return driver.ResultNoRows, nil
}

func (s *verticaStandInStmt) QueryContext(ctx context.Context, _ []driver.NamedValue) (driver.Rows, error) {
	s.v.call(ctx)
	return &verticaStandInRows{}, nil
}

// verticaStandInRows is one row of one column, one = 1.
type verticaStandInRows struct{ done bool }

func (*verticaStandInRows) Columns() []string { return []string{"one"} }
func (*verticaStandInRows) Close() error      { return nil }
func (r *verticaStandInRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done, dest[0] = true, int64(1)
	return nil
}

// verticaStandInConnector makes connections of conn through newVerticaConn.
type verticaStandInConnector struct{ conn *verticaStandIn }

func (c verticaStandInConnector) Connect(context.Context) (driver.Conn, error) {
	return newVerticaConn(c.conn)
}

func (verticaStandInConnector) Driver() driver.Driver { return sqlDriverStub{} }

// A vertica statement whose server does not answer returns at the scrape's
// deadline, saying so, though the driver's call has not returned; the
// driver is never given a context that ends, so it sends no cancel request
// of its own, over a connection of its own; the connection is given up,
// database/sql closing it, and the driver's connection is closed once the
// call still running on it returns.
func TestAVerticaStatementReturnsAtTheDeadlineThoughTheServerDoesNotAnswer(t *testing.T) {
	standIn := newVerticaStandIn()
	db := sql.OpenDB(verticaStandInConnector{standIn})
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, done, err := sqlReadOnlyTx(nil)(ctx, conn, "SELECT 1 AS one", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	done()
	for range len(standIn.contexts) {
		if c := <-standIn.contexts; c.Done() != nil {
			t.Error("the driver was given a context that can end")
		}
	}
	standIn.hold = make(chan struct{})
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, _, err = sqlReadOnlyTx(nil)(deadline, conn, "SELECT 1 AS one", nil, 0)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the vertica server had not answered when the scrape's deadline came, and the connection is given up") {
		t.Errorf("%v, want the deadline", err)
	}
	if c := <-standIn.contexts; c.Done() != nil {
		t.Error("the driver was given a context that can end")
	}
	_ = conn.Close()
	select {
	case <-standIn.closed:
		t.Error("the driver's connection was closed while a call was running on it")
	default:
	}
	close(standIn.hold)
	<-standIn.closed
}

// A vertica connection given up refuses every call after, as one
// database/sql must not keep, and says it is no longer valid; a driver that
// panics in a call fails the call, not the process, and gives the
// connection up.
func TestAGivenUpVerticaConnectionRefusesEveryCall(t *testing.T) {
	standIn := newVerticaStandIn()
	conn, err := newVerticaConn(standIn)
	if err != nil {
		t.Fatal(err)
	}
	c := conn.(*verticaConn)
	standIn.panics = true
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "the vertica driver failed on what the server sent (it panicked: index out of range") {
		t.Errorf("%v", err)
	}
	if c.IsValid() {
		t.Error("a connection whose driver panicked is still valid")
	}
	standIn.panics = false
	if _, err := c.PrepareContext(context.Background(), "SELECT 1"); !errors.Is(err, driver.ErrBadConn) {
		t.Errorf("%v, want driver.ErrBadConn", err)
	}
	if err := c.ResetSession(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Errorf("%v, want driver.ErrBadConn", err)
	}
	_ = c.Close()
	<-standIn.closed
	if len(standIn.contexts) != 1 {
		t.Errorf("the driver was called %d times, want once", len(standIn.contexts))
	}
}

// A vertica target whose addresses could not be looked up fails before any
// connection is made, though the collector names no target list, only the
// cloud metadata addresses being refused, since the driver would look the
// name up itself and connect to what that gives, which nothing has held to
// the policy.
func TestAVerticaTargetThatCouldNotBeLookedUpFails(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("lookup timed out")
	}
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverVertica, "SELECT 1 AS one")
	_, err := FetchCollector(context.Background(), "db.example.com", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "db.example.com has to be looked up here to hold its addresses to the target lists before the driver, which dials by itself, connects, and the lookup failed") {
		t.Errorf("%v", err)
	}
	if made := f.madeWith(); len(made) != 0 {
		t.Errorf("%d connections were made", len(made))
	}
}

// tls.ca_file or tls.server_name beside vertica's tlsmode server, which
// does not verify the certificate, is refused, naming server-strict.
func TestVerticaVerificationKeysBesideTLSModeServerAreRefused(t *testing.T) {
	d := sqlDriverTable[SQLDriverVertica]
	for _, keys := range []model.TLSConfig{{CAFile: "ca.pem"}, {ServerName: "db.internal"}} {
		if err := checkSQLOptions(d, map[string]string{"tlsmode": "server"}, keys); err == nil || !strings.Contains(err.Error(), "set tlsmode to server-strict to verify it with them") {
			t.Errorf("%+v: %v", keys, err)
		}
		if err := checkSQLOptions(d, map[string]string{"tlsmode": "server-strict"}, keys); err != nil {
			t.Errorf("%+v beside server-strict: %v", keys, err)
		}
	}
}
