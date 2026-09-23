package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// collector_files lists further files of collectors (collectorfiles.go). Each
// holds a collectors list and nothing else, and a collector name is unique
// across the configuration and every file.

// collectorYAML is one regex collector named name, indented as an item of a
// top-level collectors list.
func collectorYAML(name string) string {
	return `  - name: ` + name + `
    request:
      type: http
      path: /status
    transform:
      type: regex
    metrics:
      - name: ` + name + `_value
        expression: 'value=(\d+)'
`
}

// collectorsDocument is a collectors list of the named collectors.
func collectorsDocument(names ...string) string {
	var b strings.Builder
	b.WriteString("collectors:\n")
	for _, name := range names {
		b.WriteString(collectorYAML(name))
	}
	return b.String()
}

// writeIn writes a file under dir, creating its directories.
func writeIn(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func collectorNames(c *Config) []string {
	names := make([]string, 0, len(c.Collectors))
	for _, x := range c.Collectors {
		names = append(names, x.Name)
	}
	return names
}

func loadExpectingError(t *testing.T, path string, want ...string) {
	t.Helper()
	_, err := LoadConfig(path)
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
	writeIn(t, dir, "teams/b.yaml", collectorsDocument("team_b"))
	writeIn(t, dir, "teams/a.yaml", collectorsDocument("team_a1", "team_a2"))
	extra := writeIn(t, dir, "extra.yaml", collectorsDocument("extra"))
	config := writeIn(t, dir, "config.yaml", "collector_files:\n  - extra.yaml\n  - teams/*.yaml\n"+collectorsDocument("own"))

	c, err := LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := collectorNames(c), []string{"own", "extra", "team_a1", "team_a2", "team_b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("collectors=%v, want %v", got, want)
	}
	if want := []string{extra, filepath.Join(dir, "teams/a.yaml"), filepath.Join(dir, "teams/b.yaml")}; !reflect.DeepEqual(c.LoadedCollectorFiles, want) {
		t.Fatalf("files=%v, want %v", c.LoadedCollectorFiles, want)
	}
	if c.CollectorSources["own"] != config || c.CollectorSources["team_b"] != filepath.Join(dir, "teams/b.yaml") {
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
	writeIn(t, dir, "collectors.d/app.yaml", collectorsDocument("app"))
	c, err := LoadConfig(writeIn(t, dir, "config.yaml", "collector_files: [collectors.d/*.yaml]\nweb:\n  self_metrics:\n    verbose: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectorNames(c); !reflect.DeepEqual(got, []string{"app"}) || !c.Web.SelfMetrics.Verbose {
		t.Fatalf("collectors=%v verbose=%v", got, c.Web.SelfMetrics.Verbose)
	}
}

// Relative entries are resolved against the configuration's directory, not
// the working directory; absolute ones are used as they are.
func TestCollectorFilePathsAreRelativeToTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	elsewhere := writeIn(t, t.TempDir(), "absolute.yaml", collectorsDocument("absolute"))
	writeIn(t, dir, "relative.yaml", collectorsDocument("relative"))
	c, err := LoadConfig(writeIn(t, dir, "config.yaml", "collector_files:\n  - relative.yaml\n  - "+elsewhere+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectorNames(c); !reflect.DeepEqual(got, []string{"relative", "absolute"}) {
		t.Fatalf("collectors=%v", got)
	}
}

// A collector file holds collectors and nothing else.
func TestACollectorFileMayOnlyContainCollectors(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"web settings":           {"web:\n  self_metrics:\n    verbose: true\n" + collectorsDocument("x"), []string{`"web" is not allowed`, "may only contain collectors", "line 1"}},
		"otlp settings":          {collectorsDocument("x") + "otlp:\n  enabled: true\n", []string{`"otlp" is not allowed`}},
		"nested collector_files": {"collector_files: [more.yaml]\n" + collectorsDocument("x"), []string{`"collector_files" is not allowed`}},
		"a misspelt key":         {"colectors:\n" + collectorYAML("x"), []string{`"colectors" is not allowed`}},
		"empty file":             {"", []string{"is empty"}},
		"only a comment":         {"# nothing here\n", []string{"is empty"}},
		"empty collectors":       {"collectors: []\n", []string{"defines no collectors"}},
		"a list":                 {"- name: x\n", []string{"must be a mapping"}},
		"unknown collector key":  {strings.Replace(collectorsDocument("x"), "    request:", "    requst: {}\n    request:", 1), []string{"field requst not found"}},
		"invalid YAML":           {"collectors: [\n", []string{"yaml"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := writeIn(t, dir, "bad.yaml", tc.body)
			config := writeIn(t, dir, "config.yaml", "collector_files: [bad.yaml]\n"+collectorsDocument("own"))
			loadExpectingError(t, config, append([]string{"collector file " + file}, tc.want...)...)
		})
	}
}

// The collectors in a file are validated exactly as the configuration's own.
func TestCollectorsInAFileAreValidated(t *testing.T) {
	dir := t.TempDir()
	writeIn(t, dir, "bad.yaml", strings.Replace(collectorsDocument("broken"), "expression: 'value=(\\d+)'", "expression: 'value=(\\d+'", 1))
	loadExpectingError(t, writeIn(t, dir, "config.yaml", "collector_files: [bad.yaml]\n"), "broken")
	writeIn(t, dir, "badname.yaml", collectorsDocument("bad-name"))
	loadExpectingError(t, writeIn(t, dir, "config2.yaml", "collector_files: [badname.yaml]\n"), `collector "bad-name" has invalid name`)
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
			config: "collector_files: [a.yaml]\n" + collectorsDocument("shared"),
			files:  map[string]string{"a.yaml": collectorsDocument("shared")},
			want:   []string{`duplicate collector "shared": defined in `, "config.yaml and in ", "a.yaml"},
		},
		"two files": {
			config: "collector_files: [a.yaml, b.yaml]\n",
			files:  map[string]string{"a.yaml": collectorsDocument("one", "shared"), "b.yaml": collectorsDocument("shared")},
			want:   []string{`duplicate collector "shared": defined in `, "a.yaml and in ", "b.yaml"},
		},
		"two files of one pattern": {
			config: "collector_files: ['d/*.yaml']\n",
			files:  map[string]string{"d/a.yaml": collectorsDocument("shared"), "d/b.yaml": collectorsDocument("shared")},
			want:   []string{`duplicate collector "shared"`, "d/a.yaml and in ", "d/b.yaml"},
		},
		"within one file": {
			config: "collector_files: [a.yaml]\n",
			files:  map[string]string{"a.yaml": collectorsDocument("shared", "shared")},
			want:   []string{`duplicate collector "shared": defined twice in `, "a.yaml"},
		},
		"within the configuration": {
			config: collectorsDocument("shared", "shared"),
			want:   []string{`duplicate collector "shared": defined twice in `, "config.yaml"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, body := range tc.files {
				writeIn(t, dir, file, body)
			}
			loadExpectingError(t, writeIn(t, dir, "config.yaml", tc.config), tc.want...)
		})
	}
}

// Two entries that match the same file read it once, rather than failing on
// its collectors as duplicates of themselves, and the configuration itself is
// never read as a collector file.
func TestAFileIsReadOnceAndTheConfigurationIsNotAFileOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeIn(t, dir, "app.yaml", collectorsDocument("app"))
	c, err := LoadConfig(writeIn(t, dir, "config.yaml", "collector_files: ['*.yaml', app.yaml]\n"+collectorsDocument("own")))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectorNames(c); !reflect.DeepEqual(got, []string{"own", "app"}) {
		t.Fatalf("collectors=%v", got)
	}
}

