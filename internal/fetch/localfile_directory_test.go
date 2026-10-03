//go:build !select_request_types || request_type_localfile

package fetch

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A localfile collector reading a directory with request.files
// (localfile_directory.go, exporter/filebatch.go).

// dirCollector reads the files matching patterns under root, passing
// Prometheus text through.
func dirCollector(name, root string, patterns ...string) model.Collector {
	c := fileCollector(name, root, "")
	c.Request.Files = patterns
	return c
}
