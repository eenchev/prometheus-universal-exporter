package transform

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Prometheus 3 accepts metric and label names in any UTF-8, such as
// http.server.duration or {service.name="api"}, which a target can now
// expose. The exporter's own answer is the classic text format, whose names
// are limited to [a-zA-Z_:][a-zA-Z0-9_:]* for metrics and [a-zA-Z_][a-zA-Z0-9_]*
// for labels, as are the names every older Prometheus, recording rule and
// dashboard expects. A collector's name_escaping says what to do with a name
// outside them, whether a prometheus transform passed it through, a Python
// script emitted it or a pre-script built it:
//
//	fail         the default: the scrape fails, naming the name, as it
//	             always did, so a renamed metric never appears by surprise
//	underscores  every character a classic name may not have becomes "_":
//	             http.server.duration becomes http_server_duration
//	values       Prometheus's reversible value encoding: U__ followed by the
//	             name, "_" doubled and every other character written as _hex_:
//	             http.server.duration becomes U__http_2e_server_2e_duration,
//	             which Prometheus 3 can turn back into the original
//
// These are the escaping schemes of the same names that Prometheus itself
// negotiates with a target. Its third, dots, is left out: it rewrites every
// name with an underscore in it, classic ones included. A classic name is
// never changed by either scheme, and label values are never escaped, since
// they may hold any UTF-8. The collector's metrics_prefix is joined first, so
// a values-escaped name still starts with U__. Two names that escape to the
// same name are duplicates, and fail the scrape as duplicates do.

// The values of name_escaping.
const (
	NameEscapingFail        = "fail"
	NameEscapingUnderscores = "underscores"
	NameEscapingValues      = "values"
)

// ValidateNameEscaping checks a collector's name_escaping and fills in the
// default, fail.
func ValidateNameEscaping(c *model.Collector) error {
	switch c.NameEscaping {
	case "":
		c.NameEscaping = NameEscapingFail
	case NameEscapingFail, NameEscapingUnderscores, NameEscapingValues:
	default:
		return fmt.Errorf("collector %q name_escaping must be %s, %s or %s, not %q", c.Name, NameEscapingFail, NameEscapingUnderscores, NameEscapingValues, c.NameEscaping)
	}
	return nil
}

// classicRune reports whether r may appear at position i of a classic metric
// name (colons allowed) or label name (colons not).
func classicRune(r rune, i int, colon bool) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || colon && r == ':' || r >= '0' && r <= '9' && i > 0
}

func classicName(name string, colon bool) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !classicRune(r, i, colon) {
			return false
		}
	}
	return true
}

// escapeName escapes a name that is not classic by the scheme; a classic
// name, and any name under fail, is returned as it is.
func escapeName(name, scheme string, colon bool) string {
	if name == "" || classicName(name, colon) {
		return name
	}
	var b strings.Builder
	switch scheme {
	case NameEscapingUnderscores:
		for i, r := range name {
			if classicRune(r, i, colon) {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
	case NameEscapingValues:
		b.WriteString("U__")
		for i, r := range name {
			switch {
			case r == '_':
				b.WriteString("__")
			case classicRune(r, i, colon):
				b.WriteRune(r)
			case r == utf8.RuneError:
				b.WriteString("_FFFD_")
			default:
				b.WriteByte('_')
				b.WriteString(strconv.FormatInt(int64(r), 16))
				b.WriteByte('_')
			}
		}
	default:
		return name
	}
	return b.String()
}

// escapeNames applies the collector's name_escaping to a transform's output.
// A label map a transform may share among metrics is copied before a change.
// A label whose escaped name another label of the series already has is an
// error, rather than one of the two values silently winning.
func escapeNames(set *model.MetricSet, scheme string) error {
	if scheme == "" || scheme == NameEscapingFail {
		return nil
	}
	for i := range set.Metrics {
		m := &set.Metrics[i]
		m.Name = escapeName(m.Name, scheme, true)
		changed := false
		for k := range m.Labels {
			if !classicName(k, false) {
				changed = true
				break
			}
		}
		if !changed {
			continue
		}
		labels := make(map[string]string, len(m.Labels))
		from := make(map[string]string, len(m.Labels))
		for _, k := range model.SortedKeys(m.Labels) {
			escaped := escapeName(k, scheme, false)
			if other, clash := from[escaped]; clash {
				return fmt.Errorf("series %s: labels %q and %q both become %q with name_escaping %s", m.Name, other, k, escaped, scheme)
			}
			from[escaped] = k
			labels[escaped] = m.Labels[k]
		}
		m.Labels = labels
	}
	return nil
}
