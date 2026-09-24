package decode

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
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
// negative, NaN or infinite is an error instead of an arbitrary integer; a
// label set with no metric name is an error instead of joining the previous
// line's family; and a name that mixes bare and quoted parts, such as a"b", is
// an error instead of being spliced together. It never panics, where expfmt
// did on input such as {b="c",} 1.
//
// Families come back in the order they were first seen, and series in the
// order of their first sample, so a decode is deterministic.
func parsePrometheusText(body []byte) ([]model.Metric, error) {
	p := promParser{byName: map[string]*promFamily{}}
	lines := bytes.Split(body, []byte("\n"))
	for i, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\r")
		if err := p.line(line); err != nil {
			return nil, fmt.Errorf("text format parsing error in line %d: %w", i+1, err)
		}
	}
	return p.metrics(), nil
}

const (
	promRoleNone = iota
	promRoleSum
	promRoleCount
)

type promFamily struct {
	name    string
	help    string
	helpSet bool
	typ     model.MetricType // empty until a TYPE line or the first sample
	series  []*promSeries
	grouped map[string]*promSeries // summary and histogram series by signature
}

type promSeries struct {
	labels    map[string]string
	value     float64
	timestamp *int64
	histogram *model.Histogram
	summary   *model.Summary
}

type promParser struct {
	byName   map[string]*promFamily
	families []*promFamily
}

func (p *promParser) line(line string) error {
	s := strings.TrimLeft(line, " \t")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if s[0] == '#' {
		return p.comment(s[1:])
	}
	return p.sample(s)
}

// comment handles a # line. Only HELP and TYPE mean anything; a HELP or TYPE
// line that stops after its keyword or after its metric name is not an error,
// as it is not for expfmt.
func (p *promParser) comment(s string) error {
	s = strings.TrimLeft(s, " \t")
	keyword, rest := cutBlank(s)
	if keyword != "HELP" && keyword != "TYPE" {
		return nil
	}
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return nil
	}
	name, rest, err := readPromName(rest, isPromMetricNameStart, isPromMetricNameByte)
	if err != nil {
		return err
	}
	if rest == "" {
		return nil
	}
	if !isBlank(rest[0]) {
		return errors.New("invalid metric name in comment")
	}
	family, _, err := p.family(name)
	if err != nil {
		return err
	}
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return nil
	}
	if keyword == "HELP" {
		if family.helpSet {
			return fmt.Errorf("second HELP line for metric name %q", family.name)
		}
		help, err := unescapePromText(rest, "help text")
		if err != nil {
			return err
		}
		family.help, family.helpSet = help, true
		return nil
	}
	if family.typ != "" {
		return fmt.Errorf("second TYPE line for metric name %q, or TYPE reported after samples", family.name)
	}
	switch t := strings.ToLower(strings.TrimRight(rest, " \t")); t {
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
		return fmt.Errorf("unknown metric type %q", rest)
	}
	return nil
}

// sample handles a sample line: a name, optional labels, a value and an
// optional timestamp.
func (p *promParser) sample(s string) error {
	var (
		name   string
		labels []promLabel
		err    error
	)
	if s[0] == '{' {
		name, labels, s, err = readPromLabels(s[1:], true)
		if err != nil {
			return err
		}
		if name == "" {
			return errors.New("invalid metric name")
		}
	} else {
		name, s, err = readPromName(s, isPromMetricNameStart, isPromMetricNameByte)
		if err != nil {
			return err
		}
		if name == "" {
			return errors.New("invalid metric name")
		}
		s = strings.TrimLeft(s, " \t")
		if s != "" && s[0] == '{' {
			if _, labels, s, err = readPromLabels(s[1:], false); err != nil {
				return err
			}
		}
	}
	s = strings.TrimLeft(s, " \t")
	token, s := cutBlank(s)
	value, err := parsePromFloat(token)
	if err != nil {
		return fmt.Errorf("expected float as value, got %q", token)
	}
	var timestamp *int64
	s = strings.TrimLeft(s, " \t")
	if s != "" {
		token, s = cutBlank(s)
		t, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return fmt.Errorf("expected integer as timestamp, got %q", token)
		}
		timestamp = &t
		if s = strings.TrimSpace(s); s != "" {
			return fmt.Errorf("spurious string after timestamp: %q", s)
		}
	}
	family, role, err := p.family(name)
	if err != nil {
		return err
	}
	if family.typ == "" {
		family.typ = model.UntypedMetricType
	}
	return family.add(labels, role, value, timestamp)
}

