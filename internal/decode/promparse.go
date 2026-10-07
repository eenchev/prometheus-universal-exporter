package decode

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// parsePrometheusText parses the Prometheus text exposition format, version
// 0.0.4, which is what the prometheus decoder reads. It replaces the parser in
// github.com/prometheus/common/expfmt, which brought protobuf and four other
// modules into the binary for this one function, and it follows that parser's
// rules:
//
//   - A sample's family is the one named by an earlier HELP or TYPE line, or by
//     the sample itself. For a summary or histogram family foo, the series
//     foo_sum and foo_count, and for a histogram foo_bucket, belong to foo.
//   - A family with no TYPE line is untyped, and a TYPE line after the family's
//     first sample, or a second HELP or TYPE line, is an error.
//   - Summary and histogram series are grouped by their labels without
//     quantile and le. A quantile or le label must be a float.
//   - Metric and label names may be quoted UTF-8, including the braces form
//     {"my.metric", key="value"} for a metric name that is not a bare name.
//   - HELP text and label values unescape \\, \n and \"; any other escape is an
//     error.
//   - A value is a Go float without p, P or _, so NaN, +Inf and -Inf are
//     accepted; a timestamp is an integer of milliseconds.
//   - Comments other than HELP and TYPE, including OpenMetrics' # EOF, are
//     ignored, and families that end up without samples are dropped.
//
// It is more lenient than expfmt in three ways that real endpoints trip over:
// the last line need not end in a newline, lines may end in \r\n, and trailing
// blanks after the value or timestamp are ignored. It is stricter where expfmt
// produced nonsense: a histogram or summary count, or a bucket count, that is
// negative, NaN, infinite or a fraction is an error instead of an arbitrary
// integer; a
// label set with no metric name is an error instead of joining the previous
// line's family; and a name that mixes bare and quoted parts, such as a"b", is
// an error instead of being spliced together. It never panics, where expfmt
// did on input such as {b="c",} 1.
//
// A histogram or a summary is kept as the source wrote it (settle): one
// without _sum or without _count, which OpenMetrics allows and expfmt read as
// a sum or a count of 0, is marked as having none, a histogram's count then
// being its +Inf bucket's. A histogram whose +Inf bucket and _count differ
// keeps both numbers, and one with neither of the two has neither
// (model.Histogram): a target that updates its buckets and its count without
// a lock is read between the two now and then, and one such series must not
// fail every family of the scrape. What cannot be one series is an error
// naming it: a second _sum or _count, and two buckets with one bound or two
// values of one quantile.
//
// A sample line that is no part of its family is left out, and counted
// (PrometheusReport): a sample of a histogram that is neither a bucket with
// an le label, a _sum nor a _count, such as one named as the family itself,
// and a sample named as its summary family without a quantile label. Client
// libraries write such lines, and Prometheus ingests them: Micrometer writes
// a timer's percentiles as x{quantile="0.95"} under "# TYPE x histogram", and
// VictoriaMetrics' library its buckets as x_bucket{vmrange="..."}. Failing
// the decode over one lost every family of such a target. The line cannot be
// passed on either: its series would be a second family named as the
// histogram, which an exposition cannot hold. expfmt made an empty series of
// it.
//
// A label written with an empty value is left off its series (promParser.
// empty): to Prometheus m{l=""} is the series m, and the exporter leaves an
// empty label off wherever one comes from. The label is read and checked as
// any other first, so a name written twice is still an error, and so is a
// histogram's le or a summary's quantile that is empty, which is no number.
// Two series that differ only in such a label are one series: a histogram's
// or a summary's samples are then one series' samples, refused where that
// series has a sample twice, and two plain series are the duplicates the
// check of every scrape refuses (model.MetricSet.Validate).
//
// Families come back in the order they were first seen, and series in the
// order of their first sample, so a decode is deterministic.
func parsePrometheusText(body []byte) ([]model.Metric, error) {
	return parseExposition(body, promOptions{})
}

// PrometheusReport is what the prometheus decoder left out of an exposition.
type PrometheusReport struct {
	// LeftOutLines counts the sample lines left out as no part of their
	// histogram or summary family, in the families that are kept, and
	// FirstLeftOut says which was first and what was expected in its place:
	// an error, so that the log takes it for the same when only the line it
	// names differs (model.SameFailureText).
	LeftOutLines int
	FirstLeftOut error
}

// promOptions says how to read an exposition, and which of its series to
// keep.
type promOptions struct {
	// openMetrics reads the body as OpenMetrics 1.0 text rather than the
	// text format 0.0.4 (openmetrics.go).
	openMetrics bool
	// keep, when set, says whether the series of a metric name are kept;
	// the others are parsed, and so checked, but not stored.
	keep func(name string) bool
	// limit, when above 0, is how many series may be kept: the parse stops
	// at the first one past it with model.MetricCountError.
	limit int
	// counts, when set, says whether the kept series of a metric name and
	// type count against limit: those of a family it answers false for are
	// kept without being counted, for whoever is given the series to count
	// what it makes of them.
	counts func(name string, typ model.MetricType) bool
}

