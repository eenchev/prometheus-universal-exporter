package repository

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// `make run` is how a shipped example is tried out. Running one by hand went
// wrong twice: an old ./prometheus-universal-exporter, left in the repository
// root by a `go build`, ran in place of the bin/ binary `make build` had just
// made, and later the bin/ binary was itself older than the tree. The target
// builds first, through `make build`, and is no part of `make ci`, which
// would never return from it.
func TestMakeRunBuildsThenRunsAndIsNoPartOfCI(t *testing.T) {
	prerequisites, recipes := makefileRules(t)
	recipe, ok := recipes["run"]
	if !ok {
		t.Fatal("the Makefile has no run target")
	}
	phony := false
	for _, match := range regexp.MustCompile(`(?m)^\.PHONY:(.*)$`).FindAllStringSubmatch(read(t, "Makefile"), -1) {
		phony = phony || slices.Contains(strings.Fields(match[1]), "run")
	}
	if !phony {
		t.Error("the run target is not .PHONY")
	}
	build := strings.Index(recipe, "$(MAKE) --no-print-directory build OPT_IN_REQUEST_TYPES=")
	run := strings.Index(recipe, "sh tools/run-example.sh run '$(EXAMPLE)' '$(HOST)' '$(PORT)' $(ARGS)")
	if build < 0 || run < 0 || run < build {
		t.Errorf("make run does not run make build and then tools/run-example.sh run:\n%s", recipe)
	}
	reached, queue := map[string]bool{}, []string{"ci"}
	for len(queue) > 0 {
		target := queue[0]
		queue = queue[1:]
		if reached[target] {
			continue
		}
		reached[target] = true
		queue = append(queue, prerequisites[target]...)
	}
	if reached["run"] {
		t.Error("make ci reaches run, which runs the exporter until it is stopped")
	}
	makefile := read(t, "Makefile")
	for _, want := range []string{"\nEXAMPLE =\n", "\nHOST = 127.0.0.1\n", "\nPORT = 8080\n", "\nARGS =\n"} {
		if !strings.Contains(makefile, want) {
			t.Errorf("the Makefile does not set %q: set with =, only the command line changes it, and HOST is this machine alone", strings.TrimSpace(want))
		}
	}
}

// exampleName is the name `make run` knows an example configuration by:
// its directory's under examples/, or <name> of examples/config.<name>.*.yaml.
func exampleName(t *testing.T, config string) string {
	t.Helper()
	if dir := filepath.Dir(config); dir != "examples" {
		return filepath.Base(dir)
	}
	name, ok := strings.CutPrefix(filepath.Base(config), "config.")
	if !ok {
		t.Fatalf("%s: a configuration directly in examples/ is named config.<name>.<anything>.yaml, or make run cannot name it", config)
	}
	name, _, _ = strings.Cut(name, ".")
	return name
}

