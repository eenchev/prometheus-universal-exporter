//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// labelNameTarget is a target that answers every request with the body
// body holds, as Prometheus text exposition.
func labelNameTarget(t *testing.T) (string, *atomic.Pointer[string]) {
	t.Helper()
	var body atomic.Pointer[string]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(target.Close)
	return target.URL, &body
}

// A label name the target writes longer than limits.max_label_name_length,
// 200 bytes by default, fails the scrape at validation, as a metric name over
// its limit does: a prometheus pass-through answers 502 naming the metric,
// the label by its first 200 bytes and its length, and the limit, logs it,
// and counts it in http_exporter_series_limit_exceeded_total, and so does a
// Python script's label. error_handling, which a failed transform heeds,
// does not carry such a scrape on, for a label name as for a metric name. A
// name at the limit passes, and one over the default passes a raised limit.
func TestALabelNameOverItsLimitFailsTheScrape(t *testing.T) {
	address, body := labelNameTarget(t)
	over := "l" + strings.Repeat("a", alloctest.UnlessRaced(1<<20, 1<<16))
	failure := fmt.Sprintf(`metric "m" label name "%s"... (%d bytes) is longer than limits.max_label_name_length 200; rename the label with transform.rename_labels, or raise limits.max_label_name_length`, over[:200], len(over))
	const passthrough = `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}`
	for name, test := range map[string]struct {
		collector, body, want string
	}{
		"a pass-through": {passthrough + `}`, "m{" + over + `="v"} 1` + "\n", failure},
		"a pass-through carrying on after a failed transform": {passthrough + `, error_handling: {on_transform_error: log}}`, "m{" + over + `="v"} 1` + "\n", failure},
		"an over-long metric name, alike": {passthrough + `, error_handling: {on_transform_error: log}}`, "m" + over + " 1\n",
			fmt.Sprintf(`invalid metric name "m%s"... (%d bytes): longer than limits.max_metric_name_length 200`, over[:199], len(over)+1)},
		// A script's answer is bounded by limits.max_output_bytes, 1MiB
		// by default, so its label is of 300 bytes.
		"a Python script's label": {`{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "metric('m', value=1, labels={'l' + 'a' * (data['n'] - 1): 'v'})"}}`, `{"n": 300}`,
			fmt.Sprintf(`metric "m" label name "%s"... (300 bytes) is longer than limits.max_label_name_length 200`, over[:200])},
		"a histogram's own label": {passthrough + `, limits: {max_label_name_length: 1}}`, "# TYPE m histogram\nm_bucket{le=\"+Inf\"} 1\nm_sum 1\nm_count 1\n",
			`metric "m" is a histogram, whose buckets carry the label le, which is longer than limits.max_label_name_length 1; raise limits.max_label_name_length`},
	} {
		t.Run(name, func(t *testing.T) {
			server, logs := errorLengthServer(t, test.collector)
			body.Store(&test.body)
			got := probeOnce(t, server, probePath("c", address, ""), nil)
			if got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), test.want) {
				t.Fatalf("answered %d %.600q, want 502 with %.600q", got.Code, got.Body, test.want)
			}
			if lines := linesOf(t, logs, "probe failed"); len(lines) != 1 || lines[0]["stage"] != "validation" || !strings.Contains(fmt.Sprint(lines[0]["error"]), test.want) {
				t.Errorf("logged %.600v", lines)
			}
			metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
			if counted := metricValue(t, metrics, `http_exporter_series_limit_exceeded_total{collector="c"}`); counted != 1 {
				t.Errorf("counted %v scrapes over a limit, want 1", counted)
			}
		})
	}
	for name, test := range map[string]struct{ collector, body string }{
		"a name at the limit":         {passthrough + `}`, "m{" + over[:200] + `="v"} 1` + "\n"},
		"a name under a raised limit": {passthrough + fmt.Sprintf(`, limits: {max_label_name_length: %d}}`, len(over)), "m{" + over + `="v"} 1` + "\n"},
		"a histogram at the least":    {passthrough + `, limits: {max_label_name_length: 2}}`, "# TYPE m histogram\nm_bucket{le=\"+Inf\"} 1\nm_sum 1\nm_count 1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := errorLengthServer(t, test.collector)
			body.Store(&test.body)
			if got := probeOnce(t, server, probePath("c", address, ""), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "m") {
				t.Fatalf("answered %d %.600q", got.Code, got.Body)
			}
		})
	}
}

