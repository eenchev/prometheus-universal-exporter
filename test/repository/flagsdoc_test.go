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

// The flag reference, docs/FLAGS.md, lists every flag the exporter has, and
// nothing it does not. The README links it rather than carrying a second
// table, since a subject has one home: a table in each would drift apart.
func TestFlagReferenceListsEveryFlag(t *testing.T) {
	help := helpText(t)
	var flags []string
	for _, match := range regexp.MustCompile(`(?m)^  -([a-z.-]+)`).FindAllStringSubmatch(help, -1) {
		flags = append(flags, match[1])
	}
	flagRow := regexp.MustCompile("(?m)^\\| `--([a-z.-]+)`")
	raw, err := os.ReadFile("docs/FLAGS.md")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, match := range flagRow.FindAllStringSubmatch(string(raw), -1) {
		documented = append(documented, match[1])
	}
	sort.Strings(flags)
	sort.Strings(documented)
	if len(flags) == 0 || strings.Join(flags, " ") != strings.Join(documented, " ") {
		t.Fatalf("flags:      %v\ndocumented: %v", flags, documented)
	}
	if rows := flagRow.FindAllString(read(t, "README.md"), -1); len(rows) > 0 {
		t.Errorf("README.md has flag rows %q; flags belong in docs/FLAGS.md, which the README links", rows)
	}
}

// No document names an exporter flag the exporter does not have, such as a
// --log.format for logs that are always JSON: a reader would pass it and get
// "flag provided but not defined" instead of a running exporter. Flags of the
// exporter's own prefixes are checked, which leaves helm's and go's out. A
// flag of the target allocator, cmd/pue-allocator, is one too.
func TestDocsNameOnlyFlagsTheExporterHas(t *testing.T) {
	flags := map[string]bool{}
	allocator, err := allocatorHelp()
	if err != nil {
		t.Fatal(err)
	}
	for _, help := range []string{helpText(t), allocator} {
		for _, match := range regexp.MustCompile(`(?m)^  -([a-z.-]+)`).FindAllStringSubmatch(help, -1) {
			flags["--"+match[1]] = true
		}
	}
	exporterFlag := regexp.MustCompile(`--(?:web|log|config|probe|python|runtime|static-targets)[.-][a-z][a-z.-]*[a-z]`)
	err = filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
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
