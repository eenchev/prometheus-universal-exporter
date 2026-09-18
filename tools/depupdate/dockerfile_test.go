package main

import (
	"os"
	"strings"
	"testing"
)

// The shape of the real Dockerfile: defaults at the top, the same names
// repeated without a default in the later stage so the values cross the stage
// boundary.
const sampleDockerfile = `ARG GO_VERSION=1.23
ARG PYTHON_VERSION=3.12
ARG LXML_VERSION=5.3.0

FROM golang:${GO_VERSION}-alpine AS build
RUN go build .

FROM python:${PYTHON_VERSION}-slim
ARG LXML_VERSION
RUN pip install --no-cache-dir lxml==${LXML_VERSION}
`

func TestParseArgDefaultsReadsOnlyDeclarationsWithAValue(t *testing.T) {
	pins := parseArgDefaults(sampleDockerfile)
	want := map[string]string{"GO_VERSION": "1.23", "PYTHON_VERSION": "3.12", "LXML_VERSION": "5.3.0"}
	if len(pins) != len(want) {
		t.Fatalf("parsed %v, want %v", pins, want)
	}
	for name, value := range want {
		if pins[name] != value {
			t.Fatalf("%s=%q, want %q", name, pins[name], value)
		}
	}
}

func TestRewriteArgDefaultsLeavesTheForwardingDeclarationAlone(t *testing.T) {
	rewritten, err := rewriteArgDefaults(sampleDockerfile, map[string]string{
		"GO_VERSION":   "1.24",
		"LXML_VERSION": "5.3.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ARG GO_VERSION=1.24", "ARG LXML_VERSION=5.3.2", "ARG PYTHON_VERSION=3.12"} {
		if !strings.Contains(rewritten, want) {
			t.Fatalf("rewritten Dockerfile is missing %q:\n%s", want, rewritten)
		}
	}
	// The bare declaration in the second stage must survive untouched, or the
	// pip pin would stop being interpolated.
	if !strings.Contains(rewritten, "\nARG LXML_VERSION\n") {
		t.Fatalf("the stage-local ARG declaration was rewritten:\n%s", rewritten)
	}
	if strings.Contains(rewritten, "1.23") || strings.Contains(rewritten, "5.3.0") {
		t.Fatalf("an old version survived the rewrite:\n%s", rewritten)
	}
	// Everything else, including the interpolations, is left byte for byte.
	if !strings.Contains(rewritten, "FROM golang:${GO_VERSION}-alpine AS build") {
		t.Fatalf("the FROM line was altered:\n%s", rewritten)
	}
}

func TestRewriteArgDefaultsFailsRatherThanSilentlyDoingNothing(t *testing.T) {
	if _, err := rewriteArgDefaults(sampleDockerfile, map[string]string{"NO_SUCH_VERSION": "1.0"}); err == nil {
		t.Fatal("rewriting an argument the Dockerfile does not declare should fail")
	}
	duplicated := sampleDockerfile + "ARG GO_VERSION=1.23\n"
	if _, err := rewriteArgDefaults(duplicated, map[string]string{"GO_VERSION": "1.24"}); err == nil {
		t.Fatal("a duplicated declaration should fail rather than update one of them")
	}
}

func TestDockerTagCandidatesKeepOnlyTheImageFormTheBuildUses(t *testing.T) {
	tags := []string{
		"1.23-alpine", "1.24-alpine", "1.24.0-alpine", "1", "latest", "alpine",
		"1.24-alpine3.20", "1.25rc1-alpine", "1.24-bookworm", "1.24-slim",
	}
	got := dockerTagCandidates(tags, "-alpine")
	want := map[string]bool{"1.23": true, "1.24": true, "1.24.0": true}
	if len(got) != len(want) {
		t.Fatalf("candidates=%v, want exactly %v", got, want)
	}
	for _, candidate := range got {
		if !want[candidate] {
			t.Fatalf("candidate %q is not published as an -alpine image", candidate)
		}
	}
}

func TestPypiCandidatesDropReleasesPipCannotInstall(t *testing.T) {
	releases := map[string][]pypiFile{
		"5.3.0": {{}},
		"5.3.1": {{Yanked: true}, {Yanked: true}},
		"5.3.2": {{Yanked: true}, {}},
		"5.3.3": {},
	}
	got := pypiCandidates(releases)
	want := []string{"5.3.0", "5.3.2"}
	if len(got) != len(want) {
		t.Fatalf("candidates=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates=%v, want %v", got, want)
		}
	}
}

func TestSummaryIsEmptyWhenNothingMoves(t *testing.T) {
	if summaryMarkdown(nil) != "" {
		t.Fatal("an empty update set must produce no pull request body")
	}
	body := summaryMarkdown([]update{{Arg: "GO_VERSION", Source: "docker.io/library/golang (`alpine`)", Previous: "1.23", Next: "1.24"}})
	for _, want := range []string{"GO_VERSION", "1.23", "1.24", "library/golang"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the pull request body should mention %q:\n%s", want, body)
		}
	}
}

// The pin table and the real Dockerfile have to agree: a renamed build argument
// would otherwise leave a dependency silently unwatched.
func TestEveryPinnedVersionInTheDockerfileIsWatched(t *testing.T) {
	raw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check against: %v", err)
	}
	declared := parseArgDefaults(string(raw))
	watched := map[string]bool{}
	for _, p := range pins {
		watched[p.Arg] = true
		if _, ok := declared[p.Arg]; !ok {
			t.Errorf("%s is watched but the Dockerfile no longer declares it", p.Arg)
		}
	}
	for name := range declared {
		if !watched[name] {
			t.Errorf("the Dockerfile pins %s but nothing updates it", name)
		}
	}
}

// The policies must produce the moves the documentation promises for the
// versions actually pinned today.
func TestPolicyKeepsTheDockerfilePinsWithinTheirMajor(t *testing.T) {
	cases := []struct {
		arg        string
		current    string
		candidates []string
		want       string
	}{
		{"GO_VERSION", "1.23", []string{"1.22", "1.24", "2.0"}, "1.24"},
		{"PYTHON_VERSION", "3.12", []string{"3.13", "4.0", "3.13.1"}, "3.13"},
		{"LXML_VERSION", "5.3.0", []string{"5.3.2", "6.0.0"}, "5.3.2"},
		{"PYTHON_DATEUTIL_VERSION", "2.9.0.post0", []string{"2.9.0.post1", "3.0.0.post0"}, "2.9.0.post1"},
	}
	byArg := map[string]pin{}
	for _, p := range pins {
		byArg[p.Arg] = p
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			p, ok := byArg[tc.arg]
			if !ok {
				t.Fatalf("%s is not in the pin table", tc.arg)
			}
			got, err := selectUpdate(tc.current, tc.candidates, p.Policy)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("selectUpdate(%q)=%q, want %q", tc.current, got, tc.want)
			}
		})
	}
}
