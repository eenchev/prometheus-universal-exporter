package decode

import (
	"bytes"
	"fmt"
	"math"
	"strings"

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
//     [timestamp]", is skipped: the model has no place for one.
//   - A family's type may be unknown, read as untyped; stateset, read as a
//     gauge, whose samples already are one per state with the value 1 or 0;
//     info, read as the gauge foo_info with its value, 1, as Prometheus reads
//     it; and gaugehistogram, read as the gauges foo_bucket (its le a plain
//     label), foo_gcount and foo_gsum, since a gauge histogram's buckets go
//     up and down and a histogram's may not.
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
			part := &promFamily{name: name + suffix, typ: model.GaugeMetricType, helpOf: family}
			p.byName[part.name] = part
			p.families = append(p.families, part)
		}
	default:
		return fmt.Errorf("unknown metric type %q", raw)
	}
	return nil
}

// withoutExemplar is the rest of a sample line after its value without the
// exemplar that may end it. An exemplar starts with a # after a blank; a
// timestamp, the only other thing that may follow the value, has none.
func withoutExemplar(s string) string {
	if i := strings.Index(s, "#"); i >= 0 && (i == 0 || isBlank(s[i-1])) {
		return s[:i]
	}
	return s
}

// openMetricsTimestamp reads an OpenMetrics timestamp, a float number of
// seconds, as milliseconds.
func openMetricsTimestamp(token string) (*int64, error) {
	seconds, err := parsePromFloat(token)
	ms := math.Round(seconds * 1000)
	if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) || ms >= math.MaxInt64 || ms < math.MinInt64 {
		return nil, fmt.Errorf("expected a number of seconds as timestamp, got %q", token)
	}
	at := int64(ms)
	return &at, nil
}
