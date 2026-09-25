//go:build !select_request_types || request_type_localfile

package exporter

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A file of a directory read with more series than limits.max_metrics fails
// alone, as a limit and not as a file that did not parse, although the
// decoder is what stops at its 11th series.
func TestLocalDirectoryFileOverTheSeriesLimit(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	var big strings.Builder
	big.WriteString("# TYPE w gauge\n")
	for i := range 20 {
		fmt.Fprintf(&big, "w{i=%q} 1\n", strings.Repeat("x", i))
	}
	testutil.WriteIn(t, root, "big.prom", big.String())
	c := dirCollector("dir", root, "*.prom")
	c.Limits.MaxMetrics = 10
	server := fileServer(t, c)
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK, `v{file="a.prom"} 1`, `localfile_scrape_error{file="big.prom"} 1`)
	metrics := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_series_limit_exceeded_total{collector="dir"}`: 1,
		`http_exporter_parse_errors_total{collector="dir"}`:          0,
	} {
		if got := seriesValue(t, metrics, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}
