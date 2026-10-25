package repository

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// The examples are whatever YAML lies under examples/, at any depth: a
// configuration of its own there, or a directory holding a configuration,
// its config.yaml, with the static target file and the collector files that
// go with it. What a file directly in examples/ or beside a config.yaml is
// shows in the file, not in its name: a static target file has `targets`, a
// configuration has `collectors` or `collector_files`. In a directory, the
// configuration is config.yaml alone: any other file of collectors there or
// below is a collector file, which must be one the configuration's
// collector_files names, and is checked as the configuration loads it, not
// on its own. tools/run-example.sh tells them apart the same way. The tests
// that check "every example" take their list from here, so an example added
// in a new directory is covered without anyone naming it.
type exampleTree struct {
	configs       []string
	staticTargets []string
	// collectorFiles has the collector files of each directory's
	// configuration.
	collectorFiles map[string][]string
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
	tree, problems := exampleTreeAt(".")
	for _, problem := range problems {
		t.Error(problem)
	}
	if len(tree.configs) == 0 {
		t.Fatal("no example configurations found; this must not pass by finding nothing")
	}
	return tree
}

// exampleTreeAt reads the examples under root/examples, with their paths
// from root, and says what is wrong with the tree.
func exampleTreeAt(root string) (exampleTree, []string) {
	tree := exampleTree{collectorFiles: map[string][]string{}}
	var problems []string
	// The files of collectors in directories other than their config.yaml,
	// and each directory's configuration, from which they must be named.
	var others []string
	dirConfigs := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "examples"), func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || (filepath.Ext(full) != ".yaml" && filepath.Ext(full) != ".yml") {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		path := filepath.ToSlash(rel)
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		parts := strings.Split(path, "/")
		kind, kindErr := exampleKind(raw)
		switch {
		case kindErr != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", path, kindErr))
		case len(parts) == 2 && kind == exampleStaticTargets:
			problems = append(problems, path+": a static target file directly in examples/ has no configuration to go with; put it beside one's config.yaml in a directory")
		case len(parts) == 2:
			tree.configs = append(tree.configs, path)
		case len(parts) == 3 && (parts[2] == "config.yaml" || parts[2] == "config.yml"):
			if kind != exampleConfiguration {
				problems = append(problems, path+": the config.yaml of an example directory is its configuration, but this is a "+kind)
				return nil
			}
			if other, ok := dirConfigs[parts[1]]; ok {
				problems = append(problems, fmt.Sprintf("examples/%s has two configurations, %s and %s", parts[1], other, path))
				return nil
			}
			dirConfigs[parts[1]] = path
			tree.configs = append(tree.configs, path)
		case kind == exampleStaticTargets && len(parts) == 3:
			tree.staticTargets = append(tree.staticTargets, path)
		case kind == exampleStaticTargets:
			problems = append(problems, path+": a static target file of an example is beside its config.yaml, not below it")
		default:
			others = append(others, path)
		}
		return nil
	})
	if err != nil {
		return tree, append(problems, err.Error())
	}
	for _, path := range append(slices.Clone(tree.staticTargets), others...) {
		if dir := strings.Split(path, "/")[1]; dirConfigs[dir] == "" {
			problems = append(problems, fmt.Sprintf("%s: examples/%s has no config.yaml, the configuration of an example directory", path, dir))
		}
	}
	for _, config := range slices.Sorted(maps.Values(dirConfigs)) {
		named, err := namedCollectorFiles(root, config)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, path := range others {
			if slices.Contains(named, path) {
				tree.collectorFiles[config] = append(tree.collectorFiles[config], path)
			}
		}
	}
	for _, path := range others {
		if config := dirConfigs[strings.Split(path, "/")[1]]; config != "" && !slices.Contains(tree.collectorFiles[config], path) {
			problems = append(problems, fmt.Sprintf("%s: a file of collectors beside %s that its collector_files does not name, so nothing loads or checks it", path, config))
		}
	}
	slices.Sort(tree.configs)
	slices.Sort(tree.staticTargets)
	return tree, problems
}

