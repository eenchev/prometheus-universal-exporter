package exporter

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// collector_files lists further files of collectors (config/collectorfiles.go). Each
// holds a collectors list and nothing else, and a collector name is unique
// across the configuration and every file.

func pathCollector(path string) model.Collector {
	c := testutil.Collector("tenants", "text")
	c.Request.Path = path
	return c
}

func scriptLimits() model.Limits { return model.Limits{ScriptTimeout: model.Duration(5 * time.Second)} }

// Python scripts run in long-lived workers (transform/pythonworker.go). These tests pin
// reuse, isolation, timeouts, crashes, output limits and the sandbox.

// requirePython skips a test without python3, and gives it a worker pool of
// its own (usePythonPool).
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	usePythonPool(t)
}

// usePythonPool runs the test against a fresh worker pool and restores the
// previous one afterwards (transform.IsolatePythonWorkers). Every count a test reads
// from the pool is then its own, so the tests pass under -count=N and
// -shuffle=on. Tests do not run in parallel, so swapping the pool is safe.
func usePythonPool(t *testing.T) {
	t.Helper()
	t.Cleanup(transform.IsolatePythonWorkers())
}

// fixtureType is a second request type registered only for these tests, so
// the per-type rules can be exercised while http is the only real one. It
// accepts path and nothing else, and its fetch returns a canned body.
func registerFixtureType(t *testing.T) {
	t.Helper()
	fetch.RequestTypes["fixture"] = &fetch.RequestType{
		Name:         "fixture",
		Fields:       []string{"path"},
		Overrides:    []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*model.Collector) error { return nil },
		Fetch: func(_ context.Context, target string, c *model.Collector, _ fetch.RequestOverrides, _ http.Header) (*fetch.HTTPResponse, error) {
			return &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("value=5\n"), Target: target, Collector: c.Name}, nil
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "fixture") })
}

func fixtureCollector() model.Collector {
	c := testutil.Collector("fixed", "text")
	c.Request = model.RequestConfig{Type: "fixture", Path: "/data"}
	return c
}

func otlpConfig(endpoint string) model.OTLPConfig {
	return model.OTLPConfig{
		Enabled:            true,
		Endpoint:           endpoint,
		ServiceName:        "prometheus-universal-exporter",
		ResourceAttributes: map[string]string{"deployment.environment": "test"},
		Interval:           model.Duration(time.Minute),
		Timeout:            model.Duration(5 * time.Second),
	}
}

func regexCollector(name string) model.Collector {
	return model.Collector{
		Name:      name,
		Request:   model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Transform: model.TransformConfig{Type: "regex"},
		Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: `v=(\d+) (?P<who>\S+)`, Labels: []model.LabelRule{{Name: "who", Expression: "who"}}}},
	}
}

// Requests made with the same TLS and HTTP/2 settings share one connection
// pool (fetch/transport.go).

// countingServer counts the connections made to it.
func countingServer(t *testing.T, tlsServer bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
// accepted reload would. The server reads its configuration from its manager
// on every use, so a manager holding cfg stands in for one that reloaded it.
func installConfig(server *Server, cfg *model.Config) {
	server.manager = config.NewManager(cfg, "", server.logger)
}

// parseExposition reads body as the Prometheus text format, the way a
// collector passing Prometheus text through reads a target.
func parseExposition(body []byte) error {
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{"Content-Type": {"text/plain; version=0.0.4"}}}
	_, err := decode.Decode(r, &model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}})
	return err
}

// scrapeStaticTargets scrapes every static target once, now, within
// budget, as StaticScrapeLoop would when each came due: the tests drive
// scrapes directly rather than wait for the schedule.
func (s *Server) scrapeStaticTargets(ctx context.Context, budget time.Duration) {
	if budget <= 0 {
		budget = 30 * time.Second
	}
	scrapeCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var wg sync.WaitGroup
	slots := make(chan struct{}, staticTargetConcurrency)
	for _, target := range s.manager.StaticTargets() {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.scrapeTarget(scrapeCtx, target)
		}()
	}
	wg.Wait()
}
