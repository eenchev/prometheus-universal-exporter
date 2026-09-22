package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The README's command-line table lists every flag the exporter has, and
// nothing it does not.
func TestReadmeListsEveryFlag(t *testing.T) {
	help := runCLI(t, "-h")
	var flags []string
	for _, match := range regexp.MustCompile(`(?m)^  -([a-z.-]+)`).FindAllStringSubmatch(help.stdout, -1) {
		flags = append(flags, match[1])
	}
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, match := range regexp.MustCompile("(?m)^\\| `--([a-z.-]+)`").FindAllStringSubmatch(string(raw), -1) {
		documented = append(documented, match[1])
	}
	sort.Strings(flags)
	sort.Strings(documented)
	if len(flags) == 0 || strings.Join(flags, " ") != strings.Join(documented, " ") {
		t.Fatalf("flags:      %v\ndocumented: %v", flags, documented)
	}
}
