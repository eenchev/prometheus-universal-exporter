package repository

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The README's command-line table lists every flag the exporter has, and
// nothing it does not.
func TestReadmeListsEveryFlag(t *testing.T) {
	help := helpText(t)
	var flags []string
	for _, match := range regexp.MustCompile(`(?m)^  -([a-z.-]+)`).FindAllStringSubmatch(help, -1) {
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

// No document names an exporter flag the exporter does not have, such as a
// --log.format for logs that are always JSON: a reader would pass it and get
// "flag provided but not defined" instead of a running exporter. Flags of the
// exporter's own prefixes are checked, which leaves helm's and go's out.
func TestDocsNameOnlyFlagsTheExporterHas(t *testing.T) {
	flags := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^  -([a-z.-]+)`).FindAllStringSubmatch(helpText(t), -1) {
		flags["--"+match[1]] = true
	}
	exporterFlag := regexp.MustCompile(`--(?:web|log|config|probe|python|runtime|static-targets)[.-][a-z][a-z.-]*[a-z]`)
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- a file of the repository
		if err != nil {
			return err
		}
		for _, flag := range exporterFlag.FindAllString(string(raw), -1) {
			if !flags[flag] {
				t.Errorf("%s names %s, which the exporter does not have", path, flag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
