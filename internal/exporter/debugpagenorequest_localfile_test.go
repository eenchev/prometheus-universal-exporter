//go:build !select_request_types || request_type_localfile

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A localfile playground reads the pasted content alone, never the file its
// request names, which holds other series.
func TestALocalfilePlaygroundReadsNoFile(t *testing.T) {
	served, playground := t.TempDir(), t.TempDir()
	testutil.WriteIn(t, served, "app.prom", "served_series 1\n")
	testutil.WriteIn(t, playground, "app.prom", "file_series 1\n")
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			root := playground
			if source {
				root = served
			}
			return "name: " + name + "\nrequest: {type: localfile, root: '" + root + "', path: app.prom}\ntransform: {type: prometheus}\n"
		},
		target: func(string) string { return "" },
		body:   "pasted_series 2\n", series: "pasted_series 2\n",
	})
	requirePlaygroundRefusesFiles(t, "localfile")
}
