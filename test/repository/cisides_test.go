package repository

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// CI runs the target allocator's tests when its filter output, allocator,
// names a change, and the exporter's when exporter does. The two lists are
// written out in ci.yml, so a package one side comes to import, or a file its
// tests come to open, would be left out of its list without anybody noticing:
// a change to it would then merge with that side's tests never run. These
// tests work out what each side needs and hold the lists to it.

// ciSides are the sides of CI that test packages of their own, each named as
// tools/ci-packages.sh and the filter output name it.
var ciSides = []string{"allocator", "exporter"}

// ciSideRuns are the files outside any package each side's run needs beside
// those its packages are built from and their tests open: the module files,
// the workflow that runs the tests and the script listing the packages, and
// for the exporter the script of the request types' tags its matrix job runs
// and what its Python tests run with, the Python version the Dockerfile
// names and the modules test/python pins.
var ciSideRuns = map[string][]string{
	"allocator": {"go.mod", "go.sum", ".github/workflows/ci.yml", "tools/ci-packages.sh"},
	"exporter": {
		"go.mod", "go.sum", ".github/workflows/ci.yml", "tools/ci-packages.sh",
		"tools/request-type-tags.sh", "Dockerfile", "test/python/requirements.txt",
	},
}

// ciFilters reads the changed-path filter of ci.yml's job test, each output
// with its patterns. The other jobs carry the same filter, which
// TestTheGoJobsOfCIStartWithTheSetupOfTest keeps so.
func ciFilters(t *testing.T) map[string][]string {
	t.Helper()
	for _, step := range workflowSteps(t, ".github/workflows/ci.yml")["test"] {
		if uses, _ := step["uses"].(string); !strings.HasPrefix(uses, "dorny/paths-filter@") {
			continue
		}
		with, _ := step["with"].(map[string]any)
		text, _ := with["filters"].(string)
		var filters map[string][]string
		if err := yaml.Unmarshal([]byte(text), &filters); err != nil {
			t.Fatalf("ci.yml's changed-path filter is no mapping of outputs to patterns: %v", err)
		}
		return filters
	}
	t.Fatal("ci.yml's job test has no changed-path filter")
	return nil
}

// filterMatch reports whether a pattern of the changed-path filter matches a
// file, given relative to the repository's root with slashes, as
// paths-filter's picomatch does with its dot option: a ** segment matches
// any number of directories, none among them, and any other segment matches
// one name, * and ? never across a slash.
func filterMatch(pattern, file string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for skip := 0; skip <= len(name); skip++ {
			if matchSegments(pattern[1:], name[skip:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, err := filepath.Match(pattern[0], name[0]); err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], name[1:])
}

// matchesAny reports whether any of the patterns matches the file.
func matchesAny(patterns []string, file string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool { return filterMatch(pattern, file) })
}

// ciSidePackages runs tools/ci-packages.sh for a side, as CI does.
func ciSidePackages(t *testing.T, side string) []string {
	t.Helper()
	out, err := exec.Command("sh", "tools/ci-packages.sh", side).Output()
	if err != nil {
		t.Fatalf("sh tools/ci-packages.sh %s: %v", side, err)
	}
	return strings.Fields(string(out))
}

// listedPackage is what go list -json says of a package, as far as the
// files a side needs go.
type listedPackage struct {
	ImportPath, Dir, ForTest                     string
	Module                                       *struct{ Main bool }
	GoFiles, CgoFiles, TestGoFiles, XTestGoFiles []string
	IgnoredGoFiles, OtherFiles                   []string
	EmbedFiles, TestEmbedFiles, XTestEmbedFiles  []string
}

// goSources are the Go files of a package, whatever build tags they need.
func (p *listedPackage) goSources() []string {
	return slices.Concat(p.GoFiles, p.CgoFiles, p.TestGoFiles, p.XTestGoFiles, p.IgnoredGoFiles)
}

