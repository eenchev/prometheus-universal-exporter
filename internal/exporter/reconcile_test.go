package exporter

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Per-collector state after a reload (reconcile.go), the probe's missing
// parameters, and the exporter's own HTTP server timeouts (lifecycle.go).

func TestHTTPServerTimeouts(t *testing.T) {
	s := NewHTTPServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 10*time.Second || s.ReadTimeout != 30*time.Second || s.IdleTimeout != 2*time.Minute || s.WriteTimeout != 0 {
		t.Fatalf("timeouts: header %s, read %s, idle %s, write %s", s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout, s.WriteTimeout)
	}
}

// An idle keep-alive connection is closed by the exporter.
func TestIdleConnectionsAreClosed(t *testing.T) {
	previous := httpIdleTimeout
	httpIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { httpIdleTimeout = previous })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var served atomic.Int64
	s := NewHTTPServer(listener.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	go func() { _ = s.Serve(listener) }()
	t.Cleanup(func() { _ = s.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// The exporter's closing ends the read, however long the machine takes
	// over it; the half minute bounds a connection that is never closed, as
	// one is not for two minutes without the timeout set above.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err = reader.ReadByte()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection was not closed: %v", err)
	}
}

// failureLogFor is a failure log with a clock the test moves.
func failureLogFor(t *testing.T) (*failureLog, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	f := newFailureLog()
	f.now = func() time.Time { return now }
	return f, &now
}

func TestRepeatedFailuresAreLoggedSparingly(t *testing.T) {
	f, now := failureLogFor(t)
	logs := testutil.CaptureLogs(t)
	logger := slog.Default()
	key := failureKey("web", "http://a", "")
	boom := errors.New("connection refused")
	// A failure a minute: at 12:00 in full, 12:01-12:04 only at debug level,
	// 12:05 again with the count.
	for i := 0; i < 6; i++ {
		f.failed(logger, slog.LevelError, key, "probe failed", "http", boom, "collector", "web")
		*now = now.Add(time.Minute)
	}
	if got := strings.Count(logs.String(), `"msg":"probe failed"`); got != 2 {
		t.Fatalf("6 failures over 5 minutes logged %d lines, want the first and one repeat:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"repeated":5`) || !strings.Contains(logs.String(), `"failing_since":"2026-09-01T12:00:00Z"`) || !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("the repeat line lacks its count or start:\n%s", logs.String())
	}
	// Another error is a new failure, logged at once.
	f.failed(logger, slog.LevelError, key, "probe failed", "http", errors.New("timeout"), "collector", "web")
	if got := strings.Count(logs.String(), `"msg":"probe failed"`); got != 3 {
		t.Fatalf("a different error was not logged: %d lines", got)
	}
	*now = now.Add(90 * time.Second)
	f.recovered(logger, key, "probe recovered", "collector", "web")
	if !strings.Contains(logs.String(), `"msg":"probe recovered","collector":"web","stage":"http","failed_for":"1m30s","failures":1`) {
		t.Fatalf("recovery not logged as expected:\n%s", logs.String())
	}
	before := logs.Len()
	f.recovered(logger, key, "probe recovered")
	if logs.Len() != before {
		t.Fatal("a success without a failure was logged")
	}
}

// At debug level every repeat is still visible.
func TestRepeatsAreLoggedAtDebugLevel(t *testing.T) {
	f, _ := failureLogFor(t)
	var out strings.Builder
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for i := 0; i < 3; i++ {
		f.failed(logger, slog.LevelWarn, "k", "probe stage failed; continuing", "decode", errors.New("bad"))
	}
	if strings.Count(out.String(), `"repeat":true`) != 2 || strings.Count(out.String(), `"level":"WARN"`) != 1 {
		t.Fatalf("debug output:\n%s", out.String())
	}
}

// What is remembered is bounded; failures not seen for an hour make room.
func TestTheFailureLogIsBounded(t *testing.T) {
	f, now := failureLogFor(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for i := 0; i < failureLogMaxEntries; i++ {
		f.failed(logger, slog.LevelError, failureKey("c", "t", string(rune(i))), "m", "s", nil)
	}
	f.failed(logger, slog.LevelError, "extra", "m", "s", nil)
	if _, ok := f.entries["extra"]; ok || len(f.entries) != failureLogMaxEntries {
		t.Fatalf("remembered %d entries past the bound", len(f.entries))
	}
	*now = now.Add(failureLogForget + time.Minute)
	f.failed(logger, slog.LevelError, "extra", "m", "s", nil)
	if _, ok := f.entries["extra"]; !ok || len(f.entries) != 1 {
		t.Fatalf("stale entries were not forgotten: %d", len(f.entries))
	}
	f.forgetCollectors(map[string]bool{"extra": true})
	if len(f.entries) != 0 {
		t.Fatal("a removed collector's failures are still remembered")
	}
}

// A failure not reported for an hour is forgotten, whether or not the log is
// full: the same failure days later is logged as new, not as a repeat
// failing since then, and its recovery after the silence is not logged.
func TestAFailureSilentForAnHourIsForgotten(t *testing.T) {
	f, now := failureLogFor(t)
	logs := testutil.CaptureLogs(t)
	logger := slog.Default()
	key := failureKey("web", "http://a", "")
	boom := errors.New("connection refused")
	f.failed(logger, slog.LevelError, key, "probe failed", "http", boom)
	*now = now.Add(72 * time.Hour)
	f.failed(logger, slog.LevelError, key, "probe failed", "http", boom)
	if strings.Contains(logs.String(), `"repeated"`) || strings.Count(logs.String(), `"msg":"probe failed"`) != 2 {
		t.Fatalf("the failure days later was a repeat:\n%s", logs)
	}
	*now = now.Add(2 * time.Hour)
	f.recovered(logger, key, "probe recovered")
	if strings.Contains(logs.String(), "probe recovered") {
		t.Fatalf("a recovery after hours of silence was logged:\n%s", logs)
	}
	// Swept on the next failure of anything, below the limit.
	f.failed(logger, slog.LevelError, failureKey("web", "http://b", ""), "probe failed", "http", boom)
	*now = now.Add(2 * time.Hour)
	f.failed(logger, slog.LevelError, failureKey("web", "http://c", ""), "probe failed", "http", boom)
	f.mu.Lock()
	remembered := len(f.entries)
	f.mu.Unlock()
	if remembered != 1 {
		t.Fatalf("%d failures remembered, want the one just reported", remembered)
	}
}