// parseExposition parses body as options say.
//
// A large exposition is most of what a pass-through probe costs, so the
// parser is written to do little for each line and to keep little of it:
//
//   - The body is read where it lies, a line at a time, as bytes: no line is
//     copied to a string, and no slice of every line is made first. Only
//     what a kept series holds is copied out of the body, so that the series
//     of a scrape that keeps ten of a million do not hold on to the rest.
//   - A sample's labels are read into a list the parser uses again for every
//     line (promParser.labels), and are made the series' map only when a
//     series is made of them: not for a sample of a family that is not
//     kept, and not for the second and later buckets of a histogram, which
//     find their series by a signature written over the same buffer every
//     time.
//   - The text of a label's name, and of its value, is the previous line's
//     where it is the same (promParser.previous), as the names nearly always
//     are and the values often.
//   - Series are held in one slice in the order they were read
//     (promParser.series), sized from the number of lines when every series
//     is kept, and are put in the families' order at the end only when they
//     were not read in it, which is rare.
func parseExposition(body []byte, options promOptions) ([]model.Metric, error) {
	metrics, _, err := parseExpositionReporting(body, options)
	return metrics, err
}

// parseExpositionReporting is parseExposition, and reports the sample lines
// it left out when there are any.
func parseExpositionReporting(body []byte, options promOptions) ([]model.Metric, *PrometheusReport, error) {
	p := promParser{byName: map[string]*promFamily{}, options: options}
	if options.keep == nil {
		// Without a filter every sample line may be a series, and most
		// are: the slice is made once, for as many series as there are
		// lines, or as many as may be kept. The estimate is capped, since
		// an exposition of histograms has ten lines to a series; past it
		// the slice grows as any does. With a filter nothing says how few
		// are kept, and the slice starts empty.
		estimate := min(bytes.Count(body, []byte("\n"))+1, promSeriesEstimate)
		if options.limit > 0 {
			estimate = min(estimate, options.limit)
		}
		p.series = make([]promSeries, 0, estimate)
	}
	for number := 1; ; number++ {
		line, more := body, false
		if end := bytes.IndexByte(body, '\n'); end >= 0 {
			line, body, more = body[:end], body[end+1:], true
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		p.number = number
		if err := p.line(line); err != nil {
			if errors.Is(err, model.ErrLimitExceeded) {
				return nil, nil, err
			}
			return nil, nil, model.Errorf("text format parsing error in line %d: %w", model.Position(number), err)
		}
		if !more {
			break
		}
	}
	if err := p.settle(); err != nil {
		return nil, nil, fmt.Errorf("text format parsing error: %w", err)
	}
	return p.metrics(), p.report, nil
}

// promSeriesEstimate is the most series the parser makes room for before it
// has read any.
const promSeriesEstimate = 1 << 16

const (
	promRoleNone = iota
	promRoleSum
	promRoleCount
	// promRoleCreated is an OpenMetrics _created sample, which is read and
	// dropped: the metric model has no creation time.
	promRoleCreated
)

type promFamily struct {
	name    string
	help    string
	helpSet bool
	typ     model.MetricType // empty until a TYPE line or the first sample
	// index is the family's place among the families, in the order they
	// were first seen, which is the order their series come back in.
	index int
	// grouped finds a summary's or a histogram's series, in
	// promParser.series, by its signature, and latest is the one made last.
	grouped map[string]int
	latest  int
	// exported, when set, is the name the family's series have, where
	// OpenMetrics names them other than the family: foo_total for a
	// counter foo, foo_info for an info foo.
	exported string
	// helpOf, when set, is the family whose HELP this one's series carry:
	// an OpenMetrics gauge histogram's, for the gauges it is read as.
	helpOf *promFamily
	// kept says whether the family's series are stored, once decided by
	// promOptions.keep at its first series, and uncounted that they are
	// stored without counting against promOptions.limit (promOptions.counts).
	kept, keptKnown bool
	uncounted       bool
}

// exportedName is the name of the family's series.
func (f *promFamily) exportedName() string {
	if f.exported != "" {
		return f.exported
	}
	return f.name
}

type promSeries struct {
	family    *promFamily
	labels    map[string]string
	value     float64
	timestamp *int64
	histogram *model.Histogram
	summary   *model.Summary
	// line is the line of a histogram's or summary's first sample, for the
	// error of one that cannot be a series (settle).
	line int
}

type promParser struct {
	byName map[string]*promFamily
	// families counts the families, for promFamily.index.
	families int
	options  promOptions
	// aliases are the OpenMetrics sample names that belong to a family
	// other than their own, with the role they have in it.
	aliases map[string]promAlias
	// series are the series kept, in the order their first samples were
	// read, and unordered says that this is not the families' order.
	// uncounted is how many of them do not count against the limit.
	series    []promSeries
	unordered bool
	uncounted int
	// eof is set by OpenMetrics' # EOF, after which nothing may follow.
	eof bool
	// number is the line being read.
	number int
	// report is what was left out, and nil while nothing was.
	report *PrometheusReport

	// What follows is used again for every line, so that a line costs no
	// allocation of its own.

	// labels are the labels of the sample being read, as they are written
	// in the body or, when they have escapes, unescaped in unescaped.
	labels    []promLabel
	unescaped []byte
	// empty says that one of labels has an empty value, which add leaves
	// off: nearly no sample has one, and it is then all a sample pays.
	empty bool
	// seen holds the label names of a sample with more labels than
	// promLabelsCompared, to find one written twice.
	seen map[string]struct{}
	// signature is where a histogram's or a summary's sample writes what
	// identifies its series (promSignature).
	signature []byte
	// previous are the label names and values of the last series made, by
	// their place in the label set. The next series mostly has the same
	// names, and often some of the same values, and takes these strings
	// rather than making its own.
	previous []promLabelText
	// sampleName, sampleFamily and sampleRole are the name of the sample
	// read last and what family found it to be, which the next sample has
	// too when it has the same name, as within a family it has. No comment
	// line may lie between the two: a TYPE line changes what a name is.
	sampleName   []byte
	sampleFamily *promFamily
	sampleRole   int
}

type promAlias struct {
	family *promFamily
	role   int
}

func (p *promParser) line(line []byte) error {
	s := skipBlanks(line)
	if blankLine(s) {
		return nil
	}
	if p.eof {
		return errors.New("unexpected content after # EOF")
	}
	p.unescaped = p.unescaped[:0]
	if s[0] == '#' {
		return p.comment(s[1:])
	}
	return p.sample(s)
}

// blankLine reports whether s, which starts with neither a space nor a tab,
// holds nothing but white space, as a form feed or a no-break space is. A
// line nearly always starts with a letter or a #, which settles it.
func blankLine(s []byte) bool {
	if len(s) == 0 {
		return true
	}
	if c := s[0]; c > ' ' && c < utf8.RuneSelf {
		return false
	}
	return len(bytes.TrimSpace(s)) == 0
}

// comment handles a # line. Only HELP and TYPE mean anything; a HELP or TYPE
// line that stops after its keyword or after its metric name is not an error,
// as it is not for expfmt.
func (p *promParser) comment(s []byte) error {
	s = skipBlanks(s)
	keyword, rest := cutBlank(s)
	if p.options.openMetrics && string(keyword) == "EOF" && len(bytes.TrimSpace(rest)) == 0 {
		p.eof = true
		return nil
	}
	if string(keyword) != "HELP" && string(keyword) != "TYPE" {
		return nil
	}
	// A TYPE line changes which family a name belongs to.
	p.sampleFamily = nil
	rest = skipBlanks(rest)
	if len(rest) == 0 {
		return nil
	}
	name, rest, err := p.readName(rest, promMetricNameStart, promMetricNameByte)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return nil
	}
	if !isBlank(rest[0]) {
		return errors.New("invalid metric name in comment")
	}
	family, _, err := p.family(name)
	if err != nil {
		return err
	}
	rest = skipBlanks(rest)
	if len(rest) == 0 {
		return nil
	}
	if string(keyword) == "HELP" {
		if family.helpSet {
			return model.Errorf("second HELP line for metric name %s", model.Quoted(family.name))
		}
		help, err := unescapePromText(string(rest), "help text")
		if err != nil {
			return err
		}
		family.help, family.helpSet = help, true
		return nil
	}
	if family.typ != "" {
		return model.Errorf("second TYPE line for metric name %s, or TYPE reported after samples", model.Quoted(family.name))
	}
	raw := string(rest)
	t := strings.ToLower(strings.TrimRight(raw, " \t"))
	if p.options.openMetrics {
		return p.openMetricsType(family, t, raw)
	}
	switch t {
	case "counter":
		family.typ = model.CounterMetricType
	case "gauge":
		family.typ = model.GaugeMetricType
	case "summary":
		family.typ = model.SummaryMetricType
	case "histogram":
		family.typ = model.HistogramMetricType
	case "untyped":
		family.typ = model.UntypedMetricType
	default:
		return model.Errorf("unknown metric type %s", model.Quoted(raw))
	}
	return nil
}

