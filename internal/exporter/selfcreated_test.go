package exporter

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter's own counters, histograms and summaries say when they began
// to count (selfcreated.go): as _created samples in an OpenMetrics answer of
// the self-metrics, with web.self_metrics.created_timestamps, and as the
// start time of their points over OTLP. Nothing else changes: not the text
// format, not a probe's answer nor the static targets endpoint's, and not the
// self-metrics without the setting.

// Counters that absorb another probe's keep their own creation time,
// whichever of the two began earlier: what they absorb is counted from then
// on, and a time that has been shown does not move.
func TestAbsorbLeavesTheCreationTimeAsItIs(t *testing.T) {
	early, late := time.Unix(100, 0), time.Unix(200, 0)
	for _, tc := range []struct {
		name          string
		kept, a, want time.Time
	}{
		{"an earlier probe", late, early, late},
		{"a later probe", early, late, early},
		{"a probe without a time", early, time.Time{}, early},
	} {
		kept := statsValues{created: tc.kept, probes: 1}
		kept.absorb(statsValues{created: tc.a, probes: 2})
		if !kept.created.Equal(tc.want) || kept.probes != 3 {
			t.Errorf("%s: created %v with %d probes, want %v with 3", tc.name, kept.created, kept.probes, tc.want)
		}
	}
}

// A request first probed by two probes at once counts since the one that
// ended first began: that is the time it is tracked with, and the other
// probe, though it began earlier, adds its counts without moving it.
func TestARequestOfTwoProbesCountsSinceTheOneThatEndedFirstBegan(t *testing.T) {
	tracker := newRequestTracker()
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	key := requestKey{Collector: "c", URL: "http://h", Method: "GET"}
	early := tracker.newStats()
	early.probes = 1
	now = now.Add(time.Second)
	late := tracker.newStats()
	late.probes = 1
	now = now.Add(time.Second)
	tracker.adopt(key, late, nil)
	first, _ := tracker.Snapshot()
	now = now.Add(time.Second)
	tracker.adopt(key, early, nil)
	samples, _ := tracker.Snapshot()
	want := time.Unix(1_700_000_001, 0)
	if len(first) != 1 || !first[0].Values.created.Equal(want) || first[0].Values.probes != 1 {
		t.Fatalf("tracked by the later probe, the request is %+v, want one probe since %v", first, want)
	}
	if len(samples) != 1 || !samples[0].Values.created.Equal(want) || samples[0].Values.probes != 2 {
		t.Fatalf("after the earlier probe ended the request is at %d since %v, want 2 since %v, as before", samples[0].Values.probes, samples[0].Values.created, want)
	}
}

// A request dropped and tracked again counts since a later time than it did,
// also when the probe that gets it tracked again began before the dropped
// request was tracked: its counts join the series when it ends, so the series
// counts since then. However a request is dropped - with verbose mode, for
// being idle, with its static target or with its collector - the rule is the
// same; a probe that began after every dropped request's time keeps its own.
func TestARequestTrackedAgainByAnOlderProbeCountsSinceThatProbeEnded(t *testing.T) {
	key := requestKey{Collector: "c", URL: "http://h", Method: "GET"}
	other := requestKey{Collector: "c", URL: "http://other", Method: "GET"}
	start := time.Unix(1_700_000_000, 0)
	for name, drop := range map[string]func(*requestTracker, *time.Time){
		"verbose mode switched off": func(tracker *requestTracker, _ *time.Time) { tracker.Reset() },
		"an hour without a probe": func(tracker *requestTracker, now *time.Time) {
			*now = now.Add(VerboseRequestIdleExpiry + time.Minute)
			tracker.expire()
		},
		"the collector removed": func(tracker *requestTracker, _ *time.Time) {
			tracker.forgetCollectors(map[string]bool{"c": true})
		},
		"the static target removed": func(tracker *requestTracker, _ *time.Time) {
			tracker.setStatic(map[requestKey]bool{})
		},
	} {
		tracker := newRequestTracker()
		now := start
		tracker.now = func() time.Time { return now }
		old := tracker.newStats() // a probe that runs all the while
		old.probes = 1
		now = now.Add(time.Minute)
		if name == "the static target removed" {
			tracker.setStatic(map[requestKey]bool{key: true})
		} else {
			tracker.adopt(key, tracker.newStats(), nil)
		}
		before, _ := tracker.Snapshot()
		drop(tracker, &now)
		if dropped, _ := tracker.Snapshot(); len(before) != 1 || len(dropped) != 0 {
			t.Fatalf("%s: %d requests tracked before and %d after, want 1 and 0", name, len(before), len(dropped))
		}
		now = now.Add(time.Minute)
		ended := now
		tracker.adopt(key, old, nil)
		// A probe of another request that began after the drop is tracked
		// with its own time, though it ends later.
		fresh := tracker.newStats()
		now = now.Add(time.Minute)
		tracker.adopt(other, fresh, nil)
		after, _ := tracker.Snapshot()
		if len(after) != 2 || !after[0].Values.created.Equal(ended) || after[0].Values.probes != 1 {
			t.Fatalf("%s: tracked again the request is %+v, want one probe since the probe ended at %v", name, after, ended)
		}
		if !after[0].Values.created.After(before[0].Values.created) {
			t.Errorf("%s: tracked again the request counts since %v, which is not after the %v it counted since before", name, after[0].Values.created, before[0].Values.created)
		}
		if !after[1].Values.created.Equal(ended) {
			t.Errorf("%s: the probe that began at %v after the drop is tracked since %v", name, ended, after[1].Values.created)
		}
	}
}

