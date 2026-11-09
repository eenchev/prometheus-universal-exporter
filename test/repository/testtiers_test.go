package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The quick tier is the developer's alone: `make test-quick` runs go test
// with -short, which tier.Slow skips the slow tests under, and nothing that
// gates a change does. `make test`, every workflow and every script of
// tools run without -short, in go test's flags or in GOFLAGS, so every slow
// test runs in CI; tier.Slow skips only under -short.
func TestOnlyTheQuickTierLeavesTheSlowTestsOut(t *testing.T) {
	makefile := read(t, "Makefile")
	quick := regexp.MustCompile(`(?m)^test-quick:\n\tgo test -short `).FindString(makefile)
	if quick == "" {
		t.Fatal("the Makefile has no test-quick target running go test -short")
	}
	short := regexp.MustCompile(`(^|[\s'"=])-(test\.)?short\b`)
	paths := []string{"Makefile"}
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml", "tools/*.sh"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, matches...)
	}
	if len(paths) < 3 {
		t.Fatalf("found only %v; the workflows have moved", paths)
	}
	for _, path := range paths {
		text := read(t, path)
		if path == "Makefile" {
			text = strings.Replace(text, quick, "", 1)
		}
		if found := short.FindString(text); found != "" {
			t.Errorf("%s runs %q: only make test-quick may leave the slow tests out", path, found)
		}
	}
	tier := read(t, "internal/testutil/tier/tier.go")
	if !strings.Contains(tier, "if testing.Short() {\n\t\tt.Skip(") {
		t.Fatal("tier.Slow skips on something other than -short")
	}
	// tier.Slow is a test's first line, so a slow test does none of its
	// work in the quick tier before it is skipped.
	marked := 0
	call := regexp.MustCompile(`\n\t+tier\.Slow\(`)
	first := regexp.MustCompile(`\nfunc Test\w+\((\w+) \*testing\.T\) \{\n\ttier\.Slow\((\w+)\)\n`)
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && (entry.Name() == "vendor" || entry.Name() == "bin" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		text := read(t, path)
		calls := len(call.FindAllString(text, -1))
		firsts := first.FindAllStringSubmatch(text, -1)
		for _, m := range firsts {
			if m[1] != m[2] {
				t.Errorf("%s: tier.Slow(%s) in a test of %s", path, m[2], m[1])
			}
		}
		if calls != len(firsts) {
			t.Errorf("%s calls tier.Slow %d times, %d of them as a test's first line; make it the first", path, calls, len(firsts))
		}
		marked += calls
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if marked < 50 {
		t.Fatalf("%d tests are marked slow; the tiers have moved", marked)
	}
}
