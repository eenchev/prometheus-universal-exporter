//go:build !select_request_types || request_type_http

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// aheadClock is the wall clock, moved ahead by what the test adds to it, so
// the first member list forms without waiting for a heartbeat interval while
// waits still take real time.
type aheadClock struct{ ahead atomic.Int64 }

func (c *aheadClock) Now() time.Time { return time.Now().Add(time.Duration(c.ahead.Load())) }

func (c *aheadClock) AfterFunc(d time.Duration, f func()) allocator.Timer {
	return time.AfterFunc(d, f)
}

// A bundle long poll whose wait is longer than the server's read timeout is
// answered 304 when its wait runs out, over the server run builds and a real
// connection: the read timeout, had it cancelled the request on its way,
// would leave an empty 200 that the replica takes for a failed fetch. The
// server's 30s timeout is shortened here so the test takes a second.
func TestALongPollOutlastsTheReadTimeout(t *testing.T) {
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
	in, err := allocator.LoadInputs(allocator.Paths{Config: conf})
	if err != nil {
		t.Fatal(err)
	}
	clock := &aheadClock{}
	alloc := allocator.New(in, allocator.Options{Clock: clock, Logger: testutil.QuietLogger(t)})
	clock.ahead.Store(int64(time.Minute))
	if _, err := alloc.Heartbeat(allocapi.Heartbeat{Member: "r1", Advertise: "127.0.0.1:9115", Instance: "i", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	bundle := alloc.Bundle("r1")
	if bundle == nil {
		t.Fatal("the member list did not form")
	}

	server := newServer("127.0.0.1:0", alloc.Handler(""))
	if server.ReadTimeout <= 0 || server.ReadHeaderTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes <= 0 {
		t.Fatalf("the server lacks a timeout or the header bound: %+v", server)
	}
	server.ReadTimeout = 200 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	wait := 5 * server.ReadTimeout
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/v1/bundle?member=r1&wait=%s", listener.Addr(), wait), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", `"`+bundle.Generation+`"`)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if took := time.Since(start); resp.StatusCode != http.StatusNotModified || took < wait {
		t.Errorf("a long poll of %s got %s after %s, want 304 when the wait ran out", wait, resp.Status, took)
	}
}

// A copy among peers that is sent SIGTERM keeps serving for
// allocator.PeerShutdownDelay, its /v1/peer and /ready answering 503, so
// its peers see it is shutting down at their next check and the next copy
// leads at once, rather than finding its listener closed; then it exits 0.
func TestACopyAmongPeersAnswers503BeforeItStops(t *testing.T) {
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
		done <- run([]string{"--web.listen-address=" + address, "--config.file=" + conf, "--config.watch-interval=0", "--peers=" + address}, &stdout, &stderr)
	}()
	status := func(path string) (int, error) {
		resp, err := http.Get("http://" + address + path)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	testutil.WaitFor(t, "the allocator to serve", func() bool {
		code, _ := status("/ready")
		return code == http.StatusOK
	})
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	signalled := time.Now()
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var unavailable time.Duration
	for {
		peer, err := status(allocator.PeerPath)
		if err != nil {
			break
		}
		if ready, err := status("/ready"); err == nil && peer == http.StatusServiceUnavailable {
			if ready != http.StatusServiceUnavailable {
				t.Errorf("/ready answered %d while /v1/peer answered 503", ready)
			}
			unavailable = time.Since(signalled)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if unavailable < allocator.PeerInterval {
		t.Errorf("/v1/peer answered 503 until %s after the signal, want it served for at least %s", unavailable, allocator.PeerInterval)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exited %d:\n%s", code, stderr.String())
		}
	case <-time.After(allocator.PeerShutdownDelay + 10*time.Second):
		t.Fatal("the allocator did not exit")
	}
}
