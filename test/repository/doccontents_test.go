package repository

import (
	"regexp"
	"strings"
	"testing"
)

// A page with a Contents list lists every section and subsection of it, ##
// and ###, by the anchor GitHub gives the heading: a section added without
// its line is one the reader of the list never finds, as Dates and times
// was in REQUESTS.md.
func TestAPagesContentsListsEverySection(t *testing.T) {
	heading := regexp.MustCompile(`^(#{2,3}) (.+)$`)
	link := regexp.MustCompile(`\]\(#([^)]+)\)`)
	for _, path := range []string{"docs/REQUESTS.md", "docs/CONFIGURATION.md"} {
		text := read(t, path)
		start := strings.Index(text, "\n## Contents\n")
		if start < 0 {
			t.Fatalf("%s has no Contents", path)
		}
		contents := text[start+1:]
		contents = contents[:strings.Index(contents[3:], "\n## ")+3]
		listed := map[string]bool{}
		for _, m := range link.FindAllStringSubmatch(contents, -1) {
			listed[m[1]] = true
		}
		fenced, sections := false, 0
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "```") {
				fenced = !fenced
			}
			m := heading.FindStringSubmatch(line)
			if fenced || m == nil || m[2] == "Contents" {
				continue
			}
			sections++
			if anchor := githubAnchor(m[2]); !listed[anchor] {
				t.Errorf("%s: %q is not in its Contents, as (#%s)", path, line, anchor)
			}
		}
		if sections < 10 {
			t.Fatalf("%s: %d sections were read", path, sections)
		}
	}
}

// githubAnchor is the anchor GitHub gives a heading of text: lower case,
// with what is not a letter, digit, _, - or space left out, and spaces as
// hyphens.
func githubAnchor(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}
