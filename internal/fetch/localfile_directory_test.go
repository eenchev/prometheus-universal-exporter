//go:build !select_request_types || request_type_localfile

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
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

func TestLocalDirectoryValidation(t *testing.T) {
	root := t.TempDir()
	c := dirCollector("dir", root, "*.prom")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if c.Request.MaxFiles != DefaultLocalFileMaxFiles || c.Request.MaxTotalBytes != DefaultLocalFileMaxTotalBytes {
		t.Fatalf("defaults: max_files=%d max_total_bytes=%d", c.Request.MaxFiles, c.Request.MaxTotalBytes)
	}
	for name, tc := range map[string]struct {
		change func(c *model.Collector)
		want   string
	}{
		"path too":             {func(c *model.Collector) { c.Request.Path = "a.prom" }, "both request.path and request.files"},
		"a directory":          {func(c *model.Collector) { c.Request.Files = []string{"sub/*.prom"} }, "without / or"},
		"a bad pattern":        {func(c *model.Collector) { c.Request.Files = []string{"[a"} }, "is not valid"},
		"an empty pattern":     {func(c *model.Collector) { c.Request.Files = []string{" "} }, "must not be empty"},
		"negative files":       {func(c *model.Collector) { c.Request.MaxFiles = -1 }, "max_files must not be negative"},
		"negative bytes":       {func(c *model.Collector) { c.Request.MaxTotalBytes = -1 }, "max_total_bytes must not be negative"},
		"max_files for a file": {func(c *model.Collector) { c.Request.Files = nil; c.Request.Path = "a.prom"; c.Request.MaxFiles = 3 }, "request.max_files, which applies only"},
		"max_bytes for a file": {func(c *model.Collector) {
			c.Request.Files = nil
			c.Request.Path = "a.prom"
			c.Request.MaxTotalBytes = 3
		}, "request.max_total_bytes, which applies only"},
		"files on http": {func(c *model.Collector) { *c = testutil.Collector("web", "text"); c.Request.Files = []string{"*"} }, "request.files, which does not apply"},
	} {
		t.Run(name, func(t *testing.T) {
			c := dirCollector("dir", root, "*.prom")
			tc.change(&c)
			err := ValidateRequest(&c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}