// ciNeeded works out, once for each side, every file of the repository its
// run needs: the files of each package of the module its packages and their
// tests are built from — every Go file, whatever its build tags, the
// embedded files and the package's testdata —, every existing path outside
// its own directory a string literal of one of the side's own packages names
// (a test opening ../../examples/x.yaml, or the root package's tests opening
// examples/x.yaml), and ciSideRuns.
var ciNeeded = sync.OnceValues(func() (map[string]map[string]bool, error) {
	needed := map[string]map[string]bool{}
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for _, side := range ciSides {
		files := map[string]bool{}
		for _, file := range ciSideRuns[side] {
			files[file] = true
		}
		out, err := exec.Command("sh", "tools/ci-packages.sh", side).Output()
		if err != nil {
			return nil, err
		}
		own := strings.Fields(string(out))
		cmd := exec.Command("go", append([]string{"list", "-deps", "-test", "-json"}, own...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		listed, err := cmd.Output()
		if err != nil {
			return nil, errors.New(strings.TrimSpace(stderr.String()))
		}
		decoder := json.NewDecoder(bytes.NewReader(listed))
		for {
			var p listedPackage
			if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, err
			}
			if p.Module == nil || !p.Module.Main || p.ForTest != "" || strings.HasSuffix(p.ImportPath, ".test") {
				continue
			}
			dir, err := filepath.Rel(root, p.Dir)
			if err != nil {
				return nil, err
			}
			for _, name := range slices.Concat(p.goSources(), p.OtherFiles, p.EmbedFiles, p.TestEmbedFiles, p.XTestEmbedFiles) {
				files[filepath.ToSlash(filepath.Join(dir, name))] = true
			}
			if err := addTree(files, filepath.Join(dir, "testdata")); err != nil {
				return nil, err
			}
			if slices.Contains(own, p.ImportPath) {
				if err := addNamedPaths(files, dir, p.goSources()); err != nil {
					return nil, err
				}
			}
		}
		needed[side] = files
	}
	return needed, nil
})

// addTree adds every file under a directory, or the file itself, to files;
// a path that does not exist adds nothing.
func addTree(files map[string]bool, root string) error {
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files[filepath.ToSlash(file)] = true
		}
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// addNamedPaths adds to files every existing path of the repository outside
// the package's directory, dir, that a string literal of the package's
// files names relative to it, a glob's matches for one with a glob's
// characters, and every file under a directory one names.
func addNamedPaths(files map[string]bool, dir string, sources []string) error {
	for _, source := range sources {
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, source), nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		var walkErr error
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING || walkErr != nil {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\n\x00") {
				return true
			}
			named := filepath.Clean(filepath.Join(dir, value))
			// What is named before a glob's first character: a glob such as
			// the XPath ../* names nothing past the directory the package
			// is in.
			fixed := named
			if at := strings.IndexAny(fixed, "*?["); at >= 0 {
				fixed = filepath.Dir(fixed[:at+1])
			}
			inside, err := filepath.Rel(dir, fixed)
			if err != nil || !leavesDir(inside) || leavesDir(fixed) || strings.HasPrefix(dir+"/", fixed+"/") || fixed == "." {
				// In the package's directory, outside the repository, or a
				// directory the package is in, as the "../" a check of
				// paths compares with names.
				return true
			}
			matches := []string{named}
			if strings.ContainsAny(value, "*?[") {
				if matches, err = filepath.Glob(named); err != nil {
					return true
				}
			}
			for _, match := range matches {
				if walkErr = addTree(files, match); walkErr != nil {
					return false
				}
			}
			return true
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

// leavesDir reports whether a clean relative path leads out of the directory
// it is relative to: a name such as ..x stays in it.
func leavesDir(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "../")
}

// repositoryFiles walks the repository's files, the module cache's vendor
// copy, git's directory and build output left out.
func repositoryFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch file {
			case ".git", "vendor", "dist", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, filepath.ToSlash(file))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// sideFilterProblems says what is wrong with a side's patterns given the
// files its run needs: a needed file no pattern matches, and a pattern that
// matches none of them.
func sideFilterProblems(side string, patterns []string, needed map[string]bool) []string {
	var problems []string
	files := make([]string, 0, len(needed))
	for file := range needed {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		if !matchesAny(patterns, file) {
			problems = append(problems, "ci.yml's "+side+" filter matches no pattern for "+file+", which the "+side+"'s tests need: a change to it would merge without them run")
		}
	}
	for _, pattern := range patterns {
		if !slices.ContainsFunc(files, func(file string) bool { return filterMatch(pattern, file) }) {
			problems = append(problems, "ci.yml's "+side+" filter has the pattern "+pattern+", which matches no file the "+side+"'s tests need")
		}
	}
	return problems
}

// Each side's filter matches every file its tests need — the files of every
// package of the module its packages and their tests are built from, the
// paths outside their directories its packages' tests open, and what CI runs
// them with — and has no pattern that matches none of them. Every file a
// side's filter matches the go filter matches too, so a change that runs a
// side's tests also runs the setup steps they need, which run under go.
func TestEachSideOfCIRunsWhenAFileItsTestsNeedChanges(t *testing.T) {
	needed, err := ciNeeded()
	if err != nil {
		t.Fatalf("could not work out the files each side of CI needs: %v", err)
	}
	filters := ciFilters(t)
	files := repositoryFiles(t)
	for _, side := range ciSides {
		patterns, ok := filters[side]
		if !ok {
			t.Errorf("ci.yml's changed-path filter has no %s output", side)
			continue
		}
		for _, problem := range sideFilterProblems(side, patterns, needed[side]) {
			t.Error(problem)
		}
		for _, file := range files {
			if matchesAny(patterns, file) && !matchesAny(filters["go"], file) {
				t.Errorf("ci.yml's %s filter matches %s and its go filter does not, so the setup steps a change to it needs would not run", side, file)
			}
		}
	}
	// What the sides need today: most of internal/ for both, and the
	// examples for the exporter, whose tests run them.
	for side, file := range map[string]string{"allocator": "internal/config/config.go", "exporter": "examples/config.ecb.xml-test.yaml"} {
		if !needed[side][file] {
			t.Errorf("%s was not found among the files the %s's tests need; the reader has stopped finding them", file, side)
		}
	}
}

// A side's filter that loses a directory its tests need, or gains one they
// do not, is told apart from the right one by sideFilterProblems.
func TestAFilterMissingOrPastWhatItsSideNeedsIsNoticed(t *testing.T) {
	needed, err := ciNeeded()
	if err != nil {
		t.Fatalf("could not work out the files each side of CI needs: %v", err)
	}
	patterns := ciFilters(t)["allocator"]
	if problems := sideFilterProblems("allocator", patterns, needed["allocator"]); len(problems) != 0 {
		t.Fatalf("the allocator's filter as ci.yml has it was found wrong: %q", problems)
	}
	for _, c := range []struct{ name, remove, add, want string }{
		{"a directory its tests import left out", "internal/config/**", "", "matches no pattern for internal/config/config.go"},
		{"its own directory left out", "cmd/pue-allocator/**", "", "matches no pattern for cmd/pue-allocator/main.go"},
		{"a directory only the exporter imports added", "", "internal/grpctest/**", "has the pattern internal/grpctest/**, which matches no file"},
		{"a file nothing reads added", "", "Makefile", "has the pattern Makefile, which matches no file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			changed := slices.DeleteFunc(slices.Clone(patterns), func(pattern string) bool { return pattern == c.remove })
			if c.add != "" {
				changed = append(changed, c.add)
			}
			problems := sideFilterProblems("allocator", changed, needed["allocator"])
			if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, c.want) }) {
				t.Errorf("the problems found were %q, none of them %q", problems, c.want)
			}
		})
	}
}

