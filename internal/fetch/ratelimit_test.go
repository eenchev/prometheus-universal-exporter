package fetch

import (
	"context"
	"errors"
	"math"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// rateClock is the clock request.rate_limit reads in a test: it stands
// still but for the waits, which move it on at once instead of sleeping,
// and are recorded.
type rateClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

// freshRateLimits gives the test buckets and counts of its own, of
// request.rate_limit and of rate_limits, no entry of rate_limits, and a clock
// that only the waits move, from now, so the deadlines of real contexts
// still hold against it.
func freshRateLimits(t *testing.T) *rateClock {
	t.Helper()
	clock := &rateClock{now: time.Now()}
	previous, previousNow, previousSleep := rateLimits, rateLimitNow, rateLimitSleep
	previousHosts, previousRules := hostRateLimits, hostRateLimitRules.Load()
	rateLimits, hostRateLimits = newRateLimiterSet(), newRateLimiterSet()
	hostRateLimitRules.Store(nil)
	resetHostRateLimits()
	t.Cleanup(func() { hostRateLimits = previousHosts; hostRateLimitRules.Store(previousRules) })
	rateLimitNow = func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return clock.now
	}
	rateLimitSleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.mu.Lock()
		defer clock.mu.Unlock()
		clock.now = clock.now.Add(d)
		clock.waits = append(clock.waits, d)
		return nil
	}
	t.Cleanup(func() { rateLimits, rateLimitNow, rateLimitSleep = previous, previousNow, previousSleep })
	return clock
}

func rateLimitOf(perSecond float64, burst int) *model.RateLimitConfig {
	return &model.RateLimitConfig{RequestsPerSecond: perSecond, Burst: &burst}
}

// The buckets are bounded: beyond rateLimitMaxBuckets the least recently
// used is dropped, and a bucket that has filled up again is dropped as soon
// as another is asked for, since it holds nothing a new one would not.
func TestRateLimitBucketsAreBounded(t *testing.T) {
	freshRateLimits(t)
	previous := rateLimitMaxBuckets
	rateLimitMaxBuckets = 5
	t.Cleanup(func() { rateLimitMaxBuckets = previous })
	now := time.Now()
	key := func(i int) rateLimitKey {
		return rateLimitKey{collector: "web", origin: "http://host" + string(rune('a'+i)) + ":80", perSecond: 1, burst: 1}
	}
	for i := range 20 {
		if wait, err := rateLimits.reserve(key(i), now, time.Time{}, false, true); wait != 0 || err != nil {
			t.Fatalf("origin %d: waited %s, %v; want its first request sent at once", i, wait, err)
		}
		if n := rateLimits.size(); n > 5 {
			t.Fatalf("after %d origins, %d buckets are kept; want at most 5", i+1, n)
		}
	}
	// The most recently used is still kept: its next request waits.
	if wait, _ := rateLimits.reserve(key(19), now, time.Time{}, false, true); wait != time.Second {
		t.Errorf("the last origin's second request waited %s; want 1s", wait)
	}
	// A second later every bucket has filled up again, so asking for a new
	// one drops them all.
	if _, err := rateLimits.reserve(key(30), now.Add(3*time.Second), time.Time{}, false, true); err != nil {
		t.Fatal(err)
	}
	if n := rateLimits.size(); n != 1 {
		t.Errorf("%d buckets are kept after every other filled up again; want 1", n)
	}
}

// A reload drops the buckets and counts of the collectors it removed.
func TestRateLimitsFollowTheConfiguration(t *testing.T) {
	freshRateLimits(t)
	now := time.Now()
	for _, collector := range []string{"kept", "gone"} {
		k := rateLimitKey{collector: collector, origin: "http://a:80", perSecond: 1, burst: 1}
		_, _ = rateLimits.reserve(k, now, time.Time{}, false, true)
		_, _ = rateLimits.reserve(k, now, time.Time{}, false, true)
	}
	FollowConfiguration([]model.Collector{{Name: "kept"}}, nil)
	if n := rateLimits.size(); n != 1 {
		t.Errorf("%d buckets kept; want the kept collector's one", n)
	}
	if waited, _ := CollectorRateLimit("gone"); waited != 0 {
		t.Errorf("the removed collector still counts %d waits", waited)
	}
	if waited, _ := CollectorRateLimit("kept"); waited != 1 {
		t.Errorf("the kept collector counts %d waits; want 1", waited)
	}
	// The exporter's shutdown forgets them all.
	CloseConnections()
	if waited, _ := CollectorRateLimit("kept"); waited != 0 || rateLimits.size() != 0 {
		t.Errorf("after the shutdown %d buckets are kept and %d waits counted; want none", rateLimits.size(), waited)
	}
}

