//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// queueOTLP stages metrics under the exporter-wide OTLP resource.
func (s *Server) queueOTLP(set model.MetricSet) {
	s.queueOTLPResource(set, defaultResourceIdentity(s.manager.Get().OTLP), scrapeTime{})
}

// verboseRequestSeriesNames are the self-metric families the verbose mode
// republishes per request: the per-collector families that have a per-request
// value, which leaves out http_exporter_cache_entries.
func verboseRequestSeriesNames() []string {
	var names []string
	for _, d := range selfMetricDescriptors {
		if d.Value != nil {
			names = append(names, d.Name)
		}
	}
	return names
}

// publishStaticTarget is publishStaticResult for a result a test publishes
// itself: one without the age series, scraped at no time in particular.
func (s *Server) publishStaticTarget(target model.StaticTarget, identity otlpResourceIdentity, set model.MetricSet) {
	s.publishStaticResult(configRead{}, target, identity, set, time.Time{}, scrapeTime{})
}

// staticTargetResults returns the latest result of each target in force, by
// name, its data's age as of now, and forgets the results of targets no
// longer in force.
func (s *Server) staticTargetResults() []namedSet {
	out, _ := s.storedStaticResults(s.manager.StaticTargetFile(), time.Now())
	return out
}

// collector_files lists further files of collectors (config/collectorfiles.go). Each
// holds a collectors list and nothing else, and a collector name is unique
// across the configuration and every file.

func pathCollector(path string) model.Collector {
	c := testutil.Collector("tenants", "text")
	c.Request.Path = path
	return c
}

func scriptLimits() model.Limits { return model.Limits{ScriptTimeout: model.Duration(5 * time.Second)} }

// Requests made with the same TLS and HTTP/2 settings share one connection
// pool (fetch/transport.go).

// countingServer counts the connections made to it.
//
// It answers a request only once the client has reported it written
// (awaitRequestsReported), so that whether the next request reuses the
// connection depends on the exporter alone: on its keeping the pool, and
// reading the answer to its end.
func countingServer(t *testing.T, tlsServer bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		awaitRequestsReported(t)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	if tlsServer {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server, &conns
}

// awaitRequestsReported waits until every HTTP/1 connection this process
// has made as a client has reported the request it wrote.
//
// Go writes a request in a goroutine of the connection, which then reports
// the write and waits for the next request. When the answer has been read
// before that report, the connection waits 50ms for it and is otherwise
// closed rather than reused (net/http's persistConn.wroteRequest). The bytes
// are on the wire before the report is made, so a server in the same
// process can answer in between, and on a machine busy enough to leave the
// writing goroutine without a CPU for 50ms the next request then opens a
// second connection, whatever the exporter does. A test that counts
// connections must not answer in that gap.
//
// Nothing in net/http's API tells when the report has been made, but the
// goroutine dump does: the writing goroutine, persistConn.writeLoop, is back
// in its select. So this reads the dump until every such goroutine is, and
// there is at least one, the connection the request came on; should a later
// Go name them otherwise, it finds none and fails the test saying so rather
// than letting the gap back in.
func awaitRequestsReported(t *testing.T) {
	const writeLoop = "net/http.(*persistConn).writeLoop("
	var seen string
	reported := func() bool {
		stacks := make([]byte, 1<<16)
		for {
			n := runtime.Stack(stacks, true)
			if n < len(stacks) {
				stacks = stacks[:n]
				break
			}
			stacks = make([]byte, 2*len(stacks))
		}
		loops := 0
		for _, goroutine := range strings.Split(string(stacks), "\n\n") {
			if !strings.Contains(goroutine, writeLoop) {
				continue
			}
			loops++
			// The first line is the goroutine's state: "goroutine 52 [select]:".
			if state, _, _ := strings.Cut(goroutine, "\n"); !strings.Contains(state, "[select") {
				seen = goroutine
				return false
			}
		}
		if loops == 0 {
			seen = "no goroutine in " + writeLoop + ")"
		}
		return loops > 0
	}
	// The wait is a few microseconds; the bound is only for a write loop
	// that never comes back, which is a failure of its own.
	for deadline := time.Now().Add(30 * time.Second); !reported(); time.Sleep(50 * time.Microsecond) {
		if time.Now().After(deadline) {
			t.Errorf("the client never reported its request written; last seen:\n%s", seen)
			return
		}
	}
}

const watchConfigTemplate = "collectors:\n  - name: watched\n    request:\n      type: http\n    transform:\n      type: regex\n" +
	"    metrics:\n      - name: %s\n        expression: 'value=(\\d+)'\n"

func writeWatchedConfig(t *testing.T, path, metric string) {
	t.Helper()
	document := strings.Replace(watchConfigTemplate, "%s", metric, 1)
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
}

func watchedManager(t *testing.T) (*config.Manager, string) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	writeWatchedConfig(t, path, "first_value")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, path, slog.Default())
	manager.SetPythonPath("python3")
	return manager, path
}

// idleWorkers counts a collector's idle workers.
func idleWorkers(collector string) int { return transform.PythonWorkers().Snapshot(collector).Idle }

// installConfig puts an already validated cfg in force on server, as an
// accepted reload would, the static target file in force staying so. The
// server reads its configuration from its manager on every use, so a manager
// holding cfg stands in for one that reloaded it.
func installConfig(server *Server, cfg *model.Config) {
	manager := config.NewManager(cfg, "", server.logger)
	manager.SetTargets("", server.manager.StaticTargetFile())
	server.manager = manager
}
