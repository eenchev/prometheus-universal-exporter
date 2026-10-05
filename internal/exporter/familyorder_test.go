package exporter

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The text format wants the lines of a metric in one group. A set that has
// a metric's series apart is written with them together: the metrics in the
// order each first appears, each under one HELP and TYPE, those of its first
// series, with its series in the order the set has them, a histogram's and a
// summary's lines among them. Nothing is sorted. Such a set was written in
// its own order, a metric's later series without a HELP or TYPE after
// another metric's, which a strict reader of the format refuses.
func TestTheTextFormatWritesAMetricsSeriesTogether(t *testing.T) {
	at := int64(1700000000000)
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "host_cpu", Help: "CPU", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"host": "web02"}},
		{Name: "host_mem", Type: model.GaugeMetricType, Value: 2, Labels: map[string]string{"host": "web02"}},
		{Name: "host_cpu", Help: "another help", Type: model.GaugeMetricType, Value: 3, Labels: map[string]string{"host": "web01"}},
		{Name: "wait", Help: "Waits", Type: model.HistogramMetricType, Labels: map[string]string{"q": "b"}, Timestamp: &at,
			Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}}, Sum: 3, Count: 4}},
		{Name: "host_mem", Help: "Memory", Type: model.GaugeMetricType, Value: 4, Labels: map[string]string{"host": "web01"}},
		{Name: "took", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: 2, Count: 3}},
		{Name: "wait", Type: model.HistogramMetricType, Labels: map[string]string{"q": "a"},
			Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}}, Sum: 1, Count: 1}},
		{Name: "host_cpu", Type: model.GaugeMetricType, Value: 5},
		{Name: "requests_total", Type: model.CounterMetricType, Value: 6},
	}}
	const want = `# HELP host_cpu CPU
# TYPE host_cpu gauge
host_cpu{host="web02"} 1
host_cpu{host="web01"} 3
host_cpu 5
# TYPE host_mem gauge
host_mem{host="web02"} 2
host_mem{host="web01"} 4
# HELP wait Waits
# TYPE wait histogram
wait_bucket{le="1",q="b"} 2 1700000000000
wait_bucket{le="+Inf",q="b"} 4 1700000000000
wait_sum{q="b"} 3 1700000000000
wait_count{q="b"} 4 1700000000000
wait_bucket{le="1",q="a"} 1
wait_bucket{le="+Inf",q="a"} 1
wait_sum{q="a"} 1
wait_count{q="a"} 1
# TYPE took summary
took{quantile="0.5"} 1
took_sum 2
took_count 3
# TYPE requests_total counter
requests_total 6
`
	before := fmt.Sprint(set.Metrics)
	if got := string(appendMetricSet(nil, set)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// After text that is in the buffer already, which is kept.
	if got := string(appendMetricSet([]byte("# before\n"), set)); got != "# before\n"+want {
		t.Errorf("after other text: got\n%s", got)
	}
	if fmt.Sprint(set.Metrics) != before {
		t.Error("writing the set changed it")
	}
	if err := parseExposition([]byte(want)); err != nil {
		t.Errorf("the answer does not parse: %v", err)
	}
	// As it was written, the answer had the series in the set's order, a
	// metric's lines in three places.
	if was := string(formerAppendMetricSet(nil, set)); !strings.Contains(was, "host_mem{host=\"web02\"} 2\nhost_cpu{host=\"web01\"} 3\n") {
		t.Errorf("the former writer wrote\n%s", was)
	}
}

// seriesByFamily lists the places of a set's series metric by metric, the
// metrics in the order each first appears and each one's series in the
// set's order; of a set that has them so already, every place in turn.
func TestSeriesByFamilyKeepsTheOrderOfFirstAppearance(t *testing.T) {
	for names, want := range map[string][]int{
		"":              {},
		"a":             {0},
		"a a b b c":     {0, 1, 2, 3, 4},
		"a b a b":       {0, 2, 1, 3},
		"c b a c b a c": {0, 3, 6, 1, 4, 2, 5},
		"a b b a c a":   {0, 3, 5, 1, 2, 4},
	} {
		set := &model.MetricSet{}
		for _, name := range strings.Fields(names) {
			set.Metrics = append(set.Metrics, model.Metric{Name: name})
		}
		if got := seriesByFamily(set); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", names, got, want)
		}
	}
}

