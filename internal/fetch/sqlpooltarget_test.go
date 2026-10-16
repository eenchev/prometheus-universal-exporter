//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
)

// sqlPoolsOf is the pools of a collector open now, by the host, database
// and account they reach.
func sqlPoolsOf(collector string) map[string]*sqlPool {
	sqlPools.mu.Lock()
	defer sqlPools.mu.Unlock()
	pools := map[string]*sqlPool{}
	for key, p := range sqlPools.pools {
		if key.collector == collector {
			pools[key.address.host+"/"+key.database+"/"+key.base.username] = p
		}
	}
	return pools
}

// A keepalive ping holds one of the target's places at max_per_target while
// it pings: with max_per_target 1 and a ping that hangs, a scrape waits at
// the limit, counted, and fails there at its deadline, rather than taking the
// place and then waiting inside the pool for the connection the ping has, or
// having the pool make a second one; the ping is given the keepalive
// interval, and when it fails at its end, counted as a failed keepalive, the
// place is free again and a waiting scrape goes on, on the one connection.
func TestAHangingSQLKeepalivePingHoldsOnePlaceAtMaxPerTargetUntilItsInterval(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	one, interval := 1, model.Duration(500*time.Millisecond)
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverPostgres, Statement: "q", Username: "u",
		Connections: &model.SQLConnections{MaxPerTarget: &one, Keepalive: &model.SQLKeepalive{Interval: &interval, Check: sqlCheckPing}}}}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	pinging := make(chan struct{}, 1)
	f.db.HangPings(true, pinging)
	t.Cleanup(func() { f.db.HangPings(false, nil) })
	<-pinging
	_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 20 * time.Millisecond}, nil)
	if err == nil || !strings.Contains(err.Error(), `collector "db" has request.connections.max_per_target, 1, connections to db.example:5432, and none was free before the scrape's deadline`) {
		t.Errorf("a scrape while the ping hangs: %v, want it to wait at max_per_target", err)
	}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 30 * time.Second}, nil); err != nil {
		t.Fatalf("a scrape waiting for the ping's place: %v", err)
	}
	// The ping gives its place back before the keepalive reports the
	// failure, so the report is awaited rather than expected at once.
	events.await(t, "keepalive failed", 1)
	if failures := sqlPools.collectorPools("db").KeepaliveFailures; failures < 1 {
		t.Errorf("%d keepalive failures; a ping that did not answer within the interval is one", failures)
	}
	if waited, failed := sqlPools.limitCounts("db"); waited < 2 || failed != 1 {
		t.Errorf("waited %d, failed %d at max_per_target", waited, failed)
	}
	if f.db.Opened() != 1 {
		t.Errorf("%d connections made, with max_per_target 1", f.db.Opened())
	}
}

// A keepalive ping does not wait for a place at max_per_target: while the
// scrapes hold every place, the pool's ping is skipped, and none is sent.
func TestASQLKeepaliveSkipsItsPingWhileTheScrapesHoldEveryPlace(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	one, interval := 1, model.Duration(5*time.Millisecond)
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverPostgres, Statement: "q", Username: "u",
		Connections: &model.SQLConnections{MaxPerTarget: &one, Keepalive: &model.SQLKeepalive{Interval: &interval, Check: sqlCheckPing}}}}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	events.await(t, "keepalive", 1)
	sem := sqlPoolsOf("db")["db.example//u"].target.sem
	sem <- struct{}{}
	pings := f.db.Pings()
	events.await(t, "keepalive skipped", 2)
	if f.db.Pings() != pings {
		t.Errorf("%d pings sent while every place was taken", f.db.Pings()-pings)
	}
	<-sem
	events.await(t, "keepalive", 2)
}

// A keepalive ping makes no connection: when the pool's idle connection is
// gone by the time the ping takes one, the ping is skipped, not sent on a
// connection made for it, and is no failure.
func TestASQLKeepalivePingMakesNoConnection(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "q")
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	p := sqlPoolsOf("db")["db.example//metrics"]
	p.db.SetMaxIdleConns(0)
	round := keepaliveOnce(sqlPools, p, time.Second)
	if round != (sqlKeepaliveRound{skipped: true}) || f.db.Opened() != 1 || f.db.Pings() != 0 {
		t.Errorf("round %+v; %d connections made, %d pings", round, f.db.Opened(), f.db.Pings())
	}
}

