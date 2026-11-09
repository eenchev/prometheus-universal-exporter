package repository

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The specifications hold rules, not a record of the tests each change
// added: a test says what it checks in its name and its comment, beside the
// code. § 34 keeps the rules the suite is held to, its subsections 34.1 to
// 34.34, and no section after them, and no specification lists a test by its
// name, as the sections that recorded each change's tests did, some
// nineteen thousand lines of them, nor in passing, as the chart's Helm
// tests named theirs in parentheses. A pattern of names, as
// `TestExternal*`, is a rule and stays.
func TestTheSpecificationsListNoTests(t *testing.T) {
	section := regexp.MustCompile(`^## 34\.(\d+)`)
	listed := regexp.MustCompile("`(Test|Fuzz|Benchmark)[A-Z][A-Za-z0-9_]*`")
	for _, path := range []string{"docs/SPECIFICATION-EXPORTER.md", "docs/SPECIFICATION-CHART.md"} {
		sections := 0
		for n, line := range strings.Split(read(t, path), "\n") {
			if m := section.FindStringSubmatch(line); m != nil {
				sections++
				if number, _ := strconv.Atoi(m[1]); number > 34 {
					t.Errorf("%s:%d: %q records tests; say what a test checks in its comment, and a rule in the section of its feature", path, n+1, line)
				}
			}
			if listed.MatchString(line) {
				t.Errorf("%s:%d lists a test by its name: %q", path, n+1, line)
			}
		}
		if sections == 0 {
			t.Errorf("%s has no subsection of § 34; the chapter has moved", path)
		}
	}
}
