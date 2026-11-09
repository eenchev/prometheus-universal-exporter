package fetch

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A placeholder with ~last_success starts from the last success when one
// is known, and otherwise from its fallback before the trip; the offset,
// the truncation and the zone apply after either.
func TestALastSuccessPlaceholderStartsFromTheLastSuccess(t *testing.T) {
	now := time.Date(2026, time.November, 9, 22, 30, 15, 0, time.UTC)
	since := time.Date(2026, time.November, 9, 21, 10, 5, 0, time.UTC)
	for text, want := range map[string][2]string{
		"datetime~last_success(-1h)":                {"2026-11-09T21:30:15Z", "2026-11-09T21:10:05Z"},
		"datetime~last_success(-1d)/d":              {"2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z"},
		"datetime~last_success(-2h):-5m":            {"2026-11-09T20:25:15Z", "2026-11-09T21:05:05Z"},
		"date~last_success(-1w)@Europe/Sofia":       {"2026-11-03", "2026-11-09"},
		"unix~last_success(-30m)":                   {"1794261615", "1794258605"},
		"datetime~last_success(-1h)/m@Europe/Sofia": {"2026-11-09T23:30:00+02:00", "2026-11-09T23:10:00+02:00"},
	} {
		c, ok, err := parseClock("request.query.from", text)
		if err != nil || !ok || !c.since {
			t.Fatalf("{{%s}}: %v, %v", text, ok, err)
		}
		if got := c.at(ScrapeClock{Now: now}); got != want[0] {
			t.Errorf("{{%s}} with no last success writes %q, want %q", text, got, want[0])
		}
		if got := c.at(ScrapeClock{Now: now, Since: since}); got != want[1] {
			t.Errorf("{{%s}} after a success writes %q, want %q", text, got, want[1])
		}
	}
	for text, want := range map[string]string{
		"datetime~last_success":          `starts from "~last_success"`,
		"datetime~last_success()":        `has the fallback ""`,
		"datetime~last_success(+1h)":     `has the fallback "+1h"`,
		"datetime~last_success(-1x)":     `has the fallback "-1x"`,
		"datetime~since(-1h)":            `starts from "~since(-1h)"`,
		"datetime:-1h~last_success(-1h)": "is a clock placeholder written wrong",
	} {
		_, ok, err := parseClock("request.query.from", text)
		if !ok || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("{{%s}}: %v, %v, want an error with %q", text, ok, err, want)
		}
	}
	// In a body, the last success follows the name directly, as an
	// offset does.
	found, err := parsePlaceholders("request.body", `{"from": {{unix_ms~last_success(-1h)|number}}}`, false, true)
	if err != nil || len(found) != 1 || found[0].Clock == nil || !found[0].Clock.since {
		t.Fatalf("found %+v, %v", found, err)
	}
}

// Every clock placeholder of a trip is filled from one reading of the
// clock, which its context may carry; outside a trip, as when a request is
// shown or checked, each is read anew.
func TestATripReadsTheClockOnce(t *testing.T) {
	carried := ScrapeClock{Now: time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC), Since: time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)}
	if got := scrapeClockFrom(WithScrapeClock(context.Background(), carried)); got != carried {
		t.Fatalf("the context carries %+v, want %+v", got, carried)
	}
	params := withScrapeClock(map[string]string{"param_x": "1"}, carried)
	if got := scrapeClockOf(params); !got.Now.Equal(carried.Now) || !got.Since.Equal(carried.Since) || params["param_x"] != "1" {
		t.Fatalf("the parameters carry %+v", got)
	}
	if got := scrapeClockOf(withScrapeClock(nil, ScrapeClock{Now: carried.Now})); !got.Since.IsZero() {
		t.Fatalf("a trip with no last success carries %v", got.Since)
	}
	atClock(t, carried.Now.Add(time.Hour))
	if got := scrapeClockOf(map[string]string{"param_x": "1"}); !got.Now.Equal(carried.Now.Add(time.Hour)) || !got.Since.IsZero() {
		t.Fatalf("outside a trip the clock is %+v", got)
	}
}
