package repository

import (
	"slices"
	"testing"
)

// CI runs on a push to main, on a push of any tag and on a pull request, and
// on no other push: a push to a pull request's branch would run the same
// commit a second time beside the pull request's run. Every tag runs it, so
// the commit a release tag names gets the push run the release workflows
// wait for (tools/ci-passed.py).
func TestCIRunsOnMainTagsAndPullRequestsAlone(t *testing.T) {
	document := workflowDocument(t, ".github/workflows/ci.yml")
	triggers, ok := document["on"].(map[string]any)
	if !ok {
		t.Fatalf("ci.yml's trigger is %T, want a mapping", document["on"])
	}
	if _, ok := triggers["pull_request"]; !ok {
		t.Error("ci.yml does not run on a pull request")
	}
	push, ok := triggers["push"].(map[string]any)
	if !ok {
		t.Fatalf("ci.yml's push trigger is %T, want a mapping naming its branches and tags", triggers["push"])
	}
	var branches []string
	if list, ok := push["branches"].([]any); ok {
		for _, branch := range list {
			if text, ok := branch.(string); ok {
				branches = append(branches, text)
			}
		}
	}
	if !slices.Equal(branches, []string{"main"}) {
		t.Errorf("ci.yml runs on a push to the branches %q, want main alone", branches)
	}
	if tags := tagPatterns(t, document); !slices.Equal(tags, []string{"**"}) {
		t.Errorf("ci.yml runs on a push of the tags %q, want every tag (**)", tags)
	}
	for key := range push {
		if key != "branches" && key != "tags" {
			t.Errorf("ci.yml's push trigger has %s, which could leave a tag or main unrun", key)
		}
	}
}
