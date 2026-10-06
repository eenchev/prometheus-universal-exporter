package repository

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The decoder no longer pulls in the Prometheus client libraries; this keeps
// them from coming back unnoticed. The protobuf runtime they brought is back
// for the grpc request type alone, which the build tests below keep out of
// every build without it.
func TestGoModDoesNotNeedThePrometheusClientLibraries(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"github.com/prometheus/common", "github.com/prometheus/client_model", "github.com/munnerz/goautoneg"} {
		if strings.Contains(string(raw), module+" ") {
			t.Errorf("go.mod requires %s again; the prometheus decoder parses the text format itself (promparse.go)", module)
		}
	}
}

// usePythonPool swaps the shared Python worker pool for the length of a test,
// which is only safe while no test runs in parallel with another, in any
// package.
func TestNoTestRunsInParallel(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	call := "t." + "Parallel("
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), call) {
			t.Errorf("%s calls %s), but usePythonPool swaps the shared worker pool per test", file, call)
		}
	}
}

// Only the tests give a Python script more time than its
// limits.script_timeout, and an interpreter another time to start in than
// the exporter's ten seconds: the exporter's own pool has no least time, so
// the limit a collector configures is the one its scripts run under, and its
// start timeout is the constant.
func TestOnlyTestsGiveScriptsALeastTime(t *testing.T) {
	for _, setter := range []struct{ name, gives string }{
		{"SetLeastScriptTimeout", "gives every script of the pool more time than its limits.script_timeout"},
		{"SetStartTimeout", "gives every interpreter of the pool another time to start in than the exporter's"},
	} {
		call := "." + setter.name + "("
		calls := 0
		err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if n := strings.Count(string(raw), call); n > 0 {
				calls += n
				if !strings.HasSuffix(path, "_test.go") {
					t.Errorf("%s calls %s), which %s; only the tests may", path, call, setter.gives)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls == 0 {
			t.Fatalf("no file calls %s): the test looks for a name that is gone", call)
		}
	}
}

// Only the tests hold the exporter in its --web.shutdown-delay: the wait is a
// variable of the main package (waitOutShutdownDelay), which a test of the
// shutdown replaces in the child process it runs the exporter in, so that the
// delay ends when the test has seen what it is about. The exporter itself
// declares it, as time.Sleep, and calls it, and nothing but a test sets it.
func TestOnlyTestsHoldTheShutdownDelay(t *testing.T) {
	const name = "waitOutShutdownDelay"
	declared, called, set := 0, 0, 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case !strings.Contains(line, name), strings.HasPrefix(line, "//"):
			case strings.HasSuffix(path, "_test.go"):
				if strings.HasPrefix(line, name+" = ") {
					set++
				}
			case line == "var "+name+" = time.Sleep":
				declared++
			case strings.HasPrefix(line, name+"("):
				called++
			default:
				t.Errorf("%s: %q: only a test may give the wait for --web.shutdown-delay another meaning than time.Sleep", path, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if declared != 1 || called != 1 || set == 0 {
		t.Fatalf("%s is declared %d times, called %d times and set by the tests %d times; want one declaration, one call and a test that sets it: the test looks for a name that is gone", name, declared, called, set)
	}
}

// internalLayers is the order of the internal packages: each may import only
// the ones before it (docs/SPECIFICATION-EXPORTER.md, section 7.1).
var internalLayers = []string{"model", "expr", "fetch", "decode", "transform", "config", "exporter"}

// testOnly are the internal packages only tests import: testutil, with the
// package within it, alloctest, that measures allocations, and grpctest, the
// grpc request type's test server.
var testOnly = map[string]bool{"testutil": true, "grpctest": true}

// The internal packages stay layered, and the test-only ones stay out of the
// binary.
func TestInternalPackagesAreLayered(t *testing.T) {
	const prefix = "github.com/eenchev/prometheus-universal-exporter/internal/"
	rank := map[string]int{}
	for i, name := range internalLayers {
		rank[name] = i
	}
	dirs, err := os.ReadDir("internal")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		name := dir.Name()
		own, layered := rank[name]
		if !layered && !testOnly[name] {
			t.Errorf("internal/%s is not in internalLayers; add it where it belongs", name)
			continue
		}
		files, err := filepath.Glob(filepath.Join("internal", name, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		// A package within one is held to what holds for it.
		within, err := filepath.Glob(filepath.Join("internal", name, "*", "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range append(files, within...) {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			test := strings.HasSuffix(file, "_test.go")
			for _, spec := range parsed.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				imported, internal := strings.CutPrefix(path, prefix)
				if !internal {
					continue
				}
				imported, _, _ = strings.Cut(imported, "/")
				if testOnly[imported] {
					if !test {
						t.Errorf("%s imports internal/%s, which only tests may", file, imported)
					}
					continue
				}
				if testOnly[name] {
					if imported != "model" {
						t.Errorf("%s imports internal/%s; %s may import only internal/model, so every package's tests can use it", file, imported, name)
					}
					continue
				}
				if rank[imported] >= own {
					t.Errorf("%s imports internal/%s, which comes after internal/%s in internalLayers", file, imported, name)
				}
			}
		}
	}
	for _, file := range []string{"main.go", "check.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			for name := range testOnly {
				if strings.HasSuffix(spec.Path.Value, `/internal/`+name+`"`) || strings.Contains(spec.Path.Value, `/internal/`+name+`/`) {
					t.Errorf("%s imports internal/%s, which only tests may", file, name)
				}
			}
		}
	}
}
