//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// steppedTarget answers a request only when release lets it: one request for
// each value sent, every request once it is closed. It says on entered that a
// request has arrived, and on cancelled that one was cancelled before it was
// answered, so a test moves on when the trip is where it wants it rather than
// after a while. A request with unheld in its query is answered at once.
type steppedTarget struct {
	*httptest.Server
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func newSteppedTarget(t *testing.T, status int, body string) *steppedTarget {
	t.Helper()
	h := &steppedTarget{entered: make(chan struct{}, 8), cancelled: make(chan struct{}, 8), release: make(chan struct{})}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.Query().Has("unheld") {
			h.entered <- struct{}{}
			select {
			case <-h.release:
			case <-r.Context().Done():
				h.cancelled <- struct{}{}
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(h.Close)
	return h
}

// sharedTripServer is a verbose exporter with one collector, trips, whose
// cache is on, so a trip to the target is seen as a cache miss.
func sharedTripServer(t *testing.T) *Server {
	t.Helper()
	c := testutil.Collector("trips", "text")
	c.Cache.TTL = model.Duration(time.Minute)
	server := verboseServer(t, true, c)
	server.logger = testutil.QuietLogger(t)
	return server
}

// tripInFlight is the one trip in progress, whose done channel says when it
// has ended and counted all it counts.
func tripInFlight(t *testing.T, server *Server) *probeFlight {
	t.Helper()
	server.flights.mu.Lock()
	defer server.flights.mu.Unlock()
	if len(server.flights.flights) != 1 {
		t.Fatalf("%d trips are in flight, want 1", len(server.flights.flights))
	}
	for _, flight := range server.flights.flights {
		return flight
	}
	return nil
}

// tripCounters are the counter families a trip to the target raises, and
// tripGauges the latest values it leaves; probeCounters are what each probe
// counts for itself.
var (
	tripCounters = []string{
		"http_exporter_decode_success_total",
		"http_exporter_parse_errors_total",
		"http_exporter_transform_errors_total",
		"http_exporter_missing_keys_total",
		"http_exporter_metrics_emitted_total",
		"http_exporter_series_limit_exceeded_total",
		"http_exporter_cache_hits_total",
		"http_exporter_cache_misses_total",
		"http_exporter_cache_stale_served_total",
		"http_exporter_probes_rejected_total",
		"http_exporter_targets_refused_total",
	}
	tripGauges = []string{
		"http_exporter_scrape_http_status_code",
		"http_exporter_scrape_response_bytes",
	}
	probeCounters = []string{
		"http_exporter_scrapes_total",
		"http_exporter_scrape_success_total",
		"http_exporter_probes_coalesced_total",
	}
)

// sharedTripViews reads the collector's series and the request's of every
// family a trip or a probe counts in.
func sharedTripViews(t *testing.T, server *Server, target string) (collector, request map[string]float64) {
	t.Helper()
	exposition := selfMetrics(t, server)
	collector, request = map[string]float64{}, map[string]float64{}
	labels := fmt.Sprintf(`{collector="trips",http_method="GET",url=%q}`, target)
	for _, names := range [][]string{tripCounters, tripGauges, probeCounters} {
		for _, name := range names {
			collector[name] = metricValue(t, exposition, name+`{collector="trips"}`)
			request[name] = metricValue(t, exposition, name+labels)
		}
	}
	request["last_scrape"] = metricValue(t, exposition, "http_exporter_request_last_scrape_timestamp_seconds"+labels)
	return collector, request
}

// requireTripOnRequest fails unless the request's series say everything the
// collector's say about the trips to the target.
func requireTripOnRequest(t *testing.T, collector, request map[string]float64) {
	t.Helper()
	for _, names := range [][]string{tripCounters, tripGauges} {
		for _, name := range names {
			if collector[name] != request[name] {
				t.Errorf("%s: the collector has %v and the request %v; a trip that answered a probe is counted on both", name, collector[name], request[name])
			}
		}
	}
	if request["last_scrape"] == 0 {
		t.Error("the request has no last scrape time, though a trip to its target answered a probe")
	}
}

// requireSeries fails unless view has each of the values wanted.
func requireSeries(t *testing.T, view string, got, want map[string]float64) {
	t.Helper()
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s: %s is %v, want %v", view, name, got[name], value)
		}
	}
}

// The probe that started a shared trip leaves, and the trip carries on and
// answers the two probes that joined it. What the trip counted — the cache
// miss, the target's status and bytes, the decode, the series emitted, the
// failed transform, the time of the scrape — is on the request those probes
// start tracking, once, and not lost with the probe that left: the request's
// series say what the collector's say, but for the probe that was not
// answered, which the collector alone counts.
func TestATripItsStarterLeftIsCountedOnTheRequestOfTheProbesItAnswered(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		answered  int
		successes float64
		trip      map[string]float64
	}{
		{
			name: "a trip that succeeds", status: http.StatusOK, body: "value=42\n", answered: http.StatusOK, successes: 2,
			trip: map[string]float64{
				"http_exporter_cache_misses_total": 1, "http_exporter_decode_success_total": 1, "http_exporter_metrics_emitted_total": 1,
				"http_exporter_scrape_http_status_code": http.StatusOK, "http_exporter_scrape_response_bytes": 9,
			},
		},
		{
			name: "a trip the target fails", status: http.StatusServiceUnavailable, body: "unavailable\n", answered: http.StatusBadGateway,
			trip: map[string]float64{
				"http_exporter_cache_misses_total": 1, "http_exporter_decode_success_total": 0, "http_exporter_metrics_emitted_total": 0,
				"http_exporter_scrape_http_status_code": http.StatusServiceUnavailable, "http_exporter_scrape_response_bytes": 12,
			},
		},
		{
			// More than the collector's limits.max_response_bytes of 1 KiB.
			name: "a trip whose answer is too large", status: http.StatusOK, body: strings.Repeat("value=42\n", 200), answered: http.StatusBadGateway,
			trip: map[string]float64{
				"http_exporter_cache_misses_total": 1, "http_exporter_series_limit_exceeded_total": 1, "http_exporter_decode_success_total": 0,
				"http_exporter_metrics_emitted_total": 0, "http_exporter_scrape_http_status_code": 0,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newSteppedTarget(t, tc.status, tc.body)
			server := sharedTripServer(t)
			path := probePath("trips", target.URL, "")

			starterCtx, leave := context.WithCancel(context.Background())
			defer leave()
			starter := probeAsync(starterCtx, server, path, nil)
			<-target.entered
			joined := []<-chan probeOutcome{
				probeAsync(context.Background(), server, path, nil),
				probeAsync(context.Background(), server, path, nil),
			}
			waitForWaiters(t, server, 3)
			leave()
			<-starter
			close(target.release)
			for _, outcome := range joined {
				if got := <-outcome; got.code != tc.answered {
					t.Fatalf("a probe that joined the trip was answered %d %q, want %d", got.code, got.body, tc.answered)
				}
			}

			collector, request := sharedTripViews(t, server, target.URL)
			requireSeries(t, "the collector", collector, tc.trip)
			requireSeries(t, "the request", request, tc.trip)
			requireTripOnRequest(t, collector, request)
			// Three probes came and two were answered, by one trip.
			requireSeries(t, "the collector", collector, map[string]float64{
				"http_exporter_scrapes_total": 3, "http_exporter_scrape_success_total": tc.successes, "http_exporter_probes_coalesced_total": 2,
			})
			requireSeries(t, "the request", request, map[string]float64{
				"http_exporter_scrapes_total": 2, "http_exporter_scrape_success_total": tc.successes, "http_exporter_probes_coalesced_total": 2,
			})
		})
	}
}