// formerAppendMetricSet is appendMetricSet as it was, writing the series in
// the set's order whatever that is, for the differential tests below.
func formerAppendMetricSet(b []byte, s *model.MetricSet) []byte {
	var e expositionWriter
	described := map[string]bool{}
	last := ""
	for _, m := range s.Metrics {
		if m.Name != last && !described[m.Name] {
			if m.Help != "" {
				b = append(b, "# HELP "...)
				b = append(b, m.Name...)
				b = append(b, ' ')
				b = appendEscaped(b, m.Help, false)
				b = append(b, '\n')
			}
			b = append(b, "# TYPE "...)
			b = append(b, m.Name...)
			b = append(b, ' ')
			b = append(b, m.Type...)
			b = append(b, '\n')
			described[m.Name] = true
		}
		last = m.Name
		switch {
		case m.Histogram != nil:
			b = e.appendHistogram(b, m)
		case m.Summary != nil:
			b = e.appendSummary(b, m)
		default:
			b = e.appendSample(b, m.Name, "", m.Labels, "", "", m.Value, m)
		}
	}
	return b
}

// generatedFamilySet is a set of some families of every type with a few
// series each: together, a family after another, or, apart, shuffled.
func generatedFamilySet(random *rand.Rand, apart bool) *model.MetricSet {
	at := int64(1700000000000)
	set := &model.MetricSet{}
	for f, families := 0, 1+random.Intn(6); f < families; f++ {
		name := fmt.Sprintf("family_%d", f)
		typ := []model.MetricType{model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType, model.HistogramMetricType, model.SummaryMetricType}[random.Intn(5)]
		help := []string{"", "Help of " + name, "a \\ b\nc"}[random.Intn(3)]
		for s, series := 0, 1+random.Intn(4); s < series; s++ {
			m := model.Metric{Name: name, Help: help, Type: typ, Value: float64(random.Intn(100)), Labels: map[string]string{"s": strconv.Itoa(s), "q": "a\"b"}}
			if s == 0 {
				m.Labels = nil
			}
			if random.Intn(4) == 0 {
				m.Timestamp = &at
			}
			switch typ {
			case model.HistogramMetricType:
				m.Histogram = &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}, {UpperBound: math.Inf(1), CumulativeCount: 2}}, Sum: 1.5, Count: 2}
			case model.SummaryMetricType:
				m.Summary = &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: 2, Count: 3}
			}
			set.Metrics = append(set.Metrics, m)
		}
	}
	if apart {
		random.Shuffle(len(set.Metrics), func(i, j int) { set.Metrics[i], set.Metrics[j] = set.Metrics[j], set.Metrics[i] })
	}
	return set
}

// familiesTogether is a set with each family's series together: the
// families in the order each first appears, and each one's series in the
// set's order. It is written as plainly as it can be, to hold the writer
// against.
func familiesTogether(set *model.MetricSet) *model.MetricSet {
	var names []string
	byName := map[string][]model.Metric{}
	for _, m := range set.Metrics {
		if _, seen := byName[m.Name]; !seen {
			names = append(names, m.Name)
		}
		byName[m.Name] = append(byName[m.Name], m)
	}
	out := &model.MetricSet{}
	for _, name := range names {
		out.Metrics = append(out.Metrics, byName[name]...)
	}
	return out
}

// A set that has each metric's series together is written byte for byte as
// it was: of two thousand generated sets of every type of series, with and
// without help, labels and timestamps. A set that has them apart is written
// as the set with them together was and is: the same lines, every one once,
// in the order of that set.
func TestTheTextFormatIsWrittenAsItWasButForSeriesApart(t *testing.T) {
	random := rand.New(rand.NewSource(8))
	together, apart := 0, 0
	for i := 0; i < 2000; i++ {
		set := generatedFamilySet(random, i%2 == 1)
		got := string(appendMetricSet(nil, set))
		grouped := familiesTogether(set)
		if slices.EqualFunc(set.Metrics, grouped.Metrics, func(a, b model.Metric) bool { return a.Name == b.Name }) {
			together++
			if was := string(formerAppendMetricSet(nil, set)); got != was {
				t.Fatalf("a set with its families together:\n%s\nwas written\n%s", got, was)
			}
			continue
		}
		apart++
		if want := string(formerAppendMetricSet(nil, grouped)); got != want || got != string(appendMetricSet(nil, grouped)) {
			t.Fatalf("a set with its families apart:\n%s\nwant what the set with them together is written as\n%s", got, want)
		}
		if err := parseExposition([]byte(got)); err != nil {
			t.Fatalf("%v:\n%s", err, got)
		}
	}
	if together < 1000 || apart < 500 {
		t.Errorf("%d sets have their families together and %d apart: the sets do not cover both", together, apart)
	}
}