// sample handles a sample line: a name, optional labels, a value and an
// optional timestamp.
func (p *promParser) sample(s []byte) error {
	var (
		name []byte
		err  error
	)
	p.labels, p.empty = p.labels[:0], false
	if s[0] == '{' {
		name, s, err = p.readLabels(s[1:], true)
		if err != nil {
			return err
		}
		if len(name) == 0 {
			return errors.New("invalid metric name")
		}
	} else {
		name, s, err = p.readName(s, promMetricNameStart, promMetricNameByte)
		if err != nil {
			return err
		}
		if len(name) == 0 {
			return errors.New("invalid metric name")
		}
		s = skipBlanks(s)
		if len(s) != 0 && s[0] == '{' {
			if _, s, err = p.readLabels(s[1:], false); err != nil {
				return err
			}
		}
	}
	s = skipBlanks(s)
	token, s := cutBlank(s)
	value, err := parsePromFloat(token)
	if err != nil {
		return model.Errorf("expected float as value, got %s", model.Quoted(token))
	}
	if p.options.openMetrics {
		s = withoutExemplar(s)
	}
	var (
		timestamp int64
		timed     bool
	)
	s = skipBlanks(s)
	if len(s) != 0 {
		token, s = cutBlank(s)
		if p.options.openMetrics {
			timestamp, err = openMetricsTimestamp(token)
			if err != nil {
				return err
			}
		} else {
			timestamp, err = strconv.ParseInt(string(token), 10, 64)
			if err != nil {
				return model.Errorf("expected integer as timestamp, got %s", model.Quoted(token))
			}
		}
		timed = true
		if s = bytes.TrimSpace(s); len(s) != 0 {
			return model.Errorf("spurious string after timestamp: %s", model.Quoted(s))
		}
	}
	family, role := p.sampleFamily, p.sampleRole
	if family == nil || !bytes.Equal(name, p.sampleName) {
		family, role, err = p.family(name)
		if err != nil {
			return err
		}
		// The name is copied: the body's may be one unescaped into a
		// buffer the next line writes over.
		p.sampleName = append(p.sampleName[:0], name...)
		p.sampleFamily, p.sampleRole = family, role
	}
	if family.typ == "" {
		family.typ = model.UntypedMetricType
	}
	return p.add(family, role, value, timestamp, timed)
}

