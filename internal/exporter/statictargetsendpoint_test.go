package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The static targets endpoint (statictargetsendpoint.go).

func getStaticTargets(t *testing.T, server *Server, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", path, recorder.Code, recorder.Body)
	}
	if err := parseExposition(recorder.Body.Bytes()); err != nil {
		t.Fatalf("%s is not a valid exposition: %v\n%s", path, err, recorder.Body)
	}
	return recorder.Body.String()
}

// Every target's latest result is served on one endpoint, each series
// labelled with its target, with the target's health metrics, and without
// OTLP export being involved at all.
func TestTheStaticTargetsEndpointServesEveryTarget(t *testing.T) {
	up := textTarget(t, "value=42\n")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: up.URL, Labels: map[string]string{"region": "eu"}},
		{Name: "us", Collector: "text", Target: up.URL, Labels: map[string]string{"region": "us"}},
		{Name: "broken", Collector: "text", Target: down.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)

	if body := getStaticTargets(t, server, "/static-targets"); body != "" {
		t.Fatalf("before any scrape the endpoint served:\n%s", body)
	}
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := getStaticTargets(t, server, "/static-targets")
	for series, want := range map[string]float64{
		`demo_value{region="eu",static_target="eu"}`:                                                       42,
		`demo_value{region="us",static_target="us"}`:                                                       42,
		`http_exporter_target_up{collector="text",region="eu",static_target="eu",target="` + up.URL + `"}`: 1,
		`http_exporter_target_up{collector="text",static_target="broken",target="` + down.URL + `"}`:       0,
	} {
		if got := metricValue(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if strings.Count(body, "# TYPE demo_value ") != 1 || strings.Count(body, "# TYPE http_exporter_target_up ") != 1 {
		t.Errorf("a family's series are not together under one TYPE:\n%s", body)
	}
	if strings.Contains(body, `demo_value{static_target="broken"`) {
		t.Errorf("the failed target exported metrics:\n%s", body)
	}
	// None of the targets sets export_via_otlp: nothing waits for OTLP.
	if _, ok := pendingValue(server, "demo_value"); ok {
		t.Error("a target without export_via_otlp was queued for OTLP")
	}
}

// With export_via_otlp a target is queued for OTLP as well as served; without
// it, only served.
func TestExportViaOTLPAlsoQueuesTheTargetForOTLP(t *testing.T) {
	target := textTarget(t, "value=7\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "exported", Collector: "text", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"via": "otlp"}},
		{Name: "served", Collector: "text", Target: target.URL, Labels: map[string]string{"via": "endpoint"}},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	body := getStaticTargets(t, server, "/static-targets")
	for _, name := range []string{"exported", "served"} {
		if !strings.Contains(body, `static_target="`+name+`"`) {
			t.Errorf("%s is not on the endpoint:\n%s", name, body)
		}
	}
	var queued []string
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "demo_value" {
				queued = append(queued, m.Labels["via"])
			}
		}
	}
	if len(queued) != 1 || queued[0] != "otlp" {
		t.Fatalf("queued for OTLP: %v, want only the target with export_via_otlp", queued)
	}
}

// A family one target exports with another type than an earlier one is left
// out for that target, and logged; everything else of both is served.
func TestAFamilyTypeClashLeavesOutTheLaterTarget(t *testing.T) {
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}, nil)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	merged := server.mergeStaticTargets([]namedSet{
		{name: "a", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.GaugeMetricType, Value: 1}}}},
		{name: "b", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.CounterMetricType, Value: 2}, {Name: "own", Type: model.GaugeMetricType, Value: 3}}}},
	})
	var got []string
	for _, m := range merged.Metrics {
		got = append(got, m.Name+"/"+m.Labels["static_target"])
	}
	if strings.Join(got, " ") != "shared/a own/b" {
		t.Fatalf("merged %v, want a's shared and b's own", got)
	}
	if !strings.Contains(logs.String(), "static target metric left out of the static targets endpoint") || !strings.Contains(logs.String(), `"type_in_use":"gauge"`) {
		t.Fatalf("the clash was not logged:\n%s", logs)
	}
}

// A target removed from the file leaves the endpoint with the reload.
func TestARemovedTargetLeavesTheEndpoint(t *testing.T) {
	target := textTarget(t, "value=1\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	both := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "kept", Collector: "text", Target: target.URL},
		{Name: "removed", Collector: "text", Target: target.URL},
	}}
	server := newStaticServer(t, cfg, both)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	if body := getStaticTargets(t, server, "/static-targets"); !strings.Contains(body, `static_target="removed"`) {
		t.Fatalf("before the reload:\n%s", body)
	}
	one := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: both.Targets[:1]}
	if err := config.ValidateStaticTargets(one); err != nil {
		t.Fatal(err)
	}
	server.manager.SetTargets("", one)
	body := getStaticTargets(t, server, "/static-targets")
	if strings.Contains(body, `static_target="removed"`) || !strings.Contains(body, `static_target="kept"`) {
		t.Fatalf("after the reload:\n%s", body)
	}
}

