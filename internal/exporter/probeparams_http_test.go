//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A parameter the probe reads one value of is refused when it is given twice,
// with a 400 naming it, before the target is contacted or anything is
// counted: the probe would use one value and key the entry by both.
// header_<name> may repeat, every value being forwarded, and so may a
// parameter the probe does not read.
func TestARepeatedProbeParameterIsRefused(t *testing.T) {
	testutil.CaptureLogs(t)
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(1)
	c := staleCollector(time.Minute, time.Hour)
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	c.Request.Path = "/{{param_region:eu}}"
	server, _ := newCacheTestServer(t, c)
	probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)
	for name, repeat := range map[string]string{
		"target":               "&target=x",
		"collector":            "&collector=x",
		"method":               "&method=GET&method=POST",
		"path":                 "&path=/a&path=/b",
		"timeout":              "&timeout=5s&timeout=x",
		"body":                 "&body=a&body=a",
		"insecure_skip_verify": "&insecure_skip_verify=true&insecure_skip_verify=true",
		"follow_redirects":     "&follow_redirects=true&follow_redirects=false",
		"enable_http2":         "&enable_http2=true&enable_http2=",
		"retry_attempts":       "&retry_attempts=1&retry_attempts=2",
		"retry_backoff":        "&retry_backoff=1s&retry_backoff=2s",
		"param_region":         "&param_region=us&param_region=eu",
	} {
		r := probeOnce(t, server, probe+repeat, nil)
		if want := "probe parameter " + name + " is given 2 times; give it once"; r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), want) {
			t.Errorf("%s: answered %d %q, want 400 saying %q", repeat, r.Code, r.Body, want)
		}
	}
	if calls := flaky.calls.Load(); calls != 0 {
		t.Fatalf("refused probes went to the target %d times", calls)
	}
	if counted := metricValue(t, selfMetrics(t, server), `http_exporter_scrapes_total{collector="flaky"}`); counted != 0 {
		t.Fatalf("%v refused probes were counted", counted)
	}
	for _, allowed := range []string{"&header_X-Tenant=a&header_X-Tenant=b", "&x=1&x=2"} {
		if r := probeOnce(t, server, probe+allowed, nil); r.Code != http.StatusOK {
			t.Errorf("%s: answered %d %q, want 200", allowed, r.Code, r.Body)
		}
	}
}

// One request written several ways is one cache entry: the key is made of the
// parameters as the probe read them, so the case of a method, the unit of a
// duration, the case of a boolean and the padding of a number make no
// difference, and none of them takes the probe to the target past cache.ttl.
// A different value is still a probe of its own.
func TestSpellingsOfOneRequestShareACacheEntry(t *testing.T) {
	testutil.CaptureLogs(t)
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(1)
	server, _ := newCacheTestServer(t, staleCollector(time.Minute, time.Hour))
	probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)
	for _, spelling := range []string{
		"&method=GET&timeout=50s&retry_backoff=1s&retry_attempts=2&insecure_skip_verify=true&follow_redirects=false",
		"&method=get&timeout=50s&retry_backoff=1s&retry_attempts=2&insecure_skip_verify=true&follow_redirects=false",
		"&method=%20Get%20&timeout=50000ms&retry_backoff=1000ms&retry_attempts=2&insecure_skip_verify=true&follow_redirects=false",
		"&method=GET&timeout=50.0s&retry_backoff=0m1s&retry_attempts=02&insecure_skip_verify=TRUE&follow_redirects=False",
		"&method=GET&timeout=%2050s&retry_backoff=1s&retry_attempts=%2B2&insecure_skip_verify=%20true&follow_redirects=false&x=1",
	} {
		if r := probeOnce(t, server, probe+spelling, nil); r.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", spelling, r.Code, r.Body)
		}
	}
	if calls := flaky.calls.Load(); calls != 1 {
		t.Errorf("five spellings of one request went to the target %d times, want once", calls)
	}
	if n := server.cache.Stats(time.Now())["flaky"]; n != 1 {
		t.Errorf("five spellings of one request hold %d cache entries, want 1", n)
	}
	// An empty method or timeout is one not given.
	bare := []string{"", "&method=", "&timeout=", "&method=%20&timeout=%20"}
	for _, spelling := range bare {
		if r := probeOnce(t, server, probe+spelling, nil); r.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", spelling, r.Code, r.Body)
		}
	}
	if calls := flaky.calls.Load(); calls != 2 {
		t.Errorf("the probe without parameters, written %d ways, went to the target %d times, want once", len(bare), calls-1)
	}
	for _, other := range []string{"&method=POST", "&timeout=60s", "&retry_attempts=3", "&follow_redirects=true"} {
		before := flaky.calls.Load()
		if r := probeOnce(t, server, probe+other, nil); r.Code != http.StatusOK || flaky.calls.Load() != before+1 {
			t.Errorf("%s answered %d after %d trips, want a trip of its own", other, r.Code, flaky.calls.Load()-before)
		}
	}
}

