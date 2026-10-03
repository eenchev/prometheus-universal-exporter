package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

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
	cfg, file := s.manager.InForce()
	slots := make(chan struct{}, file.ScrapeConcurrency())
	for _, target := range staticTargetsOf(file) {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.scrapeTarget(scrapeCtx, cfg, target)
		}()
	}
	wg.Wait()
}

func pathServer(t *testing.T, collectors ...model.Collector) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

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
