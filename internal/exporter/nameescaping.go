package exporter

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A collector with metric_name_validation_scheme: utf8 exports a name outside
// the classic characters (http.server.duration, or a label service.name) as
// it is (transform.UTF8Names). Each answer writes it the way its scraper
// asks. Prometheus 3 names an escaping in its Accept header, as Prometheus's
// own model and Go client read it:
//
//	escaping=allow-utf-8  the name as it is, quoted: {"http.server.duration",
//	                      "service.name"="api"}
//	escaping=underscores  every character a classic name may not have as _:
//	                      http_server_duration{service_name="api"}
//	escaping=dots         . as _dot_, _ as __ and any other such character
//	                      as __: http_dot_server_dot_duration
//	escaping=values       U__ and the name, _ doubled and every other such
//	                      character as _<hex code point>_:
//	                      U__http_2e_server_2e_duration
//
// The answer uses underscores, Prometheus's default, when the range it is
// written for names no escaping or one it does not know. A classic name is
// never escaped, not even under dots, and label values never are. The
// answer's Content-Type names the escaping it was written with, as
// "; escaping=underscores".
//
// An answer of classic names only (every answer of a legacy collector, and
// the self-metrics) has nothing to escape. It is written, and typed, without
// an escaping, as it always was. Only a utf8 collector's answer has its
// names checked, since a legacy collector's names are classic by
// validation. So the check costs a legacy answer nothing.

// answerEscaping is the escaping an answer is written with for the escaping
// parameter of the media range it answers.
func answerEscaping(param string) string {
	switch param {
	case transform.NameEscapingAllowUTF8, transform.NameEscapingUnderscores, transform.NameEscapingDots, transform.NameEscapingValues:
		return param
	}
	return transform.NameEscapingUnderscores
}

// hasUTF8Names reports whether a set has a metric or label name that is not
// classic.
func hasUTF8Names(s *model.MetricSet) bool {
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if !model.ValidMetricName(m.Name) {
			return true
		}
		for k := range m.Labels {
			if !model.ValidLabelName(k) {
				return true
			}
		}
	}
	return false
}

// escapedLabels is labels with every non-classic name escaped. Two names
// that escape to the same name (a.b and a_b under underscores) are one label
// to the scraper, which can take one value. It gets the classic name's value,
// or of two escaped names the first in order, so every answer gives the same.
func escapedLabels(labels map[string]string, scheme string) map[string]string {
	out := make(map[string]string, len(labels))
	for _, k := range model.SortedKeys(labels) {
		if model.ValidLabelName(k) {
			out[k] = labels[k]
		}
	}
	for _, k := range model.SortedKeys(labels) {
		if model.ValidLabelName(k) {
			continue
		}
		if escaped := transform.EscapeName(k, scheme, false); !hasKey(out, escaped) {
			out[escaped] = labels[k]
		}
	}
	return out
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// escapedSet is s with every non-classic name escaped by the scheme:
// underscores, dots or values. A series without such a name is shared with
// s. A series with one is a copy with a new labels map. A histogram's or a
// summary's family name is escaped, and its _bucket, _sum and _count follow
// it, as Prometheus's Go client escapes a family.
func escapedSet(s *model.MetricSet, scheme string) *model.MetricSet {
	out := &model.MetricSet{Metrics: make([]model.Metric, len(s.Metrics))}
	for i, m := range s.Metrics {
		m.Name = transform.EscapeName(m.Name, scheme, true)
		for k := range m.Labels {
			if model.ValidLabelName(k) {
				continue
			}
			m.Labels = escapedLabels(m.Labels, scheme)
			break
		}
		out.Metrics[i] = m
	}
	return out
}

// debugExposition is the text a debug report shows of an answer: the text
// format, a name that is not classic written quoted, as it is, rather than
// as any one scraper would have it escaped.
func debugExposition(s *model.MetricSet) []byte {
	return expositionFormat{escaping: transform.NameEscapingAllowUTF8}.appendMetricSet(nil, s)
}
