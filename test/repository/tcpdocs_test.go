//go:build request_type_tcp

package repository

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The answers of the stand-in services the examples of docs/TCP.md are
// tried against, as memcached 1.6, ZooKeeper 3.8 and HAProxy 2.8 write
// them, cut short.
var (
	tcpDocsMemcached = "STAT pid 1\r\nSTAT uptime 3600\r\nSTAT curr_connections 10\r\nSTAT curr_items 120\r\nSTAT bytes 4096\r\nSTAT limit_maxbytes 67108864\r\nSTAT get_hits 90\r\nSTAT get_misses 10\r\nSTAT evictions 0\r\nEND\r\n"
	tcpDocsMntr      = "zk_version\t3.8.4-9316c2a7a97e1666d8f4593f34dd6fc36ecc436c, built on 2024-02-12 22:16 UTC\nzk_server_state\tleader\nzk_avg_latency\t0.5\nzk_znode_count\t42\nzk_open_file_descriptor_count\t61\n"
	tcpDocsShowStat  = "# pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,\nweb,FRONTEND,,,3,10,262124,120,1000,2000,0,0,0,,,,,OPEN,\nweb,app1,0,0,2,5,,80,700,1500,,0,,0,0,0,0,UP,\nweb,app2,0,0,0,3,,40,300,500,,0,,1,0,0,0,DOWN,\n" +
		"web,app3,0,0,0,3,,40,300,500,,0,,1,0,0,0,UP 1/3,\nweb,app4,0,0,0,3,,40,300,500,,0,,1,0,0,0,no check,\n" +
		"web,app5,0,0,0,3,,40,300,500,,0,,1,0,0,0,MAINT (via web/app1),\nweb,app6,0,0,0,3,,40,300,500,,0,,1,0,0,0,DOWN 1/2,\n" +
		"admin,FRONTEND,,,0,0,100,0,0,0,0,0,0,,,,,STOP,\n\n"
)

// tcpDocsServer answers as memcached does to stats, keeping the
// connection, as ZooKeeper does to mntr and HAProxy to show stat, closing
// it.
func tcpDocsServer(conn net.Conn) {
	first, err := testutil.ReadUntil(conn, "t")
	if err != nil {
		return
	}
	switch first {
	case "st":
		if rest, _ := testutil.ReadUntil(conn, "\n"); rest != "ats\r\n" {
			return
		}
		_, _ = io.WriteString(conn, tcpDocsMemcached)
		_, _ = io.Copy(io.Discard, conn)
	case "mnt":
		if rest, _ := testutil.ReadUntil(conn, "r"); rest == "r" {
			_, _ = io.WriteString(conn, tcpDocsMntr)
		}
	case "show st":
		if rest, _ := testutil.ReadUntil(conn, "\n"); rest == "at\n" {
			_, _ = io.WriteString(conn, tcpDocsShowStat)
		}
	}
}

// The examples of docs/TCP.md work as written against stand-in services,
// in a build with the type: memcached's connections, items, memory, hits
// and misses; ZooKeeper's numbers by name and whether it leads; HAProxy's
// sessions and states by proxy and server, read from show stat's CSV, a
// status by its first word, going down still up and coming up still down;
// the
// TLS block and the placeholder example load, and so do the static targets
// against the collectors.
func TestTCPDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/TCP.md", "yaml")
	server := testutil.NewTCPStub(t, tcpDocsServer)
	dir := t.TempDir()
	conf := pageBlock(t, blocks, "collectors:\n") + pageBlock(t, blocks, "  - name: zookeeper\n") + pageBlock(t, blocks, "  - name: haproxy\n")
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	if len(cfg.Collectors) != 3 {
		t.Fatalf("%d collectors loaded, want the three examples", len(cfg.Collectors))
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for collector, wants := range map[string][]string{
		"memcached": {
			"memcached_current_connections 10", "memcached_items 120", "memcached_memory_used_bytes 4096", "memcached_memory_limit_bytes 6.7108864e+07",
			"memcached_get_hits_total 90", "memcached_get_misses_total 10", "memcached_evictions_total 0",
		},
		"zookeeper": {`zookeeper_mntr{name="avg_latency"} 0.5`, `zookeeper_mntr{name="znode_count"} 42`, `zookeeper_mntr{name="open_file_descriptor_count"} 61`, "zookeeper_leader 1"},
		"haproxy": {
			`haproxy_current_sessions{proxy="web",server="FRONTEND"} 3`, `haproxy_current_sessions{proxy="web",server="app1"} 2`,
			`haproxy_sessions_total{proxy="web",server="app2"} 40`,
			`haproxy_up{proxy="web",server="FRONTEND"} 1`, `haproxy_up{proxy="web",server="app1"} 1`, `haproxy_up{proxy="web",server="app2"} 0`,
			`haproxy_up{proxy="web",server="app3"} 1`, `haproxy_up{proxy="web",server="app4"} 1`, `haproxy_up{proxy="web",server="app5"} 0`,
			`haproxy_up{proxy="web",server="app6"} 0`, `haproxy_up{proxy="admin",server="FRONTEND"} 0`,
		},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+server.Addr+"&collector="+collector, nil))
		for _, want := range wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: %d, want %q in\n%s", collector, recorder.Code, want, recorder.Body)
			}
		}
	}
	for _, prefix := range []string{"request:\n  type: tcp\n  send: \"show stat\\n\"\n  tls:\n", "  - name: haproxy_proxy\n"} {
		request := pageBlock(t, blocks, prefix)
		if !strings.HasPrefix(request, "  - ") {
			request = "  - name: tls\n    " + strings.ReplaceAll(strings.TrimSuffix(request, "\n"), "\n", "\n    ") + "\n"
		}
		document := "collectors:\n" + request + "    transform:\n      type: csv\n    metrics:\n      - name: v\n        expression: scur\n"
		if _, err := config.Load(testutil.WriteIn(t, dir, "more.yaml", document)); err != nil {
			t.Errorf("%v\n%s", err, document)
		}
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