func TestCollectorFileEntriesThatDoNotResolve(t *testing.T) {
	dir := t.TempDir()
	// A named file must exist.
	loadExpectingError(t, writeIn(t, dir, "missing.yaml", "collector_files: [absent.yaml]\n"+collectorsDocument("own")), "collector file "+filepath.Join(dir, "absent.yaml"))
	// A directory is not a file; the error suggests a pattern.
	if err := os.Mkdir(filepath.Join(dir, "collectors.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	loadExpectingError(t, writeIn(t, dir, "directory.yaml", "collector_files: [collectors.d]\n"+collectorsDocument("own")), "is a directory", "collectors.d/*.yaml")
	// An empty entry is a mistake.
	loadExpectingError(t, writeIn(t, dir, "blank.yaml", "collector_files: ['']\n"+collectorsDocument("own")), "empty entry")
	// A malformed pattern is reported.
	loadExpectingError(t, writeIn(t, dir, "pattern.yaml", "collector_files: ['[']\n"+collectorsDocument("own")), "pattern")
	// A pattern may match nothing, so an empty directory of collector files
	// is fine while the configuration has collectors of its own...
	if _, err := LoadConfig(writeIn(t, dir, "empty.yaml", "collector_files: ['collectors.d/*.yaml']\n"+collectorsDocument("own"))); err != nil {
		t.Fatal(err)
	}
	// ...but not when nothing defines any.
	loadExpectingError(t, writeIn(t, dir, "none.yaml", "collector_files: ['collectors.d/*.yaml']\n"), "no collectors")
}

// ${NAME} expansion reaches the collector files as it does the configuration.
func TestCollectorFilesAreExpandedLikeTheConfiguration(t *testing.T) {
	t.Setenv("COLLECTOR_FILE_PATH", "/from-env")
	dir := t.TempDir()
	writeIn(t, dir, "a.yaml", strings.Replace(collectorsDocument("app"), "path: /status", "path: ${COLLECTOR_FILE_PATH}", 1))
	config := writeIn(t, dir, "config.yaml", "collector_files: [a.yaml]\n")
	c, err := LoadConfig(config, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Collectors[0].Request.Path; got != "/from-env" {
		t.Fatalf("path=%q", got)
	}
	os.Unsetenv("COLLECTOR_FILE_ABSENT")
	writeIn(t, dir, "a.yaml", strings.Replace(collectorsDocument("app"), "path: /status", "path: ${COLLECTOR_FILE_ABSENT}", 1))
	if _, err := LoadConfig(config, WithEnvExpansion()); err == nil || !strings.Contains(err.Error(), "COLLECTOR_FILE_ABSENT") || !strings.Contains(err.Error(), "a.yaml") {
		t.Fatalf("err=%v", err)
	}
}

// Startup and --dry-run both refuse a duplicate across files, and the dry run
// reports which collector files it read.
func TestStartupAndDryRunRefuseDuplicateCollectors(t *testing.T) {
	dir := t.TempDir()
	writeIn(t, dir, "a.yaml", collectorsDocument("demo"))
	duplicate := writeIn(t, dir, "duplicate.yaml", "collector_files: [a.yaml]\n"+collectorsDocument("demo"))
	check := runCheckCLI(t, "--config.file="+duplicate)
	if check.code != 1 || !strings.Contains(strings.Join(check.result(t, "config").Errors, "\n"), `duplicate collector "demo"`) {
		t.Fatalf("exit=%d\n%s", check.code, check.stdout)
	}
	if start := runCLI(t, "--config.file="+duplicate); start.code != 1 || !strings.Contains(start.stderr, `duplicate collector \"demo\"`) {
		t.Fatalf("startup exit=%d stderr=%s", start.code, start.stderr)
	}

	fine := writeIn(t, dir, "fine.yaml", "collector_files: [a.yaml]\n"+collectorsDocument("own"))
	check = runCheckCLI(t, "--config.file="+fine)
	details := check.result(t, "config").Details
	if check.code != 0 || !reflect.DeepEqual(details["collectors"], []any{"own", "demo"}) || !reflect.DeepEqual(details["collector_files"], []any{filepath.Join(dir, "a.yaml")}) {
		t.Fatalf("exit=%d\n%s", check.code, check.stdout)
	}
}

// The watch notices a collector file being edited, added or removed, though
// the configuration file itself is untouched, and a reload that would define a
// collector twice is rejected, keeping the configuration in force.
func TestTheWatchFollowsCollectorFiles(t *testing.T) {
	dir := t.TempDir()
	writeIn(t, dir, "collectors.d/a.yaml", collectorsDocument("first"))
	config := writeIn(t, dir, "config.yaml", "collector_files: ['collectors.d/*.yaml']\n")
	c, err := LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(c, config, slog.Default())
	if st, err := os.Stat(config); err == nil {
		manager.lastMod = st.ModTime()
	}
	names := func() []string { return collectorNames(manager.Get()) }

	// Nothing changed: no reload.
	manager.reloadConfig()
	if manager.Get() != c {
		t.Fatal("reloaded without a change")
	}

	// A file edited.
	later := time.Now().Add(time.Second)
	writeIn(t, dir, "collectors.d/a.yaml", collectorsDocument("first", "second"))
	if err := os.Chtimes(filepath.Join(dir, "collectors.d/a.yaml"), later, later); err != nil {
		t.Fatal(err)
	}
	manager.reloadConfig()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("after an edit collectors=%v", got)
	}

	// A file added.
	writeIn(t, dir, "collectors.d/b.yaml", collectorsDocument("third"))
	manager.reloadConfig()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("after an addition collectors=%v", got)
	}

	// A file that would define a collector twice: rejected, and not retried
	// until something changes again.
	writeIn(t, dir, "collectors.d/c.yaml", collectorsDocument("first"))
	before := manager.Get()
	manager.reloadConfig()
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
	manager.reloadConfig()
	if got := names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("after a removal collectors=%v", got)
	}
}

// collector-file.schema.json is committed, current, printed by its flag, and
// describes collectors exactly as the configuration schema does.
func TestCommittedCollectorFileSchemaIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(collectorFileSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := collectorFileSchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, generated) {
		t.Fatalf("%s is out of date; regenerate it with: go run . --config.collector-file-schema > %s", collectorFileSchemaFile, collectorFileSchemaFile)
	}
	out := runCLI(t, "--config.collector-file-schema")
	if out.code != 0 || out.stdout != string(generated) || out.stderr != "" {
		t.Fatalf("exit=%d stderr=%q", out.code, out.stderr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(generated, &parsed); err != nil {
		t.Fatal(err)
	}
	fileItems := collectorFileSchema()["properties"].(map[string]any)["collectors"].(map[string]any)["items"]
	configItems := configSchema()["properties"].(map[string]any)["collectors"].(map[string]any)["items"]
	if !reflect.DeepEqual(fileItems, configItems) {
		t.Fatal("the collector file schema describes collectors differently from the configuration schema")
	}
}

func TestCollectorFileSchemaAcceptsCollectorsOnly(t *testing.T) {
	schema := loadSchemaFile(t, collectorFileSchemaFile)
	valid := collectorsDocument("demo")
	for name, tc := range map[string]struct {
		document string
		valid    bool
	}{
		"collectors":        {valid, true},
		"another key":       {"web: {}\n" + valid, false},
		"collector_files":   {"collector_files: [x.yaml]\n" + valid, false},
		"no collectors":     {"collectors: []\n", false},
		"nothing":           {"{}\n", false},
		"invalid collector": {strings.Replace(valid, "name: demo", "name: bad-name", 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := yaml.Unmarshal([]byte(tc.document), &doc); err != nil {
				t.Fatal(err)
			}
			errs := validateAgainstSchema(schema, normalizeYAML(doc))
			if tc.valid != (len(errs) == 0) {
				t.Fatalf("valid=%v, errors=%v", tc.valid, errs)
			}
		})
	}
	// The configuration schema accepts a configuration made of collector
	// files alone.
	var doc any
	if err := yaml.Unmarshal([]byte("collector_files: ['collectors.d/*.yaml']\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(loadSchema(t), normalizeYAML(doc)); len(errs) > 0 {
		t.Fatal(errs)
	}
}

// In the chart, every key of config.data is a file beside config.yaml, which is
// how collector files are supplied. The template renders every key and the
// whole ConfigMap is mounted, and the example the chart README documents loads
// as the exporter would load it, beside the scheduled target file the chart
// renders into the same directory.
func TestChartSuppliesCollectorFilesAsConfigMapKeys(t *testing.T) {
	configmap := readChartFile(t, "templates/configmap.yaml")
	if !strings.Contains(configmap, "range $name, $content := .Values.config.data") {
		t.Fatal("the ConfigMap template must render every key of config.data")
	}
	deployment := readChartFile(t, "templates/deployment.yaml")
	const configVolume = "      volumes:\n        - name: config\n"
	_, volume, found := strings.Cut(deployment, configVolume)
	if !found {
		t.Fatal("the deployment has no configuration volume")
	}
	volume, _, _ = strings.Cut(volume, "        - ")
	if !strings.Contains(volume, "configMap:") || strings.Contains(volume, "items:") {
		t.Fatalf("the configuration volume must mount the whole ConfigMap:\n%s", volume)
	}

	readme := readChartFile(t, "README.md")
	start := strings.Index(readme, "### Collector files")
	if start < 0 {
		t.Fatal("the chart README does not document collector files")
	}
	_, block, _ := strings.Cut(readme[start:], "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal([]byte(block), &values); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range values.Config.Data {
		writeIn(t, dir, name, content)
	}
	writeIn(t, dir, "targets.yaml", "targets: []\n")
	c, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectorNames(c); !reflect.DeepEqual(got, []string{"payments_api", "search_api"}) {
		t.Fatalf("collectors=%v", got)
	}
}
