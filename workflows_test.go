package main

import (
	"os"
	"path/filepath"
	"sort"
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