// The mirrored case: a probe that joined a shared trip leaves, and the probe
// that started it stays and is answered. The trip is on the request the
// starter starts tracking, and the probe that left is counted on the
// collector alone, as a probe whose caller went away is.
func TestAProbeThatLeavesASharedTripIsCountedOnItsCollectorOnly(t *testing.T) {
	target := newSteppedTarget(t, http.StatusOK, "value=42\n")
	server := sharedTripServer(t)
	path := probePath("trips", target.URL, "")

	starter := probeAsync(context.Background(), server, path, nil)
	<-target.entered
	joinedCtx, leave := context.WithCancel(context.Background())
	defer leave()
	joined := probeAsync(joinedCtx, server, path, nil)
	waitForWaiters(t, server, 2)
	leave()
	<-joined
	close(target.release)
	if got := <-starter; got.code != http.StatusOK {
		t.Fatalf("the probe that started the trip was answered %d %q", got.code, got.body)
	}

	collector, request := sharedTripViews(t, server, target.URL)
	requireTripOnRequest(t, collector, request)
	// The probe that left was answered by nobody's trip: it is not coalesced.
	requireSeries(t, "the collector", collector, map[string]float64{
		"http_exporter_scrapes_total": 2, "http_exporter_scrape_success_total": 1, "http_exporter_probes_coalesced_total": 0,
		"http_exporter_cache_misses_total": 1, "http_exporter_decode_success_total": 1, "http_exporter_metrics_emitted_total": 1,
	})
	requireSeries(t, "the request", request, map[string]float64{
		"http_exporter_scrapes_total": 1, "http_exporter_scrape_success_total": 1, "http_exporter_probes_coalesced_total": 0,
		"http_exporter_cache_misses_total": 1, "http_exporter_decode_success_total": 1, "http_exporter_metrics_emitted_total": 1,
	})
}

