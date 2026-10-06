//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A client has AnswerWriteTimeout to read an answer, from its first byte
// (middleware.go).

// deadlineRecorder is a ResponseWriter that notes the write deadlines it is
// given, as a connection would be, and whether anything was written before
// the first.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	early     int
}

func (d *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	d.deadlines = append(d.deadlines, at)
	return nil
}

func (d *deadlineRecorder) WriteHeader(code int) {
	if len(d.deadlines) == 0 {
		d.early++
	}
	d.ResponseRecorder.WriteHeader(code)
}

func (d *deadlineRecorder) Write(p []byte) (int, error) {
	if len(d.deadlines) == 0 {
		d.early++
	}
	return d.ResponseRecorder.Write(p)
}

// Every endpoint sets the write deadline before the first byte of its answer,
// once, AnswerWriteTimeout ahead: an answer, an error and a report alike,
// compressed or not.
func TestEveryAnswerIsWrittenUnderADeadline(t *testing.T) {
	testutil.CaptureLogs(t)
	if AnswerWriteTimeout != 30*time.Second {
		t.Fatalf("AnswerWriteTimeout is %s, want 30s", AnswerWriteTimeout)
	}
	target := textTarget(t, "value=1\n")
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}},
		&model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: target.URL}}})
	server.SetProbeDebug(true)
	server.scrapeStaticTargets(t.Context(), 0)
	probe := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	for _, endpoint := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, probe, http.StatusOK},
		{http.MethodGet, probe + "&debug=true", http.StatusOK},
		{http.MethodGet, "/probe?collector=none", http.StatusBadRequest},
		{http.MethodGet, "/static-targets", http.StatusOK},
		{http.MethodGet, "/static-targets?debug=one", http.StatusOK},
		{http.MethodGet, "/self-metrics", http.StatusOK},
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/collectors", http.StatusOK},
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodGet, "/ready", http.StatusOK},
		{http.MethodPost, "/-/reload", http.StatusForbidden},
		{http.MethodGet, "/no-such-endpoint", http.StatusNotFound},
	} {
		for _, encoding := range []string{"", "gzip"} {
			request := httptest.NewRequest(endpoint.method, endpoint.path, nil)
			request.Header.Set("Accept-Encoding", encoding)
			recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			before := time.Now()
			server.Handler().ServeHTTP(recorder, request)
			after := time.Now()
			if recorder.Code != endpoint.want {
				t.Errorf("%s %s answered %d, want %d: %.200s", endpoint.method, endpoint.path, recorder.Code, endpoint.want, recorder.Body)
				continue
			}
			if len(recorder.deadlines) != 1 || recorder.early != 0 {
				t.Errorf("%s %s (Accept-Encoding %q): %d write deadlines set, %d writes before the first; want one deadline before any write", endpoint.method, endpoint.path, encoding, len(recorder.deadlines), recorder.early)
				continue
			}
			// The first byte was written while the handler ran, however long
			// the machine took over it.
			if at := recorder.deadlines[0]; at.Before(before.Add(AnswerWriteTimeout)) || at.After(after.Add(AnswerWriteTimeout)) {
				t.Errorf("%s %s: the write deadline is %s after the request began and %s after it ended, want %s from the answer's first byte", endpoint.method, endpoint.path, at.Sub(before), at.Sub(after), AnswerWriteTimeout)
			}
		}
	}
}

// largeExposition is an exposition of about 7 MiB, more than the socket
// buffers between the exporter and a client that reads nothing hold.
func largeExposition() string {
	var b strings.Builder
	b.WriteString("# TYPE g gauge\n")
	pad := strings.Repeat("x", 120)
	for i := range 9000 {
		fmt.Fprintf(&b, "g{i=\"%d\",a=\"%s\",b=\"%s\",c=\"%s\",d=\"%s\",e=\"%s\",f=\"%s\"} 1\n", i, pad, pad, pad, pad, pad, pad)
	}
	return b.String()
}

// syncedBuffer is a bytes.Buffer safe to write from handlers and read from the
// test.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// servedExporter is an exporter's endpoints on a real listener, where a write
// can block: its address, the handlers in progress, the connections it has
// closed, and its side of every connection made to it.
type servedExporter struct {
	address    string
	inProgress atomic.Int64
	closed     atomic.Int64
	mu         sync.Mutex
	conns      []*notedConn
}

// notedConn is the exporter's side of a connection. It notes the write
// deadlines the exporter gives it and when the exporter closed it, so that a
// test reads from the connection what it would otherwise have to wait out on
// the clock. The zero time, with which net/http takes the deadline off again
// when an answer has ended, is not noted.
type notedConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
	closedAt  time.Time
}

func (c *notedConn) SetWriteDeadline(at time.Time) error {
	if !at.IsZero() {
		c.mu.Lock()
		c.deadlines = append(c.deadlines, at)
		c.mu.Unlock()
	}
	return c.Conn.SetWriteDeadline(at)
}

func (c *notedConn) Close() error {
	c.mu.Lock()
	if c.closedAt.IsZero() {
		c.closedAt = time.Now()
	}
	c.mu.Unlock()
	return c.Conn.Close()
}

// noted returns the write deadlines set so far and when the connection was
// closed, the zero time while it is open.
func (c *notedConn) noted() (deadlines []time.Time, closedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deadlines...), c.closedAt
}

// notingListener hands the server its connections as notedConns.
type notingListener struct {
	net.Listener
	served *servedExporter
}

func (l notingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	noted := &notedConn{Conn: conn}
	l.served.mu.Lock()
	l.served.conns = append(l.served.conns, noted)
	l.served.mu.Unlock()
	return noted, nil
}

