//go:build !select_request_types || request_type_http

package exporter

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// resettingTarget is a target that resets every connection made to it once
// the request has arrived, or, when answers is set, answers value=42. close
// stops it listening, so that connections to it are refused.
type resettingTarget struct {
	listener net.Listener
	answers  atomic.Bool
}

func newResettingTarget(t *testing.T) *resettingTarget {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := &resettingTarget{listener: listener}
	t.Cleanup(target.close)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go target.serve(conn)
		}
	}()
	return target
}

func (r *resettingTarget) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		if line, err := reader.ReadString('\n'); err != nil || line == "\r\n" {
			break
		}
	}
	if r.answers.Load() {
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: close\r\n\r\nvalue=42\n"))
		_ = conn.Close()
		return
	}
	// Closed without lingering, the connection is reset rather than ended.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

func (r *resettingTarget) addr() string { return r.listener.Addr().String() }

func (r *resettingTarget) close() { _ = r.listener.Close() }

// A target that resets every connection fails every probe with an error
// that names the port that connection was made from, which is another each
// time. It is one failure to the log all the same: logged in full once, as
// an error, and then as repeats at debug level; five minutes on the line
// comes again with every repeat counted and when the failure began; and the
// recovery counts every failed probe. Each line, and each answer, has the
// error in full, with the port of that probe's connection. A different
// failure of the same target, a connection refused where it was reset, is
// still logged in full.
func TestATargetThatResetsEveryConnectionIsOneFailureToTheLog(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newResettingTarget(t)
	server, _ := newCacheTestServer(t, testutil.Collector("flaky", "text"))
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var later atomic.Int64
	server.failures.now = func() time.Time { return time.Now().Add(time.Duration(later.Load())) }
	url := "http://" + target.addr()
	reset := fmt.Sprintf(`HTTP request failed: Get "%s": read tcp 127.0.0.1:`, url)
	// probe returns the one line the failed probe logged.
	probe := func(want string) map[string]any {
		t.Helper()
		answer := probeOnce(t, server, probePath("flaky", url, ""), nil)
		lines := linesOf(t, logs, "probe failed")
		if len(lines) != 1 || lines[0]["stage"] != "http" {
			t.Fatalf("the failure is logged as %v, want one line of the http stage", lines)
		}
		text, _ := lines[0]["error"].(string)
		if !strings.HasPrefix(text, want) || strings.Contains(text, "#") {
			t.Fatalf("the line's error is %q, want it in full, starting %q", text, want)
		}
		if answer.Code != http.StatusBadGateway || !strings.Contains(answer.Body.String(), text) {
			t.Fatalf("answered %d %q, want 502 with the error %q", answer.Code, answer.Body, text)
		}
		return lines[0]
	}
	seen := map[any]bool{}
	for i := range 6 {
		line := probe(reset)
		seen[line["error"]] = true
		switch {
		case i == 0 && (line["level"] != "ERROR" || line["repeat"] != nil || line["repeated"] != nil):
			t.Errorf("the first failure is logged as %v, want in full as an error", line)
		case i > 0 && (line["level"] != "DEBUG" || line["repeat"] != true):
			t.Errorf("probe %d is logged as %v, want as a repeat at debug level", i+1, line)
		}
	}
	if len(seen) < 2 {
		t.Fatalf("six probes failed with one text, %v: the connections were all made from one port, which shows nothing", seen)
	}
	later.Store(int64(failureRepeatInterval + time.Second))
	if line := probe(reset); line["level"] != "ERROR" || line["repeated"] != float64(6) || line["failing_since"] == nil {
		t.Errorf("five minutes on the line is %v, want an error with repeated 6 and failing_since", line)
	}
	target.answers.Store(true)
	if answer := probeOnce(t, server, probePath("flaky", url, ""), nil); answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), "demo_value 42") {
		t.Fatalf("with the target answering: answered %d: %s", answer.Code, answer.Body)
	}
	if lines := linesOf(t, logs, "probe recovered"); len(lines) != 1 || lines[0]["failures"] != float64(7) {
		t.Errorf("the recovery is logged as %v, want one line with failures 7", lines)
	}

	target.answers.Store(false)
	if line := probe(reset); line["level"] != "ERROR" || line["repeat"] != nil {
		t.Errorf("the reset after the recovery is logged as %v, want in full as an error", line)
	}
	if line := probe(reset); line["level"] != "DEBUG" || line["repeat"] != true {
		t.Errorf("the reset after it is logged as %v, want as a repeat at debug level", line)
	}
	target.close()
	refused := fmt.Sprintf(`HTTP request failed: Get "%s": dial tcp %s: connect: connection refused`, url, target.addr())
	if line := probe(refused); line["level"] != "ERROR" || line["repeat"] != nil || line["error"] != refused {
		t.Errorf("the refused connection is logged as %v, want in full as an error", line)
	}
	if line := probe(refused); line["level"] != "DEBUG" || line["repeat"] != true {
		t.Errorf("the second refused connection is logged as %v, want as a repeat at debug level", line)
	}
}
