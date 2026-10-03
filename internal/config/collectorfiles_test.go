package config

import (
	"strings"
	"testing"

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
