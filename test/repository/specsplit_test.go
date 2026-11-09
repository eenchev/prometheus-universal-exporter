package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Each request type but http has its section of spec § 5.1 in a document of
// its own, docs/SPECIFICATION-<TYPE>.md, so that a change to one type is a
// change to one document: there is one for every type of the tree and none
// for a type that is not, each opens as the others do and holds the type's
// section, § 5.1's list and the README's line name them all in order, and
// no type's section has come back into the exporter's specification.
func TestEveryRequestTypeHasASpecificationOfItsOwn(t *testing.T) {
	var types []string
	for _, typ := range treeRequestTypes(t) {
		if typ != "http" {
			types = append(types, typ)
		}
	}
	slices.Sort(types)
	files, err := filepath.Glob("docs/SPECIFICATION-*.md")
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, file := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "SPECIFICATION-"), ".md")
		if name != "EXPORTER" && name != "CHART" {
			have = append(have, strings.ToLower(name))
		}
	}
	slices.Sort(have)
	if !slices.Equal(have, types) {
		t.Fatalf("the request types' specifications are of %v, the types of the tree %v", have, types)
	}

	exporter := read(t, "docs/SPECIFICATION-EXPORTER.md")
	opening, _, _ := strings.Cut(exporter, "\n## 1. ")
	_, list, ok := strings.Cut(exporter, "\n#### The other request types\n")
	if !ok {
		t.Fatal("spec § 5.1 has no \"The other request types\"; it has moved")
	}
	list, _, _ = strings.Cut(list, "\n#")
	var listed, readme []string
	for _, m := range regexp.MustCompile("(?m)^- \\[`([a-z0-9]+)`\\]\\(SPECIFICATION-([A-Z0-9]+)\\.md\\)$").FindAllStringSubmatch(list, -1) {
		if m[2] != strings.ToUpper(m[1]) {
			t.Errorf("§ 5.1 links %s to SPECIFICATION-%s.md", m[1], m[2])
		}
		listed = append(listed, m[1])
	}
	if !slices.Equal(listed, types) {
		t.Errorf("§ 5.1's list of the types' specifications is %v, not %v", listed, types)
	}
	line := regexp.MustCompile(`(?m)^- The request types' specifications, .*$`).FindString(read(t, "README.md"))
	for _, m := range regexp.MustCompile("\\[`([a-z0-9]+)`\\]\\(docs/SPECIFICATION-([A-Z0-9]+)\\.md\\)").FindAllStringSubmatch(line, -1) {
		readme = append(readme, m[1])
	}
	if !slices.Equal(readme, types) {
		t.Errorf("the README's line of the types' specifications names %v, not %v: %q", readme, types, line)
	}

	for _, typ := range types {
		NAME := strings.ToUpper(typ)
		path := "docs/SPECIFICATION-" + NAME + ".md"
		text := read(t, path)
		if !strings.HasPrefix(text, "# Prometheus Universal Exporter — `"+typ+"` Request Type Specification\n\nThis is the `"+typ+"` request type's section of § 5.1 of the\n[exporter specification](SPECIFICATION-EXPORTER.md)") {
			t.Errorf("%s does not open as the types' specifications do", path)
		}
		if !strings.Contains(text, "\n## `"+typ+"`\n") {
			t.Errorf("%s has no section \"## `%s`\"", path, typ)
		}
		if _, err := os.Stat("docs/" + NAME + ".md"); err == nil && !strings.Contains(text, "["+NAME+".md]("+NAME+".md)") {
			t.Errorf("%s does not link the type's page, %s.md", path, NAME)
		}
		// A numbered section a type's document took with it, as § 9a and
		// § 5.1a, is one a reference may name: the exporter's opening says
		// where it went.
		for _, m := range regexp.MustCompile(`(?m)^#+ ([0-9]+(?:\.[0-9]+)*[a-z]?)\.? `).FindAllStringSubmatch(text, -1) {
			if !strings.Contains(opening, "§ "+m[1]+",") {
				t.Errorf("%s holds § %s, which the exporter's specification does not say it moved to", path, m[1])
			}
		}
		if strings.Contains(exporter, "\n#### `"+typ+"`\n") {
			t.Errorf("the exporter's specification has a section of `%s` again; it belongs in %s", typ, path)
		}
	}
	if !strings.Contains(exporter, "\n#### `http`\n") {
		t.Error("the exporter's specification has no section of `http`, which stays in it")
	}
}
