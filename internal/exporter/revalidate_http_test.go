//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// validatingTarget answers {"value": N} with an ETag and a Last-Modified,
// and 304 Not Modified, with the headers notModified holds, to a request
// whose If-None-Match is its ETag, or, when it sends no ETag, whose
// If-Modified-Since is its Last-Modified. It keeps the headers of every
// request.
type validatingTarget struct {
	mu           sync.Mutex
	etag         string
	lastModified string
	value        int
	notModified  http.Header
	// fail answers 500 to every request while it is set.
	fail     bool
	requests []http.Header
}

func (v *validatingTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = append(v.requests, r.Header.Clone())
	if v.fail {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	matches := v.etag != "" && r.Header.Get("If-None-Match") == v.etag ||
		v.etag == "" && v.lastModified != "" && r.Header.Get("If-Modified-Since") == v.lastModified
	if matches {
		for name, values := range v.notModified {
			w.Header()[name] = values
		}
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if v.etag != "" {
		w.Header().Set("ETag", v.etag)
	}
	if v.lastModified != "" {
		w.Header().Set("Last-Modified", v.lastModified)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Mode", "first")
	_, _ = fmt.Fprintf(w, `{"value": %d}`, v.value)
}

// set changes the target's answer under its lock.
func (v *validatingTarget) set(change func(v *validatingTarget)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	change(v)
}

// asked returns the headers of every request so far.
func (v *validatingTarget) asked() []http.Header {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]http.Header(nil), v.requests...)
}

func newValidatingTarget(t *testing.T, v *validatingTarget) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(v)
	t.Cleanup(server.Close)
	return server
}

// revalidatingCollector reads .value with jq, and the status and the
// X-Mode header the rules see as $status and $headers, cached for ttl.
func revalidatingCollector(name string, ttl time.Duration) model.Collector {
	c := testutil.Collector(name, "json")
	c.Transform.Type = "jq"
	c.Cache.TTL = model.Duration(ttl)
	c.Metrics = []model.MetricRule{
		{Name: "demo_value", Type: model.GaugeMetricType, Expression: ".value"},
		{Name: "demo_status", Type: model.GaugeMetricType, Expression: "$status", Labels: []model.LabelRule{{Name: "mode", Expression: `$headers["x-mode"]`}}},
	}
	return c
}

// An expired cached response that carried an ETag and a Last-Modified is
// asked for again with If-None-Match and If-Modified-Since, as received; a
// 304 answers with the kept body, which the rules read again with the
// original status and the headers the 304 updated, and the entry is fresh
// again for the collector's ttl. The revalidation is counted, and the
// self-metrics show the 304 the target sent.
func TestAnExpiredResponseIsRevalidatedAndTransformedAgain(t *testing.T) {
	testutil.CaptureLogs(t)
	const lastModified = "Wed, 21 Oct 2026 07:28:00 GMT"
	v := &validatingTarget{etag: `W/"v1"`, lastModified: lastModified, value: 42, notModified: http.Header{"X-Mode": {"revalidated"}, "Cache-Control": {"max-age=60"}}}
	target := newValidatingTarget(t, v)
	server := modeServer(t, revalidatingCollector("etag", time.Minute))
	path := probePath("etag", target.URL, "")

	first := probeOnce(t, server, path, nil)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `demo_status{mode="first"} 200`) {
		t.Fatalf("first probe answered %d: %s", first.Code, first.Body)
	}
	outOfDate(server, true)
	second := probeOnce(t, server, path, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second probe answered %d: %s", second.Code, second.Body)
	}
	for _, want := range []string{"demo_value 42\n", `demo_status{mode="revalidated"} 200` + "\n"} {
		if !strings.Contains(second.Body.String(), want) {
			t.Errorf("no %q in the revalidated answer:\n%s", want, second.Body)
		}
	}
	asked := v.asked()
	if len(asked) != 2 {
		t.Fatalf("the target was asked %d times, want 2", len(asked))
	}
	if got := asked[0].Get("If-None-Match") + asked[0].Get("If-Modified-Since"); got != "" {
		t.Errorf("the first request was conditional: %q", got)
	}
	if got := asked[1].Get("If-None-Match"); got != `W/"v1"` {
		t.Errorf("If-None-Match = %q, want the ETag as received", got)
	}
	if got := asked[1].Get("If-Modified-Since"); got != lastModified {
		t.Errorf("If-Modified-Since = %q, want the Last-Modified as received", got)
	}
	// Fresh again: the next probe is answered from the cache.
	if third := probeOnce(t, server, path, nil); third.Code != http.StatusOK || len(v.asked()) != 2 {
		t.Fatalf("answered %d after %d requests, want a cache hit: %s", third.Code, len(v.asked()), third.Body)
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_cache_revalidations_total{collector="etag",result="not_modified"} 1`,
		`http_exporter_cache_revalidations_total{collector="etag",result="modified"} 0`,
		`http_exporter_scrape_http_status_code{collector="etag"} 304`,
		`http_exporter_scrape_response_bytes{collector="etag"} 0`,
		`http_exporter_cache_hits_total{collector="etag"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics:\n%s", want, metrics)
		}
	}
}

