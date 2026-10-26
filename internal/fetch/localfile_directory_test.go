//go:build !select_request_types || request_type_localfile

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
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

// What a localfile collector reading a directory must be: patterns for the
// names of files, no path beside them, limits that are not negative, and the
// directory's limits with a directory alone. request.files on an http
// collector is in TestTheKeysOfOtherTypesDoNotApplyToHTTP.
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

// Of max_files and max_total_bytes on a collector that reads one file, the
// first in the file is named; it once was either, as a map gave them.
func TestOfTheDirectoryLimitsTheFirstInTheFileIsNamed(t *testing.T) {
	for _, order := range [][2]string{{"max_files", "max_total_bytes"}, {"max_total_bytes", "max_files"}} {
		// A map's order changes from one range to the next.
		for range 20 {
			var c model.Collector
			if err := yaml.Unmarshal([]byte("name: f\nrequest:\n  type: localfile\n  root: "+t.TempDir()+"\n  path: a.prom\n  "+order[0]+": 3\n  "+order[1]+": 3\n"), &c); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "request."+order[0]+", which applies only") {
				t.Fatalf("%v: error %v", order, err)
			}
		}
	}
}
