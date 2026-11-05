//go:build !select_request_types || request_type_http

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// startRun runs the allocator with args, a configuration of its own and a
// free address, until the test ends, and returns the address.
func startRun(t *testing.T, args ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	startRunAt(t, address, args...)
	return address
}

// startRunAt is startRun at address.
func startRunAt(t *testing.T, address string, args ...string) {
	t.Helper()
	dir := t.TempDir()
	conf := testutil.WriteIn(t, dir, "config.yaml", `collectors:
  - name: demo
    request:
      type: http
    transform:
      type: regex
    metrics:
      - name: demo_value
        expression: 'value=(\d+)'
`)
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- run(append([]string{"--web.listen-address=" + address, "--config.file=" + conf, "--config.watch-interval=0"}, args...), &stdout, &stderr)
	}()
	testutil.WaitFor(t, "the allocator to serve", func() bool {
		resp, err := http.Get("http://" + address + "/ready")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	t.Cleanup(func() {
		process, _ := os.FindProcess(os.Getpid())
		_ = process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(allocator.PeerShutdownDelay + 10*time.Second):
			t.Error("the allocator did not exit")
		}
	})
}

func fetchStatus(t *testing.T, address string) allocator.Status {
	t.Helper()
	resp, err := http.Get("http://" + address + allocator.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var s allocator.Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The status names the instance by its --web.listen-address and the build's
// version and revision, which run hands the allocator.
func TestTheStatusNamesTheInstanceByItsAddressAndBuild(t *testing.T) {
	address := startRun(t)
	s := fetchStatus(t, address)
	b := exporter.BuildVersion()
	if s.Instance.Address != address || s.Instance.Version != b.Version || s.Instance.Revision != b.Revision || !s.Instance.Ready || s.Peers != nil {
		t.Errorf("the instance is %+v, peers %v; want %s, %s, %s", s.Instance, s.Peers, address, b.Version, b.Revision)
	}
}

// With --peers.leader-only, an instance alone among its peers leads and serves
// the whole of /metrics and of the status; the flag reaches the allocator.
func TestTheLeaderOnlyFlagReachesTheAllocator(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	startRunAt(t, address, "--peers="+address, "--peers.leader-only")
	var s allocator.Status
	testutil.WaitFor(t, "the instance to lead", func() bool {
		s = fetchStatus(t, address)
		return s.Peers != nil && s.Peers.Role == "leader"
	})
	if !s.Peers.LeaderOnly || s.Partial || s.Members == nil {
		t.Errorf("the leader's status is %+v, peers %+v", s, s.Peers)
	}
}

// --heartbeat.interval reaches the allocator, which says it in its status
// and in every heartbeat reply.
func TestTheHeartbeatIntervalFlagReachesTheAllocator(t *testing.T) {
	address := startRun(t, "--heartbeat.interval=2500ms")
	if s := fetchStatus(t, address); s.Members == nil || s.Members.HeartbeatIntervalSeconds != 2.5 || s.Members.MemberTTLSeconds != 7.5 {
		t.Errorf("the members are %+v, want a heartbeat interval of 2.5s and a member TTL of 7.5s", s.Members)
	}
	resp, err := http.Post("http://"+address+allocapi.HeartbeatPath, "application/json",
		strings.NewReader(`{"member":"r1","advertise":"10.0.0.1:9115","instance":"i","sequence":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var reply allocapi.HeartbeatReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.HeartbeatIntervalSeconds != 2.5 {
		t.Errorf("the heartbeat reply is %+v, %v; want a heartbeat interval of 2.5 seconds", reply, err)
	}
}

// --config.watch-interval, --settle and --discovery.refresh-interval reach
// the allocator, which says them in its status: how often it reads the
// input files again, how long a changed member list holds, and how often
// it asks a discovery source that sets no refresh_interval, where one that
// sets its own keeps it.
func TestTheIntervalFlagsReachTheAllocator(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "sd/a.json", `[{"targets": ["10.0.0.1:80"]}]`)
	probes := testutil.WriteIn(t, dir, "probes.yaml", `probe_jobs:
  - name: web
    collector: demo
    discovery:
      - name: default
        file_sd: {files: [sd/a.json]}
      - name: own
        file_sd: {files: [sd/a.json], refresh_interval: 2m}
`)
	address := startRun(t, "--probe-targets-file="+probes, "--config.watch-interval=45s", "--settle=3s", "--discovery.refresh-interval=7s")
	s := fetchStatus(t, address)
	if s.Instance.WatchIntervalSeconds != 45 || s.Members == nil || s.Members.SettleSeconds != 3 {
		t.Errorf("the instance is %+v and the members %+v; want a watch interval of 45s and a settle time of 3s", s.Instance, s.Members)
	}
	refresh := map[string]float64{}
	for _, src := range s.Sources {
		refresh[src.Name] = src.RefreshIntervalSeconds
	}
	if len(refresh) != 2 || refresh["default"] != 7 || refresh["own"] != 120 {
		t.Errorf("the sources are refreshed every %v seconds, want default 7 and own 120", refresh)
	}
}
