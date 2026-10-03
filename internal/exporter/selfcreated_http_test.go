//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// createdServer is a server with self-metric families of every kind: two
// regex collectors and one that runs Python, the verbose families, the OTLP
// status and a static target, and the go_ and process_ families when resource
// says so. They change at every read, so a test that compares two reads does
// without them.
func createdServer(t *testing.T, created, resource bool) *Server {
	t.Helper()
	usePythonPool(t)
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("kept", "text"), testutil.Collector("gone", "text"), pythonCollector("py", `metric(name="v", value=1)`)},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true, ResourceMetrics: resource, CreatedTimestamps: created}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "kept", Target: "http://static.example"}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	return server
}

// setCreatedTimestamps switches web.self_metrics.created_timestamps of the
// configuration in force, in place, so that one state of a server can be read
// with the setting and without it.
func setCreatedTimestamps(server *Server, on bool) {
	server.manager.Get().Web.SelfMetrics.CreatedTimestamps = on
}

// answerOf is the answer of an endpoint to an Accept header: its content
// type and its body.
func answerOf(t *testing.T, server *Server, path, accept string) (contentType, body string) {
	t.Helper()
	header := http.Header{}
	if accept != "" {
		header.Set("Accept", accept)
	}
	recorder := probeOnce(t, server, path, header)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", path, recorder.Code, recorder.Body)
	}
	return recorder.Header().Get("Content-Type"), recorder.Body.String()
}

// createdTimes are the _created samples of an answer: the time of each, in
// milliseconds, by the sample's name and labels as they are written.
func createdTimes(t *testing.T, answer string) map[string]int64 {
	t.Helper()
	times := map[string]int64{}
	for _, line := range strings.Split(answer, "\n") {
		if !isCreatedLine(line) {
			continue
		}
		series, value := line[:strings.LastIndexByte(line, ' ')], line[strings.LastIndexByte(line, ' ')+1:]
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("the _created sample %q has no time: %v", line, err)
		}
		times[series] = int64(math.Round(seconds * 1000))
	}
	return times
}

// isCreatedLine reports whether a line of an answer is a _created sample.
// The self-metrics have no timestamps, so the time is the line's last field.
func isCreatedLine(line string) bool {
	end := strings.IndexAny(line, "{ ")
	return end > 0 && !strings.HasPrefix(line, "#") && strings.HasSuffix(line[:end], "_created")
}

