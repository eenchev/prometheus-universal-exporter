package exporter

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Delivering OTLP exports: compression, retries, what happens to data that
// did not get through, the export status self-metrics, the last export at
// shutdown, readiness and the proxy from the environment (otlp.go,
// otlp.go, readiness.go, fetch/transport.go).

func TestOTLPCompressionIsValidated(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://otel:4318/v1/metrics")}
	cfg.OTLP.Compression = "zstd"
	if err := config.Validate(cfg); err == nil || !strings.Contains(err.Error(), "otlp.compression") {
		t.Fatalf("otlp.compression zstd: %v", err)
	}
}

// Retry-After is honoured, in seconds or as a date.
func TestOTLPRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":                              0,
		"3":                             3 * time.Second,
		"-1":                            0,
		"soon":                          0,
		"Tue, 01 Sep 2026 12:00:07 GMT": 7 * time.Second,
		"Tue, 01 Sep 2026 11:00:00 GMT": 0,
	} {
		if got := retryAfter(value, now); got != want {
			t.Errorf("Retry-After %q = %s, want %s", value, got, want)
		}
	}
	if otlpBackoff(0) != otlpRetryBackoff || otlpBackoff(1) != 2*otlpRetryBackoff || otlpBackoff(30) != otlpMaxBackoff {
		t.Errorf("backoff %s %s %s", otlpBackoff(0), otlpBackoff(1), otlpBackoff(30))
	}
}

// The times a run of failures keeps, to say when the exporter is unready
// since under any threshold, stop at the most a threshold may be, however
// long the run.
func TestARunOfOTLPFailuresKeepsBoundedTimes(t *testing.T) {
	o := &otlpStatus{}
	start := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	n := 0
	o.now = func() time.Time { n++; return start.Add(time.Duration(n) * time.Second) }
	for range model.MaxOTLPUnreadyAfterFailures + 500 {
		o.record("http://collector.invalid/v1/metrics", otlpExport{}, errors.New("503 Service Unavailable"))
	}
	if got := len(o.history.failureEnds); got != model.MaxOTLPUnreadyAfterFailures {
		t.Errorf("a run of %d failures keeps %d times, want %d", model.MaxOTLPUnreadyAfterFailures+500, got, model.MaxOTLPUnreadyAfterFailures)
	}
	if got, want := o.history.unreadySince(model.MaxOTLPUnreadyAfterFailures), start.Add(model.MaxOTLPUnreadyAfterFailures*time.Second); !got.Equal(want) {
		t.Errorf("unready since %v under the most, want %v", got, want)
	}
	o.record("http://collector.invalid/v1/metrics", otlpExport{}, nil)
	if o.history.failureEnds != nil {
		t.Errorf("a success keeps %d times", len(o.history.failureEnds))
	}
}
