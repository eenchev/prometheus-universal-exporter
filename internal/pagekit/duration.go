package pagekit

import (
	"strings"
	"time"
)

// Duration is d as the pages write a duration, an interval configured or
// an age: Go's form with every unit that is zero left out, so 1m, 2h and
// 1h30s rather than 1m0s, 2h0m0s and 1h0m30s, the zeros of which only make
// it longer to read. Seconds that are not whole keep their fraction, 1m0.5s.
// A duration under a second is written as Go writes it, 500ms or 1.5µs,
// and 0 is 0s. Hours are not carried into days: a day is 24h.
func Duration(d time.Duration) string {
	s := d.String()
	// Go writes the minutes and seconds of a duration of a minute or more
	// whatever they are, and nothing smaller than seconds then, so a zero
	// seconds is its last two characters after a unit, and a zero minutes
	// follows the hours.
	if d <= -time.Minute || d >= time.Minute {
		s = strings.TrimSuffix(s, "m0s")
		if !strings.HasSuffix(s, "s") {
			s += "m"
		}
		s = strings.Replace(s, "h0m", "h", 1)
	}
	return s
}

// DurationText is text, a duration as Go writes it, as Duration writes it,
// for a value kept in its Go form, as a JSON answer has it, that a page
// shows; text that is no duration is written as it is.
func DurationText(text string) string {
	d, err := time.ParseDuration(text)
	if err != nil {
		return text
	}
	return Duration(d)
}
