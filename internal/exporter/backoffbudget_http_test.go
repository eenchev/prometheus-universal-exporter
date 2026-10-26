//go:build !select_request_types || request_type_http

package exporter

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sluggishTarget answers after delay, value=1 or 500 while failing. It gives
// up waiting when the request goes away, so that closing it does not wait
// out the delay. It does not count the requests it is sent: whether a probe
// with a 10ms budget gets its request to the target before the budget runs
// out depends on how busy the machine is, so a count would test the
// machine; the answers to the probes show what reached the target.
type sluggishTarget struct {
	*httptest.Server
	delay   atomic.Int64
	failing atomic.Bool
}

func newSluggishTarget(t *testing.T, delay time.Duration) *sluggishTarget {
	t.Helper()
	target := &sluggishTarget{}
	target.delay.Store(int64(delay))
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay := time.Duration(target.delay.Load()); delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if target.failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

// scrapeTimeout is the header Prometheus sends a probe its scrape timeout
// in, which the probe's budget comes from.
func scrapeTimeout(seconds string) http.Header {
	return http.Header{scrapeTimeoutHeader: {seconds}}
}

// A caller cannot back a target off for everyone by asking for less time
// than it needs: probes whose scrape timeout header leaves them a budget
// below failure_backoff.min_budget, 1s by default, time out against a
// target answering in 100ms, and none of them counts. The next probe,
// with the budget Prometheus would give, reaches the target and succeeds.
func TestATimeoutBelowTheMinimumBudgetDoesNotBackTheTargetOff(t *testing.T) {
	target := newSluggishTarget(t, 100*time.Millisecond)
	server := modeServer(t, backoffCollector("slow", 3, time.Minute, 10*time.Minute))
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	probeURL := probeOf("slow", target.URL)
	for i := range 3 {
		if r := probeOnce(t, server, probeURL, scrapeTimeout("0.01")); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "ran out of its") {
			t.Fatalf("probe %d with a 10ms scrape timeout is answered %d: %s; want it timed out", i+1, r.Code, r.Body)
		}
	}
	if n := server.backoffs.remembered("slow"); n != 0 {
		t.Errorf("%d targets are remembered after the timeouts below min_budget, want none", n)
	}
	if r := probeOnce(t, server, probeURL, scrapeTimeout("10")); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 1") {
		t.Fatalf("the probe with Prometheus's own budget is answered %d: %s; want it to reach the target", r.Code, r.Body)
	}
}

// A timeout below min_budget is no evidence either way: it neither counts
// as a failure nor forgets the failures before it. Two failed probes and
// such a timeout leave the target contacted; the next failure is the third,
// and the probe after it backs off.
func TestATimeoutBelowTheMinimumBudgetNeitherCountsNorResets(t *testing.T) {
	target := newSluggishTarget(t, 0)
	target.failing.Store(true)
	server := modeServer(t, backoffCollector("flaky", 3, time.Minute, 10*time.Minute))
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	probeURL := probeOf("flaky", target.URL)
	for i := range 2 {
		if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "received HTTP status 500") {
			t.Fatalf("failing probe %d is answered %d: %s", i+1, r.Code, r.Body)
		}
	}
	target.delay.Store(int64(200 * time.Millisecond))
	if r := probeOnce(t, server, probeURL, scrapeTimeout("0.01")); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "ran out of its") {
		t.Fatalf("the probe with a 10ms scrape timeout is answered %d: %s; want it timed out", r.Code, r.Body)
	}
	target.delay.Store(0)
	if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "received HTTP status 500") {
		t.Fatalf("the probe after the timeout is answered %d: %s; want it to reach the target, the timeout not counted", r.Code, r.Body)
	}
	if r := probeOnce(t, server, probeURL, nil); !strings.Contains(r.Body.String(), "is backing off after 3 consecutive failed scrapes") {
		t.Fatalf("the probe after the third failure is answered %d: %s; want it backed off, the failures before the timeout kept", r.Code, r.Body)
	}
}

// A timeout of a budget at least min_budget counts: a target that does not
// answer within it backs off.
func TestATimeoutOfTheMinimumBudgetCounts(t *testing.T) {
	target := newSluggishTarget(t, 2*time.Second)
	c := backoffCollector("hung", 1, time.Minute, 10*time.Minute)
	floor := model.Duration(50 * time.Millisecond)
	c.FailureBackoff.MinBudget = &floor
	server := modeServer(t, c)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	probeURL := probeOf("hung", target.URL)
	// 0.2s less the offset is below half of it, so the budget is 100ms.
	if r := probeOnce(t, server, probeURL, scrapeTimeout("0.2")); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "ran out of its 100ms budget") {
		t.Fatalf("the probe of the hung target is answered %d: %s; want it timed out", r.Code, r.Body)
	}
	if r := probeOnce(t, server, probeURL, nil); !strings.Contains(r.Body.String(), "is backing off after 1 consecutive failed scrapes") {
		t.Fatalf("the probe after the timeout is answered %d: %s; want it backed off", r.Code, r.Body)
	}
}

// A failure that is not the deadline's counts whatever the budget: a refused
// connection within a budget below min_budget backs the target off.
func TestARefusedConnectionCountsWithASmallBudget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	_ = listener.Close()
	server := modeServer(t, backoffCollector("closed", 1, time.Minute, 10*time.Minute))
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	probeURL := probeOf("closed", address)
	// A 300ms budget, below the 1s floor, and far more than a refusal
	// takes.
	if r := probeOnce(t, server, probeURL, scrapeTimeout("0.6")); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "refused") {
		t.Fatalf("the probe of the closed port is answered %d: %s; want the connection refused", r.Code, r.Body)
	}
	if r := probeOnce(t, server, probeURL, nil); !strings.Contains(r.Body.String(), "is backing off after 1 consecutive failed scrapes") {
		t.Fatalf("the probe after the refusal is answered %d: %s; want it backed off", r.Code, r.Body)
	}
}
