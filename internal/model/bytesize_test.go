package model

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ByteSizePattern, which the configuration schema gives an editor, and
// parseByteSize, which reads the size, accept the same spellings. The pattern
// once let through what the exporter refuses — a fraction without a unit —
// so a configuration an editor showed as valid did not load. What a pattern
// cannot hold is the range: a size of 2^63 bytes or more matches it and is
// refused when read.
func TestTheSizePatternAcceptsWhatIsReadAsASize(t *testing.T) {
	pattern := regexp.MustCompile(ByteSizePattern)
	for spelling, accepted := range map[string]bool{
		"0": true, "1024": true, "0512": true, "9223372036854775807": true,
		"10B": true, "10b": true, "10 B": true, "1.5B": true, "1.0001B": true,
		"1k": true, "1K": true, "1kB": true, "1KB": true, "1kb": true, "1 kB": true, "1.5 kb": true,
		"2Ki": true, "2KiB": true, "2kib": true, "2KIB": true, "0.5KiB": true, "0.0001KiB": true,
		"10M": true, "10MB": true, "64Mi": true, "64MiB": true, "64 MiB": true, "1.5MiB": true,
		"1G": true, "1GB": true, "1.5GiB": true, "0.5GiB": true, "1T": true, "1tb": true, "1TiB": true, "8388607TiB": true,

		"": false, " ": false, "MiB": false, "B": false, "lots": false, "ten": false,
		"1.5": false, "1.0": false, "0.0": false, "1.": false, ".5": false, ".5MiB": false, "5.MiB": false, "1.5.5MiB": false, "1,5MiB": false,
		"-1": false, "-1MiB": false, "-1.5MiB": false, "+5": false, "+5MiB": false,
		"1e3": false, "1e3KiB": false, "0x100": false, "1_000": false,
		"8EiB": false, "1PiB": false, "10 XB": false, "10MiBs": false, "10Mib/s": false, "1 k B": false, "1kiB B": false,
		"10  MiB": false, " 10MiB": false, "10MiB ": false, "10\tMiB": false, "10MiB\n": false, "１０MiB": false,
	} {
		_, err := parseByteSize(spelling)
		if matches := pattern.MatchString(spelling); matches != accepted || (err == nil) != accepted {
			t.Errorf("%q: the pattern says %v and reading it %v; want both %v", spelling, matches, err, accepted)
		}
	}
	for _, spelling := range []string{"8388608TiB", "99999999999TiB", "9223372036854775808", "99999999999999999999", "10000000000GB"} {
		_, err := parseByteSize(spelling)
		if !pattern.MatchString(spelling) || err == nil || !strings.Contains(err.Error(), "is too large; a size is under 8EiB, which is 2^63 bytes") {
			t.Errorf("%q, past the range: the pattern says %v and reading it %v; want a match, refused as too large", spelling, pattern.MatchString(spelling), err)
		}
	}
	// A number of bytes without a unit is read exactly, the largest included.
	for spelling, want := range map[string]ByteSize{"9223372036854775807": 1<<63 - 1, "9007199254740993": 1<<53 + 1} {
		if got, err := parseByteSize(spelling); err != nil || got != want {
			t.Errorf("%q is read as %d, %v; want %d", spelling, got, err, want)
		}
	}
}

func TestByteSizes(t *testing.T) {
	for in, want := range map[string]ByteSize{
		"0": 0, "1024": 1024, "10B": 10, "1kB": 1000, "1KB": 1000, "1k": 1000, "2KiB": 2048, "2kib": 2048,
		"10MB": 10_000_000, "64MiB": 64 << 20, "64 MiB": 64 << 20, "1.5GiB": 3 << 29, "1GB": 1e9, "1TiB": 1 << 40, "0.5KiB": 512, "1.0001B": 1,
	} {
		got, err := parseByteSize(in)
		if err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	// 8388608TiB is 2^63 bytes, one past the most a size holds; a fraction
	// needs a unit to scale.
	for _, in := range []string{"", "MiB", "-1", "10 XB", "1e3", "10MiBs", "ten", "99999999999TiB", "8388608TiB", "9223372036854775808", "1.5", "0.0"} {
		if _, err := parseByteSize(in); err == nil {
			t.Errorf("parseByteSize(%q) was accepted", in)
		}
	}
	var doc struct {
		A ByteSize `yaml:"a"`
		B ByteSize `yaml:"b"`
		C ByteSize `yaml:"c"`
	}
	if err := yaml.Unmarshal([]byte("a: 1048576\nb: 64MiB\nc: \"512\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.A != 1<<20 || doc.B != 64<<20 || doc.C != 512 {
		t.Fatalf("decoded %+v", doc)
	}
	if err := yaml.Unmarshal([]byte("a: [1]\n"), &doc); err == nil {
		t.Fatal("a list was accepted as a size")
	}
	// A YAML number is held to the same: not negative, whole, under 2^63.
	for document, want := range map[string]string{
		"a: -1\n":                  "line 1: size -1 is negative",
		"a: 9223372036854775808\n": "line 1: size 9223372036854775808 is too large",
		"a: 1.5\n":                 `line 1: size "1.5" is not a whole number of bytes`,
		"a: -1.5MiB\n":             `line 1: size "-1.5MiB" is not a number of bytes`,
	} {
		if err := yaml.Unmarshal([]byte(document), &doc); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", document, err, want)
		}
	}
	if err := yaml.Unmarshal([]byte("a: 9223372036854775807\nb: 8388607TiB\n"), &doc); err != nil || doc.A != 1<<63-1 || doc.B != 8388607<<40 {
		t.Fatalf("the largest sizes: %v %+v", err, doc)
	}
}
