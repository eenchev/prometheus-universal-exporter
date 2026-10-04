//go:build !select_request_types || request_type_localfile

package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a trip of a localfile collector writes to the failure log from its
// own places goes nowhere once reloads have ended the stay of the collector
// it read (reconcile.go): a directory read (filebatch.go) and the carbon
// lines of a file (graphitereport.go).

// A static target's scrape that read its collector before reloads removed it
// and brought it back writes nothing to the failure log at any place where
// the read of a directory writes there, and its success ends no failure of
// the collector in force: a directory with more entries than one scrape
// lists, one with more matching files than request.max_files, and a file
// that fails and is left out. The same holds for the carbon lines the
// graphite decoder skips under response.graphite.invalid_lines: skip.
func TestALateScrapeWritesNothingToTheFailureLogOfAFileRead(t *testing.T) {
	static := func(collector string) model.StaticTarget {
		return model.StaticTarget{Name: "one", Collector: collector, Interval: model.Duration(time.Minute)}
	}
	remove := func(path string) func(*lateScrape) {
		return func(*lateScrape) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}

	// A thousand entries are listed whole, and one more is one too many
	// (fetch/localfile_directory.go).
	crowded := t.TempDir()
	testutil.WriteIn(t, crowded, "a.prom", promFile)
	for i := range 999 {
		testutil.WriteIn(t, crowded, fmt.Sprintf("entry-%03d.txt", i), "")
	}
	checkLateSite(t, lateSite{
		name: "a directory with more entries than one scrape lists", collector: dirCollector("crowded", crowded, "*.prom"), static: static("crowded"),
		fail: func(*lateScrape) { testutil.WriteIn(t, crowded, "one-more.txt", "") }, mend: remove(filepath.Join(crowded, "one-more.txt")),
		stage: "listing", failure: "directory has more entries than one scrape lists", recovery: "directory is listed whole again",
	})

	matching := t.TempDir()
	testutil.WriteIn(t, matching, "a.prom", promFile)
	oneFile := dirCollector("one_file", matching, "*.prom")
	oneFile.Request.MaxFiles = 1
	checkLateSite(t, lateSite{
		name: "a directory with more matching files than request.max_files", collector: oneFile, static: static("one_file"),
		fail: func(*lateScrape) { testutil.WriteIn(t, matching, "b.prom", promFile) }, mend: remove(filepath.Join(matching, "b.prom")),
		stage: "max_files", failure: "directory has more matching files than request.max_files", recovery: "directory is within request.max_files again",
	})

	files := t.TempDir()
	testutil.WriteIn(t, files, "a.prom", promFile)
	checkLateSite(t, lateSite{
		name: "a file of a directory that fails", collector: dirCollector("files", files, "*.prom"), static: static("files"),
		fail:  func(*lateScrape) { testutil.WriteIn(t, files, "b.prom", "app_jobs_total{queue=\n") },
		mend:  func(*lateScrape) { testutil.WriteIn(t, files, "b.prom", promFile) },
		stage: "decode", failure: "file of a directory failed", recovery: "file of a directory recovered",
	})

	lines := t.TempDir()
	now := strconv.FormatInt(time.Now().Unix(), 10)
	carbon := fileCollector("carbon", lines, "jobs.graphite")
	carbon.Transform = model.TransformConfig{Type: "jq"}
	carbon.Response.Graphite = model.GraphiteConfig{MaxAge: model.Duration(time.Hour), InvalidLines: "skip"}
	carbon.Metrics = []model.MetricRule{{Name: "jobs_done", Items: ".series[]", Expression: ".value", Labels: []model.LabelRule{{Name: "job", Expression: ".segments[1]"}}}}
	checkLateSite(t, lateSite{
		name: "carbon lines the graphite decoder skips", collector: carbon, static: static("carbon"),
		fail:  func(*lateScrape) { testutil.WriteIn(t, lines, "jobs.graphite", "jobs.a.done 1 "+now+"\njobs.b.do\n") },
		mend:  func(*lateScrape) { testutil.WriteIn(t, lines, "jobs.graphite", "jobs.a.done 1 "+now+"\n") },
		stage: "decode", failure: "carbon lines skipped", recovery: "carbon lines read whole again",
	})
}
