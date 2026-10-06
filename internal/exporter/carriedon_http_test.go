//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A probe that carries on past a failed stage, under error_handling log or
// ignore, is answered as a probe whose rules produced no series is: by the
// same writer, in the format asked for (probeTrip, carriedOnAnswer).

// openMetrics001Accept asks for the older OpenMetrics version only.
const openMetrics001Accept = "application/openmetrics-text;version=0.0.1"

// scrapedAnswer is an answer as a scraper is sent it over a connection: the
// status, every header but Date, and the body as sent, compressed when the
// scrape asked for gzip.
type scrapedAnswer struct {
	status int
	header http.Header
	body   string
}

// plain is the body uncompressed.
func (a scrapedAnswer) plain(t *testing.T) string {
	t.Helper()
	if a.header.Get("Content-Encoding") != "gzip" {
		return a.body
	}
	reader, err := gzip.NewReader(strings.NewReader(a.body))
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the body is not gzip to its end: %v", err)
	}
	return string(raw)
}

// scrapeAs asks the exporter served at base for path as a scraper would: with
// accept as its Accept header when not empty, and asking for gzip when
// gzipped. The client adds no Accept-Encoding of its own and decompresses
// nothing, so the headers and the body are the ones sent.
func scrapeAs(t *testing.T, base, method, path, accept string, gzipped bool) scrapedAnswer {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	if gzipped {
		request.Header.Set("Accept-Encoding", "gzip")
	}
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Header.Del("Date")
	return scrapedAnswer{status: response.StatusCode, header: response.Header, body: string(body)}
}

// expositionFormats are the formats a scrape can ask for, with the
// Content-Type of each and what an exposition without series is in it.
var expositionFormats = []struct {
	name, accept, contentType, empty string
}{
	{"text", "", expositionContentType, ""},
	{"openmetrics 1.0.0", prometheus3Accept, openMetricsType1, "# EOF\n"},
	{"openmetrics 0.0.1", openMetrics001Accept, "application/openmetrics-text; version=0.0.1; charset=utf-8", "# EOF\n"},
}

// carryOnCase is a stage a probe can carry on past, made to fail: target is
// where the collector set up by setup fails in stage, and counter, when not
// empty, the self-metric that counts the failure.
type carryOnCase struct {
	name, stage, counter string
	target               func(t *testing.T) string
	setup                func(c *model.Collector, policy string)
}

// carryOnCases are the stages of errorStageCases, and a target nothing
// listens at, which fails the fetch before there is a status.
func carryOnCases() []carryOnCase {
	counters := map[string]string{"decode": "http_exporter_parse_errors_total", "transform": "http_exporter_transform_errors_total"}
	cases := []carryOnCase{{
		name: "unreachable", stage: "http",
		target: func(t *testing.T) string {
			gone := httptest.NewServer(http.NotFoundHandler())
			gone.Close()
			return gone.URL
		},
		setup: func(c *model.Collector, policy string) { c.ErrorHandling.OnFetchError = policy },
	}}
	for _, tc := range errorStageCases {
		cases = append(cases, carryOnCase{
			name: tc.name, stage: tc.stage, counter: counters[tc.name],
			target: func(t *testing.T) string { return errorStageTarget(t, tc).URL },
			setup:  tc.setup,
		})
	}
	return cases
}

// noSeriesCollector is a collector whose rule finds nothing in "nothing\n"
// and carries on without its series: its probe goes through whole and
// produces no series.
func noSeriesCollector(name string) model.Collector {
	c := testutil.Collector(name, "text")
	c.Metrics[0].ErrorMode = model.ErrorModeIgnore
	return c
}

