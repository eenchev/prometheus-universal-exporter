//go:build !select_request_types || request_type_http

package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The configuration's own collectors come first, then each entry's files in
// the order listed, a pattern's matches in file name order.
func TestCollectorFilesAreMergedInOrder(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "teams/b.yaml", testutil.CollectorsDocument("team_b"))
	testutil.WriteIn(t, dir, "teams/a.yaml", testutil.CollectorsDocument("team_a1", "team_a2"))
	extra := testutil.WriteIn(t, dir, "extra.yaml", testutil.CollectorsDocument("extra"))
	conf := testutil.WriteIn(t, dir, "config.yaml", "collector_files:\n  - extra.yaml\n  - teams/*.yaml\n"+testutil.CollectorsDocument("own"))

	c, err := Load(conf)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := testutil.CollectorNames(c), []string{"own", "extra", "team_a1", "team_a2", "team_b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("collectors=%v, want %v", got, want)
	}
	if want := []string{extra, filepath.Join(dir, "teams/a.yaml"), filepath.Join(dir, "teams/b.yaml")}; !reflect.DeepEqual(c.LoadedCollectorFiles, want) {
		t.Fatalf("files=%v, want %v", c.LoadedCollectorFiles, want)
	}
	if c.CollectorSources["own"] != conf || c.CollectorSources["team_b"] != filepath.Join(dir, "teams/b.yaml") {
		t.Fatalf("sources=%v", c.CollectorSources)
	}
	// Merged collectors are validated like the configuration's own: defaults
	// are applied to them too.
	if c.Collectors[4].Limits.MaxMetrics != 10000 || c.Collectors[4].Decoder.Type != "text" {
		t.Fatalf("a collector from a file was not validated: %+v", c.Collectors[4])
	}
}