// family finds the family a name belongs to, creating it if there is none.
// The role says whether the name is a summary's or histogram's _sum or _count.
// The name is made a string only for a new family: a map is looked up by a
// string made of bytes without the string being allocated.
func (p *promParser) family(name []byte) (*promFamily, int, error) {
	if len(name) == 0 || !utf8.Valid(name) {
		return nil, 0, model.Errorf("invalid metric name %s", model.Quoted(name))
	}
	if alias, ok := p.aliases[string(name)]; ok {
		return alias.family, alias.role, nil
	}
	if f := p.byName[string(name)]; f != nil {
		return f, promRoleNone, nil
	}
	role := promRoleNone
	base := name
	switch {
	case len(name) > len("_count") && bytes.HasSuffix(name, []byte("_count")):
		role, base = promRoleCount, name[:len(name)-len("_count")]
	case len(name) > len("_sum") && bytes.HasSuffix(name, []byte("_sum")):
		role, base = promRoleSum, name[:len(name)-len("_sum")]
	}
	if f := p.byName[string(base)]; f != nil && f.typ == model.SummaryMetricType {
		return f, role, nil
	}
	if role == promRoleNone && len(name) > len("_bucket") && bytes.HasSuffix(name, []byte("_bucket")) {
		base = name[:len(name)-len("_bucket")]
	}
	if f := p.byName[string(base)]; f != nil && f.typ == model.HistogramMetricType {
		return f, role, nil
	}
	return p.newFamily(&promFamily{name: string(name)}), promRoleNone, nil
}

// newFamily adds a family, after those there are.
func (p *promParser) newFamily(f *promFamily) *promFamily {
	f.index = p.families
	p.families++
	p.byName[f.name] = f
	return f
}