// withoutCreatedLines is an answer without its _created samples.
func withoutCreatedLines(answer string) string {
	var kept []string
	for _, line := range strings.Split(answer, "\n") {
		if !isCreatedLine(line) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// cumulativeSeries counts the counters, histograms and summaries of a set.
func cumulativeSeries(set model.MetricSet) int {
	n := 0
	for _, m := range set.Metrics {
		switch m.Type {
		case model.CounterMetricType, model.HistogramMetricType, model.SummaryMetricType:
			n++
		}
	}
	return n
}

// The setting is web.self_metrics.created_timestamps of the configuration
// file, and is off unless the file says otherwise.
func TestCreatedTimestampsAreOffUnlessConfigured(t *testing.T) {
	for document, want := range map[string]bool{
		"": false,
		"web:\n  self_metrics:\n    created_timestamps: false\n": false,
		"web:\n  self_metrics:\n    created_timestamps: true\n":  true,
	} {
		cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", document+testutil.MinimalConfig))
		if err != nil {
			t.Fatal(err)
		}
		server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
		_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
		if got := strings.Contains(body, `http_exporter_scrapes_created{collector="demo"} `); cfg.Web.SelfMetrics.CreatedTimestamps != want || got != want {
			t.Errorf("%q: the setting is %v and the answer has a _created sample: %v; want both %v", document, cfg.Web.SelfMetrics.CreatedTimestamps, got, want)
		}
	}
}

// Without the setting the self-metrics have no _created sample in either
// format. With it, the text format's answer is byte for byte what it is
// without, and the OpenMetrics answer is what it is without plus one _created
// line for every counter, histogram and summary series: no family moves, and
// no other line changes. The setting does not change which format an Accept
// header is answered in.
func TestCreatedSamplesAreWrittenOnlyWithTheSettingAndOnlyInOpenMetrics(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := createdServer(t, false, false)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)

	type answer struct{ contentType, body string }
	accepts := []string{"", "text/plain", prometheus2Accept, prometheus3Accept, "application/openmetrics-text;version=0.0.1"}
	read := func() map[string]answer {
		answers := map[string]answer{}
		for _, accept := range accepts {
			contentType, body := answerOf(t, server, "/self-metrics", accept)
			answers[accept] = answer{contentType, body}
		}
		return answers
	}
	off := read()
	setCreatedTimestamps(server, true)
	on := read()

	for _, accept := range accepts {
		if strings.Contains(off[accept].body, "_created") {
			t.Errorf("Accept %q, without the setting: the answer has a _created sample:\n%s", accept, off[accept].body)
		}
		if on[accept].contentType != off[accept].contentType {
			t.Errorf("Accept %q is answered as %q with the setting and as %q without", accept, on[accept].contentType, off[accept].contentType)
		}
		openMetrics := strings.HasPrefix(off[accept].contentType, "application/openmetrics-text")
		if openMetrics != (accept != "" && accept != "text/plain") {
			t.Errorf("Accept %q is answered as %q", accept, off[accept].contentType)
		}
		if !openMetrics {
			if on[accept].body != off[accept].body {
				t.Errorf("Accept %q: the text format's answer changes with the setting:\n%s\nwithout:\n%s", accept, on[accept].body, off[accept].body)
			}
			continue
		}
		if got := withoutCreatedLines(on[accept].body); got != off[accept].body {
			t.Errorf("Accept %q: the answer with the setting differs from the one without in more than its _created lines:\n%s\nwithout:\n%s", accept, on[accept].body, off[accept].body)
		}
		if got, want := len(createdTimes(t, on[accept].body)), cumulativeSeries(server.selfMetricSet()); got != want || want < 40 {
			t.Errorf("Accept %q: %d _created samples for %d counters, histograms and summaries", accept, got, want)
		}
	}
}

// With the setting, every counter, histogram and summary series of the
// self-metrics is followed, in its family, by its _created sample with its
// labels: after a counter's _total, and after a histogram's or a summary's
// _sum and _count. Gauges have none. The time is no earlier than the
// exporter's start and not in the future, and the answer is one a strict
// parser reads.
func TestEveryOwnCounterHistogramAndSummaryIsFollowedByItsCreatedSample(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := createdServer(t, true, true)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	latest := time.Now().UnixMilli()

	if err := strictOpenMetricsError(body); err != nil {
		t.Fatalf("a strict parser refuses the answer: %v\n%s", err, body)
	}
	families, err := readOpenMetrics(body)
	if err != nil {
		t.Fatal(err)
	}
	times := createdTimes(t, body)
	types := map[string]int{}
	for _, f := range families {
		cumulative := f.typ == "counter" || f.typ == "histogram" || f.typ == "summary"
		types[f.typ]++
		for i, s := range f.samples {
			created := s.name == f.name+"_created"
			if !cumulative {
				if strings.HasSuffix(s.name, "_created") {
					t.Errorf("the %s family %s has the sample %s", f.typ, f.name, s.name)
				}
				continue
			}
			// The series ends where the next sample is another's, or the
			// family ends: there, and only there, is its _created sample.
			last := i == len(f.samples)-1 || strictGroup(f, f.samples[i+1]) != strictGroup(f, s)
			switch {
			case last && !created:
				t.Errorf("%s %s: the series %v ends with %s, not with its _created sample", f.typ, f.name, s.labels, s.name)
			case !last && created:
				t.Errorf("%s %s: the _created sample of %v is not the last of its series", f.typ, f.name, s.labels)
			case created && i == 0, created && strictGroup(f, f.samples[i-1]) != strictGroup(f, s):
				t.Errorf("%s %s: the _created sample of %v follows no sample of its series", f.typ, f.name, s.labels)
			case created && f.typ == "counter" && f.samples[i-1].name != f.name+"_total":
				t.Errorf("counter %s: the _created sample of %v follows %s, not its _total", f.name, s.labels, f.samples[i-1].name)
			case created && f.typ != "counter" && f.samples[i-1].name != f.name+"_count":
				t.Errorf("%s %s: the _created sample of %v follows %s, not its _count", f.typ, f.name, s.labels, f.samples[i-1].name)
			}
		}
	}
	for _, typ := range []string{"counter", "histogram", "summary", "gauge"} {
		if types[typ] == 0 {
			t.Errorf("the self-metrics have no %s family, so the test does not cover one", typ)
		}
	}
	// One counter has no _created sample: go_memstats_alloc_bytes_total,
	// whose OpenMetrics family would be named as the gauge
	// go_memstats_alloc_bytes is, and which is therefore written as unknown
	// (planOpenMetrics).
	if types["unknown"] != 1 || !strings.Contains(body, "# TYPE go_memstats_alloc_bytes_total unknown\n") {
		t.Errorf("%d families of the self-metrics are written as unknown, want only go_memstats_alloc_bytes_total:\n%s", types["unknown"], body)
	}
	if got, want := len(times), cumulativeSeries(server.selfMetricSet())-1; got != want {
		t.Errorf("%d _created samples for %d counters, histograms and summaries that keep their type", got, want)
	}
	for series, at := range times {
		if at < exporterStart().UnixMilli() || at > latest {
			t.Errorf("%s is %d, which is not between the exporter's start %d and now %d", series, at, exporterStart().UnixMilli(), latest)
		}
	}
}

// The strict reference parser, prometheus_client's, reads the self-metrics
// with their _created samples, and reads every one of those samples.
func TestTheStrictOpenMetricsParserReadsTheCreatedSamples(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := createdServer(t, true, true)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	verdict := strictParser(t, []string{body})[0]
	if verdict.Error != "" {
		t.Fatalf("the strict parser refuses the self-metrics with _created samples: %s\n%s", verdict.Error, body)
	}
	read := 0
	for _, s := range verdict.Samples {
		if strings.HasSuffix(s.Name, "_created") {
			read++
			if s.Type != "counter" && s.Type != "histogram" && s.Type != "summary" {
				t.Errorf("the strict parser read %s in the %s family %s", s.Name, s.Type, s.Family)
			}
		}
	}
	if want := len(createdTimes(t, body)); read != want || want == 0 {
		t.Fatalf("the strict parser read %d _created samples of the %d written", read, want)
	}
}

// A series that has counted since the exporter started says so: the
// counters of a collector the exporter started with, its rule failures and
// its scrape-time histogram, the reload, OTLP and Python worker counters. A
// request of the verbose mode counts since the first probe of it began.
func TestCreatedIsTheExportersStartForASeriesThatExistsFromTheStart(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := createdServer(t, true, false)
	probed := time.Unix(1_700_000_000, 250_000_000)
	server.requests.now = func() time.Time { return probed }
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	times := createdTimes(t, body)

	started := exporterStart().UnixMilli()
	request := `collector="kept",http_method="GET",url="` + target.URL + `"`
	for series, want := range map[string]int64{
		`http_exporter_scrapes_created{collector="kept"}`:                                               started,
		`http_exporter_scrapes_created{collector="gone"}`:                                               started,
		`http_exporter_probes_coalesced_created{collector="py"}`:                                        started,
		`http_exporter_rule_failures_created{collector="kept",metric="demo_value"}`:                     started,
		`http_exporter_collector_scrape_duration_seconds_created{collector="kept"}`:                     started,
		`http_exporter_config_reloads_created{file="config",result="success"}`:                          started,
		`http_exporter_otlp_exports_created{result="failure"}`:                                          started,
		`http_exporter_otlp_points_dropped_created`:                                                     started,
		`http_exporter_python_worker_starts_created{collector="py"}`:                                    started,
		`http_exporter_python_runs_created{collector="py",outcome="ok"}`:                                started,
		`http_exporter_python_pool_worker_stops_created{reason="idle"}`:                                 started,
		`http_exporter_scrapes_created{` + request + `}`:                                                probed.UnixMilli(),
		`http_exporter_metrics_emitted_created{` + request + `}`:                                        probed.UnixMilli(),
		`http_exporter_scrapes_created{collector="kept",http_method="GET",url="http://static.example"}`: probed.UnixMilli(),
	} {
		if got, written := times[series]; !written || got != want {
			t.Errorf("%s is %d (written: %v), want %d", series, got, written, want)
		}
	}
	if !strings.Contains(body, "http_exporter_scrapes_created{"+request+"} 1700000000.25\n") {
		t.Errorf("the time is not written in seconds:\n%s", body)
	}
	if now := time.Now(); started > now.UnixMilli() || now.Sub(exporterStart()) > time.Hour {
		t.Errorf("the exporter's start %v is not shortly before now, %v", exporterStart(), now)
	}
}

// The go_ and process_ counters and the garbage collection summary count
// since the process started, at the very time process_start_time_seconds
// gives where the platform has it.
func TestResourceCountersCountSinceTheProcessStarted(t *testing.T) {
	server := createdServer(t, true, true)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	times := createdTimes(t, body)
	started := exporterStart().UnixMilli()
	series := []string{"go_memstats_mallocs_created", "go_memstats_frees_created", "go_cpu_classes_gc_total_cpu_seconds_created", "go_gc_duration_seconds_created"}
	if strings.Contains(body, "process_start_time_seconds ") {
		series = append(series, "process_cpu_seconds_created")
		// A start /proc puts after the moment the exporter was loaded is not
		// believed (exporterStart).
		if want := int64(math.Round(seriesValue(t, body, "process_start_time_seconds") * 1000)); started != want && want <= loadedAt.UnixMilli() {
			t.Errorf("the exporter's start is %d and process_start_time_seconds says %d", started, want)
		}
	}
	for _, name := range series {
		if got, written := times[name]; !written || got != started {
			t.Errorf("%s is %d (written: %v), want the exporter's start %d", name, got, written, started)
		}
	}
}

// A counter that starts again says so with a later time: a collector a
// reload removed and brought back starts from zero, and its counters, rule
// failures and histogram count from when it came back; one a reload adds
// counts from then, whether a probe or a read of the self-metrics is the
// first to ask for it; one that stayed keeps the time it had.
func TestACollectorThatStartsAgainHasALaterCreated(t *testing.T) {
	target := textTarget(t, "value=42\n")
	kept, gone := testutil.Collector("kept", "text"), testutil.Collector("gone", "text")
	server := verboseServer(t, true, kept, gone)
	server.logger = testutil.QuietLogger(t)
	setCreatedTimestamps(server, true)
	for _, name := range []string{"kept", "gone"} {
		probeOnce(t, server, probePath(name, target.URL, ""), nil)
	}
	read := func() (string, map[string]int64) {
		_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
		return body, createdTimes(t, body)
	}
	started := exporterStart().UnixMilli()
	families := []string{"http_exporter_scrapes_created{collector=%q}", "http_exporter_collector_scrape_duration_seconds_created{collector=%q}", "http_exporter_rule_failures_created{collector=%q,metric=\"demo_value\"}"}
	check := func(times map[string]int64, collector string, from, to int64) {
		t.Helper()
		for _, family := range families {
			series := strings.ReplaceAll(family, "%q", strconv.Quote(collector))
			if got, written := times[series]; !written || got < from || got > to {
				t.Errorf("%s is %d (written: %v), want it from %d to %d", series, got, written, from, to)
			}
		}
	}
	_, times := read()
	check(times, "kept", started, started)
	check(times, "gone", started, started)

	reloadTo(t, server, kept)
	if body, _ := read(); strings.Contains(body, `collector="gone"`) {
		t.Fatalf("the removed collector is still shown:\n%s", body)
	}
	before := time.Now().UnixMilli()
	reloadTo(t, server, kept, gone, testutil.Collector("added", "text"))
	// One collector is first heard of by a probe, the other by the read of
	// the self-metrics.
	probeOnce(t, server, probePath("added", target.URL, ""), nil)
	body, times := read()
	after := time.Now().UnixMilli()
	if before <= started {
		t.Fatalf("the reload at %d is not after the exporter's start %d", before, started)
	}
	check(times, "kept", started, started)
	check(times, "gone", before, after)
	check(times, "added", before, after)
	if got := seriesValue(t, body, `http_exporter_scrapes_total{collector="gone"}`); got != 0 {
		t.Errorf("the collector brought back starts at %v, want 0", got)
	}
	if got := seriesValue(t, body, `http_exporter_scrapes_total{collector="kept"}`); got != 1 {
		t.Errorf("the collector that stayed is at %v, want 1", got)
	}
	// The time stays as it is from read to read.
	if _, again := read(); !maps.Equal(again, times) {
		t.Errorf("the _created samples changed between two reads:\n%v\n%v", times, again)
	}
}

// A request of the verbose mode that was dropped, for not being asked for or
// with verbose mode switched off, starts from zero when it is probed again,
// and its _created is the time of that probe, not of the first.
func TestARequestTrackedAgainHasALaterCreated(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := verboseServer(t, true, testutil.Collector("kept", "text"))
	server.logger = testutil.QuietLogger(t)
	setCreatedTimestamps(server, true)
	now := time.Unix(1_700_000_000, 0)
	server.requests.now = func() time.Time { return now }
	scrapes := `http_exporter_scrapes_total{collector="kept",http_method="GET",url="` + target.URL + `"}`
	created := `http_exporter_scrapes_created{collector="kept",http_method="GET",url="` + target.URL + `"}`
	read := func() (string, map[string]int64) {
		_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
		return body, createdTimes(t, body)
	}

	first := now
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	now = now.Add(time.Minute)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	body, times := read()
	if got := seriesValue(t, body, scrapes); got != 2 || times[created] != first.UnixMilli() {
		t.Fatalf("after two probes the request is at %v, created %d, want 2 and the first probe's time %d", got, times[created], first.UnixMilli())
	}

	now = now.Add(VerboseRequestIdleExpiry + time.Minute)
	if body, _ := read(); strings.Contains(body, target.URL) {
		t.Fatalf("the idle request is still shown:\n%s", body)
	}
	now = now.Add(time.Minute)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	body, times = read()
	if got := seriesValue(t, body, scrapes); got != 1 || times[created] != now.UnixMilli() {
		t.Fatalf("probed again the request is at %v, created %d, want 1 and that probe's time %d", got, times[created], now.UnixMilli())
	}
	// The collector's own counter went on counting, since the start.
	if got := seriesValue(t, body, `http_exporter_scrapes_total{collector="kept"}`); got != 3 || times[`http_exporter_scrapes_created{collector="kept"}`] != exporterStart().UnixMilli() {
		t.Fatalf("the collector is at %v, created %d, want 3 and the exporter's start", got, times[`http_exporter_scrapes_created{collector="kept"}`])
	}

	// Verbose mode switched off forgets the requests; switched on again, the
	// request starts once more.
	server.manager.Get().Web.SelfMetrics.Verbose = false
	if body, _ := read(); strings.Contains(body, target.URL) {
		t.Fatalf("without verbose mode the request is still shown:\n%s", body)
	}
	server.manager.Get().Web.SelfMetrics.Verbose = true
	now = now.Add(time.Minute)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	body, times = read()
	if got := seriesValue(t, body, scrapes); got != 1 || times[created] != now.UnixMilli() {
		t.Fatalf("after verbose mode was off the request is at %v, created %d, want 1 and the time of its probe %d", got, times[created], now.UnixMilli())
	}
}

// A static target's request counts since it was registered, which its first
// scrape does when it comes before the first read of the self-metrics.
func TestAStaticTargetsRequestCountsSinceItsFirstScrape(t *testing.T) {
	target := textTarget(t, "value=42\n")
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true, CreatedTimestamps: true}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	now := time.Unix(1_700_000_000, 0)
	server.requests.now = func() time.Time { return now }
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	now = now.Add(time.Minute)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	request := `{collector="text",http_method="GET",url="` + target.URL + `"}`
	if got := createdTimes(t, body)["http_exporter_decode_success_created"+request]; got != 1_700_000_000_000 || seriesValue(t, body, "http_exporter_decode_success_total"+request) != 1 {
		t.Fatalf("the request is created %d, want the time of its scrape:\n%s", got, body)
	}
}