// The pools of the databases a collector reaches on one server are one
// target's: one server probed with three databases counts once in
// http_exporter_sql_targets and in max_targets, and another target at the
// limit closes every pool of the target used longest ago.
func TestTheSQLPoolsOfOneServersDatabasesAreOneTarget(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	two, three := 2, 3
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Database = "{{param_db}}"
		// A target keeps no more pools than its max_per_target.
		r.Connections = &model.SQLConnections{MaxTargets: &two, MaxPerTarget: &three}
	})
	scrape := func(target, database string) {
		t.Helper()
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{Params: map[string]string{"param_db": database}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, database := range []string{"a", "b", "c"} {
		scrape("one.example", database)
	}
	if targets, pools := sqlPools.collectorPools("db").Targets, sqlPools.size(); targets != 1 || pools != 3 {
		t.Errorf("one server with three databases: %d targets, %d pools", targets, pools)
	}
	scrape("two.example", "a")
	if events.count("closed: max_targets") != 0 || sqlPools.collectorPools("db").Targets != 2 {
		t.Errorf("a second target at max_targets 2 closed pools: %v", events.seen)
	}
	scrape("three.example", "a")
	events.await(t, "closed: max_targets", 3)
	var pools []string
	for name := range sqlPoolsOf("db") {
		pools = append(pools, name)
	}
	if targets := sqlPools.collectorPools("db").Targets; targets != 2 || len(pools) != 2 || strings.Contains(strings.Join(pools, " "), "one.example") {
		t.Errorf("after a third target: %d targets, pools %v; want one.example's three closed", targets, pools)
	}
}

// max_per_target bounds the connections to one server across the pools of
// its databases: while two scrapes of one database hold both places, a
// scrape of another waits and fails at the limit; and a scrape that would
// make a connection while another database's pool keeps one idle closes
// that one first, so that no more than max_per_target are open at once.
func TestMaxPerTargetBoundsTheConnectionsToAServerAcrossItsDatabases(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 4)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	// Two places, so that the target may keep two pools.
	two := 2
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Database = "{{param_db}}"
		r.Connections = &model.SQLConnections{MaxPerTarget: &two, MaxIdlePerTarget: &two}
	})
	of := func(database string) RequestOverrides {
		return RequestOverrides{Params: map[string]string{"param_db": database}}
	}
	first := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := FetchCollector(context.Background(), "db.example", c, of("a"), nil)
			first <- err
		}()
	}
	<-f.db.Started
	<-f.db.Started
	waiting := of("b")
	waiting.Timeout = 20 * time.Millisecond
	_, err := FetchCollector(context.Background(), "db.example", c, waiting, nil)
	if err == nil || !strings.Contains(err.Error(), `collector "db" has request.connections.max_per_target, 2, connections to db.example:5432`) {
		t.Errorf("a scrape of another database while both places are held: %v", err)
	}
	close(f.db.Block)
	for range 2 {
		if err := <-first; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := FetchCollector(context.Background(), "db.example", c, of("b"), nil); err != nil {
		t.Fatal(err)
	}
	if open, _ := sqlPools.collectorConnections("db"); open != 2 || f.db.Opened() != 3 || f.db.Closed() != 1 || events.count("room made") != 1 {
		t.Errorf("open %d, made %d, closed %d, %v; want one of database a's idle connections closed before b's was made", open, f.db.Opened(), f.db.Closed(), events.seen)
	}
	if waited, failed := sqlPools.limitCounts("db"); waited != 1 || failed != 1 {
		t.Errorf("waited %d, failed %d", waited, failed)
	}
}

// A static target whose connections block gives it other settings is a
// target of its own, with its own places at max_per_target, though it
// reaches a server the collector's probes reach; one that sets only its own
// account shares the probes' target and its places.
func TestAStaticTargetWithItsOwnConnectionSettingsIsATargetOfItsOwn(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	// Two places, so that the probes' target may keep the pools of two
	// accounts.
	two, three := 2, 3
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &two} })
	own := &model.StaticTarget{Name: "own", Collector: "db", Target: "db.example", Request: model.TargetRequestConfig{Username: "owner", Connections: &model.SQLConnections{MaxPerTarget: &three}}}
	account := &model.StaticTarget{Name: "account", Collector: "db", Target: "db.example", Request: model.TargetRequestConfig{Username: "reader"}}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*model.StaticTarget{own, account} {
		if _, err := FetchCollector(context.Background(), target.Target, c, TargetOverrides(target), nil); err != nil {
			t.Fatal(err)
		}
	}
	pools := sqlPoolsOf("db")
	probe, ownPool, accountPool := pools["db.example//metrics"], pools["db.example//owner"], pools["db.example//reader"]
	switch {
	case len(pools) != 3 || probe == nil || ownPool == nil || accountPool == nil:
		t.Fatalf("pools %v", pools)
	case probe.target != accountPool.target || cap(probe.target.sem) != 2:
		t.Errorf("the target with its own account has places of its own: %+v and %+v", probe.target.key, accountPool.target.key)
	case ownPool.target == probe.target || cap(ownPool.target.sem) != 3:
		t.Errorf("the target with its own max_per_target shares the probes' places: %+v", ownPool.target.key)
	}
	if targets := sqlPools.collectorPools("db").Targets; targets != 2 {
		t.Errorf("%d targets, want the probes' and the one with its own settings", targets)
	}
}
