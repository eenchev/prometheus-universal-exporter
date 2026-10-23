//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sqlProbe is a scrape of c on target with param_db database.
func sqlProbe(c *model.Collector, target, database string, timeout time.Duration) error {
	_, err := FetchCollector(context.Background(), target, c, RequestOverrides{Timeout: timeout, Params: map[string]string{"param_db": database}}, nil)
	return err
}

// awaitSQL waits for done, failing the test when it has not come in 30
// seconds, the guard of a wait that would otherwise hang.
func awaitSQL(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not happen in 30s", what)
	}
}

// A target keeps no more pools, one for each database a probe names, than
// its max_per_target: a probe of another database at the limit closes the
// pool used longest ago that no scrape uses, and fails, counted as a limit
// failure, naming the limit, when every pool is in use.
func TestATargetKeepsNoMoreSQLPoolsThanItsMaxPerTarget(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	two := 2
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Database = "{{param_db}}"
		r.Connections = &model.SQLConnections{MaxPerTarget: &two}
	})
	for _, database := range []string{"a", "b", "a", "c"} {
		if err := sqlProbe(c, "db.example", database, 0); err != nil {
			t.Fatal(err)
		}
	}
	pools := sqlPoolsOf("db")
	if len(pools) != 2 || pools["db.example/a/metrics"] == nil || pools["db.example/c/metrics"] == nil || events.count("closed: pools per target") != 1 {
		t.Errorf("pools %v, events %v; want b's, used longest ago, closed for c's", pools, events.seen)
	}

	one := 1
	busy := f.collector(t, "busy", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Database = "{{param_db}}"
		r.Connections = &model.SQLConnections{MaxPerTarget: &one}
	})
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 1)
	first := make(chan error, 1)
	go func() { first <- sqlProbe(busy, "db.example", "a", 0) }()
	<-f.db.Started
	err := sqlProbe(busy, "db.example", "b", 100*time.Millisecond)
	if err == nil || err.Error() != `collector "busy" has request.connections.max_per_target, 1, pools open to db.example:5432, one for each database and account scraped there, each in use by a scrape, and a target keeps no more pools than it may have connections; raise max_per_target` {
		t.Errorf("a probe of another database while the only pool is in use: %v", err)
	}
	close(f.db.Block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, failed := sqlPools.limitCounts("busy"); failed != 1 {
		t.Errorf("%d limit failures, want the refused pool's 1", failed)
	}
}

// A target without a max_per_target keeps no more than
// sqlMaxPoolsPerTarget pools: probes naming one database more close the
// pool used longest ago.
func TestATargetWithoutMaxPerTargetKeepsABoundedNumberOfSQLPools(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	zero := 0
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Database = "{{param_db}}"
		r.Connections = &model.SQLConnections{MaxPerTarget: &zero}
	})
	for i := range sqlMaxPoolsPerTarget + 1 {
		if err := sqlProbe(c, "db.example", fmt.Sprintf("d%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	pools := sqlPoolsOf("db")
	if len(pools) != sqlMaxPoolsPerTarget || pools["db.example/d0/metrics"] != nil || events.count("closed: pools per target") != 1 {
		t.Errorf("%d pools, d0's kept %v, %d closed; want %d, d0's closed", len(pools), pools["db.example/d0/metrics"] != nil, events.count("closed: pools per target"), sqlMaxPoolsPerTarget)
	}
}

// sqlSlowClose is a stand-in connection whose Close, once armed, says it
// began and waits for hang, as a driver's Close that waits on the network
// does (gosnowflake's posts the session's logout).
type sqlSlowClose struct {
	driver.Conn
	armed   *atomic.Bool
	closing chan struct{}
	hang    chan struct{}
}

func (c sqlSlowClose) Close() error {
	if c.armed.Load() {
		c.closing <- struct{}{}
		<-c.hang
	}
	return c.Conn.Close()
}

func (c sqlSlowClose) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c sqlSlowClose) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

// slowCloses makes the fixture's connections close slowly once armed,
// until release is called, at the latest when the test ends.
func slowCloses(t *testing.T, f *sqlFixture) (armed *atomic.Bool, closing chan struct{}, release func()) {
	t.Helper()
	armed, closing = &atomic.Bool{}, make(chan struct{}, 4)
	hang := make(chan struct{})
	release = sync.OnceFunc(func() { close(hang) })
	t.Cleanup(release)
	hook := func(ctx context.Context, _ string, _ *sqlConnectSpec) (driver.Conn, error) {
		inner, err := f.db.Connect(ctx)
		return sqlSlowClose{inner, armed, closing, hang}, err
	}
	sqlConnectHook.Store(&hook)
	return armed, closing, release
}

// A reload that closes a pool a scrape is using closes the pool's idle
// connections apart, not under the pools' lock: while a driver's Close
// waits on the network, a reload returns and the pools of every collector
// are read.
func TestAReloadClosesASQLPoolsIdleConnectionsOutsideThePoolsLock(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	armed, closing, release := slowCloses(t, f)
	two := 2
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Connections = &model.SQLConnections{MaxIdlePerTarget: &two}
	})
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 4)
	errs := make(chan error, 3)
	scrape := func() {
		_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		errs <- err
	}
	go scrape()
	go scrape()
	<-f.db.Started
	<-f.db.Started
	close(f.db.Block)
	<-errs
	<-errs
	f.db.Block = make(chan struct{})
	go scrape()
	<-f.db.Started
	armed.Store(true)
	changed := *c
	changed.Request.Database = "other"
	followed := make(chan struct{})
	go func() { FollowConfiguration([]model.Collector{changed}, nil); close(followed) }()
	<-closing
	awaitSQL(t, "the reload's return while an idle connection closes", followed)
	read := make(chan struct{})
	go func() { sqlPools.collectorConnections("another"); close(read) }()
	awaitSQL(t, "another collector's read of the pools while an idle connection closes", read)
	release()
	close(f.db.Block)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