// A wait cut short gives its turn back, so a trip that gave up does not
// slow those after it.
func TestACutShortRateLimitWaitGivesItsTurnBack(t *testing.T) {
	freshRateLimits(t)
	ctx := context.WithValue(context.Background(), rateLimitContextKey{}, &rateLimitSetting{collector: "web", perSecond: 1, burst: 1})
	u := mustParseURL(t, "http://legacy.example/")
	if _, err := waitRateLimit(ctx, u); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := waitRateLimit(cancelled, u); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "for its turn under the rate limit of 1 request per second for http://legacy.example:80 was cut short") {
		t.Fatalf("got %v; want the wait cut short, saying so", err)
	}
	// One turn is promised, the cut-short one's given back: the next
	// request waits one second, not two.
	if wait, _ := rateLimits.reserve(rateLimitKey{collector: "web", origin: "http://legacy.example:80", perSecond: 1, burst: 1}, rateLimitNow(), time.Time{}, false, true); wait != time.Second {
		t.Errorf("the next request waits %s; want 1s", wait)
	}
}

// Two spellings of one origin share a bucket.
func TestRequestOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"http://Legacy.Example/a":        "http://legacy.example:80",
		"http://legacy.example:80/b?q=1": "http://legacy.example:80",
		"HTTPS://legacy.example/":        "https://legacy.example:443",
		"https://[2001:DB8::1]:8443/x":   "https://[2001:db8::1]:8443",
	} {
		if got := requestOrigin(mustParseURL(t, raw)); got != want {
			t.Errorf("%s: origin %s, want %s", raw, got, want)
		}
	}
}

// request.rate_limit is http's and graphite's: every other type refuses it
// at load, as any key it does not take; for those two its values are
// checked and its burst filled in.
func TestRateLimitIsCheckedWhenItLoads(t *testing.T) {
	for _, name := range BuiltRequestTypes() {
		c := model.Collector{Name: "c", Request: model.RequestConfig{Type: name, RateLimit: rateLimitOf(1, 1)}}
		err := ValidateRequest(&c)
		if name == RequestTypeHTTP || name == RequestTypeGraphite {
			if err != nil && strings.Contains(err.Error(), "rate_limit") {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), `sets request.rate_limit, which does not apply to request.type "`+name+`"`) {
			t.Errorf("%s: got %v; want rate_limit refused for the type", name, err)
		}
	}
	if _, ok := RequestTypes[RequestTypeHTTP]; !ok {
		return
	}
	zero := 0
	for _, tc := range []struct {
		limit *model.RateLimitConfig
		want  string
	}{
		{&model.RateLimitConfig{}, `request.rate_limit.requests_per_second is 0; it is required`},
		{&model.RateLimitConfig{RequestsPerSecond: -1}, `requests_per_second is -1`},
		{&model.RateLimitConfig{RequestsPerSecond: math.NaN()}, `requests_per_second is NaN`},
		{&model.RateLimitConfig{RequestsPerSecond: math.Inf(1)}, `requests_per_second is +Inf`},
		{&model.RateLimitConfig{RequestsPerSecond: 1, Burst: &zero}, `request.rate_limit.burst is 0; it is how many requests may be sent at once after a pause, at least 1`},
	} {
		c := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeHTTP, RateLimit: tc.limit}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v; want %q", tc.limit, err, tc.want)
		}
	}
	c := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeHTTP, RateLimit: &model.RateLimitConfig{RequestsPerSecond: 0.5}}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if c.Request.RateLimit.Burst == nil || *c.Request.RateLimit.Burst != 1 {
		t.Errorf("burst %v; want it filled in as 1", c.Request.RateLimit.Burst)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
