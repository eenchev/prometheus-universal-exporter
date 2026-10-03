//go:build !select_request_types || request_type_localfile

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A localfile target names a file or a directory under the collector's root.
func TestTheCollectorsPageHintsALocalfileTarget(t *testing.T) {
	c := testutil.Collector("textfile", "text")
	c.Request = model.RequestConfig{Type: "localfile", Root: t.TempDir(), Path: "batch.prom"}
	page := getPage(t, landingServer(t, &model.Config{Collectors: []model.Collector{c}}), "/collectors")
	requireContains(t, page, `placeholder="a file or directory under its root (optional)"`)
}
