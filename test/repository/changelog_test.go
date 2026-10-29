package repository

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A GitHub Release's description is its section of CHANGELOG.md, printed by
// tools/release-notes.sh, rather than the list of merged pull requests
// gh's --generate-notes made, which never told a user what an upgrade asks of
// them. These tests hold the changelog to the shape the script and the
// release documentation rely on, the script to what it prints and refuses,
// and the release workflows to using it.

const (
	changelogPath     = "CHANGELOG.md"
	releaseNotesTool  = "tools/release-notes.sh"
	unreleasedHeading = "## Unreleased"
	whatChanged       = "### What changed for you"
)

// changelogVersionHeading is a released section's heading.
var changelogVersionHeading = regexp.MustCompile(`^## (exporter|chart) ([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// changelogSection is one `## ` section of the changelog.
type changelogSection struct {
	heading string
	line    int
	body    []string
}

// changelogSections splits a changelog into its `## ` sections, and returns
// the lines before the first.
func changelogSections(text string) (preamble []string, sections []changelogSection) {
	for index, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") || line == "##" {
			sections = append(sections, changelogSection{heading: line, line: index + 1})
			continue
		}
		if len(sections) == 0 {
			preamble = append(preamble, line)
			continue
		}
		sections[len(sections)-1].body = append(sections[len(sections)-1].body, line)
	}
	return preamble, sections
}

// changelogProblems lists what is wrong with a changelog's shape: headings
// other than one leading `## Unreleased` and `## exporter X.Y.Z` or
// `## chart X.Y.Z`, versions of a component not strictly descending, and a
// section that does not begin with What changed for you and a line under
// it, or has subsections other than New and Fixed after it, in that order.
func changelogProblems(text string) []string {
	var problems []string
	preamble, sections := changelogSections(text)
	for index, line := range preamble {
		if strings.HasPrefix(line, "#") && (index != 0 || !strings.HasPrefix(line, "# ")) {
			problems = append(problems, fmt.Sprintf("line %d: %q comes before the first section; only the title, on the first line, may be a heading there", index+1, line))
		}
	}
	if len(sections) == 0 || sections[0].heading != unreleasedHeading {
		problems = append(problems, "the first section is not "+unreleasedHeading)
	}
	last := map[string][3]int{}
	for index, section := range sections {
		if section.heading == unreleasedHeading {
			if index != 0 {
				problems = append(problems, fmt.Sprintf("line %d: %s again, or not first", section.line, unreleasedHeading))
			}
		} else if match := changelogVersionHeading.FindStringSubmatch(section.heading); match == nil {
			problems = append(problems, fmt.Sprintf("line %d: %q is neither %s nor ## exporter X.Y.Z or ## chart X.Y.Z", section.line, section.heading, unreleasedHeading))
		} else {
			var version [3]int
			for part := range version {
				version[part], _ = strconv.Atoi(match[part+2])
			}
			if previous, seen := last[match[1]]; seen && slices.Compare(version[:], previous[:]) >= 0 {
				problems = append(problems, fmt.Sprintf("line %d: %q is not older than the %s section above it, %d.%d.%d", section.line, section.heading, match[1], previous[0], previous[1], previous[2]))
			}
			last[match[1]] = version
		}
		allowed := []string{whatChanged, "### New", "### Fixed"}
		var subsections []string
		said := false
		for _, line := range section.body {
			if strings.HasPrefix(line, "#") {
				subsections = append(subsections, line)
				continue
			}
			if len(subsections) == 0 && strings.TrimSpace(line) != "" {
				problems = append(problems, fmt.Sprintf("%q: %q comes before %s", section.heading, line, whatChanged))
			}
			if len(subsections) == 1 && strings.TrimSpace(line) != "" {
				said = true
			}
		}
		if len(subsections) == 0 || subsections[0] != whatChanged {
			problems = append(problems, fmt.Sprintf("%q does not begin with %s", section.heading, whatChanged))
		} else if !said {
			problems = append(problems, fmt.Sprintf("%q says nothing under %s; write \"Nothing; you can upgrade without changes.\" when there is nothing", section.heading, whatChanged))
		}
		at := 0
		for _, subsection := range subsections {
			found := slices.Index(allowed[at:], subsection)
			if found < 0 {
				problems = append(problems, fmt.Sprintf("%q has the subsection %q, which is not one of %q or is out of that order or repeated", section.heading, subsection, allowed))
				continue
			}
			at += found + 1
		}
	}
	return problems
}

// The changelog has the shape the release notes script and docs/RELEASING.md
// rely on.
func TestTheChangelogHasOnlyReleaseSectionsInOrder(t *testing.T) {
	for _, problem := range changelogProblems(read(t, changelogPath)) {
		t.Error(problem)
	}
}