// The endpoint is at --web.static-targets-path, protected like the
// self-metrics, and read with GET or HEAD only.
func TestTheStaticTargetsEndpointPathAndProtection(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		Web:        model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}},
	}
	server := newStaticServer(t, cfg, nil)
	server.SetStaticTargetsPath("/targets")
	handler := server.Handler()
	for path, want := range map[string]int{"/targets": http.StatusUnauthorized, "/static-targets": http.StatusNotFound} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != want {
			t.Errorf("%s answered %d, want %d", path, recorder.Code, want)
		}
	}
	for method, want := range map[string]int{http.MethodGet: http.StatusOK, http.MethodHead: http.StatusOK, http.MethodPost: http.StatusMethodNotAllowed} {
		request := httptest.NewRequest(method, "/targets", nil)
		request.SetBasicAuth("u", "p")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Errorf("%s /targets answered %d, want %d", method, recorder.Code, want)
		}
	}
}

func TestStaticTargetsPathIsChecked(t *testing.T) {
	for path, want := range map[string]string{"/static-targets": "/static-targets", "targets": "/targets", "/a/b": "/a/b"} {
		if got, err := StaticTargetsPath(path, "/self-metrics"); err != nil || got != want {
			t.Errorf("StaticTargetsPath(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"", "/", "/probe", "/collectors", "/self-metrics", "/x/", "/{a}", "/x/..", "/./x"} {
		if _, err := StaticTargetsPath(path, "/self-metrics"); err == nil {
			t.Errorf("StaticTargetsPath(%q) was accepted", path)
		}
	}
}

// A target's last successful scrape is served as a timestamp: 0 until one
// succeeds, and kept through later failures, so how old the values it serves
// are can be alerted on.
func TestTheLastSuccessOfAStaticTargetIsServed(t *testing.T) {
	var failing atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	series := `http_exporter_target_last_success_timestamp_seconds{collector="text",static_target="t",target="` + target.URL + `"}`

	failing.Store(true)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	if got := metricValue(t, getStaticTargets(t, server, "/static-targets"), series); got != 0 {
		t.Fatalf("before any success the timestamp is %v, want 0", got)
	}

	failing.Store(false)
	before := float64(time.Now().Unix())
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	succeeded := metricValue(t, getStaticTargets(t, server, "/static-targets"), series)
	if succeeded < before || succeeded > float64(time.Now().Unix()+1) {
		t.Fatalf("after a success the timestamp is %v, want about %v", succeeded, before)
	}

	failing.Store(true)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := getStaticTargets(t, server, "/static-targets")
	if got := metricValue(t, body, series); got != succeeded {
		t.Fatalf("after a failure the timestamp is %v, want the earlier success %v", got, succeeded)
	}
	if got := metricValue(t, body, `http_exporter_target_up{collector="text",static_target="t",target="`+target.URL+`"}`); got != 0 {
		t.Fatalf("up=%v after the failure", got)
	}
}

// A clash that goes away is said to have, and one that comes back is logged
// as new; a clash whose target is gone is forgotten without a recovery line.
func TestAFamilyTypeClashThatEndsIsLoggedAsEnded(t *testing.T) {
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}, nil)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	gauge := namedSet{name: "a", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.GaugeMetricType, Value: 1}}}}
	counter := namedSet{name: "b", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.CounterMetricType, Value: 2}}}}
	fixed := namedSet{name: "b", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.GaugeMetricType, Value: 2}}}}
	count := func(msg string) int { return strings.Count(logs.String(), `"msg":"`+msg+`"`) }
	const left, back = "static target metric left out of the static targets endpoint", "static target metric back on the static targets endpoint"

	server.mergeStaticTargets([]namedSet{gauge, counter})
	server.mergeStaticTargets([]namedSet{gauge, counter})
	if count(left) != 1 || count(back) != 0 {
		t.Fatalf("a clash on two reads logged %d warnings and %d recoveries, want 1 and 0:\n%s", count(left), count(back), logs)
	}
	if merged := server.mergeStaticTargets([]namedSet{gauge, fixed}); len(merged.Metrics) != 2 {
		t.Fatalf("once b agrees, both are served: %+v", merged.Metrics)
	}
	if count(back) != 1 || !strings.Contains(logs.String(), `"failures":2`) {
		t.Fatalf("the end of the clash was not logged with its count:\n%s", logs)
	}
	server.mergeStaticTargets([]namedSet{gauge, counter})
	if count(left) != 2 {
		t.Fatalf("a clash that came back was not logged afresh:\n%s", logs)
	}
	server.mergeStaticTargets([]namedSet{gauge})
	server.mergeStaticTargets([]namedSet{gauge, counter})
	if count(back) != 1 || count(left) != 3 {
		t.Fatalf("a clash whose target left was logged as recovered, or not forgotten: %d recoveries, %d warnings:\n%s", count(back), count(left), logs)
	}
}

