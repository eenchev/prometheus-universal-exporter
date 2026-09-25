package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A workflow that installs an older Go than go.mod requires fails on the first
// `go` command with "go.mod requires go >= X", and nothing in the repository
// points at the cause: the go directive is raised by a dependency bump, in a
// pull request that never touches a workflow. This test ties the two together,
// so the mismatch is caught by `go test ./...` rather than by a red build after
// a dependency update lands.
//
// The workflows ask for `stable`, which setup-go resolves to the newest stable
// release, so the build follows Go's releases without anyone editing a pin.
// An explicit version is still allowed, provided it satisfies go.mod.

var (
	goDirective   = regexp.MustCompile(`(?m)^go[ \t]+([0-9]+(?:\.[0-9]+){0,2})[ \t]*$`)
	workflowGoVer = regexp.MustCompile(`(?m)^[ \t]*go-version:[ \t]*['"]?([^'"\s]+)['"]?[ \t]*$`)
)

// parseGoVersion reads a dotted Go version. The trailing `.x` setup-go accepts
// is dropped rather than rejected: `1.25.x` constrains the same minor as 1.25.
func parseGoVersion(raw string) ([]int, bool) {
	raw = strings.TrimSuffix(raw, ".x")
	if raw == "" {
		return nil, false
	}
	var parsed []int
	for _, part := range strings.Split(raw, ".") {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		parsed = append(parsed, number)
	}
	return parsed, true
}