// A scrape that closes another database's idle connection to make room at
// max_per_target closes it apart, and waits for it no longer than its
// deadline: a driver's Close that hangs does not hold the scrape.
func TestMakingRoomAtMaxPerTargetDoesNotHoldTheScrapePastItsDeadline(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	armed, closing, _ := slowCloses(t, f)
	two := 2
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Database = "{{param_db}}"
		r.Connections = &model.SQLConnections{MaxPerTarget: &two, MaxIdlePerTarget: &two}
	})
	// Database a's pool keeps two connections idle.
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- sqlProbe(c, "db.example", "a", 0) }()
	}
	<-f.db.Started
	<-f.db.Started
	close(f.db.Block)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	f.db.Block = nil
	armed.Store(true)
	returned := make(chan struct{})
	var err error
	go func() { err = sqlProbe(c, "db.example", "b", 50*time.Millisecond); close(returned) }()
	<-closing
	awaitSQL(t, "the scrape's return at its deadline", returned)
	if err == nil || !strings.Contains(err.Error(), "the scrape's deadline came first") {
		t.Errorf("the scrape while the connection it made room with closes: %v, want it to end at its deadline", err)
	}
}

// A statement whose end does not return, a rollback that waits on a server
// that stops answering as go-sql-driver/mysql's Rollback waits with no
// context, does not hold the scrape past its deadline: the scrape answers
// the rows it read, the connection's network connection is closed under the
// driver, the connection is closed and never given back, and its place at
// max_per_target is free only once it is closed, so that the next scrape
// makes a new connection with no more than max_per_target open.
func TestAStatementsEndThatHangsDoesNotHoldTheSQLScrapePastItsDeadline(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	f.db.HangRollbacks(true)
	server, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accepted []net.Conn
	t.Cleanup(func() {
		_ = server.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := server.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()
	var over atomic.Bool
	hook := func(ctx context.Context, _ string, spec *sqlConnectSpec) (driver.Conn, error) {
		if f.db.Opened()-f.db.Closed() >= 1 {
			over.Store(true)
		}
		nc, err := spec.dial(ctx, "tcp", spec.hostPort)
		if err != nil {
			return nil, err
		}
		return f.db.ConnectOver(ctx, nc)
	}
	sqlConnectHook.Store(&hook)
	one := 1
	c := f.collector(t, "db", SQLDriverMySQL, "q", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &one} })
	target := server.Addr().String()
	returned := make(chan struct{})
	var resp *HTTPResponse
	go func() {
		resp, err = FetchCollector(context.Background(), target, c, RequestOverrides{Timeout: 50 * time.Millisecond}, nil)
		close(returned)
	}()
	awaitSQL(t, "the scrape's return at its deadline", returned)
	if err != nil || resp == nil || len(resp.Rows) != 1 {
		t.Fatalf("the scrape whose rollback hangs: %v, %+v; want the row it read", err, resp)
	}
	events.await(t, "end abandoned", 1)
	f.db.HangRollbacks(false)
	if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{Timeout: 30 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, "the first connection to be closed", func() bool { return f.db.Closed() >= 1 })
	if f.db.Opened() != 2 || over.Load() {
		t.Errorf("%d connections made, one made while another was open %v; want the abandoned one closed, never reused, before the second", f.db.Opened(), over.Load())
	}
}

