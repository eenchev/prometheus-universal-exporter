//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The bounds of a debug report (probedebug.go), with targets that speak
// HTTP: what a redirect, a target's headers and a response of long names
// make of a report, what a cut shows of what the report withholds, and the
// reports of the repository's examples.

// A redirect whose Location is most of a megabyte is followed with a request
// for that URL, which the report lists by its first 512 bytes and its
// length, as the failure of the request shows the URL, and with how the
// request ended after it: `2. GET http://…LLL... (921645 bytes) (redirect)
// -> 200 OK`. The report listed the URL whole, in a line of 900 kB, and is
// now a kilobyte or two. The query of the URL is masked before it is cut.
// Under the race detector the Location is 64 kB.
func TestARedirectToAURLOfMostOfAMegabyteIsListedByItsStart(t *testing.T) {
	testutil.CaptureLogs(t)
	size := alloctest.UnlessRaced(900<<10, 64<<10)
	const collector = `{name: c, request: {type: http, follow_redirects: true}, decoder: {type: json}, transform: {type: jq}, metrics: [{name: m, expression: .n}]}`
	refusing := refusingAddress(t)
	for name, tc := range map[string]struct {
		// location is where the redirect leads, given the target's URL, and
		// ended how the request for it ended.
		location func(base string) string
		ended    string
	}{
		"answered": {func(base string) string { return base + "/" }, " (redirect) -> 200 OK in "},
		"refused":  {func(string) string { return "http://" + refusing + "/" }, " (redirect) -> error: "},
	} {
		var location string
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(`{"n":1}`))
		}))
		t.Cleanup(target.Close)
		location = tc.location(target.URL) + strings.Repeat("L", size) + "?token=s3cret-query"
		server, _ := errorLengthServer(t, collector)
		trip := debugTripOf(t, server, "c", target.URL+"/", url.Values{})
		report, old := trip.report(), trip.old()
		shownURL := strings.TrimSuffix(location, "s3cret-query") + "<redacted>"
		line := "\n  2. GET " + shownURL[:debugURLLimit] + fmt.Sprintf("... (%d bytes)", len(shownURL)) + tc.ended
		if !strings.Contains(report, line) {
			t.Errorf("%s: the report does not list the redirect by the first %d bytes of its URL and its length:\n%.1500s", name, debugURLLimit, report)
		}
		if !strings.Contains(old, "\n  2. GET "+shownURL+tc.ended) {
			t.Errorf("%s: the report did not list the URL whole before", name)
		}
		if longest, long := longestLine(report); longest > mostCutLine || len(report) > 8<<10 || len(old) < size || strings.Contains(report, "s3cret") {
			t.Errorf("%s: the report is %d bytes, was %d, and its longest line is %d, %.80q", name, len(report), len(old), longest, long)
		}
		// The failure of the request quotes the URL, and shows as much of it.
		if quoted := fmt.Sprintf("Get %q... (%d bytes): dial tcp", shownURL[:debugURLLimit], len(shownURL)); tc.ended == " (redirect) -> error: " && !strings.Contains(report, quoted) {
			t.Errorf("%s: the failure does not show the URL as the list of requests does:\n%.1500s", name, report)
		}
	}
}

