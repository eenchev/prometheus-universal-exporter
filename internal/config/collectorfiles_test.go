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

// collector_files lists further files of collectors (collectorfiles.go). Each
// holds a collectors list and nothing else, and a collector name is unique
// across the configuration and every file.

func loadExpectingError(t *testing.T, path string, want ...string) {
	t.Helper()
	_, err := Load(path)
	if err == nil {
		t.Fatalf("%s loaded, want an error containing %q", path, want)
	}
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not contain %q", err, fragment)
		}
	}
}

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

// A collector file holds collectors and nothing else.
func TestACollectorFileMayOnlyContainCollectors(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"web settings":           {"web:\n  self_metrics:\n    verbose: true\n" + testutil.CollectorsDocument("x"), []string{`"web" is not allowed`, "may only contain collectors", "line 1"}},
		"otlp settings":          {testutil.CollectorsDocument("x") + "otlp:\n  enabled: true\n", []string{`"otlp" is not allowed`}},
		"nested collector_files": {"collector_files: [more.yaml]\n" + testutil.CollectorsDocument("x"), []string{`"collector_files" is not allowed`}},
		"a misspelt key":         {"colectors:\n" + testutil.CollectorYAML("x"), []string{`"colectors" is not allowed`}},
		"empty file":             {"", []string{"is empty"}},
		"only a comment":         {"# nothing here\n", []string{"is empty"}},
		"empty collectors":       {"collectors: []\n", []string{"defines no collectors"}},
		"a list":                 {"- name: x\n", []string{"must be a mapping"}},
		"unknown collector key":  {strings.Replace(testutil.CollectorsDocument("x"), "    request:", "    requst: {}\n    request:", 1), []string{`unknown key "requst" in a collector`}},
		"invalid YAML":           {"collectors: [\n", []string{"yaml"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := testutil.WriteIn(t, dir, "bad.yaml", tc.body)
			conf := testutil.WriteIn(t, dir, "config.yaml", "collector_files: [bad.yaml]\n"+testutil.CollectorsDocument("own"))
			loadExpectingError(t, conf, append([]string{"collector file " + file}, tc.want...)...)
		})
	}
}

// The collectors in a file are validated exactly as the configuration's own.
func TestCollectorsInAFileAreValidated(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "bad.yaml", strings.Replace(testutil.CollectorsDocument("broken"), "expression: 'value=(\\d+)'", "expression: 'value=(\\d+'", 1))
	loadExpectingError(t, testutil.WriteIn(t, dir, "config.yaml", "collector_files: [bad.yaml]\n"), "broken")
	testutil.WriteIn(t, dir, "badname.yaml", testutil.CollectorsDocument("bad-name"))
	loadExpectingError(t, testutil.WriteIn(t, dir, "config2.yaml", "collector_files: [badname.yaml]\n"), `collector "bad-name" has invalid name`)
}

// A collector name is unique across the configuration and every collector
// file; the error names both places it was defined.
func TestCollectorNamesAreUniqueAcrossFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		config string
		files  map[string]string
		want   []string
	}{
		"configuration and file": {
			config: "collector_files: [a.yaml]\n" + testutil.CollectorsDocument("shared"),
			files:  map[string]string{"a.yaml": testutil.CollectorsDocument("shared")},
			want:   []string{`duplicate collector "shared": defined in `, "config.yaml and in ", "a.yaml"},
		},
		"two files": {
			config: "collector_files: [a.yaml, b.yaml]\n",
			files:  map[string]string{"a.yaml": testutil.CollectorsDocument("one", "shared"), "b.yaml": testutil.CollectorsDocument("shared")},
			want:   []string{`duplicate collector "shared": defined in `, "a.yaml and in ", "b.yaml"},
		},
		"two files of one pattern": {
			config: "collector_files: ['d/*.yaml']\n",
			files:  map[string]string{"d/a.yaml": testutil.CollectorsDocument("shared"), "d/b.yaml": testutil.CollectorsDocument("shared")},
			want:   []string{`duplicate collector "shared"`, "d/a.yaml and in ", "d/b.yaml"},
		},
		"within one file": {
			config: "collector_files: [a.yaml]\n",
			files:  map[string]string{"a.yaml": testutil.CollectorsDocument("shared", "shared")},
			want:   []string{`duplicate collector "shared": defined twice in `, "a.yaml"},
		},
		"within the configuration": {
			config: testutil.CollectorsDocument("shared", "shared"),
			want:   []string{`duplicate collector "shared": defined twice in `, "config.yaml"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, body := range tc.files {
				testutil.WriteIn(t, dir, file, body)
			}
			loadExpectingError(t, testutil.WriteIn(t, dir, "config.yaml", tc.config), tc.want...)
		})
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
		manager.LastMod = st.ModTime()
	}
	names := func() []string { return testutil.CollectorNames(manager.Get()) }

	// Nothing changed: no reload.
	manager.ReloadConfig()
	if manager.Get() != c {
		t.Fatal("reloaded without a change")
	}

	// A file edited.
	later := time.Now().Add(time.Second)
	testutil.WriteIn(t, dir, "collectors.d/a.yaml", testutil.CollectorsDocument("first", "second"))
	if err := os.Chtimes(filepath.Join(dir, "collectors.d/a.yaml"), later, later); err != nil {
		t.Fatal(err)
	}
	manager.ReloadConfig()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("after an edit collectors=%v", got)
	}

	// A file added.
	testutil.WriteIn(t, dir, "collectors.d/b.yaml", testutil.CollectorsDocument("third"))
	manager.ReloadConfig()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("after an addition collectors=%v", got)
	}

	// A file that would define a collector twice: rejected, and not retried
	// until something changes again.
	testutil.WriteIn(t, dir, "collectors.d/c.yaml", testutil.CollectorsDocument("first"))
	before := manager.Get()
	manager.ReloadConfig()
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
	manager.ReloadConfig()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("after a removal collectors=%v", got)
	}
}
