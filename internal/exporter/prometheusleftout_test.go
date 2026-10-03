//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A sample line that is no part of its histogram or summary family failed
// the decode for a while, and with it every scrape of a target whose client
// library writes such lines, which Prometheus ingests. The line is left out
// and the rest is served (decode/promparse.go); the tests here probe what
// two such libraries write, and say what is counted and logged.

// micrometerTimer is what Micrometer's PrometheusMeterRegistry (the
// simpleclient one, up to 1.12, the default of Spring Boot up to 3.2) writes
// for a Timer with publishPercentiles and publishPercentileHistogram: one
// family of type histogram that holds the percentiles as samples named as
// the family, with a quantile label, before the buckets, and a _max gauge.
// percentiles says whether the percentile lines are written.
func micrometerTimer(percentiles bool) string {
	lines := []string{
		"# HELP jvm_threads_live_threads The current number of live threads including both daemon and non-daemon threads",
		"# TYPE jvm_threads_live_threads gauge",
		"jvm_threads_live_threads 23.0",
		"# HELP http_server_requests_seconds  ",
		"# TYPE http_server_requests_seconds histogram",
	}
	for _, s := range []struct{ uri, p50, p95, sum string }{{"/api/orders", "0.046137344", "0.184549376", "0.27"}, {"/api/users/{id}", "0.012058624", "0.050331648", "0.07"}} {
		tags := `exception="None",method="GET",outcome="SUCCESS",status="200",uri="` + s.uri + `",`
		if percentiles {
			lines = append(lines,
				"http_server_requests_seconds{"+tags+`quantile="0.5",} `+s.p50,
				"http_server_requests_seconds{"+tags+`quantile="0.95",} `+s.p95)
		}
		lines = append(lines,
			"http_server_requests_seconds_bucket{"+tags+`le="0.001",} 0.0`,
			"http_server_requests_seconds_bucket{"+tags+`le="0.05",} 2.0`,
			"http_server_requests_seconds_bucket{"+tags+`le="0.2",} 3.0`,
			"http_server_requests_seconds_bucket{"+tags+`le="+Inf",} 3.0`,
			"http_server_requests_seconds_count{"+tags+"} 3.0",
			"http_server_requests_seconds_sum{"+tags+"} "+s.sum)
	}
	lines = append(lines,
		"# HELP http_server_requests_seconds_max  ",
		"# TYPE http_server_requests_seconds_max gauge",
		`http_server_requests_seconds_max{exception="None",method="GET",outcome="SUCCESS",status="200",uri="/api/orders",} 0.18`)
	return strings.Join(lines, "\n") + "\n"
}

// victoriaMetricsHistogram is what github.com/VictoriaMetrics/metrics, v1.29
// to v1.40, writes with ExposeMetadata(true), as the VictoriaMetrics
// components do under -metrics.exposeMetadata: a HELP line without a text, a
// family of type histogram, and buckets labelled vmrange, with no le.
const victoriaMetricsHistogram = `# HELP go_goroutines
# TYPE go_goroutines gauge
go_goroutines 12
# HELP vm_rows_per_insert
# TYPE vm_rows_per_insert histogram
vm_rows_per_insert_bucket{type="promremotewrite",vmrange="1.000e+00...1.136e+00"} 3
vm_rows_per_insert_bucket{type="promremotewrite",vmrange="8.799e+00...1.000e+01"} 1
vm_rows_per_insert_sum{type="promremotewrite"} 13
vm_rows_per_insert_count{type="promremotewrite"} 4
# HELP vm_http_requests_total
# TYPE vm_http_requests_total counter
vm_http_requests_total{path="/api/v1/write"} 7
`

// changingTarget answers with the body it holds, which a test replaces.
func changingTarget(t *testing.T, body string) (*httptest.Server, *atomic.Pointer[string]) {
	t.Helper()
	var held atomic.Pointer[string]
	held.Store(&body)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(*held.Load()))
	}))
	t.Cleanup(target.Close)
	return target, &held
}

// passthroughWithLog is a server of one collector whose log, debug
// lines too, is returned.
func passthroughWithLog(t *testing.T, c model.Collector) (*Server, *bytes.Buffer) {
	t.Helper()
	server := verboseServer(t, false, c)
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return server, &logs
}

