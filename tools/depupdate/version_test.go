package main

import "testing"

func TestParseVersionRejectsUnstableVersions(t *testing.T) {
	for _, raw := range []string{"1.23", "1.23.4", "4.12.3", "2.9.0.post0", "10.0.0"} {
		if _, ok := parseVersion(raw); !ok {
			t.Errorf("parseVersion(%q) should be accepted", raw)
		}
	}
	for _, raw := range []string{"", "latest", "3.13.0rc1", "1.24.0-beta", "2.0.0.dev1", "1.2.3.post1.dev0", "alpine", "1.post2.3"} {
		if _, ok := parseVersion(raw); ok {
			t.Errorf("parseVersion(%q) should be refused", raw)
		}
	}
}

func TestSelectUpdateNeverCrossesAMajor(t *testing.T) {
	got, err := selectUpdate("4.12.3", []string{"4.13.4", "5.0.0", "6.1.2"}, policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "4.13.4" {
		t.Fatalf("got %q, want the newest 4.x", got)
	}
}

// A pin like 1.23 is a floating tag that already picks up patch releases, so
// rewriting it to 1.23.4 would freeze it.
func TestSelectUpdateKeepsPinGranularity(t *testing.T) {
	got, err := selectUpdate("1.23", []string{"1.23.4", "1.23.9", "1.24", "1.24.1"}, policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.24" {
		t.Fatalf("got %q, want the two-component 1.24", got)
	}

	got, err = selectUpdate("5.3.0", []string{"5.4", "5.3.1"}, policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "5.3.1" {
		t.Fatalf("got %q, want the three-component 5.3.1", got)
	}
}

func TestSelectUpdateHoldsMinorWhenNotAllowed(t *testing.T) {
	got, err := selectUpdate("3.12", []string{"3.13", "3.14", "3.12"}, policy{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q, want no update while the minor is held", got)
	}
}

func TestSelectUpdateHandlesPostReleases(t *testing.T) {
	got, err := selectUpdate("2.9.0.post0", []string{"2.9.0.post1", "2.9.0"}, policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "2.9.0.post1" {
		t.Fatalf("got %q, want the newer post release", got)
	}
}

func TestSelectUpdateIgnoresUnstableAndUnchangedCandidates(t *testing.T) {
	got, err := selectUpdate("6.0.2", []string{"6.0.2", "6.1.0rc1", "latest", "6.0.1"}, policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q, want nothing when only unstable or older versions exist", got)
	}
}

func TestSelectUpdateRejectsAnUnparseablePin(t *testing.T) {
	if _, err := selectUpdate("not-a-version", []string{"1.0.0"}, policy{}); err == nil {
		t.Fatal("an unparseable pin must be an error rather than a silent no-op")
	}
}

func TestCompareOrdersAcrossGranularity(t *testing.T) {
	a, _ := parseVersion("1.24")
	b, _ := parseVersion("1.23.9")
	if compare(a, b) <= 0 {
		t.Fatal("1.24 should sort above 1.23.9")
	}
	c, _ := parseVersion("1.23")
	d, _ := parseVersion("1.23.0")
	if compare(c, d) != 0 {
		t.Fatal("1.23 and 1.23.0 should compare equal")
	}
}

// The minor a policy holds is reported when a newer one exists in the pin's own
// major and granularity, and only then: not for a pin whose minor may move, not
// for a new major, a pre-release or a patch release of the minor it is on.
func TestHeldMinorNamesTheNewestMinorThePolicyKeepsThePinFrom(t *testing.T) {
	cases := []struct {
		name       string
		current    string
		candidates []string
		policy     policy
		want       string
	}{
		{"a newer feature release", "3.12", []string{"3.11", "3.12", "3.13", "3.14"}, policy{}, "3.14"},
		{"a three-component pin", "3.12.4", []string{"3.12.9", "3.13.7", "3.14.1", "3.14"}, policy{}, "3.14"},
		{"only a patch release is newer", "3.12.4", []string{"3.12.9", "3.12"}, policy{}, ""},
		{"only a major and a release candidate are newer", "3.12", []string{"4.0", "3.13.0rc1", "3.13rc1"}, policy{}, ""},
		{"another granularity is not what the build pulls", "3.12", []string{"3.13.1", "3.14.0"}, policy{}, ""},
		{"the minor may move", "1.26", []string{"1.27"}, policy{AllowMinor: true}, ""},
		{"a pin that does not parse", "stable", []string{"3.14"}, policy{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := heldMinor(tc.current, tc.candidates, tc.policy); got != tc.want {
				t.Fatalf("heldMinor(%q, %v)=%q, want %q", tc.current, tc.candidates, got, tc.want)
			}
		})
	}
}
