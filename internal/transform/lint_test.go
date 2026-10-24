package transform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// lintCollectorOf is a jq collector with a rule of each name and type given as
// "name:type".
func lintCollectorOf(name string, rules ...string) model.Collector {
	c := model.Collector{Name: name, Transform: model.TransformConfig{Type: "jq"}}
	for _, rule := range rules {
		metric, typ, _ := strings.Cut(rule, ":")
		c.Metrics = append(c.Metrics, model.MetricRule{Name: metric, Type: model.MetricType(typ), Expression: ".x"})
	}
	return c
}

// lintRuleIDs are the rules of findings about metric, in order.
func lintRuleIDs(findings []LintFinding, metric string) []string {
	var out []string
	for _, f := range findings {
		if f.Metric == metric {
			out = append(out, f.Rule)
		}
	}
	return out
}

func lintOne(t *testing.T, name string, typ model.MetricType) []LintFinding {
	t.Helper()
	c := lintCollectorOf("app", name+":"+string(typ))
	return LintCollector(&c, nil)
}

// counter-total: a counter not named _total, whose name differs between the
// text format and OpenMetrics, and a gauge named _total.
func TestLintCounterTotal(t *testing.T) {
	for _, test := range []struct {
		name  string
		typ   model.MetricType
		fires bool
	}{
		{"jobs_done", model.CounterMetricType, true},
		{"jobs_done_total", model.CounterMetricType, false},
		{"jobs_totals", model.CounterMetricType, true},
		{"queue_total", model.GaugeMetricType, true},
		{"queue_totals", model.GaugeMetricType, false},
		{"queue", model.GaugeMetricType, false},
		{"jobs_done", model.UntypedMetricType, false},
		{"requests_total", model.UntypedMetricType, false},
	} {
		findings := lintOne(t, test.name, test.typ)
		if got := slices.Contains(lintRuleIDs(findings, test.name), model.LintCounterTotal); got != test.fires {
			t.Errorf("%s %s: counter-total fired %v, want %v: %v", test.typ, test.name, got, test.fires, findings)
		}
	}
	findings := lintOne(t, "jobs_done", model.CounterMetricType)
	if want := "is written as jobs_done in the Prometheus text format and as jobs_done_total in OpenMetrics"; !strings.Contains(findings[0].Message, want) || !strings.Contains(findings[0].Message, "name it jobs_done_total") {
		t.Fatalf("message %q", findings[0].Message)
	}
}