// atLeast reports whether have satisfies want, padding the shorter with zeros
// so 1.25 counts as satisfying 1.25.0.
func atLeast(have, want []int) bool {
	for i := 0; i < len(have) || i < len(want); i++ {
		x, y := 0, 0
		if i < len(have) {
			x = have[i]
		}
		if i < len(want) {
			y = want[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}

func moduleGoVersion(t *testing.T) ([]int, string) {
	t.Helper()
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	match := goDirective.FindSubmatch(raw)
	if match == nil {
		t.Fatal("go.mod has no go directive")
	}
	required, ok := parseGoVersion(string(match[1]))
	if !ok {
		t.Fatalf("go.mod declares an unparseable go directive %q", match[1])
	}
	return required, string(match[1])
}

func TestWorkflowsInstallAGoThatSatisfiesTheModule(t *testing.T) {
	required, declared := moduleGoVersion(t)
	found := 0
	for _, path := range workflowPaths(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range workflowGoVer.FindAllStringSubmatch(string(raw), -1) {
			found++
			requested := match[1]
			// setup-go resolves this to the newest stable release, which
			// always satisfies the module.
			if requested == "stable" {
				continue
			}
			// The release reads the Dockerfile's GO_VERSION, which
			// TestDockerfileGoVersionSatisfiesTheModule checks, and
			// TestReleaseBinariesBuildWithTheImagesGo checks the wiring.
			if requested == "${{" && filepath.Base(path) == "release.yml" {
				continue
			}
			version, ok := parseGoVersion(requested)
			if !ok {
				t.Errorf("%s: go-version %q is neither `stable` nor a version number", filepath.Base(path), requested)
				continue
			}
			if !atLeast(version, required) {
				t.Errorf("%s: go-version %q is older than the go %s that go.mod requires; the build fails before it starts",
					filepath.Base(path), requested, declared)
			}
		}
	}
	if found == 0 {
		t.Fatal("no workflow requests a Go version; this test must not silently pass if the key is renamed")
	}
}

// The comparison is the part worth testing directly: the repository check above
// only exercises whichever versions happen to be committed today.
func TestGoVersionComparison(t *testing.T) {
	tests := []struct {
		have, want string
		satisfied  bool
	}{
		{"1.25", "1.25.0", true},
		{"1.25.x", "1.25.0", true},
		{"1.27", "1.25.0", true},
		{"1.25.3", "1.25.0", true},
		{"1.23", "1.25.0", false},
		{"1.23.x", "1.25.0", false},
		{"1.24.9", "1.25.0", false},
		{"2.0", "1.25.0", true},
		{"1.25.0", "1.25", true},
	}
	for _, test := range tests {
		t.Run(test.have+"_vs_"+test.want, func(t *testing.T) {
			have, ok := parseGoVersion(test.have)
			if !ok {
				t.Fatalf("cannot parse %q", test.have)
			}
			want, ok := parseGoVersion(test.want)
			if !ok {
				t.Fatalf("cannot parse %q", test.want)
			}
			if got := atLeast(have, want); got != test.satisfied {
				t.Fatalf("atLeast(%s, %s)=%v, want %v", test.have, test.want, got, test.satisfied)
			}
		})
	}
	for _, bad := range []string{"stable", "oldstable", "", "1.x.3", "tip"} {
		if _, ok := parseGoVersion(bad); ok {
			t.Errorf("%q should not parse as a version number", bad)
		}
	}
}

// The Dockerfile builds the released image, so it must satisfy the module too.
// It pins a two-component version deliberately: that tag already picks up patch
// rebuilds, and tools/depupdate keeps it moving within the major.
func TestDockerfileGoVersionSatisfiesTheModule(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	match := regexp.MustCompile(`(?m)^ARG[ \t]+GO_VERSION=([^\s#]+)`).FindSubmatch(raw)
	if match == nil {
		t.Fatal("the Dockerfile no longer pins GO_VERSION")
	}
	pinned, ok := parseGoVersion(string(match[1]))
	if !ok {
		t.Fatalf("GO_VERSION=%q is not a version number", match[1])
	}
	required, declared := moduleGoVersion(t)
	if !atLeast(pinned, required) {
		t.Fatalf("the image builds with Go %s but go.mod requires go %s", match[1], declared)
	}
}

// go-version-file installs exactly the version go.mod names, and go.mod's go
// directive is the minimum the code needs, not a release anyone should ship:
// 1.25.0 lacks every standard-library security fix since. So no workflow may
// build from it, and the release builds its binaries with the Go the image is
// built with, read from the Dockerfile, at its newest patch release.
func TestReleaseBinariesBuildWithTheImagesGo(t *testing.T) {
	for _, path := range workflowPaths(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "go-version-file:") {
			t.Errorf("%s installs Go from go-version-file, which pins go.mod's minimum rather than a current release", filepath.Base(path))
		}
	}
	raw, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	release := string(raw)
	for _, want := range []string{
		"sed -n 's/^ARG GO_VERSION=//p' Dockerfile",
		"go-version: ${{ steps.go.outputs.version }}",
		"check-latest: true",
	} {
		if !strings.Contains(release, want) {
			t.Errorf("release.yml no longer contains %q", want)
		}
	}
}

// The Python worker's sandbox depends on what the standard library imports,
// which changes between releases: from 3.12 zoneinfo needs threading. Every
// workflow that runs the test suite — CI, the exporter release, and the
// Dockerfile update that tests the new pins before proposing them — tests the
// Python the image ships, read from the Dockerfile, with its pinned libraries,
// so a test that passes there passes in the image. One running `make ci`
// installs gopls first, since `make ci` runs gopls check and fails without it.
func TestWorkflowsRunTheSuiteWithTheImagesPythonAndTheirTools(t *testing.T) {
	runsSuite := regexp.MustCompile(`(?m)^\s*(go test |make ci\b)`)
	checked := map[string]bool{}
	for _, path := range workflowPaths(t) {
		name := filepath.Base(path)
		for job, steps := range workflowSteps(t, path) {
			for index, step := range steps {
				run, _ := step["run"].(string)
				if !runsSuite.MatchString(run) {
					continue
				}
				checked[name] = true
				before := steps[:index]
				for what, found := range map[string]bool{
					"reads PYTHON_VERSION from the Dockerfile in a step with id python": hasStep(before, func(s map[string]any) bool {
						r, _ := s["run"].(string)
						return s["id"] == "python" && strings.Contains(r, "sed -n 's/^ARG PYTHON_VERSION=//p' Dockerfile")
					}),
					"sets that Python up": hasStep(before, func(s map[string]any) bool {
						uses, _ := s["uses"].(string)
						with, _ := s["with"].(map[string]any)
						return strings.HasPrefix(uses, "actions/setup-python@") && with["python-version"] == "${{ steps.python.outputs.version }}"
					}),
					// A setup-python Python has no PyYAML, which
					// check-manifests.py needs, nor the libraries the Python
					// tests use.
					"installs the Dockerfile's lxml, PyYAML and python-dateutil": hasStep(before, func(s map[string]any) bool {
						r, _ := s["run"].(string)
						return strings.Contains(r, `"lxml==$(arg LXML_VERSION)"`) && strings.Contains(r, `"PyYAML==$(arg PYYAML_VERSION)"`) && strings.Contains(r, `"python-dateutil==$(arg PYTHON_DATEUTIL_VERSION)"`)
					}),
				} {
					if !found {
						t.Errorf("%s job %q runs the suite (%s) but no step before it %s", name, job, strings.TrimSpace(run), what)
					}
				}
				if strings.Contains(run, "make ci") && !hasStep(before, func(s map[string]any) bool {
					r, _ := s["run"].(string)
					return strings.Contains(r, "gopls-install") && strings.Contains(r, "lint-install")
				}) {
					t.Errorf("%s job %q runs make ci, which runs golangci-lint and gopls check, without installing both first (make lint-install gopls-install)", name, job)
				}
			}
		}
	}
	for _, name := range []string{"ci.yml", "release.yml", "update-docker-deps.yml"} {
		if !checked[name] {
			t.Errorf("%s no longer runs the test suite", name)
		}
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^ARG PYTHON_VERSION=3\.[0-9]+$`).Match(dockerfile) {
		t.Error("the Dockerfile no longer pins PYTHON_VERSION as 3.MINOR, which the workflows read")
	}
}

// workflowSteps returns each job's steps of the workflow at path, in order.
func workflowSteps(t *testing.T, path string) map[string][]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Jobs map[string]struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	steps := map[string][]map[string]any{}
	for name, job := range document.Jobs {
		steps[name] = job.Steps
	}
	return steps
}

func hasStep(steps []map[string]any, match func(map[string]any) bool) bool {
	return slices.ContainsFunc(steps, match)
}

// The linter version is pinned twice: in the Makefile for `make lint` and in
// the CI workflow for the action. They have to agree, or a clean local run
// stops meaning a clean CI run — which is the whole promise `make lint` makes.
func TestTheLinterVersionIsPinnedConsistently(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION[ \t]*:?=[ \t]*(\S+)`).FindSubmatch(makefile)
	if match == nil {
		t.Fatal("the Makefile no longer pins GOLANGCI_LINT_VERSION")
	}
	pinned := string(match[1])

	workflow, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	// The version input of the golangci-lint action, whose step is the only
	// place CI names a linter version.
	action := regexp.MustCompile(`golangci/golangci-lint-action@[^\s]+\s+with:\s+version:[ \t]*(\S+)`).FindSubmatch(workflow)
	if action == nil {
		t.Fatal("ci.yml no longer pins a golangci-lint version")
	}
	if got := string(action[1]); got != pinned {
		t.Fatalf("ci.yml lints with %s but the Makefile installs %s; a clean `make lint` would not mean a clean CI run", got, pinned)
	}
}

// Pinning the version is only half of it: `make lint` has to refuse a
// different installed release, since another release enables different checks
// and passes locally what CI fails. And the pre-commit hook has to run it, so
// such a commit is refused before it is made.
func TestLocalLintRunsOnlyWithThePinnedVersion(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(makefile)
	for _, want := range []string{
		"\nlint: lint-version\n",
		"golangci-lint version --short",
		"want='$(GOLANGCI_LINT_VERSION)'",
		"\nprecommit: fmt-check lint gopls-check vet\n",
		"\ngopls-check: gopls-version\n",
		"go install golang.org/x/tools/gopls@$(GOPLS_VERSION)",
		"git config core.hooksPath .githooks",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Makefile no longer contains %q", want)
		}
	}

	info, err := os.Stat(".githooks/pre-commit")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error(".githooks/pre-commit is not executable, so git would skip it")
	}
	hook, err := os.ReadFile(".githooks/pre-commit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(hook), "#!/bin/sh\n") || !strings.Contains(string(hook), "make --no-print-directory precommit") {
		t.Errorf(".githooks/pre-commit no longer runs make precommit:\n%s", hook)
	}
}