// createdDocument is a configuration file with created timestamps, verbose
// self-metrics or not, and the named collectors.
func createdDocument(verbose bool, collectors ...string) string {
	return "web:\n  self_metrics:\n    verbose: " + strconv.FormatBool(verbose) + "\n    created_timestamps: true\n" + testutil.CollectorsDocument(collectors...)
}

// reloadTo reloads the server with document as its configuration file.
func (r *reloadable) reloadTo(document string) {
	r.t.Helper()
	r.write(r.path, document)
	if err := r.manager.Reload(config.ReloadTriggerSignal); err != nil {
		r.t.Fatal(err)
	}
}

// slowFirstTarget is a target that answers value=42 at once, except to the
// first request it gets, which waits until release is called. arrived has a
// value for every request that reached it.
func slowFirstTarget(t *testing.T) (target *httptest.Server, arrived <-chan struct{}, release func()) {
	t.Helper()
	var first atomic.Bool
	var once sync.Once
	held, reached := make(chan struct{}), make(chan struct{}, 16)
	release = func() { once.Do(func() { close(held) }) }
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wait := first.CompareAndSwap(false, true)
		reached <- struct{}{}
		if wait {
			select {
			case <-held:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(func() {
		release()
		target.Close()
	})
	return target, reached, release
}

// strictlyRead fails the test unless every answer is one a strict parser
// reads: by the rules the tests keep, and by the strict reference parser,
// prometheus_client's, where there is one.
func strictlyRead(t *testing.T, answers ...string) {
	t.Helper()
	for _, answer := range answers {
		if err := strictOpenMetricsError(answer); err != nil {
			t.Errorf("a strict parser refuses the answer: %v\n%s", err, answer)
		}
	}
	t.Run("the strict reference parser", func(t *testing.T) {
		for i, verdict := range strictParser(t, answers) {
			if verdict.Error != "" {
				t.Errorf("the strict parser refuses the answer: %s\n%s", verdict.Error, answers[i])
			}
		}
	})
}

// A trip that is under way when a reload removes its collector, and ends
// after the collector's statistics were dropped, counts nowhere that is
// shown: not in the scrape-time histogram of a collector brought back under
// the name, which would then hold an observation older than its _created, not
// in that collector's counters, and not in a per-request series, which is not
// shown for a collector that is gone either. That holds whether the trip ends
// before the collector is back or after it was probed again.
func TestATripEndingAfterItsCollectorWasRemovedIsNotCountedInTheOneBroughtBack(t *testing.T) {
	for _, endsLate := range []bool{false, true} {
		target, arrived, release := slowFirstTarget(t)
		r := newReloadable(t, createdDocument(true, "kept", "gone"), "")
		read := func() (string, map[string]int64) {
			_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
			return body, createdTimes(t, body)
		}
		old := probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil)
		<-arrived
		r.reloadTo(createdDocument(true, "kept"))
		removed, _ := read()
		end := func() {
			release()
			if outcome := <-old; outcome.code != http.StatusOK {
				t.Fatalf("the probe under way at the reload was answered %d: %s", outcome.code, outcome.body)
			}
		}
		answers := []string{removed}
		want := 0.0
		if !endsLate {
			end()
			ended, _ := read()
			answers = append(answers, ended)
		}
		for _, answer := range answers {
			if at := strings.Index(answer, `collector="gone"`); at >= 0 {
				t.Errorf("ends late %v: the removed collector is shown: %s", endsLate, answer[strings.LastIndexByte(answer[:at], '\n')+1:at+strings.IndexByte(answer[at:], '\n')])
			}
		}
		time.Sleep(2 * time.Millisecond)
		back := time.Now().UnixMilli()
		r.reloadTo(createdDocument(true, "kept", "gone"))
		if endsLate {
			// Another timeout makes it another probe, which does not wait for
			// the one under way.
			if code := probeOnce(t, r.server, probePath("gone", target.URL, "&timeout=5s"), nil).Code; code != http.StatusOK {
				t.Fatalf("the probe of the collector brought back was answered %d", code)
			}
			end()
			want = 1
		}
		body, times := read()
		request := `{collector="gone",http_method="GET",url="` + target.URL + `/status"}`
		for _, series := range []string{
			`http_exporter_collector_scrape_duration_seconds_count{collector="gone"}`,
			`http_exporter_scrapes_total{collector="gone"}`,
			`http_exporter_decode_success_total{collector="gone"}`,
			`http_exporter_metrics_emitted_total{collector="gone"}`,
			`http_exporter_request_series_tracked`,
		} {
			if got := seriesValue(t, body, series); got != want {
				t.Errorf("ends late %v: %s is %v, want %v", endsLate, series, got, want)
			}
		}
		if tracked := strings.Contains(body, "http_exporter_scrapes_total"+request+" 1\n"); tracked != endsLate || strings.Contains(body, "http_exporter_scrapes_total"+request+" 2\n") {
			t.Errorf("ends late %v: the request of the collector brought back:\n%s", endsLate, body)
		}
		series := []string{`http_exporter_collector_scrape_duration_seconds_created{collector="gone"}`, `http_exporter_scrapes_created{collector="gone"}`}
		if endsLate {
			series = append(series, "http_exporter_scrapes_created"+request)
		}
		for _, name := range series {
			if got, written := times[name]; !written || got < back {
				t.Errorf("ends late %v: %s is %d (written: %v), want it no earlier than the reload at %d that brought the collector back", endsLate, name, got, written, back)
			}
		}
		strictlyRead(t, append(answers, body)...)
	}
}

// The _created of a per-request series does not move while the request is
// tracked, and is later for a request that was dropped and is tracked again,
// whatever a probe that began early and ends late does:
//
//   - with no drop, the request counts since the probe that ended first
//     began, and the earlier probe adds its count without moving that;
//   - after a drop, with verbose mode switched off and on, an earlier probe
//     that is the first to end starts the request again as of then, not as of
//     when it began, which is before the time the dropped request showed;
//   - and one that ends after another probe started the request again adds
//     its count without moving that probe's time.
func TestARequestsCreatedNeverMovesEarlier(t *testing.T) {
	for _, tc := range []struct {
		name            string
		drop, probedNew bool
	}{
		{"no drop", false, false},
		{"dropped, and the early probe ends first", true, false},
		{"dropped, and probed again before the early probe ends", true, true},
	} {
		target, arrived, release := slowFirstTarget(t)
		r := newReloadable(t, createdDocument(true, "kept"), "")
		var mu sync.Mutex
		now := time.Unix(1_700_000_000, 0)
		r.server.requests.now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
		step := func() int64 {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(time.Minute)
			return now.UnixMilli()
		}
		request := `{collector="kept",http_method="GET",url="` + target.URL + `/status"}`
		var answers []string
		read := func() (float64, int64) {
			_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
			answers = append(answers, body)
			if !strings.Contains(body, "http_exporter_scrapes_total"+request) {
				return 0, 0
			}
			return seriesValue(t, body, "http_exporter_scrapes_total"+request), createdTimes(t, body)["http_exporter_scrapes_created"+request]
		}
		// Another timeout makes a probe another one than the early probe,
		// which does not wait for it, of the same request.
		probe := func() {
			if code := probeOnce(t, r.server, probePath("kept", target.URL, "&timeout=5s"), nil).Code; code != http.StatusOK {
				t.Fatalf("%s: a probe was answered %d", tc.name, code)
			}
		}

		early := probeAsync(context.Background(), r.server, probePath("kept", target.URL, ""), nil)
		<-arrived
		second := step()
		probe()
		value, created := read()
		if value != 1 || created != second {
			t.Fatalf("%s: the request is at %v, created %d, want 1 and the time %d of the probe that ended", tc.name, value, created, second)
		}
		want, wantValue := created, 2.0
		if tc.drop {
			r.reloadTo(createdDocument(false, "kept"))
			if value, created := read(); value != 0 || created != 0 {
				t.Fatalf("%s: without verbose mode the request is still shown, at %v", tc.name, value)
			}
			r.reloadTo(createdDocument(true, "kept"))
			want, wantValue = step(), 1
			if tc.probedNew {
				probe()
				if value, created := read(); value != 1 || created != want {
					t.Fatalf("%s: probed again the request is at %v, created %d, want 1 and that probe's time %d", tc.name, value, created, want)
				}
				wantValue = 2
				step()
			}
		} else {
			step()
		}
		release()
		if outcome := <-early; outcome.code != http.StatusOK {
			t.Fatalf("%s: the early probe was answered %d: %s", tc.name, outcome.code, outcome.body)
		}
		for range 2 {
			if value, created := read(); value != wantValue || created != want {
				t.Errorf("%s: after the early probe ended the request is at %v, created %d, want %v and %d", tc.name, value, created, wantValue, want)
			}
		}
		if tc.drop && want <= second {
			t.Errorf("%s: the request tracked again is created %d, not after the %d it showed before", tc.name, want, second)
		}
		strictlyRead(t, answers...)
	}
}

// A request whose first probe shares its trip (probeflight.go) counts since
// that probe began, on a clock that moves with every reading too.
func TestARequestCountsSinceItsFirstProbeBeganWhateverItsTripCounts(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := verboseServer(t, true, testutil.Collector("kept", "text"))
	server.logger = testutil.QuietLogger(t)
	setCreatedTimestamps(server, true)
	now := time.Unix(1_700_000_000, 0)
	server.requests.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	request := `{collector="kept",http_method="GET",url="` + target.URL + `"}`
	if got := createdTimes(t, body)["http_exporter_scrapes_created"+request]; got != 1_700_000_001_000 || seriesValue(t, body, "http_exporter_scrapes_total"+request) != 1 {
		t.Fatalf("the request is created %d, want the clock's first reading, when its probe began:\n%s", got, body)
	}
}

// A collector a reload adds while the self-metrics are being read, after the
// collectors' counters were taken, has its histogram's _created in that very
// answer, and the same one in the next, where its counters have it too.
func TestAHistogramAddedDuringAReadHasItsCreatedAtOnce(t *testing.T) {
	kept, added := testutil.Collector("kept", "text"), testutil.Collector("added", "text")
	server := verboseServer(t, true, kept)
	server.logger = testutil.QuietLogger(t)
	setCreatedTimestamps(server, true)
	read := func() (string, map[string]int64) {
		_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
		return body, createdTimes(t, body)
	}
	before := time.Now().UnixMilli()
	// The request tracker reads its clock between the two.
	var once sync.Once
	server.requests.now = func() time.Time {
		once.Do(func() { reloadTo(t, server, kept, added) })
		return time.Now()
	}
	first, times := read()
	const histogram = `http_exporter_collector_scrape_duration_seconds_created{collector="added"}`
	if !strings.Contains(first, `http_exporter_collector_scrape_duration_seconds_count{collector="added"} 0`) || strings.Contains(first, `http_exporter_scrapes_total{collector="added"}`) {
		t.Fatalf("the reload did not come between the counters and the histograms of one answer:\n%s", first)
	}
	created, written := times[histogram]
	if !written || created < before || created > time.Now().UnixMilli() {
		t.Fatalf("the histogram of the collector added during the read is created %d (written: %v), want the time of the read, from %d:\n%s", created, written, before, first)
	}
	second, times := read()
	if times[histogram] != created || times[`http_exporter_scrapes_created{collector="added"}`] != created {
		t.Errorf("at the next read the histogram is created %d and the counters %d, want both %d as before", times[histogram], times[`http_exporter_scrapes_created{collector="added"}`], created)
	}
	// Over OTLP the histogram's points start at that time from the first.
	for _, m := range server.selfMetricSet().Metrics {
		if m.Name == "http_exporter_collector_scrape_duration_seconds" && m.Labels["collector"] == "added" && m.Created != created {
			t.Errorf("the histogram's creation time is %d in the set OTLP exports, want %d", m.Created, created)
		}
	}
	strictlyRead(t, first, second)
}

// What the exporter reads from targets has no creation time, with the
// setting or without: a probe's answer and the static targets endpoint's are
// byte for byte the same in both formats, counters, histograms and summaries
// included, while the self-metrics of the same server have their _created
// samples.
func TestProbeAndStaticTargetAnswersAreTheSameWithCreatedTimestamps(t *testing.T) {
	exposition := "# TYPE requests_total counter\nrequests_total{code=\"200\"} 7\n" +
		"# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"1\"} 2\nlatency_seconds_bucket{le=\"+Inf\"} 3\nlatency_seconds_sum 4.5\nlatency_seconds_count 3\n" +
		"# TYPE size_bytes summary\nsize_bytes{quantile=\"0.5\"} 10\nsize_bytes_sum 30\nsize_bytes_count 3\n"
	target := utf8Target(t, exposition)
	cfg := &model.Config{Collectors: []model.Collector{passthrough("pass", "", "")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "pass", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	type key struct{ path, accept string }
	paths := []string{probePath("pass", target.URL, ""), "/static-targets"}
	read := func() map[key]string {
		answers := map[key]string{}
		for _, path := range paths {
			for _, accept := range []string{"", prometheus2Accept} {
				contentType, body := answerOf(t, server, path, accept)
				answers[key{path, accept}] = contentType + "\n" + body
			}
		}
		return answers
	}
	off := read()
	setCreatedTimestamps(server, true)
	on := read()
	for k, want := range off {
		if on[k] != want {
			t.Errorf("%s with Accept %q changes with the setting:\n%s\nwithout:\n%s", k.path, k.accept, on[k], want)
		}
		if strings.Contains(on[k], "_created") || !strings.Contains(on[k], "requests_total{") || !strings.Contains(on[k], "latency_seconds_count") || !strings.Contains(on[k], "size_bytes_count") {
			t.Errorf("%s with Accept %q:\n%s", k.path, k.accept, on[k])
		}
	}
	wantOM := openMetricsType1 + "\n# TYPE requests counter\nrequests_total{code=\"200\"} 7\n" +
		"# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"1.0\"} 2\nlatency_seconds_bucket{le=\"+Inf\"} 3\nlatency_seconds_sum 4.5\nlatency_seconds_count 3\n" +
		"# TYPE size_bytes summary\nsize_bytes{quantile=\"0.5\"} 10\nsize_bytes_sum 30\nsize_bytes_count 3\n# EOF\n"
	if got := on[key{paths[0], prometheus2Accept}]; got != wantOM {
		t.Errorf("the probe's OpenMetrics answer with the setting:\n%s\nwant:\n%s", got, wantOM)
	}
	if got := on[key{paths[0], ""}]; got != expositionContentType+"\n"+exposition {
		t.Errorf("the probe's text answer with the setting:\n%s\nwant:\n%s", got, exposition)
	}
	if _, self := answerOf(t, server, "/self-metrics", prometheus2Accept); !strings.Contains(self, `http_exporter_scrapes_created{collector="pass"} `) {
		t.Errorf("the self-metrics of the server have no _created sample:\n%s", self)
	}
}

// An exporter that probes another's self-metrics, as OpenMetrics, reads the
// answer with _created samples without an error, and gives the very series it
// gives when the other writes none.
func TestAChainedExporterReadsTheSelfMetricsWithCreatedSamples(t *testing.T) {
	target := textTarget(t, "value=42\n")
	inner := createdServer(t, true, false)
	probeOnce(t, inner, probePath("kept", target.URL, ""), nil)
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Uncompressed, so that what was served can be read below.
		r.Header.Del("Accept-Encoding")
		recorder := httptest.NewRecorder()
		inner.Handler().ServeHTTP(recorder, r)
		served = append(served, recorder.Header().Get("Content-Type")+"\n"+recorder.Body.String())
		maps.Copy(w.Header(), recorder.Header())
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(upstream.Close)

	chained := passthrough("chained", "", "")
	chained.Request.Headers = map[string]string{"Accept": "application/openmetrics-text"}
	outer := verboseServer(t, false, chained)
	outer.logger = testutil.QuietLogger(t)
	probe := func() string {
		_, body := answerOf(t, outer, probePath("chained", upstream.URL+"/self-metrics", ""), "")
		return body
	}
	on := probe()
	setCreatedTimestamps(inner, false)
	off := probe()

	if len(served) != 2 || !strings.HasPrefix(served[0], "application/openmetrics-text") || !strings.Contains(served[0], "_created") || strings.Contains(served[1], "_created") {
		t.Fatalf("the inner exporter served:\n%s", strings.Join(served, "\n"))
	}
	if on != off {
		t.Fatalf("the chained exporter gives other series for the answer with _created samples:\n%s\nwithout:\n%s", on, off)
	}
	if strings.Contains(on, "_created") {
		t.Errorf("the chained exporter passes a _created sample on:\n%s", on)
	}
	for _, want := range []string{
		`http_exporter_scrapes_total{collector="kept"} 1`,
		`http_exporter_collector_scrape_duration_seconds_count{collector="kept"} 1`,
		`http_exporter_python_pool_runs_total{outcome="ok"} 0`,
	} {
		if !strings.Contains(on, want+"\n") {
			t.Errorf("the chained exporter's answer lacks %s:\n%s", want, on)
		}
	}
}

// Over OTLP a cumulative point of the exporter's own series starts at the
// series' creation time, with the setting or without, since OTLP has a field
// for it; a series read from a target, which has no such time, starts when it
// was first exported, as before.
func TestOTLPStartsTheExportersOwnSeriesAtTheirCreation(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := createdServer(t, false, true)
	probed := time.Unix(1_700_000_000, 250_000_000)
	server.requests.now = func() time.Time { return probed }
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)

	starts := newOTLPStartTimes()
	set := server.selfMetricSet()
	set.Metrics = append(set.Metrics, model.Metric{Name: "target_requests_total", Type: model.CounterMetricType, Value: 3})
	const now = "1800000000000000000"
	started := strconv.FormatInt(exporterStart().UnixMilli()*int64(time.Millisecond), 10)
	kinds := map[string]int{}
	for _, m := range otlpMetrics(set, now, starts.forResource("r")) {
		type point struct {
			attributes []otlpAttribute
			start      string
		}
		var points []point
		switch {
		case m.Sum != nil:
			kinds["sum"]++
			for _, p := range m.Sum.DataPoints {
				points = append(points, point{p.Attributes, p.StartTimeUnixNano})
			}
		case m.Histogram != nil:
			kinds["histogram"]++
			for _, p := range m.Histogram.DataPoints {
				points = append(points, point{p.Attributes, p.StartTimeUnixNano})
			}
		case m.Summary != nil:
			kinds["summary"]++
			for _, p := range m.Summary.DataPoints {
				points = append(points, point{p.Attributes, p.StartTimeUnixNano})
			}
		case m.Gauge != nil:
			for _, p := range m.Gauge.DataPoints {
				if p.StartTimeUnixNano != "" {
					t.Errorf("the gauge %s has the start time %s", m.Name, p.StartTimeUnixNano)
				}
			}
		}
		for _, p := range points {
			want := started
			switch {
			case m.Name == "target_requests_total":
				want = now
			case slices.ContainsFunc(p.attributes, func(a otlpAttribute) bool { return a.Key == "url" && a.Value.StringValue != "http://static.example" }):
				want = "1700000000250000000"
			case slices.ContainsFunc(p.attributes, func(a otlpAttribute) bool { return a.Key == "url" }):
				continue
			}
			if p.start != want {
				t.Errorf("%s %v starts at %s, want %s", m.Name, p.attributes, p.start, want)
			}
		}
	}
	if kinds["sum"] < 20 || kinds["histogram"] != 1 || kinds["summary"] != 1 {
		t.Errorf("the export has %v, which does not cover every kind", kinds)
	}
	// Only the series read from a target needed its start remembered.
	if len(starts.series) != 1 {
		t.Errorf("%d series' start times are remembered, want only the target's", len(starts.series))
	}
}
