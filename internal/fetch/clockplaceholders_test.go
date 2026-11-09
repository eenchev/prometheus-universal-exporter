package fetch

import (
	"strings"
	"testing"
	"time"
)

// atClock makes clockNow return now for the rest of the test.
func atClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := clockNow
	clockNow = func() time.Time { return now }
	t.Cleanup(func() { clockNow = previous })
}

// Every format writes the moment as it is named, an offset moves it, by
// calendar days or by elapsed time, a truncation takes the start of its
// unit after the offset, and a zone is where all of it is read.
func TestAClockPlaceholderWritesTheMomentInItsFormat(t *testing.T) {
	sofia, err := time.LoadLocation("Europe/Sofia")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 9, 22, 30, 15, 250000000, time.UTC)
	// Sofia leaves summer time at 04:00 on 25 October 2026: at 23:30 that
	// day, a calendar day back is the 24th, and 24 hours back the 25th.
	acrossDST := time.Date(2026, time.October, 25, 23, 30, 0, 0, sofia)
	for text, want := range map[string]string{
		"date":                       "2026-11-09",
		"date_compact":               "20261109",
		"date_eu":                    "09.11.2026",
		"date_us":                    "11/09/2026",
		"month":                      "2026-11",
		"year":                       "2026",
		"time":                       "22:30:15",
		"datetime":                   "2026-11-09T22:30:15Z",
		"datetime_ms":                "2026-11-09T22:30:15.250Z",
		"unix":                       "1794263415",
		"unix_ms":                    "1794263415250",
		"date:-1d":                   "2026-11-08",
		"date:+1w":                   "2026-11-16",
		"date@Europe/Sofia":          "2026-11-10",
		"datetime@Europe/Sofia":      "2026-11-10T00:30:15+02:00",
		"datetime:-1d/d":             "2026-11-08T00:00:00Z",
		"datetime/d@Europe/Sofia":    "2026-11-10T00:00:00+02:00",
		"datetime:-1h/h":             "2026-11-09T21:00:00Z",
		"datetime:-1d12h30m15s":      "2026-11-08T10:00:00Z",
		"datetime:+90m/m":            "2026-11-10T00:00:00Z",
		"unix/d":                     "1794182400",
		"unix_ms:-1d/d@Europe/Sofia": "1794175200000",
	} {
		c, ok, err := parseClock("request.path", text)
		if err != nil || !ok {
			t.Fatalf("{{%s}}: %v, %v", text, ok, err)
		}
		if got := c.at(now); got != want {
			t.Errorf("{{%s}} writes %q, want %q", text, got, want)
		}
	}
	for text, want := range map[string]string{
		"date:-1d@Europe/Sofia":       "2026-10-24",
		"date:-24h@Europe/Sofia":      "2026-10-25",
		"datetime:-1d/d@Europe/Sofia": "2026-10-24T00:00:00+03:00",
	} {
		c, _, err := parseClock("request.path", text)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.at(acrossDST); got != want {
			t.Errorf("across the change, {{%s}} writes %q, want %q", text, got, want)
		}
	}
}

// A clock placeholder written wrong is refused when the configuration
// loads, saying what is wrong; a name that is no clock format's is left to
// be read as what else it may be.
func TestAClockPlaceholderWrittenWrongIsRefused(t *testing.T) {
	for text, want := range map[string]string{
		"date:1d":             "is a clock placeholder written wrong",
		"date:-1x":            `has the offset "-1x"`,
		"date:-":              "is a clock placeholder written wrong",
		"date:-100001d":       "more than 100000",
		"date/w":              `truncates to "w"`,
		"date@Mars/Olympus":   `the time zone "Mars/Olympus"`,
		"date@Europe/Sofia/d": `the time zone "Europe/Sofia/d"`,
		"date/d:-1d":          "is a clock placeholder written wrong",
	} {
		_, ok, err := parseClock("request.path", text)
		if !ok || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("{{%s}}: %v, %v, want an error with %q", text, ok, err, want)
		}
	}
	for _, text := range []string{"param_date", "dates", "today", "Date"} {
		if _, ok, err := parseClock("request.path", text); ok || err != nil {
			t.Errorf("{{%s}} is read as a clock placeholder: %v", text, err)
		}
	}
}

// A clock placeholder is found where a {{param_...}} one is, in a path and
// in a body alike, takes a filter where one may stand, and is no probe
// parameter; outside a path, braces before what is no clock format's name
// are text, and so is a name with a space before it.
func TestAClockPlaceholderIsFoundWhereAParameterIs(t *testing.T) {
	atClock(t, time.Date(2026, time.November, 9, 8, 0, 0, 0, time.UTC))
	found, err := parsePlaceholders("request.body", `{"day": {{date:-1d|json}}, "at": {{unix|number}}, "other": "{{dates}} {{ date }} {{param_x:1}}"}`, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 || found[0].Clock == nil || found[0].Filter != "json" || found[1].Clock == nil || found[2].Clock != nil {
		t.Fatalf("found %+v", found)
	}
	f := templateField{where: "request.body", text: `{"day": {{date:-1d|json}}, "at": {{unix|number}}}`, kind: "body"}
	got, err := f.render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"day": "2026-11-08", "at": 1794211200}`; got != want {
		t.Fatalf("rendered %s, want %s", got, want)
	}
	if _, err := parsePathParams("/x/{{today}}"); err == nil || !strings.Contains(err.Error(), "date, date_compact, date_eu") {
		t.Fatalf("an unknown name in a path: %v", err)
	}
}
