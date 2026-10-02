package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The Dockerfile declares every pinned version as a build argument with a
// default, and interpolates the defaults into the FROM lines and the pip
// install. The later stage repeats some of those names without a default so the
// values cross the stage boundary; those lines carry no version and must be left
// alone, which is why only `ARG NAME=value` is matched here.
var argDefault = regexp.MustCompile(`(?m)^ARG[ \t]+([A-Za-z_][A-Za-z0-9_]*)=([^\s#]+)[ \t]*$`)

// parseArgDefaults returns the pinned value of every build argument that
// declares one.
func parseArgDefaults(dockerfile string) map[string]string {
	pins := map[string]string{}
	for _, match := range argDefault.FindAllStringSubmatch(dockerfile, -1) {
		pins[match[1]] = match[2]
	}
	return pins
}

// rewriteArgDefaults replaces the default of each named build argument. It fails
// rather than guessing when an argument is missing or declared with a default
// more than once, because a silent no-op would let the workflow open a pull
// request that claims an update it did not make.
func rewriteArgDefaults(dockerfile string, updates map[string]string) (string, error) {
	for _, name := range sortedNames(updates) {
		value := updates[name]
		line := regexp.MustCompile(`(?m)^ARG[ \t]+` + regexp.QuoteMeta(name) + `=[^\s#]+[ \t]*$`)
		found := line.FindAllString(dockerfile, -1)
		if len(found) == 0 {
			return "", fmt.Errorf("no `ARG %s=` line to update", name)
		}
		if len(found) > 1 {
			return "", fmt.Errorf("`ARG %s=` is declared %d times, refusing to guess which to update", name, len(found))
		}
		dockerfile = line.ReplaceAllLiteralString(dockerfile, "ARG "+name+"="+value)
	}
	return dockerfile, nil
}

// dockerTagCandidates keeps only the tags that carry the exact suffix the
// Dockerfile uses, and returns them with that suffix removed. Filtering on the
// full tag is what guarantees the proposed version exists in the image form the
// build actually pulls: `golang:1.24-alpine` being published is a different fact
// from Go 1.24 being released.
func dockerTagCandidates(tags []string, suffix string) []string {
	candidates := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !strings.HasSuffix(tag, suffix) {
			continue
		}
		trimmed := strings.TrimSuffix(tag, suffix)
		if _, ok := parseVersion(trimmed); !ok {
			continue
		}
		candidates = append(candidates, trimmed)
	}
	return candidates
}

// pypiFile is the part of a PyPI release file entry this tool reads.
type pypiFile struct {
	Yanked bool `json:"yanked"`
}

// pypiCandidates drops releases that have no files left, and releases whose
// every file has been yanked: pip will not install those, so proposing one
// would produce a pull request whose image cannot build.
func pypiCandidates(releases map[string][]pypiFile) []string {
	candidates := make([]string, 0, len(releases))
	for version, files := range releases {
		if len(files) == 0 {
			continue
		}
		live := false
		for _, file := range files {
			if !file.Yanked {
				live = true
				break
			}
		}
		if !live {
			continue
		}
		candidates = append(candidates, version)
	}
	sort.Strings(candidates)
	return candidates
}

// update is one proposed version change.
type update struct {
	Arg      string
	Source   string
	Previous string
	Next     string
}

// heldRelease is a newer minor release that exists and is not proposed, because
// the pin's policy holds the minor.
type heldRelease struct {
	Arg       string
	Product   string
	Current   string
	Available string
	Because   string
}

// sentence is what the run's log and the pull request say about a held release.
func (h heldRelease) sentence() string {
	return fmt.Sprintf("%s %s is available; %s stays at %s until it is bumped by hand, because %s",
		h.Product, h.Available, h.Arg, h.Current, h.Because)
}

// summaryMarkdown renders the updates for a pull request body. The empty string
// means nothing moved, which the caller treats as "do not open a pull request":
// a held release is a note in a summary that is written anyway, never a reason
// to write one.
func summaryMarkdown(updates []update, held []heldRelease) string {
	if len(updates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Automated update of the versions pinned in the Dockerfile.\n\n")
	b.WriteString("| Build argument | From | To | Source |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, u := range updates {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n", u.Arg, u.Previous, u.Next, u.Source)
	}
	b.WriteString("\nGo and the Python libraries move by minor and patch versions, and Python only within " +
		"its feature release (the `3.x` of `PYTHON_VERSION`), which collector scripts run on; " +
		"a major bump is never proposed automatically. " +
		"The full suite ran against these versions before this pull request was opened.\n")
	for _, h := range held {
		fmt.Fprintf(&b, "\n%s.\n", h.sentence())
	}
	return b.String()
}

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