// A probe that carries on past a failed stage is answered 200 with an empty
// exposition in the format asked for, in every header and byte what a probe
// whose rules produced no series is answered: the text format's Content-Type
// or the OpenMetrics one of the version asked for and its # EOF, nosniff,
// Vary on Accept-Encoding and Accept, compressed for a scrape that accepts
// gzip, and to HEAD the same headers without the body. That holds for a
// fetch that fails before or with a status, a decode and a transform, under
// log and ignore. The failure is still counted and, under log, logged as the
// stage failing, and the probe counts as a success.
func TestAProbeThatCarriesOnIsAnsweredWithAnEmptyExposition(t *testing.T) {
	for _, tc := range carryOnCases() {
		for _, policy := range []string{model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
			t.Run(tc.name+"/"+policy, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				carried := testutil.Collector("carried", "text")
				tc.setup(&carried, policy)
				server := flightServer(t, carried, noSeriesCollector("none"))
				exporter := httptest.NewServer(server.Handler())
				t.Cleanup(exporter.Close)
				failing, working := probePath("carried", tc.target(t), ""), probePath("none", textTarget(t, "nothing\n").URL, "")

				probes := 0
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					for _, format := range expositionFormats {
						for _, gzipped := range []bool{false, true} {
							got := scrapeAs(t, exporter.URL, method, failing, format.accept, gzipped)
							probes++
							want := scrapedAnswer{status: http.StatusOK, header: http.Header{
								"Content-Type":           {format.contentType},
								"X-Content-Type-Options": {"nosniff"},
								"Vary":                   {"Accept-Encoding", "Accept"},
							}}
							switch {
							case method == http.MethodHead:
								// HEAD is answered GET's headers, uncompressed,
								// and net/http gives the length of what was
								// written when anything was.
								if format.empty != "" {
									want.header.Set("Content-Length", strconv.Itoa(len(format.empty)))
								}
							case gzipped:
								want.header.Set("Content-Encoding", "gzip")
								want.header.Set("Content-Length", strconv.Itoa(len(got.body)))
								want.body = got.body
								if plain := got.plain(t); plain != format.empty {
									t.Errorf("%s as %s, gzip: the body is %q uncompressed, want %q", method, format.name, plain, format.empty)
								}
							default:
								want.header.Set("Content-Length", strconv.Itoa(len(format.empty)))
								want.body = format.empty
							}
							if !reflect.DeepEqual(got, want) {
								t.Errorf("%s as %s, gzip=%v: answered\n%+v\nwant\n%+v", method, format.name, gzipped, got, want)
							}
							if twin := scrapeAs(t, exporter.URL, method, working, format.accept, gzipped); !reflect.DeepEqual(got, twin) {
								t.Errorf("%s as %s, gzip=%v: answered\n%+v\nbut a probe whose rules produced no series is answered\n%+v", method, format.name, gzipped, got, twin)
							}
						}
					}
				}

				stats := selfMetrics(t, server)
				counted := map[string]float64{
					`http_exporter_scrapes_total{collector="carried"}`:            float64(probes),
					`http_exporter_scrape_success_total{collector="carried"}`:     float64(probes),
					`http_exporter_metrics_emitted_total{collector="carried"}`:    0,
					`http_exporter_cache_stale_served_total{collector="carried"}`: 0,
					`http_exporter_cache_entries{collector="carried"}`:            0,
				}
				if tc.counter != "" {
					counted[tc.counter+`{collector="carried"}`] = float64(probes)
				}
				for series, want := range counted {
					if got := seriesValue(t, stats, series); got != want {
						t.Errorf("%s is %v, want %v", series, got, want)
					}
				}
				output := logs.String()
				if strings.Contains(output, `"msg":"probe failed"`) || strings.Contains(output, `"msg":"probe recovered"`) {
					t.Errorf("a probe that carried on was logged as failed or as recovered:\n%s", output)
				}
				continuing := strings.Count(output, `"msg":"probe stage failed; continuing"`)
				switch policy {
				case model.ErrorPolicyLog:
					if continuing == 0 || !strings.Contains(output, `"level":"WARN","msg":"probe stage failed; continuing"`) || !strings.Contains(output, `"stage":"`+tc.stage+`"`) {
						t.Errorf("under log the %s stage's failure is not logged at warning level:\n%s", tc.stage, output)
					}
				case model.ErrorPolicyIgnore:
					if continuing != 0 {
						t.Errorf("under ignore the failure was logged above debug level:\n%s", output)
					}
				}
			})
		}
	}
}

// What the CSV fixtures turned up: a csv collector with on_decode_error: log
// whose target names a column twice, which fails the decode, was answered
// 200 with no Content-Type at all. It is answered the text format's.
func TestACSVProbeThatCarriesOnPastAFailedDecodeHasAContentType(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = io.WriteString(w, "host,cpu,cpu\nweb01,1,2\n")
	}))
	t.Cleanup(target.Close)
	exporter := httptest.NewServer(csvFixtureServer(t, `
collectors:
  - name: carried_on
    request:
      type: http
    error_handling:
      on_decode_error: log
    transform:
      type: csv
    metrics:
      - name: host_cpu
        expression: cpu
`).Handler())
	t.Cleanup(exporter.Close)
	response, err := http.Get(exporter.URL + probePath("carried_on", target.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != expositionContentType || len(body) != 0 {
		t.Errorf("status=%d as %q body=%q, want 200 as %s and no series", response.StatusCode, response.Header.Get("Content-Type"), body, expositionContentType)
	}
	if !strings.Contains(logs.String(), `CSV header names column \"cpu\" twice`) {
		t.Errorf("the failed decode is not logged:\n%s", logs)
	}
}

// switchedTarget answers what a test tells it to, and counts its requests.
type switchedTarget struct {
	*httptest.Server
	status   atomic.Int64
	requests atomic.Int64
}

func newSwitchedTarget(t *testing.T) *switchedTarget {
	t.Helper()
	target := &switchedTarget{}
	target.status.Store(http.StatusOK)
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.requests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(int(target.status.Load()))
		_, _ = io.WriteString(w, "value=42\n")
	}))
	t.Cleanup(target.Close)
	return target
}

