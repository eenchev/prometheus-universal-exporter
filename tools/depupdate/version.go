package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The Dockerfile pins every dependency through an ARG, so the only safe
// automatic move is one that cannot change behaviour in a way the test suite
// would not notice. Two rules encode that:
//
//   - The major version never moves. A major bump is deliberate work.
//   - The pin keeps its granularity. `1.23` is a floating tag that already
//     picks up patch releases on every rebuild, so rewriting it to `1.23.4`
//     would freeze it and make things worse, not better.
type policy struct {
	// AllowMinor lets the minor component move within the same major.
	AllowMinor bool
}

// version is a release version with an optional PEP 440 post-release, which
// python-dateutil uses (2.9.0.post0). Pre-releases are rejected outright.
type version struct {
	release []int
	post    int
	hasPost bool
	raw     string
}

var (
	numericComponent = regexp.MustCompile(`^[0-9]+$`)
	postComponent    = regexp.MustCompile(`^post([0-9]+)$`)
)

// parseVersion accepts dotted numeric versions and an optional trailing
// `.postN`. Anything else — a release candidate, a dev build, a date-stamped
// tag — is refused, so an unstable version can never be proposed.
func parseVersion(raw string) (version, bool) {
	if raw == "" {
		return version{}, false
	}
	parts := strings.Split(raw, ".")
	parsed := version{raw: raw}
	for i, part := range parts {
		if numericComponent.MatchString(part) {
			if parsed.hasPost {
				return version{}, false // numbers after a post segment
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				return version{}, false
			}
			parsed.release = append(parsed.release, n)
			continue
		}
		if match := postComponent.FindStringSubmatch(part); match != nil && i == len(parts)-1 {
			n, err := strconv.Atoi(match[1])
			if err != nil {
				return version{}, false
			}
			parsed.post, parsed.hasPost = n, true
			continue
		}
		return version{}, false
	}
	if len(parsed.release) == 0 {
		return version{}, false
	}
	return parsed, true
}

// compare orders two versions, padding the shorter release with zeros so 1.24
// sorts above 1.23.9.
func compare(a, b version) int {
	for i := 0; i < len(a.release) || i < len(b.release); i++ {
		x, y := 0, 0
		if i < len(a.release) {
			x = a.release[i]
		}
		if i < len(b.release) {
			y = b.release[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if a.post != b.post {
		if a.post < b.post {
			return -1
		}
		return 1
	}
	return 0
}

// selectUpdate returns the highest candidate that the policy permits, or an
// empty string when nothing should move. Candidates must keep the pin's
// granularity and its major, and must be strictly newer than the current pin.
func selectUpdate(current string, candidates []string, p policy) (string, error) {
	currentVersion, ok := parseVersion(current)
	if !ok {
		return "", fmt.Errorf("cannot parse the current pin %q", current)
	}
	best := currentVersion
	for _, candidate := range candidates {
		parsed, ok := parseVersion(candidate)
		if !ok {
			continue
		}
		if len(parsed.release) != len(currentVersion.release) {
			continue // a different pin granularity
		}
		if parsed.release[0] != currentVersion.release[0] {
			continue // a major bump is never automatic
		}
		if !p.AllowMinor && len(parsed.release) > 1 && parsed.release[1] != currentVersion.release[1] {
			continue
		}
		if compare(parsed, best) > 0 {
			best = parsed
		}
	}
	if compare(best, currentVersion) == 0 {
		return "", nil
	}
	return best.raw, nil
}
