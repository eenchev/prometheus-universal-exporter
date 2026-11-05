package pagekit

import (
	"testing"
	"time"
)

// TestADurationLeavesOutItsZeroUnits: the pages write a duration in Go's
// form with every unit that is zero left out, a fraction of a second kept
// and one under a second as Go writes it.
func TestADurationLeavesOutItsZeroUnits(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                        "0s",
		500 * time.Millisecond:                   "500ms",
		1500 * time.Microsecond:                  "1.5ms",
		time.Second:                              "1s",
		1500 * time.Millisecond:                  "1.5s",
		59 * time.Second:                         "59s",
		time.Minute:                              "1m",
		90 * time.Second:                         "1m30s",
		time.Minute + 500*time.Millisecond:       "1m0.5s",
		10 * time.Minute:                         "10m",
		74 * time.Second:                         "1m14s",
		time.Hour:                                "1h",
		2 * time.Hour:                            "2h",
		time.Hour + 30*time.Second:               "1h30s",
		time.Hour + 10*time.Minute:               "1h10m",
		time.Hour + 10*time.Minute + time.Second: "1h10m1s",
		time.Hour + 500*time.Millisecond:         "1h0.5s",
		100 * time.Hour:                          "100h",
		-time.Minute:                             "-1m",
		-(time.Hour + 30*time.Second):            "-1h30s",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%s) = %q, want %q", d.String(), got, want)
		}
	}
}

// TestADurationTextIsWrittenAsADuration: a duration kept in Go's form is
// written as Duration writes it, and text that is no duration as it is.
func TestADurationTextIsWrittenAsADuration(t *testing.T) {
	for text, want := range map[string]string{"1m0s": "1m", "2h0m0s": "2h", "1h0m30s": "1h30s", "30s": "30s", "500ms": "500ms", "never": "never", "": ""} {
		if got := DurationText(text); got != want {
			t.Errorf("DurationText(%q) = %q, want %q", text, got, want)
		}
	}
}