// The check itself refuses every shape the rules leave out, so a changelog
// that passes it is not one it cannot see into.
func TestTheChangelogCheckRefusesWhatTheRulesLeaveOut(t *testing.T) {
	const good = "# Changelog\n\nintro\n\n## Unreleased\n\n### What changed for you\n\n- Nothing; you can upgrade without changes.\n\n" +
		"## exporter 1.10.0\n\n### What changed for you\n\n- a\n\n### New\n\n- b\n\n### Fixed\n\n- c\n\n" +
		"## chart 1.2.0\n\n### What changed for you\n\n- d\n\n" +
		"## exporter 1.9.1\n\n### What changed for you\n\n- e\n\n### Fixed\n\n- f\n"
	if problems := changelogProblems(good); len(problems) > 0 {
		t.Fatalf("a good changelog was refused: %q", problems)
	}
	for name, text := range map[string]string{
		"no Unreleased":           strings.Replace(good, "## Unreleased\n\n### What changed for you\n\n- Nothing; you can upgrade without changes.\n\n", "", 1),
		"Unreleased twice":        good + "\n## Unreleased\n\n### What changed for you\n\n- x\n",
		"another heading":         strings.Replace(good, "## chart 1.2.0", "## Chart 1.2.0", 1),
		"a v in the version":      strings.Replace(good, "## chart 1.2.0", "## chart v1.2.0", 1),
		"ascending versions":      strings.Replace(good, "## exporter 1.9.1", "## exporter 1.11.0", 1),
		"a version twice":         strings.Replace(good, "## exporter 1.9.1", "## exporter 1.10.0", 1),
		"New first":               strings.Replace(good, "## chart 1.2.0\n\n### What changed for you", "## chart 1.2.0\n\n### New", 1),
		"nothing under it":        strings.Replace(good, "### What changed for you\n\n- d\n", "### What changed for you\n", 1),
		"another subsection":      strings.Replace(good, "### Fixed\n\n- f", "### Security\n\n- f", 1),
		"Fixed before New":        strings.Replace(good, "### New\n\n- b\n\n### Fixed\n\n- c", "### Fixed\n\n- c\n\n### New\n\n- b", 1),
		"text before subsections": strings.Replace(good, "## chart 1.2.0\n\n", "## chart 1.2.0\n\nloose\n\n", 1),
		"a heading in the intro":  strings.Replace(good, "intro", "### intro", 1),
	} {
		if len(changelogProblems(text)) == 0 {
			t.Errorf("%s: the changelog check accepted\n%s", name, text)
		}
	}
}

// The chart version Chart.yaml declares, and the exporter release its
// appVersion deploys, have their sections: a chart is bumped in the commit
// it is released from, together with its section, and deploys an exporter
// already released, so CI fails before a tag would.
func TestTheChangelogHasTheChartsVersions(t *testing.T) {
	chart := readChartMetadata(t)
	_, sections := changelogSections(read(t, changelogPath))
	for _, want := range []string{"## chart " + chart.Version, "## exporter " + chart.AppVersion} {
		if !slices.ContainsFunc(sections, func(section changelogSection) bool { return section.heading == want }) {
			t.Errorf("%s has no %q section, for Chart.yaml's version %s and appVersion %s", changelogPath, want, chart.Version, chart.AppVersion)
		}
	}
}

// releaseNotes runs tools/release-notes.sh on a changelog and returns its
// output and its exit status.
func releaseNotes(t *testing.T, changelog string, args ...string) (stdout, stderr string, status int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{releaseNotesTool}, append(args, changelog)...)...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), errOut.String(), exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), 0
}

