package decode

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The prometheus decoder leaves a label with an empty value off its series:
// to Prometheus m{l=""} is the series m, and the exporter leaves an empty
// label off wherever one comes from. These tests say what that changes by
// the parser as it was, without its switch (promoracle_test.go): a body is
// read as the parser it was reads the same body with those labels not
// written, to the series, their order and the error's words. What follows
// from the labels being off is then in that reading too: two series that
// were told apart by one are one series.

// sameAsWithout parses with by the parser and without by the parser as it
// was, both read one way, and reports any difference between the two. It
// says whether the body was accepted.
func sameAsWithout(t *testing.T, with, without string, reading promReading) bool {
	t.Helper()
	var askedOld, asked []string
	old, oldErr := oracleParseExposition([]byte(without), reading.options(&askedOld))
	got, err := parseExposition([]byte(with), reading.options(&asked))
	switch {
	case (err == nil) != (oldErr == nil) || err != nil && err.Error() != oldErr.Error():
		t.Errorf("%q (%s): err=%v, and the parser it was reads %q as err=%v", with, reading, err, without, oldErr)
	case errors.Is(err, model.ErrLimitExceeded) != errors.Is(oldErr, model.ErrLimitExceeded):
		t.Errorf("%q (%s): err=%#v, was %#v", with, reading, err, oldErr)
	case !reflect.DeepEqual(asked, askedOld):
		t.Errorf("%q (%s): the filter was asked about %q, and about %q for %q", with, reading, asked, askedOld, without)
	default:
		if diff := sameMetrics(old, got); diff != "" {
			t.Errorf("%q (%s) is not read as the parser it was reads %q: %s", with, reading, without, diff)
		}
	}
	return err == nil
}