// A probe that ends after a reload removed its collector starts no request
// being tracked: the collector's requests were forgotten with it.
func TestAProbeOfARemovedCollectorStartsNoTrackedRequest(t *testing.T) {
	tracker := newRequestTracker()
	key := requestKey{Collector: "c", URL: "http://h", Method: "GET"}
	collector := newServerStats(time.Now())
	staged := tracker.newStats()
	collector.retired.Store(true)
	tracker.adopt(key, staged, collector)
	if samples, _ := tracker.Snapshot(); len(samples) != 0 {
		t.Fatalf("the probe of a removed collector is tracked: %+v", samples)
	}
	// Under the collector brought back it is tracked as ever.
	tracker.adopt(key, staged, newServerStats(time.Now()))
	if samples, _ := tracker.Snapshot(); len(samples) != 1 {
		t.Fatalf("%d requests tracked for a collector in the configuration, want 1", len(samples))
	}
}

// The writer puts a _created sample after a series with a creation time only
// where OpenMetrics has one: in a counter, histogram or summary family that
// keeps its type. It carries the series' labels and timestamp, and the time
// in seconds. A gauge has none, and neither has a family written as unknown,
// whose samples are named as in the text format.
func TestCreatedSamplesFollowTheSeriesOfFamiliesThatKeepTheirType(t *testing.T) {
	stamp := int64(1_727_000_000_500)
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "c_total", Type: model.CounterMetricType, Value: 3, Labels: map[string]string{"a": "b"}, Created: 1_700_000_000_123},
		{Name: "c_total", Type: model.CounterMetricType, Value: 4, Labels: map[string]string{"a": "c"}},
		{Name: "plain", Type: model.CounterMetricType, Value: 5, Created: 1_700_000_000_000, Timestamp: &stamp},
		{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}}, Sum: 3, Count: 2}, Created: 1_700_000_000_500},
		{Name: "bare", Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: math.Inf(1), CumulativeCount: 2}}, NoSum: true, NoCount: true}, Created: 1},
		{Name: "s", Type: model.SummaryMetricType, Labels: map[string]string{"op": "get"}, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: 2, Count: 3}, Created: 1_700_000_000_010},
		{Name: "g", Type: model.GaugeMetricType, Value: 1, Created: 1_700_000_000_000},
		{Name: "u", Type: model.UntypedMetricType, Value: 1, Created: 1_700_000_000_000},
		// A counter that is negative is written as unknown, and so is one
		// whose family name a gauge has.
		{Name: "odd_total", Type: model.CounterMetricType, Value: -1, Created: 1_700_000_000_000},
		{Name: "met_total", Type: model.CounterMetricType, Value: 1, Created: 1_700_000_000_000},
		{Name: "met", Type: model.GaugeMetricType, Value: 1},
	}}
	want := "# TYPE c counter\nc_total{a=\"b\"} 3\nc_created{a=\"b\"} 1700000000.123\nc_total{a=\"c\"} 4\n" +
		"# TYPE plain counter\nplain_total 5 1727000000.5\nplain_created 1700000000 1727000000.5\n" +
		"# TYPE h histogram\nh_bucket{le=\"1.0\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\nh_created 1700000000.5\n" +
		"# TYPE bare histogram\nbare_bucket{le=\"+Inf\"} 2\nbare_created 0.001\n" +
		"# TYPE s summary\ns{op=\"get\",quantile=\"0.5\"} 1\ns_sum{op=\"get\"} 2\ns_count{op=\"get\"} 3\ns_created{op=\"get\"} 1700000000.01\n" +
		"# TYPE g gauge\ng 1\n# TYPE u unknown\nu 1\n" +
		"# TYPE odd_total unknown\nodd_total -1\n# TYPE met_total unknown\nmet_total 1\n# TYPE met gauge\nmet 1\n# EOF\n"
	got := string(appendOpenMetrics(nil, set))
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	checkOpenMetricsClaims(t, got)
	if err := strictOpenMetricsError(got); err != nil {
		t.Fatalf("a strict parser refuses the answer: %v", err)
	}
	// The text format has no such sample.
	bare := model.CloneMetricSet(*set)
	withoutCreated(&bare)
	if with, without := string(appendMetricSet(nil, set)), string(appendMetricSet(nil, &bare)); with != without || strings.Contains(with, "_created") {
		t.Fatalf("the text format changes with a creation time:\n%s\nwithout:\n%s", with, without)
	}
}