// unit-suffix: a unit that is not a base unit, and a name that reads as a
// duration, a time or a size without its base unit, each with the name it
// should have; the near misses give nothing.
func TestLintUnitSuffix(t *testing.T) {
	for _, test := range []struct {
		name string
		typ  model.MetricType
		// advice is a part of the message, "" for none.
		advice string
	}{
		{"request_latency_ms", model.GaugeMetricType, "scale: 0.001 and name it request_latency_seconds"},
		{"request_latency_milliseconds", model.GaugeMetricType, "name it request_latency_seconds"},
		{"request_latency_us", model.GaugeMetricType, "scale: 0.000001 and name it request_latency_seconds"},
		{"job_runtime_minutes", model.GaugeMetricType, "scale: 60 and name it job_runtime_seconds"},
		{"job_runtime_hours", model.GaugeMetricType, "scale: 3600"},
		{"heap_kb", model.GaugeMetricType, "scale: 1000 and name it heap_bytes"},
		{"heap_mb", model.GaugeMetricType, "scale: 1000000"},
		{"heap_gb", model.GaugeMetricType, "scale: 1000000000"},
		{"heap_kib", model.GaugeMetricType, "scale: 1024 and name it heap_bytes"},
		{"disk_used_percent", model.GaugeMetricType, "scale: 0.01 and name it disk_used_ratio"},
		{"cpu_ms_total", model.CounterMetricType, "name it cpu_seconds_total"},
		{"request_duration", model.HistogramMetricType, "a duration is in seconds, named request_duration_seconds"},
		{"process_uptime", model.GaugeMetricType, "named process_uptime_seconds"},
		{"cache_age", model.GaugeMetricType, "named cache_age_seconds"},
		{"last_success_time", model.GaugeMetricType, "a point in time a Unix time in seconds, named last_success_time_timestamp_seconds"},
		{"last_success_timestamp", model.GaugeMetricType, "named last_success_timestamp_seconds"},
		{"cpu_time_total", model.CounterMetricType, "named cpu_time_seconds_total"},
		{"heap_memory", model.GaugeMetricType, "a size is in bytes, named heap_memory_bytes"},
		// Near misses.
		{"request_latency_seconds", model.GaugeMetricType, ""},
		{"request_duration_seconds", model.HistogramMetricType, ""},
		{"last_success_timestamp_seconds", model.GaugeMetricType, ""},
		{"cpu_seconds_total", model.CounterMetricType, ""},
		{"heap_memory_bytes", model.GaugeMetricType, ""},
		{"memory_usage_ratio", model.GaugeMetricType, ""},
		{"request_timeouts_total", model.CounterMetricType, ""},
		{"request_timeout", model.GaugeMetricType, ""},
		{"queue_size", model.GaugeMetricType, ""},
		{"page_views_total", model.CounterMetricType, ""},
		{"storage_objects", model.GaugeMetricType, ""},
		{"runtime_threads", model.GaugeMetricType, ""},
		{"ms_teams_users", model.GaugeMetricType, ""},
		{"requests_us", model.GaugeMetricType, ""},
		{"pods_ns", model.GaugeMetricType, ""},
		{"heap_kbytes_free", model.GaugeMetricType, ""},
	} {
		findings := lintOne(t, test.name, test.typ)
		var advice []string
		for _, f := range findings {
			if f.Rule == model.LintUnitSuffix {
				advice = append(advice, f.Message)
			}
		}
		switch {
		case test.advice == "" && len(advice) > 0:
			t.Errorf("%s: unit-suffix fired: %v", test.name, advice)
		case test.advice != "" && (len(advice) != 1 || !strings.Contains(advice[0], test.advice)):
			t.Errorf("%s: unit-suffix gave %v, want one with %q", test.name, advice, test.advice)
		}
	}
}

// The configuration does not know what the series of a python script or of
// a prometheus rule without a name are called, so neither is linted before
// a run, and a prometheus rule without a type keeps the series' own.
func TestNamesNotKnownBeforeARunAreNotLinted(t *testing.T) {
	python := lintCollectorOf("py", "jobs_ms:counter")
	python.Transform.Type = "python"
	prometheus := model.Collector{Name: "prom", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{{Expression: "^app_"}, {Name: "jobs_done"}}}
	for _, c := range []model.Collector{python, prometheus} {
		if findings := LintCollector(&c, nil); len(findings) != 0 {
			t.Errorf("%s: %v", c.Name, findings)
		}
	}
	// A run shows the names, and they are linted then.
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "jobs_ms", Type: model.CounterMetricType, Value: 1}}}
	if got := lintRuleIDs(LintRun(&python, nil, set), "jobs_ms"); !slices.Equal(got, []string{model.LintCounterTotal, model.LintUnitSuffix}) {
		t.Fatalf("a run of the script gave %v", got)
	}
}

// The metrics_prefix is part of the name linted and named.
func TestTheExportedNameIsLinted(t *testing.T) {
	c := lintCollectorOf("app", "jobs:counter")
	c.MetricsPrefix = "vendor"
	findings := LintCollector(&c, nil)
	if len(findings) != 1 || findings[0].Metric != "vendor_jobs" || findings[0].Collector != "app" {
		t.Fatalf("%v", findings)
	}
}

func lintSeriesWith(metric, label string, values ...string) *model.MetricSet {
	set := &model.MetricSet{}
	for _, value := range values {
		set.Metrics = append(set.Metrics, model.Metric{Name: metric, Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{label: value, "host": "a"}})
	}
	return set
}

