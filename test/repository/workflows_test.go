package repository

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// GitHub refuses to create a run for a workflow file it cannot parse, and a
// rejected file produces no jobs at all. No CI step can therefore catch the
// mistake in the commit that introduces it: the checker would live in the same
// file GitHub is refusing to read. This test is the local guard, so `go test
// ./...` fails before a broken workflow reaches GitHub.
//
// Decoding into a map makes gopkg.in/yaml.v3 report duplicate mapping keys,
// which a permissive parser accepts silently by keeping the last value. That is
// the failure this test exists for: an editing mistake that leaves a second
// `if:` on a step reads as valid YAML to a lenient parser and as a broken
// workflow to GitHub.

// stepKeys is the set of step fields used by this repository's workflows. An
// unknown key is usually a typo, which GitHub reports only at run time.
var stepKeys = map[string]bool{
	"name": true, "id": true, "if": true, "uses": true, "run": true,
	"with": true, "env": true, "shell": true, "working-directory": true,
	"continue-on-error": true, "timeout-minutes": true,
}

func workflowPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatal("no workflow files found; this test must not silently pass if they move")
	}
	return paths
}

func TestWorkflowFilesHaveNoDuplicateKeys(t *testing.T) {
	for _, path := range workflowPaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// A duplicate mapping key makes the whole file invalid to GitHub.
			var document map[string]any
			if err := yaml.Unmarshal(raw, &document); err != nil {
				t.Fatalf("%s is not a workflow GitHub can parse: %v", path, err)
			}
			if len(document) == 0 {
				t.Fatalf("%s decoded to an empty document", path)
			}
		})
	}
}

func TestWorkflowStepsAreWellFormed(t *testing.T) {
	for _, path := range workflowPaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				On   any `yaml:"on"`
				Jobs map[string]struct {
					Steps []map[string]any `yaml:"steps"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			if document.On == nil {
				t.Fatalf("%s has no trigger", path)
			}
			if len(document.Jobs) == 0 {
				t.Fatalf("%s defines no jobs", path)
			}
			for jobName, job := range document.Jobs {
				if len(job.Steps) == 0 {
					t.Errorf("job %q has no steps", jobName)
				}
				for index, step := range job.Steps {
					_, hasRun := step["run"]
					_, hasUses := step["uses"]
					label, _ := step["name"].(string)
					if label == "" {
						label = "step"
					}
					switch {
					case hasRun && hasUses:
						t.Errorf("%s job %q step %d (%s) sets both run and uses", path, jobName, index, label)
					case !hasRun && !hasUses:
						t.Errorf("%s job %q step %d (%s) sets neither run nor uses", path, jobName, index, label)
					}
					for key := range step {
						if !stepKeys[key] {
							t.Errorf("%s job %q step %d (%s) has unknown key %q", path, jobName, index, label, key)
						}
					}
				}
			}
		})
	}
}

// The release workflows are triggered on every tag under their namespace, not
// only well-formed ones, so that a malformed release tag fails loudly instead
// of matching no workflow and looking like it released. Narrowing either
// trigger back to the strict pattern would silently restore that hole, so the
// breadth is pinned here alongside the format each workflow accepts.

type releaseTagCase struct {
	workflow string
	prefix   string
	valid    []string
	invalid  []string
}

var releaseTagCases = []releaseTagCase{
	{
		workflow: ".github/workflows/release.yml",
		prefix:   "exporter",
		valid: []string{
			"exporter/prometheus-universal-exporter-v1.0.0",
			"exporter/prometheus-universal-exporter-v0.0.1",
			"exporter/prometheus-universal-exporter-v12.34.56",
		},
		invalid: []string{
			"exporter/prometheus-universal-exporter-1.0.0",      // missing the v
			"exporter/prometheus-universal-exporter-v1.0",       // not MAJOR.MINOR.PATCH
			"exporter/prometheus-universal-exporter-v1.0.0-rc1", // pre-release suffix
			"exporter/prometheus-universal-exporter-vX.Y.Z",
			"exporter/something-else-v1.0.0",
			"exporter",
			"exporterv1.0.0",
			"chart/prometheus-universal-exporter-1.0.0",
		},
	},
	{
		workflow: ".github/workflows/release-chart.yml",
		prefix:   "chart",
		valid: []string{
			"chart/prometheus-universal-exporter-0.2.0",
			"chart/prometheus-universal-exporter-1.2.3",
			"chart/prometheus-universal-exporter-10.0.0",
		},
		invalid: []string{
			"chart/prometheus-universal-exporter-v0.2.0", // the chart version carries no v
			"chart/prometheus-universal-exporter-0.2",
			"chart/prometheus-universal-exporter-0.2.0-rc1",
			"chart/something-else-1.2.3",
			"chart",
			"chart0.2.0",
			"exporter/prometheus-universal-exporter-v1.0.0",
		},
	},
}

func workflowDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// tagPatterns returns the push tag filters of a workflow.
func tagPatterns(t *testing.T, document map[string]any) []string {
	t.Helper()
	triggers, ok := document["on"].(map[string]any)
	if !ok {
		t.Fatalf("workflow trigger is %T, want a mapping", document["on"])
	}
	push, ok := triggers["push"].(map[string]any)
	if !ok {
		t.Fatalf("workflow has no push trigger: %v", triggers)
	}
	raw, ok := push["tags"].([]any)
	if !ok {
		t.Fatalf("push trigger has no tag filters: %v", push)
	}
	patterns := make([]string, 0, len(raw))
	for _, value := range raw {
		patterns = append(patterns, value.(string))
	}
	return patterns
}

func TestReleaseWorkflowsTriggerOnEveryNamespacedTag(t *testing.T) {
	for _, test := range releaseTagCases {
		t.Run(test.prefix, func(t *testing.T) {
			patterns := tagPatterns(t, workflowDocument(t, test.workflow))
			// "prefix/**" catches namespaced tags and "prefix*" catches the
			// unnamespaced ones, because a filter's * never matches a slash.
			for _, want := range []string{test.prefix + "/**", test.prefix + "*"} {
				if !slices.Contains(patterns, want) {
					t.Fatalf("%s must trigger on %q so a malformed tag still fails; patterns=%v", test.workflow, want, patterns)
				}
			}
		})
	}
}

// validationPattern extracts the expression the workflow validates its tag
// with, so the test checks the shipped rule rather than a copy of it.
func validationPattern(t *testing.T, path string) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`grep -Eq '([^']+)'`).FindSubmatch(raw)
	if matches == nil {
		t.Fatalf("%s has no tag validation expression", path)
	}
	return regexp.MustCompile(string(matches[1]))
}