// add adds the sample read, whose labels are p.labels, to its family, or
// checks it and drops it when the family is not kept or the sample is an
// OpenMetrics _created.
func (p *promParser) add(f *promFamily, role int, value float64, timestamp int64, timed bool) error {
	if !f.keptKnown {
		f.kept = p.options.keep == nil || p.options.keep(f.exportedName())
		// The family's type is what it stays: a TYPE line after a sample
		// is refused.
		f.uncounted = f.kept && p.options.counts != nil && !p.options.counts(f.exportedName(), f.typ)
		f.keptKnown = true
	}
	special := ""
	switch f.typ {
	case model.SummaryMetricType:
		special = "quantile"
	case model.HistogramMetricType:
		special = "le"
	}
	// A summary's quantile label, and a histogram's le, is not a label of
	// the series: it is taken out of p.labels, which then are the series'
	// own, and read as the number it is.
	bound, hasBound := math.NaN(), false
	if special != "" {
		for i, l := range p.labels {
			if string(l.name) != special {
				continue
			}
			b, err := parsePromFloat(l.value)
			if err != nil {
				return model.Errorf("expected float as value for '%s' label, got %s", special, model.Quoted(l.value))
			}
			bound, hasBound = b, true
			p.labels = append(p.labels[:i], p.labels[i+1:]...)
			break
		}
	}
	if p.empty {
		// A label with an empty value is no label of the series. It is left
		// off here, once le or quantile is taken out, which is refused when
		// it is empty rather than left off, and before anything is made of
		// the labels: the series' map and a histogram's signature.
		p.labels = slices.DeleteFunc(p.labels, func(l promLabel) bool { return len(l.value) == 0 })
	}
	if role == promRoleCreated {
		return nil
	}
	if role == promRoleCount || (hasBound && f.typ == model.HistogramMetricType) {
		// A count is a whole number of observations, which a uint64 holds
		// up to 2^64-1; past it the conversion gives a meaningless number.
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value >= math.MaxUint64 {
			return model.Errorf("expected a count from 0 to 2^64-1 for %s, got %v", model.Quoted(f.name), value)
		}
		// Nor is it a fraction: 1.5 observations would be passed on as 1,
		// a number the target never reported.
		if value != math.Trunc(value) {
			return model.Errorf("expected a whole number as the count for %s, got %v", model.Quoted(f.name), value)
		}
	}
	if !f.kept {
		// Checked as a kept sample is, and dropped: nothing of it is held.
		return nil
	}
	if special != "" && role == promRoleNone && !hasBound {
		// A summary or histogram sample that is neither _sum, _count, a
		// quantile nor a bucket holds nothing a series of the family has: its
		// value has no place. It makes no series and is left out, counted.
		p.leaveOut(f)
		return nil
	}
	if special == "" {
		series, err := p.keep(f)
		if err != nil {
			return err
		}
		series.value = value
		if timed {
			// A copy, made only here: a parameter whose address is taken
			// is allocated for every call, with a time or without.
			at := timestamp
			series.timestamp = &at
		}
		return nil
	}
	var series *promSeries
	p.signature = promSignature(p.signature[:0], p.labels)
	if at, ok := f.grouped[string(p.signature)]; ok {
		series = &p.series[at]
	} else {
		// As many buckets or quantiles as the family's series before this
		// one had, which is how many this one will have.
		size := 0
		if len(f.grouped) > 0 {
			if latest := &p.series[f.latest]; latest.summary != nil {
				size = len(latest.summary.Quantiles)
			} else {
				size = len(latest.histogram.Buckets)
			}
		}
		var err error
		if series, err = p.keep(f); err != nil {
			return err
		}
		// The series has no _sum and no _count until one is read.
		series.line = p.number
		if f.typ == model.SummaryMetricType {
			series.summary = &model.Summary{NoSum: true, NoCount: true}
			if size > 0 {
				series.summary.Quantiles = make([]model.Quantile, 0, size)
			}
		} else {
			series.histogram = &model.Histogram{NoSum: true, NoCount: true}
			if size > 0 {
				series.histogram.Buckets = make([]model.Bucket, 0, size)
			}
		}
		if f.grouped == nil {
			f.grouped = map[string]int{}
		}
		f.latest = len(p.series) - 1
		f.grouped[string(p.signature)] = f.latest
	}
	if timed {
		// The series' time is its last sample's. It is held by nothing
		// but the series, and so is written over rather than made anew
		// for each of a histogram's buckets.
		if series.timestamp == nil {
			at := timestamp
			series.timestamp = &at
		} else {
			*series.timestamp = timestamp
		}
	}
	var (
		sum            *float64
		count          *uint64
		noSum, noCount *bool
	)
	if x := series.summary; x != nil {
		sum, count, noSum, noCount = &x.Sum, &x.Count, &x.NoSum, &x.NoCount
	} else {
		x := series.histogram
		sum, count, noSum, noCount = &x.Sum, &x.Count, &x.NoSum, &x.NoCount
	}
	switch {
	case role == promRoleSum && !*noSum:
		return model.Errorf("second %s_sum sample for the %s", model.Bare(f.name), describePromSeries(f, series))
	case role == promRoleSum:
		*sum, *noSum = value, false
	case role == promRoleCount && !*noCount:
		return model.Errorf("second %s_count sample for the %s", model.Bare(f.name), describePromSeries(f, series))
	case role == promRoleCount:
		*count, *noCount = uint64(value), false
	case series.summary != nil:
		series.summary.Quantiles = append(series.summary.Quantiles, model.Quantile{Quantile: bound, Value: value})
	default:
		series.histogram.Buckets = append(series.histogram.Buckets, model.Bucket{UpperBound: bound, CumulativeCount: uint64(value)})
	}
	return nil
}

