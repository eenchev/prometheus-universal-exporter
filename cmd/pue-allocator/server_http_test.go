//go:build !select_request_types || request_type_http

package main

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
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
	if _, err := alloc.Heartbeat(allocapi.Heartbeat{Member: "r1", Advertise: "127.0.0.1:9115"}); err != nil {
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
