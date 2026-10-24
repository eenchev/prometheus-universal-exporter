package transform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's derived entries compute series from the series its rules
// made, after the transform, whatever the transform (derived.go).

// derivedScrape runs the collector's transform over body, with a rule report.
func derivedScrape(t *testing.T, c model.Collector, body string) (*model.MetricSet, []RuleFailure, error) {
	t.Helper()
	if c.Request.Type == "" {
		c.Request.Type = fetch.RequestTypeHTTP
	}
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, report := WithRuleReport(context.Background())
	ctx = WithRuleLogger(ctx, slog.New(slog.DiscardHandler))
	set, err := Transform(ctx, d, r, &c, "python3")
	return set, report.Failures(), err
}

// seriesLines is each series as name{labels} value, the labels sorted, in
// the order of the set, with the type of a family after its name where a
// derived one is asked about.
func seriesLines(set *model.MetricSet) []string {
	var lines []string
	for _, m := range set.Metrics {
		names := model.SortedKeys(m.Labels)
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = fmt.Sprintf("%s=%q", name, m.Labels[name])
		}
		lines = append(lines, fmt.Sprintf("%s{%s} %g", m.Name, strings.Join(parts, ","), m.Value))
	}
	return lines
}

func wantSeries(t *testing.T, set *model.MetricSet, want ...string) {
	t.Helper()
	got := seriesLines(set)
	for _, line := range want {
		if !slices.Contains(got, line) {
			t.Errorf("no %s among\n%s", line, strings.Join(got, "\n"))
		}
	}
}

func noSeries(t *testing.T, set *model.MetricSet, names ...string) {
	t.Helper()
	for _, m := range set.Metrics {
		if slices.Contains(names, m.Name) {
			t.Errorf("series of %s: %v", m.Name, seriesLines(set))
		}
	}
}

func derivedCounterRule(name, expression string, labels ...model.LabelRule) model.MetricRule {
	return model.MetricRule{Name: name, Type: model.CounterMetricType, Expression: expression, ErrorMode: model.ErrorModeLog, Labels: labels}
}

const derivedStats = `hits pool=a 90
misses pool=a 10
hits pool=b 0
misses pool=b 0
items queue=q1 shard=1 4
items queue=q1 shard=2 6
items queue=q2 shard=1 5
`

func derivedRegexCollector(derived ...model.DerivedRule) model.Collector {
	pool := model.LabelRule{Name: "pool", Expression: "pool"}
	return model.Collector{
		Name: "stats", Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
		Metrics: []model.MetricRule{
			derivedCounterRule("hits_total", `(?m)^hits pool=(?P<pool>\w+) (?P<value>\d+)$`, pool),
			derivedCounterRule("misses_total", `(?m)^misses pool=(?P<pool>\w+) (?P<value>\d+)$`, pool),
			{Name: "queue_items", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog, Expression: `(?m)^items queue=(?P<queue>\w+) shard=(?P<shard>\d+) (?P<value>\d+)$`,
				Labels: []model.LabelRule{{Name: "queue", Expression: "queue"}, {Name: "shard", Expression: "shard"}}},
		},
		Derived: derived,
	}
}

func by(labels ...string) *[]string { return &labels }

