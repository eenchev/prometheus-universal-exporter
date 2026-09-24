package exporter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// Prometheus tells a target how long it will wait for a scrape in the
// X-Prometheus-Scrape-Timeout-Seconds header. A probe that takes longer is
// abandoned: Prometheus records only `up 0` and a generic timeout, and the
// reason — a slow target, a hung file read, a script stuck in a loop — never
// reaches it. So, as blackbox_exporter does, a probe gives itself that long
// less a small offset, and when the budget runs out it answers with the error
// that stopped it while Prometheus is still listening.
//
// The budget bounds the whole trip to the target: the request or file read,
// decoding, transforms and Python. A timeout probe parameter still bounds the
// request alone, and whichever ends first wins. Probes answered from the cache
// never need it. Identical probes that share one trip (probeflight.go) share
// the budget of the probe that started it.
//
// A probe that names no deadline at all — no header and no timeout parameter,
// as from curl, a script or the collectors page with its script off — gets
// --probe.default-timeout, so a target that accepts the connection and never
// answers cannot hold the probe, and its collector's max_concurrent_probes
// slot, for ever.

const scrapeTimeoutHeader = "X-Prometheus-Scrape-Timeout-Seconds"

// DefaultTimeoutOffset is how much of Prometheus's scrape timeout a probe
// leaves for writing its answer and for the network. blackbox_exporter uses
// the same default.
const DefaultTimeoutOffset = 500 * time.Millisecond

// DefaultProbeTimeout is how long a probe that names no deadline may take.
const DefaultProbeTimeout = 30 * time.Second

// Where a probe's budget came from, for the error that says it ran out.
const (
	budgetFromScrapeTimeout = "Prometheus's scrape timeout less --probe.timeout-offset"
	budgetFromDefault       = "--probe.default-timeout, as the probe named neither a scrape timeout nor a timeout parameter"
)

// probeBudget reads the scrape timeout Prometheus sent and returns how long the
// probe may take. A missing, unparseable or non-positive header gives no
// budget, so a probe from something other than Prometheus behaves as before.
// An offset larger than the timeout would leave nothing; the probe then keeps
// half the timeout rather than failing at once.
func probeBudget(h http.Header, offset time.Duration) time.Duration {
	raw := strings.TrimSpace(h.Get(scrapeTimeoutHeader))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0
	}
	timeout := time.Duration(seconds * float64(time.Second))
	budget := timeout - offset
	if budget < timeout/2 {
		budget = timeout / 2
	}
	return budget
}

// ValidateTimeoutOffset refuses a negative --probe.timeout-offset.
func ValidateTimeoutOffset(offset time.Duration) error {
	if offset < 0 {
		return fmt.Errorf("--probe.timeout-offset must not be negative, got %s", offset)
	}
	return nil
}

// SetTimeoutOffset sets how much of Prometheus's scrape timeout a probe leaves
// unused.
func (s *Server) SetTimeoutOffset(offset time.Duration) { s.timeoutOffset = offset }

// ValidateDefaultProbeTimeout refuses a negative --probe.default-timeout; zero
// gives a probe that names no deadline none.
func ValidateDefaultProbeTimeout(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--probe.default-timeout must not be negative, got %s", timeout)
	}
	return nil
}

// SetDefaultProbeTimeout sets how long a probe that names no deadline may
// take; zero leaves it unbounded.
func (s *Server) SetDefaultProbeTimeout(timeout time.Duration) { s.defaultProbeTimeout = timeout }

// probeDeadline is the budget of a probe: Prometheus's scrape timeout less the
// offset when it sent one, else the default timeout when the probe did not
// bound its request with a timeout parameter either, else none. source says
// where it came from.
func (s *Server) probeDeadline(h http.Header, overrides fetch.RequestOverrides) (budget time.Duration, source string) {
	if budget := probeBudget(h, s.timeoutOffset); budget > 0 {
		return budget, budgetFromScrapeTimeout
	}
	if overrides.Timeout <= 0 && s.defaultProbeTimeout > 0 {
		return s.defaultProbeTimeout, budgetFromDefault
	}
	return 0, ""
}

// explainBudget says so when an error is the probe's budget running out, since
// "context deadline exceeded" alone does not say whose deadline it was.
func explainBudget(ctx context.Context, budget time.Duration, source string, err error) error {
	if budget <= 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w (the probe ran out of its %s budget: %s)", err, budget, source)
}