// A changed response replaces the kept one: the target's new answer to the
// conditional request is the answer, counted as modified, and its own ETag
// is what the next revalidation sends.
func TestAChangedResponseReplacesTheKeptOne(t *testing.T) {
	testutil.CaptureLogs(t)
	v := &validatingTarget{etag: `"v1"`, value: 1}
	target := newValidatingTarget(t, v)
	server := modeServer(t, revalidatingCollector("etag", time.Minute))
	path := probePath("etag", target.URL, "")
	probeOnce(t, server, path, nil)
	v.set(func(v *validatingTarget) { v.etag, v.value = `"v2"`, 2 })
	outOfDate(server, true)
	if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 2\n") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	outOfDate(server, true)
	if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 2\n") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	asked := v.asked()
	if len(asked) != 3 || asked[1].Get("If-None-Match") != `"v1"` || asked[2].Get("If-None-Match") != `"v2"` {
		t.Fatalf("If-None-Match sent: %q", ifNoneMatches(asked))
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_cache_revalidations_total{collector="etag",result="not_modified"} 1`,
		`http_exporter_cache_revalidations_total{collector="etag",result="modified"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics:\n%s", want, metrics)
		}
	}
}

func ifNoneMatches(headers []http.Header) []string {
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		out = append(out, h.Get("If-None-Match"))
	}
	return out
}

// Only a validator keeps an expired entry: one without ETag or
// Last-Modified is dropped at expiry and asked for unconditionally, and so
// is every response of a collector with cache.revalidate false, of a
// collector whose method is not GET or HEAD, and of a paginated one.
func TestOnlyARevalidableResponseIsKeptAndRevalidated(t *testing.T) {
	testutil.CaptureLogs(t)
	off := revalidatingCollector("off", time.Minute)
	off.Cache.Revalidate = new(false)
	posting := revalidatingCollector("posting", time.Minute)
	posting.Request.Method = http.MethodPost
	for _, tc := range []struct {
		name      string
		collector model.Collector
		v         *validatingTarget
	}{
		{"no validator", revalidatingCollector("plain", time.Minute), &validatingTarget{value: 1}},
		{"revalidate false", off, &validatingTarget{etag: `"v1"`, value: 1}},
		{"POST", posting, &validatingTarget{etag: `"v1"`, value: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newValidatingTarget(t, tc.v)
			server := modeServer(t, tc.collector)
			path := probePath(tc.collector.Name, target.URL, "")
			probeOnce(t, server, path, nil)
			outOfDate(server, true)
			if entries := server.cache.Stats(time.Now())[tc.collector.Name]; entries != 0 {
				t.Errorf("%d entries kept past their expiry, want none", entries)
			}
			if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 1\n") {
				t.Fatalf("answered %d: %s", r.Code, r.Body)
			}
			for i, h := range tc.v.asked() {
				if h.Get("If-None-Match") != "" || h.Get("If-Modified-Since") != "" {
					t.Errorf("request %d was conditional: %v", i+1, h)
				}
			}
		})
	}
}