// createdLine matches the _created sample the test below gives every series.
var createdLine = regexp.MustCompile(`(?m)^[^ {]+_created(\{[^\n]*\})? 1700000000\.123( [^ \n]+)?\n`)

// Sets of every type with odd values, and with families whose names meet
// through OpenMetrics' suffixes, whose every series has a creation time: the
// answer is still one a strict parser reads, and it is the answer without
// creation times plus _created lines, so a series without one, as every
// series of a probe is, is written as it was before the writer knew of them.
func TestCreatedSamplesKeepEveryAnswerOfOddValuesValid(t *testing.T) {
	written := 0
	for _, c := range oddCases(300) {
		with := model.CloneMetricSet(*c.set)
		for i := range with.Metrics {
			with.Metrics[i].Created = 1_700_000_000_123
		}
		without := string(appendOpenMetrics(nil, c.set))
		answer := string(appendOpenMetrics(nil, &with))
		if err := strictOpenMetricsError(answer); err != nil {
			t.Fatalf("%s: a strict parser refuses the answer: %v\n%s", c.name, err, answer)
		}
		if got := createdLine.ReplaceAllString(answer, ""); got != without {
			t.Fatalf("%s: the answer differs from the one without creation times in more than its _created lines:\n%s\nwithout:\n%s", c.name, answer, without)
		}
		if text := string(appendMetricSet(nil, &with)); text != string(appendMetricSet(nil, c.set)) {
			t.Fatalf("%s: the text format changes with a creation time:\n%s", c.name, text)
		}
		written += len(createdLine.FindAllString(answer, -1))
	}
	if written < 500 {
		t.Fatalf("only %d _created samples were written, so the cases do not cover them", written)
	}
}

// The strict reference parser, prometheus_client's, reads those answers too.
func TestTheStrictOpenMetricsParserReadsCreatedSamplesBesideOddValues(t *testing.T) {
	var answers []string
	for _, c := range oddCases(300) {
		with := model.CloneMetricSet(*c.set)
		for i := range with.Metrics {
			with.Metrics[i].Created = 1_700_000_000_123
		}
		answers = append(answers, string(appendOpenMetrics(nil, &with)))
	}
	for i, verdict := range strictParser(t, answers) {
		if verdict.Error != "" {
			t.Fatalf("the strict parser refuses an answer with _created samples: %s\n%s", verdict.Error, answers[i])
		}
	}
}