// Each operation computes what it says, of the series matched by their
// labels: a sum, a difference and a ratio, element-wise; a sum by labels and
// a sum by none over one family; and a ratio over a hidden sum, the hit
// ratio of hits / (hits + misses), the sum itself not exported.
func TestDerivedSeriesComputeEachOperation(t *testing.T) {
	c := derivedRegexCollector(
		model.DerivedRule{Name: "gets_total", Type: model.CounterMetricType, Description: "Gets", Sum: []string{"hits_total", "misses_total"}},
		model.DerivedRule{Name: "hits_over_misses", Type: model.GaugeMetricType, Difference: []string{"hits_total", "misses_total"}},
		model.DerivedRule{Name: "miss_ratio", Type: model.GaugeMetricType, Ratio: []string{"misses_total", "gets_total"}},
		model.DerivedRule{Name: "gets", Type: model.GaugeMetricType, Hidden: true, Sum: []string{"hits_total", "misses_total"}},
		model.DerivedRule{Name: "hit_ratio", Type: model.GaugeMetricType, Ratio: []string{"hits_total", "gets"}},
		model.DerivedRule{Name: "queue_depth", Type: model.GaugeMetricType, Sum: []string{"queue_items"}, By: by("queue")},
		model.DerivedRule{Name: "items_total", Type: model.GaugeMetricType, Sum: []string{"queue_items"}, By: by()},
	)
	set, failures, err := derivedScrape(t, c, derivedStats)
	if err != nil || len(failures) > 0 {
		t.Fatalf("err %v, failures %v", err, failures)
	}
	wantSeries(t, set,
		`gets_total{pool="a"} 100`, `gets_total{pool="b"} 0`,
		`hits_over_misses{pool="a"} 80`, `hits_over_misses{pool="b"} 0`,
		`miss_ratio{pool="a"} 0.1`,
		`hit_ratio{pool="a"} 0.9`,
		`queue_depth{queue="q1"} 10`, `queue_depth{queue="q2"} 5`,
		`items_total{} 15`,
	)
	noSeries(t, set, "gets")
	for _, m := range set.Metrics {
		if m.Name == "gets_total" && (m.Type != model.CounterMetricType || m.Help != "Gets") {
			t.Errorf("gets_total is a %s with help %q", m.Type, m.Help)
		}
		if m.Name == "hit_ratio" && m.Type != model.GaugeMetricType {
			t.Errorf("hit_ratio is a %s", m.Type)
		}
	}
	// 0 / 0 of pool b makes no series: no ratio is better than NaN.
	for _, line := range seriesLines(set) {
		if strings.HasPrefix(line, "hit_ratio{pool=\"b\"}") || strings.HasPrefix(line, "miss_ratio{pool=\"b\"}") {
			t.Errorf("a ratio over 0: %s", line)
		}
	}
}

// A series without a partner of the same labels makes no derived series,
// nor does a label set two series of one operand have; a series that has
// one does. A label of an empty value is no label, to the matching too.
func TestDerivedSeriesMatchByIdenticalLabelSets(t *testing.T) {
	c := derivedRegexCollector(model.DerivedRule{Name: "gets_total", Type: model.GaugeMetricType, Sum: []string{"hits_total", "misses_total"}})
	set, _, err := derivedScrape(t, c, "hits pool=a 1\nhits pool=b 2\nmisses pool=b 3\nmisses pool=c 4\n")
	if err != nil {
		t.Fatal(err)
	}
	wantSeries(t, set, `gets_total{pool="b"} 5`)
	if got := strings.Count(strings.Join(seriesLines(set), "\n"), "gets_total"); got != 1 {
		t.Errorf("%d series of gets_total: %v", got, seriesLines(set))
	}
}

// A ratio over 0, and any result that is NaN or infinite, makes no series
// and is no failure.
func TestDerivedSeriesThatAreNotFiniteAreLeftOut(t *testing.T) {
	c := model.Collector{
		Name: "values", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{
			{Name: "a", Type: model.GaugeMetricType, Expression: ".a", ErrorMode: model.ErrorModeLog},
			{Name: "zero", Type: model.GaugeMetricType, Expression: ".zero", ErrorMode: model.ErrorModeLog},
			{Name: "inf", Type: model.GaugeMetricType, Expression: ".inf", ErrorMode: model.ErrorModeLog, ValueMap: map[string]float64{"big": posInf()}},
		},
		Derived: []model.DerivedRule{
			{Name: "over_zero", Type: model.GaugeMetricType, Ratio: []string{"a", "zero"}},
			{Name: "zero_over_zero", Type: model.GaugeMetricType, Ratio: []string{"zero", "zero"}},
			{Name: "plus_inf", Type: model.GaugeMetricType, Sum: []string{"a", "inf"}},
			{Name: "fine", Type: model.GaugeMetricType, Difference: []string{"a", "zero"}},
		},
	}
	set, failures, err := derivedScrape(t, c, `{"a": 3, "zero": 0, "inf": "big"}`)
	if err != nil || len(failures) > 0 {
		t.Fatalf("err %v, failures %v", err, failures)
	}
	noSeries(t, set, "over_zero", "zero_over_zero", "plus_inf")
	wantSeries(t, set, `fine{} 3`)
}

func posInf() float64 {
	var zero float64
	return 1 / zero
}

