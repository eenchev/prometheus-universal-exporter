package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

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

// CI vets and builds each request type on its own, so its loop names every
// internal/fetch/requesttype_<name>.go.
func TestCIBuildsEveryRequestTypeOnItsOwn(t *testing.T) {
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
	match := regexp.MustCompile(`(?m)^\s*for type in ([a-z ]+); do\s*$`).FindStringSubmatch(read(t, ".github/workflows/ci.yml"))
	if match == nil {
		t.Fatal("ci.yml has no loop over the request types")
	}
	listed := strings.Fields(match[1])
	sort.Strings(listed)
	if !slices.Equal(listed, types) {
		t.Fatalf("ci.yml builds the request types %v on their own, but the tree has %v", listed, types)
	}
}
