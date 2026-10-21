//go:build !select_request_types || request_type_sql

package exporter

import (
	"context"
	"database/sql/driver"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sqlCollectors are two sql collectors of one statement, one reading its
// rows with a csv transform, one series a row, and one with jq, and a
// password file to replace.
const sqlCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: jobs
    request:
      type: sql
      driver: postgres
      database: app
      username: metrics
      password_file: PASSWORD
      statement: SELECT state, n FROM jobs WHERE queue = $1
      args: ["{{param_queue:default}}"]
    transform:
      type: csv
    metrics:
      - name: jobs
        expression: n
        labels:
          - name: state
            expression: state
  - name: jobs_jq
    request:
      type: sql
      driver: mysql
      username: metrics
      password_file: PASSWORD
      statement: SELECT state, n FROM jobs WHERE queue = $1
      args: [default]
    transform:
      type: jq
    metrics:
      - name: jobs
        items: .[]
        expression: .n
        labels:
          - name: state
            expression: .state
`

// sqlExporter is an exporter of sqlCollectors whose connections reach db,
// and what each connection was made with.
type sqlExporter struct {
	server  *Server
	manager *config.Manager
	db      *sqltest.DB
	dir     string
	written string
	mu      sync.Mutex
	made    []fetch.SQLConnectInfo
}

func newSQLExporter(t *testing.T) *sqlExporter {
	t.Helper()
	e := &sqlExporter{db: &sqltest.DB{Results: map[string]sqltest.Result{
		"SELECT state, n FROM jobs WHERE queue = $1": {Columns: []string{"state", "n", "note"}, Types: []string{"TEXT", "INT8", "TEXT"}, Rows: [][]driver.Value{{"open", int64(3), nil}, {"done", int64(40), "x"}}},
	}}}
	t.Cleanup(fetch.UseSQLTestDriver(func(ctx context.Context, info fetch.SQLConnectInfo) (driver.Conn, error) {
		e.mu.Lock()
		e.made = append(e.made, info)
		e.mu.Unlock()
		return e.db.Connect(ctx)
	}))
	t.Cleanup(fetch.CloseConnections)
	e.dir = t.TempDir()
	password := testutil.WriteIn(t, e.dir, "password", "s3cret\n")
	e.written = strings.ReplaceAll(sqlCollectors, "PASSWORD", password)
	path := testutil.WriteIn(t, e.dir, "config.yaml", e.written)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e.manager = config.NewManager(cfg, path, testutil.QuietLogger(t))
	e.server = NewServer(e.manager, "python3", testutil.QuietLogger(t))
	return e
}

// A sql collector's probe reads the statement's rows: a csv transform one
// series a row by column name, a jq transform the list of objects with
// items; the probe's parameter is bound to the statement; and the
// connection is the collector's account, password, target and database.
func TestASQLCollectorProbe(t *testing.T) {
	e := newSQLExporter(t)
	for _, query := range []string{"/probe?collector=jobs&target=db.example&param_queue=mail", "/probe?collector=jobs_jq&target=db.example:3307"} {
		recorder := probeOnce(t, e.server, query, nil)
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(body, `jobs{state="open"} 3`) || !strings.Contains(body, `jobs{state="done"} 40`) {
			t.Errorf("%s: %d %s", query, recorder.Code, body)
		}
	}
	queries := e.db.Queries()
	if len(queries) != 2 || queries[0].Args[0] != "mail" || queries[1].Args[0] != "default" {
		t.Errorf("queries %+v", queries)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.made) != 2 || e.made[0].Driver != "postgres" || e.made[0].Host != "db.example" || e.made[0].Port != "5432" || e.made[0].Username != "metrics" ||
		e.made[0].Password != "s3cret" || e.made[0].Database != "app" || e.made[1].Driver != "mysql" || e.made[1].Port != "3307" {
		t.Errorf("connections made with %+v", e.made)
	}
}

// A sql collector's self-metrics count its pools and connections — open,
// idle, targets, made — its keepalive failures, its waits and failures at
// its limits and its last statement's rows, with no per-request series; its
// verbose series carry driver://host:port/database and QUERY; and a
// collector of another type has none of the families.
func TestASQLCollectorsSelfMetrics(t *testing.T) {
	e := newSQLExporter(t)
	for _, query := range []string{"/probe?collector=jobs&target=db.example", "/probe?collector=jobs&target=db.example", "/probe?collector=jobs&target=other.example"} {
		if recorder := probeOnce(t, e.server, query, nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, recorder.Code, recorder.Body)
		}
	}
	metrics := selfMetrics(t, e.server)
	for _, want := range []string{
		"# HELP http_exporter_sql_connections " + selfMetricHelp["http_exporter_sql_connections"] + "\n# TYPE http_exporter_sql_connections gauge\n",
		"# TYPE http_exporter_sql_connects_total counter\n",
		`http_exporter_sql_connections{collector="jobs"} 2`,
		`http_exporter_sql_idle_connections{collector="jobs"} 2`,
		`http_exporter_sql_targets{collector="jobs"} 2`,
		`http_exporter_sql_connects_total{collector="jobs"} 2`,
		`http_exporter_sql_keepalive_failures_total{collector="jobs"} 0`,
		`http_exporter_sql_limit_waits_total{collector="jobs"} 0`,
		`http_exporter_sql_limit_failures_total{collector="jobs"} 0`,
		`http_exporter_sql_rows{collector="jobs"} 2`,
		`http_exporter_sql_rows{collector="jobs_jq"} 0`,
		`http_exporter_sql_targets{collector="jobs_jq"} 0`,
		`http_exporter_scrapes_total{collector="jobs",http_method="QUERY",url="postgres://db.example:5432/app"} 2`,
		`http_exporter_targets_refused_total{collector="jobs"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, `http_exporter_sql_connections{collector="jobs",`) {
		t.Error("the connections have a per-request series")
	}
}

