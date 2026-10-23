//go:build request_type_redis

package exporter

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// redisExporter is an exporter, with verbose self-metrics, of a redis
// collector reading memory from INFO and one reading a queue's length, the
// queue named by the probe, both logging in to the stand-in server it
// returns as the user exporter.
func redisExporter(t *testing.T) (*Server, *redistest.Server) {
	t.Helper()
	server := redistest.New(t)
	server.RequirePassword("exporter", "s3cret")
	server.SetInfo(redistest.Section{Name: "Memory", Lines: []string{"used_memory:1048576", "maxmemory:4194304"}})
	server.Reply("LLEN queue:emails", ":42\r\n")
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteIn(t, dir, "config.yaml", `web:
  self_metrics:
    verbose: true
collectors:
  - name: memory
    request:
      type: redis
      username: exporter
      password_file: `+passwordFile+`
      info_sections: [memory]
    transform:
      type: jq
    metrics:
      - name: redis_memory_used_bytes
        expression: .info.memory.used_memory
  - name: queue
    request:
      type: redis
      username: exporter
      password_file: `+passwordFile+`
      db: 1
      commands:
        - name: length
          command: [LLEN, "queue:{{param_queue}}"]
      denied_targets: [10.0.0.0/8]
    transform:
      type: jq
    metrics:
      - name: redis_queue_length
        expression: .commands.length
        labels:
          - name: queue
            value: "{{param_queue}}"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), server
}

// A redis collector's probe gives the series its jq rules read from the
// document; the verbose self-metrics label the scrape with
// redis://host:port/db and INFO or COMMANDS; a refused target is answered
// 403 and counted; a target with a path is answered 400; and the
// collectors page hints the target as a server.
func TestARedisCollectorProbe(t *testing.T) {
	exp, server := redisExporter(t)
	recorder := probeOnce(t, exp, "/probe?collector=memory&target="+server.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "redis_memory_used_bytes 1.048576e+06" {
		t.Errorf("%d %v", recorder.Code, got)
	}
	recorder = probeOnce(t, exp, "/probe?collector=queue&param_queue=emails&target=redis://"+server.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `redis_queue_length{queue="emails"} 42` {
		t.Errorf("%d %v", recorder.Code, got)
	}
	if recorder := probeOnce(t, exp, "/probe?collector=queue&param_queue=emails&target=10.1.2.3", nil); recorder.Code != http.StatusForbidden {
		t.Errorf("a denied target: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := probeOnce(t, exp, "/probe?collector=memory&target="+server.Addr+"/2", nil); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "the database is request.db") {
		t.Errorf("a target with a path: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, exp, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		`http_exporter_scrapes_total{collector="memory",http_method="INFO",url="redis://` + server.Addr + `/0"} 1`,
		`http_exporter_scrapes_total{collector="queue",http_method="COMMANDS",url="redis://` + server.Addr + `/1"} 1`,
		`http_exporter_targets_refused_total{collector="queue"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %s in\n%s", want, metrics)
		}
	}
	if page := probeOnce(t, exp, "/collectors", nil).Body.String(); !strings.Contains(page, `placeholder="host, host:port or rediss://host:port"`) {
		t.Errorf("the collectors page hints no redis target:\n%s", page)
	}
}

// A debug probe shows each command sent, AUTH with its password masked,
// and the document; the password is nowhere in the report.
func TestADebugProbeOfARedisCollector(t *testing.T) {
	exp, server := redisExporter(t)
	exp.SetDebug(DebugFull)
	body := probeOnce(t, exp, "/probe?debug=true&collector=queue&param_queue=emails&target="+server.Addr, nil).Body.String()
	for _, want := range []string{"1. AUTH redis://" + server.Addr + "/1", "Redis-Command: AUTH exporter ***", "2. SELECT redis://" + server.Addr + "/1", "3. LLEN redis://" + server.Addr + "/1", "Redis-Command: LLEN queue:emails", "the integer 42", `{"commands":{"length":42}}`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	if strings.Contains(body, "s3cret") {
		t.Errorf("the report shows the password:\n%s", body)
	}
}

// A redis collector's document is read by a python transform as data, its
// INFO numbers as numbers, and its label values take the probe's
// param_<name> parameters, as every type's do.
func TestARedisCollectorsDocumentInPython(t *testing.T) {
	requirePython(t)
	server := redistest.New(t)
	server.SetInfo(redistest.Section{Name: "Keyspace", Lines: []string{"db0:keys=12,expires=3,avg_ttl=1500", "db2:keys=1,expires=0,avg_ttl=0"}})
	exp := labelServer(t, `collectors:
  - name: keyspace
    request:
      type: redis
      info_sections: [keyspace]
    transform:
      type: python
      labels:
        env: "{{param_env:dev}}"
      script: |
        for db, stats in data["info"]["keyspace"].items():
            metric(name="redis_db_keys", type="gauge", value=stats["keys"] - stats["expires"], labels={"db": db})
    metrics: []
`)
	recorder := probeOnce(t, exp, "/probe?collector=keyspace&param_env=prod&target="+server.Addr, nil)
	want := []string{`redis_db_keys{db="db0",env="prod"} 9`, `redis_db_keys{db="db2",env="prod"} 1`}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v\n%s", recorder.Code, got, want, recorder.Body)
	}
}