func TestReleaseWorkflowsRejectMalformedNamespacedTags(t *testing.T) {
	for _, test := range releaseTagCases {
		t.Run(test.prefix, func(t *testing.T) {
			pattern := validationPattern(t, test.workflow)
			for _, tag := range test.valid {
				if !pattern.MatchString(tag) {
					t.Errorf("%s rejects valid tag %q", test.workflow, tag)
				}
			}
			for _, tag := range test.invalid {
				if pattern.MatchString(tag) {
					t.Errorf("%s accepts malformed tag %q", test.workflow, tag)
				}
			}
		})
	}
}

// The chart release refuses a tag that disagrees with Chart.yaml, so the
// committed version has to be releasable in the first place.
func TestChartVersionIsReleasable(t *testing.T) {
	raw, err := os.ReadFile("charts/prometheus-universal-exporter/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Name       string `yaml:"name"`
		Version    string `yaml:"version"`
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	semver := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	if !semver.MatchString(chart.Version) {
		t.Errorf("Chart.yaml version %q is not MAJOR.MINOR.PATCH", chart.Version)
	}
	if !semver.MatchString(chart.AppVersion) {
		t.Errorf("Chart.yaml appVersion %q is not MAJOR.MINOR.PATCH", chart.AppVersion)
	}
	tag := "chart/" + chart.Name + "-" + chart.Version
	if pattern := validationPattern(t, ".github/workflows/release-chart.yml"); !pattern.MatchString(tag) {
		t.Errorf("the tag %q implied by Chart.yaml would be rejected by the chart release workflow", tag)
	}
}

// The vulnerability check is for reference: a newly published advisory must
// not turn a pull request red. The step that runs govulncheck therefore reads
// its exit status instead of letting `bash -e` act on it, and nothing in the
// step exits non-zero on its own.
func TestTheVulnerabilityCheckNeverFails(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/govulncheck.yml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var check string
	for _, job := range document.Jobs {
		for _, step := range job.Steps {
			if regexp.MustCompile(`(?m)^\s*govulncheck `).MatchString(step.Run) {
				check = step.Run
			}
		}
	}
	if check == "" {
		t.Fatal("govulncheck.yml no longer has a step that runs govulncheck")
	}
	if !regexp.MustCompile(`(?m)^\s*set \+e\s*$`).MatchString(check) {
		t.Error("the govulncheck step does not turn off exit-on-error before running it, so a finding would fail the job")
	}
	if regexp.MustCompile(`\bexit\b`).MatchString(check) {
		t.Error("the govulncheck step calls exit, which could fail the job")
	}
	if !strings.Contains(check, "GITHUB_STEP_SUMMARY") {
		t.Error("the govulncheck step does not write its findings to the run summary, where they are meant to be read")
	}
}

// requestTypesInTheTree names every internal/fetch/requesttype_<name>.go,
// sorted.
func requestTypesInTheTree(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("internal/fetch/requesttype_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "requesttype_"), ".go")
		if name == "none" {
			continue
		}
		types = append(types, name)
	}
	sort.Strings(types)
	return types
}

// requestTypeLoop matches a shell loop over the request types, as ci.yml and
// the Makefile write it.
var requestTypeLoop = regexp.MustCompile(`(?m)\bfor type in ([a-z0-9 ]+); do\b`)

// CI vets and builds each request type on its own, in the job of the matrix
// over every type that tests each (TestCIAndMakeTestEveryRequestTypeOnItsOwn
// keeps the matrix whole), under the go filter, so no build of one type is
// left to a loop in one job, which took most of test's time; and
// `make build-request-types` runs the same commands in a loop that names
// every internal/fetch/requesttype_<name>.go.
func TestCIBuildsEveryRequestTypeOnItsOwn(t *testing.T) {
	const tags, vet, build = `tags="$(sh tools/request-type-tags.sh "$type")"`, `go vet -tags "$tags" ./...`, `go build -tags "$tags" ./...`
	ci := read(t, ".github/workflows/ci.yml")
	if requestTypeLoop.MatchString(ci) {
		t.Error("ci.yml builds the request types one after another in a loop, not each in its job of the request types' matrix")
	}
	var buildJob string
	for name, steps := range workflowSteps(t, ".github/workflows/ci.yml") {
		index := stepIndex(steps, tags, vet, build)
		if index < 0 {
			continue
		}
		buildJob = name
		step := steps[index]
		if env, _ := step["env"].(map[string]any); env["type"] != "${{ matrix.type }}" {
			t.Errorf("ci.yml's job %s vets and builds with type %v in its env, not ${{ matrix.type }}", name, env["type"])
		}
		if condition := step["if"]; condition != sideCondition("go") {
			t.Errorf("ci.yml vets and builds each request type under the condition %q, not %q", condition, sideCondition("go"))
		}
		if testing := stepIndex(steps, `go test -count=1 -tags "$tags" $packages`); testing < 0 {
			t.Errorf("ci.yml vets and builds each request type in the job %s, which does not test them; the request types' matrix was meant", name)
		}
	}
	if buildJob == "" {
		t.Fatalf("no job of ci.yml runs %s and %s after %s", vet, build, tags)
	}
	_, recipes := makefileRules(t)
	loop := requestTypeLoop.FindStringSubmatch(recipes["build-request-types"])
	if loop == nil {
		t.Fatal("make build-request-types has no loop over the request types")
	}
	listed := strings.Fields(loop[1])
	sort.Strings(listed)
	if types := requestTypesInTheTree(t); !slices.Equal(listed, types) {
		t.Errorf("make build-request-types builds the request types %v on their own, but the tree has %v", listed, types)
	}
	for _, want := range []string{tags, vet, build} {
		if !strings.Contains(recipes["build-request-types"], want) {
			t.Errorf("make build-request-types does not run %s, as ci.yml does", want)
		}
	}
}

// A build with one request type is one the exporter ships, so CI runs the
// exporter's tests with each, in a job of a matrix over every type in the
// tree that lets each type finish when another fails, the type handed to the
// step as $type, the packages those of the exporter's side a build with the
// type compiles, under the condition of the exporter's filter; and
// `make test-request-types`, which `make ci` includes, runs the same command
// over every package and the same types: a test that needs a type its build
// lacks fails there, not in somebody's single-type build.
func TestCIAndMakeTestEveryRequestTypeOnItsOwn(t *testing.T) {
	const tags, packages, command = `tags="$(sh tools/request-type-tags.sh "$type")"`, `packages="$(sh tools/ci-packages.sh exporter "$tags")"`, `go test -count=1 -tags "$tags" $packages`
	var job string
	for name, steps := range workflowSteps(t, ".github/workflows/ci.yml") {
		index := stepIndex(steps, tags, packages, command)
		if index < 0 {
			continue
		}
		if job != "" {
			t.Fatalf("ci.yml tests each request type in the jobs %s and %s, where one job of a matrix over the types was meant", job, name)
		}
		job = name
		step := steps[index]
		if run, _ := step["run"].(string); strings.Contains(run, "for type in ") {
			t.Errorf("ci.yml's job %s tests the request types one after another in a loop, not each in a job of a matrix", name)
		}
		if env, _ := step["env"].(map[string]any); env["type"] != "${{ matrix.type }}" {
			t.Errorf("ci.yml's job %s runs %s with type %v in its env, not ${{ matrix.type }}", name, command, env["type"])
		}
		if condition := step["if"]; condition != sideCondition("exporter") {
			t.Errorf("ci.yml tests each request type under the condition %q, not %q", condition, sideCondition("exporter"))
		}
	}
	if job == "" {
		t.Fatalf("ci.yml has no step that runs %s after %s for a request type", command, packages)
	}
	definition, _ := workflowDocument(t, ".github/workflows/ci.yml")["jobs"].(map[string]any)[job].(map[string]any)
	strategy, _ := definition["strategy"].(map[string]any)
	if strategy["fail-fast"] != false {
		t.Errorf("ci.yml's job %s has fail-fast %v, so one type's failure cancels the others' runs; set it to false", job, strategy["fail-fast"])
	}
	matrix, _ := strategy["matrix"].(map[string]any)
	values, _ := matrix["type"].([]any)
	var matrixTypes []string
	for _, value := range values {
		name, _ := value.(string)
		matrixTypes = append(matrixTypes, name)
	}
	sort.Strings(matrixTypes)
	if types := requestTypesInTheTree(t); !slices.Equal(matrixTypes, types) {
		t.Errorf("ci.yml's job %s tests the request types %v, but the tree has %v", job, matrixTypes, types)
	}
	if len(matrix) != 1 {
		t.Errorf("ci.yml's job %s has the matrix %v; its one dimension is type", job, matrix)
	}
	makefile := read(t, "Makefile")
	target := regexp.MustCompile(`(?m)^test-request-types:\n((?:\t.*\n)+)`).FindStringSubmatch(makefile)
	if target == nil {
		t.Fatal("the Makefile has no test-request-types target")
	}
	recipe := strings.ReplaceAll(target[1], "$$", "$")
	for _, want := range []string{tags, everyPackage(command)} {
		if !strings.Contains(recipe, want) {
			t.Errorf("make test-request-types does not run %s, as ci.yml does:\n%s", want, recipe)
		}
	}
	loop := requestTypeLoop.FindStringSubmatch(recipe)
	if loop == nil {
		t.Fatalf("make test-request-types has no loop over the request types:\n%s", recipe)
	}
	listed := strings.Fields(loop[1])
	sort.Strings(listed)
	if types := requestTypesInTheTree(t); !slices.Equal(listed, types) {
		t.Errorf("make test-request-types tests the request types %v, but the tree has %v", listed, types)
	}
	if !regexp.MustCompile(`(?m)^ci:.*\btest-request-types\b`).MatchString(makefile) {
		t.Error("make ci does not include test-request-types")
	}
}

// The lint job lints builds that carry every request type, where a helper
// a build of one type leaves unused - one in an untagged file only another
// type's tests call - is used, so twice such a helper reached main. So the
// request types' matrix lints each type's build on its own too: a step reads
// the tags tools/request-type-tags.sh prints for the job's type, under the
// go filter, and the golangci-lint action, pinned and at the version as the
// lint job's, lints with them after it, under the same condition; and
// `make lint-request-types`, which `make ci` includes, lints the same
// builds in a loop naming every type of the tree.
func TestCIAndMakeLintEveryRequestTypeOnItsOwn(t *testing.T) {
	const tags, output = `tags="$(sh tools/request-type-tags.sh "$type")"`, `echo "tags=$tags" >> "$GITHUB_OUTPUT"`
	all := workflowSteps(t, ".github/workflows/ci.yml")
	var action string
	for _, step := range all["lint"] {
		if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "golangci/golangci-lint-action@") {
			action = uses
			break
		}
	}
	if action == "" {
		t.Fatal("ci.yml's job lint no longer uses the golangci-lint action")
	}
	version := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION := (\S+)$`).FindStringSubmatch(read(t, "Makefile"))
	if version == nil {
		t.Fatal("the Makefile no longer pins GOLANGCI_LINT_VERSION")
	}
	jobs, _ := workflowDocument(t, ".github/workflows/ci.yml")["jobs"].(map[string]any)
	matrixJobs := 0
	for name, raw := range jobs {
		definition, _ := raw.(map[string]any)
		strategy, _ := definition["strategy"].(map[string]any)
		matrix, _ := strategy["matrix"].(map[string]any)
		if _, ok := matrix["type"]; !ok {
			continue
		}
		matrixJobs++
		steps := all[name]
		at := stepIndex(steps, tags, output)
		if at < 0 {
			t.Errorf("ci.yml's job %s does not read the tags of its type with %s and %s", name, tags, output)
			continue
		}
		step := steps[at]
		if env, _ := step["env"].(map[string]any); env["type"] != "${{ matrix.type }}" {
			t.Errorf("ci.yml's job %s reads the tags of the type %v, not ${{ matrix.type }}", name, env["type"])
		}
		if step["if"] != sideCondition("go") {
			t.Errorf("ci.yml's job %s reads the tags of its type under the condition %v, not %q", name, step["if"], sideCondition("go"))
		}
		id, _ := step["id"].(string)
		want := "--build-tags ${{ steps." + id + ".outputs.tags }}"
		linted := false
		for _, lint := range steps[at+1:] {
			if lint["uses"] != action {
				continue
			}
			with, _ := lint["with"].(map[string]any)
			if with["args"] != want {
				continue
			}
			linted = true
			if with["version"] != version[1] {
				t.Errorf("ci.yml's job %s lints its type with golangci-lint %v, but the Makefile pins %s", name, with["version"], version[1])
			}
			if lint["if"] != sideCondition("go") {
				t.Errorf("ci.yml's job %s lints its type under the condition %v, not %q", name, lint["if"], sideCondition("go"))
			}
		}
		if id == "" || !linted {
			t.Errorf("ci.yml's job %s has no step after the one reading its type's tags that uses %s with the args %s", name, action, want)
		}
	}
	if matrixJobs != 1 {
		t.Errorf("ci.yml has %d jobs of a matrix over the request types; one was meant", matrixJobs)
	}
	prerequisites, recipes := makefileRules(t)
	recipe := recipes["lint-request-types"]
	for _, want := range []string{tags, `golangci-lint run --build-tags "$tags"`} {
		if !strings.Contains(recipe, want) {
			t.Errorf("make lint-request-types does not run %s:\n%s", want, recipe)
		}
	}
	if !slices.Contains(prerequisites["lint-request-types"], "lint-version") {
		t.Error("make lint-request-types does not check the golangci-lint version first (lint-version)")
	}
	loop := requestTypeLoop.FindStringSubmatch(recipe)
	if loop == nil {
		t.Fatalf("make lint-request-types has no loop over the request types:\n%s", recipe)
	}
	listed := strings.Fields(loop[1])
	sort.Strings(listed)
	if types := requestTypesInTheTree(t); !slices.Equal(listed, types) {
		t.Errorf("make lint-request-types lints the request types %v, but the tree has %v", listed, types)
	}
	if !slices.Contains(prerequisites["ci"], "lint-request-types") {
		t.Error("make ci does not include lint-request-types")
	}
}

// makeRule is a rule of the Makefile: a target, its prerequisites after the
// colon, and no assignment (:=).
var makeRule = regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+):([^=\n][^\n]*)?$`)