// paths-filter's patterns are read as picomatch reads them: ** across
// directories, none among them, * and ? within one name.
func TestTheFilterPatternsAreMatchedAsPathsFilterMatchesThem(t *testing.T) {
	for _, c := range []struct {
		pattern, file string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "internal/config/config.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/config/config.go", true},
		{"internal/config/**", "internal/config/config.go", true},
		{"internal/config/**", "internal/config/testdata/a/b.yaml", true},
		{"internal/config/**", "internal/configs/x.go", false},
		{"internal/allocator/**", "internal/allocapi/api.go", false},
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{"go.mod", "go.mod", true},
		{"go.mod", "tools/go.mod", false},
		{"README.md", "README.md", true},
		{"*.md", "docs/DEVELOPMENT.md", false},
	} {
		if got := filterMatch(c.pattern, c.file); got != c.want {
			t.Errorf("the pattern %s matches %s: %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

// Every package of the module is tested by exactly one side of CI, or is
// test/repository, which the job test runs on every change of the go
// filter: a package on neither side would have its tests run by nobody, one
// on both twice. A side selecting nothing is an error of the script, not an
// empty list, which go test would read as the package in the directory it
// runs in.
func TestEveryPackageIsTestedByOneSideOfCI(t *testing.T) {
	out, err := exec.Command("go", "list", "./...").Output()
	if err != nil {
		t.Fatal(err)
	}
	module, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	repository := strings.TrimSpace(string(module)) + "/test/repository"
	sides := map[string][]string{}
	for _, side := range ciSides {
		for _, pkg := range ciSidePackages(t, side) {
			sides[pkg] = append(sides[pkg], side)
		}
	}
	packages := strings.Fields(string(out))
	for _, pkg := range packages {
		switch {
		case pkg == repository && len(sides[pkg]) == 0:
		case pkg == repository:
			t.Errorf("test/repository is on the %v side of CI, where the job test runs it on its own", sides[pkg])
		case len(sides[pkg]) != 1:
			t.Errorf("%s is tested by the sides %v of CI, where one was meant", pkg, sides[pkg])
		}
	}
	for pkg := range sides {
		if !slices.Contains(packages, pkg) {
			t.Errorf("tools/ci-packages.sh lists %s, which go list ./... does not", pkg)
		}
	}
	for _, want := range []string{"/cmd/pue-allocator", "/internal/allocator"} {
		if pkg := strings.TrimSpace(string(module)) + want; !slices.Equal(sides[pkg], []string{"allocator"}) {
			t.Errorf("%s is tested by the sides %v of CI, not the allocator's", pkg, sides[pkg])
		}
	}
	if out, err := exec.Command("sh", "tools/ci-packages.sh", "neither").CombinedOutput(); err == nil {
		t.Errorf("tools/ci-packages.sh accepted an unknown side and printed %q", out)
	}
}