// leaveOut counts the sample being read, which belongs to the histogram or
// summary family f by its name and is none of the samples such a family has,
// as left out. Of the first such line the report names the line, the sample
// and what was expected in its place; the others are only counted, as an
// exposition has one such line for every series of the family.
func (p *promParser) leaveOut(f *promFamily) {
	if p.report == nil {
		// Bounded as a decode error is (failurebound.go), though it shows no
		// more than the start of the family's name and of the sample's.
		p.report = &PrometheusReport{FirstLeftOut: boundedFailure(model.Errorf("line %d: %w", model.Position(p.number), p.stray(f)))}
	}
	p.report.LeftOutLines++
}

// stray says what the sample being read is and what the family f has in its
// place, the two names by their start when they are long.
func (p *promParser) stray(f *promFamily) error {
	family, got := model.Bare(f.name), model.Bare(p.sampleName)
	if f.typ == model.SummaryMetricType {
		return model.Errorf("expected %[1]s with a quantile label, %[1]s_sum or %[1]s_count as a sample of the summary %[1]s, got %[2]s without a quantile label", family, got)
	}
	// Compared in place: neither name is copied to be compared.
	if base, bucket := bytes.CutSuffix(p.sampleName, []byte("_bucket")); bucket && string(base) == f.name {
		return model.Errorf("expected %[1]s_bucket with an le label, %[1]s_sum or %[1]s_count as a sample of the histogram %[1]s, got %[2]s without an le label", family, got)
	}
	return model.Errorf("expected %[1]s_bucket with an le label, %[1]s_sum or %[1]s_count as a sample of the histogram %[1]s, got %[2]s", family, got)
}

// settle checks every histogram and summary read, once all their samples
// are, and gives a histogram without a _count its +Inf bucket's
// (model.Histogram.Settle). Its error names the series and the line of its
// first sample, since what is wrong with a series is not on one line.
//
// It first puts the series in the order they come back in, the families'
// and within a family the series', which is the order they are checked in.
// An exposition writes a family's series together, after its HELP and TYPE,
// so they nearly always were read in that order.
func (p *promParser) settle() error {
	if p.unordered {
		slices.SortStableFunc(p.series, func(a, b promSeries) int { return a.family.index - b.family.index })
	}
	for i := range p.series {
		s := &p.series[i]
		var err error
		switch {
		case s.histogram != nil:
			err = s.histogram.Settle()
		case s.summary != nil:
			err = s.summary.Settle()
		}
		if err != nil {
			return model.Errorf("the %s, which starts in line %d, %w", describePromSeries(s.family, s), model.Position(s.line), err)
		}
	}
	return nil
}

// describePromSeries names a histogram or summary series for an error: its
// type, its family and its labels, as the exposition writes them, each name
// and each value by its start when it is long (model.Bare, model.Quoted). The
// series is recognised by the same without the lengths of what was cut.
func describePromSeries(f *promFamily, s *promSeries) model.QuotedValue {
	var text, same strings.Builder
	written := func(as string) {
		text.WriteString(as)
		same.WriteString(as)
	}
	shown := func(value model.QuotedValue) {
		text.WriteString(value.String())
		same.WriteString(value.Same())
	}
	written(string(f.typ))
	written(" ")
	shown(model.Bare(f.name))
	if len(s.labels) > 0 {
		written("{")
		for i, name := range model.SortedKeys(s.labels) {
			if i > 0 {
				written(",")
			}
			shown(model.Bare(name))
			written("=")
			shown(model.Quoted(s.labels[name]))
		}
		written("}")
	}
	return model.ShownAs(text.String(), same.String())
}

// keep stores one more series of f, with p.labels as its labels, and fails
// past promOptions.limit, before anything is made of the series. A series
// of a family that does not count (promFamily.uncounted) is stored whatever
// the limit.
func (p *promParser) keep(f *promFamily) (*promSeries, error) {
	if f.uncounted {
		p.uncounted++
	} else if kept := len(p.series) + 1 - p.uncounted; p.options.limit > 0 && kept > p.options.limit {
		return nil, model.MetricCountError(kept, p.options.limit)
	}
	if n := len(p.series); n > 0 && f.index < p.series[n-1].family.index {
		p.unordered = true
	}
	p.series = append(p.series, promSeries{family: f, labels: p.labelMap()})
	return &p.series[len(p.series)-1], nil
}

// labelMap makes p.labels the label set of a series: a map of the size it
// has, filled once. A name or a value that the series made before this one
// had in the same place is that series' string.
func (p *promParser) labelMap() map[string]string {
	labels := make(map[string]string, len(p.labels))
	for len(p.previous) < len(p.labels) {
		p.previous = append(p.previous, promLabelText{})
	}
	for i, l := range p.labels {
		before := &p.previous[i]
		// Neither comparison makes a string: the bytes are compared in
		// place.
		if before.name != string(l.name) {
			before.name = string(l.name)
		}
		if before.value != string(l.value) {
			before.value = string(l.value)
		}
		labels[before.name] = before.value
	}
	return labels
}