// freshnessOnly is the text exposition of an answer holding nothing but the
// freshness series of a result just fetched.
func freshnessOnly() string {
	var b strings.Builder
	for _, name := range []string{resultStaleMetric, resultAgeMetric} {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s 0\n", name, resultFreshnessHelp[name], name, name)
	}
	return b.String()
}

// A collector with cache.stale_if_error has the freshness series on every
// answer, so on one that carried on too: nothing of the collector's, and the
// two gauges saying the answer is no stale result, as a probe whose rules
// produced no series has them. Carrying on is no failure, so the last good
// result does not stand in for it; and it is no result, so it is not stored
// as one: the last good result stays stored as it was.
func TestAProbeThatCarriesOnIsNotAnsweredStaleAndHasTheFreshnessSeries(t *testing.T) {
	for _, policy := range []string{model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
		t.Run(policy, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			target := newSwitchedTarget(t)
			carried := testutil.Collector("carried", "text")
			carried.ErrorHandling.OnFetchError = policy
			carried.Cache.StaleIfError = model.Duration(5 * time.Minute)
			none := noSeriesCollector("none")
			none.Cache.StaleIfError = model.Duration(5 * time.Minute)
			cfg := &model.Config{Collectors: []model.Collector{carried, none}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
			server := newStaticServer(t, cfg, nil)
			exporter := httptest.NewServer(server.Handler())
			t.Cleanup(exporter.Close)
			path := probePath("carried", target.URL, "")
			working := probePath("none", textTarget(t, "nothing\n").URL, "")

			good := "# TYPE demo_value gauge\ndemo_value 42\n" + freshnessOnly()
			if first := scrapeAs(t, exporter.URL, http.MethodGet, path, "", false); first.status != http.StatusOK || first.body != good {
				t.Fatalf("the first probe answered %d:\n%s\nwant\n%s", first.status, first.body, good)
			}
			if queued := server.drainOTLP(); len(queued) != 1 || metricByName(queued[0].Set, "demo_value") == nil {
				t.Fatalf("the first probe queued %+v for OTLP, want its result", queued)
			}
			target.status.Store(http.StatusServiceUnavailable)
			// Nothing was read from the target, so nothing is exported over
			// OTLP, not even the freshness series.
			if got := scrapeAs(t, exporter.URL, http.MethodGet, path, "", false); got.body != freshnessOnly() {
				t.Errorf("answered\n%s\nwant the freshness series only\n%s", got.body, freshnessOnly())
			}
			if queued := server.drainOTLP(); len(queued) != 0 {
				t.Errorf("the probe that carried on queued %+v for OTLP", queued)
			}
			for _, format := range expositionFormats {
				for _, gzipped := range []bool{false, true} {
					got := scrapeAs(t, exporter.URL, http.MethodGet, path, format.accept, gzipped)
					if got.status != http.StatusOK || got.header.Get("Content-Type") != format.contentType {
						t.Fatalf("as %s: answered %d as %q", format.name, got.status, got.header.Get("Content-Type"))
					}
					body := got.plain(t)
					if format.accept == "" && body != freshnessOnly() {
						t.Errorf("answered\n%s\nwant the freshness series only\n%s", body, freshnessOnly())
					}
					samples, _ := sampleLines(body)
					if want := []string{resultStaleMetric + " 0", resultAgeMetric + " 0"}; !reflect.DeepEqual(samples, want) || !strings.HasSuffix(body, format.empty) {
						t.Errorf("as %s: answered\n%s\nwant the samples %v, and the format's ending", format.name, body, want)
					}
					if twin := scrapeAs(t, exporter.URL, http.MethodGet, working, format.accept, gzipped); !reflect.DeepEqual(got, twin) {
						t.Errorf("as %s, gzip=%v: answered\n%+v\nbut a probe whose rules produced no series is answered\n%+v", format.name, gzipped, got, twin)
					}
				}
			}
			stats := selfMetrics(t, server)
			for series, want := range map[string]float64{
				`http_exporter_cache_stale_served_total{collector="carried"}`: 0,
				// The good result of the first probe, and nothing of the
				// probes that carried on.
				`http_exporter_cache_entries{collector="carried"}`:         1,
				`http_exporter_scrape_success_total{collector="carried"}`:  8,
				`http_exporter_metrics_emitted_total{collector="carried"}`: 1,
			} {
				if got := seriesValue(t, stats, series); got != want {
					t.Errorf("%s is %v, want %v", series, got, want)
				}
			}
			if strings.Contains(logs.String(), "answered with the last successful result") {
				t.Errorf("a probe that carried on was answered stale:\n%s", logs)
			}
			// The target is asked on every probe, and answers again.
			target.status.Store(http.StatusOK)
			if again := scrapeAs(t, exporter.URL, http.MethodGet, path, "", false); again.body != good || target.requests.Load() != 9 {
				t.Errorf("after the target recovered: answered\n%s\nafter %d requests to it, want the series again and 9", again.body, target.requests.Load())
			}
		})
	}
}