// makefileRules reads the Makefile's rules: each target's prerequisites and
// recipe, the recipe with make's $$ read as the shell's $.
func makefileRules(t *testing.T) (prerequisites map[string][]string, recipes map[string]string) {
	t.Helper()
	prerequisites, recipes = map[string][]string{}, map[string]string{}
	lines := strings.Split(read(t, "Makefile"), "\n")
	for i, line := range lines {
		match := makeRule.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		prerequisites[match[1]] = strings.Fields(match[2])
		var recipe strings.Builder
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "\t") {
				break
			}
			recipe.WriteString(strings.ReplaceAll(next, "$$", "$"))
			recipe.WriteString("\n")
		}
		recipes[match[1]] = recipe.String()
	}
	if len(recipes) == 0 {
		t.Fatal("no rule of the Makefile was read")
	}
	return prerequisites, recipes
}

// `make ci` is meant to be what CI runs, and had drifted from it: CI vets and
// builds each request type on its own, which make ci did not, and ci was no
// .PHONY target, so a file named ci would have stopped it running. Every go
// command a step of ci.yml's test job runs, and every make target one runs
// beside the installing ones, has to be run by a target `make ci` reaches,
// and every target of the Makefile is .PHONY, since none makes a file of
// its name. go mod download is the one exception: it fills the module cache
// before CI's steps, which each go command make runs does for itself. Every
// job of ci.yml is read, not only test: the race run, each request type's
// tests and the allocator's are jobs of their own. CI tests each side's
// packages, $packages, and test/repository apart; make runs the same command
// over ./..., every package, which the sides and test/repository make up.
func TestMakeCIRunsTheGoCommandsOfTheCIWorkflow(t *testing.T) {
	prerequisites, recipes := makefileRules(t)
	reached, queue := map[string]bool{}, []string{"ci"}
	for len(queue) > 0 {
		target := queue[0]
		queue = queue[1:]
		if reached[target] {
			continue
		}
		if _, ok := recipes[target]; !ok {
			t.Fatalf("make ci reaches %s, which the Makefile has no rule for", target)
		}
		reached[target] = true
		queue = append(queue, prerequisites[target]...)
	}
	var run strings.Builder
	for target := range reached {
		run.WriteString(recipes[target])
	}
	makeCI := run.String()
	commands := 0
	var steps []map[string]any
	for _, job := range workflowSteps(t, ".github/workflows/ci.yml") {
		steps = append(steps, job...)
	}
	for _, step := range steps {
		script, _ := step["run"].(string)
		for line := range strings.SplitSeq(script, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "go mod download":
			case strings.HasPrefix(line, "go "), strings.HasPrefix(line, "for type in "), strings.HasPrefix(line, `tags="$(sh tools/request-type-tags.sh`):
				commands++
				if !strings.Contains(makeCI, everyPackage(line)) {
					t.Errorf("ci.yml runs %s, which no target make ci reaches runs (as %s)", line, everyPackage(line))
				}
			case strings.HasPrefix(line, "make "):
				for _, target := range strings.Fields(line)[1:] {
					if !strings.HasSuffix(target, "-install") && !reached[target] {
						t.Errorf("ci.yml runs make %s, which make ci does not reach", target)
					}
				}
			}
		}
	}
	// What ci.yml runs today: the suite, raced, vetted and built with every
	// type and with each on its own, each type's tests and the allocator's.
	if commands < 8 {
		t.Errorf("only %d go commands were read from ci.yml; the reader has stopped finding them", commands)
	}
	phony := regexp.MustCompile(`(?m)^\.PHONY:(.*)$`).FindAllStringSubmatch(read(t, "Makefile"), -1)
	declared := map[string]bool{}
	for _, match := range phony {
		for _, target := range strings.Fields(match[1]) {
			declared[target] = true
		}
	}
	for target := range recipes {
		if !strings.HasPrefix(target, ".") && !declared[target] {
			t.Errorf("the Makefile's %s target is not .PHONY; a file of that name would stop it running", target)
		}
	}
}