// A scrape whose label names are all within the limit answers byte for byte
// what it answers with a limit no name comes near: over answers of many
// families, labels, histograms and summaries, at the default limit, the
// limit at the longest name, and one far over it.
func TestAScrapeWithinTheLabelNameLimitAnswersAsWithoutIt(t *testing.T) {
	address, body := labelNameTarget(t)
	answers := []string{
		"# HELP up Up.\n# TYPE up gauge\nup 1\n",
		"m{a=\"1\",b=\"2\"} 1\nm{a=\"2\",b=\"2\"} 2\nn{" + strings.Repeat("k", 200) + "=\"v\"} 3\n",
		"# TYPE h histogram\nh_bucket{x=\"1\",le=\"1\"} 1\nh_bucket{x=\"1\",le=\"+Inf\"} 2\nh_sum{x=\"1\"} 3\nh_count{x=\"1\"} 2\n",
		"# TYPE s summary\ns{quantile=\"0.5\",zone=\"eu\"} 1\ns_sum{zone=\"eu\"} 2\ns_count{zone=\"eu\"} 3\n",
		"a{" + strings.Repeat("x", 199) + "=\"\",y=\"1\"} 1\n",
	}
	const passthrough = `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}`
	answered := map[string]string{}
	for _, limits := range []string{"", ", limits: {max_label_name_length: 200}", ", limits: {max_label_name_length: 1000000}"} {
		server, _ := errorLengthServer(t, passthrough+limits+"}")
		for i := range answers {
			body.Store(&answers[i])
			got := probeOnce(t, server, probePath("c", address, ""), nil)
			if got.Code != http.StatusOK {
				t.Fatalf("answer %d with %q: %d %s", i, limits, got.Code, got.Body)
			}
			// The scrape's own duration is the one line that differs
			// from one scrape to the next.
			var kept []string
			for _, line := range strings.Split(got.Body.String(), "\n") {
				if !strings.Contains(line, "scrape_duration") {
					kept = append(kept, line)
				}
			}
			text := strings.Join(kept, "\n")
			if before, ok := answered[answers[i]]; ok && before != text {
				t.Errorf("answer %d with %q is\n%s\nwas\n%s", i, limits, text, before)
			}
			answered[answers[i]] = text
		}
	}
}

// The scrape holds a label name to limits.max_label_name_length after
// transform.rename_labels and remove_labels, and so does the load: a label
// over the limit that a rename makes short, or a removal takes off, is
// served, whether the target writes it to a pass-through or the collector's
// own rule does, and so is a long transform.labels key renamed. The rules'
// were refused at load, by the name as written, though their scrape passed.
func TestALongLabelNameRenamedOrRemovedIsServed(t *testing.T) {
	address, body := labelNameTarget(t)
	long := strings.Repeat("l", 201)
	for name, test := range map[string]struct{ collector, body, want string }{
		"a pass-through's label renamed": {`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus, rename_labels: {` + long + `: short}}}`,
			"m{" + long + `="v"} 1` + "\n", `m{short="v"} 1`},
		"a rule's label renamed": {`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus, rename_labels: {` + long + `: short}}, metrics: [{name: n, expression: m, labels: [{name: ` + long + `, value: v}]}]}`,
			"m 1\n", `n{short="v"} 1`},
		"a rule's label removed": {`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus, remove_labels: [` + long + `]}, metrics: [{name: n, expression: m, labels: [{name: ` + long + `, value: v}]}]}`,
			"m 1\n", "n 1"},
		"a transform.labels key renamed": {`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus, labels: {` + long + `: v}, rename_labels: {` + long + `: short}}}`,
			"m 1\n", `m{short="v"} 1`},
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := errorLengthServer(t, test.collector)
			body.Store(&test.body)
			got := probeOnce(t, server, probePath("c", address, ""), nil)
			if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), test.want+"\n") {
				t.Fatalf("answered %d %.300q; want %q", got.Code, got.Body, test.want)
			}
		})
	}
}

// The labels the exporter adds to a static target's series after the
// scrape's validation — static_target on every series, collector and target
// beside it on its health series — are held to the collector's
// limits.max_label_name_length when the target file is checked against the
// configuration: under 13, the length of static_target, the target is
// refused, where it loaded and the endpoint served a label name the
// collector's validation refuses; at 13 what the endpoint serves of the
// target, its health series among them, passes that validation.
func TestTheLabelsTheExporterAddsToAStaticTargetAreHeldToItsCollectorsLimit(t *testing.T) {
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "c", Target: "http://t.invalid"}}}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{8, 12, 13} {
		cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", fmt.Sprintf("collectors:\n  - {name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}, limits: {max_label_name_length: %d}}\n", limit)))
		if err != nil {
			t.Fatal(err)
		}
		err = config.ValidateStaticTargetsAgainst(file, cfg)
		if limit < 13 {
			if want := fmt.Sprintf(`target "t" gets the label static_target, 13 bytes, on every series of it, longer than limits.max_label_name_length %d of collector "c"`, limit); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("limit %d: error %v, want %q", limit, err, want)
			}
			continue
		}
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		c := &cfg.Collectors[0]
		served := model.MetricSet{Metrics: []model.Metric{{Name: "m", Type: model.GaugeMetricType, Value: 1}}}
		served.Metrics = append(served.Metrics, staticTargetHealthMetrics(file.Targets[0], c, 1, 0.5, time.Now()).Metrics...)
		served = withStaticTargetLabel(served, "t")
		if err := served.Validate(c.Limits); err != nil {
			t.Errorf("limit %d: what the endpoint serves of the target fails the collector's validation: %v", limit, err)
		}
	}
}
