package repository

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// paths-filter compares a tag push with the tag's merge base on main, which
// for a tag on main's newest commit is that commit: no file changed, every
// step of ci.yml was skipped, the run passed, and the release workflows took
// that run for CI passing on the commit. So the steps of every job run under
// the outputs of a step `changes` after the filter, which are all true on a
// tag push and the filter's own otherwise. This test runs that step's script
// as GitHub would, with the env it is given, and holds its OUTPUTS to the
// filter's outputs and every step's condition to it rather than to the
// filter.
func TestATagPushRunsEveryStepOfCI(t *testing.T) {
	const workflow = ".github/workflows/ci.yml"
	outputs := slices.Sorted(maps.Keys(ciFilters(t)))
	condition := regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outputs\.`)
	for job, steps := range workflowSteps(t, workflow) {
		filter, changes := "", map[string]any(nil)
		for _, step := range steps {
			if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "dorny/paths-filter@") {
				filter, _ = step["id"].(string)
			}
			if step["id"] == "changes" {
				if filter == "" {
					t.Errorf("ci.yml's job %s has its step changes before any changed-path filter", job)
				}
				changes = step
			}
			if when, _ := step["if"].(string); filter != "" {
				for _, match := range condition.FindAllStringSubmatch(when, -1) {
					if match[1] == filter {
						t.Errorf("a step of ci.yml's job %s runs under the filter's own output, which is false on a tag push: if: %s", job, when)
					}
				}
			}
		}
		if filter == "" {
			continue
		}
		if changes == nil {
			t.Errorf("ci.yml's job %s has a changed-path filter and no step changes after it", job)
			continue
		}
		env, _ := changes["env"].(map[string]any)
		if env["REF_TYPE"] != "${{ github.ref_type }}" || env["CHANGED"] != "${{ steps."+filter+".outputs.changes }}" {
			t.Errorf("ci.yml's job %s gives its step changes the env %v, not the ref type and the outputs of the filter %s", job, env, filter)
		}
		if named, _ := env["OUTPUTS"].(string); !slices.Equal(slices.Sorted(slices.Values(strings.Fields(named))), outputs) {
			t.Errorf("ci.yml's job %s has its step changes pass on %q, where the filter has the outputs %v", job, named, outputs)
		}
		script, _ := changes["run"].(string)
		named, _ := env["OUTPUTS"].(string)
		for _, run := range []struct {
			refType, changed string
			want             []string
		}{
			{"tag", "[]", outputs},
			{"tag", `["chart"]`, outputs},
			{"branch", "[]", nil},
			{"branch", `["chart"]`, []string{"chart"}},
			{"branch", `["go","exporter"]`, []string{"exporter", "go"}},
		} {
			file := filepath.Join(t.TempDir(), "output")
			cmd := exec.Command("bash", "-e", "-c", script) // #nosec G204 -- the workflow's own script
			cmd.Env = append(os.Environ(), "REF_TYPE="+run.refType, "CHANGED="+run.changed, "OUTPUTS="+named, "GITHUB_OUTPUT="+file)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the step changes of ci.yml's job %s failed: %v\n%s", job, err, out)
			}
			written, err := os.ReadFile(file) // #nosec G304 -- the test's own file
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for line := range strings.SplitSeq(strings.TrimSpace(string(written)), "\n") {
				name, value, _ := strings.Cut(line, "=")
				got[name] = value
			}
			for _, output := range outputs {
				want := "false"
				if slices.Contains(run.want, output) {
					want = "true"
				}
				if got[output] != want {
					t.Errorf("on a %s push that changed %s, the step changes of ci.yml's job %s gives %s=%q, want %q", run.refType, run.changed, job, output, got[output], want)
				}
			}
		}
	}
}