// `make build` compiled every package and kept nothing, while the docs gave
// it as the way to build the exporter with only some request types. It
// builds the main package into bin/, under the name the image gives the
// binary, with the tags of REQUEST_TYPES, SQL_DRIVERS and
// OPT_IN_REQUEST_TYPES, and the docs that
// give it say where the binary lands. The target allocator, cmd/pue-allocator,
// is built beside it with the same tags, and the image carries both.
func TestMakeBuildKeepsTheExportersBinary(t *testing.T) {
	_, recipes := makefileRules(t)
	const command = `tags="$(sh tools/request-type-tags.sh '$(REQUEST_TYPES)' '$(SQL_DRIVERS)' '$(OPT_IN_REQUEST_TYPES)')" && go build -tags "$tags" -o bin/$(APP) . && go build -tags "$tags" -o bin/$(ALLOCATOR) ./cmd/$(ALLOCATOR)`
	if got := strings.TrimSpace(recipes["build"]); got != command {
		t.Errorf("make build runs %q, want %q", got, command)
	}
	app := regexp.MustCompile(`(?m)^APP := (\S+)$`).FindStringSubmatch(read(t, "Makefile"))
	if app == nil {
		t.Fatal("the Makefile no longer names APP")
	}
	dockerfile := read(t, "Dockerfile")
	if want := "-o /out/" + app[1] + " ."; !strings.Contains(dockerfile, want) {
		t.Errorf("the Dockerfile does not build %q, the binary make build names", want)
	}
	allocator := regexp.MustCompile(`(?m)^ALLOCATOR := (\S+)$`).FindStringSubmatch(read(t, "Makefile"))
	if allocator == nil {
		t.Fatal("the Makefile no longer names ALLOCATOR")
	}
	if _, err := os.Stat(filepath.Join("cmd", allocator[1], "main.go")); err != nil {
		t.Errorf("the Makefile's ALLOCATOR %s is no command under cmd/: %v", allocator[1], err)
	}
	for _, want := range []string{
		"-o /out/" + allocator[1] + " ./cmd/" + allocator[1] + "\n",
		"COPY --from=build /out/" + allocator[1] + " /bin/" + allocator[1] + "\n",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the Dockerfile does not have %q: the image carries the allocator beside the exporter", strings.TrimSpace(want))
		}
	}
	if !strings.Contains(read(t, "docs/ALLOCATOR.md"), "bin/"+allocator[1]) {
		t.Errorf("docs/ALLOCATOR.md does not say make build leaves the allocator in bin/%s", allocator[1])
	}
	for _, doc := range []string{"docs/CONFIGURATION.md", "docs/DEVELOPMENT.md", "docs/GRPC.md"} {
		text := read(t, doc)
		if !strings.Contains(text, "make build") {
			t.Errorf("%s no longer gives make build", doc)
		}
		if !strings.Contains(text, "bin/"+app[1]) {
			t.Errorf("%s gives make build without saying the binary lands in bin/%s", doc, app[1])
		}
	}
}

