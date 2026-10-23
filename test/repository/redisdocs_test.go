//go:build request_type_redis

package repository

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/REDIS.md work as written against a stand-in server,
// in a build with the type: the main collector's clients, memory, hit
// ratio and keys of each database, the queue's length for the queue a
// probe names, the replicas and how far behind each is, and a stream's
// consumer groups; the TLS block loads, and so do the static targets
// against the collectors.
func TestRedisDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/REDIS.md", "yaml")
	server := redistest.New(t)
	server.RequirePassword("", "s3cret")
	server.SetInfo(
		redistest.Section{Name: "Clients", Lines: []string{"connected_clients:12", "blocked_clients:0"}},
		redistest.Section{Name: "Memory", Lines: []string{"used_memory:1048576", "used_memory_human:1.00M", "maxmemory:4194304"}},
		redistest.Section{Name: "Stats", Lines: []string{"keyspace_hits:900", "keyspace_misses:100"}},
		redistest.Section{Name: "Replication", Lines: []string{"role:master", "connected_slaves:2", "slave0:ip=10.0.0.2,port=6379,state=online,offset=1234,lag=0", "slave1:ip=10.0.0.3,port=6379,state=online,offset=1000,lag=3", "master_repl_offset:1240"}},
		redistest.Section{Name: "Keyspace", Lines: []string{"db0:keys=12,expires=3,avg_ttl=1500", "db2:keys=1,expires=0,avg_ttl=0"}},
	)
	server.Reply("LLEN queue:emails", ":42\r\n")
	server.Reply("XINFO GROUPS events", "*1\r\n*4\r\n"+redistest.Bulk("name")+redistest.Bulk("workers")+redistest.Bulk("pending")+":5\r\n")
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Each collector logs in with the stand-in's password, as the page's
	// first does with its own.
	conf := pageBlock(t, blocks, "collectors:\n") + pageBlock(t, blocks, "  - name: queue_length\n") + pageBlock(t, blocks, "  - name: replication\n") + pageBlock(t, blocks, "  - name: stream_groups\n")
	conf = strings.ReplaceAll(conf, "/var/run/secrets/redis/password", passwordFile)
	conf = strings.ReplaceAll(conf, "      type: redis\n      commands:", "      type: redis\n      password_file: "+passwordFile+"\n      commands:")
	conf = strings.ReplaceAll(conf, "      type: redis\n      info_sections: [replication]", "      type: redis\n      password_file: "+passwordFile+"\n      info_sections: [replication]")
	conf = strings.ReplaceAll(conf, "      type: redis\n      db: 2", "      type: redis\n      password_file: "+passwordFile+"\n      db: 2")
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	if len(cfg.Collectors) != 4 {
		t.Fatalf("%d collectors loaded, want the four examples", len(cfg.Collectors))
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for probe, wants := range map[string][]string{
		"collector=redis": {
			"redis_connected_clients 12", "redis_memory_used_bytes 1.048576e+06", "redis_memory_max_bytes 4.194304e+06", "redis_memory_used_ratio 0.25",
			"redis_keyspace_hits_total 900", "redis_keyspace_misses_total 100", "redis_keyspace_hit_ratio 0.9", `redis_db_keys{db="db0"} 12`, `redis_db_keys{db="db2"} 1`,
		},
		"collector=queue_length&param_queue=emails": {`redis_queue_length{queue="emails"} 42`},
		"collector=replication": {
			"redis_connected_replicas 2", `redis_replica_lag_seconds{replica="10.0.0.2:6379"} 0`, `redis_replica_lag_seconds{replica="10.0.0.3:6379"} 3`,
			`redis_replica_behind_bytes{replica="10.0.0.2:6379"} 6`, `redis_replica_behind_bytes{replica="10.0.0.3:6379"} 240`,
		},
		"collector=stream_groups": {`redis_stream_group_pending{group="workers"} 5`},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+server.Addr+"&"+probe, nil))
		for _, want := range wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: %d, want %q in\n%s", probe, recorder.Code, want, recorder.Body)
			}
		}
	}
	request := strings.TrimSuffix(pageBlock(t, blocks, "request:\n  type: redis\n  tls:\n"), "\n")
	withTLS := "collectors:\n  - name: tls\n    " + strings.ReplaceAll(request, "\n", "\n    ") + "\n    transform:\n      type: jq\n    metrics:\n      - name: v\n        expression: .info.server.uptime_in_seconds\n"
	if _, err := config.Load(testutil.WriteIn(t, dir, "tls.yaml", withTLS)); err != nil {
		t.Errorf("%v\n%s", err, withTLS)
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 30s\ntargets:\n")))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}
