package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSources serves everything a run asks for: the registry's pull token and
// the tags of each image, and PyPI's release index of each package, all of it
// from one server the three endpoints are pointed at.
func stubSources(t *testing.T, tags map[string][]string, releases map[string][]string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"stub-token"}`))
			return
		}
		if repo, ok := strings.CutPrefix(r.URL.Path, "/v2/"); ok {
			repo = strings.TrimSuffix(repo, "/tags/list")
			_, _ = w.Write([]byte(`{"tags":["` + strings.Join(tags[repo], `","`) + `"]}`))
			return
		}
		if pkg, ok := strings.CutPrefix(r.URL.Path, "/pypi/"); ok {
			pkg = strings.TrimSuffix(pkg, "/json")
			var entries []string
			for _, version := range releases[pkg] {
				entries = append(entries, `"`+version+`":[{"yanked":false}]`)
			}
			_, _ = w.Write([]byte(`{"releases":{` + strings.Join(entries, ",") + `}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	dockerAuthBase, registryBase, pypiBase = server.URL, server.URL, server.URL
	t.Cleanup(func() {
		dockerAuthBase, registryBase, pypiBase = "https://auth.docker.io", "https://registry-1.docker.io", "https://pypi.org"
	})
}

// The pins of the real Dockerfile, at the versions the stubbed sources below
// call current unless a test says otherwise.
const pinnedDockerfile = `ARG GO_VERSION=1.26
ARG PYTHON_VERSION=3.12
ARG LXML_VERSION=6.1.3
ARG PYYAML_VERSION=6.0.2
ARG PYTHON_DATEUTIL_VERSION=2.9.0.post0

FROM golang:${GO_VERSION}-alpine AS build
FROM python:${PYTHON_VERSION}-slim
`

var currentReleases = map[string][]string{
	"lxml": {"6.1.3"}, "PyYAML": {"6.0.2"}, "python-dateutil": {"2.9.0.post0"},
}

// runIn runs the command on a Dockerfile of its own, with a summary path beside
// it, and returns both paths with what the run printed.
func runIn(t *testing.T, dockerfile string, extra ...string) (dockerfilePath, summaryPath, stdout string) {
	t.Helper()
	dir := t.TempDir()
	dockerfilePath = filepath.Join(dir, "Dockerfile")
	summaryPath = filepath.Join(dir, "summary.md")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := append([]string{"--dockerfile", dockerfilePath, "--summary", summaryPath}, extra...)
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("depupdate exited %d: %s", code, errOut.String())
	}
	return dockerfilePath, summaryPath, out.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Users' collector scripts run on the image's Python, and a feature release
// removes standard-library modules and changes behaviour the suite cannot see
// in scripts it does not have. So a run that finds Python 3.13 and 3.14 leaves
// PYTHON_VERSION alone, moves what else is newer, and says in the pull request
// that a newer Python exists and how it gets in.
func TestANewerPythonFeatureReleaseIsAnnouncedAndNotApplied(t *testing.T) {
	stubSources(t, map[string][]string{
		"library/golang": {"1.26-alpine", "1.27-alpine", "1.27.1-alpine"},
		"library/python": {"3.12-slim", "3.13-slim", "3.14-slim", "3.14.2-slim", "3.15.0rc1-slim", "3.14-alpine", "latest"},
	}, map[string][]string{
		"lxml": {"6.1.3", "6.2.0"}, "PyYAML": {"6.0.2"}, "python-dateutil": {"2.9.0.post0"},
	})
	dockerfilePath, summaryPath, stdout := runIn(t, pinnedDockerfile)

	rewritten := readFile(t, dockerfilePath)
	for _, want := range []string{"ARG GO_VERSION=1.27\n", "ARG PYTHON_VERSION=3.12\n", "ARG LXML_VERSION=6.2.0\n"} {
		if !strings.Contains(rewritten, want) {
			t.Errorf("the rewritten Dockerfile is missing %q:\n%s", want, rewritten)
		}
	}
	const note = "Python 3.14 is available; PYTHON_VERSION stays at 3.12 until it is bumped by hand, because collector scripts run on it"
	summary := readFile(t, summaryPath)
	if !strings.Contains(summary, "\n"+note+".\n") {
		t.Errorf("the pull request body does not announce the newer Python:\n%s", summary)
	}
	if strings.Contains(summary, "| `PYTHON_VERSION`") {
		t.Errorf("the pull request body lists PYTHON_VERSION among the updates:\n%s", summary)
	}
	for _, want := range []string{"| `GO_VERSION` | `1.26` | `1.27` |", "| `LXML_VERSION` | `6.1.3` | `6.2.0` |"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the pull request body is missing the row %q:\n%s", want, summary)
		}
	}
	if !strings.Contains(stdout, "depupdate: "+note+"\n") {
		t.Errorf("the run's log does not announce the newer Python:\n%s", stdout)
	}
}