// An exposition's label written with an empty value is read as the same
// exposition without the label was: beside other labels and alone, with a
// quoted name, in the braces form, with a timestamp and an exemplar, on a
// histogram's and a summary's samples, and where the label is named le or
// quantile on a series that has no buckets or quantiles. A label of one
// blank is a value and stays.
func TestAnEmptyLabelOfAnExpositionIsReadAsTheLabelNotWritten(t *testing.T) {
	pairs := [][2]string{
		{`m{l=""} 1`, `m 1`},
		{`m{l="",k="v"} 1`, `m{k="v"} 1`},
		{`m{k="v",l=""} 1`, `m{k="v"} 1`},
		{`m{a="",k="v",b="",j="w",c=""} 1`, `m{k="v",j="w"} 1`},
		{`m{a="",b="",c=""} 1 1700000000000`, `m{} 1 1700000000000`},
		{`m { l = "" , k = "v" , } 1`, `m { k = "v" , } 1`},
		{`m{l="",} 1`, `m 1`},
		{`m{l=" ",k=""} 1`, `m{l=" "} 1`},
		{`m{"l.1"="","k k"="v"} 1`, `m{"k k"="v"} 1`},
		{`{"my.metric",l="",k="v"} 1`, `{"my.metric",k="v"} 1`},
		{`{l="","my.metric"} 1`, `{"my.metric"} 1`},
		{`m{l="",k="a\\b\"c\nd"} 1`, `m{k="a\\b\"c\nd"} 1`},
		{"m{l=\"\",k=\"\xff\"} 1", "m{k=\"\xff\"} 1"},
		{"m{l=\"\",k=\"v\"} 1\nm{l=\"x\",k=\"v\"} 2\nm{k=\"w\",l=\"\"} 3\n", "m{k=\"v\"} 1\nm{l=\"x\",k=\"v\"} 2\nm{k=\"w\"} 3\n"},
		// le and quantile are labels as any other where the series has no
		// buckets or quantiles.
		{"# TYPE g gauge\ng{le=\"\",quantile=\"\",k=\"v\"} 1\n", "# TYPE g gauge\ng{k=\"v\"} 1\n"},
		{`u{le=""} 1`, `u 1`},
		{"# TYPE c counter\nc{quantile=\"\"} 1\n", "# TYPE c counter\nc 1\n"},
		{"# TYPE h histogram\nh_bucket{quantile=\"\",le=\"1\"} 1\nh_bucket{le=\"+Inf\",quantile=\"\"} 2\nh_sum{quantile=\"\"} 3\nh_count{quantile=\"\"} 2\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n"},
		{"# TYPE s summary\ns{le=\"\",quantile=\"0.5\"} 1\ns_sum{le=\"\"} 3\ns_count{le=\"\"} 2\n", "# TYPE s summary\ns{quantile=\"0.5\"} 1\ns_sum 3\ns_count 2\n"},
		// Histograms and summaries: the label is off every sample, so the
		// samples are one series' as they were.
		{"# TYPE h histogram\nh_bucket{l=\"\",k=\"v\",le=\"1\"} 1\nh_bucket{le=\"+Inf\",k=\"v\",l=\"\"} 2\nh_sum{k=\"v\",l=\"\"} 3\nh_count{l=\"\",k=\"v\"} 2\n", "# TYPE h histogram\nh_bucket{k=\"v\",le=\"1\"} 1\nh_bucket{le=\"+Inf\",k=\"v\"} 2\nh_sum{k=\"v\"} 3\nh_count{k=\"v\"} 2\n"},
		{"# TYPE s summary\ns{l=\"\",quantile=\"0.5\"} 1\ns{quantile=\"0.9\",l=\"\"} 2\ns_sum{l=\"\"} 3\ns_count{l=\"\"} 4\n", "# TYPE s summary\ns{quantile=\"0.5\"} 1\ns{quantile=\"0.9\"} 2\ns_sum 3\ns_count 4\n"},
		// A histogram some of whose samples have the label and some not was
		// two series, one of buckets and one of a sum and a count; it is
		// the one histogram.
		{"# TYPE h histogram\nh_bucket{l=\"\",le=\"1\"} 1\nh_bucket{l=\"\",le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n"},
		// A sample that is no part of its family is left out as it was.
		{"# TYPE h histogram\nh{l=\"\"} 1\nh_bucket{l=\"\"} 2\nh_count{l=\"\"} 1\n", "# TYPE h histogram\nh 1\nh_bucket 2\nh_count 1\n"},
		// OpenMetrics: an exemplar's labels are not read, a _created sample
		// is dropped with its labels, and an info's, a state set's and a
		// gauge histogram's samples are gauges, le and all.
		{"# TYPE c counter\nc_total{l=\"\",k=\"v\"} 1 # {trace_id=\"\"} 0.5\nc_created{l=\"\",k=\"v\"} 5\n# EOF\n", "# TYPE c counter\nc_total{k=\"v\"} 1 # {trace_id=\"\"} 0.5\nc_created{k=\"v\"} 5\n# EOF\n"},
		{"# TYPE i info\ni_info{version=\"\",build=\"7\"} 1\n# EOF\n", "# TYPE i info\ni_info{build=\"7\"} 1\n# EOF\n"},
		{"# TYPE st stateset\nst{st=\"\"} 1\nst{st=\"b\"} 0\n# EOF\n", "# TYPE st stateset\nst 1\nst{st=\"b\"} 0\n# EOF\n"},
		{"# TYPE g gaugehistogram\ng_bucket{le=\"\"} 1\ng_bucket{le=\"+Inf\",l=\"\"} 2\ng_gcount{l=\"\"} 2\n# EOF\n", "# TYPE g gaugehistogram\ng_bucket 1\ng_bucket{le=\"+Inf\"} 2\ng_gcount 2\n# EOF\n"},
		// Series told apart by nothing but the label are the same series
		// twice, as the two written without it are.
		{"m{l=\"\"} 1\nm 2\n", "m 1\nm 2\n"},
		{"m{l=\"\",k=\"v\"} 1\nm{k=\"v\",j=\"\"} 2\n", "m{k=\"v\"} 1\nm{k=\"v\"} 2\n"},
		{"# TYPE h histogram\nh_bucket{l=\"\",le=\"+Inf\"} 1\nh_count{l=\"\"} 1\nh_bucket{le=\"+Inf\"} 1\nh_count 1\n", "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_count 1\nh_bucket{le=\"+Inf\"} 1\nh_count 1\n"},
		{"# TYPE h histogram\nh_bucket{l=\"\",le=\"1\"} 1\nh_bucket{le=\"1\"} 1\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"1\"} 1\n"},
		{"# TYPE s summary\ns_sum{l=\"\"} 1\ns_sum 2\n", "# TYPE s summary\ns_sum 1\ns_sum 2\n"},
	}
	accepted, refused := 0, 0
	for _, pair := range pairs {
		if pair[0] == pair[1] || !mayHoldAnEmptyLabel([]byte(pair[0])) {
			t.Fatalf("%q has no empty label that %q is without", pair[0], pair[1])
		}
		for _, reading := range promReadings {
			if sameAsWithout(t, pair[0], pair[1], reading) {
				accepted++
			} else {
				refused++
			}
		}
	}
	if accepted < 100 || refused < 20 {
		t.Errorf("%d readings are accepted and %d refused: the table should hold both", accepted, refused)
	}
}

