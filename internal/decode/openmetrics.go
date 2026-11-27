package decode

import (
	"bytes"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// OpenMetrics 1.0 text is read by the same parser as the text format 0.0.4
// (promparse.go), with these differences, which are where the two formats
// differ:
//
//   - A timestamp is a float number of seconds, which is kept in
//     milliseconds, as the model keeps every timestamp.
//   - An exemplar after a sample's value or timestamp, " # {labels} value
//     [timestamp]", is kept on a histogram's bucket and on a counter's
//     sample, the samples OpenMetrics gives exemplars to (readExemplar).
//     One on any other sample is skipped, and so is one that does not read
//     as an exemplar or whose labels are longer than OpenMetrics allows, so
//     that a target's exemplar never fails a scrape.
//   - A family's type may be unknown, read as untyped. It may be stateset,
//     read as a gauge, whose samples already are one per state with the
//     value 1 or 0. It may be info, read as the gauge foo_info with its
//     value, 1, as Prometheus reads it. It may be gaugehistogram, read as
//     the gauges foo_bucket (its le a plain label), foo_gcount and foo_gsum,
//     since a gauge histogram's buckets go up and down and a histogram's
//     may not.
//   - A counter foo's samples are foo_total, and foo_created, the time it
//     started counting. The series is named foo_total, as Prometheus names
//     it, with the counter's type and help; a family already named foo_total
//     is read the same way. _created samples of counters, histograms and
//     summaries are read and dropped: the model has no creation time.
//   - UNIT lines are ignored, and # EOF ends the exposition: anything but
//     blank lines after it is an error. A body without it is accepted.
//
// A body is OpenMetrics when its Content-Type is application/openmetrics-text,
// or, for a file or a target that does not say which format it is, when its
// last line is # EOF, which the text format 0.0.4 has no use for. A body whose
// Content-Type is text/plain; version=0.0.4 is that, whatever its last line.

// isOpenMetrics reports whether r's body is OpenMetrics text.
func isOpenMetrics(r *fetch.HTTPResponse) bool {
	raw := strings.ToLower(r.Headers.Get("Content-Type"))
	switch ct := strings.TrimSpace(strings.Split(raw, ";")[0]); {
	case ct == "application/openmetrics-text":
		return true
	case ct == "text/plain" && strings.Contains(raw, "version=0.0.4"):
		return false
	}
	body := bytes.TrimRight(r.Body, " \t\r\n")
	last := body[bytes.LastIndexByte(body, '\n')+1:]
	return string(bytes.TrimSpace(last)) == "# EOF"
}

// openMetricsType handles an OpenMetrics TYPE line for family, whose type t
// is, lower-cased; raw is the type as written.
func (p *promParser) openMetricsType(family *promFamily, t, raw string) error {
	alias := func(name string, target *promFamily, role int) {
		if p.aliases == nil {
			p.aliases = map[string]promAlias{}
		}
		if _, taken := p.aliases[name]; !taken && p.byName[name] == nil {
			p.aliases[name] = promAlias{family: target, role: role}
		}
	}
	name := family.name
	switch t {
	case "counter":
		family.typ = model.CounterMetricType
		base, total := strings.CutSuffix(name, "_total")
		if !total {
			family.exported = name + "_total"
			alias(name+"_total", family, promRoleNone)
		}
		alias(base+"_created", family, promRoleCreated)
	case "gauge", "stateset":
		family.typ = model.GaugeMetricType
	case "unknown", "untyped":
		family.typ = model.UntypedMetricType
	case "summary":
		family.typ = model.SummaryMetricType
		alias(name+"_created", family, promRoleCreated)
	case "histogram":
		family.typ = model.HistogramMetricType
		alias(name+"_created", family, promRoleCreated)
	case "info":
		family.typ = model.GaugeMetricType
		if !strings.HasSuffix(name, "_info") {
			family.exported = name + "_info"
			alias(name+"_info", family, promRoleNone)
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
			p.newFamily(&promFamily{name: name + suffix, typ: model.GaugeMetricType, helpOf: family})
		}
	default:
		return model.Errorf("unknown metric type %s", model.Quoted(raw))
	}
	return nil
}

// splitExemplar cuts the exemplar that may end a sample line off the rest of
// the line after its value: the rest, and what follows the exemplar's #, nil
// for none. An exemplar starts with a # after a blank; a timestamp, the only
// other thing that may follow the value, has none.
func splitExemplar(s []byte) (rest, exemplar []byte) {
	if i := bytes.IndexByte(s, '#'); i >= 0 && (i == 0 || isBlank(s[i-1])) {
		return s[:i], s[i+1:]
	}
	return s, nil
}

// readExemplar reads an exemplar, "{labels} value [timestamp]", as what
// follows its #: its labels, unescaped, its value and its time, a float
// number of seconds. It is nil for text that is not one, for labels longer
// than model.MaxExemplarRunes together, for a label written twice, and for
// labels not separated by commas.
func readExemplar(s []byte) *model.Exemplar {
	s = skipBlanks(s)
	if len(s) == 0 || s[0] != '{' {
		return nil
	}
	s = s[1:]
	x := &model.Exemplar{}
	runes := 0
	for {
		s = skipBlanks(s)
		if len(s) > 0 && s[0] == '}' {
			s = s[1:]
			break
		}
		end := 0
		for end < len(s) && (s[end] == '_' || s[end] >= 'a' && s[end] <= 'z' || s[end] >= 'A' && s[end] <= 'Z' || end > 0 && s[end] >= '0' && s[end] <= '9') {
			end++
		}
		if end == 0 {
			return nil
		}
		name := string(s[:end])
		s = skipBlanks(s[end:])
		if len(s) < 2 || s[0] != '=' {
			return nil
		}
		s = skipBlanks(s[1:])
		if len(s) == 0 || s[0] != '"' {
			return nil
		}
		closing := 1
		for closing < len(s) && s[closing] != '"' {
			if s[closing] == '\\' {
				closing++
			}
			closing++
		}
		if closing >= len(s) {
			return nil
		}
		value, err := unescapePromText(string(s[1:closing]), "exemplar label value")
		if err != nil || !utf8.ValidString(value) {
			return nil
		}
		if _, twice := x.Labels[name]; twice {
			return nil
		}
		if x.Labels == nil {
			x.Labels = map[string]string{}
		}
		x.Labels[name] = value
		if runes += utf8.RuneCountInString(name) + utf8.RuneCountInString(value); runes > model.MaxExemplarRunes {
			return nil
		}
		// A comma or the closing brace follows each label.
		switch s = skipBlanks(s[closing+1:]); {
		case len(s) > 0 && s[0] == ',':
			s = s[1:]
		case len(s) == 0 || s[0] != '}':
			return nil
		}
	}
	token, s := cutBlank(skipBlanks(s))
	value, err := parsePromFloat(token)
	if err != nil {
		return nil
	}
	x.Value = value
	if token, s = cutBlank(skipBlanks(s)); len(token) > 0 {
		ms, err := openMetricsTimestamp(token)
		if err != nil || len(bytes.TrimSpace(s)) != 0 {
			return nil
		}
		x.Time = time.UnixMilli(ms).UTC()
	}
	return x
}

// openMetricsTimestamp reads an OpenMetrics timestamp, a float number of
// seconds, as milliseconds.
func openMetricsTimestamp(token []byte) (int64, error) {
	seconds, err := parsePromFloat(token)
	ms := math.Round(seconds * 1000)
	if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) || ms >= math.MaxInt64 || ms < math.MinInt64 {
		return 0, model.Errorf("expected a number of seconds as timestamp, got %s", model.Quoted(token))
	}
	return int64(ms), nil
}