// family finds the family a name belongs to, creating it if there is none.
// The role says whether the name is a summary's or histogram's _sum or _count.
func (p *promParser) family(name string) (*promFamily, int, error) {
	if name == "" || !utf8.ValidString(name) {
		return nil, 0, fmt.Errorf("invalid metric name %q", name)
	}
	if f := p.byName[name]; f != nil {
		return f, promRoleNone, nil
	}
	role := promRoleNone
	base := name
	switch {
	case len(name) > len("_count") && strings.HasSuffix(name, "_count"):
		role, base = promRoleCount, strings.TrimSuffix(name, "_count")
	case len(name) > len("_sum") && strings.HasSuffix(name, "_sum"):
		role, base = promRoleSum, strings.TrimSuffix(name, "_sum")
	}
	if f := p.byName[base]; f != nil && f.typ == model.SummaryMetricType {
		return f, role, nil
	}
	if role == promRoleNone && len(name) > len("_bucket") && strings.HasSuffix(name, "_bucket") {
		base = strings.TrimSuffix(name, "_bucket")
	}
	if f := p.byName[base]; f != nil && f.typ == model.HistogramMetricType {
		return f, role, nil
	}
	f := &promFamily{name: name}
	p.byName[name] = f
	p.families = append(p.families, f)
	return f, promRoleNone, nil
}

func (f *promFamily) add(labels []promLabel, role int, value float64, timestamp *int64) error {
	special := ""
	switch f.typ {
	case model.SummaryMetricType:
		special = "quantile"
	case model.HistogramMetricType:
		special = "le"
	}
	own := map[string]string{}
	bound, hasBound := math.NaN(), false
	for _, l := range labels {
		if special != "" && l.name == special {
			b, err := parsePromFloat(l.value)
			if err != nil {
				return fmt.Errorf("expected float as value for '%s' label, got %q", special, l.value)
			}
			bound, hasBound = b, true
			continue
		}
		own[l.name] = l.value
	}
	if special == "" {
		f.series = append(f.series, &promSeries{labels: own, value: value, timestamp: timestamp})
		return nil
	}
	if f.grouped == nil {
		f.grouped = map[string]*promSeries{}
	}
	key := promSignature(own)
	series := f.grouped[key]
	if series == nil {
		series = &promSeries{labels: own}
		if f.typ == model.SummaryMetricType {
			series.summary = &model.Summary{}
		} else {
			series.histogram = &model.Histogram{}
		}
		f.grouped[key] = series
		f.series = append(f.series, series)
	}
	if timestamp != nil {
		series.timestamp = timestamp
	}
	if role == promRoleSum {
		if series.summary != nil {
			series.summary.Sum = value
		} else {
			series.histogram.Sum = value
		}
		return nil
	}
	if role == promRoleCount || (hasBound && series.histogram != nil) {
		// A count is a whole number of observations, which a uint64 holds
		// up to 2^64-1; past it the conversion gives a meaningless number.
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value >= math.MaxUint64 {
			return fmt.Errorf("expected a count from 0 to 2^64-1 for %q, got %v", f.name, value)
		}
	}
	switch {
	case role == promRoleCount && series.summary != nil:
		series.summary.Count = uint64(value)
	case role == promRoleCount:
		series.histogram.Count = uint64(value)
	case !hasBound:
		// A summary or histogram sample that is neither _sum, _count, a
		// quantile nor a bucket carries nothing to keep, as for expfmt.
	case series.summary != nil:
		series.summary.Quantiles = append(series.summary.Quantiles, model.Quantile{Quantile: bound, Value: value})
	default:
		series.histogram.Buckets = append(series.histogram.Buckets, model.Bucket{UpperBound: bound, CumulativeCount: uint64(value)})
	}
	return nil
}

func (p *promParser) metrics() []model.Metric {
	var out []model.Metric
	for _, f := range p.families {
		for _, s := range f.series {
			m := model.Metric{Name: f.name, Help: f.help, Type: f.typ, Labels: s.labels, Value: s.value, Timestamp: s.timestamp, Histogram: s.histogram, Summary: s.summary}
			out = append(out, m)
		}
	}
	return out
}

type promLabel struct{ name, value string }

