package exporter

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Per-collector state after a reload (reconcile.go), the probe's missing
// parameters, and the exporter's own HTTP server timeouts (lifecycle.go).

// The exporter's HTTP server leaves a request's headers ten seconds, the
// whole request thirty and an idle connection two minutes, and has no write
// timeout. The ten seconds are those of a server nothing has given another
// limit: the tests' servers leave the headers half a minute (TestMain), which
// is taken off here and put back.
func TestHTTPServerTimeouts(t *testing.T) {
	inTests := NewHTTPServer(":0", http.NotFoundHandler())
	if inTests.ReadHeaderTimeout != 30*time.Second {
		t.Errorf("a server of the tests leaves the headers %s, want the half minute TestMain sets", inTests.ReadHeaderTimeout)
	}
	SetReadHeaderTimeout(exportersReadHeaderTimeout)
	t.Cleanup(func() { SetReadHeaderTimeout(testsHandshakeAndHeaderTimeout) })
	s := NewHTTPServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 10*time.Second || s.ReadTimeout != 30*time.Second || s.IdleTimeout != 2*time.Minute || s.WriteTimeout != 0 {
		t.Fatalf("timeouts: header %s, read %s, idle %s, write %s", s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout, s.WriteTimeout)
	}
}

// readNotingConn is the exporter's side of a connection, which notes when it
// was accepted, the first read deadline the server gives it, and when that
// was set.
type readNotingConn struct {
	net.Conn
	accepted time.Time
	mu       sync.Mutex
	deadline time.Time
	setAt    time.Time
}

func (c *readNotingConn) SetReadDeadline(at time.Time) error {
	c.mu.Lock()
	if c.deadline.IsZero() && !at.IsZero() {
		c.deadline, c.setAt = at, time.Now()
	}
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(at)
}

// readNotingListener hands the server its connections as readNotingConns,
// and the test each of them.
type readNotingListener struct {
	net.Listener
	conns chan *readNotingConn
}

func (l readNotingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	noted := &readNotingConn{Conn: conn, accepted: time.Now()}
	l.conns <- noted
	return noted, nil
}

// A client that does not end its request's headers is dropped at the limit
// the headers have: the exporter closes the connection without an answer,
// and no handler has seen the request.
//
// The client sends a request line and one header and then nothing, so the
// headers cannot be complete before the limit however slow the machine, and
// the limit stays short. That it is the limit set is read from the
// connection, whose first read deadline is the limit after the server began
// to read the request; the closing is then waited for with no bound but a
// hang's. With the tests' half minute in place of the 100ms the deadline
// read is another, and the test fails before it waits.
func TestAClientThatNeverEndsItsHeadersIsDropped(t *testing.T) {
	const limit = 100 * time.Millisecond
	SetReadHeaderTimeout(limit)
	t.Cleanup(func() { SetReadHeaderTimeout(testsHandshakeAndHeaderTimeout) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	noting := readNotingListener{Listener: listener, conns: make(chan *readNotingConn, 1)}
	var served atomic.Int64
	s := NewHTTPServer(listener.Addr().String(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served.Add(1) }))
	go func() { _ = s.Serve(noting) }()
	t.Cleanup(func() { _ = s.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatal(err)
	}
	exporters := <-noting.conns
	var deadline, setAt time.Time
	testutil.WaitFor(t, "the exporter to begin reading the request", func() bool {
		exporters.mu.Lock()
		defer exporters.mu.Unlock()
		deadline, setAt = exporters.deadline, exporters.setAt
		return !deadline.IsZero()
	})
	if deadline.Before(exporters.accepted.Add(limit)) || deadline.After(setAt.Add(limit)) {
		t.Fatalf("the headers are to be read %s after the connection was accepted and %s after the deadline was set, want %s after the server began to read", deadline.Sub(exporters.accepted), deadline.Sub(setAt), limit)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
	answer, err := io.ReadAll(conn)
	if err != nil || len(answer) != 0 {
		t.Fatalf("the client read %q and %v, want the connection closed with no answer", answer, err)
	}
	if early := time.Until(deadline); early > 0 {
		t.Errorf("the connection was closed %s before the deadline the headers had", early)
	}
	if got := served.Load(); got != 0 {
		t.Errorf("%d requests reached a handler, want none of a request whose headers never ended", got)
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
		f.failed(logger, slog.LevelWarn, failureKey("k", "", ""), "probe stage failed; continuing", "decode", errors.New("bad"))
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
	extra := failureKey("extra", "", "")
	f.failed(logger, slog.LevelError, extra, "m", "s", nil)
	if _, ok := f.entries[extra.bytes]; ok || len(f.entries) != failureLogMaxEntries {
		t.Fatalf("remembered %d entries past the bound", len(f.entries))
	}
	*now = now.Add(failureLogForget + time.Minute)
	f.failed(logger, slog.LevelError, extra, "m", "s", nil)
	if _, ok := f.entries[extra.bytes]; !ok || len(f.entries) != 1 {
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