// What a target sends in its headers is bounded in the report: a value as
// long as a response's headers may be, a megabyte, is shown by its first
// 1,024 bytes and its length, and of five thousand headers the first
// hundred are listed and then how many the target sent. The report had the
// megabyte in one line and a line for every header. Under the race detector
// the headers are a thousand.
func TestTheHeadersATargetSendsAreBoundedInTheReport(t *testing.T) {
	testutil.CaptureLogs(t)
	const collector = `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: jq}, metrics: [{name: m, expression: .n}]}`
	const most = 1<<20 - 2048
	many := alloctest.UnlessRaced(5000, 1000)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/long" {
			w.Header().Set("X-Long", strings.Repeat("h", most))
		} else {
			for i := range many {
				w.Header().Set(fmt.Sprintf("X-Sent-%04d", i), "v")
			}
		}
		_, _ = w.Write([]byte(`{"n":1}`))
	}))
	t.Cleanup(target.Close)
	server, _ := errorLengthServer(t, collector)

	trip := debugTripOf(t, server, "c", target.URL+"/long", url.Values{})
	report, old := trip.report(), trip.old()
	assertContains(t, report, "\n    X-Long: "+strings.Repeat("h", debugHeaderValueLimit)+fmt.Sprintf("... (%d bytes)\n", most), "A probe would have answered 200 with 1 series.")
	if longest, long := longestLine(report); longest > 2*debugHeaderValueLimit || len(report) > 8<<10 || !strings.Contains(old, "\n    X-Long: "+strings.Repeat("h", most)+"\n") {
		t.Errorf("the report is %d bytes, was %d, and its longest line is %d, %.80q", len(report), len(old), longest, long)
	}

	// How many headers the target sends, counted as a client counts them.
	answer, err := http.Get(target.URL + "/many")
	if err != nil {
		t.Fatal(err)
	}
	_ = answer.Body.Close()
	sent := 0
	for _, values := range answer.Header {
		sent += len(values)
	}
	trip = debugTripOf(t, server, "c", target.URL+"/many", url.Values{})
	report, old = trip.report(), trip.old()
	_, after, _ := strings.Cut(report, "\n  Headers\n")
	listed, _, _ := strings.Cut(after, "\n  Body: ")
	lines := strings.Split(listed, "\n")
	if sent < many || len(lines) != debugHeaderLimit+1 || lines[len(lines)-1] != fmt.Sprintf("    ... (%d headers)", sent) {
		t.Fatalf("%d headers are listed in %d lines, the last %q", sent, len(lines), lines[len(lines)-1])
	}
	if strings.Count(old, "\n    X-Sent-") != many || strings.Count(report, "\n    X-Sent-") > debugHeaderLimit || len(report) > 8<<10 {
		t.Errorf("the report lists %d of the target's headers in %d bytes, and listed %d", strings.Count(report, "\n    X-Sent-"), len(report), strings.Count(old, "\n    X-Sent-"))
	}
}

// A collector whose limits allow a metric name and a label value of a
// megabyte answers a probe with them whole, and its debug report shows the
// name in the Transform section by its first 200 bytes and its length and
// the lines of the exposition by their first 8,192 bytes and theirs, under a
// sentence that says lines were cut and are no valid exposition as shown.
// The line that says what a probe would have answered, the lines of the
// exposition that are within the bound and the section's heading are as
// they were. The probe is untouched: its answer is what the report showed
// before, to the byte, and what it answers after the debug probe, from the
// cache the debug probe neither read nor filled. Under the race detector
// the name and the value are 64 kB.
func TestAProbeServesTheLongLinesItsDebugReportCuts(t *testing.T) {
	testutil.CaptureLogs(t)
	size := alloctest.UnlessRaced(1<<20, 64<<10)
	name, value := "n"+strings.Repeat("a", size), strings.Repeat("v", size)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(w, "# HELP short A series within every bound.\n# TYPE short gauge\nshort 1\n%s 2\nlabelled{l=%q} 3\n", name, value)
	}))
	t.Cleanup(target.Close)
	server, _ := errorLengthServer(t, `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}, cache: {ttl: 1m}, limits: {max_metric_name_length: 2097152, max_label_value_length: 2097152}}`)

	before := probeOnce(t, server, probePath("c", target.URL, ""), nil)
	if before.Code != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("the probe answered %d after %d requests: %.300s", before.Code, hits.Load(), before.Body)
	}
	served := before.Body.String()
	assertContains(t, served, "\n"+name+" 2\n", "\nlabelled{l=\""+value+"\"} 3\n")
	if err := parseExposition(before.Body.Bytes()); err != nil {
		t.Fatalf("the probe's answer is no exposition: %.300v", err)
	}

	trip := debugTripOf(t, server, "c", target.URL, url.Values{})
	report, old := trip.report(), trip.old()
	const heading = "\nMetrics a probe would have served\n"
	was, section, found := strings.Cut(old, heading)
	if !found || section != served {
		t.Fatalf("the report did not show the probe's answer under its heading before: %d bytes, and the probe served %d", len(section), len(served))
	}
	now, section, _ := strings.Cut(report, heading)
	const cut = "  Lines longer than 8192 bytes are cut below (3 of them), so this is not valid exposition as it stands; a probe serves them whole.\n"
	requireSame(t, "the exposition of the report", section, cut+cutAsDocumented(served))
	assertContains(t, section,
		"# HELP short A series within every bound.\n# TYPE short gauge\nshort 1\n",
		"\n# TYPE "+name[:lineLimit-len("# TYPE ")]+fmt.Sprintf("... (%d bytes)\n", len("# TYPE  untyped")+len(name)),
		"\n"+name[:lineLimit]+fmt.Sprintf("... (%d bytes)\n", len(name)+len(" 2")),
		"\nlabelled{l=\""+value[:lineLimit-len(`labelled{l="`)]+fmt.Sprintf("... (%d bytes)\n", len(`labelled{l=""} 3`)+len(value)),
	)
	if parseExposition([]byte(section)) == nil {
		t.Error("the section with its lines cut still reads as exposition, and says it does not")
	}
	// Above the exposition only the name's line of the Transform section
	// differs.
	requireSame(t, "the report above its exposition", now, strings.Replace(was, "\n    "+name+": 1\n", "\n    "+name[:debugNameLimit]+fmt.Sprintf("... (%d bytes): 1\n", len(name)), 1))
	assertContains(t, report, "Took 1.5s. A probe would have answered 200 with 3 series.\n", "\n    short: 1\n", "\n    labelled: 1\n")
	if longest, long := longestLine(report); longest > mostCutLine || len(report) > debugBodyLimit+64<<10 || len(old) < 4*size {
		t.Errorf("the report is %d bytes, was %d, and its longest line is %d, %.80q", len(report), len(old), longest, long)
	}

	after := probeOnce(t, server, probePath("c", target.URL, ""), nil)
	if after.Code != http.StatusOK || after.Body.String() != served {
		t.Errorf("after the debug probe the probe answers %d with %d bytes, and served %d before", after.Code, after.Body.Len(), len(served))
	}
	if hits.Load() != 2 {
		t.Errorf("the target was asked %d times, want once by the probe, once by the debug probe and not by the probe answered from the cache", hits.Load())
	}
}