// readPromLabels reads a label set after its opening brace, up to and
// including the closing one, and returns the rest of the line. In the braces
// form, where the line starts with the brace, one entry may be a bare metric
// name instead of a label.
func readPromLabels(s string, bracesForm bool) (string, []promLabel, string, error) {
	var (
		name   string
		labels []promLabel
		seen   = map[string]bool{}
	)
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return "", nil, "", errors.New("unexpected end of label set")
		}
		if s[0] == '}' {
			return name, labels, s[1:], nil
		}
		label, rest, err := readPromName(s, isPromLabelNameStart, isPromLabelNameByte)
		if err != nil {
			return "", nil, "", err
		}
		if label == "" {
			return "", nil, "", fmt.Errorf("invalid label name %q", firstRune(s))
		}
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" || rest[0] != '=' {
			if !bracesForm || rest == "" || (rest[0] != ',' && rest[0] != '}') {
				return "", nil, "", fmt.Errorf("expected '=' after label name %q", label)
			}
			if name != "" {
				return "", nil, "", fmt.Errorf("multiple metric names for metric %q", name)
			}
			name = label
			if rest[0] == ',' {
				rest = rest[1:]
			}
			s = rest
			continue
		}
		if label == "__name__" {
			return "", nil, "", fmt.Errorf("label name %q is reserved", label)
		}
		if !utf8.ValidString(label) {
			return "", nil, "", fmt.Errorf("invalid label name %q", label)
		}
		if seen[label] {
			return "", nil, "", fmt.Errorf("duplicate label name %q", label)
		}
		seen[label] = true
		rest = strings.TrimLeft(rest[1:], " \t")
		if rest == "" || rest[0] != '"' {
			return "", nil, "", fmt.Errorf("expected '\"' at start of the value of label %q", label)
		}
		value, after, err := readPromQuoted(rest[1:], "label value")
		if err != nil {
			return "", nil, "", err
		}
		if !utf8.ValidString(value) {
			return "", nil, "", fmt.Errorf("invalid label value %q", value)
		}
		labels = append(labels, promLabel{name: label, value: value})
		after = strings.TrimLeft(after, " \t")
		switch {
		case after == "":
			return "", nil, "", fmt.Errorf("unexpected end of label set after label %q", label)
		case after[0] == ',':
			s = after[1:]
		case after[0] == '}':
			s = after
		default:
			return "", nil, "", fmt.Errorf("unexpected %q after the value of label %q", firstRune(after), label)
		}
	}
}

// readPromName reads a bare name, whose bytes the two predicates allow, or a
// quoted UTF-8 name. It returns the name and the rest of the line; an empty
// name means the line does not start with one.
func readPromName(s string, start, cont func(byte) bool) (string, string, error) {
	if s == "" {
		return "", s, nil
	}
	if s[0] == '"' {
		return readPromQuoted(s[1:], "name")
	}
	if !start(s[0]) {
		return "", s, nil
	}
	i := 1
	for i < len(s) && cont(s[i]) {
		i++
	}
	return s[:i], s[i:], nil
}

// readPromQuoted reads up to the closing quote, unescaping \\, \n and \".
func readPromQuoted(s, what string) (string, string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), s[i+1:], nil
		case '\\':
			if i+1 == len(s) {
				return "", "", fmt.Errorf("%s %q ends in a lone backslash", what, b.String())
			}
			i++
			switch s[i] {
			case '\\', '"':
				b.WriteByte(s[i])
			case 'n':
				b.WriteByte('\n')
			default:
				return "", "", fmt.Errorf("invalid escape sequence '\\%c'", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Errorf("%s %q contains unescaped new-line", what, b.String())
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
			return "", fmt.Errorf("%s %q ends in a lone backslash", what, b.String())
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
// but without hexadecimal exponents or digit separators.
func parsePromFloat(s string) (float64, error) {
	if strings.ContainsAny(s, "pP_") {
		return 0, errors.New("unsupported character in float")
	}
	return strconv.ParseFloat(s, 64)
}

// promSignature identifies a label set regardless of the order of its labels.
func promSignature(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0xff)
		b.WriteString(labels[k])
		b.WriteByte(0xff)
	}
	return b.String()
}

func cutBlank(s string) (string, string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

func firstRune(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	return string(r)
}

func isBlank(b byte) bool { return b == ' ' || b == '\t' }

func isPromLabelNameStart(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_'
}

func isPromLabelNameByte(b byte) bool {
	return isPromLabelNameStart(b) || b >= '0' && b <= '9'
}

func isPromMetricNameStart(b byte) bool { return isPromLabelNameStart(b) || b == ':' }

func isPromMetricNameByte(b byte) bool { return isPromLabelNameByte(b) || b == ':' }
