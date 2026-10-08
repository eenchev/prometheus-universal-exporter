//go:build !select_request_types || request_type_localfile

package exporter

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The report of a directory read at the end of a path of four kilobytes, as
// long as a path may be, lists the directory as it did: its line with the
// path, the files read and the files skipped are each within the bound of a
// line, since a path is no longer than 4,096 bytes and a name than 255, and
// are written as they were. The one line that is not is the log's, which
// names the directory twice, as the target and as the path read: it is
// shown by its first 8,192 bytes and its length, and nothing else of the
// report differs from what it was.
func TestTheReportOfADirectoryIsCutOnlyWhereALineIsTooLong(t *testing.T) {
	testutil.CaptureLogs(t)
	root := t.TempDir()
	var deep string
	for len(deep) < 4000 {
		deep = filepath.Join(deep, strings.Repeat("d", 250))
	}
	tree, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	if err := tree.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	directory, err := tree.OpenRoot(deep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	var names []string
	for i := range 5 {
		name := fmt.Sprintf("%d%s.prom", i, strings.Repeat("f", 240))
		names = append(names, name)
		if err := directory.WriteFile(name, []byte(promFile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := dirCollector("d", root, "*.prom")
	c.Request.MaxFiles = 2
	server := fileServer(t, c)

	trip := debugTripOf(t, server, "d", deep, url.Values{})
	report, old := trip.report(), trip.old()
	requireSame(t, "a directory at the end of a long path", report, cutAsDocumented(old))
	full := filepath.Join(root, deep)
	listing := fmt.Sprintf("\n  Directory %s, 2 files read\n    %s: %d bytes\n    %s: %d bytes\n", full, names[0], len(promFile), names[1], len(promFile)) +
		"    " + strings.Join(names[2:], ": skipped, over request.max_files\n    ") + ": skipped, over request.max_files\n"
	assertContains(t, report, listing+"\nStages\n", "A probe would have answered 200 with ")
	logged := `  level=WARN msg="directory has more matching files than request.max_files; the rest were skipped" collector=d target=` + deep + " directory=" + full
	var cut []string
	for _, line := range strings.Split(report, "\n") {
		if strings.HasSuffix(line, " bytes)") && !strings.Contains(old, line+"\n") {
			cut = append(cut, line)
		}
	}
	if len(cut) != 1 || !strings.HasPrefix(cut[0], logged[:lineLimit]+"... (") || !strings.Contains(old, logged+" matched=5 max_files=2 skipped=3 first_skipped="+names[2]+"\n") {
		t.Fatalf("%d lines of the report are cut, want the log's line of the skipped files alone: %.200q", len(cut), cut)
	}
	if longest, line := longestLine(report); longest > mostCutLine {
		t.Errorf("a line of the report is %d bytes, %.80q", longest, line)
	}
}