func staticTargetsAnswer(t *testing.T, server *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// staticTargetsIn lists the static_target values of an answer, in order of
// first appearance.
func staticTargetsIn(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		_, rest, found := strings.Cut(line, `static_target="`)
		if !found || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(rest, `"`)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ?targets= narrows the endpoint to the targets it names, given separated by
// commas or as the parameter repeated, which is how Prometheus renders a
// scrape config's params; without it every target is served.
func TestTheStaticTargetsEndpointServesTheNamedTargets(t *testing.T) {
	target := textTarget(t, "value=5\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: target.URL},
		{Name: "us", Collector: "text", Target: target.URL},
		{Name: "asia", Collector: "text", Target: target.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	all := getStaticTargets(t, server, "/static-targets")
	for query, want := range map[string]string{
		"":                          "asia eu us",
		"?targets=eu":               "eu",
		"?targets=eu,us":            "eu us",
		"?targets=eu&targets=asia":  "asia eu",
		"?targets=+eu+,,us":         "eu us",
		"?targets=eu&targets=eu,eu": "eu",
		"?other=x&targets=asia":     "asia",
	} {
		body := getStaticTargets(t, server, "/static-targets"+query)
		if got := strings.Join(staticTargetsIn(body), " "); got != want {
			t.Errorf("%q served %q, want %q", query, got, want)
		}
		// What is served of a target is the same as without the filter:
		// every line of the narrowed answer is a line of the whole one.
		for _, line := range strings.Split(body, "\n") {
			if line != "" && !strings.Contains(all, line+"\n") {
				t.Errorf("%q served %q, which the whole endpoint does not", query, line)
			}
		}
		if strings.Count(body, "# TYPE demo_value ") != 1 {
			t.Errorf("%q: demo_value is not under one TYPE:\n%s", query, body)
		}
	}
	if head := staticTargetsAnswer(t, server, http.MethodHead, "/static-targets?targets=eu"); head.Code != http.StatusOK {
		t.Errorf("HEAD with targets: %d", head.Code)
	}
}

// A name no target has, and a parameter naming none, are refused with 400
// saying so, rather than answered with nothing, so a misspelt or removed
// target fails the scrape where it shows. A target that exists but has not
// been scraped yet is simply absent.
func TestTheStaticTargetsParameterRefusesUnknownNames(t *testing.T) {
	target := textTarget(t, "value=5\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: target.URL},
	}}
	server := newStaticServer(t, cfg, file)
	if body := getStaticTargets(t, server, "/static-targets?targets=eu"); body != "" {
		t.Fatalf("a target not scraped yet was served:\n%s", body)
	}
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	for query, want := range map[string]string{
		"?targets=eu,uss,apac": `no static target is named "apac", "uss"`,
		"?targets=":            "the targets parameter names no static target",
		"?targets=,+,":         "the targets parameter names no static target",
	} {
		answer := staticTargetsAnswer(t, server, http.MethodGet, "/static-targets"+query)
		if answer.Code != http.StatusBadRequest || !strings.Contains(answer.Body.String(), want) {
			t.Errorf("%q: %d %s, want 400 with %q", query, answer.Code, answer.Body, want)
		}
		if strings.Contains(answer.Body.String(), "demo_value") {
			t.Errorf("%q served metrics with its error", query)
		}
	}
}

// A narrowed read keeps a type clash as a whole read does: the later target
// still has the clashing family left out when it is the only one asked for,
// and the clash is not logged as ended and begun again between the two kinds
// of read.
func TestANarrowedReadKeepsTheClashAsAWholeReadDoes(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "a", Collector: "text", Target: "http://a.invalid"},
		{Name: "b", Collector: "text", Target: "http://b.invalid"},
	}}
	server := newStaticServer(t, cfg, file)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	server.publishStaticTarget(file.Targets[0], otlpResourceIdentity{}, model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.GaugeMetricType, Value: 1}}})
	server.publishStaticTarget(file.Targets[1], otlpResourceIdentity{}, model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.CounterMetricType, Value: 2}, {Name: "own", Type: model.GaugeMetricType, Value: 3}}})

	for _, query := range []string{"", "?targets=b", "", "?targets=b"} {
		body := getStaticTargets(t, server, "/static-targets"+query)
		if strings.Contains(body, `shared{static_target="b"}`) {
			t.Fatalf("%q served b's clashing family:\n%s", query, body)
		}
		if query != "" && (strings.Contains(body, `static_target="a"`) || !strings.Contains(body, `own{static_target="b"} 3`)) {
			t.Fatalf("%q served:\n%s", query, body)
		}
	}
	if n := strings.Count(logs.String(), "static target metric left out of the static targets endpoint"); n != 1 {
		t.Errorf("the clash was logged %d times over four reads, want once:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "back on the static targets endpoint") {
		t.Errorf("a narrowed read ended the clash:\n%s", logs)
	}
}

