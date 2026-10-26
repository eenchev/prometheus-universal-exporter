//go:build !select_request_types

package repository

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A new request type is `make new-request-type NAME=<name> [OPT_IN=1]`
// (tools/new-request-type.sh) and then its logic: the script writes the
// type's files and adds it to every list that names the types, which the
// wiring tests compare across the tree. This runs it on a copy of the tree,
// for an opt-in type and a type of every build, and holds the copy to what
// the tree is held to: it builds and vets in the default build, with every
// opt-in type and with each new type alone, the generated tests pass, and
// the tests that keep the lists of types in step pass in it.
//
// It builds the copy's packages, a few minutes on a cold build cache, so
// it runs in the default build alone, not under -short or the race
// detector, which would only run the same commands again. The copy is
// built with -trimpath, so the packages it shares with the tree, and the
// vendored modules, come from the build cache.

// newRequestTypeCopy marks the environment of the commands run in the copy,
// so the copy's own tests never run this one again.
const newRequestTypeCopy = "NEW_REQUEST_TYPE_COPY"

// copyWiringTests are the tests of the copy that compare the lists of the
// request types across files, by package.
var copyWiringTests = map[string][]string{
	"./internal/fetch/": {
		"TestEveryRequestTypeFileHasItsSelectionConstraint",
		"TestTheGuardNamesEveryRequestType",
		"TestBuildTagsSelectTheRequestTypeFiles",
		"TestRequestTypeTestFilesCarryTheirTypesConstraint",
		"TestADefaultBuildRegistersEveryRequestType",
		"TestTheRefusalOfAnUnusedParameterNamesThePlacesOfTheRequestType",
	},
	"./internal/config/": {
		"TestTheRequestKeyCatalogueIsCurrent",
	},
	"./test/repository/": {
		"TestMarkedListsNameEveryRequestType",
		"TestEveryRequestTypeHasItsPage",
		"TestCIBuildsEveryOptInRequestType",
		"TestCIBuildsEveryRequestTypeOnItsOwn",
		"TestCIAndMakeTestEveryRequestTypeOnItsOwn",
		"TestMakeCIRunsTheGoCommandsOfTheCIWorkflow",
		"TestTheCommittedSchemaComparisonsRunInTheDefaultBuild",
		"TestCommittedConfigSchemaIsCurrent",
		"TestCommittedCollectorFileSchemaIsCurrent",
		"TestTheCommonPartHoldsNoKeyOfOneType",
		"TestEveryConstrainedKeyOfTheSchemasIsInATable",
		"TestDocumentationLinksResolve",
		"TestEveryDocumentationPageIsIndexed",
		"TestEveryRequestTypePageNamesTheKeysAStaticTargetMaySet",
	},
}