// A paginated collector's merged pages are never revalidated, whatever
// validators its first page had.
func TestPaginatedResponsesAreNotRevalidated(t *testing.T) {
	testutil.CaptureLogs(t)
	var conditional atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"page"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"name":"repo1","stargazers_count":1}]`)
	}))
	t.Cleanup(target.Close)
	c := reposCollector(3)
	c.Cache.TTL = model.Duration(time.Minute)
	server := modeServer(t, c)
	path := probePath("repos", target.URL, "")
	probeOnce(t, server, path, nil)
	outOfDate(server, true)
	if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "github_repos 1\n") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	if n := conditional.Load(); n != 0 {
		t.Fatalf("%d conditional requests, want none", n)
	}
}

// An expired entry with a validator stays for revalidation with ttl alone,
// but answers nothing; a validator lets a trip whose request is answered
// 500 be answered with the stale result under stale_if_error, and the next
// trip's 304 makes the entry fresh again, no longer stale.
func TestRevalidationAndStaleIfErrorWorkTogether(t *testing.T) {
	testutil.CaptureLogs(t)
	v := &validatingTarget{etag: `"v1"`, value: 5}
	target := newValidatingTarget(t, v)
	c := revalidatingCollector("bridged", time.Minute)
	c.Cache.StaleIfError = model.Duration(time.Hour)
	server := modeServer(t, c)
	path := probePath("bridged", target.URL, "")
	probeOnce(t, server, path, nil)

	outOfDate(server, false)
	v.set(func(v *validatingTarget) { v.fail = true })
	stale := probeOnce(t, server, path, nil)
	if stale.Code != http.StatusOK || !strings.Contains(stale.Body.String(), "http_exporter_result_stale 1\n") || !strings.Contains(stale.Body.String(), "demo_value 5\n") {
		t.Fatalf("a failed revalidation was not answered stale: %d %s", stale.Code, stale.Body)
	}
	v.set(func(v *validatingTarget) { v.fail = false })
	fresh := probeOnce(t, server, path, nil)
	if fresh.Code != http.StatusOK || !strings.Contains(fresh.Body.String(), "http_exporter_result_stale 0\n") || !strings.Contains(fresh.Body.String(), "demo_value 5\n") {
		t.Fatalf("the 304 was not answered fresh: %d %s", fresh.Code, fresh.Body)
	}
	asked := v.asked()
	if len(asked) != 3 || asked[1].Get("If-None-Match") != `"v1"` || asked[2].Get("If-None-Match") != `"v1"` {
		t.Fatalf("If-None-Match sent: %q", ifNoneMatches(asked))
	}

	// Past both windows, the entry is still revalidated, and answers no
	// failure.
	outOfDate(server, true)
	if entries := server.cache.Stats(time.Now())["bridged"]; entries != 1 {
		t.Fatalf("%d entries, want the one kept for revalidation", entries)
	}
	v.set(func(v *validatingTarget) { v.fail = true })
	if r := probeOnce(t, server, path, nil); r.Code == http.StatusOK {
		t.Fatalf("an expired entry answered a failure: %s", r.Body)
	}
	v.set(func(v *validatingTarget) { v.fail = false })
	if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 5\n") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	if asked := v.asked(); asked[len(asked)-1].Get("If-None-Match") != `"v1"` {
		t.Fatalf("the expired entry was not revalidated: %q", ifNoneMatches(asked))
	}
}

// A 401 to a conditional request whose token the target no longer takes
// logs in again, as any 401 to a request that logged in does, and the
// request sent with the new token is as conditional as the first.
func TestARevalidationLogsInAgainOn401(t *testing.T) {
	fetch.CloseConnections()
	t.Cleanup(fetch.CloseConnections)
	testutil.CaptureLogs(t)
	var logins atomic.Int64
	var mu sync.Mutex
	var conditional []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			n := logins.Add(1)
			_, _ = fmt.Fprintf(w, `{"access_token": "token-%d", "expires_in": 3600}`, n)
			return
		}
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", logins.Load()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		conditional = append(conditional, r.Header.Get("If-None-Match"))
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, "value=3")
	}))
	t.Cleanup(target.Close)
	c := loginCollector(testutil.WriteFile(t, "password", "secret"))
	c.Cache.TTL = model.Duration(time.Minute)
	server := modeServer(t, c)
	path := probePath("login_api", target.URL+"/data", "")
	probeOnce(t, server, path, nil)
	outOfDate(server, true)
	// The target revokes the token: only a new login's is taken.
	logins.Add(1)
	if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 3\n") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(conditional) != 2 || conditional[0] != "" || conditional[1] != `"v1"` {
		t.Fatalf("If-None-Match of the requests the target took: %q", conditional)
	}
	if n := logins.Load(); n != 3 {
		t.Fatalf("%d logins counted at the target, want 2 and the revocation", n)
	}
}

// A debug probe sends the conditional request the same probe would, and its
// report shows the validators sent, the 304 and that the cached body was
// reused; it keeps nothing, so the entry is still out of date after it.
func TestADebugProbeShowsTheRevalidation(t *testing.T) {
	testutil.CaptureLogs(t)
	v := &validatingTarget{etag: `"v1"`, value: 7}
	target := newValidatingTarget(t, v)
	server := modeServer(t, revalidatingCollector("etag", time.Minute))
	server.SetDebug(DebugFull)
	probeOnce(t, server, probePath("etag", target.URL, ""), nil)
	outOfDate(server, true)
	report := debugProbeGet(t, server, "collector=etag&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, report,
		`If-None-Match: "v1"`,
		"-> 304 Not Modified; the cached body of 12 bytes was reused, with status 200",
		"304 Not Modified; the cached response was reused: status 200, 12 bytes",
		"demo_value 7",
	)
	if server.cache.Revalidation(soleKey(t, server), time.Now()) == nil {
		t.Fatal("the debug probe refreshed the entry")
	}
}

// soleKey is the key of the one entry the server's cache holds.
func soleKey(t *testing.T, server *Server) string {
	t.Helper()
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	if len(server.cache.entries) != 1 {
		t.Fatalf("%d cache entries, want 1", len(server.cache.entries))
	}
	for key := range server.cache.entries {
		return key
	}
	return ""
}

// A 304 to a request that was not conditional on a cached response has no
// response to stand for, and fails the probe saying so.
func TestAnUnaskedFor304FailsTheProbe(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, revalidatingCollector("etag", time.Minute))
	r := probeOnce(t, server, probePath("etag", target.URL, ""), nil)
	if r.Code == http.StatusOK || !strings.Contains(r.Body.String(), "received HTTP status 304 Not Modified to a request that was not conditional on a cached response") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
}