// Identical probes in flight share a trip by the same key, so two spellings
// of one request arriving together make one trip.
func TestSpellingsOfOneRequestShareATrip(t *testing.T) {
	testutil.CaptureLogs(t)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	// Released before the target is closed, also when the test fails.
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	server, _ := newCacheTestServer(t, testutil.Collector("text", "text"))
	probe := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	answers := make(chan int, 2)
	// The timeout is one neither probe is held to: the target answers when
	// the second has joined the first, however long that takes.
	go func() { answers <- probeOnce(t, server, probe+"&method=get&timeout=50s", nil).Code }()
	<-entered
	go func() { answers <- probeOnce(t, server, probe+"&method=GET&timeout=50000ms", nil).Code }()
	testutil.WaitFor(t, "the second probe to join the first", func() bool {
		server.flights.mu.Lock()
		defer server.flights.mu.Unlock()
		for _, flight := range server.flights.flights {
			return flight.waiters == 2
		}
		return false
	})
	letGo()
	for range 2 {
		if code := <-answers; code != http.StatusOK {
			t.Fatalf("a probe answered %d", code)
		}
	}
	if got := metricValue(t, selfMetrics(t, server), `http_exporter_probes_coalesced_total{collector="text"}`); got != 1 || len(entered) != 0 {
		t.Fatalf("coalesced probes %v, further requests to the target %d; want 1 and 0", got, len(entered))
	}
}

// The value of target, collector and every parameter a probe reads may be
// MaxProbeParameterBytes long, and a longer one is refused with a 400 naming
// the parameter and the limit, before anything remembers the probe: the
// failure log and the verbose self-metrics keep what a probe names, and a
// failing probe with a megabyte of target would leave a megabyte behind.
func TestAnOverLongProbeParameterIsRefused(t *testing.T) {
	testutil.CaptureLogs(t)
	c := testutil.Collector("text", "text")
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	c.Request.Path = "/{{param_region:eu}}"
	server := verboseServer(t, true, c)
	long := strings.Repeat("a", MaxProbeParameterBytes+1)
	// A port nothing listens on: a probe that is let through fails at once.
	target := "http://127.0.0.1:1/"
	for name, query := range map[string]string{
		"target":          "collector=text&target=" + target + long,
		"collector":       "target=" + target + "&collector=" + long,
		"path":            "collector=text&target=" + target + "&path=/" + long,
		"body":            "collector=text&target=" + target + "&method=POST&body=" + long,
		"param_region":    "collector=text&target=" + target + "&param_region=" + long,
		"header_X-Tenant": "collector=text&target=" + target + "&header_X-Tenant=ok&header_X-Tenant=" + long,
		"HEADER_X-Tenant": "collector=text&target=" + target + "&HEADER_X-Tenant=" + long,
	} {
		r := probeOnce(t, server, "/probe?"+query, nil)
		want := fmt.Sprintf("probe parameter %s is %d bytes long; a probe parameter's value may be at most %d bytes", name, len(query)-strings.LastIndex(query, "=")-1, MaxProbeParameterBytes)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), want) {
			t.Errorf("%s: answered %d %.200q, want 400 saying %q", name, r.Code, r.Body, want)
		}
		if r.Body.Len() > 1024 {
			t.Errorf("%s: the refusal is %d bytes long; it repeats the value", name, r.Body.Len())
		}
	}
	server.failures.mu.Lock()
	remembered := len(server.failures.entries)
	server.failures.mu.Unlock()
	if tracked, _ := server.requests.Snapshot(); remembered != 0 || len(tracked) != 0 {
		t.Fatalf("refused probes left %d failure log entries and %d tracked requests", remembered, len(tracked))
	}
	// A value of exactly the limit is let through, and so is a longer one of
	// a parameter the probe does not read.
	atLimit := target + strings.Repeat("a", MaxProbeParameterBytes-len(target))
	for _, query := range []string{"collector=text&target=" + atLimit, "collector=text&target=" + target + "&x=" + long} {
		if r := probeOnce(t, server, "/probe?"+query, nil); r.Code != http.StatusBadGateway {
			t.Errorf("a probe within the limit answered %d %.200q, want the 502 of its failed trip", r.Code, r.Body)
		}
	}
	// What a failing probe leaves behind is bounded by what it may name.
	server.failures.mu.Lock()
	defer server.failures.mu.Unlock()
	for key := range server.failures.entries {
		if len(key) > 2*MaxProbeParameterBytes {
			t.Errorf("a failure log key is %d bytes long", len(key))
		}
	}
}

