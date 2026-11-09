package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The README is what someone reads before deciding to use this and while
// starting it the first time. Reference material crowds that out: a README that
// answers every question stops answering the first one. So the depth lives
// under docs/, the README indexes it, and these tests keep the arrangement from
// quietly coming apart — a moved page leaving a dead link, a new page nothing
// points at, or the README growing back into a manual.

// markdownLink matches an inline link's destination.
var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func markdownFiles(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"README.md", "docs/*.md", "charts/*/README.md"} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, found...)
	}
	sort.Strings(paths)
	if len(paths) < 3 {
		t.Fatalf("found %d markdown files; the documentation has moved", len(paths))
	}
	return paths
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A link to a page that is not there is the failure this split makes most
// likely, and the one a reader hits rather than a maintainer. A link to a
// section of a page, on another page or its own, names a heading the page
// has, by the anchor GitHub gives it: a renamed section otherwise leaves a
// link that opens the page at its top, as How do I…, a page of nothing but
// such links, would be most exposed to.
func TestDocumentationLinksResolve(t *testing.T) {
	anchors := map[string]map[string]bool{}
	for _, path := range markdownFiles(t) {
		t.Run(path, func(t *testing.T) {
			for _, match := range markdownLink.FindAllStringSubmatch(read(t, path), -1) {
				target := match[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				target, anchor, _ := strings.Cut(target, "#")
				resolved := path
				if target != "" {
					resolved = filepath.Join(filepath.Dir(path), target)
				}
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s links to %s, which does not exist", path, target)
					continue
				}
				if anchor == "" || !strings.HasSuffix(resolved, ".md") {
					continue
				}
				if anchors[resolved] == nil {
					anchors[resolved] = headingAnchors(read(t, resolved))
				}
				if !anchors[resolved][anchor] {
					t.Errorf("%s links to %s#%s, a section %s does not have", path, target, anchor, resolved)
				}
			}
		})
	}
}

// headingAnchors are the anchors GitHub gives the headings of a page, those
// in code blocks left out, a heading's second and later namesakes taking
// -1, -2 and on.
func headingAnchors(text string) map[string]bool {
	heading := regexp.MustCompile(`^#{1,6} (.+)$`)
	anchors, seen, fenced := map[string]bool{}, map[string]int{}, false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		m := heading.FindStringSubmatch(line)
		if fenced || m == nil {
			continue
		}
		anchor := githubAnchor(m[1])
		if n := seen[anchor]; n > 0 {
			anchors[fmt.Sprintf("%s-%d", anchor, n)] = true
		} else {
			anchors[anchor] = true
		}
		seen[anchor]++
	}
	return anchors
}

// A page nothing links to is a page nobody reads. The README's index is the way
// in, so every page under docs/ has to appear in it.
func TestEveryDocumentationPageIsIndexed(t *testing.T) {
	readme := read(t, "README.md")
	pages, err := filepath.Glob("docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("docs/ holds no pages; the documentation has moved")
	}
	for _, page := range pages {
		if !strings.Contains(readme, "("+page+")") {
			t.Errorf("%s is not linked from the README, so nothing leads a reader to it", page)
		}
	}
	if !strings.Contains(readme, "(charts/prometheus-universal-exporter/README.md)") {
		t.Error("the README must link the chart README rather than restate its values")
	}
}

// The README grows back one useful paragraph at a time, which is why this is a
// test rather than an intention. The limit is generous: what it catches is a
// reference section landing here instead of in docs/.
func TestReadmeStaysAnOverview(t *testing.T) {
	const limit = 200
	lines := strings.Count(read(t, "README.md"), "\n")
	if lines > limit {
		t.Errorf("README.md is %d lines, over the %d-line limit; reference material belongs in a page under docs/ that the README links",
			lines, limit)
	}
}
