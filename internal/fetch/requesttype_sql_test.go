//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sqlFixture is a stand-in database that every connection of the sql
// request type reaches in place of its driver, through the type's pools and
// connectors, and a password file to reach it with. specs are what each
// connection was made with.
type sqlFixture struct {
	db           *sqltest.DB
	passwordFile string
	mu           sync.Mutex
	specs        []*sqlConnectSpec
}

func newSQLFixture(t *testing.T) *sqlFixture {
	t.Helper()
	f := &sqlFixture{db: &sqltest.DB{Results: map[string]sqltest.Result{}}}
	f.passwordFile = testutil.WriteIn(t, t.TempDir(), "password", "s3cret\n")
	hook := func(ctx context.Context, _ string, spec *sqlConnectSpec) (driver.Conn, error) {
		f.mu.Lock()
		f.specs = append(f.specs, spec)
		f.mu.Unlock()
		return f.db.Connect(ctx)
	}
	sqlConnectHook.Store(&hook)
	t.Cleanup(func() {
		sqlConnectHook.Store(nil)
		sqlPools.closeAll()
	})
	return f
}

// madeWith is what the connections were made with, in order.
func (f *sqlFixture) madeWith() []*sqlConnectSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.specs)
}

// sqlCollector is a validated sql collector of driver running statement
// with the fixture's password, changed by change first.
func (f *sqlFixture) collector(t *testing.T, name, driverName, statement string, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := &model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: RequestTypeSQL, Driver: driverName, Statement: statement, Username: "metrics", PasswordFile: f.passwordFile},
		Transform:     model.TransformConfig{Type: "jq"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
	for _, apply := range change {
		apply(&c.Request)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// sqlEvents records the pools' events until the test ends, and waits for
// one.
type sqlEvents struct {
	mu     sync.Mutex
	seen   []string
	signal chan struct{}
}

func watchSQLEvents(t *testing.T) *sqlEvents {
	t.Helper()
	e := &sqlEvents{signal: make(chan struct{}, 1)}
	hook := func(event, _ string) {
		e.mu.Lock()
		e.seen = append(e.seen, event)
		e.mu.Unlock()
		select {
		case e.signal <- struct{}{}:
		default:
		}
	}
	sqlPoolEvent.Store(&hook)
	t.Cleanup(func() { sqlPoolEvent.Store(nil) })
	return e
}

// count is how many times event happened.
func (e *sqlEvents) count(event string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, seen := range e.seen {
		if seen == event {
			n++
		}
	}
	return n
}

// await waits for event to have happened n times.
func (e *sqlEvents) await(t *testing.T, event string, n int) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for e.count(event) < n {
		select {
		case <-e.signal:
		case <-deadline.C:
			t.Fatalf("no %d %q events: %v", n, event, e.seen)
		}
	}
}

// A statement's rows reach the transforms as JSON values: integers and
// floats as numbers, a decimal column's text as the number it writes, a
// boolean as 1 or 0, a time as its Unix seconds, text and UTF-8 bytes as
// text, a driver's own value by the text it writes, and NULL as a column
// left out of its row. The answer's body is the same rows as JSON, its
// status none, and the probe's target and collector are on it.
func TestASQLStatementsRowsAreJSONValues(t *testing.T) {
	f := newSQLFixture(t)
	when := time.Date(2026, 10, 14, 8, 30, 15, 500_000_000, time.UTC)
	f.db.Results["SELECT *"] = sqltest.Result{
		Columns: []string{"n", "f", "d", "b", "t", "s", "raw", "big", "null"},
		Types:   []string{"INT8", "FLOAT8", "NUMERIC", "BOOL", "TIMESTAMPTZ", "TEXT", "BYTEA", "NUMERIC", "TEXT"},
		Rows: [][]driver.Value{
			{int64(42), 1.5, "12.50", true, when, "ok", []byte("bytes"), big.NewInt(7), nil},
			{int64(-1), 0.0, "99999999999999999999", false, when, "", []byte(""), big.NewInt(0), "x"},
		},
	}
	c := f.collector(t, "db", SQLDriverPostgres, "SELECT *")
	resp, err := FetchCollector(context.Background(), "db.example:5432", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	huge, _ := new(big.Int).SetString("99999999999999999999", 10)
	seconds := float64(when.Unix()) + 0.5
	want := []any{
		map[string]any{"n": 42, "f": 1.5, "d": 12.5, "b": 1, "t": seconds, "s": "ok", "raw": "bytes", "big": 7},
		map[string]any{"n": -1, "f": 0.0, "d": huge, "b": 0, "t": seconds, "s": "", "raw": "", "big": 0, "null": "x"},
	}
	if !reflect.DeepEqual(resp.Rows, want) {
		t.Errorf("rows %#v\nwant %#v", resp.Rows, want)
	}
	var shown []any
	if err := json.Unmarshal(resp.Body, &shown); err != nil || len(shown) != 2 || strings.Contains(string(resp.Body), "null") && !strings.Contains(string(resp.Body), `"null":"x"`) {
		t.Errorf("body %s: %v", resp.Body, err)
	}
	if resp.Status() != nil || resp.Target != "db.example:5432" || resp.Collector != "db" {
		t.Errorf("status %v, target %q, collector %q", resp.Status(), resp.Target, resp.Collector)
	}
}

// A statement whose result cannot be read by column name is refused: a
// column without a name, a name given twice, binary data that is no UTF-8
// text and a value of a type the exporter does not read, each named.
func TestASQLResultThatCannotBeReadByNameIsRefused(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["unnamed"] = sqltest.Result{Columns: []string{"a", ""}, Rows: [][]driver.Value{{int64(1), int64(2)}}}
	f.db.Results["twice"] = sqltest.Result{Columns: []string{"a", "b", "a"}, Rows: [][]driver.Value{{int64(1), int64(2), int64(3)}}}
	f.db.Results["binary"] = sqltest.Result{Columns: []string{"blob"}, Types: []string{"BYTEA"}, Rows: [][]driver.Value{{[]byte{0xff, 0xfe}}}}
	f.db.Results["odd"] = sqltest.Result{Columns: []string{"c"}, Rows: [][]driver.Value{{struct{}{}}}}
	for statement, want := range map[string]string{
		"unnamed": "column 2 of the statement's result has no name; name it with AS",
		"twice":   `the statement's result names column "a" twice, as columns 1 and 3; give one of them another name with AS`,
		"binary":  `row 1 column "blob": holds binary data that is no UTF-8 text, of the column type BYTEA`,
		"odd":     `row 1 column "c": holds a value of the Go type struct {}, of the column type the driver does not name`,
	} {
		c := f.collector(t, "db", SQLDriverPostgres, statement)
		_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", statement, err, want)
		}
	}
}

// request.max_rows lets that many rows through and fails one more, as a
// limit; request.max_response_bytes bounds the values the rows hold, text by
// its length and anything else as eight bytes, each with its column's name,
// failing as a limit past it.
func TestTheRowsOfASQLStatementAreBounded(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["three"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{"aaaa"}, {"bbbb"}, {int64(1)}}}
	for _, tc := range []struct {
		maxRows  int
		maxBytes model.ByteSize
		want     string
	}{
		{3, 0, ""},
		{2, 0, "the statement returned more than request.max_rows, 2, rows"},
		{0, 19, ""},
		{0, 18, "the statement's rows hold more than the response limit of 18 bytes of values and column names"},
	} {
		c := f.collector(t, "db", SQLDriverMySQL, "three", func(r *model.RequestConfig) { r.MaxRows, r.MaxResponseBytes = tc.maxRows, tc.maxBytes })
		resp, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		switch {
		case tc.want == "" && (err != nil || len(resp.Rows) != 3):
			t.Errorf("max_rows %d, max_response_bytes %d: %v", tc.maxRows, tc.maxBytes, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, model.ErrLimitExceeded)):
			t.Errorf("max_rows %d, max_response_bytes %d: %v, want a limit's %q", tc.maxRows, tc.maxBytes, err, tc.want)
		}
	}
}

