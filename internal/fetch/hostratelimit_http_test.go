//go:build !select_request_types || request_type_http

package fetch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Two collectors asking one host share the bucket of the entry of
// rate_limits it matches: the second waits for the turn the first took, or
// is refused when the turn would come past its deadline, though neither has
// a rate_limit of its own. A debug run takes its turn and is not counted.
func TestHostRateLimitIsSharedByEveryCollector(t *testing.T) {
	clock := freshRateLimits(t)
	server, hits := valueServer(t)
	FollowHostRateLimits([]model.HostRateLimit{hostEntry("api", 1, 1, "127.0.0.1")})
	a := limitedCollector(t, "a", nil, nil)
	b := limitedCollector(t, "b", nil, nil)
	if err := fetchCounted(server.URL, a, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	err := fetchCounted(server.URL, b, 500*time.Millisecond)
	if !errors.Is(err, errRateLimitRefused) || !strings.Contains(err.Error(), `rate limit of 1 request per second of rate_limits entry "api" for 127.0.0.1 would delay the request past its deadline`) {
		t.Fatalf("got %v; want b refused by the entry", err)
	}
	if err := fetchCounted(server.URL, b, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	traced, err := fetchBy(server.URL, a, RequestOverrides{}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := clock.waited(); hits.Load() != 3 || !reflect.DeepEqual(got, []time.Duration{time.Second, time.Second}) {
		t.Errorf("%d requests sent after the waits %v; want 3 after two of 1s", hits.Load(), got)
	}
	note := `sent after waiting 1s for its turn under the rate limit of 1 request per second of rate_limits entry "api" for 127.0.0.1`
	if len(traced) != 1 || traced[0].Wait != note {
		t.Errorf("the trace is %+v; want the wait %q", traced, note)
	}
	if counts := HostRateLimitCounts(); len(counts) != 1 || counts[0].Waited != 1 || counts[0].Refused != 1 {
		t.Errorf("counts %+v; want 1 wait and 1 refusal, the debug run's not counted", counts)
	}
	for _, name := range []string{"a", "b"} {
		if waited, refused := CollectorRateLimit(name); waited != 0 || refused != 0 {
			t.Errorf("collector %s counted %d waits and %d refusals under its own limit, which it has not", name, waited, refused)
		}
	}
}

// The collector's own rate_limit still applies beside an entry: a request
// waits for whichever of the two has no turn to spare.
func TestTheCollectorsRateLimitAppliesBesideTheHosts(t *testing.T) {
	clock := freshRateLimits(t)
	server, _ := valueServer(t)
	FollowHostRateLimits([]model.HostRateLimit{hostEntry("api", 100, 100, "127.0.0.1")})
	c := limitedCollector(t, "own", rateLimitOf(1, 1), nil)
	for range 2 {
		if err := fetchCounted(server.URL, c, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.waited(); !reflect.DeepEqual(got, []time.Duration{time.Second}) {
		t.Errorf("waits %v; want one of 1s, under the collector's own limit", got)
	}
	if waited, _ := CollectorRateLimit("own"); waited != 1 {
		t.Errorf("the collector counted %d waits; want 1", waited)
	}
	if counts := HostRateLimitCounts(); len(counts) != 1 || counts[0].Waited != 0 {
		t.Errorf("counts %+v; want no wait under the entry", counts)
	}
}

// A redirect hop to a host of an entry takes a turn under it, though the
// request it follows went to another host, which does not; and a login's
// request takes one as the request it logs in for does.
func TestHostRateLimitCountsRedirectsAndLogins(t *testing.T) {
	t.Run("redirects", func(t *testing.T) {
		clock := freshRateLimits(t)
		var hits atomic.Int64
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.URL.Path == "/old" {
				// The same server under the name localhost.
				moved, _ := url.Parse(server.URL)
				moved.Host = "localhost:" + moved.Port()
				http.Redirect(w, r, moved.String()+"/new", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte("value=1"))
		}))
		t.Cleanup(server.Close)
		FollowHostRateLimits([]model.HostRateLimit{hostEntry("moved", 1, 1, "localhost")})
		c := limitedCollector(t, "c", nil, func(c *model.Collector) { c.Request.FollowRedirects = true })
		for range 2 {
			if err := fetchCounted(server.URL+"/old", c, 10*time.Second); err != nil {
				t.Fatal(err)
			}
		}
		if got := clock.waited(); hits.Load() != 4 || !reflect.DeepEqual(got, []time.Duration{time.Second}) {
			t.Errorf("%d requests sent after the waits %v; want 4 after one of 1s, the second hop's", hits.Load(), got)
		}
		if counts := HostRateLimitCounts(); len(counts) != 1 || counts[0].Waited != 1 {
			t.Errorf("counts %+v; want the hop's wait", counts)
		}
	})
	t.Run("logins", func(t *testing.T) {
		clock := freshRateLimits(t)
		freshLogins(t)
		server := newLoginServer(t, tokenAnswer(3600), bearer("tok1"))
		FollowHostRateLimits([]model.HostRateLimit{hostEntry("api", 1, 1, "127.0.0.1")})
		c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "s3cret"})})
		traced, err := fetchBy(server.URL+"/data", c, RequestOverrides{}, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := clock.waited(); server.logins.Load() != 1 || server.requests.Load() != 1 || !reflect.DeepEqual(got, []time.Duration{time.Second}) {
			t.Errorf("%d logins and %d requests after the waits %v; want one of each after one of 1s", server.logins.Load(), server.requests.Load(), got)
		}
		if len(traced) != 2 || !traced[0].Login || traced[0].Wait != "" || !strings.Contains(traced[1].Wait, `rate_limits entry "api"`) {
			t.Errorf("the trace is %+v; want the login sent at once and the request after its wait under the entry", traced)
		}
	})
}