// What an empty label was refused for it still is, to the word: the label is
// read and checked before it is left off. A name written twice is an error
// when one of the two values is empty and when both are, a histogram's le
// and a summary's quantile that are empty are no numbers, also on a _created
// sample that is then dropped, and __name__ is reserved. A line that is
// refused for something else is refused for it, whatever labels it has.
func TestAnEmptyLabelIsStillRefusedForWhatItWas(t *testing.T) {
	for body, want := range map[string]string{
		`m{l="",l="x"} 1`:  `text format parsing error in line 1: duplicate label name "l"`,
		`m{l="x",l=""} 1`:  `text format parsing error in line 1: duplicate label name "l"`,
		`m{l="",l=""} 1`:   `text format parsing error in line 1: duplicate label name "l"`,
		`m{"l"="",l=""} 1`: `text format parsing error in line 1: duplicate label name "l"`,
		"# TYPE h histogram\nh_bucket{le=\"\"} 1\n":         `text format parsing error in line 2: expected float as value for 'le' label, got ""`,
		"# TYPE h histogram\nh_bucket{l=\"\",le=\"\"} 1\n":  `text format parsing error in line 2: expected float as value for 'le' label, got ""`,
		"# TYPE s summary\ns{quantile=\"\"} 1\n":            `text format parsing error in line 2: expected float as value for 'quantile' label, got ""`,
		"# TYPE h histogram\nh_created{le=\"\"} 5\n# EOF\n": `text format parsing error in line 2: expected float as value for 'le' label, got ""`,
		`m{__name__=""} 1`:  `text format parsing error in line 1: label name "__name__" is reserved`,
		`m{l="",k="v"} x`:   `text format parsing error in line 1: expected float as value, got "x"`,
		`m{l="",k="v"} 1 x`: `text format parsing error in line 1: expected integer as timestamp, got "x"`,
		`m{l="",k=} 1`:      `text format parsing error in line 1: expected '"' at start of the value of label "k"`,
		`m{l=""`:            `text format parsing error in line 1: unexpected end of label set after label "l"`,
		`{l=""} 1`:          `text format parsing error in line 1: invalid metric name`,
		"# TYPE h histogram\nh_count{l=\"\"} 1.5\n":                         `text format parsing error in line 2: expected a whole number as the count for "h", got 1.5`,
		"# TYPE h histogram\nh_bucket{l=\"\",le=\"1\"} -1\n":                `text format parsing error in line 2: expected a count from 0 to 2^64-1 for "h", got -1`,
		"# TYPE h histogram\nh_sum{l=\"\"} 1\nh_sum{l=\"\"} 2\n":            `text format parsing error in line 3: second h_sum sample for the histogram h`,
		"# TYPE s summary\ns_count{l=\"\",k=\"v\"} 1\ns_count{k=\"v\"} 2\n": `text format parsing error in line 3: second s_count sample for the summary s{k="v"}`,
	} {
		_, err := parseExposition([]byte(body), promOptions{openMetrics: strings.HasSuffix(body, "# EOF\n")})
		if err == nil || err.Error() != want {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
	// What is refused before any label is left off is refused in the words
	// of the parser as it was, reading the body itself.
	for _, body := range []string{`m{l="",l="x"} 1`, `m{l="",l=""} 1`, "# TYPE h histogram\nh_bucket{le=\"\"} 1\n", "# TYPE s summary\ns{quantile=\"\"} 1\n", `m{__name__=""} 1`, `m{l="",k="v"} x`, `m{l=""`, `{l=""} 1`} {
		for _, reading := range promReadings {
			if sameAsWithout(t, body, body, reading) {
				t.Errorf("%q (%s) is accepted", body, reading)
			}
		}
	}
}

// Two series a target tells apart by nothing but a label with an empty
// value are one series to Prometheus, and are the same series twice once
// the label is off: the decoder returns both, without labels, and the check
// every scrape's series pass refuses them as the duplicates they are, as it
// refuses m written twice (model.MetricSet.Validate). It refused them
// before, when the decoder kept the label, since it always read such a
// label as none; it said then that the empty label was why. A histogram's
// samples told apart so are one histogram's, which has its _sum twice, and
// are refused where they are read.
func TestSeriesToldApartByAnEmptyLabelAreOneSeries(t *testing.T) {
	for _, body := range []string{"m{l=\"\"} 1\nm 2\n", "m{l=\"\",k=\"v\"} 1\nm{k=\"v\"} 2\n", "m{k=\"v\"} 1\nm{k=\"v\",j=\"\"} 2\n", "m 1\nm 2\n"} {
		metrics, err := parsePrometheusText([]byte(body))
		if err != nil || len(metrics) != 2 || !reflect.DeepEqual(metrics[0].Labels, metrics[1].Labels) {
			t.Fatalf("%q: %v, %+v, want two series with the same labels", body, err, metrics)
		}
		for name, value := range metrics[0].Labels {
			if value == "" {
				t.Errorf("%q: the series has the label %s with an empty value", body, name)
			}
		}
		if err := (&model.MetricSet{Metrics: metrics}).Validate(model.Limits{}); err == nil || err.Error() != `duplicate metric series "m"` {
			t.Errorf("%q: checked as %v, want the duplicate refused", body, err)
		}
	}
	twice := "# TYPE h histogram\nh_bucket{l=\"\",le=\"+Inf\"} 1\nh_sum{l=\"\"} 1\nh_count{l=\"\"} 1\nh_bucket{le=\"+Inf\"} 1\nh_sum 1\nh_count 1\n"
	if _, err := parsePrometheusText([]byte(twice)); err == nil || err.Error() != "text format parsing error in line 6: second h_sum sample for the histogram h" {
		t.Errorf("%q: err=%v, want the second _sum refused", twice, err)
	}
	if old, err := oracleParseExposition([]byte(twice), promOptions{}); err != nil || len(old) != 2 {
		t.Errorf("%q was read as %+v, %v: the parser it was is to make two series of it", twice, old, err)
	}
}

// Leaving a label off costs the parser nothing: the label is read where it
// lies, as every label is, and dropped from the list the sample's labels
// are read into before a map is made of them. An exposition of plain series
// and of histograms whose every sample has two labels with empty values
// costs the allocations of the same exposition without them, and so no
// more for each series than the parser's bound (decodeallocs_test.go).
func TestLeavingEmptyLabelsOffAllocatesNothing(t *testing.T) {
	const series, histograms = 1000, 100
	var with, without strings.Builder
	for i := 0; i < series; i++ {
		fmt.Fprintf(&with, "node_cpu_seconds_total{zone=\"\",cpu=\"%d\",mode=\"m%d\",rack=\"\"} %d.25\n", i/8, i%8, i*3)
		fmt.Fprintf(&without, "node_cpu_seconds_total{cpu=\"%d\",mode=\"m%d\"} %d.25\n", i/8, i%8, i*3)
	}
	with.WriteString("# TYPE h histogram\n")
	without.WriteString("# TYPE h histogram\n")
	for i := 0; i < histograms; i++ {
		for b, le := range []string{"0.1", "1", "+Inf"} {
			fmt.Fprintf(&with, "h_bucket{zone=\"\",handler=\"/h%d\",le=\"%s\",rack=\"\"} %d\n", i, le, b)
			fmt.Fprintf(&without, "h_bucket{handler=\"/h%d\",le=\"%s\"} %d\n", i, le, b)
		}
		fmt.Fprintf(&with, "h_sum{rack=\"\",handler=\"/h%d\",zone=\"\"} 12.5\nh_count{handler=\"/h%d\",zone=\"\",rack=\"\"} 2\n", i, i)
		fmt.Fprintf(&without, "h_sum{handler=\"/h%d\"} 12.5\nh_count{handler=\"/h%d\"} 2\n", i, i)
	}
	if !sameAsWithout(t, with.String(), without.String(), promReading{}) {
		t.Fatal("the exposition is refused")
	}
	c := model.Collector{Decoder: model.DecoderConfig{Type: "prometheus"}}
	labelled := allocationsFor(t, c, "text/plain; version=0.0.4", with.String(), series+histograms)
	plain := allocationsFor(t, c, "text/plain; version=0.0.4", without.String(), series+histograms)
	if labelled > plain+0.01 || labelled > 5 {
		t.Fatalf("%.2f allocations for each series with empty labels, and %.2f without them: want no more, and at most 5", labelled, plain)
	}
}

// emptyLabelEntry is a label as a generated line writes it: its name as
// written, quoted or bare, the blanks around its =, and its value as
// written, escapes and all.
type emptyLabelEntry struct{ name, before, after, value string }

// emptyLabelGenerator writes an exposition twice: as a target writes it,
// some of its labels with empty values, and without those labels.
type emptyLabelGenerator struct {
	random        *rand.Rand
	with, without strings.Builder
	// left counts the labels left out of without.
	left int
}

func (g *emptyLabelGenerator) pick(choices ...string) string {
	return choices[g.random.IntN(len(choices))]
}

func (g *emptyLabelGenerator) rarely(n int) bool { return g.random.IntN(n) == 0 }

// both writes the same text to the two expositions.
func (g *emptyLabelGenerator) both(parts ...string) {
	for _, part := range parts {
		g.with.WriteString(part)
		g.without.WriteString(part)
	}
}

// labelSet is a series' labels: a few of few names, a third of them with an
// empty value, so that two series of a family often differ in nothing but
// such a label, after an id label when the series is given one. A family
// that reads le or quantile as its buckets' or quantiles' own (special) has
// no label of that name here.
func (g *emptyLabelGenerator) labelSet(special, id string) []emptyLabelEntry {
	names := []string{"a", "b", "method", "le", "quantile", `"x y"`, `"é.ü"`}
	var entries []emptyLabelEntry
	if id != "" {
		entries = append(entries, emptyLabelEntry{name: "id", value: id})
	}
	used := map[string]bool{}
	for i, n := 0, g.random.IntN(5); i < n; i++ {
		name := names[g.random.IntN(len(names))]
		// A name written twice is an error, wanted rarely.
		if name == special || used[name] && !g.rarely(60) {
			continue
		}
		used[name] = true
		value := g.pick("", "", "", "1", "1", "2", " ", "eu", `a\\b`, `\"`, `line\nbreak`, "é", "\xff")
		entries = append(entries, emptyLabelEntry{name: name, before: g.pick("", "", "", " ", "\t"), after: g.pick("", "", "", " "), value: value})
	}
	return entries
}

// sample writes a sample line of a series to the two expositions: its
// name, its labels, with special, an le or a quantile as written, among
// them when it is not empty, and the rest of the line. The second is
// without the labels whose values are empty, unless the line names a label
// twice: such a line is refused for that, and is written whole.
func (g *emptyLabelGenerator) sample(name string, entries []emptyLabelEntry, special, rest string) {
	twice, seen := false, map[string]bool{}
	for _, entry := range entries {
		twice = twice || seen[entry.name]
		seen[entry.name] = true
	}
	at := g.random.IntN(len(entries) + 1)
	braces, comma, bare := name[0] == '"' && !g.rarely(8), g.rarely(5), g.rarely(2)
	write := func(out *strings.Builder, keepEmpty bool) {
		var written []string
		if braces {
			written = append(written, name)
		}
		for i, entry := range entries {
			if i == at && special != "" {
				written = append(written, special)
			}
			if entry.value == "" && !keepEmpty {
				continue
			}
			written = append(written, entry.name+entry.before+"="+entry.after+`"`+entry.value+`"`)
		}
		if at == len(entries) && special != "" {
			written = append(written, special)
		}
		if !braces {
			out.WriteString(name)
		}
		if len(written) > 0 || !bare {
			out.WriteString("{")
			out.WriteString(strings.Join(written, ","))
			if comma && len(written) > 0 {
				out.WriteString(",")
			}
			out.WriteString("}")
		}
		out.WriteString(" ")
		out.WriteString(rest)
		out.WriteString("\n")
	}
	write(&g.with, true)
	write(&g.without, twice)
	if !twice {
		for _, entry := range entries {
			if entry.value == "" {
				g.left++
			}
		}
	}
}

// rest is a sample's value, as a count when it is one, and now and then a
// timestamp, an exemplar, or something that is none of these.
func (g *emptyLabelGenerator) rest(count bool) string {
	value := g.pick("0", "1", "2.5", "-1", "NaN", "+Inf", "1e3")
	if count {
		value = g.pick("0", "1", "2", "7", "7.0", "1e2")
	}
	switch g.random.IntN(200) {
	case 0:
		value = g.pick("x", "", "1_0", "1.5", "-2", "3", "4", "5")
	case 1:
		value += g.pick(" x", " 12.5", ` # {trace_id=""} 0.5`, ` # {a="b"} 1 1700000000.5`)
	case 2, 3, 4, 5, 6, 7, 8, 9, 10, 11:
		value += g.pick(" 1700000000000", " 1700000000", " -5", " 0")
	}
	return value
}

// again is the label set of the series before, entries, written again as
// another series that a target tells from it by nothing but labels with
// empty values: without some of those it has, and with one or two more.
func (g *emptyLabelGenerator) again(entries []emptyLabelEntry) []emptyLabelEntry {
	var out []emptyLabelEntry
	for _, entry := range entries {
		if entry.value != "" || g.rarely(2) {
			out = append(out, entry)
		}
	}
	for _, name := range []string{"zone", `"rack 1"`}[:1+g.random.IntN(2)] {
		at := g.random.IntN(len(out) + 1)
		out = append(out[:at], append([]emptyLabelEntry{{name: name}}, out[at:]...)...)
	}
	return out
}

// family writes a family: its TYPE, mostly, and a few series, each with a
// label set of its own. A histogram's or a summary's series has its samples
// one after the other, now and then without one of them, and each with the
// series' labels in the order it has them; its id label tells it from the
// family's others, but for one series in four, which is the one before it
// with other labels of an empty value (again).
func (g *emptyLabelGenerator) family(number int) {
	name := g.pick("requests", "up", "temp", "node:cpu", `"my.metric`) + strconv.Itoa(number)
	if name[0] == '"' {
		name += `"`
	}
	suffixed := func(suffix string) string {
		if name[0] == '"' {
			return name[:len(name)-1] + suffix + `"`
		}
		return name + suffix
	}
	kind := g.pick("counter", "gauge", "untyped", "histogram", "histogram", "summary", "summary", "none", "none")
	if g.rarely(25) {
		kind = g.pick("unknown", "info", "stateset", "gaugehistogram")
	}
	if kind != "none" {
		if g.rarely(2) {
			g.both("# HELP ", name, " Help of it.\n")
		}
		g.both("# TYPE ", name, " ", kind, "\n")
	}
	var entries []emptyLabelEntry
	for series, n := 0, 1+g.random.IntN(4); series < n; series++ {
		switch {
		case kind != "histogram" && kind != "summary":
		case series > 0 && g.rarely(4):
			entries = g.again(entries)
		default:
			entries = g.labelSet(map[string]string{"histogram": "le", "summary": "quantile"}[kind], strconv.Itoa(series))
		}
		switch kind {
		case "histogram":
			for _, bound := range []string{"0.5", "1", "+Inf"} {
				if g.rarely(4) {
					continue
				}
				if g.rarely(150) {
					bound = g.pick("x", "", "1.0")
				}
				g.sample(suffixed("_bucket"), entries, `le="`+bound+`"`, g.rest(true))
			}
			if !g.rarely(5) {
				g.sample(suffixed("_sum"), entries, "", g.rest(false))
			}
			if !g.rarely(5) {
				g.sample(suffixed("_count"), entries, "", g.rest(true))
			}
			if g.rarely(10) {
				g.sample(suffixed(g.pick("", "_created", "_bucket")), entries, "", g.rest(false))
			}
		case "summary":
			for _, quantile := range []string{"0.5", "0.99"} {
				if g.rarely(4) {
					continue
				}
				if g.rarely(150) {
					quantile = g.pick("x", "", "0.50")
				}
				g.sample(name, entries, `quantile="`+quantile+`"`, g.rest(false))
			}
			if !g.rarely(5) {
				g.sample(suffixed("_sum"), entries, "", g.rest(false))
			}
			if !g.rarely(5) {
				g.sample(suffixed("_count"), entries, "", g.rest(true))
			}
			if g.rarely(10) {
				g.sample(suffixed(g.pick("", "_created")), entries, "", g.rest(false))
			}
		case "gaugehistogram":
			// Its buckets are gauges, and their le a label as any other.
			g.sample(suffixed(g.pick("_bucket", "_gsum", "_gcount")), g.labelSet("", ""), "", g.rest(false))
		default:
			g.sample(suffixed(g.pick("", "", "", "", "_total", "_created", "_info", "_sum")), g.labelSet("", ""), "", g.rest(false))
		}
	}
}

// exposition writes one exposition of a few families and returns it as a
// target writes it and without its labels of an empty value.
func (g *emptyLabelGenerator) exposition() (with, without string) {
	g.with.Reset()
	g.without.Reset()
	for i, n := 0, 1+g.random.IntN(5); i < n; i++ {
		g.family(i)
		if g.rarely(10) {
			g.both(g.pick("# a comment\n", "\n", "# a comment\n", "\n", "# EOF\n", "junk\n"))
		}
	}
	if g.rarely(3) {
		g.both("# EOF\n")
	}
	return g.with.String(), g.without.String()
}

// 6,000 generated expositions, each written twice, as a target writes it
// and without the labels it gives an empty value, and each read two ways,
// as the text format or OpenMetrics, with a filter or without, with a limit
// or without: 12,000 times the parser reads the first as the parser it was
// reads the second, to the series, their order and the error's words. The
// expositions have histograms and summaries, complete and incomplete,
// quoted names, the braces form, escapes, timestamps, exemplars, labels
// named le and quantile on series that have none of their own, names
// written twice and values that are no numbers; a third of their labels
// are empty, and one histogram or summary series in four is the one before
// it but for such labels. Of the readings of an exposition that has such a
// label, the parser it was read an eighth at least otherwise when it kept
// the label, in more than the label: series that are one series now, or
// the error they make. An exposition that happens to have none is read as
// it was, as every one of the differential test's is (promdiff_test.go).
// Under the race detector 600 expositions, 1,200 readings.
func TestGeneratedExpositionsAreReadAsTheyWereWithoutTheirEmptyLabels(t *testing.T) {
	g := &emptyLabelGenerator{random: rand.New(rand.NewPCG(2026, 1006))}
	expositions := alloctest.UnlessRaced(6000, 600)
	accepted, refused, same, labelled, followed := 0, 0, 0, 0, 0
	for i := 0; i < expositions && !t.Failed(); i++ {
		g.left = 0
		with, without := g.exposition()
		if (g.left > 0) != (with != without) {
			t.Fatalf("%q is written as %q without its %d empty labels", with, without, g.left)
		}
		for _, reading := range []promReading{
			{openMetrics: g.random.IntN(2) == 0},
			{openMetrics: g.random.IntN(2) == 0, filtered: g.random.IntN(2) == 0, limit: g.random.IntN(3) * g.random.IntN(6)},
		} {
			if sameAsWithout(t, with, without, reading) {
				accepted++
			} else {
				refused++
			}
			if g.left == 0 {
				same++
				continue
			}
			labelled++
			// What the parser it was made of the exposition as written,
			// with the labels kept: other than now in more than the labels
			// when it has another number of series or another error.
			var asked []string
			kept, keptErr := oracleParseExposition([]byte(with), reading.options(&asked))
			got, err := parseExposition([]byte(with), reading.options(&asked))
			if len(kept) != len(got) || (keptErr == nil) != (err == nil) || err != nil && keptErr.Error() != err.Error() {
				followed++
			}
		}
	}
	t.Logf("%d readings accepted, %d refused; %d of expositions without an empty label, %d of expositions with one, of which the parser it was read %d otherwise in more than the label", accepted, refused, same, labelled, followed)
	if accepted < expositions/2 || refused < expositions/2 || same < expositions/20 || labelled < expositions || followed < labelled/8 {
		t.Fatal("the generator no longer writes expositions of every kind")
	}
}
