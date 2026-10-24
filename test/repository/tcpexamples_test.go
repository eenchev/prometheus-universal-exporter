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

// What memcached 1.6, a ZooKeeper 3.9 leader and HAProxy 2.8 answer the
// commands of examples/tcp, cut short but in their order and form: STAT
// lines ending in CRLF and END; a name, a tab and a value on each line,
// the version and the state text; and show stat's CSV, whose frontend has
// no queue and whose servers have no HTTP responses counted, with a
// stopped frontend and a server in each state tcpExampleStatuses lists.
var (
	tcpExampleStats = "STAT pid 1\r\nSTAT uptime 3600\r\nSTAT version 1.6.29\r\nSTAT curr_connections 10\r\nSTAT total_connections 250\r\n" +
		"STAT cmd_get 100\r\nSTAT cmd_set 40\r\nSTAT cmd_flush 0\r\nSTAT cmd_touch 2\r\nSTAT get_hits 90\r\nSTAT get_misses 10\r\n" +
		"STAT bytes_read 52000\r\nSTAT bytes_written 98000\r\nSTAT limit_maxbytes 67108864\r\nSTAT bytes 4096\r\nSTAT curr_items 120\r\nSTAT evictions 3\r\nEND\r\n"
	tcpExampleMntr = "zk_version\t3.9.2-e454e8c7283100c7caec6dcae2bc82aaecb63023, built on 2024-02-12 20:59 UTC\nzk_server_state\tleader\n" +
		"zk_avg_latency\t0.5\nzk_packets_received\t1200\nzk_packets_sent\t1199\nzk_znode_count\t42\nzk_synced_followers\t2\n"
	tcpExampleShowStat = "# pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,weight,act,bck,chkfail,chkdown,lastchg,downtime,qlimit,pid,iid,sid,throttle,lbtot,tracked,type,rate,rate_lim,rate_max,check_status,check_code,check_duration,hrsp_1xx,hrsp_2xx,hrsp_3xx,hrsp_4xx,hrsp_5xx,\n" +
		"web,FRONTEND,,,3,10,262124,120,1000,2000,0,0,0,,,,,OPEN,,,,,,,,,1,2,0,,,,0,1,0,4,,,,0,110,2,5,3,\n" +
		"app,app1,0,0,2,5,,80,700,1500,,0,,0,0,0,0,UP,1,1,0,0,0,600,0,,1,3,1,,80,,2,1,,3,L7OK,200,1,,,,,,\n" +
		"app,app2,1,2,0,3,,40,300,500,,0,,4,0,0,0,DOWN,1,1,0,3,1,30,30,,1,3,2,,40,,2,0,,2,L4CON,,0,,,,,,\n" +
		"app,BACKEND,1,2,2,6,26213,120,1000,2000,0,0,,4,0,0,0,UP,1,1,0,,0,600,0,,1,3,0,,120,,1,1,,4,,,,0,110,2,5,3,\n" +
		"admin,FRONTEND,,,0,0,100,0,0,0,0,0,0,,,,,STOP,,,,,,,,,1,4,0,,,,0,0,0,0,,,,0,0,0,0,0,\n" +
		tcpExampleServerRows + "\n"
)

// tcpExampleStatuses are the states HAProxy 2.8 writes in show stat's
// status column for a server, by the server's name in the stand-in, and
// whether the server is serving: going down (UP 1/3), up but taking no
// new sessions from the balancer (NOLB) or draining (DRAIN), and serving
// without a health check (no check), all serving; coming up (DOWN 1/2), in
// maintenance, itself or through the server it tracks, down by its agent,
// and a cell left empty, none serving.
var tcpExampleStatuses = []struct {
	name, status string
	up           bool
}{
	{"going_down", "UP 1/3", true},
	{"nolb", "NOLB", true},
	{"drain", "DRAIN", true},
	{"unchecked", "no check", true},
	{"coming_up", "DOWN 1/2", false},
	{"maint", "MAINT", false},
	{"tracking", "MAINT (via app/app1)", false},
	{"agent_down", "DOWN (agent)", false},
	{"empty", "", false},
}

// tcpExampleServerRows are app2's row of show stat for each state of
// tcpExampleStatuses, under its name.
var tcpExampleServerRows = func() string {
	var b strings.Builder
	for _, s := range tcpExampleStatuses {
		b.WriteString("app," + s.name + ",1,2,0,3,,40,300,500,,0,,4,0,0,0," + s.status + ",1,1,0,3,1,30,30,,1,3,2,,40,,2,0,,2,L4CON,,0,,,,,,\n")
	}
	return b.String()
}()