// A probe that carried on is not kept in the response cache: with cache.ttl
// the next probe goes to the target again rather than being answered the
// empty result from memory, and the result of that one is what is stored.
func TestAProbeThatCarriesOnIsNotCached(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newSwitchedTarget(t)
	target.status.Store(http.StatusServiceUnavailable)
	carried := cachingCollector("carried", time.Minute)
	carried.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	server := flightServer(t, carried)
	path := probePath("carried", target.URL, "")

	if first := probeOnce(t, server, path, nil); first.Code != http.StatusOK || first.Header().Get("Content-Type") != expositionContentType || first.Body.Len() != 0 {
		t.Fatalf("the probe that carried on answered %d as %q:\n%s", first.Code, first.Header().Get("Content-Type"), first.Body)
	}
	if entries := seriesValue(t, selfMetrics(t, server), `http_exporter_cache_entries{collector="carried"}`); entries != 0 {
		t.Fatalf("the probe that carried on left %v cache entries", entries)
	}
	target.status.Store(http.StatusOK)
	if second := probeOnce(t, server, path, nil); second.Body.String() != "# TYPE demo_value gauge\ndemo_value 42\n" || target.requests.Load() != 2 {
		t.Fatalf("the next probe answered\n%s\nafter %d requests to the target, want its series and 2", second.Body, target.requests.Load())
	}
	// That one is cached, and answers while the target fails again.
	target.status.Store(http.StatusServiceUnavailable)
	if third := probeOnce(t, server, path, nil); third.Body.String() != "# TYPE demo_value gauge\ndemo_value 42\n" || target.requests.Load() != 2 {
		t.Fatalf("the cached result answered\n%s\nafter %d requests to the target", third.Body, target.requests.Load())
	}
}

// Probes sharing one trip that carried on each get the empty exposition in
// the format they asked for.
func TestProbesSharingATripThatCarriedOnEachGetTheirOwnFormat(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusServiceUnavailable, "unavailable")
	carried := testutil.Collector("shared", "text")
	carried.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	server := flightServer(t, carried)
	path := probePath("shared", target.URL, "")
	text := probeAsync(context.Background(), server, path, nil)
	om := probeAsync(context.Background(), server, path, http.Header{"Accept": {prometheus2Accept}})
	waitForWaiters(t, server, 2)
	target.open()
	if got := <-text; got.code != http.StatusOK || got.body != "" {
		t.Errorf("the text probe got %d:\n%s", got.code, got.body)
	}
	if got := <-om; got.code != http.StatusOK || got.body != "# EOF\n" {
		t.Errorf("the OpenMetrics probe got %d: %q, want # EOF alone", got.code, got.body)
	}
	if n := target.requests.Load(); n != 1 {
		t.Errorf("the target was asked %d times, want once", n)
	}
}

// A debug probe of a collector that carries on still reports the stage as
// failed and as carried on past, and says what a probe would have been
// answered: 200 with no series of the collector's, and, with
// cache.stale_if_error, the freshness series.
func TestADebugProbeReportsTheStageAProbeCarriedOnPast(t *testing.T) {
	for _, tc := range carryOnCases() {
		for _, staleIfError := range []time.Duration{0, 5 * time.Minute} {
			t.Run(fmt.Sprintf("%s/stale_if_error=%s", tc.name, staleIfError), func(t *testing.T) {
				testutil.CaptureLogs(t)
				carried := testutil.Collector("carried", "text")
				tc.setup(&carried, model.ErrorPolicyLog)
				carried.Cache.StaleIfError = model.Duration(staleIfError)
				server := flightServer(t, carried)
				server.SetProbeDebug(true)
				response := probeOnce(t, server, probePath("carried", tc.target(t), "&debug=true"), nil)
				report := response.Body.String()
				if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
					t.Fatalf("the report answered %d as %q:\n%s", response.Code, response.Header().Get("Content-Type"), report)
				}
				assertContains(t, report, "A probe would have answered 200 with no series of the collector's: a stage failed and error_handling carried on.\n")
				failed := false
				for _, line := range strings.Split(report, "\n") {
					fields := strings.Fields(line)
					if len(fields) > 2 && fields[0] == tc.stage && fields[1] == "failed" && strings.HasSuffix(line, "; carried on, error_handling log") {
						failed = true
					}
				}
				if !failed {
					t.Errorf("the report does not show the %s stage as failed and carried on past:\n%s", tc.stage, report)
				}
				served := "  none\n"
				if staleIfError > 0 {
					served = freshnessOnly()
				}
				if !strings.HasSuffix(report, "\nMetrics a probe would have served\n"+served) {
					t.Errorf("the report does not end with what a probe would have served, %q:\n%s", served, report)
				}
			})
		}
	}
}