// A statement still running at the scrape's deadline is cancelled through
// its context, and the scrape fails saying the deadline came first.
func TestASQLStatementEndsAtTheScrapesDeadline(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Block = make(chan struct{})
	f.db.Results["slow"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "slow")
	_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 50 * time.Millisecond}, nil)
	if err == nil || !strings.Contains(err.Error(), "running the statement: the scrape's deadline came first") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

// Each driver runs a statement as it can make it read-only: postgres and
// mysql in a transaction begun read-only through database/sql, postgres
// with the statement timeout set to the scrape's time left first; sqlserver
// has no read-only transaction and runs it as it is. The transaction is
// rolled back.
func TestEachSQLDriverRunsTheStatementReadOnlyAsItCan(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["SELECT 1 AS one"] = sqltest.Result{Columns: []string{"one"}, Rows: [][]driver.Value{{int64(1)}}}
	for _, tc := range []struct {
		driver   string
		readOnly bool
		executed string
	}{
		{SQLDriverPostgres, true, "SET LOCAL statement_timeout = "},
		{SQLDriverMySQL, true, ""},
		{SQLDriverSQLServer, false, ""},
	} {
		begun, executed, rolled := len(f.db.TxOptions()), len(f.db.Executed()), f.db.RolledBack()
		c := f.collector(t, tc.driver, tc.driver, "SELECT 1 AS one")
		if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 5 * time.Second}, nil); err != nil {
			t.Fatalf("%s: %v", tc.driver, err)
		}
		tx := f.db.TxOptions()[begun:]
		switch {
		case tc.readOnly && (len(tx) != 1 || !tx[0].ReadOnly || f.db.RolledBack() != rolled+1):
			t.Errorf("%s: transactions %+v, rolled back %d, want one read-only, rolled back", tc.driver, tx, f.db.RolledBack()-rolled)
		case !tc.readOnly && len(tx) != 0:
			t.Errorf("%s: transactions %+v, want none", tc.driver, tx)
		}
		ran := f.db.Executed()[executed:]
		if tc.executed == "" && len(ran) != 0 || tc.executed != "" && (len(ran) != 1 || !strings.HasPrefix(ran[0], tc.executed)) {
			t.Errorf("%s: ran %q before the statement, want %q", tc.driver, ran, tc.executed)
		}
	}
	// The scrape's five seconds, less what passed before the statement.
	ran := f.db.Executed()
	if ms, err := strconv.Atoi(strings.TrimPrefix(ran[0], "SET LOCAL statement_timeout = ")); err != nil || ms <= 4000 || ms > 5000 {
		t.Errorf("statement_timeout %q", ran[0])
	}
}