// A forwarded header_<name> value Go would refuse to send, one with a CR,
// an LF or another control character but tab, is the caller's mistake, as a
// malformed param_<name> is: it is answered 400 naming the parameter before
// the target is contacted, rather than failing the trip there, being retried
// and logged as the target's failure. A tab or a non-ASCII letter is sent as
// it is, and a header_<name> for a header the collector does not forward is
// still ignored, whatever it holds.
func TestAForwardedHeaderValueWithAControlCharacterIsRefused(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	var hits atomic.Int32
	var got atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		got.Store(r.Header.Get("X-Tenant"))
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("text", "text")
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	c.Request.Retry.Attempts = 3
	server := verboseServer(t, true, c)
	probe := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	for key, value := range map[string]string{
		"header_X-Tenant": "a\r\nX-Injected: 1",
		"header_x-tenant": "a\nb",
		"HEADER_X-Tenant": "a\x01b",
		"header_X-TENANT": "a\x7fb",
	} {
		r := probeOnce(t, server, probe+"&"+key+"="+url.QueryEscape(value), nil)
		if want := "probe parameter " + key + " has the control character"; r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), want) {
			t.Errorf("%s=%q: answered %d %q, want 400 saying %q", key, value, r.Code, r.Body, want)
		}
	}
	// Repeated, the bad value is refused wherever it stands.
	if r := probeOnce(t, server, probe+"&header_X-Tenant=ok&header_X-Tenant="+url.QueryEscape("a\rb"), nil); r.Code != http.StatusBadRequest {
		t.Errorf("a bad second value answered %d %q, want 400", r.Code, r.Body)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("refused probes reached the target %d times", n)
	}
	if counted := metricValue(t, selfMetrics(t, server), `http_exporter_scrapes_total{collector="text"}`); counted != 0 {
		t.Fatalf("%v refused probes were counted", counted)
	}
	if strings.Contains(logs.String(), "probe failed") {
		t.Fatalf("a refused probe was logged as the target's failure:\n%s", logs)
	}
	for _, value := range []string{"a\tb", "Zürich"} {
		if r := probeOnce(t, server, probe+"&header_X-Tenant="+url.QueryEscape(value), nil); r.Code != http.StatusOK || got.Load() != value {
			t.Errorf("%q: answered %d %q and forwarded %q, want 200 and the value", value, r.Code, r.Body, got.Load())
		}
	}
	if r := probeOnce(t, server, probe+"&header_X-Other="+url.QueryEscape("a\r\nb"), nil); r.Code != http.StatusOK {
		t.Errorf("a header not forwarded answered %d %q, want 200", r.Code, r.Body)
	}
}

// The exporter's HTTP server takes 64 KiB of request line and headers, where
// Go's default is 1 MiB, and answers a longer request 431 without reading the
// rest.
func TestTheHTTPServerBoundsARequestsLineAndHeaders(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := NewHTTPServer("", server.Handler())
	if httpServer.MaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes is %d, want 64 KiB", httpServer.MaxHeaderBytes)
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	get := func(pad int) (int, string) {
		t.Helper()
		resp, err := http.Get("http://" + listener.Addr().String() + "/probe?collector=text&x=" + strings.Repeat("a", pad))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get(200 << 10); code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("a request of 200 KiB answered %d %.200q, want 431", code, body)
	}
	// Within the limit the request reaches the probe, which answers for
	// itself.
	if code, body := get(32 << 10); code != http.StatusBadRequest || !strings.Contains(body, "the target parameter is required") {
		t.Fatalf("a request of 32 KiB answered %d %.200q, want the probe's own 400", code, body)
	}
}
