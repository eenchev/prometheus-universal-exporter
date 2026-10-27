//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// answerBytes is what the answer counters of every endpoint hold, read from
// the self-metric set rather than through the self-metrics endpoint, which
// would count its own answer: written and sent by endpoint label.
func answerBytes(t *testing.T, server *Server) (written, sent map[string]float64) {
	t.Helper()
	written, sent = map[string]float64{}, map[string]float64{}
	set := server.selfMetricSet()
	for _, metric := range set.Metrics {
		switch metric.Name {
		case "http_exporter_answer_bytes_total":
			written[metric.Labels["endpoint"]] = metric.Value
		case "http_exporter_answer_sent_bytes_total":
			sent[metric.Labels["endpoint"]] = metric.Value
		}
	}
	if len(written) != 3 || len(sent) != 3 {
		t.Fatalf("answer counters: written %v, sent %v; want a series of each for probe, self_metrics and static_targets", written, sent)
	}
	return written, sent
}

// answerGrowth is how much each counter grew from before to after, by
// endpoint label, those that did not left out.
func answerGrowth(before, after map[string]float64) map[string]int {
	out := map[string]int{}
	for endpoint, value := range after {
		if grown := int(value - before[endpoint]); grown != 0 {
			out[endpoint] = grown
		}
	}
	return out
}

func sameGrowth(got map[string]int, endpoint string, want int) bool {
	return len(got) == 1 && got[endpoint] == want
}

// A probe answered uncompressed adds the length of its answer to both
// counters of the probe endpoint; one answered to Accept-Encoding: gzip adds
// the length of the plain answer to http_exporter_answer_bytes_total and the
// length of what the client received, compressed, to
// http_exporter_answer_sent_bytes_total. HEAD and an answer the handler
// refused are counted as they were written, uncompressed. Nothing else
// moves.
func TestAnswerBytesCountTheAnswerBeforeAndAfterCompression(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := flightServer(t, testutil.Collector("text", "text"))
	probe := probePath("text", target.URL, "")

	for _, step := range []struct {
		name, method, path, acceptEncoding string
		compressed                         bool
	}{
		{"plain", http.MethodGet, probe, "", false},
		{"gzip", http.MethodGet, probe, "gzip, deflate, br", true},
		{"refused gzip", http.MethodGet, probe, "gzip;q=0", false},
		{"HEAD", http.MethodHead, probe, "gzip", false},
		{"no collector, gzip", http.MethodGet, "/probe?target=" + target.URL, "gzip", true},
		{"POST", http.MethodPost, probe, "", false},
	} {
		writtenBefore, sentBefore := answerBytes(t, server)
		answer := get(server, step.method, step.path, step.acceptEncoding)
		received := answer.Body.Len()
		plain := received
		if step.compressed {
			if answer.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("%s: not compressed", step.name)
			}
			plain = len(gunzip(t, answer.Body.Bytes()))
		} else if answer.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s: compressed", step.name)
		}
		if received == 0 || (step.compressed && received == plain) {
			t.Fatalf("%s: %d bytes received, %d plain", step.name, received, plain)
		}
		writtenAfter, sentAfter := answerBytes(t, server)
		if got := answerGrowth(writtenBefore, writtenAfter); !sameGrowth(got, "probe", plain) {
			t.Errorf("%s: http_exporter_answer_bytes_total grew by %v, want %d for the probe endpoint alone", step.name, got, plain)
		}
		if got := answerGrowth(sentBefore, sentAfter); !sameGrowth(got, "probe", received) {
			t.Errorf("%s: http_exporter_answer_sent_bytes_total grew by %v, want %d for the probe endpoint alone", step.name, got, received)
		}
	}
}

