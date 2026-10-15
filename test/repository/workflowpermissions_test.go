package repository

import (
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowWriteGrants is every write permission a workflow's jobs are given,
// by workflow file and job. A job missing here may write nothing, so a new
// write grant, or a grant moved to the top of a workflow, where every job
// would get it, fails until it is added here on purpose.
var workflowWriteGrants = map[string]map[string][]string{
	"ci.yml":          {},
	"govulncheck.yml": {},
	"release.yml": {
		// The GitHub Release, and the image on GHCR.
		"release": {"contents", "packages"},
	},
	"release-chart.yml": {
		// The GitHub Release, the chart on GHCR, and cosign's keyless
		// signature from the workflow's OIDC token.
		"release-chart": {"contents", "id-token", "packages"},
	},
	"update-docker-deps.yml": {
		// create-pull-request pushes a branch and opens the pull request.
		"update": {"contents", "pull-requests"},
	},
}

// writeScopes returns the scopes a permissions value grants write to, sorted,
// and a complaint when the value is not a mapping of scopes to read, write or
// none: read-all and write-all are refused, the second because it grants every
// scope, the first because a list of scopes says what a job reads.
func writeScopes(value any) ([]string, string) {
	grants, ok := value.(map[string]any)
	if !ok {
		return nil, "is not a mapping of scopes"
	}
	var writes []string
	for scope, level := range grants {
		switch level {
		case "write":
			writes = append(writes, scope)
		case "read", "none":
		default:
			return nil, "gives " + scope + " the level " + strings.TrimSpace(yamlText(level)) + ", not read, write or none"
		}
	}
	sort.Strings(writes)
	return writes, ""
}

func yamlText(value any) string {
	out, err := yaml.Marshal(value)
	if err != nil {
		return "?"
	}
	return string(out)
}

// A workflow without a permissions block runs with the repository's default
// token, which may write to everything. Every workflow therefore sets one at
// its top that grants no write, and a job that has to write grants itself
// exactly the scopes workflowWriteGrants lists for it, so a write a job is
// given is one somebody chose.
func TestEveryWorkflowGrantsItsJobsOnlyTheWritesListed(t *testing.T) {
	var seen []string
	for _, path := range workflowPaths(t) {
		name := filepath.Base(path)
		seen = append(seen, name)
		allowed, listed := workflowWriteGrants[name]
		if !listed {
			t.Errorf("%s is not in workflowWriteGrants; list it with the writes its jobs need, if any", name)
			continue
		}
		document := workflowDocument(t, path)
		top, ok := document["permissions"]
		if !ok {
			t.Errorf("%s has no top-level permissions, so its jobs get the repository's default token", name)
		} else if writes, problem := writeScopes(top); problem != "" {
			t.Errorf("%s: its top-level permissions %s", name, problem)
		} else if len(writes) > 0 {
			t.Errorf("%s grants %v at its top, to every job; grant a write in the job that needs it", name, writes)
		}
		jobs, _ := document["jobs"].(map[string]any)
		for job, raw := range jobs {
			definition, _ := raw.(map[string]any)
			var writes []string
			if value, ok := definition["permissions"]; ok {
				var problem string
				if writes, problem = writeScopes(value); problem != "" {
					t.Errorf("%s job %s: its permissions %s", name, job, problem)
					continue
				}
			}
			if want := allowed[job]; !slices.Equal(writes, want) {
				t.Errorf("%s job %s is granted write to %v, but workflowWriteGrants lists %v", name, job, writes, want)
			}
		}
		for job := range allowed {
			if _, ok := jobs[job]; !ok {
				t.Errorf("workflowWriteGrants lists %s job %s, which the workflow does not have", name, job)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(workflowWriteGrants)) {
		if !slices.Contains(seen, name) {
			t.Errorf("workflowWriteGrants lists %s, which is not a workflow", name)
		}
	}
}

// Each release workflow publishes nothing unless CI has passed on the commit
// the tag names: a first job validates the tag and runs tools/ci-passed.py on
// the checked-out commit with a token that may read Actions runs, and every
// other job needs it, so none starts before it has succeeded.
func TestTheReleaseWorkflowsWaitForCIToPassOnTheTaggedCommit(t *testing.T) {
	const gate = `python3 tools/ci-passed.py --repository "$GITHUB_REPOSITORY" --sha "$(git rev-parse HEAD)"`
	for _, path := range []string{".github/workflows/release.yml", ".github/workflows/release-chart.yml"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			jobs, _ := workflowDocument(t, path)["jobs"].(map[string]any)
			preflight, _ := jobs["preflight"].(map[string]any)
			if preflight == nil {
				t.Fatalf("%s has no preflight job", path)
			}
			if _, needs := preflight["needs"]; needs {
				t.Errorf("%s's preflight job waits for another job; it has to run first", path)
			}
			grants, _ := preflight["permissions"].(map[string]any)
			if grants["actions"] != "read" || grants["contents"] != "read" {
				t.Errorf("%s's preflight job is given %v; it reads the repository and the CI runs (contents: read, actions: read)", path, grants)
			}
			steps := workflowSteps(t, path)["preflight"]
			if len(steps) == 0 || !strings.Contains(stringOf(steps[0]["run"]), "grep -Eq") {
				t.Errorf("%s's preflight job does not validate the tag first, so a malformed tag would wait for CI before failing", path)
			}
			index := stepIndex(steps, gate)
			if index < 0 {
				t.Fatalf("%s's preflight job does not run %s", path, gate)
			}
			if env, _ := steps[index]["env"].(map[string]any); env["GITHUB_TOKEN"] != "${{ secrets.GITHUB_TOKEN }}" {
				t.Errorf("%s does not give tools/ci-passed.py the job's token", path)
			}
			if checkout := stepUsing(steps, "actions/checkout@"); checkout < 0 || checkout > index {
				t.Errorf("%s runs tools/ci-passed.py before checking out the tagged commit", path)
			}
			for job, raw := range jobs {
				if job == "preflight" {
					continue
				}
				definition, _ := raw.(map[string]any)
				if !needsJob(definition["needs"], "preflight") {
					t.Errorf("%s job %s does not need preflight, so it can publish before CI has passed", path, job)
				}
			}
		})
	}
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

// stepUsing returns the index of the first step that uses an action whose
// reference starts with prefix, or -1.
func stepUsing(steps []map[string]any, prefix string) int {
	for index, step := range steps {
		if strings.HasPrefix(stringOf(step["uses"]), prefix) {
			return index
		}
	}
	return -1
}

// needsJob reports whether a job's needs, a name or a list of names, names
// job.
func needsJob(needs any, job string) bool {
	switch value := needs.(type) {
	case string:
		return value == job
	case []any:
		return slices.Contains(value, any(job))
	}
	return false
}