// args are bound to the statement as values, in order, their placeholders
// filled from the probe's parameters; request.database's placeholders are
// filled too, and a value that could name more than a database is refused
// before anything is sent.
func TestSQLArgsAndDatabaseTakeTheProbesParameters(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.Args = []string{"{{param_queue}}", "fixed", "{{param_state:open}}"}
		r.Database = "app_{{param_tenant}}"
	})
	params := map[string]string{"param_queue": "it's; DROP TABLE x", "param_tenant": "acme"}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Params: params}, nil); err != nil {
		t.Fatal(err)
	}
	queries := f.db.Queries()
	if got := queries[len(queries)-1].Args; !reflect.DeepEqual(got, []any{"it's; DROP TABLE x", "fixed", "open"}) {
		t.Errorf("args %q", got)
	}
	if specs := f.madeWith(); specs[len(specs)-1].database != "app_acme" {
		t.Errorf("database %q", specs[len(specs)-1].database)
	}
	params["param_tenant"] = "acme;x"
	_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Params: params}, nil)
	if err == nil || !strings.Contains(err.Error(), `request.database: the value of param_tenant, "acme;x", may hold only letters, digits and _ - . $`) {
		t.Errorf("%v", err)
	}
	if err := CheckPathParams(c, RequestOverrides{Params: map[string]string{"param_tenant": "a", "param_queue": "b", "param_typo": "c"}}); err == nil || !strings.Contains(err.Error(), "no placeholder in its request.database, its args or its label values names them") {
		t.Errorf("%v", err)
	}
}

// A connection is made with the collector's account, its password read from
// its file when the connection is made, the target's host and the driver's
// port, and its database; a static target's account and password file
// replace the collector's; and a password replaced on disk is what the next
// connection sends.
func TestASQLConnectionIsMadeWithTheCollectorsSettings(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	c := f.collector(t, "db", SQLDriverMySQL, "q", func(r *model.RequestConfig) { r.Database = "app" })
	if _, err := FetchCollector(context.Background(), "DB.Example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	other := testutil.WriteIn(t, t.TempDir(), "other", "0ther\n")
	target := &model.StaticTarget{Name: "t", Collector: "db", Target: "db.example:3307", Request: model.TargetRequestConfig{Username: "backup", PasswordFile: other}}
	if _, err := FetchCollector(context.Background(), target.Target, c, TargetOverrides(target), nil); err != nil {
		t.Fatal(err)
	}
	testutil.WriteIn(t, filepath.Dir(f.passwordFile), "password", "rotated\n")
	sqlPools.closeAll()
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	specs := f.madeWith()
	got := make([]string, len(specs))
	for i, s := range specs {
		got[i] = strings.Join([]string{s.host, s.port, s.hostPort, s.username, s.password, s.database, s.appName}, " ")
	}
	want := []string{
		"db.example 3306 db.example:3306 metrics s3cret app prometheus-universal-exporter",
		"db.example 3307 db.example:3307 backup 0ther app prometheus-universal-exporter",
		"db.example 3306 db.example:3306 metrics rotated app prometheus-universal-exporter",
	}
	if !slices.Equal(got, want) {
		t.Errorf("connections made with\n%q\nwant\n%q", got, want)
	}
}

// The connection settings are applied to the pool: max_per_target as its
// limit on open connections, a static target's settings over the
// collector's, key by key; a scrape reuses the connection an earlier one
// left idle.
func TestASQLPoolHasTheCollectorsConnectionSettings(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	three := 3
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &three} })
	for range 3 {
		if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	five := 5
	target := &model.StaticTarget{Name: "t", Collector: "db", Target: "other.example", Request: model.TargetRequestConfig{Connections: &model.SQLConnections{MaxPerTarget: &five}}}
	if _, err := FetchCollector(context.Background(), target.Target, c, TargetOverrides(target), nil); err != nil {
		t.Fatal(err)
	}
	limits := map[string]int{}
	sqlPools.mu.Lock()
	for key, p := range sqlPools.pools {
		limits[key.address.host] = p.db.Stats().MaxOpenConnections
		if key.base.conns.maxIdle != defaultSQLMaxIdlePerTarget || key.base.conns.idleTimeout != defaultSQLIdleTimeout {
			t.Errorf("%s: settings %+v", key.address.host, key.base.conns)
		}
	}
	sqlPools.mu.Unlock()
	if !reflect.DeepEqual(limits, map[string]int{"db.example": 3, "other.example": 5}) {
		t.Errorf("pools' limits %v", limits)
	}
	if f.db.Opened() != 2 {
		t.Errorf("%d connections made, want one for each target", f.db.Opened())
	}
	open, made := sqlPools.collectorConnections("db")
	pools := sqlPools.collectorPools("db")
	if open != 2 || made != 2 || pools.Idle != 2 || pools.Targets != 2 || pools.Rows == nil || *pools.Rows != 1 {
		t.Errorf("open %d, made %d, pools %+v", open, made, pools)
	}
}