// leftOutWarnings counts the warnings about sample lines left out in a log.
func leftOutWarnings(logs *bytes.Buffer) int {
	return strings.Count(logs.String(), `"level":"WARN","msg":"sample lines left out"`)
}

// A Micrometer timer with percentiles and a histogram answered 502, and so
// did every other family of the application. It is served: the histogram
// with its buckets, _sum and _count, the gauges beside it, and the percentile
// lines, which are no part of a histogram, left out. They are counted in
// http_exporter_decoder_lines_skipped_total at every scrape and logged at
// warn level once, with the first line and what was expected, not once for
// each line nor again at the next scrape; when the target stops writing them
// the end is logged.
func TestAMicrometerTimerWithPercentilesIsServedWithoutThePercentileLines(t *testing.T) {
	target, body := changingTarget(t, micrometerTimer(true))
	server, logs := passthroughWithLog(t, passthrough("micrometer", "", ""))
	probe := "/probe?collector=micrometer&target=" + url.QueryEscape(target.URL)
	rest := `method="GET",outcome="SUCCESS",status="200",uri="/api/orders"`
	tags := `exception="None",` + rest
	for scrape := 1; scrape <= 2; scrape++ {
		r := probeOnce(t, server, probe, http.Header{"Accept": {"text/plain"}})
		answer := r.Body.String()
		if r.Code != http.StatusOK {
			t.Fatalf("status=%d: %s", r.Code, answer)
		}
		for _, want := range []string{
			"# TYPE http_server_requests_seconds histogram\n",
			`http_server_requests_seconds_bucket{exception="None",le="0.001",` + rest + `} 0` + "\n",
			`http_server_requests_seconds_bucket{exception="None",le="0.05",` + rest + `} 2` + "\n",
			`http_server_requests_seconds_bucket{exception="None",le="0.2",` + rest + `} 3` + "\n",
			`http_server_requests_seconds_bucket{exception="None",le="+Inf",` + rest + `} 3` + "\n",
			"http_server_requests_seconds_sum{" + tags + "} 0.27\n",
			"http_server_requests_seconds_count{" + tags + "} 3\n",
			`http_server_requests_seconds_sum{exception="None",method="GET",outcome="SUCCESS",status="200",uri="/api/users/{id}"} 0.07` + "\n",
			"# TYPE http_server_requests_seconds_max gauge\nhttp_server_requests_seconds_max{" + tags + "} 0.18\n",
			"# TYPE jvm_threads_live_threads gauge\njvm_threads_live_threads 23\n",
		} {
			if !strings.Contains(answer, want) {
				t.Errorf("scrape %d: the answer lacks %q:\n%s", scrape, want, answer)
			}
		}
		if strings.Contains(answer, "quantile") || strings.Contains(answer, "0.046137344") {
			t.Errorf("scrape %d: a percentile line was passed on:\n%s", scrape, answer)
		}
		if want := `http_exporter_decoder_lines_skipped_total{collector="micrometer"} ` + map[int]string{1: "4", 2: "8"}[scrape]; !strings.Contains(selfMetrics(t, server), want+"\n") {
			t.Errorf("scrape %d: the self-metrics lack %s", scrape, want)
		}
	}
	text := logs.String()
	if n := leftOutWarnings(logs); n != 1 ||
		!strings.Contains(text, `"error":"line 6: expected http_server_requests_seconds_bucket with an le label, http_server_requests_seconds_sum or http_server_requests_seconds_count as a sample of the histogram http_server_requests_seconds, got http_server_requests_seconds"`) ||
		!strings.Contains(text, `"collector":"micrometer"`) || !strings.Contains(text, `"left_out":4`) {
		t.Fatalf("%d warnings:\n%s", n, text)
	}
	if strings.Contains(selfMetrics(t, server), `http_exporter_parse_errors_total{collector="micrometer"} 1`) {
		t.Fatal("the decode is counted as failed")
	}
	without := micrometerTimer(false)
	body.Store(&without)
	if r := probeOnce(t, server, probe, nil); r.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", r.Code, r.Body.String())
	}
	if !strings.Contains(logs.String(), `"msg":"every sample line is part of its family again"`) || leftOutWarnings(logs) != 1 {
		t.Fatalf("no recovery, or a second warning:\n%s", logs.String())
	}
	if want := `http_exporter_decoder_lines_skipped_total{collector="micrometer"} 8`; !strings.Contains(selfMetrics(t, server), want+"\n") {
		t.Errorf("the self-metrics lack %s after a scrape that left nothing out", want)
	}
}

