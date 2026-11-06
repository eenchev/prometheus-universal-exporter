package allocator

import (
	"strings"
	"testing"
)

// A discovered weight label gives a weight when its value is a number above
// 0 and at most 1000 of at most three decimals; otherwise it says why it
// gives none, the reason the fallback is counted by.
func TestAWeightLabelsValue(t *testing.T) {
	for _, c := range []struct {
		value   string
		present bool
		weight  int64
		reason  string
	}{
		{"2", true, 2000, ""},
		{"0.5", true, 500, ""},
		{"0.001", true, 1, ""},
		{"1.005", true, 1005, ""},
		{"1000", true, 1000000, ""},
		{"1e2", true, 100000, ""},
		{"", false, 0, fallbackMissing},
		{"", true, 0, fallbackMissing},
		{"heavy", true, 0, fallbackNotANumber},
		{"NaN", true, 0, fallbackNotANumber},
		{"0", true, 0, fallbackOutOfRange},
		{"-1", true, 0, fallbackOutOfRange},
		{"1000.001", true, 0, fallbackOutOfRange},
		{"+Inf", true, 0, fallbackOutOfRange},
		{"1.0005", true, 0, fallbackDecimals},
		{"0.0004", true, 0, fallbackDecimals},
	} {
		w, reason := labelWeight(c.value, c.present)
		if w != c.weight || reason != c.reason {
			t.Errorf("%q (present %v) gives %d, %q; want %d, %q", c.value, c.present, w, reason, c.weight, c.reason)
		}
	}
}

// A discovered weight that moves less than 20% from the weight in force is
// held at the weight in force, in what the source keeps, so a label that
// wavers moves no target; one that moves 20% or more, or that gives no
// weight before or now, is taken.
func TestADiscoveredWeightIsHeldWithinTwentyPercent(t *testing.T) {
	const label = "__meta_pue_weight"
	answer := func(values ...string) []discovered {
		var out []discovered
		for i, v := range values {
			d := discovered{Address: string(rune('a'+i)) + ":9100", Labels: map[string]string{"zone": "x"}}
			if v != "-" {
				d.Labels[label] = v
			}
			out = append(out, d)
		}
		return out
	}
	before := answer("10", "10", "10", "10", "bad", "10", "10")
	now := answer("11", "8.1", "12", "8", "10", "-", "abc")
	got := holdWeights(before, now, label)
	for i, want := range []string{"10", "10", "12", "8", "10", "", "abc"} {
		if v := got[i].Labels[label]; v != want {
			t.Errorf("%s: the weight label is %q, want %q", got[i].Address, v, want)
		}
	}
	if now[0].Labels[label] != "11" {
		t.Error("the new answer's labels were changed in place")
	}
	if got := holdWeights(before, now, ""); got[0].Labels[label] != "11" {
		t.Error("an owner without weight_label holds a label")
	}
}

// The allocator takes default_weight and each target's weight out of the
// static target file before the exporter's loader reads it, merged-in
// weights too; a file without them is given back as it is, and a bad weight
// is refused naming where it is.
func TestTheStaticTargetFilesWeightsAreTakenOut(t *testing.T) {
	raw := []byte("x-heavy: &heavy\n  weight: 4\ninterval: 1m\ndefault_weight: 0.5\ntargets:\n  - name: a\n    collector: c\n    target: http://a/\n  - <<: *heavy\n    name: b\n    collector: c\n    target: http://b/\n  - name: c\n    weight: 2.5\n    collector: c\n    target: http://c/\n")
	out, w, err := stripStaticWeights(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The x- key that held the anchor is the file's own, which the exporter
	// leaves alone.
	if strings.Count(string(out), "weight") != 1 || w.defaultWeight != 500 || len(w.targets) != 3 || w.targets[0] != 500 || w.targets[1] != 4000 || w.targets[2] != 2500 {
		t.Errorf("got %+v and\n%s", w, out)
	}
	plain := []byte("interval: 1m\ntargets:\n  - name: a # kept\n    collector: c\n    target: http://a/\n")
	if out, w, err := stripStaticWeights(plain); err != nil || string(out) != string(plain) || w.defaultWeight != milli {
		t.Errorf("a file without weights came back as %q, %+v, %v", out, w, err)
	}
	for text, want := range map[string]string{
		"interval: 1m\ndefault_weight: 0\ntargets: []\n":                                        "default_weight 0 is not a weight",
		"interval: 1m\ndefault_weight: '2'\ntargets: []\n":                                      `default_weight "2" is not a number`,
		"interval: 1m\ntargets:\n  - {name: a, weight: 1001}\n":                                 "target 0 (a) weight 1001 is not a weight",
		"interval: 1m\ntargets:\n  - {name: a, weight: 1.0001}\n":                               "target 0 (a) weight 1.0001 is not a weight",
		"interval: 1m\ntargets:\n  - {collector: c, weight: .nan}\n":                            "target 0 weight .nan is not a weight",
		"interval: 1m\ntargets:\n  - {name: a, weight_label: __meta_w}\n":                       "target 0 (a) sets weight_label",
		"interval: 1m\ntargets:\n  - {name: a, weight: [1]}\n":                                  "target 0 (a) weight",
		"interval: 1m\ndefault_weight: 1\ntargets:\n  - {name: a}\n  - {name: b, weight: -2}\n": "target 1 (b) weight -2 is not a weight",
	} {
		if _, _, err := stripStaticWeights([]byte(text)); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), weightRule) && !strings.Contains(err.Error(), "weight_label") {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
}