// A static target whose scrape carried on is up and contributes nothing but
// its health series to the static targets endpoint, in the text format and
// in OpenMetrics, whether or not its collector has cache.stale_if_error.
func TestAStaticTargetThatCarriesOnContributesItsHealthSeriesOnly(t *testing.T) {
	for _, tc := range carryOnCases() {
		for _, policy := range []string{model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
			for _, staleIfError := range []time.Duration{0, 5 * time.Minute} {
				t.Run(fmt.Sprintf("%s/%s/stale_if_error=%s", tc.name, policy, staleIfError), func(t *testing.T) {
					testutil.CaptureLogs(t)
					collector := testutil.Collector("demo", "text")
					tc.setup(&collector, policy)
					collector.Cache.StaleIfError = model.Duration(staleIfError)
					address := tc.target(t)
					cfg := &model.Config{Collectors: []model.Collector{collector}}
					file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "flaky", Collector: "demo", Target: address}}}
					server := newStaticServer(t, cfg, file)
					server.scrapeStaticTargets(context.Background(), 0)

					labels := `{collector="demo",static_target="flaky",target="` + address + `"}`
					for _, format := range expositionFormats {
						response := probeOnce(t, server, "/static-targets", http.Header{"Accept": {format.accept}})
						body := response.Body.String()
						if response.Code != http.StatusOK || response.Header().Get("Content-Type") != format.contentType || !strings.HasSuffix(body, "\n"+format.empty) {
							t.Fatalf("as %s: answered %d as %q:\n%s", format.name, response.Code, response.Header().Get("Content-Type"), body)
						}
						samples, _ := sampleLines(body)
						var names []string
						for _, sample := range samples {
							name, _, _ := strings.Cut(sample, " ")
							names = append(names, name)
						}
						want := []string{"http_exporter_target_up" + labels, "http_exporter_target_scrape_duration_seconds" + labels, "http_exporter_target_last_success_timestamp_seconds" + labels}
						if !reflect.DeepEqual(names, want) || samples[0] != "http_exporter_target_up"+labels+" 1" {
							t.Errorf("as %s: the endpoint holds\n%s\nwant the target up and its three health series only", format.name, body)
						}
					}
				})
			}
		}
	}
}

// probeTripBeforeCarriedOnWasRendered is probeTrip as it was before a probe
// that carried on was answered an exposition: the oracle of the differential
// test below.
func probeTripBeforeCarriedOnWasRendered(ctx context.Context, s *Server, p upstreamProbe) *probeResult {
	c, name, rec, logTarget := p.collector, p.collector.Name, p.rec, p.logTarget
	out := newProbeRecorder()
	if p.cacheKey != "" && model.CacheTTL(c) > 0 {
		if cached, fetched, ok := s.cache.Get(p.cacheKey, time.Now()); ok {
			rec.update(func(x *serverStats) {
				x.cacheHits++
				x.emitted += uint64(len(cached.Metrics))
			})
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			out.metrics = &answer
			s.queueProbeOTLP(answer, c, logTarget, fetched)
			return out.result(true)
		}
		rec.update(func(x *serverStats) { x.cacheMisses++ })
	}
	limit := maxConcurrentProbes(c)
	if full := s.trips.tryAcquire(name, limit); full != nil {
		rec.update(func(x *serverStats) { countRejection(x, full) })
		s.failures.failed(s.logger, slog.LevelWarn, p.failureKey(), "probe rejected: too many probes in progress", "concurrency", nil, append(p.logAttrs(), "reason", full.message)...)
		http.Error(out, full.message+"; this probe was not sent", http.StatusServiceUnavailable)
		return out.result(false)
	}
	defer s.trips.release(name)
	if p.budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.budget)
		defer cancel()
	}
	result := s.collect(ctx, collectJob{
		collector: c, target: p.target, overrides: p.overrides, headers: p.forwarded,
		rec: rec, display: logTarget, cacheKey: p.cacheKey, budget: p.budget, budgetSource: p.budgetSource,
		log: collectLog{
			key:    p.failureKey(),
			failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
			attrs: p.logAttrs(),
		},
	})
	switch {
	case result.aborted:
		http.Error(out, "probe cancelled: the caller went away", http.StatusServiceUnavailable)
		abandoned := out.result(false)
		abandoned.abandoned = true
		return abandoned
	case result.metric != "":
		writeProbeError(out, http.StatusBadGateway, probeError{
			Stage:     result.stage,
			Collector: name,
			Metric:    result.metric,
			Target:    logTarget,
			Error:     result.err.Error(),
		})
		return out.result(false)
	case result.refused:
		http.Error(out, fmt.Sprintf("collector %s refused the target: %v", name, result.err), http.StatusForbidden)
		refused := out.result(false)
		refused.refused = true
		return refused
	case result.failed():
		http.Error(out, fmt.Sprintf("collector %s %s failed: %v", name, result.stage, result.err), http.StatusBadGateway)
		failure := out.result(false)
		failure.unauthorized = result.unauthorized
		return failure
	case result.carriedOn:
		return out.result(true)
	}
	out.metrics = &result.answer
	s.queueProbeOTLP(result.answer, c, logTarget, result.fetched)
	return out.result(true)
}

