//go:build !select_request_types || request_type_graphite || request_type_localfile

package config

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// graphiteTestCollector is a graphite collector reading the newest value of
// each series with jq. The tests of the graphite decoder's settings start
// from it, those of a localfile collector's replacing its request.
func graphiteTestCollector() model.Collector {
	return model.Collector{
		Name:      "graphite",
		Request:   model.RequestConfig{Type: fetch.RequestTypeGraphite, Targets: []string{"a.b"}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "v", Items: ".series[]", Expression: ".value"}},
	}
}
