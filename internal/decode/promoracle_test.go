package decode

// The parser of Prometheus and OpenMetrics text as it was before it was made
// cheaper (promparse.go), kept as the oracle of the differential test
// (promdiff_test.go): every name of it starts with oracle, and it shares
// nothing with the parser it is compared with but promOptions, which says
// what to parse, and the model. It is not to be changed with the parser: a
// change of what the parser accepts or returns is made there, and shown here
// as a difference the test then has to allow by name.
//
// One such difference cannot be told from the two results, and is a switch
// here that only the test of that difference sets (oracleParse's
// withoutEmptyLabels, for a body that may hold such a label): a label with
// an empty value is left off its series, where the oracle keeps it. Two
// series that differ only in such a label are then one, which changes what
// a histogram's samples make and where a limit is reached, and that takes
// the parse itself to say. Without the switch the oracle is what it was, to
// the letter, and that is what reads every body that has no "" in it, and
// the bodies of the test of the difference itself (promemptylabel_test.go),
// which compares the parser's reading of a body with the unswitched oracle's
// reading of the same body without the labels.

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

func oracleParseExposition(body []byte, options promOptions) ([]model.Metric, error) {
	return oracleParse(body, options, false)
}

// oracleParse is oracleParseExposition, with the one difference the parser
// has from it by name when withoutEmptyLabels says so: a label with an empty
// value is no label of its series.
func oracleParse(body []byte, options promOptions, withoutEmptyLabels bool) ([]model.Metric, error) {
	p := oraclePromParser{byName: map[string]*oraclePromFamily{}, options: options, withoutEmptyLabels: withoutEmptyLabels}
	for number := 1; ; number++ {
		raw, rest, more := bytes.Cut(body, []byte("\n"))
		line := strings.TrimSuffix(string(raw), "\r")
		p.number = number
		if err := p.line(line); err != nil {
			if errors.Is(err, model.ErrLimitExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("text format parsing error in line %d: %w", number, err)
		}
		if !more {
			break
		}
		body = rest
	}
	if err := p.settle(); err != nil {
		return nil, fmt.Errorf("text format parsing error: %w", err)
	}
	return p.metrics(), nil
}

const (
	oraclePromRoleNone = iota
	oraclePromRoleSum
	oraclePromRoleCount
	// oraclePromRoleCreated is an OpenMetrics _created sample, which is read and
	// dropped: the metric model has no creation time.
	oraclePromRoleCreated
)

type oraclePromFamily struct {
	name    string
	help    string
	helpSet bool
	typ     model.MetricType // empty until a TYPE line or the first sample
	series  []*oraclePromSeries
	grouped map[string]*oraclePromSeries // summary and histogram series by signature
	// exported, when set, is the name the family's series have, where
	// OpenMetrics names them other than the family: foo_total for a
	// counter foo, foo_info for an info foo.
	exported string
	// helpOf, when set, is the family whose HELP this one's series carry:
	// an OpenMetrics gauge histogram's, for the gauges it is read as.
	helpOf *oraclePromFamily
	// kept says whether the family's series are stored, once decided by
	// promOptions.keep at its first series.
	kept, keptKnown bool
}

// exportedName is the name of the family's series.
func (f *oraclePromFamily) exportedName() string {
	if f.exported != "" {
		return f.exported
	}
	return f.name
}

type oraclePromSeries struct {
	labels    map[string]string
	value     float64
	timestamp *int64
	histogram *model.Histogram
	summary   *model.Summary
	// line is the line of a histogram's or summary's first sample, for the
	// error of one that cannot be a series (settle).
	line int
}

type oraclePromParser struct {
	byName   map[string]*oraclePromFamily
	families []*oraclePromFamily
	options  promOptions
	// aliases are the OpenMetrics sample names that belong to a family
	// other than their own, with the role they have in it.
	aliases map[string]oraclePromAlias
	// kept counts the series stored, for promOptions.limit.
	kept int
	// eof is set by OpenMetrics' # EOF, after which nothing may follow.
	eof bool
	// number is the line being read.
	number int
	// withoutEmptyLabels is the switch of the one named difference: a
	// label with an empty value is left off its series.
	withoutEmptyLabels bool
}

type oraclePromAlias struct {
	family *oraclePromFamily
	role   int
}

func (p *oraclePromParser) line(line string) error {
	s := strings.TrimLeft(line, " \t")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if p.eof {
		return errors.New("unexpected content after # EOF")
	}
	if s[0] == '#' {
		return p.comment(s[1:])
	}
	return p.sample(s)
}

// comment handles a # line. Only HELP and TYPE mean anything; a HELP or TYPE
// line that stops after its keyword or after its metric name is not an error,
// as it is not for expfmt.
func (p *oraclePromParser) comment(s string) error {
	s = strings.TrimLeft(s, " \t")
	keyword, rest := oracleCutBlank(s)
	if p.options.openMetrics && keyword == "EOF" && strings.TrimSpace(rest) == "" {
		p.eof = true
		return nil
	}
	if keyword != "HELP" && keyword != "TYPE" {
		return nil
	}
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return nil
	}
	name, rest, err := oracleReadPromName(rest, oracleIsPromMetricNameStart, oracleIsPromMetricNameByte)
	if err != nil {
		return err
	}
	if rest == "" {
		return nil
	}
	if !oracleIsBlank(rest[0]) {
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
		help, err := oracleUnescapePromText(rest, "help text")
		if err != nil {
			return err
		}
		family.help, family.helpSet = help, true
		return nil
	}
	if family.typ != "" {
		return fmt.Errorf("second TYPE line for metric name %q, or TYPE reported after samples", family.name)
	}
	t := strings.ToLower(strings.TrimRight(rest, " \t"))
	if p.options.openMetrics {
		return p.openMetricsType(family, t, rest)
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
		return fmt.Errorf("unknown metric type %q", rest)
	}
	return nil
}