// A cut shows nothing the report withholds, and none ends inside the
// <redacted> that stands for a credential. A request's URL is masked before
// it is cut at its 512 bytes: with the token of its query before the bound,
// across it at every byte, and after it, the report never has the token,
// and the URL shown ends with the name, with <redacted> whole, or before
// the name. The first line, with a target of eight kilobytes whose token is
// across the 8,192 bytes of a line, ends with <redacted> and the line's
// length. A header under a name that reads as a credential's is withheld
// whole wherever its value would have been cut.
func TestACutShowsNothingTheReportWithholds(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=s3cret-cookie-"+strings.Repeat("c", 4*debugHeaderValueLimit))
		_, _ = w.Write([]byte(`{"up":1,"temperature":21.5}`))
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	server.SetProbeDebug(true)
	partial := regexp.MustCompile(`<[a-z]{0,8}\.\.\. \(`)
	const mask = "<redacted>"

	// The request is for the target with the collector's query,
	// ?token=s3cret-query&view=full, which the report shows masked.
	for inside := -1; inside <= len(mask)+1; inside++ {
		path := "/" + strings.Repeat("p", debugURLLimit-inside-len(target.URL)-len("/?token="))
		report := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL+path)).Body.String()
		whole := target.URL + path + "?token=" + mask + "&view=" + mask
		head := debugURLLimit
		if inside > 0 && inside < len(mask) {
			head += len(mask) - inside
		}
		if want := "\n  1. GET " + whole[:head] + fmt.Sprintf("... (%d bytes) -> 200 OK in ", len(whole)); !strings.Contains(report, want) {
			t.Errorf("the bound %d bytes into the redaction: the request is not listed as %q:\n%.900s", inside, want[len(want)-80:], report)
		}
		if strings.Contains(report, "s3cret") || partial.MatchString(report) {
			t.Errorf("the bound %d bytes into the redaction: the report shows a credential, or a part of the word that stands for one: %q", inside, partial.FindString(report))
		}
		assertContains(t, report, "\n     Authorization: <redacted>\n", "\n    Set-Cookie: <redacted>\n", "A probe would have answered 200 with 1 series.")
	}

	// The target as the first line shows it has its own token masked.
	const first = `Debug probe of collector "dbg", target `
	for inside := 1; inside < len(mask); inside++ {
		path := "/" + strings.Repeat("p", lineLimit-inside-len(first)-len(target.URL)-len("/?token="))
		probed := target.URL + path + "?token=s3cret-target"
		if len(probed) > MaxProbeParameterBytes {
			t.Fatalf("the target is %d bytes, more than a probe's parameter may be", len(probed))
		}
		response := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(probed))
		report := response.Body.String()
		line, _, _ := strings.Cut(report, "\n")
		whole := first + target.URL + path + "?token=" + mask
		if want := whole + fmt.Sprintf("... (%d bytes)", len(whole)); response.Code != http.StatusOK || line != want {
			t.Errorf("the bound %d bytes into the redaction: answered %d with a first line ending %q, want %q", inside, response.Code, line[max(0, len(line)-60):], want[len(want)-60:])
		}
		if strings.Contains(report, "s3cret") || partial.MatchString(report) || !utf8.ValidString(report) {
			t.Errorf("the bound %d bytes into the redaction: the report shows a credential, or a part of the word that stands for one: %q", inside, partial.FindString(report))
		}
	}
}

