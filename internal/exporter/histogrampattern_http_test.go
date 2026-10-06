//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// upAndEveryHistogram is a configuration of one prometheus collector whose
// rules are the metric up, and every histogram: a rule of a name beside a
// rule whose expression matches every name and whose type is histogram,
// with what the second rule says of its error_mode.
func upAndEveryHistogram(errorMode string) string {
	return "collectors:\n  - name: app\n    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n      - name: up\n      - expression: '.*'\n        type: histogram\n" + errorMode
}

// histogramsOfATarget are two histograms as a target exposes them, and as a
// probe answers them.
const histogramsOfATarget = "# TYPE request_duration_seconds histogram\nrequest_duration_seconds_bucket{le=\"0.5\"} 2\nrequest_duration_seconds_bucket{le=\"+Inf\"} 3\nrequest_duration_seconds_sum 1.5\nrequest_duration_seconds_count 3\n" +
	"# TYPE queue_wait_seconds histogram\nqueue_wait_seconds_bucket{le=\"1\"} 1\nqueue_wait_seconds_bucket{le=\"+Inf\"} 1\nqueue_wait_seconds_sum 0.25\nqueue_wait_seconds_count 1\n"

// upAndEveryHistogramServer is a server of that configuration, loaded as a
// file is, that logs at debug level into the buffer.
func upAndEveryHistogramServer(t *testing.T, errorMode string) (*Server, *bytes.Buffer) {
	t.Helper()
	testutil.CaptureLogs(t)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", upAndEveryHistogram(errorMode)))
	if err != nil {
		t.Fatalf("the configuration does not load: %v", err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return server, logs
}

// End to end: `name: up` beside `expression: '.*'` with `type: histogram`
// and `error_mode: ignore` is the metric up, and every histogram. The
// configuration loads, which it did not while a rule of a name beside a
// rule whose expression matches that name was refused whatever that rule
// sets; and a probe of a target that has a gauge up, a counter and two
// histograms is answered with up once and each histogram once, and without
// the counter, which neither rule is about. Under ignore nothing is logged
// at warning level or above: the second rule fails on up and on the
// counter, which are no histograms, and carries on without them.
//
// Under the default error_mode, log, the answer is the same, and the rule's
// failures on the two series are logged in one warning.
func TestANameAndEveryHistogramAreScrapedOnceEach(t *testing.T) {
	target := utf8Target(t, "# TYPE up gauge\nup{job=\"api\"} 1\n# TYPE requests_total counter\nrequests_total 7\n"+histogramsOfATarget)
	const want = "# TYPE up gauge\nup{job=\"api\"} 1\n" + histogramsOfATarget
	for errorMode, warnings := range map[string]int{"        error_mode: ignore\n": 0, "": 1} {
		server, logs := upAndEveryHistogramServer(t, errorMode)
		answer := probeOnce(t, server, "/probe?collector=app&target="+url.QueryEscape(target.URL), nil)
		if answer.Code != http.StatusOK || answer.Body.String() != want {
			t.Errorf("%q: the probe is answered %d:\n%s\nwant\n%s", errorMode, answer.Code, answer.Body, want)
		}
		logged := logs.String()
		if got := strings.Count(logged, `"level":"WARN"`); got != warnings || strings.Contains(logged, `"level":"ERROR"`) {
			t.Errorf("%q: %d warnings are logged, want %d, and no error:\n%s", errorMode, got, warnings, logged)
		}
		if warnings == 1 && (!strings.Contains(logged, `"msg":"metric extraction failed"`) || !strings.Contains(logged, `"error_mode":"log"`) || !strings.Contains(logged, `"failures":2`) || !strings.Contains(logged, "a histogram or summary keeps its own type, and no other series can become one")) {
			t.Errorf("%q: the warning is not of the rule's two failures:\n%s", errorMode, logged)
		}
	}
}

// The price of taking that pair: where the target's up is itself a
// histogram, the second rule makes it as the first does, and the probe is
// refused for the series it has twice, as a probe of two rules with
// expressions that both match a metric is. Under error_mode fail the rule
// would fail every scrape of a target that has any metric but a histogram,
// and the configuration is refused when it loads, as it was.
func TestANameAndEveryHistogramCollideOnAHistogramOfThatName(t *testing.T) {
	target := utf8Target(t, "# TYPE up histogram\nup_bucket{le=\"1\"} 1\nup_bucket{le=\"+Inf\"} 1\nup_sum 0.5\nup_count 1\n"+histogramsOfATarget)
	server, _ := upAndEveryHistogramServer(t, "        error_mode: ignore\n")
	answer := probeOnce(t, server, "/probe?collector=app&target="+url.QueryEscape(target.URL), nil)
	if want := `collector app validation failed: duplicate metric series "up"` + "\n"; answer.Code != http.StatusBadGateway || !strings.HasSuffix(answer.Body.String(), want) {
		t.Errorf("the probe is answered %d %q, want it refused with %q", answer.Code, answer.Body, want)
	}

	_, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", upAndEveryHistogram("        error_mode: fail\n")))
	if want := `collector "app" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike, so each makes every series of the metric,`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("under error_mode fail the load says %v\nwant %s", err, want)
	}
}
