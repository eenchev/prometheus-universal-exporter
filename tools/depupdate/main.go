// Command depupdate proposes updates for the versions the Dockerfile pins.
//
// Dependabot covers the Go modules and the GitHub Actions, but it cannot read
// this Dockerfile: the image versions are held in build arguments and
// interpolated into the FROM lines, and the pip pins are interpolated into a RUN
// line. This command resolves those by hand, applies only the moves that cannot
// cross a major version, and writes a summary for the pull request body. The
// workflow that runs it gates the pull request on the full test suite, so a
// proposed version that breaks the build never reaches review as a green one.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// pin describes where one build argument's versions come from and how far it may
// move. Every pin here allows minor updates within its current major, which is
// the widest move that stays non-breaking by the projects' own versioning
// promises; the major component is never touched.
type pin struct {
	Arg    string
	Source string
	Repo   string
	// Suffix is the part of the image tag that follows the version in the
	// Dockerfile, so candidates are drawn from the exact image form the build
	// pulls. It is empty for PyPI packages.
	Suffix string
	Policy policy
}

var pins = []pin{
	{Arg: "GO_VERSION", Source: "docker", Repo: "library/golang", Suffix: "-alpine", Policy: policy{AllowMinor: true}},
	{Arg: "PYTHON_VERSION", Source: "docker", Repo: "library/python", Suffix: "-slim", Policy: policy{AllowMinor: true}},
	{Arg: "LXML_VERSION", Source: "pypi", Repo: "lxml", Policy: policy{AllowMinor: true}},
	{Arg: "PYYAML_VERSION", Source: "pypi", Repo: "PyYAML", Policy: policy{AllowMinor: true}},
	{Arg: "PYTHON_DATEUTIL_VERSION", Source: "pypi", Repo: "python-dateutil", Policy: policy{AllowMinor: true}},
}

func main() { os.Exit(run()) }

func run() int {
	dockerfilePath := flag.String("dockerfile", "Dockerfile", "Dockerfile whose pinned versions are updated")
	summaryPath := flag.String("summary", "", "write a Markdown summary of the updates to this file")
	dryRun := flag.Bool("dry-run", false, "report the available updates without rewriting the Dockerfile")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall time budget for resolving versions")
	flag.Parse()

	info, err := os.Stat(*dockerfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "depupdate: %v\n", err)
		return 1
	}
	raw, err := os.ReadFile(*dockerfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "depupdate: %v\n", err)
		return 1
	}
	current := parseArgDefaults(string(raw))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	updates, err := resolve(ctx, current)
	if err != nil {
		fmt.Fprintf(os.Stderr, "depupdate: %v\n", err)
		return 1
	}
	if len(updates) == 0 {
		fmt.Println("depupdate: every pinned version is already current")
		return 0
	}
	for _, u := range updates {
		fmt.Printf("depupdate: %s %s -> %s\n", u.Arg, u.Previous, u.Next)
	}

	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, []byte(summaryMarkdown(updates)), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "depupdate: %v\n", err)
			return 1
		}
	}
	if *dryRun {
		return 0
	}

	changes := map[string]string{}
	for _, u := range updates {
		changes[u.Arg] = u.Next
	}
	rewritten, err := rewriteArgDefaults(string(raw), changes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "depupdate: %v\n", err)
		return 1
	}
	// The Dockerfile keeps the mode it had; this rewrites a tracked file in a
	// working tree rather than creating one.
	if err := os.WriteFile(*dockerfilePath, []byte(rewritten), info.Mode().Perm()); err != nil {
		fmt.Fprintf(os.Stderr, "depupdate: %v\n", err)
		return 1
	}
	return 0
}

// resolve asks each source for its versions and keeps the ones the policy
// allows. A pin the Dockerfile no longer declares is skipped rather than
// treated as an error, so removing a dependency does not break this command;
// an unresolvable source is an error, because silently proposing nothing would
// look identical to being up to date.
func resolve(ctx context.Context, current map[string]string) ([]update, error) {
	var updates []update
	for _, p := range pins {
		pinned, ok := current[p.Arg]
		if !ok {
			continue
		}
		candidates, err := candidatesFor(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Arg, err)
		}
		next, err := selectUpdate(pinned, candidates, p.Policy)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Arg, err)
		}
		if next == "" {
			continue
		}
		updates = append(updates, update{Arg: p.Arg, Source: sourceLabel(p), Previous: pinned, Next: next})
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Arg < updates[j].Arg })
	return updates, nil
}

func candidatesFor(ctx context.Context, p pin) ([]string, error) {
	switch p.Source {
	case "docker":
		tags, err := dockerTags(ctx, p.Repo)
		if err != nil {
			return nil, err
		}
		return dockerTagCandidates(tags, p.Suffix), nil
	case "pypi":
		return pypiVersions(ctx, p.Repo)
	default:
		return nil, fmt.Errorf("unknown source %q", p.Source)
	}
}

func sourceLabel(p pin) string {
	if p.Source == "docker" {
		return "docker.io/" + p.Repo + " (`" + strings.TrimPrefix(p.Suffix, "-") + "`)"
	}
	return "PyPI " + p.Repo
}