// sample handles a sample line: a name, optional labels, a value and an
// optional timestamp.
func (p *oraclePromParser) sample(s string) error {
	var (
		name   string
		labels []oraclePromLabel
		err    error
	)
	if s[0] == '{' {
		name, labels, s, err = oracleReadPromLabels(s[1:], true)
		if err != nil {
			return err
		}
		if name == "" {
			return errors.New("invalid metric name")
		}
	} else {
		name, s, err = oracleReadPromName(s, oracleIsPromMetricNameStart, oracleIsPromMetricNameByte)
		if err != nil {
			return err
		}
		if name == "" {
			return errors.New("invalid metric name")
		}
		s = strings.TrimLeft(s, " \t")
		if s != "" && s[0] == '{' {
			if _, labels, s, err = oracleReadPromLabels(s[1:], false); err != nil {
				return err
			}
		}
	}
	s = strings.TrimLeft(s, " \t")
	token, s := oracleCutBlank(s)
	value, err := oracleParsePromFloat(token)
	if err != nil {
		return fmt.Errorf("expected float as value, got %q", token)
	}
	if p.options.openMetrics {
		s = oracleWithoutExemplar(s)
	}
	var timestamp *int64
	s = strings.TrimLeft(s, " \t")
	if s != "" {
		token, s = oracleCutBlank(s)
		if p.options.openMetrics {
			timestamp, err = oracleOpenMetricsTimestamp(token)
			if err != nil {
				return err
			}
		} else {
			t, err := strconv.ParseInt(token, 10, 64)
			if err != nil {
				return fmt.Errorf("expected integer as timestamp, got %q", token)
			}
			timestamp = &t
		}
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
	return p.add(family, labels, role, value, timestamp)
}

// family finds the family a name belongs to, creating it if there is none.
// The role says whether the name is a summary's or histogram's _sum or _count.
func (p *oraclePromParser) family(name string) (*oraclePromFamily, int, error) {
	if name == "" || !utf8.ValidString(name) {
		return nil, 0, fmt.Errorf("invalid metric name %q", name)
	}
	if alias, ok := p.aliases[name]; ok {
		return alias.family, alias.role, nil
	}
	if f := p.byName[name]; f != nil {
		return f, oraclePromRoleNone, nil
	}
	role := oraclePromRoleNone
	base := name
	switch {
	case len(name) > len("_count") && strings.HasSuffix(name, "_count"):
		role, base = oraclePromRoleCount, strings.TrimSuffix(name, "_count")
	case len(name) > len("_sum") && strings.HasSuffix(name, "_sum"):
		role, base = oraclePromRoleSum, strings.TrimSuffix(name, "_sum")
	}
	if f := p.byName[base]; f != nil && f.typ == model.SummaryMetricType {
		return f, role, nil
	}
	if role == oraclePromRoleNone && len(name) > len("_bucket") && strings.HasSuffix(name, "_bucket") {
		base = strings.TrimSuffix(name, "_bucket")
	}
	if f := p.byName[base]; f != nil && f.typ == model.HistogramMetricType {
		return f, role, nil
	}
	f := &oraclePromFamily{name: name}
	p.byName[name] = f
	p.families = append(p.families, f)
	return f, oraclePromRoleNone, nil
}

// add adds a sample to its family, or checks it and drops it when the family
// is not kept or the sample is an OpenMetrics _created.
func (p *oraclePromParser) add(f *oraclePromFamily, labels []oraclePromLabel, role int, value float64, timestamp *int64) error {
	if !f.keptKnown {
		f.kept = p.options.keep == nil || p.options.keep(f.exportedName())
		f.keptKnown = true
	}
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
			b, err := oracleParsePromFloat(l.value)
			if err != nil {
				return fmt.Errorf("expected float as value for '%s' label, got %q", special, l.value)
			}
			bound, hasBound = b, true
			continue
		}
		if p.withoutEmptyLabels && l.value == "" {
			continue
		}
		own[l.name] = l.value
	}
	if role == oraclePromRoleCreated {
		return nil
	}
	if role == oraclePromRoleCount || (hasBound && f.typ == model.HistogramMetricType) {
		// A count is a whole number of observations, which a uint64 holds
		// up to 2^64-1; past it the conversion gives a meaningless number.
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value >= math.MaxUint64 {
			return fmt.Errorf("expected a count from 0 to 2^64-1 for %q, got %v", f.name, value)
		}
		// Nor is it a fraction: 1.5 observations would be passed on as 1,
		// a number the target never reported.
		if value != math.Trunc(value) {
			return fmt.Errorf("expected a whole number as the count for %q, got %v", f.name, value)
		}
	}
	if !f.kept {
		// Checked as a kept sample is, and dropped: nothing of it is held.
		return nil
	}
	if special != "" && role == oraclePromRoleNone && !hasBound {
		// A summary or histogram sample that is neither _sum, _count, a
		// quantile nor a bucket carries nothing to keep, as for expfmt, and
		// makes no series.
		return nil
	}
	if special == "" {
		if err := p.keep(); err != nil {
			return err
		}
		f.series = append(f.series, &oraclePromSeries{labels: own, value: value, timestamp: timestamp})
		return nil
	}
	if f.grouped == nil {
		f.grouped = map[string]*oraclePromSeries{}
	}
	key := oraclePromSignature(own)
	series := f.grouped[key]
	if series == nil {
		if err := p.keep(); err != nil {
			return err
		}
		// The series has no _sum and no _count until one is read.
		series = &oraclePromSeries{labels: own, line: p.number}
		if f.typ == model.SummaryMetricType {
			series.summary = &model.Summary{NoSum: true, NoCount: true}
		} else {
			series.histogram = &model.Histogram{NoSum: true, NoCount: true}
		}
		f.grouped[key] = series
		f.series = append(f.series, series)
	}
	if timestamp != nil {
		series.timestamp = timestamp
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
	case role == oraclePromRoleSum && !*noSum:
		return fmt.Errorf("second %s_sum sample for the %s", f.name, oracleDescribePromSeries(f, series))
	case role == oraclePromRoleSum:
		*sum, *noSum = value, false
	case role == oraclePromRoleCount && !*noCount:
		return fmt.Errorf("second %s_count sample for the %s", f.name, oracleDescribePromSeries(f, series))
	case role == oraclePromRoleCount:
		*count, *noCount = uint64(value), false
	case series.summary != nil:
		series.summary.Quantiles = append(series.summary.Quantiles, model.Quantile{Quantile: bound, Value: value})
	default:
		series.histogram.Buckets = append(series.histogram.Buckets, model.Bucket{UpperBound: bound, CumulativeCount: uint64(value)})
	}
	return nil
}

