//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The OTLP export loops wait by the intervals of the configuration in
// force (otlpEvery): a configuration put in force while they wait, by a
// reload or an allocator's bundle, has its intervals followed at once,
// counted from the last export, rather than once the wait armed with the
// one before has run out.

// intervalDocument is a configuration exporting the metrics, the log records
// and the spans to the receiver, each every interval.
func intervalDocument(receiver *logReceiver, interval string) string {
	return "otlp:\n  enabled: true\n  endpoint: " + receiver.server.URL + "/v1/metrics\n  compression: none\n  interval: " + interval +
		"\n  logs:\n    enabled: true\n    interval: " + interval +
		"\n  traces:\n    enabled: true\n    interval: " + interval + "\n" + testutil.MinimalConfig
}

// runExportLoops runs the three export loops of server until the test ends.
func runExportLoops(t *testing.T, server *Server) {
	t.Helper()
	server.SetOTLPLogs(NewOTLPLogs(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 3)
	for _, loop := range []func(context.Context){server.OTLPExportLoop, server.OTLPLogExportLoop, server.OTLPTraceExportLoop} {
		go func() {
			defer func() { done <- struct{}{} }()
			loop(ctx)
		}()
	}
	t.Cleanup(func() {
		cancel()
		for range 3 {
			<-done
		}
	})
}

// queueEverySignal queues a metric, a log record and a span for server's
// export.
func queueEverySignal(server *Server) {
	queueProbeMetric(server, "x", 1)
	slog.New(server.otlpLogs.Handler()).Info("a line to export")
	cfg := server.manager.Get().OTLP
	server.otlpTraces.add(queuedSpans(defaultResourceIdentity(cfg), []otlpSpan{{TraceID: "t", SpanID: "s", Name: "probe"}}), 10)
}

// exportedPaths waits up to within for the receiver to have been sent to
// each of the three endpoints, and returns those it was sent to.
func exportedPaths(receiver *logReceiver, within time.Duration) []string {
	deadline := time.Now().Add(within)
	for {
		receiver.mu.Lock()
		var paths []string
		for _, path := range receiver.paths {
			if !slices.Contains(paths, path) {
				paths = append(paths, path)
			}
		}
		receiver.mu.Unlock()
		slices.Sort(paths)
		if len(paths) == 3 || time.Now().After(deadline) {
			return paths
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var everyEndpoint = []string{"/v1/logs", "/v1/metrics", "/v1/traces"}

// An exporter in allocator mode begins with no configuration, whose
// intervals are the defaults; the bundle's of 1s are followed from when it
// comes.
func TestTheFirstBundlesOTLPIntervalsAreFollowedAtOnce(t *testing.T) {
	receiver := newLogReceiver(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	manager := config.NewAwaitingManager(path, testutil.QuietLogger(t))
	server := NewServer(manager, "python3", testutil.QuietLogger(t))
	runExportLoops(t, server)
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(intervalDocument(receiver, "1s")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reload("bundle"); err != nil {
		t.Fatal(err)
	}
	queueEverySignal(server)
	if got := exportedPaths(receiver, 2500*time.Millisecond); !slices.Equal(got, everyEndpoint) {
		t.Errorf("within 2.5s of a bundle with intervals of 1s the exports went to %v, want %v", got, everyEndpoint)
	}
}

// A reload that shortens the intervals from 45s to 1s has the next exports
// a second after the last, not 45s after it.
func TestAReloadShorteningTheOTLPIntervalsIsFollowedAtOnce(t *testing.T) {
	receiver := newLogReceiver(t)
	r := newReloadable(t, intervalDocument(receiver, "45s"), "")
	r.server = NewServer(r.manager, "python3", testutil.QuietLogger(t))
	runExportLoops(t, r.server)
	time.Sleep(50 * time.Millisecond)
	r.write(r.path, intervalDocument(receiver, "1s"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	queueEverySignal(r.server)
	if got := exportedPaths(receiver, 2500*time.Millisecond); !slices.Equal(got, everyEndpoint) {
		t.Errorf("within 2.5s of a reload to intervals of 1s the exports went to %v, want %v", got, everyEndpoint)
	}
}

// A reload that lengthens the intervals from 1s to 1h makes no export when
// the second runs out: the next is an hour after the last.
func TestAReloadLengtheningTheOTLPIntervalsExportsNothingEarly(t *testing.T) {
	receiver := newLogReceiver(t)
	r := newReloadable(t, intervalDocument(receiver, "1s"), "")
	r.server = NewServer(r.manager, "python3", testutil.QuietLogger(t))
	runExportLoops(t, r.server)
	r.write(r.path, intervalDocument(receiver, "1h"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	queueEverySignal(r.server)
	time.Sleep(2 * time.Second)
	if got := exportedPaths(receiver, 0); len(got) > 0 {
		t.Errorf("within 2s of a reload to intervals of 1h the exports went to %v", got)
	}
}
