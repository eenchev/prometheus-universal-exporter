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
			if recorder.Code != endpoint.want {
				t.Errorf("%s %s answered %d, want %d: %.200s", endpoint.method, endpoint.path, recorder.Code, endpoint.want, recorder.Body)
				continue
			}
			if len(recorder.deadlines) != 1 || recorder.early != 0 {
				t.Errorf("%s %s (Accept-Encoding %q): %d write deadlines set, %d writes before the first; want one deadline before any write", endpoint.method, endpoint.path, encoding, len(recorder.deadlines), recorder.early)
				continue
			}
			if ahead := recorder.deadlines[0].Sub(before); ahead < AnswerWriteTimeout || ahead > AnswerWriteTimeout+10*time.Second {
				t.Errorf("%s %s: the write deadline is %s ahead, want %s from the answer's first byte", endpoint.method, endpoint.path, ahead, AnswerWriteTimeout)
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
// can block: its address, the handlers in progress and the connections it has
// closed.
type servedExporter struct {
	address    string
	inProgress atomic.Int64
	closed     atomic.Int64
}

func serveExporter(t *testing.T, server *Server) *servedExporter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := &servedExporter{address: listener.Addr().String()}
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
	testutil.WaitFor(t, "the probe to start writing its answer", func() bool {
		return served.inProgress.Load() == 1 && metricValue(t, selfMetrics(t, server), `http_exporter_scrape_success_total{collector="pass"}`) == 1
	})
	testutil.WaitFor(t, "the handler to give up on a client that reads nothing", func() bool { return served.inProgress.Load() == 0 })
	// The connection is closed rather than kept for another request: half
	// an answer is on it.
	testutil.WaitFor(t, "the connection to be closed", func() bool { return served.closed.Load() == 1 })

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
// a probe that takes longer than it to make its answer is still answered in
// full, and so is a later request on the same kept-alive connection, long
// after the first answer's deadline has passed.
func TestTheWriteDeadlineIsTheAnswersAlone(t *testing.T) {
	testutil.CaptureLogs(t)
	var slow atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if slow.Load() {
			time.Sleep(600 * time.Millisecond)
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	server.answerWriteTimeout = 400 * time.Millisecond
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
	if body, _ := probe(); !strings.Contains(body, "demo_value 1") {
		t.Fatalf("first answer: %q", body)
	}
	// Twice the deadline later, on the same connection.
	time.Sleep(800 * time.Millisecond)
	if body, reused := probe(); !reused || !strings.Contains(body, "demo_value 1") {
		t.Fatalf("second answer on a reused connection (%v): %q", reused, body)
	}
	// A trip longer than the deadline.
	slow.Store(true)
	if body, reused := probe(); !reused || !strings.Contains(body, "demo_value 1") {
		t.Fatalf("the answer of a slow probe on a reused connection (%v): %q", reused, body)
	}
}