// settle checks every histogram and summary read, once all their samples
// are, and gives a histogram without a _count its +Inf bucket's
// (model.Histogram.Settle). Its error names the series and the line of its
// first sample, since what is wrong with a series is not on one line.
func (p *oraclePromParser) settle() error {
	for _, f := range p.families {
		for _, s := range f.series {
			var err error
			switch {
			case s.histogram != nil:
				err = s.histogram.Settle()
			case s.summary != nil:
				err = s.summary.Settle()
			}
			if err != nil {
				return fmt.Errorf("the %s, which starts in line %d, %w", oracleDescribePromSeries(f, s), s.line, err)
			}
		}
	}
	return nil
}

// oracleDescribePromSeries names a histogram or summary series for an error: its
// type, its family and its labels, as the exposition writes them.
func oracleDescribePromSeries(f *oraclePromFamily, s *oraclePromSeries) string {
	var b strings.Builder
	b.WriteString(string(f.typ))
	b.WriteByte(' ')
	b.WriteString(f.name)
	if len(s.labels) > 0 {
		b.WriteByte('{')
		for i, name := range model.SortedKeys(s.labels) {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(name)
			b.WriteByte('=')
			b.WriteString(model.QuoteValue(s.labels[name]))
		}
		b.WriteByte('}')
	}
	return b.String()
}

