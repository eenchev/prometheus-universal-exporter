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
// A probe without Prometheus's header — from curl, a script or the
// collectors page with its script off — gets --probe.default-timeout, so a
// target that accepts the connection and never answers cannot hold the probe,
// and its collector's max_concurrent_probes slot, for ever. A timeout
// parameter bounds the request within that budget and cannot lift it: a
// caller asking for timeout=24h still gets the default.

const scrapeTimeoutHeader = "X-Prometheus-Scrape-Timeout-Seconds"

// DefaultTimeoutOffset is how much of Prometheus's scrape timeout a probe
// leaves for writing its answer and for the network. blackbox_exporter uses
// the same default.
const DefaultTimeoutOffset = 500 * time.Millisecond

// DefaultProbeTimeout is how long a probe without a scrape timeout may take.
const DefaultProbeTimeout = 30 * time.Second

// Where a probe's budget came from, for the error that says it ran out.
const (
	budgetFromScrapeTimeout = "Prometheus's scrape timeout less --probe.timeout-offset"
	budgetFromDefault       = "--probe.default-timeout, as the probe named no scrape timeout"
)

// maxScrapeTimeout is the longest scrape timeout a probe is given, however
// long the header says: Prometheus's own timeout cannot exceed its scrape
// interval, which is rarely more than minutes.
const maxScrapeTimeout = time.Hour

// probeBudget reads the scrape timeout Prometheus sent and returns how long the
// probe may take. A missing, unparseable or non-positive header gives no
// budget, so a probe from something other than Prometheus behaves as before.
// An offset larger than the timeout would leave nothing; the probe then keeps
// half the timeout rather than failing at once. A timeout above
// maxScrapeTimeout counts as that.
func probeBudget(h http.Header, offset time.Duration) time.Duration {
	raw := strings.TrimSpace(h.Get(scrapeTimeoutHeader))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 || math.IsNaN(seconds) {
		return 0
	}
	// Whoever reaches /probe sends the header, so it is bounded: a scrape
	// timeout of years would hold a slot of max_concurrent_probes for as
	// long as the target hangs, and one past what time.Duration holds would
	// overflow.
	seconds = min(seconds, maxScrapeTimeout.Seconds())
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
// gives a probe without a scrape timeout none.
func ValidateDefaultProbeTimeout(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--probe.default-timeout must not be negative, got %s", timeout)
	}
	return nil
}

// SetDefaultProbeTimeout sets how long a probe without a scrape timeout may
// take; zero leaves it unbounded.
func (s *Server) SetDefaultProbeTimeout(timeout time.Duration) { s.defaultProbeTimeout = timeout }

// probeDeadline is the budget of a probe: Prometheus's scrape timeout less the
// offset when it sent one, else the default timeout, whatever timeout
// parameter the probe gave, unless the default is 0. source says where it
// came from, and that the timeout parameter was capped when it asked for
// more, so an error at the default's end does not read as the parameter
// having been ignored.
func (s *Server) probeDeadline(h http.Header, overrides fetch.RequestOverrides) (budget time.Duration, source string) {
	if budget := probeBudget(h, s.timeoutOffset); budget > 0 {
		return budget, budgetFromScrapeTimeout
	}
	if s.defaultProbeTimeout > 0 {
		source := budgetFromDefault
		if overrides.Timeout > s.defaultProbeTimeout {
			source += fmt.Sprintf("; its timeout parameter, %s, is capped to it", overrides.Timeout)
		}
		return s.defaultProbeTimeout, source
	}
	return 0, ""
}

// explainBudget says so when an error is the trip's budget running out, since
// "context deadline exceeded" alone does not say whose deadline it was. what
// names the trip: a probe, or a static target's scrape.
func explainBudget(ctx context.Context, what string, budget time.Duration, source string, err error) error {
	if budget <= 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w (the %s ran out of its %s budget: %s)", err, what, budget, source)
}

// budgetFromInterval is where a static target scrape's budget comes from.
const budgetFromInterval = "the target's interval, which a scrape must end within"