func (p *promParser) metrics() []model.Metric {
	if len(p.series) == 0 {
		return nil
	}
	out := make([]model.Metric, len(p.series))
	for i := range p.series {
		s := &p.series[i]
		f := s.family
		help := f.help
		if f.helpOf != nil && !f.helpSet {
			help = f.helpOf.help
		}
		out[i] = model.Metric{Name: f.exportedName(), Help: help, Type: f.typ, Labels: s.labels, Value: s.value, Timestamp: s.timestamp, Histogram: s.histogram, Summary: s.summary}
	}
	return out
}

// promLabel is a label as a sample line writes it: parts of the body, or of
// promParser.unescaped.
type promLabel struct{ name, value []byte }

// promLabelText is a label as a series holds it.
type promLabelText struct{ name, value string }

// promLabelsCompared is how many labels of a sample are compared with each
// new one, to find a name written twice, before a set of them is kept.
const promLabelsCompared = 16

// duplicateLabel reports whether name is the name of one of p.labels. A
// sample has a few labels, and comparing each new name with those before it
// costs less than the set the parser made of them for every line; a sample
// with many has its names in a set, so that a line of a million labels is
// not compared a million times a million.
func (p *promParser) duplicateLabel(name []byte) bool {
	if len(p.labels) < promLabelsCompared {
		for _, l := range p.labels {
			if bytes.Equal(l.name, name) {
				return true
			}
		}
		return false
	}
	if len(p.labels) == promLabelsCompared {
		p.seen = make(map[string]struct{}, 2*promLabelsCompared)
		for _, l := range p.labels {
			p.seen[string(l.name)] = struct{}{}
		}
	}
	if _, seen := p.seen[string(name)]; seen {
		return true
	}
	p.seen[string(name)] = struct{}{}
	return false
}

// readLabels reads a label set after its opening brace, up to and including
// the closing one, into p.labels, and returns the rest of the line. In the
// braces form, where the line starts with the brace, one entry may be a bare
// metric name instead of a label, which is returned.
func (p *promParser) readLabels(s []byte, bracesForm bool) ([]byte, []byte, error) {
	var name []byte
	for {
		s = skipBlanks(s)
		if len(s) == 0 {
			return nil, nil, errors.New("unexpected end of label set")
		}
		if s[0] == '}' {
			return name, s[1:], nil
		}
		label, rest, err := p.readName(s, promLabelNameStart, promLabelNameByte)
		if err != nil {
			return nil, nil, err
		}
		if len(label) == 0 {
			return nil, nil, fmt.Errorf("invalid label name %q", firstRune(s))
		}
		rest = skipBlanks(rest)
		if len(rest) == 0 || rest[0] != '=' {
			if !bracesForm || len(rest) == 0 || (rest[0] != ',' && rest[0] != '}') {
				return nil, nil, model.Errorf("expected '=' after label name %s", model.Quoted(label))
			}
			if len(name) != 0 {
				return nil, nil, model.Errorf("multiple metric names for metric %s", model.Quoted(name))
			}
			name = label
			if rest[0] == ',' {
				rest = rest[1:]
			}
			s = rest
			continue
		}
		if string(label) == "__name__" {
			return nil, nil, fmt.Errorf("label name %q is reserved", label)
		}
		if !utf8.Valid(label) {
			return nil, nil, model.Errorf("invalid label name %s", model.Quoted(label))
		}
		if p.duplicateLabel(label) {
			return nil, nil, model.Errorf("duplicate label name %s", model.Quoted(label))
		}
		rest = skipBlanks(rest[1:])
		if len(rest) == 0 || rest[0] != '"' {
			return nil, nil, model.Errorf("expected '\"' at start of the value of label %s", model.Quoted(label))
		}
		value, after, err := p.readQuoted(rest[1:], "label value")
		if err != nil {
			return nil, nil, err
		}
		// A label value that is not valid UTF-8 is kept as it is: the
		// transform repairs it with U+FFFD and counts it, as it does the
		// output of every other decoder (textencoding.go), rather than
		// failing the whole scrape over one value. One that is empty is
		// kept until the sample is added (add), so that its name written
		// twice is found as any other's is.
		p.labels = append(p.labels, promLabel{name: label, value: value})
		if len(value) == 0 {
			p.empty = true
		}
		after = skipBlanks(after)
		switch {
		case len(after) == 0:
			return nil, nil, model.Errorf("unexpected end of label set after label %s", model.Quoted(label))
		case after[0] == ',':
			s = after[1:]
		case after[0] == '}':
			s = after
		default:
			return nil, nil, model.Errorf("unexpected %q after the value of label %s", firstRune(after), model.Quoted(label))
		}
	}
}

