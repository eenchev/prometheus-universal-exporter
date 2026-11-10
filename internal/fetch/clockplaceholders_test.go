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
		if got := c.at(ScrapeClock{Now: now}); got != want {
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
		if got := c.at(ScrapeClock{Now: acrossDST}); got != want {
			t.Errorf("across the change, {{%s}} writes %q, want %q", text, got, want)
		}
	}
}

// In the hour a zone's clocks show twice when its summer time ends, a
// placeholder writes the moment it is, in both passes: the first was
// written as the second, an hour late, by every placeholder with the zone,
// and its hour and minute truncated an hour late too. A day whose midnight
// comes twice starts at the first, and one with no midnight when the
// clocks first show it; an hour starts on the hour of the zone, also where
// the zone is half an hour off UTC's.
func TestAClockPlaceholderInAnHourTheZoneRepeats(t *testing.T) {
	// Sofia: 04:00 EEST becomes 03:00 EET at 01:00 UTC on 25 October 2026.
	first := time.Date(2026, time.October, 25, 0, 30, 15, 0, time.UTC)
	second := first.Add(time.Hour)
	// Havana: 01:00 CDT becomes 00:00 CST at 05:00 UTC on 1 November 2026,
	// and 00:00 CST becomes 01:00 CDT at 05:00 UTC on 8 March 2026.
	twoMidnights := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	noMidnight := time.Date(2026, time.March, 8, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now        time.Time
		text, want string
	}{
		{first, "unix@Europe/Sofia", "1792888215"},
		{first, "datetime@Europe/Sofia", "2026-10-25T03:30:15+03:00"},
		{first, "datetime/m@Europe/Sofia", "2026-10-25T03:30:00+03:00"},
		{first, "datetime/h@Europe/Sofia", "2026-10-25T03:00:00+03:00"},
		{first, "datetime:-1h@Europe/Sofia", "2026-10-25T02:30:15+03:00"},
		{first, "datetime:+1h@Europe/Sofia", "2026-10-25T03:30:15+02:00"},
		{first, "datetime/d@Europe/Sofia", "2026-10-25T00:00:00+03:00"},
		{second, "unix@Europe/Sofia", "1792891815"},
		{second, "datetime@Europe/Sofia", "2026-10-25T03:30:15+02:00"},
		{second, "datetime/m@Europe/Sofia", "2026-10-25T03:30:00+02:00"},
		{second, "datetime/h@Europe/Sofia", "2026-10-25T03:00:00+02:00"},
		{second, "unix/h@Europe/Sofia", "1792890000"},
		{first, "unix/h@Europe/Sofia", "1792886400"},
		{twoMidnights, "datetime/d@America/Havana", "2026-11-01T00:00:00-04:00"},
		{twoMidnights, "unix/d@America/Havana", "1793505600"},
		{noMidnight, "datetime/d@America/Havana", "2026-03-08T01:00:00-04:00"},
		{noMidnight, "date/d@America/Havana", "2026-03-08"},
		{first, "datetime/h@Asia/Kolkata", "2026-10-25T06:00:00+05:30"},
	} {
		p, ok, err := parseClock("request.path", c.text)
		if err != nil || !ok {
			t.Fatalf("{{%s}}: %v, %v", c.text, ok, err)
		}
		if got := p.at(ScrapeClock{Now: c.now}); got != c.want {
			t.Errorf("at %s, {{%s}} writes %q, want %q", c.now.Format(time.RFC3339), c.text, got, c.want)
		}
	}
	// A window from the last success meets the one before it across the
	// hour: the first pass's end is the second pass's start.
	from, _, _ := parseClock("request.path", "unix~last_success(-1h)/m@Europe/Sofia")
	to, _, _ := parseClock("request.path", "unix/m@Europe/Sofia")
	if end, start := to.at(ScrapeClock{Now: first}), from.at(ScrapeClock{Now: second, Since: first}); end != start || end != "1792888200" {
		t.Errorf("the window of the first pass ends at %s and the next starts at %s, want both 1792888200", end, start)
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
