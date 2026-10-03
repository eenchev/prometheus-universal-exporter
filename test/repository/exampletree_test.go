package repository

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// The examples are whatever YAML lies under examples/, at any depth: a file
// of its own there, or a directory holding a configuration with the static
// target file that goes with it. What a file is shows in the file, not in its
// name: a static target file has `targets`, a configuration has `collectors`
// or `collector_files`. The tests that check "every example" take their list
// from here, so an example added in a new directory is covered without anyone
// naming it.
type exampleTree struct {
	configs       []string
	staticTargets []string
}

const (
	exampleConfiguration = "configuration"
	exampleStaticTargets = "static target file"
)

// exampleKind tells what an example file is from its top-level keys, and
// refuses one that is neither or both: a file no test would know how to
// check must not sit among the examples unnoticed.
func exampleKind(raw []byte) (string, error) {
	var keys map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &keys); err != nil {
		return "", fmt.Errorf("not a YAML mapping: %w", err)
	}
	_, targets := keys["targets"]
	_, collectors := keys["collectors"]
	_, collectorFiles := keys["collector_files"]
	switch {
	case targets && !collectors && !collectorFiles:
		return exampleStaticTargets, nil
	case !targets && (collectors || collectorFiles):
		return exampleConfiguration, nil
	}
	return "", errors.New("neither a configuration nor a static target file, so no test knows how to check it")
}

func shippedExamples(t *testing.T) exampleTree {
	t.Helper()
	var tree exampleTree
	err := filepath.WalkDir("examples", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch kind, err := exampleKind(raw); {
		case err != nil:
			t.Errorf("%s: %v", path, err)
		case kind == exampleStaticTargets:
			tree.staticTargets = append(tree.staticTargets, filepath.ToSlash(path))
		default:
			tree.configs = append(tree.configs, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(tree.configs)
	slices.Sort(tree.staticTargets)
	if len(tree.configs) == 0 {
		t.Fatal("no example configurations found; this must not pass by finding nothing")
	}
	return tree
}

// The list reaches into directories: the Open-Meteo and METAR examples, each a
// configuration and its static target file in a directory of their own, are on
// it beside the single-file examples, and so are checked by every test that
// takes the list — against the schemas, as a configuration that loads, and for
// its scripts.
func TestTheExampleListReachesIntoDirectories(t *testing.T) {
	tree := shippedExamples(t)
	for _, want := range []string{"examples/config.filebeat.json-test.yaml", "examples/open-meteo/config.yaml", "examples/metar/config.yaml"} {
		if !slices.Contains(tree.configs, want) {
			t.Errorf("%s is not among the example configurations: %v", want, tree.configs)
		}
	}
	for _, want := range []string{"examples/open-meteo/static-targets.yaml", "examples/metar/static-targets.yaml"} {
		if !slices.Contains(tree.staticTargets, want) {
			t.Errorf("%s is not among the example static target files: %v", want, tree.staticTargets)
		}
	}
}

// A configuration is known by its collectors, inline or in collector files,
// and a static target file by its targets, whatever else either holds; a YAML
// file with neither, with both, or that is no mapping is refused rather than
// left out of every check.
func TestAnExampleFileIsAConfigurationOrAStaticTargetFile(t *testing.T) {
	for document, want := range map[string]string{
		"collectors: []\n": exampleConfiguration,
		"x-common: &c {a: 1}\ncollector_files: ['c.d/*.yaml']\n":   exampleConfiguration,
		"x-shared: &s {collector: a}\ninterval: 1m\ntargets: []\n": exampleStaticTargets,
		"interval: 1m\n":                "",
		"collectors: []\ntargets: []\n": "",
		"- a\n- b\n":                    "",
		"":                              "",
	} {
		got, err := exampleKind([]byte(document))
		if got != want || (err == nil) != (want != "") {
			t.Errorf("%q: kind=%q err=%v, want %q", document, got, err, want)
		}
	}
}