// unbounded-label: a label of more than MaxLintLabelValues values in one run,
// or with a value shaped as an id or a time; a label just within the bound
// and values that only come close give nothing.
func TestLintUnboundedLabel(t *testing.T) {
	many := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("user%d", i))
		}
		return out
	}
	for _, test := range []struct {
		what   string
		values []string
		advice string
	}{
		{"101 values", many(MaxLintLabelValues + 1), "has 101 distinct values in this run, more than 100"},
		{"a UUID", []string{"5f0c6c2e-8a5b-4c1e-9d3f-2b7a1e0c9d48"}, `"5f0c6c2e-8a5b-4c1e-9d3f-2b7a1e0c9d48" is a UUID`},
		{"16 hex digits", []string{"deadbeefcafe0123"}, "a hexadecimal string of 16 or more digits"},
		{"a git hash", []string{"0x9fceb02d0ae598e95dc970b74767f19372d61af8"}, "a hexadecimal string"},
		{"10 digits", []string{"1761300000"}, "a number of 10 or more digits"},
		{"an RFC 3339 time", []string{"2026-10-24T09:14:05Z"}, "is a date or a time"},
		{"a time with an offset", []string{"2026-10-24 09:14:05.123+03:00"}, "is a date or a time"},
		{"a date", []string{"2026-10-24"}, "is a date or a time"},
		// Near misses.
		{"100 values", many(MaxLintLabelValues), ""},
		{"15 hex digits", []string{"deadbeefcafe012"}, ""},
		{"16 letters", []string{"abcdefabcdefabcg"}, ""},
		{"9 digits", []string{"176130000"}, ""},
		{"a version", []string{"2026.10.24"}, ""},
		{"an address and port", []string{"10.0.0.7:9100"}, ""},
		{"a UUID's prefix", []string{"5f0c6c2e-8a5b"}, ""},
		{"a time of day", []string{"09:14:05"}, ""},
	} {
		c := lintCollectorOf("app", "sessions:gauge")
		findings := LintRun(&c, nil, lintSeriesWith("sessions", "user", test.values...))
		var advice []string
		for _, f := range findings {
			if f.Rule == model.LintUnboundedLabel {
				advice = append(advice, f.Message)
			}
		}
		switch {
		case test.advice == "" && len(advice) > 0:
			t.Errorf("%s: unbounded-label fired: %v", test.what, advice)
		case test.advice != "" && (len(advice) != 1 || !strings.Contains(advice[0], test.advice) || !strings.Contains(advice[0], "the label user ")):
			t.Errorf("%s: unbounded-label gave %v, want one about user with %q", test.what, advice, test.advice)
		}
	}
	// The configuration alone shows no values.
	c := lintCollectorOf("app", "sessions:gauge")
	if findings := LintCollector(&c, nil); len(findings) != 0 {
		t.Fatalf("%v", findings)
	}
}

// A value quoted is cut at 64 bytes, between two characters.
func TestLintUnboundedLabelQuotesAValueByItsStart(t *testing.T) {
	c := lintCollectorOf("app", "sessions:gauge")
	long := strings.Repeat("7", 100)
	findings := LintRun(&c, nil, lintSeriesWith("sessions", "user", long))
	if len(findings) != 1 || !strings.Contains(findings[0].Message, `"`+strings.Repeat("7", 64)+`..." is a number`) {
		t.Fatalf("%v", findings)
	}
	if got := lintShownValue(strings.Repeat("a", 63) + "é" + "b"); got != strings.Repeat("a", 63)+"..." {
		t.Fatalf("shown %q", got)
	}
}