// renderedTrip is a trip's result as its probes are answered it, through the
// compression /probe has: the result's own verdicts, and the status, headers
// and body of each of the formats, plain and for a scrape accepting gzip.
type renderedTrip struct {
	ok, abandoned, unauthorized, refused bool
	answers                              map[string]scrapedAnswer
}

func renderTrip(result *probeResult) renderedTrip {
	out := renderedTrip{ok: result.ok, abandoned: result.abandoned, unauthorized: result.unauthorized, refused: result.refused, answers: map[string]scrapedAnswer{}}
	for _, format := range expositionFormats {
		for _, encoding := range []string{"", "gzip"} {
			request := httptest.NewRequest(http.MethodGet, "/probe", nil)
			if format.accept != "" {
				request.Header.Set("Accept", format.accept)
			}
			if encoding != "" {
				request.Header.Set("Accept-Encoding", encoding)
			}
			recorder := httptest.NewRecorder()
			compressed(result.writeTo)(recorder, request)
			out.answers[format.name+"/"+encoding] = scrapedAnswer{status: recorder.Code, header: recorder.Header(), body: recorder.Body.String()}
		}
	}
	return out
}

// tripOf makes one trip of the collector named name to target, by trip.
func tripOf(t *testing.T, server *Server, name, target string, trip func(context.Context, *Server, upstreamProbe) *probeResult) renderedTrip {
	t.Helper()
	c := model.CollectorByName(server.manager.Get(), name)
	if c == nil {
		t.Fatalf("no collector %s", name)
	}
	p := upstreamProbe{
		collector: c, target: target, logTarget: target,
		rec: server.probeRecorderFor(server.statsFor(name), name, "", http.MethodGet),
	}
	// A key of its own, as probeHandler gives a collector with a cache.
	if model.UsesCache(c) {
		p.cacheKey = name + "\x00" + target
	}
	return renderTrip(trip(context.Background(), server, p))
}