func runExample(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"tools/run-example.sh"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

// Every shipped example runs under its name, which comes from the tree, as
// the files the tree holds for it: its configuration and, beside it, its
// static target file. No two examples share a name, and a name no example
// has fails, naming the ones there are.
func TestEveryExampleRunsUnderItsName(t *testing.T) {
	tree := shippedExamples(t)
	names := map[string]string{}
	for _, config := range tree.configs {
		name := exampleName(t, config)
		if other, ok := names[name]; ok {
			t.Errorf("%s and %s are both named %s for make run", other, config, name)
		}
		names[name] = config
		want := []string{config}
		for _, targets := range tree.staticTargets {
			if filepath.Dir(targets) == filepath.Dir(config) && filepath.Dir(config) != "examples" {
				want = append(want, targets)
			}
		}
		out, err := runExample(t, "files", name)
		if err != nil {
			t.Errorf("tools/run-example.sh files %s: %v: %s", name, err, out)
			continue
		}
		if got := strings.Fields(out); !slices.Equal(got, want) {
			t.Errorf("tools/run-example.sh files %s gives %v, want %v", name, got, want)
		}
	}
	for _, want := range []string{"open-meteo", "metar", "tcp", "ecb"} {
		if _, ok := names[want]; !ok {
			t.Errorf("no example is named %s: %v", want, names)
		}
	}
	out, err := runExample(t, "files", "no-such-example")
	if err == nil || !strings.Contains(out, `no example "no-such-example"`) || !strings.Contains(out, "  open-meteo\n") {
		t.Errorf("an unknown example gave %v: %s", err, out)
	}
}

// `make run` alone lists the examples, every one the tree has, with a usage
// line, and fails; it builds nothing.
func TestMakeRunWithoutAnExampleListsThem(t *testing.T) {
	out, err := runMake(t, t.TempDir(), "run")
	if err == nil {
		t.Errorf("make run without EXAMPLE succeeded:\n%s", out)
	}
	if !strings.Contains(out, "usage: make run EXAMPLE=<name>") {
		t.Errorf("make run without EXAMPLE gives no usage line:\n%s", out)
	}
	if strings.Contains(out, "go build") {
		t.Errorf("make run without EXAMPLE built the exporter:\n%s", out)
	}
	for _, config := range shippedExamples(t).configs {
		if want := "  " + exampleName(t, config) + " "; !strings.Contains(out, want) || !strings.Contains(out, config) {
			t.Errorf("make run without EXAMPLE does not list %s as %q:\n%s", config, strings.TrimSpace(want), out)
		}
	}
}

// An example of an opt-in request type runs without the user knowing the
// type: the types its collectors name are added to OPT_IN_REQUEST_TYPES,
// beside any the command line gives, for every example; examples/tcp is
// built with tcp, and an unknown example fails before anything is built.
func TestMakeRunAddsTheOptInTypesAnExampleNeeds(t *testing.T) {
	optIn := optInRequestTypes(t)
	for _, config := range shippedExamples(t).configs {
		var document struct {
			Collectors []struct {
				Request struct {
					Type string `yaml:"type"`
				} `yaml:"request"`
			} `yaml:"collectors"`
		}
		if err := yaml.Unmarshal([]byte(read(t, config)), &document); err != nil {
			t.Fatalf("%s: %v", config, err)
		}
		var want []string
		for _, collector := range document.Collectors {
			if name := collector.Request.Type; slices.Contains(optIn, name) && !slices.Contains(want, name) {
				want = append(want, name)
			}
		}
		slices.Sort(want)
		name := exampleName(t, config)
		out, err := runExample(t, "opt-in-types", name)
		if err != nil {
			t.Fatalf("tools/run-example.sh opt-in-types %s: %v: %s", name, err, out)
		}
		got := strings.Split(strings.TrimSpace(out), ",")
		if strings.TrimSpace(out) == "" {
			got = nil
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: the opt-in types added are %v, want %v", name, got, want)
		}
	}
	if out, err := runExample(t, "opt-in-types", "tcp", "redis,tcp"); err != nil || strings.TrimSpace(out) != "redis,tcp" {
		t.Errorf("tcp with OPT_IN_REQUEST_TYPES=redis,tcp gives %q (%v), want redis,tcp", out, err)
	}
	out, err := runMake(t, t.TempDir(), "-n", "run", "EXAMPLE=tcp")
	if err != nil || !strings.Contains(out, "sh tools/request-type-tags.sh '' '' 'tcp'") {
		t.Errorf("make -n run EXAMPLE=tcp does not build with tcp (%v):\n%s", err, out)
	}
	out, err = runMake(t, t.TempDir(), "-n", "run", "EXAMPLE=no-such-example")
	if err == nil || strings.Contains(out, "go build") {
		t.Errorf("make -n run EXAMPLE=no-such-example built or succeeded (%v):\n%s", err, out)
	}
}

// The run itself: the URLs to open first, the landing page and, with static
// targets, their status page, then bin/'s binary with the example's files,
// the listen address and the flags of ARGS. It runs in a copy of the tree's
// shape whose bin/ binary is a script that prints what it was given.
func TestRunExampleRunsTheBinOfMakeBuild(t *testing.T) {
	script, err := filepath.Abs("tools/run-example.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, file := range []string{"examples/open-meteo/config.yaml", "examples/open-meteo/static-targets.yaml", "examples/config.ecb.xml-test.yaml"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(read(t, file)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "prometheus-universal-exporter"), []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"arg: $a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"open-meteo": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"Open http://127.0.0.1:9115/static-targets/status for the static targets' last scrapes.",
			"arg: --config.file=examples/open-meteo/config.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --static-targets-file=examples/open-meteo/static-targets.yaml",
			"arg: --log.level=debug",
		},
		"ecb": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"arg: --config.file=examples/config.ecb.xml-test.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --log.level=debug",
		},
	} {
		cmd := exec.Command("sh", script, "run", name, "127.0.0.1", "9115", "--log.level=debug")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		var lines []string
		for line := range strings.Lines(string(out)) {
			if strings.HasPrefix(line, "Open ") || strings.HasPrefix(line, "arg: ") {
				lines = append(lines, strings.TrimSuffix(line, "\n"))
			}
		}
		if len(lines) != len(want) {
			t.Errorf("%s gives %q, want %q", name, lines, want)
			continue
		}
		for i := range want {
			if !strings.HasPrefix(lines[i], want[i]) {
				t.Errorf("%s gives %q, want %q", name, lines, want)
				break
			}
		}
	}
}

// A binary that `go build` left in the repository root is ignored by git and
// soon older than bin/'s, so `make build` warns while one is there, naming
// bin/'s as the one to run.
func TestMakeBuildWarnsOfABinaryInTheRoot(t *testing.T) {
	_, recipes := makefileRules(t)
	if !strings.Contains(recipes["build"], "sh tools/run-example.sh check-root-binaries $(APP) $(ALLOCATOR)") {
		t.Errorf("make build does not check the repository root for old binaries:\n%s", recipes["build"])
	}
	gitignore := strings.Split(read(t, ".gitignore"), "\n")
	for _, want := range []string{"/prometheus-universal-exporter", "/pue-allocator"} {
		if !slices.Contains(gitignore, want) {
			t.Errorf(".gitignore does not ignore %s", want)
		}
	}
	script, err := filepath.Abs("tools/run-example.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prometheus-universal-exporter"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "check-root-binaries", "prometheus-universal-exporter", "pue-allocator")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "warning: ./prometheus-universal-exporter is an old binary") || !strings.Contains(string(out), "run that one") {
		t.Errorf("no warning of ./prometheus-universal-exporter: %s", out)
	}
	if strings.Contains(string(out), "pue-allocator") {
		t.Errorf("a warning of ./pue-allocator, which is not there: %s", out)
	}
}

// The README's first steps and the development page give make run, and the
// README says the binary is in bin/.
func TestDocsGiveMakeRun(t *testing.T) {
	readme := read(t, "README.md")
	for _, want := range []string{"## Try it", "make run EXAMPLE=open-meteo PORT=9115", "http://127.0.0.1:9115/static-targets/status", "`bin/prometheus-universal-exporter`"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not give %q", want)
		}
	}
	if !strings.Contains(read(t, "docs/DEVELOPMENT.md"), "make run EXAMPLE=open-meteo") {
		t.Error("docs/DEVELOPMENT.md does not list make run")
	}
}
