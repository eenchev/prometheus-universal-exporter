package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A workflow that installs an older Go than go.mod requires fails on the first
// `go` command with "go.mod requires go >= X", and nothing in the repository
// points at the cause: the go directive is raised by a dependency bump, in a
// pull request that never touches a workflow. This test ties the two together,
// so the mismatch is caught by `go test ./...` rather than by a red build after
// a dependency update lands.
//
// The workflows ask for `stable`, which setup-go resolves to the newest stable
// release, so the build follows Go's releases without anyone editing a pin.
// An explicit version is still allowed, provided it satisfies go.mod.

var (
	goDirective   = regexp.MustCompile(`(?m)^go[ \t]+([0-9]+(?:\.[0-9]+){0,2})[ \t]*$`)
	workflowGoVer = regexp.MustCompile(`(?m)^[ \t]*go-version:[ \t]*['"]?([^'"\s]+)['"]?[ \t]*$`)
)

// parseGoVersion reads a dotted Go version. The trailing `.x` setup-go accepts
// is dropped rather than rejected: `1.25.x` constrains the same minor as 1.25.
func parseGoVersion(raw string) ([]int, bool) {
	raw = strings.TrimSuffix(raw, ".x")
	if raw == "" {
		return nil, false
	}
	var parsed []int
	for _, part := range strings.Split(raw, ".") {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		parsed = append(parsed, number)
	}
	return parsed, true
}

// atLeast reports whether have satisfies want, padding the shorter with zeros
// so 1.25 counts as satisfying 1.25.0.
func atLeast(have, want []int) bool {
	for i := 0; i < len(have) || i < len(want); i++ {
		x, y := 0, 0
		if i < len(have) {
			x = have[i]
		}
		if i < len(want) {
			y = want[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}

func moduleGoVersion(t *testing.T) ([]int, string) {
	t.Helper()
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	match := goDirective.FindSubmatch(raw)
	if match == nil {
		t.Fatal("go.mod has no go directive")
	}
	required, ok := parseGoVersion(string(match[1]))
	if !ok {
		t.Fatalf("go.mod declares an unparseable go directive %q", match[1])
	}
	return required, string(match[1])
}

func TestWorkflowsInstallAGoThatSatisfiesTheModule(t *testing.T) {
	required, declared := moduleGoVersion(t)
	found := 0
	for _, path := range workflowPaths(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range workflowGoVer.FindAllStringSubmatch(string(raw), -1) {
			found++
			requested := match[1]
			// setup-go resolves this to the newest stable release, which
			// always satisfies the module.
			if requested == "stable" {
				continue
			}
			version, ok := parseGoVersion(requested)
			if !ok {
				t.Errorf("%s: go-version %q is neither `stable` nor a version number", filepath.Base(path), requested)
				continue
			}
			if !atLeast(version, required) {
				t.Errorf("%s: go-version %q is older than the go %s that go.mod requires; the build fails before it starts",
					filepath.Base(path), requested, declared)
			}
		}
	}
	if found == 0 {
		t.Fatal("no workflow requests a Go version; this test must not silently pass if the key is renamed")
	}
}

// The comparison is the part worth testing directly: the repository check above
// only exercises whichever versions happen to be committed today.
func TestGoVersionComparison(t *testing.T) {
	tests := []struct {
		have, want string
		satisfied  bool
	}{
		{"1.25", "1.25.0", true},
		{"1.25.x", "1.25.0", true},
		{"1.27", "1.25.0", true},
		{"1.25.3", "1.25.0", true},
		{"1.23", "1.25.0", false},
		{"1.23.x", "1.25.0", false},
		{"1.24.9", "1.25.0", false},
		{"2.0", "1.25.0", true},
		{"1.25.0", "1.25", true},
	}
	for _, test := range tests {
		t.Run(test.have+"_vs_"+test.want, func(t *testing.T) {
			have, ok := parseGoVersion(test.have)
			if !ok {
				t.Fatalf("cannot parse %q", test.have)
			}
			want, ok := parseGoVersion(test.want)
			if !ok {
				t.Fatalf("cannot parse %q", test.want)
			}
			if got := atLeast(have, want); got != test.satisfied {
				t.Fatalf("atLeast(%s, %s)=%v, want %v", test.have, test.want, got, test.satisfied)
			}
		})
	}
	for _, bad := range []string{"stable", "oldstable", "", "1.x.3", "tip"} {
		if _, ok := parseGoVersion(bad); ok {
			t.Errorf("%q should not parse as a version number", bad)
		}
	}
}

// The Dockerfile builds the released image, so it must satisfy the module too.
// It pins a two-component version deliberately: that tag already picks up patch
// rebuilds, and tools/depupdate keeps it moving within the major.
func TestDockerfileGoVersionSatisfiesTheModule(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	match := regexp.MustCompile(`(?m)^ARG[ \t]+GO_VERSION=([^\s#]+)`).FindSubmatch(raw)
	if match == nil {
		t.Fatal("the Dockerfile no longer pins GO_VERSION")
	}
	pinned, ok := parseGoVersion(string(match[1]))
	if !ok {
		t.Fatalf("GO_VERSION=%q is not a version number", match[1])
	}
	required, declared := moduleGoVersion(t)
	if !atLeast(pinned, required) {
		t.Fatalf("the image builds with Go %s but go.mod requires go %s", match[1], declared)
	}
}