// Only the answer of a trip that carried on changed. A trip that goes
// through whole, with series or with none, one that fails under fail at any
// stage, with the plain error or a rule's JSON one, one the target policy
// refuses and one answered from the cache are answered byte for byte, status
// and headers included, in every format, compressed and not, what probeTrip
// answered before: over a table of collectors and a generated one of the
// statuses and bodies a target answers. A trip that carried on was answered
// without any header and without a body in every format, which is what
// changed. Under the race detector the target answers every body with 200
// and each other status with one body, a different one each, and a collector
// that keeps no result makes one trip to each: what a trip makes of a body
// does not depend on which status it accepted, a status it does not accept
// ends it before the body, and a second trip that no cache answers is the
// first again.
func TestOnlyTheAnswerOfATripThatCarriedOnChanged(t *testing.T) {
	testutil.CaptureLogs(t)
	type collectorCase struct {
		name      string
		carriesOn bool
		setup     func(c *model.Collector)
	}
	collectors := []collectorCase{
		{name: "plain", setup: func(*model.Collector) {}},
		{name: "no_series", setup: func(c *model.Collector) { c.Metrics[0].ErrorMode = model.ErrorModeIgnore }},
		{name: "rule_fails", setup: func(c *model.Collector) { c.Metrics[0].ErrorMode = model.ErrorModeFail }},
		{name: "stale_if_error", setup: func(c *model.Collector) { c.Cache.StaleIfError = model.Duration(time.Minute) }},
		{name: "cached", setup: func(c *model.Collector) { c.Cache.TTL = model.Duration(time.Minute) }},
		{name: "json", setup: func(c *model.Collector) {
			c.Decoder.Type, c.Transform.Type = "json", "jq"
			c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: ".value"}}
		}},
		{name: "xpath", setup: func(c *model.Collector) {
			c.Decoder.Type, c.Transform.Type = "", "xpath"
			c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: "//value"}}
		}},
		{name: "denied", setup: func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.1"} }},
		{name: "limited", setup: func(c *model.Collector) {
			// A second rule that makes a second series: the same rule
			// twice, which made one too, is refused when it loads.
			second := c.Metrics[0]
			second.Labels = []model.LabelRule{{Name: "copy", Value: "2"}}
			c.Limits.MaxMetrics, c.Metrics = 1, append(c.Metrics, second)
		}},
	}
	for _, policy := range []string{model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
		collectors = append(collectors,
			collectorCase{name: "fetch_" + policy, carriesOn: true, setup: func(c *model.Collector) { c.ErrorHandling.OnFetchError = policy }},
			collectorCase{name: "decode_" + policy, carriesOn: true, setup: func(c *model.Collector) {
				c.Decoder.Type, c.Transform.Type = "json", "jq"
				c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: ".value"}}
				c.ErrorHandling.OnDecodeError = policy
			}},
			collectorCase{name: "transform_" + policy, carriesOn: true, setup: func(c *model.Collector) {
				c.Decoder.Type, c.Transform.Type = "", "xpath"
				c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: "//value"}}
				c.ErrorHandling.OnTransformError = policy
			}},
		)
	}
	servers := func() *Server {
		var all []model.Collector
		for _, tc := range collectors {
			c := testutil.Collector(tc.name, "text")
			tc.setup(&c)
			all = append(all, c)
		}
		return flightServer(t, all...)
	}
	before, after := servers(), servers()

	// What a target answers: every status with every body.
	type answer struct {
		status      int
		contentType string
		body        string
	}
	var answers []answer
	for s, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		for b, body := range []struct{ contentType, text string }{
			{"text/plain", "value=42\n"},
			{"text/plain", "value=1\nvalue=2\n"},
			{"text/plain", "nothing\n"},
			{"text/plain", ""},
			{"application/json", `{"value": 7}`},
			{"application/json", `{"other": 7}`},
			{"application/json", "{not json"},
			{"application/xml", "<r><value>3</value></r>"},
			{"application/xml", "<r><value>3</value>"},
		} {
			if alloctest.RaceDetector && s > 0 && b != s {
				continue
			}
			answers = append(answers, answer{status, body.contentType, body.text})
		}
	}
	var current atomic.Pointer[answer]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		a := current.Load()
		w.Header().Set("Content-Type", a.contentType)
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(target.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	compared, carried := 0, 0
	compare := func(tc collectorCase, address, what string) {
		t.Helper()
		// Twice, so the cache answers the second trip of a collector that
		// has one.
		trips := 2
		if alloctest.RaceDetector && !model.UsesCache(model.CollectorByName(before.manager.Get(), tc.name)) {
			trips = 1
		}
		for trip := range trips {
			was := tripOf(t, before, tc.name, address, probeTripBeforeCarriedOnWasRendered)
			is := tripOf(t, after, tc.name, address, func(ctx context.Context, s *Server, p upstreamProbe) *probeResult { return s.probeTrip(ctx, p) })
			compared++
			if reflect.DeepEqual(was, is) {
				continue
			}
			for key, old := range was.answers {
				// A trip that carried on was answered 200 with nothing, and
				// is answered an exposition: the one difference there is.
				if !tc.carriesOn || !reflect.DeepEqual(old, renderTrip(newProbeRecorder().result(true)).answers[key]) || is.answers[key].status != http.StatusOK || is.answers[key].header.Get("Content-Type") == "" {
					t.Errorf("%s, %s, trip %d, as %s: answered\n%+v\nbefore, and now\n%+v", tc.name, what, trip, key, old, is.answers[key])
				}
			}
			if !tc.carriesOn || was.ok != is.ok || was.abandoned != is.abandoned || was.unauthorized != is.unauthorized || was.refused != is.refused {
				t.Errorf("%s, %s, trip %d: the trip was %+v, and is %+v", tc.name, what, trip, was, is)
			}
			carried++
		}
	}
	for _, tc := range collectors {
		for i := range answers {
			current.Store(&answers[i])
			before.cache, after.cache = newResponseCache(), newResponseCache()
			compare(tc, target.URL, fmt.Sprintf("a target answering %d with %q", answers[i].status, answers[i].body))
		}
		compare(tc, gone.URL, "a target nothing listens at")
	}
	if compared == 0 || carried == 0 || carried == compared {
		t.Fatalf("%d trips compared, %d of them carried on: the table covers neither or only one kind", compared, carried)
	}
	if selfMetrics(t, before) == "" || !sameCounters(selfMetrics(t, before), selfMetrics(t, after)) {
		t.Errorf("the self-metrics differ:\nbefore\n%s\nafter\n%s", selfMetrics(t, before), selfMetrics(t, after))
	}
}