// keep counts one more stored series, and fails past promOptions.limit.
func (p *oraclePromParser) keep() error {
	p.kept++
	if p.options.limit > 0 && p.kept > p.options.limit {
		return model.MetricCountError(p.kept, p.options.limit)
	}
	return nil
}

func (p *oraclePromParser) metrics() []model.Metric {
	var out []model.Metric
	if p.kept > 0 {
		out = make([]model.Metric, 0, p.kept)
	}
	for _, f := range p.families {
		help := f.help
		if f.helpOf != nil && !f.helpSet {
			help = f.helpOf.help
		}
		for _, s := range f.series {
			m := model.Metric{Name: f.exportedName(), Help: help, Type: f.typ, Labels: s.labels, Value: s.value, Timestamp: s.timestamp, Histogram: s.histogram, Summary: s.summary}
			out = append(out, m)
		}
	}
	return out
}

type oraclePromLabel struct{ name, value string }

// oracleReadPromLabels reads a label set after its opening brace, up to and
// including the closing one, and returns the rest of the line. In the braces
// form, where the line starts with the brace, one entry may be a bare metric
// name instead of a label.
func oracleReadPromLabels(s string, bracesForm bool) (string, []oraclePromLabel, string, error) {
	var (
		name   string
		labels []oraclePromLabel
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
		label, rest, err := oracleReadPromName(s, oracleIsPromLabelNameStart, oracleIsPromLabelNameByte)
		if err != nil {
			return "", nil, "", err
		}
		if label == "" {
			return "", nil, "", fmt.Errorf("invalid label name %q", oracleFirstRune(s))
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
		value, after, err := oracleReadPromQuoted(rest[1:], "label value")
		if err != nil {
			return "", nil, "", err
		}
		// A label value that is not valid UTF-8 is kept as it is: the
		// transform repairs it with U+FFFD and counts it, as it does the
		// output of every other decoder (textencoding.go), rather than
		// failing the whole scrape over one value.
		labels = append(labels, oraclePromLabel{name: label, value: value})
		after = strings.TrimLeft(after, " \t")
		switch {
		case after == "":
			return "", nil, "", fmt.Errorf("unexpected end of label set after label %q", label)
		case after[0] == ',':
			s = after[1:]
		case after[0] == '}':
			s = after
		default:
			return "", nil, "", fmt.Errorf("unexpected %q after the value of label %q", oracleFirstRune(after), label)
		}
	}
}

// oracleReadPromName reads a bare name, whose bytes the two predicates allow, or a
// quoted UTF-8 name. It returns the name and the rest of the line; an empty
// name means the line does not start with one.
func oracleReadPromName(s string, start, cont func(byte) bool) (string, string, error) {
	if s == "" {
		return "", s, nil
	}
	if s[0] == '"' {
		return oracleReadPromQuoted(s[1:], "name")
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

// oracleReadPromQuoted reads up to the closing quote, unescaping \\, \n and \".
func oracleReadPromQuoted(s, what string) (string, string, error) {
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

// oracleUnescapePromText unescapes HELP text, which runs to the end of the line.
// A quote need not be escaped there, but \" is accepted.
func oracleUnescapePromText(s, what string) (string, error) {
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

// oracleParsePromFloat parses a sample value the way expfmt does: as a Go float,
// but without hexadecimal exponents or digit separators.
func oracleParsePromFloat(s string) (float64, error) {
	if strings.ContainsAny(s, "pP_") {
		return 0, errors.New("unsupported character in float")
	}
	return strconv.ParseFloat(s, 64)
}

// oraclePromSignature identifies a label set regardless of the order of its labels.
func oraclePromSignature(labels map[string]string) string {
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

func oracleCutBlank(s string) (string, string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

func oracleFirstRune(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	return string(r)
}

func oracleIsBlank(b byte) bool { return b == ' ' || b == '\t' }

func oracleIsPromLabelNameStart(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_'
}

func oracleIsPromLabelNameByte(b byte) bool {
	return oracleIsPromLabelNameStart(b) || b >= '0' && b <= '9'
}

func oracleIsPromMetricNameStart(b byte) bool { return oracleIsPromLabelNameStart(b) || b == ':' }

func oracleIsPromMetricNameByte(b byte) bool { return oracleIsPromLabelNameByte(b) || b == ':' }

func (p *oraclePromParser) openMetricsType(family *oraclePromFamily, t, raw string) error {
	alias := func(name string, target *oraclePromFamily, role int) {
		if p.aliases == nil {
			p.aliases = map[string]oraclePromAlias{}
		}
		if _, taken := p.aliases[name]; !taken && p.byName[name] == nil {
			p.aliases[name] = oraclePromAlias{family: target, role: role}
		}
	}
	name := family.name
	switch t {
	case "counter":
		family.typ = model.CounterMetricType
		base, total := strings.CutSuffix(name, "_total")
		if !total {
			family.exported = name + "_total"
			alias(name+"_total", family, oraclePromRoleNone)
		}
		alias(base+"_created", family, oraclePromRoleCreated)
	case "gauge", "stateset":
		family.typ = model.GaugeMetricType
	case "unknown", "untyped":
		family.typ = model.UntypedMetricType
	case "summary":
		family.typ = model.SummaryMetricType
		alias(name+"_created", family, oraclePromRoleCreated)
	case "histogram":
		family.typ = model.HistogramMetricType
		alias(name+"_created", family, oraclePromRoleCreated)
	case "info":
		family.typ = model.GaugeMetricType
		if !strings.HasSuffix(name, "_info") {
			family.exported = name + "_info"
			alias(name+"_info", family, oraclePromRoleNone)
		}
	case "gaugehistogram":
		// The family itself has no samples, and is dropped as one without
		// them; each of its series is a gauge family of its own, which
		// carries its help.
		family.typ = model.GaugeMetricType
		for _, suffix := range []string{"_bucket", "_gcount", "_gsum"} {
			if p.byName[name+suffix] != nil {
				continue
			}
			part := &oraclePromFamily{name: name + suffix, typ: model.GaugeMetricType, helpOf: family}
			p.byName[part.name] = part
			p.families = append(p.families, part)
		}
	default:
		return fmt.Errorf("unknown metric type %q", raw)
	}
	return nil
}

// oracleWithoutExemplar is the rest of a sample line after its value without the
// exemplar that may end it. An exemplar starts with a # after a blank; a
// timestamp, the only other thing that may follow the value, has none.
func oracleWithoutExemplar(s string) string {
	if i := strings.Index(s, "#"); i >= 0 && (i == 0 || oracleIsBlank(s[i-1])) {
		return s[:i]
	}
	return s
}

// oracleOpenMetricsTimestamp reads an OpenMetrics timestamp, a float number of
// seconds, as milliseconds.
func oracleOpenMetricsTimestamp(token string) (*int64, error) {
	seconds, err := oracleParsePromFloat(token)
	ms := math.Round(seconds * 1000)
	if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) || ms >= math.MaxInt64 || ms < math.MinInt64 {
		return nil, fmt.Errorf("expected a number of seconds as timestamp, got %q", token)
	}
	at := int64(ms)
	return &at, nil
}