// The suite runs twice and shuffled under the race detector, where a package
// takes several times what it takes without: go test's own limit of ten
// minutes a package, meant for one plain run, was what a slow machine would
// have failed on before any test did. So the command carries a limit of its
// own, at least twice go test's. CI runs it in one step for each side, the
// exporter's and the allocator's, over the packages tools/ci-packages.sh
// lists for the side and under the condition of the side's filter, by the
// same command, and `make test` runs that very command over every package,
// so that what passes here is what passes there.
func TestCIAndMakeRunTheRaceSuiteWithALimitOfItsOwn(t *testing.T) {
	pattern := regexp.MustCompile(`^go test -race -count=2 -shuffle=on -timeout (\S+) \$packages$`)
	side := regexp.MustCompile(`^packages="\$\(sh tools/ci-packages\.sh (\S+)\)"$`)
	var steps []map[string]any
	for _, job := range workflowSteps(t, ".github/workflows/ci.yml") {
		steps = append(steps, job...)
	}
	var command string
	sides := map[string]int{}
	for _, step := range steps {
		run, _ := step["run"].(string)
		if !strings.Contains(run, "-race") {
			continue
		}
		lines := strings.Split(strings.TrimSpace(run), "\n")
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		name := ""
		if match := side.FindStringSubmatch(lines[0]); len(lines) == 2 && match != nil {
			name = match[1]
		}
		if name == "" {
			t.Errorf("ci.yml runs the race detector with %q, not after %s naming the side's packages", strings.TrimSpace(run), side)
			continue
		}
		sides[name]++
		if command != "" && lines[1] != command {
			t.Errorf("ci.yml runs the race detector with %q and with %q; the sides were meant to run one command", command, lines[1])
		}
		command = lines[1]
		if condition := step["if"]; condition != sideCondition(name) {
			t.Errorf("ci.yml runs %q for the %s side under the condition %q, not %q", command, name, condition, sideCondition(name))
		}
	}
	if !reflect.DeepEqual(sides, map[string]int{"allocator": 1, "exporter": 1}) {
		t.Errorf("ci.yml runs the race detector for the sides %v; one step for the exporter and one for the allocator were meant", sides)
	}
	match := pattern.FindStringSubmatch(command)
	if match == nil {
		t.Fatalf("ci.yml runs the race detector with %q, not twice, shuffled and with a limit of its own (%s)", command, pattern)
	}
	limit, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatalf("ci.yml gives the race suite the limit %q: %v", match[1], err)
	}
	// go test stops a package after ten minutes unless told otherwise.
	if limit < 20*time.Minute {
		t.Errorf("ci.yml gives a package %s under the race detector; twice go test's own ten minutes is the least", limit)
	}
	target := regexp.MustCompile(`(?m)^test:\n((?:\t.*\n)+)`).FindStringSubmatch(read(t, "Makefile"))
	if target == nil {
		t.Fatal("the Makefile has no test target")
	}
	var raced []string
	for line := range strings.SplitSeq(target[1], "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "-race") && !strings.HasPrefix(line, "@#") {
			raced = append(raced, line)
		}
	}
	if len(raced) != 1 || raced[0] != everyPackage(command) {
		t.Errorf("make test runs the race detector with %q, but ci.yml with %q", raced, everyPackage(command))
	}
}