// The bytes of a bare name: a label's, and a metric's, which may also hold
// colons. A table answers for a byte faster than a function called for it.
const (
	promLabelNameStart = 1 << iota
	promLabelNameByte
	promMetricNameStart
	promMetricNameByte
)

var promNameBytes = func() (table [256]uint8) {
	for c := 0; c < 256; c++ {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
		digit := c >= '0' && c <= '9'
		if letter {
			table[c] |= promLabelNameStart | promMetricNameStart
		}
		if letter || digit {
			table[c] |= promLabelNameByte | promMetricNameByte
		}
		if c == ':' {
			table[c] |= promMetricNameStart | promMetricNameByte
		}
	}
	return table
}()

// readName reads a bare name, which starts with a byte that is a start in
// promNameBytes and goes on with those that are a cont, or a quoted UTF-8
// name. It returns the name and the rest of the line; an empty name means
// the line does not start with one.
func (p *promParser) readName(s []byte, start, cont uint8) ([]byte, []byte, error) {
	if len(s) == 0 {
		return nil, s, nil
	}
	if s[0] == '"' {
		return p.readQuoted(s[1:], "name")
	}
	if promNameBytes[s[0]]&start == 0 {
		return nil, s, nil
	}
	i := 1
	for i < len(s) && promNameBytes[s[i]]&cont != 0 {
		i++
	}
	return s[:i], s[i:], nil
}

// readQuoted reads up to the closing quote, unescaping \\, \n and \". What
// has no escape, as nearly every name and value, is returned where it lies
// in the body; what has is written to p.unescaped, after what the line
// already wrote there.
func (p *promParser) readQuoted(s []byte, what string) ([]byte, []byte, error) {
	plain := 0
	for plain < len(s) && s[plain] != '"' && s[plain] != '\\' {
		plain++
	}
	if plain < len(s) && s[plain] == '"' {
		return s[:plain], s[plain+1:], nil
	}
	start := len(p.unescaped)
	b := append(p.unescaped, s[:plain]...)
	for i := plain; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			p.unescaped = b
			return b[start:], s[i+1:], nil
		case '\\':
			if i+1 == len(s) {
				return nil, nil, model.Errorf("%s %s ends in a lone backslash", what, model.Quoted(b[start:]))
			}
			i++
			switch s[i] {
			case '\\', '"':
				b = append(b, s[i])
			case 'n':
				b = append(b, '\n')
			default:
				return nil, nil, fmt.Errorf("invalid escape sequence '\\%c'", s[i])
			}
		default:
			b = append(b, c)
		}
	}
	return nil, nil, model.Errorf("%s %s contains unescaped new-line", what, model.Quoted(b[start:]))
}

// unescapePromText unescapes HELP text, which runs to the end of the line.
// A quote need not be escaped there, but \" is accepted.
func unescapePromText(s, what string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 == len(s) {
			return "", model.Errorf("%s %s ends in a lone backslash", what, model.Quoted(b.String()))
		}
		i++
		switch s[i] {
		case '\\', '"':
			b.WriteByte(s[i])
		case 'n':
			b.WriteByte('\n')
		default:
			return "", fmt.Errorf("invalid escape sequence '\\%c'", s[i])
		}
	}
	return b.String(), nil
}

// parsePromFloat parses a sample value the way expfmt does: as a Go float,
// but without hexadecimal exponents or digit separators. The string it is
// read from is not kept, and so costs nothing to make.
func parsePromFloat(s []byte) (float64, error) {
	for _, c := range s {
		if c == 'p' || c == 'P' || c == '_' {
			return 0, errors.New("unsupported character in float")
		}
	}
	return strconv.ParseFloat(string(s), 64)
}

// promSignature identifies a label set regardless of the order of its
// labels, which it puts in the order of their names; it is written after
// signature, which is returned.
func promSignature(signature []byte, labels []promLabel) []byte {
	slices.SortFunc(labels, func(a, b promLabel) int { return bytes.Compare(a.name, b.name) })
	for _, l := range labels {
		signature = append(signature, l.name...)
		signature = append(signature, 0xff)
		signature = append(signature, l.value...)
		signature = append(signature, 0xff)
	}
	return signature
}

// skipBlanks is s without the spaces and tabs it starts with. It is a loop
// rather than strings.TrimLeft, which makes a set of the characters to trim
// at every call, and the parser calls it several times for every line.
func skipBlanks(s []byte) []byte {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[i:]
}

// cutBlank cuts s before its first space or tab.
func cutBlank(s []byte) ([]byte, []byte) {
	for i, c := range s {
		if c == ' ' || c == '\t' {
			return s[:i], s[i:]
		}
	}
	return s, nil
}

func firstRune(s []byte) string {
	r, _ := utf8.DecodeRune(s)
	return string(r)
}

func isBlank(b byte) bool { return b == ' ' || b == '\t' }
