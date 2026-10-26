//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A statement that returns more than max_rows is ended before its rows are
// closed: of a database whose driver reads the rest of a result when it is
// closed, as mysql's and pgx's do, the rows past the one that went over are
// not read, and the scrape fails naming max_rows. A statement whose rows are
// all read is read to its end as before.
func TestASQLStatementOverMaxRowsIsEndedBeforeTheRestIsRead(t *testing.T) {
	f := newSQLFixture(t)
	f.db.DrainOnClose = true
	rows := make([][]driver.Value, 1000)
	for i := range rows {
		rows[i] = []driver.Value{int64(i)}
	}
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: rows}
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { sqlOwn(r, func(o *SQLConfig) { o.MaxRows = 1 }) })
	_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 10 * time.Second}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the statement returned more than request.max_rows, 1, rows") {
		t.Fatalf("%v", err)
	}
	if read := f.db.RowsRead(); read != 2 {
		t.Errorf("%d rows were read of a statement over max_rows 1, want the 2 the exporter read", read)
	}
	all := f.collector(t, "all", SQLDriverPostgres, "q")
	resp, err := FetchCollector(context.Background(), "db.example", all, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Rows) != 1000 || f.db.RowsRead() != 1002 {
		t.Errorf("%d rows answered and %d read in all, want 1000 and 1002", len(resp.Rows), f.db.RowsRead())
	}
}

// A reload that closes a pool while a scrape runs a statement on it and
// another waits at its max_per_target lets both finish on it: the pool is
// closed when the last is done, and the connection the waiting scrape made
// is counted, its collector being still configured. A scrape after the
// reload makes a pool of the new settings.
func TestAReloadLetsTheScrapesUsingASQLPoolFinishOnIt(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 4)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	one := 1
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		sqlOwn(r, func(o *SQLConfig) { o.Connections = &model.SQLConnections{MaxPerTarget: &one} })
	})
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
	editSQL(&changed, func(r *SQLConfig) { r.Database = "other" })
	FollowConfiguration([]model.Collector{changed}, nil)
	events.await(t, "closed: reload", 1)
	if size := sqlPools.size(); size != 0 {
		t.Errorf("%d pools kept after the reload", size)
	}
	if open, _ := sqlPools.collectorConnections("db"); open != 1 {
		t.Errorf("%d connections open while the closed pool's scrape runs, want its 1", open)
	}
	close(f.db.Block)
	if err := <-first; err != nil {
		t.Errorf("the running scrape: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("the scrape waiting at max_per_target: %v", err)
	}
	events.await(t, "released", 1)
	if _, err := FetchCollector(context.Background(), "db.example", &changed, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if size := sqlPools.size(); size != 1 {
		t.Errorf("%d pools after a scrape of the new settings, want 1", size)
	}
	testutil.WaitFor(t, "the closed pool's connections to be closed", func() bool { return f.db.Closed() == 2 })
	open, made := sqlPools.collectorConnections("db")
	if open != 1 || made != 3 || f.db.Opened() != 3 {
		t.Errorf("open %d and made %d connections, the database opened %d; want 1, 3 and 3", open, made, f.db.Opened())
	}
	if waited, failed := sqlPools.limitCounts("db"); waited != 1 || failed != 0 {
		t.Errorf("waited %d, failed %d", waited, failed)
	}
}

// With keepalive check ping and an idle_timeout, the ping does not keep a
// connection: one no scrape has had for idle_timeout is closed by the
// keepalive, not pinged, though pings came in the meantime, and not before.
func TestAPingedSQLConnectionIsClosedAtItsIdleTimeout(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	interval, idle := model.Duration(5*time.Millisecond), model.Duration(200*time.Millisecond)
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Username: "u", Own: SQLConfig{Driver: SQLDriverPostgres, Statement: "q", Connections: &model.SQLConnections{IdleTimeout: &idle, Keepalive: &model.SQLKeepalive{Interval: &interval, Check: sqlCheckPing}}}}}
	start := time.Now()
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	events.await(t, "keepalive", 2)
	events.await(t, "idle closed", 1)
	if since := time.Since(start); since < time.Duration(idle) {
		t.Errorf("the connection was closed %s after the scrape began, before its idle_timeout %s", since, time.Duration(idle))
	}
	if open, _ := sqlPools.collectorConnections("db"); open != 0 || f.db.Closed() != 1 {
		t.Errorf("%d connections open, %d closed, after the idle_timeout", open, f.db.Closed())
	}
	if failures := sqlPools.collectorPools("db").KeepaliveFailures; failures != 0 {
		t.Errorf("closing the idle connection counted %d keepalive failures", failures)
	}
}
