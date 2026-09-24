package exporter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A probe and a scheduled target's scrape make their trip through collect
// alone (pipeline.go), so the two cannot drift apart again. Only a
// directory's files are decoded and transformed elsewhere (filebatch.go),
// one at a time, from inside collect.
func TestProbesAndScheduledTargetsShareOnePipeline(t *testing.T) {
	allowed := map[string]map[string]bool{
		"fetch.FetchCollector(": {"pipeline.go": true},
		"decode.Decode(":        {"pipeline.go": true, "filebatch.go": true},
		"transform.Transform(":  {"pipeline.go": true, "filebatch.go": true},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for call, where := range allowed {
			if strings.Contains(string(source), call) && !where[file] {
				t.Errorf("%s calls %s; a trip to the target belongs in collect (pipeline.go)", file, call)
			}
		}
	}
}