// At max_per_target a scrape waits for a connection, counted as a wait, and
// fails at its deadline naming the limit, counted as a failure; one that
// finds a connection freed in time goes on.
func TestASQLScrapeWaitsAtMaxPerTarget(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 4)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	one := 1
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &one} })
	first := make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		first <- err
	}()
	<-f.db.Started
	_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{Timeout: 20 * time.Millisecond}, nil)
	if err == nil || !strings.Contains(err.Error(), `collector "db" has request.connections.max_per_target, 1, connections to db.example:5432, and none was free before the scrape's deadline`) {
		t.Fatalf("%v", err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
		second <- err
	}()
	events.await(t, "waiting: max_per_target", 2)
	close(f.db.Block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if waited, failed := sqlPools.limitCounts("db"); waited != 2 || failed != 1 {
		t.Errorf("waited %d, failed %d", waited, failed)
	}
}

// At max_targets a scrape of another target closes the pool used longest
// ago, and fails, counted, when every target is in use.
func TestASQLCollectorKeepsAtMostMaxTargetsPools(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	two := 2
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxTargets: &two} })
	for _, target := range []string{"a.example", "b.example", "a.example", "c.example"} {
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	events.await(t, "closed: max_targets", 1)
	var hosts []string
	sqlPools.mu.Lock()
	for key := range sqlPools.pools {
		hosts = append(hosts, key.address.host)
	}
	sqlPools.mu.Unlock()
	slices.Sort(hosts)
	if !slices.Equal(hosts, []string{"a.example", "c.example"}) {
		t.Errorf("pools of %v, want b's closed", hosts)
	}
	f.db.Block, f.db.Started = make(chan struct{}), make(chan string, 2)
	done := make(chan error, 2)
	for _, target := range []string{"a.example", "c.example"} {
		go func() {
			_, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
			done <- err
		}()
		<-f.db.Started
	}
	_, err := FetchCollector(context.Background(), "d.example", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), `collector "db" has request.connections.max_targets, 2, targets with connections open, each in use by a scrape`) {
		t.Errorf("%v", err)
	}
	close(f.db.Block)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, failed := sqlPools.limitCounts("db"); failed != 1 {
		t.Errorf("%d failures", failed)
	}
}