// What an editor shows comes from gopls, and some of its analyzers exist
// nowhere else, so CI runs gopls check at the Makefile's pinned version.
func TestCIRunsGoplsCheck(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^GOPLS_VERSION[ \t]*:?=[ \t]*v[0-9]+\.[0-9]+\.[0-9]+$`).Match(makefile) {
		t.Error("the Makefile no longer pins GOPLS_VERSION")
	}
	workflow, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "run: make gopls-install gopls-check") {
		t.Error("ci.yml no longer runs gopls check")
	}
}

// helm is pinned in the Makefile for `make helm-test`, and every workflow that
// installs it installs that version: releases word errors and render
// details differently, so a chart check that passes with one can fail with
// another, as helm 3.16 and 3.22 did on the schema's messages.
func TestTheHelmVersionIsPinnedConsistently(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^HELM_VERSION[ \t]*:?=[ \t]*(v[0-9]+\.[0-9]+\.[0-9]+)$`).FindSubmatch(makefile)
	if match == nil {
		t.Fatal("the Makefile no longer pins HELM_VERSION")
	}
	pinned := string(match[1])
	for _, want := range []string{"\nhelm-test: helm-version\n", "go install helm.sh/helm/v4/cmd/helm@$(HELM_VERSION)"} {
		if !strings.Contains(string(makefile), want) {
			t.Errorf("the Makefile no longer contains %q", want)
		}
	}
	setup := regexp.MustCompile(`azure/setup-helm@[^\s]+(?:\s+with:\s+version:[ \t]*(\S+))?`)
	found := 0
	for _, path := range workflowPaths(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range setup.FindAllSubmatch(raw, -1) {
			found++
			if got := string(m[1]); got != pinned {
				t.Errorf("%s installs helm %q, but the Makefile pins %s", filepath.Base(path), got, pinned)
			}
		}
	}
	if found == 0 {
		t.Fatal("no workflow installs helm; this test must not pass by finding nothing")
	}
}

// govulncheck is pinned twice too: in the Makefile for `make vulncheck` and in
// its workflow. A local run should report what CI reports.
func TestTheVulnerabilityCheckerVersionIsPinnedConsistently(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^GOVULNCHECK_VERSION[ \t]*:?=[ \t]*(\S+)`).FindSubmatch(makefile)
	if match == nil {
		t.Fatal("the Makefile no longer pins GOVULNCHECK_VERSION")
	}
	pinned := string(match[1])

	workflow, err := os.ReadFile(".github/workflows/govulncheck.yml")
	if err != nil {
		t.Fatal(err)
	}
	installs := regexp.MustCompile(`golang\.org/x/vuln/cmd/govulncheck@(\S+)`).FindAllSubmatch(workflow, -1)
	if len(installs) == 0 {
		t.Fatal("govulncheck.yml no longer installs a pinned govulncheck")
	}
	for _, install := range installs {
		if got := string(install[1]); got != pinned {
			t.Fatalf("govulncheck.yml installs govulncheck %s but the Makefile runs %s", got, pinned)
		}
	}
}