// database/sql never closes a scrape's connection behind it: a transaction
// still open when the scrape's deadline comes is rolled back by the scrape,
// at the statement's end, not by database/sql's own goroutine, whose close
// of the connection would race the scrape's use of it (Conn.Raw, which a
// close completing inside it hands a nil connection, a panic). The scrape
// answers the rows it read, and the connection, whose driver cannot reset
// its session, is closed after the rollback, never reused, as database/sql
// would have closed it.
func TestASQLConnectionIsNotClosedBehindTheScrapeAtItsDeadline(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	f.db.Closes = make(chan struct{}, 4)
	const timeout = 50 * time.Millisecond
	var behind atomic.Bool
	// The statement's end waits past the deadline, and as long again for
	// a close of the connection, which database/sql's goroutine made at once
	// when the transaction had the scrape's context.
	hook := func(event, _ string) {
		if event != "ending" {
			return
		}
		time.Sleep(timeout + 10*time.Millisecond)
		select {
		case <-f.db.Closes:
			behind.Store(true)
		case <-time.After(300 * time.Millisecond):
		}
	}
	sqlPoolEvent.Store(&hook)
	t.Cleanup(func() { sqlPoolEvent.Store(nil) })
	c := f.collector(t, "db", SQLDriverPostgres, "q")
	resp, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: timeout}, nil)
	if err != nil || resp == nil || len(resp.Rows) != 1 {
		t.Fatalf("%v, %+v; want the row read", err, resp)
	}
	if behind.Load() {
		t.Fatal("the connection was closed at the deadline while the scrape still held it")
	}
	testutil.WaitFor(t, "the connection to be closed", func() bool { return f.db.Closed() == 1 })
	if f.db.RolledBack() != 1 {
		t.Errorf("rolled back %d transactions, want 1", f.db.RolledBack())
	}
	sqlPoolEvent.Store(nil)
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 30 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	if f.db.Opened() != 2 {
		t.Errorf("%d connections made, want the first closed and a second made", f.db.Opened())
	}
}

// Many statements in transactions, run at once and each still running at
// its scrape's deadline, each fail saying the deadline came first, none
// panicking however database/sql's goroutines and the scrapes' interleave.
func TestSQLTransactionsEndedAtTheirDeadlineAtOnceEachFailSo(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Block = make(chan struct{})
	f.db.Results["slow"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	workers := 16
	c := f.collector(t, "db", SQLDriverPostgres, "slow", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &workers} })
	var scrapes sync.WaitGroup
	for range workers {
		scrapes.Go(func() {
			for range 40 {
				_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 10 * time.Millisecond}, nil)
				if err == nil || !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("%v, want the deadline's failure", err)
					return
				}
			}
		})
	}
	scrapes.Wait()
}

// A pool a reload closed while a scrape used it is no longer counted once
// a later reload removes its collector: the connection the waiting scrape
// makes after the removal is counted for no collector.
func TestADrainingSQLPoolOfARemovedCollectorIsNoLongerCounted(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 4)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	one := 1
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &one} })
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		first <- err
	}()
	<-f.db.Started
	go func() {
		_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		second <- err
	}()
	events.await(t, "waiting: max_per_target", 1)
	changed := *c
	changed.Request.Database = "other"
	FollowConfiguration([]model.Collector{changed}, nil)
	FollowConfiguration(nil, nil)
	close(f.db.Block)
	<-first
	<-second
	events.await(t, "released", 1)
	sqlPools.mu.Lock()
	connects, kept := sqlPools.connects["db"]
	draining := len(sqlPools.draining)
	sqlPools.mu.Unlock()
	if kept || draining != 0 {
		t.Errorf("after the collector was removed: connects %d kept %v, %d pools draining", connects, kept, draining)
	}
}