// Two targets of one collector, without labels or an OTLP identity of their
// own, share a resource. Over OTLP each series carries static_target, as on
// the endpoint, so neither target's values replace the other's.
func TestStaticTargetsSharingAResourceStayApartOverOTLP(t *testing.T) {
	eu, us := textTarget(t, "value=1\n"), textTarget(t, "value=2\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: eu.URL, ExportViaOTLP: true},
		{Name: "us", Collector: "text", Target: us.URL, ExportViaOTLP: true},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	values := map[string]float64{}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "demo_value" {
				values[m.Labels["static_target"]] = m.Value
			}
		}
	}
	if len(values) != 2 || values["eu"] != 1 || values["us"] != 2 {
		t.Fatalf("exported %v, want eu=1 and us=2", values)
	}
	// The endpoint serves both, each labelled once, from results stored
	// already labelled, so a read only merges and writes them.
	body := getStaticTargets(t, server, "/static-targets")
	for series, want := range map[string]float64{`demo_value{static_target="eu"}`: 1, `demo_value{static_target="us"}`: 2} {
		if got := metricValue(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	for _, result := range server.staticTargetResults() {
		if !result.labelled {
			t.Fatalf("the result of %s is not stored labelled", result.name)
		}
		for _, m := range result.set.Metrics {
			if m.Labels["static_target"] != result.name {
				t.Fatalf("the published result of %s is labelled %v", result.name, m.Labels)
			}
		}
	}
	// A read leaves the stored series as they were.
	before := server.staticTargetResults()[0].set.Metrics[0].Labels
	getStaticTargets(t, server, "/static-targets")
	if after := server.staticTargetResults()[0].set.Metrics[0].Labels; len(after) != len(before) || after["static_target"] != before["static_target"] {
		t.Fatalf("a read changed the stored series: %v -> %v", before, after)
	}
}

// The endpoint keeps its merge of the results between scrapes: a second read
// neither merges nor renders again, a published result or a change of the
// targets makes the next read merge again, and a filtered read of the kept
// merge serves only the targets it names, leaving the merge whole.
func TestTheStaticTargetsEndpointKeepsItsMergeBetweenScrapes(t *testing.T) {
	target := textTarget(t, "value=42\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: target.URL},
		{Name: "us", Collector: "text", Target: target.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	first := getStaticTargets(t, server, "/static-targets")
	view := server.staticView
	if view == nil || view.rendered == nil {
		t.Fatal("the read kept no rendered merge")
	}
	if again := getStaticTargets(t, server, "/static-targets"); again != first || server.staticView != view {
		t.Fatal("a second read with nothing published merged again")
	}
	filtered := getStaticTargets(t, server, "/static-targets?targets=eu")
	if strings.Contains(filtered, `static_target="us"`) || !strings.Contains(filtered, `static_target="eu"`) {
		t.Fatalf("the filtered read served:\n%s", filtered)
	}
	if server.staticView != view || getStaticTargets(t, server, "/static-targets") != first {
		t.Fatal("the filtered read changed the kept merge")
	}

	server.publishStaticTarget(file.Targets[0], otlpResourceIdentity{}, model.MetricSet{Metrics: []model.Metric{
		{Name: "demo_value", Type: model.GaugeMetricType, Value: 7},
	}})
	if body := getStaticTargets(t, server, "/static-targets"); !strings.Contains(body, `demo_value{static_target="eu"} 7`) || server.staticView == view {
		t.Fatalf("a published result is not served:\n%s", body)
	}
	view = server.staticView

	server.manager.SetTargets("", &model.StaticTargetFile{Interval: file.Interval, Targets: file.Targets[1:]})
	if body := getStaticTargets(t, server, "/static-targets"); strings.Contains(body, `static_target="eu"`) || server.staticView == view {
		t.Fatalf("a removed target is still served:\n%s", body)
	}
}