// What a script printed is in the report whole: the 4,096 characters the
// worker keeps of it, and its note of how many more there were, are one log
// line of four kilobytes, which the report shows as it did. The bound of a
// line is past it.
func TestWhatAScriptPrintedIsInTheReportWhole(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"n":1}`)) }))
	t.Cleanup(target.Close)
	server, _ := errorLengthServer(t, `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "print('x' * 5000)\nmetric(name='m', type='gauge', value=1)"}}`)
	trip := debugTripOf(t, server, "c", target.URL, url.Values{})
	report := trip.report()
	requireSame(t, "a script that prints 5,000 characters", report, trip.old())
	assertContains(t, report, `msg="python transform printed" collector=c output="`+strings.Repeat("x", 4096)+`... (905 more characters)`, "A probe would have answered 200 with 1 series.")
}

// exampleProbe is a probe of a collector of one of the repository's
// examples at its stand-in.
type exampleProbe struct {
	name      string
	server    *Server
	collector string
	target    string
	query     url.Values
}

// The debug reports of the repository's examples, each probed at its
// stand-in with its fixture, are what they were, byte for byte: the nine
// examples, with each of their collectors and a second station, written by
// the writer as it was before a line had a bound and by the writer as it
// is.
func TestTheReportsOfTheExamplesAreWrittenAsTheyWere(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	var probes []exampleProbe
	add := func(name string, server *Server, target string, query url.Values, collectors ...string) {
		for _, collector := range collectors {
			probes = append(probes, exampleProbe{name: name + ", " + collector, server: server, collector: collector, target: target, query: query})
		}
	}
	serverOf := func(path string) *Server {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return NewServer(config.NewManager(cfg, path, slog.Default()), "python3", slog.Default())
	}

	ecb, ecbServer := newECB(t)
	add("ecb", ecbServer, ecb.URL, nil, "ecb_reference_rates")
	mempool, mempoolServer := newMempool(t, "917532")
	add("mempool", mempoolServer, mempool.URL, nil, "bitcoin_fees", "bitcoin_mempool", "bitcoin_chain")
	metar, metarCfg := newMETAR(t)
	metarServer := NewServer(config.NewManager(metarCfg, metarConfig, slog.Default()), "python3", slog.Default())
	add("metar", metarServer, metar.URL, nil, "metar")
	add("metar, another station", metarServer, metar.URL, url.Values{"param_station": {"KJFK"}}, "metar")
	usgs, usgsServer := newUSGS(t)
	add("usgs", usgsServer, usgs.URL, nil, "earthquakes")
	frankfurter, frankfurterCfg := newStandIn(t, frankfurterConfig, map[string]standInAnswer{
		frankfurterPath: {"application/json", readTestdata(t, "json/frankfurter-latest.json")},
	})
	add("frankfurter", NewServer(config.NewManager(frankfurterCfg, frankfurterConfig, slog.Default()), "python3", slog.Default()), frankfurter.URL, nil, "exchange_rates")
	promDemo, promDemoCfg := newStandIn(t, promDemoConfig, map[string]standInAnswer{
		"/metrics": {"text/plain; version=0.0.4; charset=utf-8", readTestdata(t, "prometheus/prometheus-server-metrics.prom")},
	})
	add("promdemo", NewServer(config.NewManager(promDemoCfg, promDemoConfig, slog.Default()), "python3", slog.Default()), promDemo.URL, nil, "prometheus_server")
	site, siteServer := newScrapeThisSite(t, htmlFixture(t, "scrapethissite-countries.html"))
	add("scrapethissite", siteServer, site.URL, nil, "countries_html")
	openMeteo, openMeteoCfg := newOpenMeteo(t)
	add("open-meteo", NewServer(config.NewManager(openMeteoCfg, openMeteoConfig, slog.Default()), "python3", slog.Default()), openMeteo.URL, nil, "open_meteo_current")
	filebeat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture := map[string]string{"/stats": "filebeat-stats.json", "/": "filebeat-info.json", "/inputs/": "filebeat-inputs.json"}[r.URL.Path]
		if fixture == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(readFixture(t, fixture))
	}))
	t.Cleanup(filebeat.Close)
	add("filebeat", serverOf(filebeatConfig), filebeat.URL, nil, "filebeat", "filebeat_info", "filebeat_inputs")

	if len(probes) != 14 {
		t.Errorf("%d probes of the examples, want 14", len(probes))
	}
	for _, p := range probes {
		trip := debugTripOf(t, p.server, p.collector, p.target, p.query)
		report := trip.report()
		requireSame(t, p.name, report, trip.old())
		assertContains(t, report, `Debug probe of collector "`+p.collector+`"`, "A probe would have answered 200 with ", "\nRequests\n  1. GET ", "\nStages\n", "\nLogs\n", "\nMetrics a probe would have served\n")
	}
}

