//go:build request_type_tcp

package repository

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The port checks of docs/TCP.md work as written: a port that takes the
// connection is port_open 1 and one that refuses it 0, with the probe
// answered 200 either way; of a host's ports, each is its own series by
// its port, and ports_open counts them; the ping beside them, and the
// static targets of both, load in a build with icmp too.
func TestTCPConnectOnlyDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/TCP.md", "yaml")
	open := testutil.NewTCPStub(t, func(net.Conn) {})
	refusing := testutil.RefusingAddress(t)
	_, openPort, _ := net.SplitHostPort(open.Addr)
	_, refusingPort, _ := net.SplitHostPort(refusing)
	ports := strings.Replace(pageBlock(t, blocks, "collectors:\n  - name: host_ports\n"), "[22, 443, 5432]", "["+openPort+", "+refusingPort+"]", 1)
	conf := pageBlock(t, blocks, "collectors:\n  - name: port\n") + strings.TrimPrefix(ports, "collectors:\n")
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for _, tc := range []struct {
		collector, target string
		wants             []string
	}{
		{"port", open.Addr, []string{"port_open 1", "port_connect_seconds "}},
		{"port", refusing, []string{"port_open 0"}},
		{"host_ports", "127.0.0.1", []string{`port_open{port="` + openPort + `"} 1`, `port_open{port="` + refusingPort + `"} 0`, "ports_open 1"}},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+tc.target+"&collector="+tc.collector, nil))
		for _, want := range tc.wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s of %s: %d, want %q in\n%s", tc.collector, tc.target, recorder.Code, want, recorder.Body)
			}
		}
	}
	if _, ok := fetch.RequestTypes["icmp"]; !ok {
		return
	}
	ping := pageBlock(t, blocks, "collectors:\n  - name: ping\n")
	path = testutil.WriteIn(t, dir, "ping.yaml", ping)
	pinged, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, ping)
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 1m\ntargets:\n")))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, pinged)
	}
	if err != nil {
		t.Fatal(err)
	}
}
