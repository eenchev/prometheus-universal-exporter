package repository

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// make new-request-type writes a type that compiles and passes its tests
// before it does anything: its Fetch fails as not implemented, and every
// place left to a person says TODO(<name>), in its code, its page, the
// lists that name the types and the tables of the tests. Those words read
// as the rest of the page does, so one can be committed with the type
// unnoticed; this test fails while any is left for a type of the tree.

// scaffoldPlaceholders are what the scaffold leaves in the tree for the
// request type name, as path: the placeholder, for each file under root
// that holds one: TODO(<name>) anywhere, and the type's not-implemented
// failure in its requesttype_<name>.go. The directories git and Go keep
// for themselves are not read.
func scaffoldPlaceholders(root string, names []string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && slices.Contains([]string{".git", "vendor", "bin"}, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path) // #nosec G304,G122 -- the tree's own files
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, name := range names {
			if placeholder := "TODO(" + name + ")"; bytes.Contains(raw, []byte(placeholder)) {
				found = append(found, rel+": "+placeholder)
			}
			if notYet := "the " + name + " request type is not implemented yet"; rel == "internal/fetch/requesttype_"+name+".go" && bytes.Contains(raw, []byte(notYet)) {
				found = append(found, rel+": "+notYet)
			}
		}
		return nil
	})
	return found, err
}

func TestNoScaffoldPlaceholderIsLeftInTheTree(t *testing.T) {
	found, err := scaffoldPlaceholders(".", treeRequestTypes(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s is left from make new-request-type; write what it stands for (docs/DEVELOPMENT.md, Adding a request type)", f)
	}
}

// The check finds a TODO of a type of the tree in any file, and a type's
// not-implemented failure in its own file, and nothing of another name.
func TestTheScaffoldPlaceholderCheckFindsWhatIsLeft(t *testing.T) {
	root := t.TempDir()
	for path, text := range map[string]string{
		"docs/ZZ.md":                       "# ZZ\n\nTODO(zz): what it reads.\n",
		"internal/fetch/requesttype_zz.go": "package fetch\n\n// errors.New(\"the zz request type is not implemented yet\")\n",
		"internal/fetch/requesttype_yy.go": "package fetch\n\n// TODO(other) is not one of the types.\n",
		"vendor/x/TODO.go":                 "TODO(zz)\n",
	} {
		file := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := scaffoldPlaceholders(root, []string{"zz", "yy"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	want := []string{"docs/ZZ.md: TODO(zz)", "internal/fetch/requesttype_zz.go: the zz request type is not implemented yet"}
	if !slices.Equal(found, want) {
		t.Errorf("found %s, want %s", strings.Join(found, "; "), strings.Join(want, "; "))
	}
}