// At --web.gzip-level 0 an answer is sent as it is to a client that
// accepts gzip too, and both counters grow by its length.
func TestAnswerBytesAtGzipLevelZeroAreTheBytesSent(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := flightServer(t, testutil.Collector("text", "text"))
	server.SetGzipLevel(0)
	writtenBefore, sentBefore := answerBytes(t, server)
	answer := get(server, http.MethodGet, probePath("text", target.URL, ""), "gzip")
	if answer.Header().Get("Content-Encoding") != "" || !strings.Contains(answer.Body.String(), "demo_value 42") {
		t.Fatalf("Content-Encoding %q:\n%s", answer.Header().Get("Content-Encoding"), answer.Body.String())
	}
	writtenAfter, sentAfter := answerBytes(t, server)
	if got := answerGrowth(writtenBefore, writtenAfter); !sameGrowth(got, "probe", answer.Body.Len()) {
		t.Errorf("http_exporter_answer_bytes_total grew by %v, want %d for the probe endpoint", got, answer.Body.Len())
	}
	if got := answerGrowth(sentBefore, sentAfter); !sameGrowth(got, "probe", answer.Body.Len()) {
		t.Errorf("http_exporter_answer_sent_bytes_total grew by %v, want %d for the probe endpoint", got, answer.Body.Len())
	}
}