// sideCondition is the condition of the steps that run when the named output
// of ci.yml's changed-path filter is true.
func sideCondition(output string) string {
	return "steps.changes.outputs." + output + " == 'true'"
}

// everyPackage is a go command of ci.yml as make runs it: over ./..., every
// package, where CI names a side's packages or test/repository.
func everyPackage(command string) string {
	for _, packages := range []string{" $packages", " ./test/repository/"} {
		if strings.HasSuffix(command, packages) {
			return strings.TrimSuffix(command, packages) + " ./..."
		}
	}
	return command
}

// The race run, each request type's tests and the allocator's run in jobs of
// their own, beside test, and each starts with the setup steps of test
// written out again: checkout, the changed-path filter, Go, the image's
// Python with its libraries and the modules only the tests use, and go mod
// download, helm's setup alone left out. A copy that drifts from test's —
// another filter, another Go or Python, a missing module — would have one
// job run what the others do not, so every other job's steps before its last
// must be test's setup steps, the same, in their order; its env and machine
// are test's; each of its own steps has the condition of one output of the
// filter whose files the setup's own condition, the go output's, covers
// (cisides_test.go keeps that so); and no job of ci.yml has a job-level
// `if`, which would leave a matrix job reported as skipped under a name no
// check requires.
func TestTheGoJobsOfCIStartWithTheSetupOfTest(t *testing.T) {
	const path = ".github/workflows/ci.yml"
	all := workflowSteps(t, path)
	test := all["test"]
	download := stepIndex(test, "go mod download")
	if download < 0 {
		t.Fatal("ci.yml's job test no longer runs go mod download, where its setup steps end")
	}
	var setup []map[string]any
	for _, step := range test[:download+1] {
		if step["if"] != chartStepCondition {
			setup = append(setup, step)
		}
	}
	if len(setup) < 8 {
		t.Fatalf("only %d setup steps were read from ci.yml's job test; the reader has stopped finding them", len(setup))
	}
	jobs, _ := workflowDocument(t, path)["jobs"].(map[string]any)
	testJob, _ := jobs["test"].(map[string]any)
	others := 0
	for name, raw := range jobs {
		definition, _ := raw.(map[string]any)
		if _, ok := definition["if"]; ok {
			t.Errorf("ci.yml's job %s has a job-level `if`; gate its steps with `if: steps.changes.outputs.go == 'true'` instead", name)
		}
		if name == "test" {
			continue
		}
		others++
		steps := all[name]
		if len(steps) <= len(setup) {
			t.Errorf("ci.yml's job %s has %d steps, not test's %d setup steps and its own after them", name, len(steps), len(setup))
			continue
		}
		for index, want := range setup {
			if !reflect.DeepEqual(steps[index], want) {
				t.Errorf("ci.yml's job %s has step %d\n%s\nwhere test's setup has\n%s", name, index+1, yamlText(steps[index]), yamlText(want))
			}
		}
		for _, step := range steps[len(setup):] {
			if condition := step["if"]; !slices.Contains([]any{sideCondition("go"), sideCondition("exporter"), sideCondition("allocator")}, condition) {
				t.Errorf("ci.yml's job %s runs a step under the condition %v, not that of the go, exporter or allocator filter:\n%s", name, condition, yamlText(step))
			}
		}
		for _, key := range []string{"env", "runs-on"} {
			if !reflect.DeepEqual(definition[key], testJob[key]) {
				t.Errorf("ci.yml's job %s has the %s %v, but test has %v", name, key, definition[key], testJob[key])
			}
		}
	}
	// The linters, the builds, the race job, the request types' matrix and
	// the allocator's job.
	if others < 5 {
		t.Errorf("ci.yml has %d jobs beside test; the linters, the builds, the race run, the request types' tests and the allocator's tests were meant to have one each", others)
	}
}

