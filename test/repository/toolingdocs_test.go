package repository

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The release builds archives for macOS, which has no sha256sum: its shasum
// reads the same checksums file. The release page gives the check for both,
// on the same file.
func TestReleaseDocsGiveTheChecksumCommandForLinuxAndMacOS(t *testing.T) {
	workflow := read(t, ".github/workflows/release.yml")
	if !strings.Contains(workflow, "darwin/") {
		t.Skip("the release builds no darwin archive, so nobody checks one on macOS")
	}
	page := read(t, "docs/RELEASING.md")
	linux := regexp.MustCompile(`(?m)^sha256sum --ignore-missing -c (\S+)$`).FindStringSubmatch(page)
	if linux == nil {
		t.Fatal("docs/RELEASING.md no longer gives the sha256sum check of a downloaded archive")
	}
	if want := "\nshasum -a 256 --ignore-missing -c " + linux[1] + "\n"; !strings.Contains(page, want) {
		t.Errorf("docs/RELEASING.md gives the check with sha256sum only, which macOS does not have; want also %q", strings.TrimSpace(want))
	}
}

// The dependency page says what the update workflow's pull request needs and
// what it does not get: the repository setting without which the last step
// fails, by its name and with the error, the branch that step leaves behind,
// and that CI does not run on a pull request the workflow's token opened until
// it is closed and reopened.
func TestDependencyDocsExplainTheUpdatePullRequest(t *testing.T) {
	page := read(t, "docs/DEPENDENCIES.md")
	_, section, found := strings.Cut(page, "\n### The pull request\n")
	if !found {
		t.Fatal(`docs/DEPENDENCIES.md has no "The pull request" section`)
	}
	if next := regexp.MustCompile(`(?m)^#{1,3} `).FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}
	text := strings.Join(strings.Fields(section), " ")
	branch := regexp.MustCompile(`(?m)^\s+branch: (\S+)$`).FindStringSubmatch(read(t, ".github/workflows/update-docker-deps.yml"))
	if branch == nil {
		t.Fatal("update-docker-deps.yml no longer names the pull request's branch")
	}
	for _, want := range []string{
		"`GITHUB_TOKEN`",
		"Settings → Actions → General → Workflow permissions",
		`"Allow GitHub Actions to create and approve pull requests"`,
		"`GitHub Actions is not permitted to create or approve pull requests`",
		"`" + branch[1] + "` branch",
		"does not start `ci.yml`",
		"`make ci` and the image build",
		"close it and reopen it",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the section does not say %s:\n%s", want, section)
		}
	}
}

// PYTHON_VERSION moves by hand, so the pages that name the image's Python have
// to be moved by the same hand: wherever one writes the base image out as
// python:<version>-slim, the version is the Dockerfile's.
func TestDocsNameThePythonTheImageShips(t *testing.T) {
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check against: %v", err)
	}
	pinned := regexp.MustCompile(`(?m)^ARG PYTHON_VERSION=(\S+)$`).FindSubmatch(dockerfile)
	if pinned == nil {
		t.Fatal("the Dockerfile no longer pins PYTHON_VERSION")
	}
	image := regexp.MustCompile("python:([0-9][0-9.]*)-slim")
	found := 0
	for _, path := range append(markdownFiles(t), "charts/prometheus-universal-exporter/values.yaml") {
		for _, match := range image.FindAllStringSubmatch(read(t, path), -1) {
			found++
			if match[1] != string(pinned[1]) {
				t.Errorf("%s says the image is based on python:%s-slim, but the Dockerfile pins PYTHON_VERSION=%s", path, match[1], pinned[1])
			}
		}
	}
	t.Logf("%d mentions of the base image checked", found)
}