// Following a configuration closes the pools of a collector it no longer
// has, or has with other connection settings, keeps those it has as they
// were, including a static target's, and forgets the counts of a collector
// gone; closing every connection at the shutdown closes the rest.
func TestFollowingAConfigurationClosesTheSQLPoolsItNoLongerUses(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	kept := f.collector(t, "kept", SQLDriverPostgres, "q")
	changed := f.collector(t, "changed", SQLDriverPostgres, "q")
	gone := f.collector(t, "gone", SQLDriverPostgres, "q")
	other := testutil.WriteIn(t, t.TempDir(), "other", "0ther\n")
	target := model.StaticTarget{Name: "t", Collector: "kept", Target: "t.example", Request: model.TargetRequestConfig{PasswordFile: other}}
	for _, c := range []*model.Collector{kept, changed, gone} {
		if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := FetchCollector(context.Background(), target.Target, kept, TargetOverrides(&target), nil); err != nil {
		t.Fatal(err)
	}
	now := *changed
	now.Request.Database = "elsewhere"
	FollowConfiguration([]model.Collector{*kept, now}, []model.StaticTarget{target})
	events.await(t, "closed: reload", 2)
	if size := sqlPools.size(); size != 2 {
		t.Errorf("%d pools after the reload, want kept's two", size)
	}
	if _, made := sqlPools.collectorConnections("gone"); made != 0 {
		t.Errorf("gone's count is %d", made)
	}
	if _, made := sqlPools.collectorConnections("kept"); made != 2 {
		t.Errorf("kept's count is %d", made)
	}
	CloseConnections()
	events.await(t, "closed: shutdown", 2)
	testutil.WaitFor(t, "every connection to be closed", func() bool { return f.db.Closed() == f.db.Opened() })
}

// With keepalive check ping, an idle connection of the pool is pinged
// every interval, and each failed ping is counted.
func TestASQLPoolPingsAnIdleConnectionWithCheckPing(t *testing.T) {
	f := newSQLFixture(t)
	events := watchSQLEvents(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	interval := model.Duration(5 * time.Millisecond)
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverPostgres, Statement: "q", Username: "u",
		Connections: &model.SQLConnections{Keepalive: &model.SQLKeepalive{Interval: &interval, Check: sqlCheckPing}}}}
	if _, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	events.await(t, "keepalive", 1)
	f.db.FailPings(errors.New("gone"))
	events.await(t, "keepalive failed", 1)
	if failures := sqlPools.collectorPools("db").KeepaliveFailures; failures < 1 {
		t.Errorf("%d keepalive failures", failures)
	}
}

// A debug probe's request section shows the statement as driver://host:port
// and the database, with the account, whether the connection was new or
// kept, and how many values were bound, but neither the values nor the
// password; and the outcome names the rows and columns.
func TestADebugProbeShowsTheSQLStatementWithoutSecrets(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"state", "n"}, Rows: [][]driver.Value{{"open", int64(3)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Database = "app"; r.Args = []string{"secret-arg"} })
	var requests []TracedRequest
	for range 2 {
		ctx, trace := WithRequestTrace(context.Background())
		if _, err := FetchCollector(ctx, "db.example", c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, trace.Requests()...)
	}
	if len(requests) != 2 {
		t.Fatalf("%d requests traced", len(requests))
	}
	for i, connection := range []string{"new", "reused"} {
		r := requests[i]
		want := http.Header{"Sql-Driver": {"postgres"}, "Sql-User": {"metrics"}, "Sql-Database": {"app"}, "Sql-Connection": {connection}, "Sql-Args": {"1"}}
		if r.Method != "QUERY" || r.URL != "postgres://db.example:5432/app" || !reflect.DeepEqual(r.Header, want) {
			t.Errorf("request %d: %s %s %v", i, r.Method, r.URL, r.Header)
		}
		shown := r.URL + r.Outcome + strings.Join(slices.Concat(r.Header["Sql-Args"], r.Header["Sql-User"]), "")
		if strings.Contains(shown, "secret-arg") || strings.Contains(shown, "s3cret") {
			t.Errorf("request %d shows a secret: %s", i, shown)
		}
		if r.Outcome != "returned 1 rows of the columns state, n" {
			t.Errorf("request %d: outcome %q", i, r.Outcome)
		}
	}
}

// A sql collector's url label is driver://host:port/database, an Oracle
// service in the database's place, with no statement and no credential,
// and its method QUERY; a target is shown as written when it is a server,
// and withheld when it holds anything else.
func TestTheVerboseLabelsOfASQLStatement(t *testing.T) {
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverSQLServer, "SELECT 1 AS one", func(r *model.RequestConfig) { r.Database = "app_{{param_tenant}}" })
	label, err := RequestLabelFor("db.example", c, RequestOverrides{})
	if err != nil || label != "sqlserver://db.example:1433/app_{{param_tenant}}" || RequestMethodFor(c, RequestOverrides{}) != "QUERY" {
		t.Errorf("label %q %v, method %q", label, err, RequestMethodFor(c, RequestOverrides{}))
	}
	for _, tc := range []struct{ target, want string }{
		{"db.example:1433", "db.example:1433"},
		{"\tdb\n", "db"},
		{"u@db", "<invalid target>"},
		{"sqlserver://db", "<invalid target>"},
	} {
		if got := DisplayTarget(c, tc.target); got != tc.want {
			t.Errorf("%q is shown as %q, want %q", tc.target, got, tc.want)
		}
	}
	if FetchStage(c) != "sql" {
		t.Errorf("stage %q", FetchStage(c))
	}
}