// The script prints a release's section without its heading and without the
// next section, and refuses a release whose section is missing or does not
// begin with What changed for you and a line under it, saying which.
func TestTheReleaseNotesScriptPrintsTheReleasesSection(t *testing.T) {
	changelog := filepath.Join(t.TempDir(), "CHANGELOG.md")
	const text = "# Changelog\n\n## Unreleased\n\n### What changed for you\n\n- Nothing; you can upgrade without changes.\n\n" +
		"## exporter 1.9.0\n\n### What changed for you\n\n- A key was renamed.\n\n### New\n\n- A flag.\n\n" +
		"## chart 1.9.0\n\n### What changed for you\n\n- Deploys exporter 1.9.0.\n\n" +
		"## chart 1.8.0\n\n### New\n\n- A value.\n\n" +
		"## exporter 1.8.0\n\n### What changed for you\n\n### New\n\n- Something.\n"
	if err := os.WriteFile(changelog, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ component, version, notes string }{
		{"exporter", "1.9.0", "### What changed for you\n\n- A key was renamed.\n\n### New\n\n- A flag.\n"},
		{"chart", "1.9.0", "### What changed for you\n\n- Deploys exporter 1.9.0.\n"},
	} {
		stdout, stderr, status := releaseNotes(t, changelog, want.component, want.version)
		if status != 0 || stdout != want.notes {
			t.Errorf("%s %s: status %d, printed %q (%s), want %q", want.component, want.version, status, stdout, stderr, want.notes)
		}
	}
	for _, refused := range []struct{ component, version, says string }{
		{"exporter", "1.10.0", `no "## exporter 1.10.0" section`},
		{"chart", "1.7.0", `no "## chart 1.7.0" section`},
		{"chart", "1.8.0", `"## chart 1.8.0" in ` + changelog + ` does not begin with "### What changed for you"`},
		{"exporter", "1.8.0", `"## exporter 1.8.0" in ` + changelog + ` does not begin with "### What changed for you" and a line under it`},
	} {
		stdout, stderr, status := releaseNotes(t, changelog, refused.component, refused.version)
		if status != 1 || stdout != "" || !strings.Contains(stderr, refused.says) {
			t.Errorf("%s %s: status %d, printed %q, said %q; want status 1, nothing printed, and %q", refused.component, refused.version, status, stdout, stderr, refused.says)
		}
	}
	for _, args := range [][]string{{"allocator", "1.9.0"}, {"exporter", "v1.9.0"}, {"exporter", "1.9"}} {
		if stdout, stderr, status := releaseNotes(t, changelog, args...); status != 2 || stdout != "" || stderr == "" {
			t.Errorf("%q: status %d, printed %q, said %q; want status 2 and a message", args, status, stdout, stderr)
		}
	}
}

// The repository's own changelog gives the release notes of the versions
// Chart.yaml names, through the script the workflows run.
func TestTheReleaseNotesScriptReadsTheChangelog(t *testing.T) {
	chart := readChartMetadata(t)
	for _, args := range [][]string{{"chart", chart.Version}, {"exporter", chart.AppVersion}} {
		cmd := exec.Command("sh", append([]string{releaseNotesTool}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("%s %q: %v", releaseNotesTool, args, err)
			continue
		}
		if !strings.HasPrefix(string(out), whatChanged+"\n") {
			t.Errorf("%s %q printed what does not begin with %s:\n%s", releaseNotesTool, args, whatChanged, out)
		}
	}
}

// Both release workflows read the notes with the script, for their own
// component and from the tag's version, before they build or check anything
// else, and hand them to every gh release that writes a description; none
// asks gh to generate them. A rerun of the exporter's sets the existing
// Release's description again.
func TestTheReleaseWorkflowsDescribeTheReleaseFromTheChangelog(t *testing.T) {
	const notes = `--notes-file "$RUNNER_TEMP/release-notes.md"`
	for _, workflow := range []struct {
		path, job, script, firstWork string
		rerun                        bool
	}{
		{
			path:      ".github/workflows/release.yml",
			job:       "release",
			script:    `sh tools/release-notes.sh exporter "${TAG#exporter/prometheus-universal-exporter-v}" > "$RUNNER_TEMP/release-notes.md"`,
			firstWork: "go test ./...",
			rerun:     true,
		},
		{
			path:      ".github/workflows/release-chart.yml",
			job:       "release-chart",
			script:    `sh tools/release-notes.sh chart "$VERSION" > "$RUNNER_TEMP/release-notes.md"`,
			firstWork: "helm lint",
		},
	} {
		if strings.Contains(read(t, workflow.path), "--generate-notes") {
			t.Errorf("%s still has gh generate the release notes", workflow.path)
		}
		steps := workflowSteps(t, workflow.path)[workflow.job]
		reads := stepIndex(steps, workflow.script)
		work := stepIndex(steps, workflow.firstWork)
		release := stepIndex(steps, "gh release create")
		if reads < 0 {
			t.Errorf("%s does not read the release notes with %s", workflow.path, workflow.script)
			continue
		}
		if work < 0 || release < 0 || reads > work {
			t.Errorf("%s reads the release notes in its step %d, not before %s (step %d) and the Release (step %d)", workflow.path, reads+1, workflow.firstWork, work+1, release+1)
			continue
		}
		lines := slices.Collect(strings.Lines(steps[release]["run"].(string)))
		create := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, "gh release create") })
		if !strings.Contains(lines[create], notes) {
			t.Errorf("%s does not give gh release create the notes with %s:\n%s", workflow.path, notes, lines[create])
		}
		if workflow.rerun {
			// The rerun branch: the Release exists, and its description is
			// set again beside its assets.
			upload := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, "gh release upload") })
			if upload < 0 || upload+1 >= len(lines) || !strings.Contains(lines[upload+1], `gh release edit "${GITHUB_REF_NAME}" `+notes) {
				t.Errorf("%s does not set an existing Release's description again where it replaces its assets:\n%s", workflow.path, strings.Join(lines, ""))
			}
		}
	}
}
