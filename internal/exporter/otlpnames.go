package exporter

import (
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// otlpUnitSuffixes are the units a Prometheus metric name ends in, each as
// the OTLP unit, a UCUM code, it stands for. They are the OpenTelemetry
// Collector's Prometheus translator's, read backwards: it names a metric of
// unit s with the suffix _seconds and one of unit By with _bytes, so an
// export with otlp.metric_names: otel gives a name with the suffix the unit
// and takes the suffix off. ratio is the translator's for a gauge of unit
// 1.
var otlpUnitSuffixes = map[string]string{
	"days": "d", "hours": "h", "minutes": "min", "seconds": "s",
	"milliseconds": "ms", "microseconds": "us", "nanoseconds": "ns",
	"bytes": "By", "kibibytes": "KiBy", "mebibytes": "MiBy", "gibibytes": "GiBy", "tibibytes": "TiBy",
	"kilobytes": "KBy", "megabytes": "MBy", "gigabytes": "GBy", "terabytes": "TBy",
	"meters": "m", "volts": "V", "amperes": "A", "joules": "J", "watts": "W", "grams": "g",
	"celsius": "Cel", "hertz": "Hz", "percent": "%", "ratio": "1",
}

// otlpPerUnitSuffixes are the units a name's _per_ suffix divides by, as in
// bytes_per_second, which is By/s. The translator writes a unit of
// By/m as bytes_per_minute; in UCUM m is the metre and min the minute, so a
// minute here is min.
var otlpPerUnitSuffixes = map[string]string{"second": "s", "minute": "min", "hour": "h", "day": "d"}

// otlpNameOf is the name and the unit a series of name is exported with,
// as a metric of kind: under otlp.metric_names: prometheus, name as the
// exposition writes it and no unit; under otel, as OpenTelemetry names it.
// A counter's _total is taken off, then a unit suffix (otlpUnitOf). A
// histogram's or a summary's name is its family's, which a unit suffix
// ends as a gauge's does.
func otlpNameOf(name string, kind int, otel bool) (string, string) {
	if !otel {
		return name, ""
	}
	base := name
	if kind == otlpKindSum {
		if trimmed, ok := strings.CutSuffix(base, "_total"); ok && trimmed != "" {
			base = trimmed
		}
	}
	return otlpUnitOf(base)
}

// otlpUnitOf takes a unit suffix off name, as the unit it stands for: the
// last of its _-separated words, when that is a unit of otlpUnitSuffixes,
// or its last three, when they are a unit, per and a unit of
// otlpPerUnitSuffixes. Something must be left of the name before them: a
// gauge named seconds keeps its name, and has no unit. A word that is not a
// whole word — milliseconds_x, xseconds — is no suffix.
func otlpUnitOf(name string) (string, string) {
	if head, per, ok := cutLastWord(name); ok {
		if perUnit, known := otlpPerUnitSuffixes[per]; known {
			if head, word, ok := cutLastWord(head); ok && word == "per" {
				if rest, unit, ok := cutLastWord(head); ok {
					if code, known := otlpUnitSuffixes[unit]; known && code != "1" && code != "%" {
						return rest, code + "/" + perUnit
					}
				}
			}
		}
	}
	if rest, unit, ok := cutLastWord(name); ok {
		if code, known := otlpUnitSuffixes[unit]; known {
			return rest, code
		}
	}
	return name, ""
}

// cutLastWord splits name before its last _: what comes before, which is
// not empty, and the last word.
func cutLastWord(name string) (string, string, bool) {
	i := strings.LastIndexByte(name, '_')
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	return name[:i], name[i+1:], true
}

// otlpShape is what a name is exported as besides its points: its kind and
// its unit. A name is one metric of one shape under a resource; two series
// whose names otlp.metric_names: otel makes one — a gauge jobs and a
// counter jobs_total, a gauge latency and a gauge latency_seconds — are one
// name, of two shapes where they differ in kind or unit.
type otlpShape struct {
	kind int
	unit string
}

// String names the shape as a log line does: its kind, and its unit where
// it has one.
func (s otlpShape) String() string {
	if s.unit == "" {
		return otlpKindNames[s.kind]
	}
	return otlpKindNames[s.kind] + " of unit " + s.unit
}

// otlpScopeOf is the instrumentation scope of the export's metrics and log
// records: otlp.scope, or the exporter's name without a version.
func otlpScopeOf(cfg model.OTLPConfig) otlpScope {
	if cfg.Scope == nil {
		return otlpScope{Name: model.DefaultOTLPScopeName}
	}
	return otlpScope{Name: cfg.Scope.Name, Version: cfg.Scope.Version}
}
