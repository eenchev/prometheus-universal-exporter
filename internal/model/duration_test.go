package model

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// A duration written as a negative amount under a nanosecond was read as
// the zero Go rounds it to, and a key that takes zero took -0.4ns, which the
// schemas refuse for its minus sign. It is now read as negative, so each
// key refuses it as it refuses any negative duration; a zero written with a
// minus sign stays zero.
func TestADurationWrittenNegativeIsReadAsNegativeHoweverSmall(t *testing.T) {
	for _, text := range []string{"-0.4ns", "-.5ns", "-0.0000000001s", "-0s0s0s0.1ns", "-0h0.0000000000001h", "-0.9ns0.9ns", "-0.000001µs"} {
		var d Duration
		if err := d.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: text}); err != nil || d >= 0 {
			t.Errorf("%s: read as %s, %v; want a negative duration", text, time.Duration(d), err)
		}
	}
	for _, text := range []string{"-0s", "-0", "-0h0m0s", "-0.0s", "-.0s", "-0.m", "+0.4ns", "0.4ns", ".5ns", "0.0000000001s", "0s0s0s0.1ns", "+0s"} {
		var d Duration
		if err := d.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: text}); err != nil || d != 0 {
			t.Errorf("%s: read as %s, %v; want zero", text, time.Duration(d), err)
		}
	}
}

// Every other text is read as it was, which is as time.ParseDuration reads
// it: over a sign or none, one or two numbers with units, well written and
// not, a duration differs from Go's only where it is written negative, with
// a digit other than zero, and Go rounds it to zero.
func TestADurationIsOtherwiseReadAsGoReadsIt(t *testing.T) {
	signs := []string{"", "+", "-", "--", " "}
	numbers := []string{"", "0", "00", "5", "05", "1.5", ".5", "5.", "0.0", ".0", "0.", ".", "0.0000000001", "1e3", "9223372036854775808", "2562048"}
	units := []string{"", "ns", "us", "µs", "μs", "ms", "s", "m", "h", "d", "S"}
	second := []string{"", "0s", "30m", "0.5s", "0.1ns", ".0m", "5", "-1s"}
	texts, differing := 0, 0
	for _, sign := range signs {
		for _, number := range numbers {
			for _, unit := range units {
				for _, rest := range second {
					text := sign + number + unit + rest
					texts++
					want, wantErr := time.ParseDuration(text)
					var got Duration
					err := got.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: text})
					if (err == nil) != (wantErr == nil) {
						t.Errorf("%q: read with %v; Go reads it with %v", text, err, wantErr)
						continue
					}
					if err != nil || time.Duration(got) == want {
						continue
					}
					differing++
					if want != 0 || got != -1 || !strings.HasPrefix(text, "-") || !strings.ContainsAny(text, "123456789") {
						t.Errorf("%q: read as %d; Go reads it as %d", text, got, want)
					}
				}
			}
		}
	}
	if texts < 7000 || differing == 0 {
		t.Fatalf("%d texts tried, %d of them read otherwise than Go reads them", texts, differing)
	}
	t.Logf("%d texts tried, %d of them read otherwise than Go reads them", texts, differing)
}