// A sql target is the server: host or host:port, an IPv6 address in
// brackets, the driver's port when it names none. A scheme, a user, a path,
// a port that is no port and an IPv6 address without brackets are refused
// before anything is sent.
func TestASQLTargetIsTheServer(t *testing.T) {
	f := newSQLFixture(t)
	ports := map[string]string{SQLDriverPostgres: "5432", SQLDriverMySQL: "3306", SQLDriverSQLServer: "1433"}
	for driverName, port := range ports {
		c := f.collector(t, "db", driverName, "q")
		for target, want := range map[string]string{
			"db.example":     "db.example:" + port,
			"DB.example.:99": "db.example:99",
			"[2001:db8::1]":  "[2001:db8::1]:" + port,
			"10.0.0.1:6000":  "10.0.0.1:6000",
		} {
			address, err := parseSQLTarget(c, target)
			if err != nil || address.hostPort != want {
				t.Errorf("%s %q: %q %v, want %q", driverName, target, address.hostPort, err, want)
			}
		}
		for target, want := range map[string]string{
			"postgres://db": "has a scheme, path, query or user in it",
			"u@db":          "has a scheme, path, query or user in it",
			"db/app":        "has a scheme, path, query or user in it",
			"db:0":          `has the port "0"`,
			"db:5432x":      `has the port "5432x"`,
			"":              "names no host",
		} {
			err := CheckTarget(c, target, false)
			if target == "" {
				if !errors.Is(err, ErrMissingTarget) {
					t.Errorf("%s %q: %v", driverName, target, err)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s %q: %v, want %q", driverName, target, err, want)
			}
		}
	}
}

// The dialer of a pool's connections holds each connection to the policy by
// the address it reached, refuses a connection to a host the request did not
// check, and dials the host through no other: an allowed address is
// connected, a denied one refused as the policy refuses.
func TestTheSQLDialerHoldsEachConnectionToThePolicy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	allow, err := compileTargetPolicy([]string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deny, err := compileTargetPolicy(nil, []string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDial(allow, "127.0.0.1", time.Second)(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("an allowed address: %v", err)
	}
	_ = conn.Close()
	if _, err := sqlDial(deny, "127.0.0.1", 0)(context.Background(), "tcp", "127.0.0.1:"+port); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a denied address: %v", err)
	}
	if _, err := sqlDial(allow, "db.example", 0)(context.Background(), "tcp", "127.0.0.1:"+port); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "the connection is to a host the request did not check") {
		t.Errorf("another host: %v", err)
	}
}