// The self-metrics count their own answers under self_metrics, and the
// static targets theirs under static_targets. An answer of the self-metrics
// was rendered before it was written: it does not hold its own bytes, which
// the next one does, exactly.
func TestTheSelfMetricsAndStaticTargetsCountTheirOwnAnswers(t *testing.T) {
	target := textTarget(t, "value=42\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)

	first := get(server, http.MethodGet, "/self-metrics", "")
	second := get(server, http.MethodGet, "/self-metrics", "gzip")
	third := get(server, http.MethodGet, "/self-metrics", "").Body.String()
	plainSecond := gunzip(t, second.Body.Bytes())
	for series, want := range map[string]int{
		`http_exporter_answer_bytes_total{endpoint="self_metrics"}`:      first.Body.Len() + len(plainSecond),
		`http_exporter_answer_sent_bytes_total{endpoint="self_metrics"}`: first.Body.Len() + second.Body.Len(),
	} {
		if got := seriesValue(t, third, series); got != float64(want) {
			t.Errorf("%s=%v after answers of %d and %d (%d compressed) bytes, want %d", series, got, first.Body.Len(), len(plainSecond), second.Body.Len(), want)
		}
		if got := seriesValue(t, first.Body.String(), series); got != 0 {
			t.Errorf("the first answer of the self-metrics holds %s=%v, its own bytes", series, got)
		}
	}

	server.scrapeStaticTargets(t.Context(), 0)
	writtenBefore, sentBefore := answerBytes(t, server)
	answer := get(server, http.MethodGet, DefaultStaticTargetsPath, "gzip")
	if !strings.Contains(gunzip(t, answer.Body.Bytes()), "demo_value") {
		t.Fatalf("the static targets answered:\n%s", answer.Body.String())
	}
	writtenAfter, sentAfter := answerBytes(t, server)
	if got := answerGrowth(writtenBefore, writtenAfter); !sameGrowth(got, "static_targets", len(gunzip(t, answer.Body.Bytes()))) {
		t.Errorf("http_exporter_answer_bytes_total grew by %v for the static targets", got)
	}
	if got := answerGrowth(sentBefore, sentAfter); !sameGrowth(got, "static_targets", answer.Body.Len()) {
		t.Errorf("http_exporter_answer_sent_bytes_total grew by %v for the static targets, want %d", got, answer.Body.Len())
	}
}

// Both counters are in the default self-metrics, without verbose mode, a
// series for each endpoint at 0 from the start, static_targets too without
// static targets, without a collector label, each family with one HELP and
// TYPE.
func TestAnswerBytesArePresentFromTheStart(t *testing.T) {
	server := flightServer(t, testutil.Collector("text", "text"))
	if server.manager.Get().Web.SelfMetrics.Verbose {
		t.Fatal("the test server is in verbose mode")
	}
	metrics := selfMetrics(t, server)
	for _, family := range []string{"http_exporter_answer_bytes_total", "http_exporter_answer_sent_bytes_total"} {
		want := "# HELP " + family + " " + exporterMetricHelp[family] + "\n# TYPE " + family + " counter\n" +
			family + `{endpoint="probe"} 0` + "\n" +
			family + `{endpoint="self_metrics"} 0` + "\n" +
			family + `{endpoint="static_targets"} 0` + "\n"
		if exporterMetricHelp[family] == "" || !strings.Contains(metrics, want) {
			t.Errorf("no\n%s\nin:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "http_exporter_answer_gzip_level") {
		t.Errorf("http_exporter_answer_gzip_level is published without verbose self-metrics:\n%s", metrics)
	}
}

// http_exporter_answer_gzip_level is --web.gzip-level, published only with
// verbose self-metrics.
func TestTheGzipLevelIsPublishedWithVerboseSelfMetricsOnly(t *testing.T) {
	for _, level := range []int{-1, 0, 6, 9} {
		for _, verbose := range []bool{false, true} {
			server := verboseServer(t, verbose, testutil.Collector("text", "text"))
			want := DefaultGzipLevel
			if level >= 0 {
				server.SetGzipLevel(level)
				want = level
			}
			metrics := selfMetrics(t, server)
			if !verbose {
				if strings.Contains(metrics, "http_exporter_answer_gzip_level") {
					t.Errorf("level %d: published without verbose self-metrics", level)
				}
				continue
			}
			series := "# HELP http_exporter_answer_gzip_level " + answerGzipLevelHelp + "\n# TYPE http_exporter_answer_gzip_level gauge\nhttp_exporter_answer_gzip_level " + strconv.Itoa(want) + "\n"
			if !strings.Contains(metrics, series) {
				t.Errorf("level %d: no\n%s\nin:\n%s", level, series, metrics)
			}
		}
	}
}

// The strict reference parser reads the new families in OpenMetrics, with
// their _created samples: counters of the families
// http_exporter_answer_bytes and http_exporter_answer_sent_bytes, and the
// verbose gauge.
func TestTheStrictOpenMetricsParserReadsTheAnswerBytes(t *testing.T) {
	target := textTarget(t, "value=42\n")
	server := createdServer(t, true, false)
	probeOnce(t, server, probePath("kept", target.URL, ""), nil)
	_, body := answerOf(t, server, "/self-metrics", prometheus2Accept)
	for _, want := range []string{
		"# TYPE http_exporter_answer_bytes counter\n",
		"# TYPE http_exporter_answer_sent_bytes counter\n",
		`http_exporter_answer_bytes_created{endpoint="static_targets"} `,
		"# TYPE http_exporter_answer_gzip_level gauge\n",
		"\nhttp_exporter_answer_gzip_level 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in:\n%s", want, body)
		}
	}
	verdict := strictParser(t, []string{body})[0]
	if verdict.Error != "" {
		t.Fatalf("the strict parser refuses the self-metrics: %s\n%s", verdict.Error, body)
	}
	read := map[string]string{}
	for _, s := range verdict.Samples {
		if strings.HasPrefix(s.Name, "http_exporter_answer_") {
			read[s.Name+"{"+s.Labels["endpoint"]+"}"] = s.Family + " " + s.Type
		}
	}
	for _, endpoint := range []string{"probe", "self_metrics", "static_targets"} {
		for _, family := range []string{"http_exporter_answer_bytes", "http_exporter_answer_sent_bytes"} {
			for _, suffix := range []string{"_total", "_created"} {
				if got := read[family+suffix+"{"+endpoint+"}"]; got != family+" counter" {
					t.Errorf("the strict parser read %s%s{endpoint=%q} as %q", family, suffix, endpoint, got)
				}
			}
		}
	}
	if got := read["http_exporter_answer_gzip_level{}"]; got != "http_exporter_answer_gzip_level gauge" {
		t.Errorf("the strict parser read http_exporter_answer_gzip_level as %q", got)
	}
}

// countsTheSelfMetricsAnswers reports whether a line of an exposition is a
// sample of the bytes of the self-metrics' own answers, which grow with each
// read of them, and by the length of the values of time read before; their
// _created samples do not change.
func countsTheSelfMetricsAnswers(line string) bool {
	return (strings.HasPrefix(line, "http_exporter_answer_bytes_total{") || strings.HasPrefix(line, "http_exporter_answer_sent_bytes_total{")) && strings.Contains(line, `endpoint="self_metrics"`)
}

// withoutTheSelfMetricsAnswers is an exposition without the samples of the
// bytes of the self-metrics' own answers, so two reads of the self-metrics
// can be compared.
func withoutTheSelfMetricsAnswers(exposition string) string {
	lines := strings.SplitAfter(exposition, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !countsTheSelfMetricsAnswers(line) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}