// test ran every check one after another and took a quarter of an hour, the
// longest job by far: the linters and the builds need nothing test does, so
// they run in the jobs lint and build beside it, and test keeps the
// repository's checks, the exporter's tests and the chart steps. A linter or
// a build added back to test would make it the longest job again.
func TestTheTestJobLeavesTheLintersAndBuildsToJobsOfTheirOwn(t *testing.T) {
	all := workflowSteps(t, ".github/workflows/ci.yml")
	for _, c := range []struct{ job, what, fragment string }{
		{"lint", "checks the formatting", "gofmt -l"},
		{"lint", "runs golangci-lint", "golangci/golangci-lint-action"},
		{"lint", "runs gopls", "gopls-check"},
		{"build", "vets the code", "go vet ./..."},
		{"build", "builds the code", "go build ./..."},
		{"build", "builds for darwin", "GOOS=darwin"},
		{"build", "builds the image", "docker build"},
		{"build", "builds with the opt-in sql drivers", "request-type-tags.sh '' oracle"},
		{"build", "builds with the opt-in request types", "request-type-tags.sh '' '' "},
	} {
		if workflowStepWith(all["test"], c.fragment) {
			t.Errorf("ci.yml's job test %s (%q); that belongs to the job %s", c.what, c.fragment, c.job)
		}
		if !workflowStepWith(all[c.job], c.fragment) {
			t.Errorf("ci.yml's job %s no longer %s (%q)", c.job, c.what, c.fragment)
		}
	}
}

