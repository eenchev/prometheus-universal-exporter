package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// exampleConfigBuild is the build constraint of a test that loads
// configs/config.example.yaml, whose collectors are http and localfile
// ones: a build without either type cannot load it.
const exampleConfigBuild = "//go:build !select_request_types || (request_type_http && request_type_localfile)"

// A test of this package that loads configs/config.example.yaml is built
// only where the example's request types are: one built without the
// constraint failed in every build of a single type, which CI vets and
// builds but does not test, and so failed for no one but a developer.
func TestTheTestsThatLoadTheExampleConfigNeedItsRequestTypes(t *testing.T) {
	loads := regexp.MustCompile(`config\.Load\(\s*"configs/config\.example\.yaml"\s*\)`)
	files, err := filepath.Glob("test/repository/*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no tests found: %v", err)
	}
	found := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !loads.MatchString(text) {
			continue
		}
		found++
		if first, _, _ := strings.Cut(text, "\n"); first != exampleConfigBuild {
			t.Errorf("%s loads configs/config.example.yaml but is built as %q; begin it with\n%s", file, first, exampleConfigBuild)
		}
	}
	if found < 2 {
		t.Errorf("%d tests load configs/config.example.yaml; the pattern no longer finds them", found)
	}
}
