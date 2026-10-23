//go:build request_type_tcp

package exporter

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// tcpExporter is an exporter, with verbose self-metrics, of a tcp
// collector reading memcached's stats with a regex transform and one
// reading a ZooKeeper's mntr, the four-letter word named by the probe, and
// the stand-in server that answers both.
func tcpExporter(t *testing.T) (*Server, *testutil.TCPStub) {
	t.Helper()
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		first, err := testutil.ReadUntil(conn, "\n")
		switch {
		case err == nil && first == "stats\r\n":
			_, _ = io.WriteString(conn, "STAT pid 1\r\nSTAT curr_connections 10\r\nSTAT get_hits 90\r\nSTAT get_misses 10\r\nEND\r\n")
			// memcached keeps the connection for the next command.
			_, _ = io.Copy(io.Discard, conn)
		case err == nil && first == "mntr\n":
			// ZooKeeper answers and closes.
			_, _ = io.WriteString(conn, "zk_version\t3.8.4\nzk_avg_latency\t2\nzk_znode_count\t42\n")
		}
	})
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", `web:
  self_metrics:
    verbose: true
collectors:
  - name: memcached
    request:
      type: tcp
      send: "stats\r\n"
      terminator: "END\r\n"
      denied_targets: [10.0.0.0/8]
    transform:
      type: regex
    metrics:
      - name: memcached_curr_connections
        expression: '(?m)^STAT curr_connections (\d+)\r$'
      - name: memcached_get_hits_total
        type: counter
        expression: '(?m)^STAT get_hits (\d+)\r$'
  - name: zookeeper
    request:
      type: tcp
      send: "{{param_word:mntr}}\n"
    transform:
      type: regex
    metrics:
      - name: zookeeper_value
        expression: '(?m)^zk_(?P<name>\w+)\t(?P<value>[0-9.]+)$'
        labels:
          - name: name
            expression: name
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), server
}

// A tcp collector's probe gives the series its regex rules read from the
// server's text; the verbose self-metrics label the scrape with
// tcp://host:port and SEND; a refused target is answered 403 and counted;
// a target without a port, and a placeholder value with a line feed, are
// answered 400 before anything is sent; and the collectors page hints the
// target as host:port.
func TestATCPCollectorProbe(t *testing.T) {
	exp, server := tcpExporter(t)
	recorder := probeOnce(t, exp, "/probe?collector=memcached&target="+server.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "memcached_curr_connections 10\nmemcached_get_hits_total 90" {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	recorder = probeOnce(t, exp, "/probe?collector=zookeeper&target="+server.Addr, nil)
	want := `zookeeper_value{name="avg_latency"} 2` + "\n" + `zookeeper_value{name="znode_count"} 42`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, exp, "/probe?collector=memcached&target=10.1.2.3:11211", nil); recorder.Code != http.StatusForbidden {
		t.Errorf("a denied target: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := probeOnce(t, exp, "/probe?collector=memcached&target=127.0.0.1", nil); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "a tcp target names its port") {
		t.Errorf("a target without a port: %d %s", recorder.Code, recorder.Body)
	}
	accepted := server.Accepted()
	if recorder := probeOnce(t, exp, "/probe?collector=zookeeper&param_word=mntr%0Akill&target="+server.Addr, nil); recorder.Code == http.StatusOK || !strings.Contains(recorder.Body.String(), "holds a carriage return or a line feed") {
		t.Errorf("a line feed in a parameter: %d %s", recorder.Code, recorder.Body)
	}
	if server.Accepted() != accepted {
		t.Error("a line feed in a parameter was sent")
	}
	metrics := probeOnce(t, exp, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		`http_exporter_scrapes_total{collector="memcached",http_method="SEND",url="tcp://` + server.Addr + `"} 1`,
		`http_exporter_scrapes_total{collector="zookeeper",http_method="SEND",url="tcp://` + server.Addr + `"} 1`,
		`http_exporter_targets_refused_total{collector="memcached"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %s in\n%s", want, metrics)
		}
	}
	if page := probeOnce(t, exp, "/collectors", nil).Body.String(); !strings.Contains(page, `placeholder="host:port"`) {
		t.Errorf("the collectors page hints no tcp target:\n%s", page)
	}
}

// A debug probe shows the bytes sent, quoted with \r\n visible, how reading
// stopped and the server's text as the body.
func TestADebugProbeOfATCPCollector(t *testing.T) {
	exp, server := tcpExporter(t)
	exp.SetDebug(DebugFull)
	body := probeOnce(t, exp, "/probe?debug=true&collector=memcached&target="+server.Addr, nil).Body.String()
	for _, want := range []string{"1. SEND tcp://" + server.Addr, `Tcp-Send: "stats\r\n"`, `read 81 bytes, the answer ended with the terminator "END\r\n"`, "STAT curr_connections 10"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
}

// A tcp collector's answer is read by a python transform as the body's
// text, here NUT's LIST VAR, and its label values take the probe's
// param_<name> parameters, as every type's do.
func TestATCPCollectorsAnswerInPython(t *testing.T) {
	requirePython(t)
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		if line, err := testutil.ReadUntil(conn, "\n"); err == nil && line == "LIST VAR ups\n" {
			_, _ = io.WriteString(conn, "BEGIN LIST VAR ups\nVAR ups battery.charge \"100\"\nVAR ups ups.load \"23\"\nVAR ups ups.status \"OL\"\nEND LIST VAR ups\n")
		}
	})
	exp := labelServer(t, `collectors:
  - name: nut
    request:
      type: tcp
      send: "LIST VAR ups\n"
      terminator: "END LIST VAR ups\n"
    transform:
      type: python
      labels:
        site: "{{param_site:lab}}"
      script: |
        for line in response.text.splitlines():
            parts = line.split(" ", 3)
            if parts[0] == "VAR" and parts[3].strip('"').replace(".", "", 1).isdigit():
                metric(name="nut_value", type="gauge", value=float(parts[3].strip('"')), labels={"var": parts[2]})
    metrics: []
`)
	recorder := probeOnce(t, exp, "/probe?collector=nut&param_site=rack1&target="+server.Addr, nil)
	want := []string{`nut_value{site="rack1",var="battery.charge"} 100`, `nut_value{site="rack1",var="ups.load"} 23`}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v\n%s", recorder.Code, got, want, recorder.Body)
	}
}
