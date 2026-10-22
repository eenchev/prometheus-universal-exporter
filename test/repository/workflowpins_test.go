package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// usesLine finds every uses: of a workflow, a commented-out one included: a
// job kept in a comment to be enabled later is enabled by deleting the
// comment marks, and it should then run pinned actions too.
var usesLine = regexp.MustCompile(`(?m)^[ \t]*(?:#[ \t]*)?(?:-[ \t]+)?uses:[ \t]*(.*)$`)

// pinnedUses is an action at a full commit SHA, with the release it is in as a
// comment, the form Dependabot keeps current: it moves the SHA and rewrites
// the comment together.
var pinnedUses = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$`)

// A tag or branch is moved by whoever controls the action's repository, and
// the next run executes whatever it then names, with the job's token, in the
// release jobs one that may write packages and releases. A commit SHA names
// one tree for good, so every action a workflow uses is pinned to one; an
// action in this repository (./...) or an image (docker://) is not fetched
// from another repository by ref and is left alone.
func TestEveryWorkflowActionIsPinnedToACommit(t *testing.T) {
	found := 0
	for _, path := range workflowPaths(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range usesLine.FindAllStringSubmatch(string(raw), -1) {
			ref := strings.TrimSpace(match[1])
			if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
				continue
			}
			found++
			if !pinnedUses.MatchString(ref) {
				t.Errorf("%s uses %q, not an action at a 40-character commit SHA followed by `# vX.Y.Z`", filepath.Base(path), ref)
			}
		}
	}
	if found == 0 {
		t.Fatal("no workflow uses an action; this test must not pass by finding nothing")
	}
}

// The pins are kept current by Dependabot's github-actions updates of the
// repository root, where it reads .github/workflows; without them a pinned
// action would stay on its release for good, security fixes included.
func TestDependabotUpdatesTheWorkflowActions(t *testing.T) {
	raw, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Updates []struct {
			Ecosystem string `yaml:"package-ecosystem"`
			Directory string `yaml:"directory"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	for _, update := range config.Updates {
		if update.Ecosystem == "github-actions" && update.Directory == "/" {
			return
		}
	}
	t.Error(".github/dependabot.yml has no github-actions update of the directory /, so nothing updates the actions' pinned SHAs")
}