// A VictoriaMetrics histogram written with metadata answered 502 too: its
// buckets are labelled vmrange and have no le. The bucket lines are left
// out, counted and logged once, and the histogram is served with the _sum
// and the _count it has, and so with a +Inf bucket of that count, beside the
// other families; as OpenMetrics too.
func TestAVictoriaMetricsHistogramIsServedWithoutItsVmrangeBuckets(t *testing.T) {
	target := utf8Target(t, victoriaMetricsHistogram)
	server, logs := passthroughWithLog(t, passthrough("vm", "", ""))
	probe := "/probe?collector=vm&target=" + url.QueryEscape(target.URL)
	r := probeOnce(t, server, probe, http.Header{"Accept": {"text/plain"}})
	answer := r.Body.String()
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", r.Code, answer)
	}
	for _, want := range []string{
		"# TYPE go_goroutines gauge\ngo_goroutines 12\n",
		"# TYPE vm_rows_per_insert histogram\n" + `vm_rows_per_insert_bucket{le="+Inf",type="promremotewrite"} 4` + "\n",
		`vm_rows_per_insert_sum{type="promremotewrite"} 13` + "\n",
		`vm_rows_per_insert_count{type="promremotewrite"} 4` + "\n",
		"# TYPE vm_http_requests_total counter\n" + `vm_http_requests_total{path="/api/v1/write"} 7` + "\n",
	} {
		if !strings.Contains(answer, want) {
			t.Errorf("the answer lacks %q:\n%s", want, answer)
		}
	}
	if strings.Contains(answer, "vmrange") {
		t.Errorf("a vmrange bucket was passed on:\n%s", answer)
	}
	if want := `http_exporter_decoder_lines_skipped_total{collector="vm"} 2`; !strings.Contains(selfMetrics(t, server), want+"\n") {
		t.Errorf("the self-metrics lack %s", want)
	}
	text := logs.String()
	if n := leftOutWarnings(logs); n != 1 ||
		!strings.Contains(text, `"error":"line 6: expected vm_rows_per_insert_bucket with an le label, vm_rows_per_insert_sum or vm_rows_per_insert_count as a sample of the histogram vm_rows_per_insert, got vm_rows_per_insert_bucket without an le label"`) ||
		!strings.Contains(text, `"left_out":2`) {
		t.Fatalf("%d warnings:\n%s", n, text)
	}
	if r := probeOnce(t, server, probe, http.Header{"Accept": {prometheus3Accept}}); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `vm_rows_per_insert_count{type="promremotewrite"} 4`+"\n") || !strings.Contains(r.Body.String(), "go_goroutines 12\n") {
		t.Fatalf("OpenMetrics: status=%d: %s", r.Code, r.Body.String())
	}
}

// The plainest such lines, "h 5" under a histogram h and "s 5" under a
// summary s: each is left out, and since neither family has anything else it
// is not served at all, without a TYPE line of its own; the rest is, the two
// lines are counted, and one warning names the first.
func TestASampleNamedAsItsHistogramOrSummaryIsLeftOutOfAProbe(t *testing.T) {
	target := utf8Target(t, "# TYPE h histogram\nh 5\n# TYPE s summary\ns 5\n# TYPE up_thing gauge\nup_thing 1\n")
	server, logs := passthroughWithLog(t, passthrough("plain", "", ""))
	r := probeOnce(t, server, "/probe?collector=plain&target="+url.QueryEscape(target.URL), http.Header{"Accept": {"text/plain"}})
	if answer := r.Body.String(); r.Code != http.StatusOK || answer != "# TYPE up_thing gauge\nup_thing 1\n" {
		t.Fatalf("status=%d:\n%s", r.Code, answer)
	}
	if want := `http_exporter_decoder_lines_skipped_total{collector="plain"} 2`; !strings.Contains(selfMetrics(t, server), want+"\n") {
		t.Errorf("the self-metrics lack %s", want)
	}
	text := logs.String()
	if n := leftOutWarnings(logs); n != 1 || !strings.Contains(text, `"error":"line 2: expected h_bucket with an le label, h_sum or h_count as a sample of the histogram h, got h"`) || !strings.Contains(text, `"left_out":2`) {
		t.Fatalf("%d warnings:\n%s", n, text)
	}
}