// The collectors page asks for the report with the probe its form makes,
// debug=true beside the collector and the target, and is answered by the
// same writer: a header value of a megabyte is shown by its first 1,024
// bytes and its length, and the second line, which the page reads for what
// a probe would have answered, is as it was.
func TestTheCollectorsPagesDebugReportIsBoundedAsAProbesIs(t *testing.T) {
	testutil.CaptureLogs(t)
	size := alloctest.UnlessRaced(1<<20-2048, 1<<16)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Long", strings.Repeat("h", size))
		_, _ = w.Write([]byte(`{"up":1,"temperature":21.5}`))
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	server.SetProbeDebug(true)
	page := getPage(t, server, "/collectors")
	requireContains(t, page,
		`<form class="probe" action="probe" method="get" autocomplete="off">`,
		`<input type="hidden" name="collector" value="dbg">`,
		`type="text" name="target"`,
		`<input type="checkbox" name="debug" value="true" role="switch">`,
		`/would have answered (\d{3})/`,
	)
	// The request the page's script makes of the form's fields.
	form := url.Values{"collector": {"dbg"}, "target": {target.URL}, "debug": {"true"}}
	response := probeOnce(t, server, "/probe?"+form.Encode(), http.Header{"X-Prometheus-Scrape-Timeout-Seconds": {"30"}})
	report := response.Body.String()
	lines := strings.SplitN(report, "\n", 3)
	if response.Code != http.StatusOK || len(lines) != 3 || !regexp.MustCompile(`^Took \S+\. A probe would have answered 200 with 1 series\.$`).MatchString(lines[1]) {
		t.Fatalf("answered %d:\n%.400s", response.Code, report)
	}
	assertContains(t, report, "\n    X-Long: "+strings.Repeat("h", debugHeaderValueLimit)+fmt.Sprintf("... (%d bytes)\n", size))
	if longest, long := longestLine(report); longest > 2*debugHeaderValueLimit || len(report) > 8<<10 {
		t.Errorf("the report is %d bytes and its longest line %d, %.80q", len(report), longest, long)
	}
}