// A configuration may keep every collector in files, with no collectors key.
func TestAConfigurationCanHaveOnlyCollectorFiles(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "collectors.d/app.yaml", testutil.CollectorsDocument("app"))
	c, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.d/*.yaml]\nweb:\n  self_metrics:\n    verbose: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectorNames(c); !reflect.DeepEqual(got, []string{"app"}) || !c.Web.SelfMetrics.Verbose {
		t.Fatalf("collectors=%v verbose=%v", got, c.Web.SelfMetrics.Verbose)
	}
}

// Relative entries are resolved against the configuration's directory, not
// the working directory; absolute ones are used as they are.
func TestCollectorFilePathsAreRelativeToTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	elsewhere := testutil.WriteIn(t, t.TempDir(), "absolute.yaml", testutil.CollectorsDocument("absolute"))
	testutil.WriteIn(t, dir, "relative.yaml", testutil.CollectorsDocument("relative"))
	c, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files:\n  - relative.yaml\n  - "+elsewhere+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectorNames(c); !reflect.DeepEqual(got, []string{"relative", "absolute"}) {
		t.Fatalf("collectors=%v", got)
	}
}

// Two entries that match the same file read it once, rather than failing on
// its collectors as duplicates of themselves, and the configuration itself is
// never read as a collector file.
func TestAFileIsReadOnceAndTheConfigurationIsNotAFileOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "app.yaml", testutil.CollectorsDocument("app"))
	c, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: ['*.yaml', app.yaml]\n"+testutil.CollectorsDocument("own")))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectorNames(c); !reflect.DeepEqual(got, []string{"own", "app"}) {
		t.Fatalf("collectors=%v", got)
	}
}

func TestCollectorFileEntriesThatDoNotResolve(t *testing.T) {
	dir := t.TempDir()
	// A named file must exist.
	loadExpectingError(t, testutil.WriteIn(t, dir, "missing.yaml", "collector_files: [absent.yaml]\n"+testutil.CollectorsDocument("own")), "collector file "+filepath.Join(dir, "absent.yaml"))
	// A directory is not a file; the error suggests a pattern.
	if err := os.Mkdir(filepath.Join(dir, "collectors.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	loadExpectingError(t, testutil.WriteIn(t, dir, "directory.yaml", "collector_files: [collectors.d]\n"+testutil.CollectorsDocument("own")), "is a directory", "collectors.d/*.yaml")
	// An empty entry is a mistake.
	loadExpectingError(t, testutil.WriteIn(t, dir, "blank.yaml", "collector_files: ['']\n"+testutil.CollectorsDocument("own")), "empty entry")
	// A malformed pattern is reported.
	loadExpectingError(t, testutil.WriteIn(t, dir, "pattern.yaml", "collector_files: ['[']\n"+testutil.CollectorsDocument("own")), "pattern")
	// A pattern may match nothing, so an empty directory of collector files
	// is fine while the configuration has collectors of its own...
	if _, err := Load(testutil.WriteIn(t, dir, "empty.yaml", "collector_files: ['collectors.d/*.yaml']\n"+testutil.CollectorsDocument("own"))); err != nil {
		t.Fatal(err)
	}
	// ...but not when nothing defines any.
	loadExpectingError(t, testutil.WriteIn(t, dir, "none.yaml", "collector_files: ['collectors.d/*.yaml']\n"), "no collectors")
}

// ${NAME} expansion reaches the collector files as it does the configuration.
func TestCollectorFilesAreExpandedLikeTheConfiguration(t *testing.T) {
	t.Setenv("COLLECTOR_FILE_PATH", "/from-env")
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "a.yaml", strings.Replace(testutil.CollectorsDocument("app"), "path: /status", "path: ${COLLECTOR_FILE_PATH}", 1))
	conf := testutil.WriteIn(t, dir, "config.yaml", "collector_files: [a.yaml]\n")
	c, err := Load(conf, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Collectors[0].Request.Path; got != "/from-env" {
		t.Fatalf("path=%q", got)
	}
	os.Unsetenv("COLLECTOR_FILE_ABSENT")
	testutil.WriteIn(t, dir, "a.yaml", strings.Replace(testutil.CollectorsDocument("app"), "path: /status", "path: ${COLLECTOR_FILE_ABSENT}", 1))
	if _, err := Load(conf, WithEnvExpansion()); err == nil || !strings.Contains(err.Error(), "COLLECTOR_FILE_ABSENT") || !strings.Contains(err.Error(), "a.yaml") {
		t.Fatalf("err=%v", err)
	}
}

// The watch notices a collector file being edited, added or removed, though
// the configuration file itself is untouched, and a reload that would define a
// collector twice is rejected, keeping the configuration in force.
func TestTheWatchFollowsCollectorFiles(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "collectors.d/a.yaml", testutil.CollectorsDocument("first"))
	conf := testutil.WriteIn(t, dir, "config.yaml", "collector_files: ['collectors.d/*.yaml']\n")
	c, err := Load(conf)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(c, conf, slog.Default())
	if st, err := os.Stat(conf); err == nil {
		manager.lastMod = st.ModTime()
	}
	names := func() []string { return testutil.CollectorNames(manager.Get()) }

	// Nothing changed: no reload.
	manager.reloadChanged()
	if manager.Get() != c {
		t.Fatal("reloaded without a change")
	}

	// A file edited.
	later := time.Now().Add(time.Second)
	testutil.WriteIn(t, dir, "collectors.d/a.yaml", testutil.CollectorsDocument("first", "second"))
	if err := os.Chtimes(filepath.Join(dir, "collectors.d/a.yaml"), later, later); err != nil {
		t.Fatal(err)
	}
	manager.reloadChanged()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("after an edit collectors=%v", got)
	}

	// A file added.
	testutil.WriteIn(t, dir, "collectors.d/b.yaml", testutil.CollectorsDocument("third"))
	manager.reloadChanged()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("after an addition collectors=%v", got)
	}

	// A file that would define a collector twice: rejected, and not retried
	// until something changes again.
	testutil.WriteIn(t, dir, "collectors.d/c.yaml", testutil.CollectorsDocument("first"))
	before := manager.Get()
	manager.reloadChanged()
	if manager.Get() != before {
		t.Fatalf("a duplicate collector was accepted on reload: %v", names())
	}

	// Removed again: the reload goes through.
	if err := os.Remove(filepath.Join(dir, "collectors.d/c.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "collectors.d/b.yaml")); err != nil {
		t.Fatal(err)
	}
	manager.reloadChanged()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("after a removal collectors=%v", got)
	}
}