// namedCollectorFiles is the files, with their paths from root, that the
// collector_files of the configuration at path from root names, each entry
// a path or a pattern against the configuration's directory as the exporter
// reads it.
func namedCollectorFiles(root, path string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	var document struct {
		CollectorFiles []string `yaml:"collector_files"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var named []string
	for _, entry := range document.CollectorFiles {
		matches, err := filepath.Glob(filepath.Join(root, filepath.Dir(path), entry))
		if err != nil {
			return nil, fmt.Errorf("%s: collector_files entry %q: %w", path, entry, err)
		}
		for _, match := range matches {
			rel, err := filepath.Rel(root, match)
			if err != nil {
				return nil, err
			}
			named = append(named, filepath.ToSlash(rel))
		}
	}
	return named, nil
}

// exampleCollectorTypes is the request types the collectors of the example
// configuration config use, in its own collectors and its collector files,
// in the order they come.
func exampleCollectorTypes(t *testing.T, tree exampleTree, config string) []string {
	t.Helper()
	var types []string
	for _, path := range append([]string{config}, tree.collectorFiles[config]...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Collectors []struct {
				Request struct {
					Type string `yaml:"type"`
				} `yaml:"request"`
			} `yaml:"collectors"`
		}
		if err := yaml.Unmarshal(raw, &document); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, collector := range document.Collectors {
			if name := collector.Request.Type; name != "" && !slices.Contains(types, name) {
				types = append(types, name)
			}
		}
	}
	return types
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

// exampleTypesNotBuilt are the request types an example's collectors use
// that this build of the tests lacks, sorted: an example of an opt-in type,
// such as examples/tcp, is loaded and checked against the schema in a build
// with its type, as CI's build with every opt-in type is, and skipped in one
// without, which would refuse it as the exporter does. A configuration's
// are those of its collector files too, and a static target file's those of
// the configuration beside it.
func exampleTypesNotBuilt(t *testing.T, tree exampleTree, path string) []string {
	t.Helper()
	if slices.Contains(tree.staticTargets, path) {
		for _, candidate := range tree.configs {
			if filepath.Dir(candidate) == filepath.Dir(path) {
				path = candidate
			}
		}
	}
	built := fetch.BuiltRequestTypes()
	var missing []string
	for _, name := range exampleCollectorTypes(t, tree, path) {
		if !slices.Contains(built, name) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// An example is skipped for the request types a build lacks and for no
// other reason: examples/tcp is skipped without the tcp type, the static
// target file beside it with it, and an example of the default build's
// types never is.
func TestAnExampleIsSkippedOnlyForATypeTheBuildLacks(t *testing.T) {
	tree := shippedExamples(t)
	if !slices.Contains(tree.configs, "examples/tcp/config.yaml") || !slices.Contains(tree.staticTargets, "examples/tcp/static-targets.yaml") {
		t.Fatalf("examples/tcp is not among the examples: %v %v", tree.configs, tree.staticTargets)
	}
	tcpBuilt := slices.Contains(fetch.BuiltRequestTypes(), "tcp")
	for _, path := range []string{"examples/tcp/config.yaml", "examples/tcp/static-targets.yaml"} {
		got := exampleTypesNotBuilt(t, tree, path)
		if want := []string{"tcp"}; tcpBuilt && len(got) != 0 || !tcpBuilt && !slices.Equal(got, want) {
			t.Errorf("%s: the types not built are %v, with tcp built %v", path, got, tcpBuilt)
		}
	}
	if got := exampleTypesNotBuilt(t, tree, "examples/open-meteo/config.yaml"); slices.Contains(fetch.BuiltRequestTypes(), "http") && len(got) != 0 {
		t.Errorf("examples/open-meteo/config.yaml: the types not built are %v", got)
	}
}