// A sql collector's request is held to its rules when it loads.
func TestASQLCollectorsRequestIsCheckedWhenItLoads(t *testing.T) {
	f := newSQLFixture(t)
	negative, zero := -1, 0
	short, half := model.Duration(500*time.Millisecond), model.Duration(-time.Second)
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"no driver":             {func(r *model.RequestConfig) { r.Driver = "" }, `collector "db" has no request.driver; a sql collector names its database's driver, one of: `},
		"an unknown driver":     {func(r *model.RequestConfig) { r.Driver = "db2" }, `collector "db" has unknown request.driver "db2"; the sql drivers are clickhouse, mysql, oracle, postgres, snowflake, sqlserver, vertica`},
		"no statement":          {func(r *model.RequestConfig) { r.Statement = " " }, `collector "db" has no request.statement`},
		"no username":           {func(r *model.RequestConfig) { r.Username = "" }, `collector "db" request.username is required for request.type sql`},
		"a blank password file": {func(r *model.RequestConfig) { r.PasswordFile = " " }, "request.password_file is written with blanks alone"},
		"a private key":         {func(r *model.RequestConfig) { r.PrivateKeyFile = "/k" }, "sets request.private_key_file, which only the snowflake driver takes"},
		"a passphrase":          {func(r *model.RequestConfig) { r.PassphraseFile = "/p" }, `collector "db" sets request.passphrase_file, which does not apply to request.type "sql"`},
		"a service name":        {func(r *model.RequestConfig) { r.ServiceName = "ORCL" }, "sets request.service_name, which only the oracle driver takes"},
		"a blank database":      {func(r *model.RequestConfig) { r.Database = "  " }, "request.database is written with blanks alone"},
		"an unknown option":     {func(r *model.RequestConfig) { r.Options = map[string]string{"sslrootcert": "/ca"} }, `request.options sets "sslrootcert", which the postgres driver does not take; it takes: application_name, sslmode`},
		"an option's value":     {func(r *model.RequestConfig) { r.Options = map[string]string{"sslmode": "prefer"} }, `request.options.sslmode is "prefer"; the postgres driver takes one of disable, require, verify-full`},
		"tls without tls": {func(r *model.RequestConfig) {
			r.Options = map[string]string{"sslmode": "disable"}
			r.TLS.CAFile = "/ca"
		}, `sets request.tls, but request.options.sslmode is "disable", which uses no TLS`},
		"half a certificate":  {func(r *model.RequestConfig) { r.TLS.CertFile = "/c" }, "request.tls sets only one of cert_file and key_file"},
		"negative max_rows":   {func(r *model.RequestConfig) { r.MaxRows = -1 }, "request.max_rows must not be negative"},
		"a negative limit":    {func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxPerTarget: &negative} }, "request.connections.max_per_target must not be negative"},
		"a negative lifetime": {func(r *model.RequestConfig) { r.Connections = &model.SQLConnections{MaxLifetime: &half} }, "request.connections.max_lifetime must not be negative"},
		"a short keepalive": {func(r *model.RequestConfig) {
			r.Connections = &model.SQLConnections{Keepalive: &model.SQLKeepalive{Interval: &short}}
		}, "request.connections.keepalive.interval 500ms is under the least, 1s"},
		"an unknown check": {func(r *model.RequestConfig) {
			r.Connections = &model.SQLConnections{Keepalive: &model.SQLKeepalive{Check: "icmp"}}
		}, `request.connections.keepalive.check is "icmp"; want tcp`},
		"a retry": {func(r *model.RequestConfig) { r.Retry.Attempts = 2 }, `sets request.retry, which does not apply to request.type "sql"`},
	} {
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverPostgres, Statement: "q", Username: "metrics", PasswordFile: f.passwordFile}}
		tc.change(&c.Request)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v\nwant %q", name, err, tc.want)
		}
	}
	// Taken: no limits at all, and a keepalive switched off.
	c := f.collector(t, "db", " PostgreS ", "q", func(r *model.RequestConfig) {
		r.Connections = &model.SQLConnections{MaxPerTarget: &zero, MaxTargets: &zero, Keepalive: &model.SQLKeepalive{Interval: new(model.Duration)}}
	})
	if c.Request.Driver != SQLDriverPostgres {
		t.Errorf("driver %q", c.Request.Driver)
	}
	// A static target may not set max_targets, nor a key another type owns.
	for _, tc := range []struct {
		request model.TargetRequestConfig
		want    string
	}{
		{model.TargetRequestConfig{Connections: &model.SQLConnections{MaxTargets: &zero}}, "request.connections.max_targets is the most targets a collector keeps connections to, so a static target cannot set it"},
		{model.TargetRequestConfig{PassphraseFile: "/p"}, `target "t" sets request.passphrase_file, which does not apply to collector "db"`},
		{model.TargetRequestConfig{Username: " "}, "request.username is written with blanks alone"},
	} {
		target := &model.StaticTarget{Name: "t", Collector: "db", Target: "db.example", Request: tc.request}
		if err := CheckTargetRequest(target, c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.request, err, tc.want)
		}
	}
}

// A driver the build left out is refused by name, saying which drivers the
// build has and how to build one with it.
func TestASQLDriverTheBuildLeftOutIsRefusedSayingHowToBuildIt(t *testing.T) {
	var missing string
	for _, name := range knownSQLDrivers {
		if sqlDriverTable[name] == nil {
			missing = name
			break
		}
	}
	if missing == "" {
		t.Skip("the build has every sql driver")
	}
	c := &model.Collector{Name: "x", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: missing, Statement: "q", Username: "u"}}
	want := `collector "x" uses sql driver "` + missing + `", which this build does not include; it was built with: ` + strings.Join(BuiltSQLDrivers(), ", ") + `. Build with SQL_DRIVERS=` + missing + ` (make build SQL_DRIVERS=` + missing + `, or the image's SQL_DRIVERS build argument)`
	if err := ValidateRequest(c); err == nil || err.Error() != want {
		t.Errorf("%v\nwant %s", err, want)
	}
}

// Every build with the sql type has the three default drivers, and the
// names it reports are those it registered, sorted.
func TestEveryBuildWithSQLHasTheDefaultDrivers(t *testing.T) {
	built := BuiltSQLDrivers()
	for _, name := range sqlDefaultDrivers {
		if !slices.Contains(built, name) {
			t.Errorf("the build lacks the default driver %s: %v", name, built)
		}
	}
	if !slices.IsSorted(built) || len(built) != len(sqlDriverTable) {
		t.Errorf("built drivers %v of %d registered", built, len(sqlDriverTable))
	}
}