// onlyConn is the one connection made to the exporter.
func (s *servedExporter) onlyConn(t *testing.T) *notedConn {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) != 1 {
		t.Fatalf("%d connections were made to the exporter, want one", len(s.conns))
	}
	return s.conns[0]
}

func serveExporter(t *testing.T, server *Server) *servedExporter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := &servedExporter{address: listener.Addr().String()}
	listener = notingListener{Listener: listener, served: served}
	handler := server.Handler()
	httpServer := NewHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.inProgress.Add(1)
		defer served.inProgress.Add(-1)
		handler.ServeHTTP(w, r)
	}))
	httpServer.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			served.closed.Add(1)
		}
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return served
}

// A client that asks for a large answer and reads none of it holds its
// handler, and the rendered answer, only until the write deadline: the
// handler then returns, the connection is closed, and the log says so once,
// at debug level.
//
// The client never reads, and the answer is more than the buffers between
// the two hold, so the write cannot end before its deadline however slow the
// machine: the deadline is what ends it, and it stays short. Nothing the
// test waits for ends with it. That the probe made its answer is read from a
// counter, which stays as it is, and that the handler was held until the
// deadline from the connection, which the exporter closed no earlier than
// the deadline it had set on it; a handler seen in progress with its answer
// made is one a test that got no CPU in those 300ms never saw.
func TestAClientThatStopsReadingIsDropped(t *testing.T) {
	exposition := largeExposition()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, exposition)
	}))
	t.Cleanup(target.Close)
	server := verboseServer(t, false, passthrough("pass", "", ""))
	logs := &syncedBuffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server.answerWriteTimeout = 300 * time.Millisecond
	served := serveExporter(t, server)

	conn, err := net.Dial("tcp", served.address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	if _, err := fmt.Fprintf(conn, "GET /probe?collector=pass&target=%s HTTP/1.1\r\nHost: exporter\r\n\r\n", url.QueryEscape(target.URL)); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, "the probe to make its answer", func() bool {
		return metricValue(t, selfMetrics(t, server), `http_exporter_scrape_success_total{collector="pass"}`) == 1
	})
	testutil.WaitFor(t, "the handler to give up on a client that reads nothing", func() bool { return served.inProgress.Load() == 0 })
	// The connection is closed rather than kept for another request: half
	// an answer is on it.
	testutil.WaitFor(t, "the connection to be closed", func() bool { return served.closed.Load() == 1 })
	if deadlines, closedAt := served.onlyConn(t).noted(); len(deadlines) != 1 || closedAt.Before(deadlines[0]) {
		t.Fatalf("the exporter set %d write deadlines %v on the connection and closed it at %s; want one deadline, and the connection held until it", len(deadlines), deadlines, closedAt)
	}

	var dropped int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if record["msg"] == "a client did not read its answer in time; its connection is closed" {
			dropped++
			if record["level"] != "DEBUG" || record["path"] != "/probe" || record["write_timeout"] != "300ms" || record["remote_address"] != conn.LocalAddr().String() {
				t.Errorf("the drop is logged as %v", record)
			}
			continue
		}
		if record["level"] == "WARN" || record["level"] == "ERROR" {
			t.Errorf("a client that stopped reading is logged above debug level: %v", record)
		}
	}
	if dropped != 1 {
		t.Fatalf("the drop is logged %d times, want once:\n%s", dropped, logs.String())
	}
}

// The deadline counts from the answer's first byte and ends with the answer:
// however long a probe takes to make its answer, the client has the whole of
// AnswerWriteTimeout to read it in, and a later request on the same
// kept-alive connection is answered in full after the deadline of the answer
// before it has passed.
//
// No deadline is run against. The exporter's side of the connection notes
// the deadlines it is given: each answer's is set no earlier than
// AnswerWriteTimeout after its target answered, where one counted from the
// request would be earlier by the trip, and each answer sets one of its own.
// And an answer's deadline is not waited out: before the next request the
// connection is given a deadline already past, the most the deadline of the
// answer before could have left on it. A deadline short enough to wait out
// twice, 400ms, was one that a handler which got no CPU between its first
// byte and its last did not write its answer within.
func TestTheWriteDeadlineIsTheAnswersAlone(t *testing.T) {
	testutil.CaptureLogs(t)
	var mu sync.Mutex
	var answered []time.Time
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		answered = append(answered, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	served := serveExporter(t, server)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	probe := func() (body string, reused bool) {
		t.Helper()
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		request, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, "http://"+served.address+"/probe?collector=text&target="+url.QueryEscape(target.URL), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		answer, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, body %q, error %v", resp.StatusCode, answer, err)
		}
		return string(answer), reused
	}
	for n := 1; n <= 3; n++ {
		body, reused := probe()
		if reused != (n > 1) || !strings.Contains(body, "demo_value 1") {
			t.Fatalf("answer %d, on a reused connection %v: %q", n, reused, body)
		}
		conn := served.onlyConn(t)
		deadlines, closedAt := conn.noted()
		mu.Lock()
		trips := append([]time.Time(nil), answered...)
		mu.Unlock()
		if len(deadlines) != n || len(trips) != n || !closedAt.IsZero() {
			t.Fatalf("after answer %d the exporter has set %d write deadlines on the connection after %d trips, and closed it %v; want a deadline for each answer on a connection kept", n, len(deadlines), len(trips), !closedAt.IsZero())
		}
		if least := trips[n-1].Add(AnswerWriteTimeout); deadlines[n-1].Before(least) {
			t.Fatalf("the write deadline of answer %d is %s before the time its target answered at and %s more; want it counted from the answer's first byte", n, least.Sub(deadlines[n-1]), AnswerWriteTimeout)
		}
		// The next request finds a deadline that has passed.
		if err := conn.Conn.SetWriteDeadline(time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}
