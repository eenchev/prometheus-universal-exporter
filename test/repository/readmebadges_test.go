package repository

import (
	"regexp"
	"strings"
	"testing"
)

// The README's license badge is a static shields.io badge: the dynamic
// github/license endpoint renders "invalid" or "rate limited" whenever
// shields.io's pool of GitHub tokens runs dry, which is often. A static badge
// cannot ask GitHub, so this holds the license it names to the license
// LICENSE holds.

// licenseBadge is the README's license badge: its message, as shields.io
// spells it in a static badge's path, and its link.
var licenseBadge = regexp.MustCompile(`(?m)^\[!\[License\]\(https://img\.shields\.io/badge/license-((?:[^-]|--)+)-[a-z]+\)\]\(([^)]+)\)$`)

// licenseIDs are the SPDX identifiers of the licenses LICENSE could hold, by
// the opening a license's text starts with.
var licenseIDs = map[string]string{
	"Apache License Version 2.0, January 2004": "Apache-2.0",
}

func TestReadmeLicenseBadgeNamesTheLicenseOfLICENSE(t *testing.T) {
	opening := strings.Join(strings.Fields(read(t, "LICENSE")), " ")
	want := ""
	for start, id := range licenseIDs {
		if strings.HasPrefix(opening, start) {
			want = id
		}
	}
	if want == "" {
		t.Fatalf("LICENSE starts with none of the licenses the test knows; add its opening and SPDX identifier to licenseIDs:\n%.120s", opening)
	}
	readme := read(t, "README.md")
	if strings.Contains(readme, "img.shields.io/github/license/") {
		t.Error("README.md's license badge asks shields.io's github/license endpoint, which fails while its GitHub tokens are exhausted; use a static badge, https://img.shields.io/badge/license-<SPDX id>-blue")
	}
	match := licenseBadge.FindStringSubmatch(readme)
	if match == nil {
		t.Fatal("README.md has no static license badge, [![License](https://img.shields.io/badge/license-<SPDX id>-<color>)](LICENSE)")
	}
	// In a static badge's path - is written -- and _ is a space.
	if got := strings.ReplaceAll(match[1], "--", "-"); got != want {
		t.Errorf("README.md's license badge names %s, but LICENSE is %s", got, want)
	}
	if match[2] != "LICENSE" {
		t.Errorf("README.md's license badge links %s, want LICENSE", match[2])
	}
}