// A csv collector derives from its columns' series, row by row, the labels
// of a row matching it with its partner.
func TestDerivedSeriesOfACSVCollector(t *testing.T) {
	host := model.LabelRule{Name: "host", Expression: "host"}
	c := model.Collector{
		Name: "disks", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"},
		Metrics: []model.MetricRule{
			{Name: "disk_used_bytes", Type: model.GaugeMetricType, Expression: "used", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{host}},
			{Name: "disk_size_bytes", Type: model.GaugeMetricType, Expression: "size", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{host}},
		},
		Derived: []model.DerivedRule{
			{Name: "disk_free_bytes", Type: model.GaugeMetricType, Difference: []string{"disk_size_bytes", "disk_used_bytes"}},
			{Name: "disk_used_ratio", Type: model.GaugeMetricType, Ratio: []string{"disk_used_bytes", "disk_size_bytes"}},
			{Name: "disk_used_bytes_all", Type: model.GaugeMetricType, Sum: []string{"disk_used_bytes"}, By: by()},
		},
	}
	set, _, err := derivedScrape(t, c, "host,used,size\nweb1,25,100\nweb2,50,200\n")
	if err != nil {
		t.Fatal(err)
	}
	wantSeries(t, set,
		`disk_free_bytes{host="web1"} 75`, `disk_free_bytes{host="web2"} 150`,
		`disk_used_ratio{host="web1"} 0.25`, `disk_used_ratio{host="web2"} 0.25`,
		`disk_used_bytes_all{} 75`,
	)
}

// The derived series are computed before the collector-wide settings and get
// them as every series does: the operands and by are written as the rules
// write their names, and the result gets transform.labels, the prefix and
// the escaping.
func TestDerivedSeriesGetTheCollectorWideSettings(t *testing.T) {
	c := model.Collector{
		Name: "app", MetricsPrefix: "acme", NameEscaping: NameEscapingUnderscores,
		Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq", Labels: map[string]string{"env": "prod"}, RenameLabels: map[string]string{"queue": "q"}},
		Metrics: []model.MetricRule{
			{Name: "items", Type: model.GaugeMetricType, Items: ".queues[]", Expression: ".n", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}}},
		},
		Derived: []model.DerivedRule{{Name: "items.all", Type: model.GaugeMetricType, Sum: []string{"items"}, By: by("queue")}},
	}
	set, _, err := derivedScrape(t, c, `{"queues": [{"name": "a", "n": 1}, {"name": "b", "n": 4}]}`)
	if err != nil {
		t.Fatal(err)
	}
	wantSeries(t, set, `acme_items_all{env="prod",q="a"} 1`, `acme_items_all{env="prod",q="b"} 4`, `acme_items{env="prod",q="a"} 1`)
}

// Exported derived series count against limits.max_metrics: a scrape whose
// rules make as many series as the limit and whose derived entries make more
// fails as one whose rules make too many does. A hidden entry's series are
// not exported and not counted.
func TestDerivedSeriesCountAgainstTheSeriesLimit(t *testing.T) {
	hidden := model.DerivedRule{Name: "gets", Type: model.GaugeMetricType, Hidden: true, Sum: []string{"hits_total", "misses_total"}}
	ratio := model.DerivedRule{Name: "hit_ratio", Type: model.GaugeMetricType, Ratio: []string{"hits_total", "gets"}}
	body := "hits pool=a 1\nmisses pool=a 1\n"
	c := derivedRegexCollector(hidden, ratio)
	c.Limits.MaxMetrics = 3
	if _, _, err := derivedScrape(t, c, body); err != nil {
		t.Fatalf("2 series and 1 derived, hidden one not counted, within 3: %v", err)
	}
	c.Limits.MaxMetrics = 2
	_, _, err := derivedScrape(t, c, body)
	if err == nil || !strings.Contains(err.Error(), model.MetricCountError(3, 2).Error()) {
		t.Fatalf("2 series and 1 derived over 2: %v", err)
	}
}

// A prometheus pass-through's operands are named at run time: a family of
// the target that is a histogram makes no derived series and is reported as
// the entry's failure, under log, while the entry's other series and the
// collector's carry on; a counter of the target is read as any series. The
// derived series are added without writing past the decoded series.
func TestDerivedSeriesOfAPassThrough(t *testing.T) {
	c := model.Collector{
		Name: "node", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Derived: []model.DerivedRule{
			{Name: "requests_failed_ratio", Type: model.GaugeMetricType, Ratio: []string{"requests_failed_total", "requests_total"}},
			{Name: "latency_twice", Type: model.GaugeMetricType, Sum: []string{"latency_seconds", "latency_seconds"}},
		},
	}
	body := "# TYPE requests_total counter\nrequests_total{code=\"x\"} 10\n# TYPE requests_failed_total counter\nrequests_failed_total{code=\"x\"} 1\n" +
		"# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"+Inf\"} 1\nlatency_seconds_sum 1\nlatency_seconds_count 1\n"
	set, failures, err := derivedScrape(t, c, body)
	if err != nil {
		t.Fatal(err)
	}
	wantSeries(t, set, `requests_failed_ratio{code="x"} 0.1`)
	noSeries(t, set, "latency_twice")
	if len(failures) != 1 || failures[0].Metric != "latency_twice" || !strings.Contains(failures[0].First.Error(), `reads "latency_seconds", a histogram`) {
		t.Fatalf("failures %+v", failures)
	}
	// The decoded series stay as they were.
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	decoded := d.Data.(model.MetricSet)
	room := slices.Grow(decoded.Metrics, 4)
	d.Data = model.MetricSet{Metrics: room}
	before := room[:len(room)+1][len(room)]
	if _, err := Transform(context.Background(), d, r, &c, "python3"); err != nil {
		t.Fatal(err)
	}
	if after := room[:len(room)+1][len(room)]; after.Name != before.Name {
		t.Fatalf("the transform wrote %q past the decoded series", after.Name)
	}
}