// sameCounters reports whether two self-metrics expositions hold the same
// counters with the same values: the series ending in _total.
func sameCounters(a, b string) bool {
	counters := func(exposition string) []string {
		var out []string
		samples, _ := sampleLines(exposition)
		for _, sample := range samples {
			name, _, _ := strings.Cut(sample, "{")
			if strings.HasSuffix(name, "_total") {
				out = append(out, sample)
			}
		}
		return out
	}
	return len(counters(a)) > 0 && reflect.DeepEqual(counters(a), counters(b))
}

// The answers of probes that do not carry on are what they were, to the
// header and the byte: a probe answered with series, one whose rules
// produced none, one a metric rule with error_mode fail fails, with its JSON
// error, and one a stage fails under fail, each as the text format and as
// OpenMetrics, plain and compressed.
func TestProbesThatDoNotCarryOnAreAnsweredAsBefore(t *testing.T) {
	testutil.CaptureLogs(t)
	strict := testutil.Collector("strict", "text")
	strict.Metrics[0].ErrorMode = model.ErrorModeFail
	server := flightServer(t, testutil.Collector("plain", "text"), noSeriesCollector("none"), strict)
	exporter := httptest.NewServer(server.Handler())
	t.Cleanup(exporter.Close)
	good, nothing, down := textTarget(t, "value=42\n"), textTarget(t, "nothing\n"), errorStageTarget(t, errorStageCases[0])

	exposition := func(series string) func(contentType, ending string) (int, http.Header, string) {
		return func(contentType, ending string) (int, http.Header, string) {
			return http.StatusOK, http.Header{"Content-Type": {contentType}, "X-Content-Type-Options": {"nosniff"}, "Vary": {"Accept-Encoding", "Accept"}}, series + ending
		}
	}
	failure := func(contentType, body string) func(string, string) (int, http.Header, string) {
		return func(string, string) (int, http.Header, string) {
			return http.StatusBadGateway, http.Header{"Content-Type": {contentType}, "X-Content-Type-Options": {"nosniff"}, "Vary": {"Accept-Encoding"}}, body
		}
	}
	for _, tc := range []struct {
		name, path string
		want       func(contentType, ending string) (int, http.Header, string)
	}{
		{"series", probePath("plain", good.URL, ""), exposition("# TYPE demo_value gauge\ndemo_value 42\n")},
		{"no series", probePath("none", nothing.URL, ""), exposition("")},
		{"a rule under fail", probePath("strict", nothing.URL, ""), failure("application/json; charset=utf-8",
			`{"status":"error","stage":"metric","collector":"strict","metric":"demo_value","target":"`+nothing.URL+`","error":"regex for metric \"demo_value\" matched no text"}`+"\n")},
		{"a stage under fail", probePath("plain", down.URL, ""), failure("text/plain; charset=utf-8", "collector plain http_status failed: received HTTP status 503\n")},
	} {
		for _, format := range expositionFormats {
			for _, gzipped := range []bool{false, true} {
				got := scrapeAs(t, exporter.URL, http.MethodGet, tc.path, format.accept, gzipped)
				status, header, body := tc.want(format.contentType, format.empty)
				if gzipped {
					header.Set("Content-Encoding", "gzip")
				}
				header.Set("Content-Length", strconv.Itoa(len(got.body)))
				var compressedBody bytes.Buffer
				if gzipped {
					// As the exporter compresses: one write, then the end.
					gz := gzip.NewWriter(&compressedBody)
					_, _ = gz.Write([]byte(body))
					_ = gz.Close()
				} else {
					compressedBody.WriteString(body)
				}
				if got.status != status || !reflect.DeepEqual(got.header, header) || got.plain(t) != body || got.body != compressedBody.String() {
					t.Errorf("%s as %s, gzip=%v: answered %d %v\n%q\nwant %d %v\n%q", tc.name, format.name, gzipped, got.status, got.header, got.plain(t), status, header, body)
				}
			}
		}
	}
}
