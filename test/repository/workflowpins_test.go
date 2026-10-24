package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
//
// And each ecosystem's majors are as § 42.16 has them: the Go modules' and
// the test-only Python modules' ignored for every dependency, the actions'
// proposed in a group of their own beside the one of every minor and patch
// update, so a major bump is a pull request reviewed by itself. The actions'
// one group of every update took their majors in with the rest, while the
// spec excluded majors everywhere.
func TestDependabotUpdatesTheWorkflowActions(t *testing.T) {
	raw, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	type group struct {
		Patterns    []string `yaml:"patterns"`
		UpdateTypes []string `yaml:"update-types"`
	}
	type ignore struct {
		Dependency  string   `yaml:"dependency-name"`
		UpdateTypes []string `yaml:"update-types"`
	}
	var config struct {
		Updates []struct {
			Ecosystem string           `yaml:"package-ecosystem"`
			Directory string           `yaml:"directory"`
			Groups    map[string]group `yaml:"groups"`
			Ignore    []ignore         `yaml:"ignore"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, update := range config.Updates {
		seen[update.Ecosystem] = true
		ignoresMajors := slices.ContainsFunc(update.Ignore, func(rule ignore) bool {
			return rule.Dependency == "*" && slices.Contains(rule.UpdateTypes, "version-update:semver-major")
		})
		switch update.Ecosystem {
		case "gomod", "pip":
			if !ignoresMajors {
				t.Errorf(".github/dependabot.yml's %s updates do not ignore version-update:semver-major for every dependency (\"*\"), so they propose majors", update.Ecosystem)
			}
		case "github-actions":
			if update.Directory != "/" {
				t.Errorf(".github/dependabot.yml updates the actions of %s, not of /, where .github/workflows is read", update.Directory)
			}
			if len(update.Ignore) > 0 {
				t.Errorf(".github/dependabot.yml's github-actions updates ignore %v; their majors are proposed", update.Ignore)
			}
			var minorPatch, major []string
			for name, g := range update.Groups {
				if !slices.Contains(g.Patterns, "*") {
					continue
				}
				types := slices.Sorted(slices.Values(g.UpdateTypes))
				switch {
				case slices.Equal(types, []string{"minor", "patch"}):
					minorPatch = append(minorPatch, name)
				case slices.Equal(types, []string{"major"}):
					major = append(major, name)
				default:
					t.Errorf(".github/dependabot.yml's github-actions group %s takes the update types %v, not minor and patch or major alone", name, g.UpdateTypes)
				}
			}
			if len(minorPatch) != 1 || len(major) != 1 {
				t.Errorf(".github/dependabot.yml groups the actions' minor and patch updates in %v and their majors in %v; one group of every action for each was meant", minorPatch, major)
			}
		}
	}
	for _, ecosystem := range []string{"gomod", "pip", "github-actions"} {
		if !seen[ecosystem] {
			t.Errorf(".github/dependabot.yml has no %s update", ecosystem)
		}
	}
}