// A derived entry reads the series the rules made after their value maps
// and scale, and a later entry may read an earlier, exported or not.
func TestDerivedEntriesChain(t *testing.T) {
	c := model.Collector{
		Name: "chain", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{
			{Name: "a_ms", Type: model.GaugeMetricType, Expression: ".a", ErrorMode: model.ErrorModeLog, Scale: ptr(0.001)},
			{Name: "b", Type: model.GaugeMetricType, Expression: ".b", ErrorMode: model.ErrorModeLog, ValueMap: map[string]float64{"up": 1}},
		},
		Derived: []model.DerivedRule{
			{Name: "s", Type: model.GaugeMetricType, Hidden: true, Sum: []string{"a_ms", "b"}},
			{Name: "t", Type: model.GaugeMetricType, Sum: []string{"s", "b"}},
			{Name: "u", Type: model.GaugeMetricType, Difference: []string{"t", "s"}},
		},
	}
	set, _, err := derivedScrape(t, c, `{"a": 500, "b": "up"}`)
	if err != nil {
		t.Fatal(err)
	}
	wantSeries(t, set, `t{} 2.5`, `u{} 1`)
	noSeries(t, set, "s")
}

func ptr[T any](v T) *T { return &v }

// The load refuses a derived list that could not be computed or whose series
// every scrape would refuse, each entry in words that say what to write
// instead; an entry without a type is a gauge.
func TestCheckDerivedRefusesWhatCannotBeComputed(t *testing.T) {
	base := func(d ...model.DerivedRule) *model.Collector {
		c := derivedRegexCollector(d...)
		c.Metrics = append(c.Metrics, model.MetricRule{Name: "latency", Type: model.HistogramMetricType, Expression: `(\d+)`, Histogram: &model.HistogramRule{Buckets: []float64{1}}})
		return &c
	}
	for name, tc := range map[string]struct {
		derived []model.DerivedRule
		want    string
	}{
		"no name":         {[]model.DerivedRule{{Sum: []string{"hits_total", "misses_total"}}}, `collector "stats" derived entry 1 has no name`},
		"invalid name":    {[]model.DerivedRule{{Name: "a-b", Sum: []string{"hits_total", "misses_total"}}}, `collector "stats" derived "a-b": "a-b" is not a valid Prometheus metric name`},
		"a rule's name":   {[]model.DerivedRule{{Name: "hits_total", Sum: []string{"hits_total", "misses_total"}}}, `has the name of a rule of the collector`},
		"a bucket":        {[]model.DerivedRule{{Name: "latency_bucket", Sum: []string{"hits_total", "misses_total"}}}, `clashes with the histogram "latency"`},
		"twice":           {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "misses_total"}}, {Name: "x", Ratio: []string{"hits_total", "misses_total"}}}, `derived "x" is the name of derived entry 1 too`},
		"histogram type":  {[]model.DerivedRule{{Name: "x", Type: model.HistogramMetricType, Sum: []string{"hits_total", "misses_total"}}}, `has type "histogram"; a derived series is one number`},
		"no operation":    {[]model.DerivedRule{{Name: "x"}}, `has no operation; set one of sum, difference and ratio`},
		"two operations":  {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "misses_total"}, Ratio: []string{"hits_total", "misses_total"}}}, `sets sum and ratio; an entry computes one of them`},
		"sum of one":      {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total"}}}, `sum reads 1 families; list two or more`},
		"ratio of three":  {[]model.DerivedRule{{Name: "x", Ratio: []string{"hits_total", "misses_total", "hits_total"}}}, `ratio reads 3 families, and takes two, [a, b]: a / b`},
		"by on a ratio":   {[]model.DerivedRule{{Name: "x", Ratio: []string{"hits_total", "misses_total"}, By: by("pool")}}, `sets by, which only sum takes`},
		"by of two":       {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "misses_total"}, By: by("pool")}}, `sets by, so its sum adds up the series of one family, and it reads 2`},
		"by twice":        {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total"}, By: by("pool", "pool")}}, `by lists the label "pool" twice`},
		"by invalid":      {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total"}, By: by("a-b")}}, `by has invalid label name "a-b"`},
		"unknown operand": {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "nothing"}}}, `sum reads "nothing", which is neither a rule of the collector nor an earlier derived entry`},
		"later operand":   {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "y"}}, {Name: "y", Sum: []string{"hits_total", "misses_total"}}}, `sum reads "y", the derived entry 2, which comes after it`},
		"itself":          {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "x"}}}, `sum reads itself`},
		"a histogram":     {[]model.DerivedRule{{Name: "x", Sum: []string{"hits_total", "latency"}}}, `sum reads "latency", a histogram, which has no one value`},
		"hidden unread":   {[]model.DerivedRule{{Name: "x", Hidden: true, Sum: []string{"hits_total", "misses_total"}}}, `derived "x" is hidden, and no later entry reads it`},
	} {
		err := CheckDerived(base(tc.derived...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	c := base(model.DerivedRule{Name: "x", Sum: []string{"hits_total", "misses_total"}})
	if err := CheckDerived(c); err != nil || c.Derived[0].Type != model.GaugeMetricType {
		t.Fatalf("a sound entry: %v, type %q", err, c.Derived[0].Type)
	}
	// Under prometheus and python, an operand no rule names is the name of
	// a series a scrape makes, and is taken.
	for _, kind := range []string{"prometheus", "python"} {
		c := &model.Collector{Name: "runtime", Transform: model.TransformConfig{Type: kind}, Derived: []model.DerivedRule{{Name: "x", Sum: []string{"a", "b"}}}}
		if err := CheckDerived(c); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	// A name past limits.max_metric_name_length once prefixed.
	c = base(model.DerivedRule{Name: "abcdef", Sum: []string{"hits_total", "misses_total"}})
	c.MetricsPrefix, c.Limits.MaxMetricNameLength = "acme", 10
	if err := CheckDerived(c); err == nil || !strings.Contains(err.Error(), `is exported as "acme_abcdef", 11 bytes, longer than limits.max_metric_name_length 10`) {
		t.Errorf("long name: %v", err)
	}
	var several model.Problems
	if err := CheckDerived(base(model.DerivedRule{}, model.DerivedRule{Name: "y"})); !errors.As(err, &several) || len(several) != 2 {
		t.Errorf("two bad entries: %v", err)
	}
}

// A derived entry named as a family a pass-through's target exports fails as
// an entry and makes no series, the target's family left as it came; an
// operand the scrape has no series of is no failure, and the rule report
// names it under each entry it gave no series, through an entry read.
func TestADerivedEntryOfAPassThroughNamedOrReadingWhatTheTargetHasNot(t *testing.T) {
	c := model.Collector{
		Name: "node", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Derived: []model.DerivedRule{
			{Name: "requests_ratio", Type: model.GaugeMetricType, Ratio: []string{"requests_total", "requests_total"}},
			{Name: "failed_all", Type: model.GaugeMetricType, Hidden: true, Sum: []string{"requests_failed_total", "requests_total"}},
			{Name: "failed_ratio", Type: model.GaugeMetricType, Ratio: []string{"failed_all", "requests_total"}},
		},
	}
	body := "# TYPE requests_total counter\nrequests_total 10\n# TYPE requests_ratio gauge\nrequests_ratio 0.5\n"
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	c.Request.Type = fetch.RequestTypeHTTP
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, report := WithRuleReport(context.Background())
	ctx = WithRuleLogger(ctx, slog.New(slog.DiscardHandler))
	set, err := Transform(ctx, d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if got := seriesLines(set); !slices.Equal(got, []string{"requests_total{} 10", "requests_ratio{} 0.5"}) {
		t.Fatalf("series %v", got)
	}
	failures := report.Failures()
	if len(failures) != 1 || failures[0].Metric != "requests_ratio" || !failures[0].Logged || !strings.Contains(failures[0].First.Error(), "is the name of a family the scrape's series have already") {
		t.Fatalf("failures %+v", failures)
	}
	want := map[string][]string{"failed_all": {"requests_failed_total"}, "failed_ratio": {"requests_failed_total"}}
	if got := report.DerivedAbsent(); len(got) != len(want) || !slices.Equal(got["failed_all"], want["failed_all"]) || !slices.Equal(got["failed_ratio"], want["failed_ratio"]) {
		t.Fatalf("absent operands %v", got)
	}
}
