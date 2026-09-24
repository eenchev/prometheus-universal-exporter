//go:build !select_request_types || request_type_localfile

package config

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// fileCollector is a localfile collector passing a Prometheus text file
// through.
func fileCollector(name, root, path string) model.Collector {
	return model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Path: path},
		Transform:     model.TransformConfig{Type: "prometheus"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
}