// Writing a set that has its families together, as nearly every set has,
// allocates what it did: looking for a family written before costs the
// lookup the writer already made, once a family, and nothing a series.
func TestWritingASetWithItsFamiliesTogetherAllocatesWhatItDid(t *testing.T) {
	set := &model.MetricSet{}
	for f := 0; f < 20; f++ {
		for s := 0; s < 250; s++ {
			set.Metrics = append(set.Metrics, model.Metric{Name: fmt.Sprintf("family_%d", f), Help: "Help", Type: model.GaugeMetricType, Value: float64(s), Labels: map[string]string{"s": strconv.Itoa(s), "region": "eu"}})
		}
	}
	buffer := make([]byte, 0, 1<<20)
	was, _ := alloctest.Allocations(20, func() { buffer = formerAppendMetricSet(buffer[:0], set) })
	now := alloctest.AllocsAtMost(20, was, func() { buffer = appendMetricSet(buffer[:0], set) })
	if now > was {
		t.Errorf("writing a set of 5000 series in 20 families allocates %v times, and allocated %v", now, was)
	}
}

// Every way a set is given out has each metric's series together, whatever
// order the set has them in: the text format and OpenMetrics, each under one
// TYPE line; the static targets endpoint, which merges its targets' sets
// family by family; and OTLP, whose metric holds all the points of its
// family. Each has the families in the order they first appear and a
// family's series in the set's order.
func TestEveryOutputHasAMetricsSeriesTogether(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "host_cpu", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"host": "web01"}},
		{Name: "host_restarts_total", Type: model.CounterMetricType, Value: 2, Labels: map[string]string{"host": "web01"}},
		{Name: "host_cpu", Type: model.GaugeMetricType, Value: 3, Labels: map[string]string{"host": "web02"}},
		{Name: "host_restarts_total", Type: model.CounterMetricType, Value: 4, Labels: map[string]string{"host": "web02"}},
	}}
	const text = `# TYPE host_cpu gauge
host_cpu{host="web01"} 1
host_cpu{host="web02"} 3
# TYPE host_restarts_total counter
host_restarts_total{host="web01"} 2
host_restarts_total{host="web02"} 4
`
	if got := string(appendMetricSet(nil, &set)); got != text {
		t.Errorf("the text format:\n%s\nwant\n%s", got, text)
	}
	const openMetrics = `# TYPE host_cpu gauge
host_cpu{host="web01"} 1
host_cpu{host="web02"} 3
# TYPE host_restarts counter
host_restarts_total{host="web01"} 2
host_restarts_total{host="web02"} 4
# EOF
`
	if got := string(appendOpenMetrics(nil, &set)); got != openMetrics {
		t.Errorf("OpenMetrics:\n%s\nwant\n%s", got, openMetrics)
	}

	registerFixtureType(t)
	merged := pathServer(t, fixtureCollector()).mergeStaticTargets([]namedSet{{name: "fleet", set: set}})
	const static = `# TYPE host_cpu gauge
host_cpu{host="web01",static_target="fleet"} 1
host_cpu{host="web02",static_target="fleet"} 3
# TYPE host_restarts_total counter
host_restarts_total{host="web01",static_target="fleet"} 2
host_restarts_total{host="web02",static_target="fleet"} 4
`
	if got := string(appendMetricSet(nil, &merged)); got != static {
		t.Errorf("the static targets endpoint:\n%s\nwant\n%s", got, static)
	}

	var points []string
	for _, metric := range otlpMetrics(set, "1", nil) {
		switch {
		case metric.Gauge != nil:
			for _, point := range metric.Gauge.DataPoints {
				points = append(points, fmt.Sprintf("gauge %s %v", metric.Name, float64(*point.AsDouble)))
			}
		case metric.Sum != nil:
			for _, point := range metric.Sum.DataPoints {
				points = append(points, fmt.Sprintf("sum %s %v", metric.Name, float64(*point.AsDouble)))
			}
		}
	}
	if want := []string{"gauge host_cpu 1", "gauge host_cpu 3", "sum host_restarts_total 2", "sum host_restarts_total 4"}; !slices.Equal(points, want) || len(otlpMetrics(set, "1", nil)) != 2 {
		t.Errorf("OTLP: %q, want the two metrics' points %q", points, want)
	}
}
