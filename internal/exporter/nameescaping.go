package exporter

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A collector with metric_name_validation_scheme: utf8 exports a name outside
// the classic characters, http.server.duration or a label service.name, as it
// is (transform.UTF8Names), and each answer writes it as its scraper asks:
// Prometheus 3 names an escaping in its Accept header, as Prometheus's own
// model and Go client read it,
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
// and underscores, Prometheus's default, when the range the answer is
// written for names none or one it does not know. A classic name is never
// escaped, under dots either, and label values never are. The answer's
// Content-Type says which escaping it was written with, as
// "; escaping=underscores".
//
// An answer of classic names alone — every answer of a legacy collector, and
// the self-metrics — has nothing to escape: it is written, and typed,
// without an escaping, as it always was. Only an answer of a utf8 collector
// has its names looked at, a legacy collector's being classic by its
// validation, so the look costs a legacy answer nothing.

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

// escapedLabels is labels with every name that is not classic escaped. Two
// names that escape to one, a.b and a_b under underscores, are one label to
// the scraper, which can be given one value: the classic name's, or of two
// escaped names the one first in order, so that every answer gives the same.
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

// escapedSet is s with every name that is not classic escaped by the scheme:
// underscores, dots or values. A series that has none is shared with s, and
// one that has one is a copy, its labels a new map. A histogram's or a
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

// anyUTF8Names reports whether a collector of the configuration keeps UTF-8
// names, which the static targets' answer, of every collector's, may then
// hold.
func anyUTF8Names(cfg *model.Config) bool {
	for i := range cfg.Collectors {
		if transform.UTF8Names(&cfg.Collectors[i]) {
			return true
		}
	}
	return false
}