// The workflow opens a pull request when the Dockerfile changed. A newer Python
// with nothing else to move changes nothing, writes no pull request body, and
// is still a line in the run's log for whoever looks.
func TestANewerPythonAloneProposesNothing(t *testing.T) {
	stubSources(t, map[string][]string{
		"library/golang": {"1.25-alpine", "1.26-alpine"},
		"library/python": {"3.12-slim", "3.13-slim", "3.14-slim"},
	}, currentReleases)
	dockerfilePath, summaryPath, stdout := runIn(t, pinnedDockerfile)

	if got := readFile(t, dockerfilePath); got != pinnedDockerfile {
		t.Errorf("the Dockerfile changed though nothing may move:\n%s", got)
	}
	if _, err := os.Stat(summaryPath); !os.IsNotExist(err) {
		t.Errorf("a pull request body was written for a newer Python alone (stat: %v)", err)
	}
	if !strings.Contains(stdout, "Python 3.14 is available; PYTHON_VERSION stays at 3.12") {
		t.Errorf("the run's log does not mention the newer Python:\n%s", stdout)
	}
	if strings.Contains(stdout, "every pinned version is already current") {
		t.Errorf("the run's log calls every pin current beside a newer Python:\n%s", stdout)
	}
}

// With every source at the pinned versions the run says so and proposes
// nothing, as it did before Python was held.
func TestNothingNewerIsReportedAsCurrent(t *testing.T) {
	stubSources(t, map[string][]string{
		"library/golang": {"1.26-alpine"},
		"library/python": {"3.11-slim", "3.12-slim", "3.12.9-slim"},
	}, currentReleases)
	dockerfilePath, summaryPath, stdout := runIn(t, pinnedDockerfile)

	if got := readFile(t, dockerfilePath); got != pinnedDockerfile {
		t.Errorf("the Dockerfile changed though nothing is newer:\n%s", got)
	}
	if _, err := os.Stat(summaryPath); !os.IsNotExist(err) {
		t.Errorf("a pull request body was written though nothing is newer (stat: %v)", err)
	}
	if stdout != "depupdate: every pinned version is already current\n" {
		t.Errorf("stdout=%q", stdout)
	}
}

// A PYTHON_VERSION pinned to a patch release still gets that feature release's
// fixes: 3.12.4 moves to the newest 3.12.x and never to 3.13 or 3.14, which the
// pull request mentions beside the move.
func TestAPatchPinnedPythonMovesOnlyWithinItsFeatureRelease(t *testing.T) {
	stubSources(t, map[string][]string{
		"library/golang": {"1.26-alpine"},
		"library/python": {"3.12-slim", "3.12.4-slim", "3.12.9-slim", "3.13.7-slim", "3.14.1-slim", "3.14-slim"},
	}, currentReleases)
	patchPinned := strings.Replace(pinnedDockerfile, "ARG PYTHON_VERSION=3.12\n", "ARG PYTHON_VERSION=3.12.4\n", 1)
	dockerfilePath, summaryPath, _ := runIn(t, patchPinned)

	if got := readFile(t, dockerfilePath); !strings.Contains(got, "ARG PYTHON_VERSION=3.12.9\n") {
		t.Errorf("PYTHON_VERSION did not move to the newest 3.12 patch release:\n%s", got)
	}
	summary := readFile(t, summaryPath)
	if !strings.Contains(summary, "| `PYTHON_VERSION` | `3.12.4` | `3.12.9` |") {
		t.Errorf("the pull request body is missing the patch move:\n%s", summary)
	}
	if !strings.Contains(summary, "Python 3.14 is available; PYTHON_VERSION stays at 3.12 until it is bumped by hand") {
		t.Errorf("the pull request body does not announce the newer Python:\n%s", summary)
	}
}

// --dry-run reports what a run would do and touches nothing but the summary it
// was asked for.
func TestADryRunLeavesTheDockerfileAlone(t *testing.T) {
	stubSources(t, map[string][]string{
		"library/golang": {"1.26-alpine", "1.27-alpine"},
		"library/python": {"3.12-slim"},
	}, currentReleases)
	dockerfilePath, summaryPath, stdout := runIn(t, pinnedDockerfile, "--dry-run")

	if got := readFile(t, dockerfilePath); got != pinnedDockerfile {
		t.Errorf("a dry run rewrote the Dockerfile:\n%s", got)
	}
	if !strings.Contains(stdout, "depupdate: GO_VERSION 1.26 -> 1.27\n") {
		t.Errorf("stdout=%q", stdout)
	}
	if summary := readFile(t, summaryPath); !strings.Contains(summary, "`GO_VERSION`") || strings.Contains(summary, "is available") {
		t.Errorf("unexpected pull request body:\n%s", summary)
	}
}