// workflowStepWith reports whether a step runs, or uses, fragment.
func workflowStepWith(steps []map[string]any, fragment string) bool {
	for _, step := range steps {
		run, _ := step["run"].(string)
		uses, _ := step["uses"].(string)
		if strings.Contains(run, fragment) || strings.Contains(uses, fragment) {
			return true
		}
	}
	return false
}

// stepIndex returns the index of the first step whose run script contains
// every fragment, or -1.
func stepIndex(steps []map[string]any, fragments ...string) int {
	for index, step := range steps {
		run, _ := step["run"].(string)
		found := true
		for _, fragment := range fragments {
			if !strings.Contains(run, fragment) {
				found = false
				break
			}
		}
		if found {
			return index
		}
	}
	return -1
}

// A chart version deploys its appVersion's image by default, and the exporter
// is released on its own tags, so the chart release checks that image exists
// before it pushes anything: a chart published ahead of its image gives every
// install an ImagePullBackOff. The chart is signed by the digest helm pushed,
// never by its tag, which could be moved to other content before the signing
// step resolves it.
func TestTheChartReleaseChecksItsImageAndSignsTheDigest(t *testing.T) {
	steps := workflowSteps(t, ".github/workflows/release-chart.yml")["release-chart"]
	inspect := stepIndex(steps, "appVersion", "docker buildx imagetools inspect", "ghcr.io/${GITHUB_REPOSITORY_OWNER}/prometheus-universal-exporter:${app_version}", "release the exporter")
	push := stepIndex(steps, "helm push")
	if inspect < 0 {
		t.Fatal("release-chart.yml does not check that the chart's default image, at its appVersion, exists")
	}
	if push < 0 || inspect > push {
		t.Error("release-chart.yml checks the chart's image after helm push, or never pushes")
	}
	if push >= 0 {
		if steps[push]["id"] != "push" || stepIndex(steps, "helm push", `echo "digest=$digest" >> "$GITHUB_OUTPUT"`) != push {
			t.Error("the helm push step does not publish the pushed digest as steps.push.outputs.digest")
		}
	}
	for _, command := range []string{"cosign sign", "cosign verify"} {
		index := stepIndex(steps, command)
		if index < 0 {
			t.Errorf("release-chart.yml no longer runs %s", command)
			continue
		}
		run := steps[index]["run"].(string)
		env, _ := steps[index]["env"].(map[string]any)
		if !strings.Contains(run, "prometheus-universal-exporter@${DIGEST}") || env["DIGEST"] != "${{ steps.push.outputs.digest }}" {
			t.Errorf("%s does not name the chart by the digest helm pushed:\n%s", command, run)
		}
		if strings.Contains(run, ":${VERSION}") {
			t.Errorf("%s names the chart by its tag:\n%s", command, run)
		}
	}
}

// The exporter release publishes a SHA-256 checksum of every archive beside
// them, so a download can be checked with sha256sum -c.
func TestTheExporterReleasePublishesChecksums(t *testing.T) {
	steps := workflowSteps(t, ".github/workflows/release.yml")["release"]
	build := stepIndex(steps, "tar -czf", `sha256sum *.tar.gz > "prometheus-universal-exporter-${VERSION}-sha256sums.txt"`)
	if build < 0 {
		t.Fatal("release.yml does not write the archives' checksums where it builds them")
	}
	upload := stepIndex(steps, "gh release create", "dist/*.tar.gz", `"dist/prometheus-universal-exporter-${VERSION}-sha256sums.txt"`)
	if upload < 0 || upload < build {
		t.Error("release.yml does not publish the checksums file with the archives")
	}
}