// With keepalive check ping, every idle connection of a pool is pinged
// each interval, not only the one given back last, which database/sql
// hands out first.
func TestASQLKeepalivePingsEveryIdleConnection(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	two, interval := 2, model.Duration(5*time.Millisecond)
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverPostgres, Statement: "q", Username: "u",
		Connections: &model.SQLConnections{MaxIdlePerTarget: &two, Keepalive: &model.SQLKeepalive{Interval: &interval, Check: sqlCheckPing}}}}
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
			errs <- err
		}()
	}
	<-f.db.Started
	<-f.db.Started
	close(f.db.Block)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	events.await(t, "keepalive", 6)
	if pinged := f.db.PingedConnections(); pinged != 2 || f.db.Opened() != 2 {
		t.Errorf("%d of %d idle connections pinged", pinged, f.db.Opened())
	}
}

// A keepalive whose ping is skipped, every place at max_per_target taken,
// still forgets the connections no scrape has had for the idle_timeout.
func TestASkippedSQLKeepaliveForgetsConnectionsPastTheirIdleTimeout(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	one, interval, idle := 1, model.Duration(5*time.Millisecond), model.Duration(time.Hour)
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverPostgres, Statement: "q", Username: "u",
		Connections: &model.SQLConnections{MaxPerTarget: &one, IdleTimeout: &idle, Keepalive: &model.SQLKeepalive{Interval: &interval, Check: sqlCheckPing}}}}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	p := sqlPoolsOf("db")["db.example//u"]
	p.target.sem <- struct{}{}
	defer func() { <-p.target.sem }()
	gone := new(int)
	p.idle.mu.Lock()
	p.idle.used[gone] = time.Now().Add(-2 * time.Hour)
	p.idle.mu.Unlock()
	events.await(t, "keepalive skipped", 2)
	p.idle.mu.Lock()
	_, kept := p.idle.used[gone]
	p.idle.mu.Unlock()
	if kept {
		t.Errorf("a connection no scrape had for twice the idle_timeout is still remembered after skipped pings")
	}
}

// max_idle_per_target is of the target, across its pools: a server
// scraped with two databases keeps one connection idle at
// max_idle_per_target 1, not one for each, and two at 2.
func TestMaxIdlePerTargetBoundsTheIdleSQLConnectionsAcrossTheTargetsDatabases(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	for _, limit := range []int{1, 2} {
		name := fmt.Sprintf("idle%d", limit)
		c := f.collector(t, name, SQLDriverPostgres, "q", func(r *model.RequestConfig) {
			r.Database = "{{param_db}}"
			r.Connections = &model.SQLConnections{MaxIdlePerTarget: &limit}
		})
		for _, database := range []string{"a", "b", "a", "b"} {
			if err := sqlProbe(c, "db.example", database, 0); err != nil {
				t.Fatal(err)
			}
		}
		if stats := sqlPools.collectorPools(name); stats.Targets != 1 || stats.Idle != limit {
			t.Errorf("max_idle_per_target %d: %d targets, %d idle connections", limit, stats.Targets, stats.Idle)
		}
	}
}

// A connection given back to a pool a reload has closed is closed, not kept
// idle, whenever the pool's idle connections are closed apart: one kept
// would be taken by a scrape still waiting on the pool, and the pool's
// connections closed in another order than its scrapes ended.
func TestAConnectionGivenBackToAClosedSQLPoolIsClosed(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "q")
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	p := sqlPoolsOf("db")["db.example//metrics"]
	conn, err := p.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Closed as closeLocked marks it, before its idle connections are.
	sqlPools.mu.Lock()
	p.closed = true
	sqlPools.mu.Unlock()
	sqlPools.giveBack(p, conn)
	if idle := p.db.Stats().Idle; idle != 0 || f.db.Closed() != 1 {
		t.Errorf("%d connections idle, %d closed, after one was given back to a closed pool", idle, f.db.Closed())
	}
	// Open again, so that the test's end closes and forgets it as any
	// other: a pool marked closed is one closeLocked passes over.
	sqlPools.mu.Lock()
	p.closed = false
	sqlPools.mu.Unlock()
}