// A debug probe shows the statement's driver, server and database, the
// account, the connection new and then kept, the count of bound values and
// the rows, and neither the password nor a bound value; the rows show as
// the JSON the transforms read.
func TestADebugProbeOfASQLCollector(t *testing.T) {
	e := newSQLExporter(t)
	e.server.SetDebug(DebugFull)
	for _, connection := range []string{"new", "reused"} {
		body := probeOnce(t, e.server, "/probe?debug=true&collector=jobs&target=db.example&param_queue=hidden-queue", nil).Body.String()
		for _, want := range []string{
			"1. QUERY postgres://db.example:5432/app -> returned 2 rows of the columns state, n, note",
			"Sql-User: metrics", "Sql-Connection: " + connection, "Sql-Args: 1", "Sql-Database: app",
			`{"n":3,"state":"open"}`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("no %q in the report:\n%s", want, body)
			}
		}
		if strings.Contains(body, "s3cret") || strings.Contains(body, "hidden-queue") {
			t.Errorf("the report shows a secret:\n%s", body)
		}
	}
}

// A probe of a target that is no server is answered 400 before anything is
// sent, and one the policy refuses 403.
func TestSQLProbeTargets(t *testing.T) {
	e := newSQLExporter(t)
	for query, want := range map[string]int{
		"/probe?collector=jobs":                         http.StatusBadRequest,
		"/probe?collector=jobs&target=u%40db":           http.StatusBadRequest,
		"/probe?collector=jobs&target=postgres://db":    http.StatusBadRequest,
		"/probe?collector=jobs&target=169.254.169.254":  http.StatusForbidden,
		"/probe?collector=jobs&target=db&param_typo=xx": http.StatusBadRequest,
	} {
		if recorder := probeOnce(t, e.server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
	if e.db.Opened() != 0 {
		t.Errorf("%d connections made", e.db.Opened())
	}
}

// A reload that changes one collector's settings closes its pool, and the
// next probe connects anew; the other collector's pool is kept.
func TestAReloadClosesTheSQLPoolOfAChangedCollector(t *testing.T) {
	e := newSQLExporter(t)
	probe := func(collector string) {
		t.Helper()
		if recorder := probeOnce(t, e.server, "/probe?collector="+collector+"&target=db.example", nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", collector, recorder.Code, recorder.Body)
		}
	}
	probe("jobs")
	probe("jobs_jq")
	testutil.WriteIn(t, e.dir, "config.yaml", strings.Replace(e.written, "      database: app\n", "      database: other\n", 1))
	if err := e.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	selfMetrics(t, e.server)
	testutil.WaitFor(t, "jobs' connection to be closed", func() bool { return e.db.Closed() == 1 })
	probe("jobs")
	probe("jobs_jq")
	if opened := e.db.Opened(); opened != 3 {
		t.Errorf("%d connections made, want jobs' two and jobs_jq's one", opened)
	}
}

// A sql collector's label values take the probe's parameters as its args
// do, one parameter filling both; a parameter that neither names is
// refused, naming where a placeholder of a sql collector stands.
func TestASQLCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	e := newSQLExporter(t)
	server := labelServer(t, `collectors:
  - name: queues
    request:
      type: sql
      driver: postgres
      username: metrics
      statement: SELECT state, n FROM jobs WHERE queue = $1
      args: ["{{param_queue:default}}"]
    transform:
      type: csv
      labels:
        queue: "{{param_queue:default}}"
    metrics:
      - name: jobs
        expression: n
        labels:
          - name: state
            expression: state
`)
	recorder := probeOnce(t, server, "/probe?collector=queues&target=db.example&param_queue=mail", nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || !slices.Equal(got, []string{`jobs{queue="mail",state="done"} 40`, `jobs{queue="mail",state="open"} 3`}) {
		t.Errorf("%d %q", recorder.Code, got)
	}
	if queries := e.db.Queries(); len(queries) != 1 || queries[0].Args[0] != "mail" {
		t.Errorf("queries %+v", queries)
	}
	recorder = probeOnce(t, server, "/probe?collector=queues&target=db.example&param_queu=mail", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `probe parameters param_queu are not used by collector "queues": no placeholder in its request.database, its args or its label values names them`) {
		t.Errorf("%d %s", recorder.Code, recorder.Body)
	}
}

// A python transform is handed a sql collector's rows as the list of dicts
// they are, a NULL column left out of its row, as a pre-script of a jq
// transform is.
func TestAPythonTransformReadsASQLCollectorsRows(t *testing.T) {
	requirePython(t)
	newSQLExporter(t)
	server := labelServer(t, `collectors:
  - name: scripted
    request:
      type: sql
      driver: postgres
      username: metrics
      statement: SELECT state, n FROM jobs WHERE queue = $1
      args: [x]
    transform:
      type: python
      script: |
        for row in data:
            metric("jobs", "gauge", row["n"], labels={"state": row["state"], "noted": str("note" in row)})
`)
	recorder := probeOnce(t, server, "/probe?collector=scripted&target=db.example", nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || !slices.Equal(got, []string{`jobs{noted="False",state="open"} 3`, `jobs{noted="True",state="done"} 40`}) {
		t.Errorf("%d %q\n%s", recorder.Code, got, recorder.Body)
	}
}