// name-clash: one name made by two collectors with another type, help or
// labels, reported for each of the two; the same lintFamily in both, and a name
// one side cannot know, give nothing.
func TestLintNameClash(t *testing.T) {
	gauge := lintCollectorOf("a", "queue_depth:gauge")
	counter := lintCollectorOf("b", "queue_depth:counter")
	conf := &model.Config{Collectors: []model.Collector{gauge, counter}}
	lintClashes := lintOnlyRule(LintConfiguration(conf), model.LintNameClash)
	if len(lintClashes) != 2 || lintClashes[0].Collector != "a" || lintClashes[1].Collector != "b" ||
		!strings.Contains(lintClashes[0].Message, `collector "b" makes it too, as a counter, where this collector makes a gauge`) {
		t.Fatalf("%v", lintClashes)
	}

	help := []model.Collector{lintCollectorOf("a", "up:gauge"), lintCollectorOf("b", "up:gauge")}
	help[0].Metrics[0].Description = "Whether the target is up."
	help[1].Metrics[0].Description = "Whether the app answered."
	if got := LintConfiguration(&model.Config{Collectors: help}); len(got) != 2 || !strings.Contains(got[0].Message, `with the help "Whether the app answered.", where this collector's is "Whether the target is up."`) {
		t.Fatalf("help: %v", got)
	}
	// A rule without a description has no help to compare.
	help[1].Metrics[0].Description = ""
	if got := LintConfiguration(&model.Config{Collectors: help}); len(got) != 0 {
		t.Fatalf("help left out: %v", got)
	}

	labels := []model.Collector{lintCollectorOf("a", "up:gauge"), lintCollectorOf("b", "up:gauge")}
	labels[0].Metrics[0].Labels = []model.LabelRule{{Name: "region", Value: "eu"}}
	labels[1].Metrics[0].Labels = []model.LabelRule{{Name: "zone", Expression: ".zone"}}
	if got := LintConfiguration(&model.Config{Collectors: labels}); len(got) != 2 || !strings.Contains(got[0].Message, "with the labels [zone], where this collector's are [region]") {
		t.Fatalf("labels: %v", got)
	}
	// transform.labels count, and rename_labels and remove_labels as they
	// leave the names.
	labels[1].Transform.Labels = map[string]string{"site": "x"}
	labels[1].Transform.RenameLabels = map[string]string{"zone": "region"}
	labels[1].Transform.RemoveLabels = []string{"site"}
	if got := LintConfiguration(&model.Config{Collectors: labels}); len(got) != 0 {
		t.Fatalf("labels renamed alike: %v", got)
	}

	// Alike, and of names one side cannot know.
	same := []model.Collector{lintCollectorOf("a", "up:gauge"), lintCollectorOf("b", "up:gauge")}
	if got := LintConfiguration(&model.Config{Collectors: same}); len(got) != 0 {
		t.Fatalf("alike: %v", got)
	}
	python := lintCollectorOf("b", "up:counter")
	python.Transform.Type = "python"
	untyped := model.Collector{Name: "c", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{{Name: "up"}, {Expression: "^up$"}}}
	if got := LintConfiguration(&model.Config{Collectors: []model.Collector{lintCollectorOf("a", "up:gauge"), python, untyped}}); len(got) != 0 {
		t.Fatalf("names not known: %v", got)
	}
	// A prometheus rule with a type is compared by it, never by labels.
	typed := model.Collector{Name: "c", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{{Name: "up", Type: model.CounterMetricType}}}
	if got := lintOnlyRule(LintConfiguration(&model.Config{Collectors: []model.Collector{labels[0], typed}}), model.LintNameClash); len(got) != 2 || strings.Contains(got[0].Message, "with the labels") {
		t.Fatalf("typed prometheus rule: %v", got)
	}
	// A collector compared with a configuration that holds one of its name,
	// as one written on the debug page is, is not compared with that one.
	if got := LintCollector(&gauge, &model.Config{Collectors: []model.Collector{lintCollectorOf("a", "queue_depth:counter")}}); len(got) != 0 {
		t.Fatalf("own name: %v", got)
	}
}