// When every probe sharing a trip leaves, the trip is cancelled at the target
// rather than carried on to fill the cache. It answered nobody, so it starts
// no tracked request: the two probes and the cache miss are the collector's
// alone. The next probe of the request makes a trip of its own and is
// tracked, with its own counts and none of the cancelled trip's.
func TestATripEveryProbeLeftIsCancelledAndStartsNoTrackedRequest(t *testing.T) {
	target := newSteppedTarget(t, http.StatusOK, "value=42\n")
	server := sharedTripServer(t)
	path := probePath("trips", target.URL, "")

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	starter := probeAsync(ctx, server, path, nil)
	<-target.entered
	joined := probeAsync(ctx, server, path, nil)
	waitForWaiters(t, server, 2)
	trip := tripInFlight(t, server)
	leave()
	<-starter
	<-joined
	<-target.cancelled
	<-trip.done

	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_scrapes_total{collector="trips"}`:          2,
		`http_exporter_scrape_success_total{collector="trips"}`:   0,
		`http_exporter_probes_coalesced_total{collector="trips"}`: 0,
		`http_exporter_cache_misses_total{collector="trips"}`:     1,
		`http_exporter_decode_success_total{collector="trips"}`:   0,
		`http_exporter_metrics_emitted_total{collector="trips"}`:  0,
		`http_exporter_cache_entries{collector="trips"}`:          0,
		"http_exporter_request_series_tracked":                    0,
	} {
		if got := metricValue(t, exposition, series); got != want {
			t.Errorf("after a trip every probe left, %s is %v, want %v", series, got, want)
		}
	}

	close(target.release)
	if got := probeOnce(t, server, path, nil); got.Code != http.StatusOK {
		t.Fatalf("the probe after the cancelled trip was answered %d %q", got.Code, got.Body.String())
	}
	// The cancelled trip left nothing in the cache: this probe went to the
	// target itself.
	select {
	case <-target.entered:
	default:
		t.Fatal("the probe after the cancelled trip made no request to the target")
	}
	collector, request := sharedTripViews(t, server, target.URL)
	requireSeries(t, "the collector", collector, map[string]float64{
		"http_exporter_scrapes_total": 3, "http_exporter_scrape_success_total": 1, "http_exporter_cache_misses_total": 2,
		"http_exporter_decode_success_total": 1, "http_exporter_metrics_emitted_total": 1,
	})
	requireSeries(t, "the request", request, map[string]float64{
		"http_exporter_scrapes_total": 1, "http_exporter_scrape_success_total": 1, "http_exporter_cache_misses_total": 1,
		"http_exporter_decode_success_total": 1, "http_exporter_metrics_emitted_total": 1,
	})
}

// A request already tracked is updated as its probes go: the probe that
// started a shared trip and left is counted on it like the one the trip
// answered, and so is the trip. The collector's series and the request's are
// then the same, probes included.
func TestAProbeLeavingATripOfATrackedRequestIsCountedOnIt(t *testing.T) {
	target := newSteppedTarget(t, http.StatusOK, "value=42\n")
	server := sharedTripServer(t)
	path := probePath("trips", target.URL, "")

	first := probeAsync(context.Background(), server, path, nil)
	<-target.entered
	target.release <- struct{}{}
	if got := <-first; got.code != http.StatusOK {
		t.Fatalf("the first probe was answered %d %q", got.code, got.body)
	}
	// The next probe goes to the target again rather than to the cache.
	expireCachedEntries(server)

	starterCtx, leave := context.WithCancel(context.Background())
	defer leave()
	starter := probeAsync(starterCtx, server, path, nil)
	<-target.entered
	joined := probeAsync(context.Background(), server, path, nil)
	waitForWaiters(t, server, 2)
	leave()
	<-starter
	close(target.release)
	if got := <-joined; got.code != http.StatusOK {
		t.Fatalf("the probe that joined the trip was answered %d %q", got.code, got.body)
	}

	collector, request := sharedTripViews(t, server, target.URL)
	requireTripOnRequest(t, collector, request)
	want := map[string]float64{
		"http_exporter_scrapes_total": 3, "http_exporter_scrape_success_total": 2, "http_exporter_probes_coalesced_total": 1,
		"http_exporter_cache_misses_total": 2, "http_exporter_decode_success_total": 2, "http_exporter_metrics_emitted_total": 2,
	}
	requireSeries(t, "the collector", collector, want)
	requireSeries(t, "the request", request, want)
}

// While a shared trip to a request not tracked yet is under way, another
// probe of the same request, differing only in the target's query, which the
// url label leaves out, starts tracking it. The trip's counts join that
// request's when the trip ends, though the probe that started it left: the
// request has both trips, and the duration of the last probe answered.
func TestATripJoinsTheRequestAnotherProbeStartedTrackingMeanwhile(t *testing.T) {
	target := newSteppedTarget(t, http.StatusOK, "value=42\n")
	server := sharedTripServer(t)
	path := probePath("trips", target.URL, "")

	starterCtx, leave := context.WithCancel(context.Background())
	defer leave()
	starter := probeAsync(starterCtx, server, path, nil)
	<-target.entered
	if got := probeOnce(t, server, probePath("trips", target.URL+"?unheld=1", ""), nil); got.Code != http.StatusOK {
		t.Fatalf("the probe with a query of its own was answered %d %q", got.Code, got.Body.String())
	}
	joined := probeAsync(context.Background(), server, path, nil)
	waitForWaiters(t, server, 2)
	leave()
	<-starter
	close(target.release)
	if got := <-joined; got.code != http.StatusOK {
		t.Fatalf("the probe that joined the trip was answered %d %q", got.code, got.body)
	}

	collector, request := sharedTripViews(t, server, target.URL)
	requireTripOnRequest(t, collector, request)
	requireSeries(t, "the collector", collector, map[string]float64{
		"http_exporter_scrapes_total": 3, "http_exporter_scrape_success_total": 2, "http_exporter_probes_coalesced_total": 1,
		"http_exporter_cache_misses_total": 2, "http_exporter_decode_success_total": 2, "http_exporter_metrics_emitted_total": 2,
	})
	requireSeries(t, "the request", request, map[string]float64{
		"http_exporter_scrapes_total": 2, "http_exporter_scrape_success_total": 2, "http_exporter_probes_coalesced_total": 1,
		"http_exporter_cache_misses_total": 2, "http_exporter_decode_success_total": 2, "http_exporter_metrics_emitted_total": 2,
	})
	labels := fmt.Sprintf(`{collector="trips",http_method="GET",url=%q}`, target.URL)
	if got := metricValue(t, selfMetrics(t, server), "http_exporter_scrape_duration_seconds"+labels); got <= 0 {
		t.Errorf("the request's last probe took %v seconds, want the duration of the probe the trip answered", got)
	}
}
