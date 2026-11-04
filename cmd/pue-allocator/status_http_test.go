//go:build !select_request_types || request_type_http

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// startRun runs the allocator with args, a configuration of its own and a
// free address, until the test ends, and returns the address.
func startRun(t *testing.T, args ...string) string {
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
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
	return address
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

// The status names the copy by its --web.listen-address and the build's
// version and revision, which run hands the allocator.
func TestTheStatusNamesTheCopysAddressAndBuild(t *testing.T) {
	address := startRun(t)
	s := fetchStatus(t, address)
	b := exporter.BuildVersion()
	if s.Copy.Address != address || s.Copy.Version != b.Version || s.Copy.Revision != b.Revision || !s.Copy.Ready || s.Peers != nil {
		t.Errorf("the copy is %+v, peers %v; want %s, %s, %s", s.Copy, s.Peers, address, b.Version, b.Revision)
	}
}