// lint.ignore silences the rules it lists for its collector alone.
func TestLintIgnoreSilencesItsRules(t *testing.T) {
	a := lintCollectorOf("a", "jobs_ms:counter", "queue_depth:gauge")
	b := lintCollectorOf("b", "queue_depth:counter")
	set := lintSeriesWith("queue_depth", "session", "5f0c6c2e-8a5b-4c1e-9d3f-2b7a1e0c9d48")
	conf := &model.Config{Collectors: []model.Collector{a, b}}
	// Every rule fires here, which keeps each id of model.LintRules a rule
	// this package applies.
	if got := lintRuleSet(LintRun(&conf.Collectors[0], conf, set)); !slices.Equal(got, slices.Sorted(slices.Values(model.LintRules))) {
		t.Fatalf("unsilenced: %v", got)
	}
	for _, id := range model.LintRules {
		conf.Collectors[0].Lint.Ignore = []string{id}
		got := lintRuleSet(LintRun(&conf.Collectors[0], conf, set))
		if slices.Contains(got, id) || len(got) != len(model.LintRules)-1 {
			t.Errorf("ignoring %s: %v", id, got)
		}
	}
	// b's own clash stays.
	conf.Collectors[0].Lint.Ignore = model.LintRules
	if got := lintRuleSet(LintConfiguration(conf)); !slices.Equal(got, []string{model.LintCounterTotal, model.LintNameClash}) {
		t.Fatalf("b: %v", got)
	}
}

func lintOnlyRule(findings []LintFinding, id string) []LintFinding {
	return slices.DeleteFunc(slices.Clone(findings), func(f LintFinding) bool { return f.Rule != id })
}

func lintRuleSet(findings []LintFinding) []string {
	var out []string
	for _, f := range findings {
		if !slices.Contains(out, f.Rule) {
			out = append(out, f.Rule)
		}
	}
	slices.Sort(out)
	return out
}

// A finding of a name the configuration and the run both show is given once.
func TestALintRunRepeatsNoFinding(t *testing.T) {
	c := lintCollectorOf("app", "jobs:counter")
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "jobs", Type: model.CounterMetricType, Value: 1}}}
	if got := LintRun(&c, nil, set); len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if got := LintRun(&c, nil, set)[0].String(); !strings.HasPrefix(got, `[counter-total] metric "jobs": a counter whose name`) {
		t.Fatalf("%s", got)
	}
}

// A run of many metric names is linted in time linear in its series: the
// type of each name was found by a scan of every series, which made 50,000
// names take seconds. The old code takes far longer than the bound for
// lintManyFamilies names; the linear one takes a small part of it.
func TestALintRunIsLinearInItsSeries(t *testing.T) {
	const lintManyFamilies = 100000
	set := &model.MetricSet{Metrics: make([]model.Metric, lintManyFamilies)}
	for i := range set.Metrics {
		set.Metrics[i] = model.Metric{Name: fmt.Sprintf("app_metric_%d", i), Type: model.GaugeMetricType, Labels: map[string]string{"job": "app"}}
	}
	c := lintCollectorOf("app")
	start := time.Now()
	findings := LintRun(&c, nil, set)
	elapsed := time.Since(start)
	t.Logf("%d names linted in %s", lintManyFamilies, elapsed)
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings[:1])
	}
	if elapsed > 10*time.Second {
		t.Errorf("%d names took %s to lint, more than 10s: the lint of a run is not linear in its series", lintManyFamilies, elapsed)
	}
}

// The configuration's lint is linear in its collectors: each collector's
// names were compared with those of every other collector, worked out anew
// for each, so a configuration of thousands of collectors took seconds. The
// old code takes far longer than the bound for lintManyCollectors; the
// linear one takes a small part of it.
func TestAConfigurationLintIsLinearInItsCollectors(t *testing.T) {
	const lintManyCollectors = 10000
	conf := &model.Config{Collectors: make([]model.Collector, lintManyCollectors)}
	for i := range conf.Collectors {
		conf.Collectors[i] = lintCollectorOf(fmt.Sprintf("app%d", i), fmt.Sprintf("app_metric_%d:gauge", i), fmt.Sprintf("app_metric_%d_seconds:gauge", i))
	}
	start := time.Now()
	findings := LintConfiguration(conf)
	elapsed := time.Since(start)
	t.Logf("%d collectors linted in %s", lintManyCollectors, elapsed)
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings[:1])
	}
	if elapsed > 10*time.Second {
		t.Errorf("%d collectors took %s to lint, more than 10s: the lint of a configuration is not linear in its collectors", lintManyCollectors, elapsed)
	}
}