// The TLS of a connection is the driver option's, or, left out, verified
// TLS with a tls key and none without: off is nil, verify checks the
// certificate against server_name or the host, and a mode without
// verification skips the check.
func TestTheTLSOfASQLConnection(t *testing.T) {
	d := sqlDriverTable[SQLDriverPostgres]
	for _, tc := range []struct {
		options  map[string]string
		tls      model.TLSConfig
		on       bool
		insecure bool
		name     string
	}{
		{nil, model.TLSConfig{}, false, false, ""},
		{nil, model.TLSConfig{ServerName: "db.internal"}, true, false, "db.internal"},
		{map[string]string{"sslmode": "verify-full"}, model.TLSConfig{}, true, false, "db.example"},
		{map[string]string{"sslmode": "require"}, model.TLSConfig{}, true, true, "db.example"},
		{map[string]string{"sslmode": "disable"}, model.TLSConfig{}, false, false, ""},
	} {
		base := &sqlBase{tls: tc.tls}
		config, digest, err := sqlTLSConfig(d, base, tc.options, "db.example")
		if err != nil {
			t.Fatal(err)
		}
		if (config != nil) != tc.on || config != nil && (config.InsecureSkipVerify != tc.insecure || config.ServerName != tc.name || digest == "") {
			t.Errorf("%v %+v: %+v", tc.options, tc.tls, config)
		}
	}
}

// The value of a column is read by its Go type, and the text of a numeric
// column, by the name the driver gives its type, as a number.
func TestASQLValueIsReadByItsType(t *testing.T) {
	for _, tc := range []struct {
		value   any
		dbType  string
		want    any
		present bool
		size    int64
	}{
		{nil, "TEXT", nil, false, 0},
		{int32(-5), "", -5, true, 8},
		{uint64(1 << 63), "", new(big.Int).SetUint64(1 << 63), true, 8},
		{float32(0.5), "", 0.5, true, 8},
		{"3.25", "DECIMAL(10,2)", 3.25, true, 4},
		{"3.25", "VARCHAR", "3.25", true, 4},
		{"12", "Nullable(UInt64)", 12, true, 2},
		{"7", "INT UNSIGNED", 7, true, 1},
		{"n/a", "NUMERIC", "n/a", true, 3},
		{[]byte("1e3"), "FLOAT8", 1000.0, true, 3},
		{"x", "INTERVAL", "x", true, 1},
		{"x", "POINT", "x", true, 1},
		{time.Unix(-1, 250_000_000).UTC(), "", -0.75, true, 8},
	} {
		value, present, size, err := sqlValue(tc.value, tc.dbType)
		if err != nil || !reflect.DeepEqual(value, tc.want) || present != tc.present || size != tc.size {
			t.Errorf("%#v %s: %#v %v %d %v, want %#v", tc.value, tc.dbType, value, present, size, err, tc.want)
		}
	}
}

// The drivers the source names, built in or not, are those of the
// sqldriver_<name>.go files, so that a driver a build left out is refused
// as one, saying how to build it in.
func TestTheKnownSQLDriversAreTheTreesDriverFiles(t *testing.T) {
	files, err := filepath.Glob("sqldriver_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		if !strings.HasSuffix(file, "_test.go") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(file, "sqldriver_"), ".go"))
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, knownSQLDrivers) || !slices.IsSorted(knownSQLDrivers) {
		t.Errorf("the tree's driver files are %v, and knownSQLDrivers %v", names, knownSQLDrivers)
	}
}

// --dry-run reads a sql collector's credential and TLS files as a
// connection would, and a static target's own credential file, and warns of
// each a scrape would fail on now: a missing password file, an empty one,
// and a CA file holding no certificate; a target without files of its own
// has none to warn of.
func TestADryRunWarnsOfTheSQLFilesAScrapeWouldFailOn(t *testing.T) {
	f := newSQLFixture(t)
	dir := t.TempDir()
	empty := testutil.WriteIn(t, dir, "empty", "\n")
	notCA := testutil.WriteIn(t, dir, "ca.pem", "no certificate\n")
	c := f.collector(t, "db", SQLDriverPostgres, "q", func(r *model.RequestConfig) {
		r.PasswordFile = filepath.Join(dir, "missing")
		r.TLS.CAFile = notCA
	})
	warnings := FileWarnings(c, nil)
	if len(warnings) != 2 || !strings.Contains(warnings[0], `collector "db": reading request.password_file: open `) || !strings.Contains(warnings[1], "request.tls: no certificates found in "+notCA) {
		t.Errorf("%q", warnings)
	}
	target := &model.StaticTarget{Name: "t", Collector: "db", Target: "db.example", Request: model.TargetRequestConfig{PasswordFile: empty}}
	if warnings := FileWarnings(c, target); len(warnings) != 1 || !strings.Contains(warnings[0], `target "t": request.password_file `+empty+" is empty") {
		t.Errorf("%q", warnings)
	}
	if warnings := FileWarnings(c, &model.StaticTarget{Name: "u", Collector: "db", Target: "db.example"}); len(warnings) != 0 {
		t.Errorf("%q", warnings)
	}
	good := f.collector(t, "good", SQLDriverPostgres, "q")
	if warnings := FileWarnings(good, nil); len(warnings) != 0 {
		t.Errorf("%q", warnings)
	}
}
