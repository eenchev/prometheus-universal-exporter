package exporter

import (
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// OpenMetrics names a counter's family without _total, and its sample as
// the family with it. One counter name has no family that way (exposition.go,
// openMetricsCounter): the name _total itself.

// A counter named _total would be a family with no name, "# TYPE  counter",
// which no parser reads; it is written as the unknown family _total.
func TestOpenMetricsCounterNamedTotalIsAnUnknownFamily(t *testing.T) {
	set := &model.MetricSet{Metrics: []model.Metric{counter("_total", "", 3), counter("jobs_total", "", 4)}}
	want := "# TYPE _total unknown\n_total 3\n# TYPE jobs counter\njobs_total 4\n# EOF\n"
	got := string(appendOpenMetrics(nil, set))
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	checkOpenMetricsClaims(t, got)
}

// decodedAnswer reads an answer of the exporter, in the text format or in
// OpenMetrics, the way a collector passing Prometheus text through reads a
// target that answers with it: with the exporter's own reader.
func decodedAnswer(t *testing.T, answer string, openMetrics bool) []model.Metric {
	t.Helper()
	contentType := expositionContentType
	if openMetrics {
		contentType = expositionFormat{openMetrics: true, version: openMetricsVersion}.contentType()
	}
	r := &fetch.HTTPResponse{Body: []byte(answer), Headers: http.Header{"Content-Type": {contentType}}}
	decoded, err := decode.Decode(r, &model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}})
	if err != nil {
		t.Fatalf("the exporter's own reader refuses the answer: %v\n%s", err, answer)
	}
	return decoded.Data.(model.MetricSet).Metrics
}

// escapedCounters are counter names as the text format has them, with the
// family and the sample OpenMetrics has for each: plain names, and names
// escaped by name_escaping values, where the _total of the original name
// reads __total and a dot _2e_.
var escapedCounters = []struct{ name, original, family, sample string }{
	{"jobs_total", "jobs_total", "jobs", "jobs_total"},
	{"jobs", "jobs", "jobs", "jobs_total"},
	{"U__my_2e_requests__total", "my.requests_total", "U__my_2e_requests_", "U__my_2e_requests__total"},
	{"U__my_2e_requests", "my.requests", "U__my_2e_requests", "U__my_2e_requests_total"},
	{"U__my_2e_total", "my.total", "U__my_2e", "U__my_2e_total"},
	{"U__my_2e_errors_2e_", "my.errors.", "U__my_2e_errors_2e_", "U__my_2e_errors_2e__total"},
	{"U___e9___total", "é_total", "U___e9__", "U___e9___total"},
	{"U__jobs_total", "U__jobs_total", "U__jobs", "U__jobs_total"},
	{"U____total", "_total, escaped", "U___", "U____total"},
}

// A counter whose name was escaped by name_escaping values was given its
// _total in escaped form: the family U__my_2e_requests with the sample
// U__my_2e_requests__total, which is not the family and _total. A reader of
// OpenMetrics, a strict parser and this exporter's own alike, then saw a
// counter family without samples and beside it a sample of no family, so the
// counter arrived as an untyped series. The suffix is the plain _total on
// whatever the name is, as Prometheus writes an escaped counter and as main
// did: the sample is always its family and _total, and the answer read back
// holds one counter, under the name its sample has.
func TestOpenMetricsCounterIsItsFamilyAndTotalWhateverItsName(t *testing.T) {
	for _, tc := range escapedCounters {
		t.Run(tc.original, func(t *testing.T) {
			want := "# TYPE " + tc.family + " counter\n" + tc.sample + " 5\n# EOF\n"
			got := string(appendOpenMetrics(nil, &model.MetricSet{Metrics: []model.Metric{counter(tc.name, "", 5)}}))
			if got != want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, want)
			}
			if tc.sample != tc.family+"_total" {
				t.Fatalf("the sample %s is not its family %s and _total", tc.sample, tc.family)
			}
			if err := strictOpenMetricsError(got); err != nil {
				t.Fatalf("a strict parser refuses the answer: %v\n%s", err, got)
			}
			checkOpenMetricsClaims(t, got)
			read := decodedAnswer(t, got, true)
			if len(read) != 1 || read[0].Type != model.CounterMetricType || read[0].Name != tc.sample || read[0].Value != 5 {
				t.Fatalf("the exporter's own reader made %+v of\n%s", read, got)
			}
		})
	}
}

// The strict reference parser, prometheus_client's, reads each of those
// answers as one counter family holding its sample, and refuses nothing. It
// reads the form with the suffix escaped, which the exporter wrote before, as
// a counter without samples and a sample of another family, which is what
// was wrong with it.
func TestTheStrictOpenMetricsParserReadsACounterOfAnyNameAsOneFamily(t *testing.T) {
	var answers []string
	for _, tc := range escapedCounters {
		answers = append(answers, string(appendOpenMetrics(nil, &model.MetricSet{Metrics: []model.Metric{counter(tc.name, "", 5)}})))
	}
	escapedSuffix := "# TYPE U__my_2e_requests counter\nU__my_2e_requests__total 5\n# EOF\n"
	verdicts := strictParser(t, append(answers, escapedSuffix))
	for i, tc := range escapedCounters {
		v := verdicts[i]
		if v.Error != "" || len(v.Samples) != 1 || v.Samples[0].Type != "counter" || v.Samples[0].Family != tc.family || v.Samples[0].Name != tc.sample || v.Samples[0].Value != "5.0" {
			t.Errorf("%s: the strict parser read %+v (%s) of\n%s", tc.original, v.Samples, v.Error, answers[i])
		}
	}
	if v := verdicts[len(answers)]; v.Error == "" && len(v.Samples) == 1 && v.Samples[0].Type == "counter" {
		t.Errorf("the strict parser reads the suffix in escaped form as a counter's sample: %+v", v.Samples)
	}
}

// An escaped counter gives way as any other does when a name it claims is
// another family's: the gauge named as its family would be.
func TestOpenMetricsEscapedCounterGivesWayToAFamilyOfItsName(t *testing.T) {
	set := &model.MetricSet{Metrics: []model.Metric{
		counter("U__my_2e_requests", "", 5),
		{Name: "U__my_2e_requests_total", Type: model.GaugeMetricType, Value: 1},
	}}
	want := "# TYPE U__my_2e_requests unknown\nU__my_2e_requests 5\n# TYPE U__my_2e_requests_total gauge\nU__my_2e_requests_total 1\n# EOF\n"
	got := string(appendOpenMetrics(nil, set))
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	checkOpenMetricsClaims(t, got)
}
