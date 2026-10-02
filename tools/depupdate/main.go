// Command depupdate proposes updates for the versions the Dockerfile pins.
//
// Dependabot covers the Go modules and the GitHub Actions, but it cannot read
// this Dockerfile: the image versions are held in build arguments and
// interpolated into the FROM lines, and the pip pins are interpolated into a RUN
// line. This command resolves those by hand, applies only the moves that cannot
// cross a major version or change the Python the collector scripts run on, and
// writes a summary for the pull request body. The workflow that runs it gates
// the pull request on the full test suite, so a proposed version that breaks
// the build never reaches review as a green one.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// pin describes where one build argument's versions come from and how far it may
// move. Go and the Python libraries allow minor updates within their current
// major, which is the widest move that stays non-breaking by the projects' own
// versioning promises; the major component is never touched. Python itself
// holds its minor too, see HeldBecause.
type pin struct {
	Arg    string
	Source string
	Repo   string
	// Suffix is the part of the image tag that follows the version in the
	// Dockerfile, so candidates are drawn from the exact image form the build
	// pulls. It is empty for PyPI packages.
	Suffix string
	Policy policy
	// Product and HeldBecause belong to a pin whose minor is held: what the
	// pull request calls the thing ("Python"), and why a newer minor of it is
	// reported there instead of proposed.
	Product     string
	HeldBecause string
}

// A CPython feature release (3.12 to 3.13) removes standard-library modules and
// changes behaviour, and the scripts that run on the image's interpreter are
// the users' collector scripts, which the suite cannot cover. So that component
// of PYTHON_VERSION moves by hand only, and a newer one is announced in the
// pull request rather than applied.
var pins = []pin{
	{Arg: "GO_VERSION", Source: "docker", Repo: "library/golang", Suffix: "-alpine", Policy: policy{AllowMinor: true}},
	{
		Arg: "PYTHON_VERSION", Source: "docker", Repo: "library/python", Suffix: "-slim",
		Product: "Python", HeldBecause: "collector scripts run on it",
	},
	{Arg: "LXML_VERSION", Source: "pypi", Repo: "lxml", Policy: policy{AllowMinor: true}},
	{Arg: "PYYAML_VERSION", Source: "pypi", Repo: "PyYAML", Policy: policy{AllowMinor: true}},
	{Arg: "PYTHON_DATEUTIL_VERSION", Source: "pypi", Repo: "python-dateutil", Policy: policy{AllowMinor: true}},
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("depupdate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dockerfilePath := flags.String("dockerfile", "Dockerfile", "Dockerfile whose pinned versions are updated")
	summaryPath := flags.String("summary", "", "write a Markdown summary of the updates to this file")
	dryRun := flags.Bool("dry-run", false, "report the available updates without rewriting the Dockerfile")
	timeout := flags.Duration("timeout", 5*time.Minute, "overall time budget for resolving versions")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	info, err := os.Stat(*dockerfilePath)
	if err != nil {
		fmt.Fprintf(stderr, "depupdate: %v\n", err)
		return 1
	}
	raw, err := os.ReadFile(*dockerfilePath)
	if err != nil {
		fmt.Fprintf(stderr, "depupdate: %v\n", err)
		return 1
	}
	current := parseArgDefaults(string(raw))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	updates, held, err := resolve(ctx, current)
	if err != nil {
		fmt.Fprintf(stderr, "depupdate: %v\n", err)
		return 1
	}
	// A held release is always worth a line in the run's log, but it is not an
	// update: on its own it writes no summary and leaves the Dockerfile as it
	// is, so the workflow, which goes by the Dockerfile's diff, opens no pull
	// request for it.
	for _, h := range held {
		fmt.Fprintf(stdout, "depupdate: %s\n", h.sentence())
	}
	if len(updates) == 0 {
		if len(held) > 0 {
			fmt.Fprintln(stdout, "depupdate: nothing else is newer, so there is nothing to propose")
		} else {
			fmt.Fprintln(stdout, "depupdate: every pinned version is already current")
		}
		return 0
	}
	for _, u := range updates {
		fmt.Fprintf(stdout, "depupdate: %s %s -> %s\n", u.Arg, u.Previous, u.Next)
	}

	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, []byte(summaryMarkdown(updates, held)), 0o600); err != nil {
			fmt.Fprintf(stderr, "depupdate: %v\n", err)
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
		fmt.Fprintf(stderr, "depupdate: %v\n", err)
		return 1
	}
	// The Dockerfile keeps the mode it had; this rewrites a tracked file in a
	// working tree rather than creating one.
	if err := os.WriteFile(*dockerfilePath, []byte(rewritten), info.Mode().Perm()); err != nil {
		fmt.Fprintf(stderr, "depupdate: %v\n", err)
		return 1
	}
	return 0
}

// resolve asks each source for its versions and keeps the ones the policy
// allows. A pin the Dockerfile no longer declares is skipped rather than
// treated as an error, so removing a dependency does not break this command;
// an unresolvable source is an error, because silently proposing nothing would
// look identical to being up to date. Beside the updates it returns the newer
// releases a held minor keeps a pin from, which are reported and never applied.
func resolve(ctx context.Context, current map[string]string) ([]update, []heldRelease, error) {
	var updates []update
	var held []heldRelease
	for _, p := range pins {
		pinned, ok := current[p.Arg]
		if !ok {
			continue
		}
		candidates, err := candidatesFor(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p.Arg, err)
		}
		next, err := selectUpdate(pinned, candidates, p.Policy)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p.Arg, err)
		}
		if next != "" {
			updates = append(updates, update{Arg: p.Arg, Source: sourceLabel(p), Previous: pinned, Next: next})
		}
		if available := heldMinor(pinned, candidates, p.Policy); available != "" {
			held = append(held, heldRelease{
				Arg: p.Arg, Product: p.Product, Current: minorOf(pinned), Available: available, Because: p.HeldBecause,
			})
		}
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Arg < updates[j].Arg })
	sort.Slice(held, func(i, j int) bool { return held[i].Arg < held[j].Arg })
	return updates, held, nil
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