// tcpExampleServer answers stats as memcached does, keeping the connection
// for the next command, and mntr and show stat as ZooKeeper and HAProxy
// do, closing it; anything else it answers with nothing.
func tcpExampleServer(conn net.Conn) {
	first, err := testutil.ReadUntil(conn, "t")
	if err != nil {
		return
	}
	switch first {
	case "st":
		if rest, _ := testutil.ReadUntil(conn, "\n"); rest == "ats\r\n" {
			_, _ = io.WriteString(conn, tcpExampleStats)
			_, _ = io.Copy(io.Discard, conn)
		}
	case "mnt":
		if rest, _ := testutil.ReadUntil(conn, "r"); rest == "r" {
			_, _ = io.WriteString(conn, tcpExampleMntr)
		}
	case "show st":
		if rest, _ := testutil.ReadUntil(conn, "\n"); rest == "at\n" {
			_, _ = io.WriteString(conn, tcpExampleShowStat)
		}
	}
}

// examples/tcp reads its three services as its comments say, in a build
// with the type: memcached's counters and gauges, its commands by name and
// its get hit ratio, derived from its hits and misses, the version left
// out; ZooKeeper's numbers by name, its packets, that it
// leads and its followers, the version and state left out of the numbers;
// HAProxy's sessions, queues, bytes, errors and states by proxy, name and
// type, an empty cell making no series rather than failing its rule. No
// rule of any of them fails.
func TestTheTCPExamplesReadTheirServices(t *testing.T) {
	const path = "examples/tcp/config.yaml"
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := testutil.NewTCPStub(t, tcpExampleServer)
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	frontend, app1, app2, backend := `{name="FRONTEND",proxy="web",type="frontend"}`, `{name="app1",proxy="app",type="server"}`, `{name="app2",proxy="app",type="server"}`, `{name="BACKEND",proxy="app",type="backend"}`
	for collector, c := range map[string]struct{ want, absent []string }{
		"memcached": {
			want: []string{
				"memcached_uptime_seconds 3600", "memcached_current_connections 10", "memcached_connections_total 250", "memcached_items 120",
				"memcached_memory_used_bytes 4096", "memcached_memory_limit_bytes 6.7108864e+07",
				`memcached_commands_total{command="get"} 100`, `memcached_commands_total{command="set"} 40`, `memcached_commands_total{command="flush"} 0`, `memcached_commands_total{command="touch"} 2`,
				"memcached_get_hits_total 90", "memcached_get_misses_total 10", "memcached_evictions_total 3",
				"memcached_read_bytes_total 52000", "memcached_written_bytes_total 98000",
				"memcached_get_hit_ratio 0.9",
			},
			// The sum the ratio reads is hidden.
			absent: []string{"memcached_gets "},
		},
		"zookeeper": {
			want: []string{
				`zookeeper_mntr{name="avg_latency"} 0.5`, `zookeeper_mntr{name="znode_count"} 42`, `zookeeper_mntr{name="synced_followers"} 2`,
				"zookeeper_packets_received_total 1200", "zookeeper_packets_sent_total 1199", "zookeeper_leader 1", "zookeeper_synced_followers 2",
			},
			absent: []string{`name="version"`, `name="server_state"`},
		},
		"haproxy": {
			want: []string{
				"haproxy_current_sessions" + frontend + " 3", "haproxy_current_sessions" + app1 + " 2", "haproxy_current_sessions" + backend + " 2",
				"haproxy_session_limit" + frontend + " 262124", "haproxy_sessions_total" + app2 + " 40",
				"haproxy_queued_requests" + app2 + " 1", "haproxy_queued_requests" + backend + " 1",
				"haproxy_received_bytes_total" + frontend + " 1000", "haproxy_sent_bytes_total" + app1 + " 1500",
				"haproxy_connection_errors_total" + app2 + " 4", "haproxy_http_responses_5xx_total" + frontend + " 3",
				"haproxy_up" + frontend + " 1", "haproxy_up" + app1 + " 1", "haproxy_up" + app2 + " 0", "haproxy_up" + backend + " 1",
			},
			absent: []string{"haproxy_queued_requests" + frontend, "haproxy_session_limit" + app1, "haproxy_http_responses_5xx_total" + app1},
		},
	} {
		if collector == "haproxy" {
			// A stopped frontend is not open; each server's state is up
			// as it serves, the status read by its first word.
			c.want = append(c.want, `haproxy_up{name="FRONTEND",proxy="admin",type="frontend"} 0`)
			for _, s := range tcpExampleStatuses {
				up := "0"
				if s.up {
					up = "1"
				}
				c.want = append(c.want, `haproxy_up{name="`+s.name+`",proxy="app",type="server"} `+up)
			}
		}
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+server.Addr+"&collector="+collector, nil))
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: %d %s", collector, recorder.Code, body)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(body, want+"\n") {
				t.Errorf("%s: no %q in\n%s", collector, want, body)
			}
		}
		for _, absent := range c.absent {
			if strings.Contains(body, absent) {
				t.Errorf("%s: %q in\n%s", collector, absent, body)
			}
		}
	}
	recorder := httptest.NewRecorder()
	exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/self-metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "http_exporter_rule_failures_total{") && !strings.HasSuffix(line, " 0") {
			t.Errorf("a rule of the examples failed: %s", line)
		}
	}
}