func TestANewRequestTypeIsWiredByTheScaffold(t *testing.T) {
	switch {
	case os.Getenv(newRequestTypeCopy) != "":
		t.Skip("this is the copy the scaffold test made")
	case testing.Short():
		t.Skip("builds a copy of the tree; not under -short")
	case alloctest.RaceDetector:
		t.Skip("builds a copy of the tree; the run without the race detector does")
	}
	for _, name := range optInRequestTypes(t) {
		if fetch.RequestTypes[name] != nil {
			t.Skipf("runs in the default build; this one has the opt-in %s", name)
		}
	}
	for _, tool := range []string{"git", "go", "gofmt", "make", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	dir := copyOfTheTree(t)
	env := append(os.Environ(), newRequestTypeCopy+"=1", "GOFLAGS="+strings.TrimSpace(os.Getenv("GOFLAGS")+" -trimpath"))
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...) // #nosec G204 -- the test's own commands
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	refused := func(want string, args ...string) {
		t.Helper()
		cmd := exec.Command("sh", append([]string{"tools/new-request-type.sh"}, args...)...) // #nosec G204 -- the test's own commands
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), want) {
			t.Errorf("new-request-type.sh %v: %v, want it refused with %q\n%s", args, err, want, out)
		}
	}

	// A name that is no new type's is refused before anything is written.
	refused(`the request type "http" is in the tree already`, "http")
	refused(`lowercase letters and digits only`, "Zz-demo")
	refused(`it starts with a letter`, "9zz")
	refused(`usage:`)
	refused(`the Go name "ZZOther" is not "zzdemo" with some of its letters in capitals`, "zzdemo", "opt-in", "ZZOther")
	refused(`the Go name "zZDemo" does not start with a capital letter`, "zzdemo", "opt-in", "zZDemo")
	if out := run("git", "status", "--porcelain"); out != "" {
		t.Fatalf("a refused name changed the copy:\n%s", out)
	}

	// The opt-in type is given its Go name, an initialism's capitals in it,
	// and the other is spelt as its name with a capital first.
	for _, c := range []struct{ name, optIn, goName string }{{"zzdemo", "OPT_IN=1", "GO_NAME=ZZDemo"}, {"zzdefault", "", ""}} {
		args := []string{"--no-print-directory", "new-request-type", "NAME=" + c.name}
		for _, arg := range []string{c.optIn, c.goName} {
			if arg != "" {
				args = append(args, arg)
			}
		}
		if out := run("make", args...); !strings.Contains(out, "TODO("+c.name+")") {
			t.Errorf("make new-request-type NAME=%s printed no checklist of what is left:\n%s", c.name, out)
		}
	}
	for _, file := range []string{
		"internal/fetch/requesttype_zzdemo.go", "internal/fetch/requesttype_zzdemo_test.go", "internal/exporter/requesttype_zzdemo_test.go",
		"test/repository/schemakeys_zzdemorows_test.go", "test/repository/schemakeys_zzdemo_test.go", "docs/ZZDEMO.md",
		"internal/fetch/requesttype_zzdefault.go", "internal/fetch/requesttype_zzdefault_test.go", "internal/exporter/requesttype_zzdefault_test.go",
		"test/repository/schemakeys_zzdefault_test.go", "docs/ZZDEFAULT.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Errorf("the scaffold wrote no %s", file)
		}
	}
	if !strings.Contains(read(t, filepath.Join(dir, "docs/ZZDEMO.md")), "\n## Building with zzdemo\n") || strings.Contains(read(t, filepath.Join(dir, "docs/ZZDEFAULT.md")), "\n## Building with") {
		t.Error("only the opt-in type's page says how to build with it")
	}
	if registry := read(t, filepath.Join(dir, "internal/fetch/requesttype.go")); !strings.Contains(registry, `RequestTypeZZDemo `) || !strings.Contains(registry, `RequestTypeZzdefault `) {
		t.Error("the types' constants are not spelt by their Go names, ZZDemo and Zzdefault")
	}
	if pages := read(t, filepath.Join(dir, "test/repository/docfacts_test.go")); !strings.Contains(pages, `{"docs/ZZDEMO.md", "internal/fetch/requesttype_zzdemo.go"},`) || !strings.Contains(pages, `{"docs/ZZDEFAULT.md", "internal/fetch/requesttype_zzdefault.go"},`) {
		t.Error("the new types' pages are not among those whose Static targets section is read")
	}

	tags := func(args ...string) string {
		return strings.TrimSpace(run("sh", append([]string{"tools/request-type-tags.sh"}, args...)...))
	}
	optIn := tags("", "", strings.Join(append(optInRequestTypes(t), "zzdemo"), ","))
	run("go", "build", "./...")
	run("go", "vet", "./...")
	run("go", "vet", "-tags", optIn, "./...")
	for _, name := range []string{"zzdemo", "zzdefault"} {
		alone := tags(name)
		run("go", "build", "-tags", alone, "./...")
		run("go", "vet", "-tags", alone, "./...")
	}

	// The generated tests pass, in the default build, with every opt-in
	// type and with their type alone.
	generated := func(build string, names ...string) {
		t.Helper()
		var tests []string
		for _, name := range names {
			title := map[string]string{"zzdemo": "ZZDemo", "zzdefault": "Zzdefault"}[name]
			tests = append(tests, "TestA"+title+"CollectorIsNotImplementedYet", "TestAProbeOfA"+title+"CollectorFailsUntilTheTypeIsWritten", "TestSchemaAndExporterAgreeOn"+title+"KeysWrittenEmpty")
			if name == "zzdemo" {
				tests = append(tests, "TestTheZZDemoTypesOptInKeysAreListed")
			}
		}
		out := run("go", "test", "-count=1", "-v", "-tags", build, "-run", "^("+strings.Join(tests, "|")+")$", "./internal/fetch/", "./internal/exporter/", "./test/repository/")
		for _, test := range tests {
			if !strings.Contains(out, "--- PASS: "+test+" ") && !strings.Contains(out, "--- SKIP: "+test+" ") {
				t.Errorf("the generated %s did not pass with -tags %q:\n%s", test, build, out)
			}
		}
	}
	generated("", "zzdefault")
	generated(optIn, "zzdemo", "zzdefault")
	generated(tags("zzdemo"), "zzdemo")
	generated(tags("zzdefault"), "zzdefault")
	for pkg, tests := range copyWiringTests {
		out := run("go", "test", "-count=1", "-v", "-run", "^("+strings.Join(tests, "|")+")$", pkg)
		for _, test := range tests {
			if !strings.Contains(out, "--- PASS: "+test+" ") {
				t.Errorf("%s did not pass in the copy, in %s:\n%s", test, pkg, out)
			}
		}
	}
}

// copyOfTheTree copies the files git knows of, committed or not, but those
// it ignores, to a temporary directory, each with its mode, the vendored
// modules linked rather than copied; the copy is a repository of its own,
// its files committed, so a change to it shows.
func copyOfTheTree(t *testing.T) string {
	t.Helper()
	listing, err := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Skipf("git ls-files: %v; not a git checkout", err)
	}
	dir := t.TempDir()
	for file := range strings.SplitSeq(strings.TrimRight(string(listing), "\x00"), "\x00") {
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			continue // deleted from the working tree, or a link such as vendor
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	if vendor, err := filepath.EvalSymlinks("vendor"); err == nil {
		abs, err := filepath.Abs(vendor)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(abs, filepath.Join(dir, "vendor")); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) // #nosec G204 -- the test's own commands
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("/vendor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=scaffold", "-c", "user.email=scaffold@example.invalid", "commit", "-q", "--no-verify", "-m", "copy"},
	} {
		git(args...)
	}
	return dir
}
