package fetch

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Clock placeholders fill in the time of the scrape, where a request needs
// a date or a time that no probe parameter could give: the day an API's
// daily summary is asked for, the window of a history, a timestamp a
// signed request carries.
//
//	request:
//	  path: /usersummary/daily/{{param_user}}
//	  query:
//	    calendarDate: "{{date@Europe/Sofia}}"
//	    from: "{{datetime:-1d/d}}"
//	    to: "{{datetime/d}}"
//
// A clock placeholder is its format's name, then, each optional and in
// this order, an offset from now after a colon, a truncation after a slash,
// and a time zone after an at sign:
//
//	{{name[:offset][/unit][@zone][|filter]}}
//
// The offset is a sign and one or more amounts of w (weeks), d (days), h,
// m and s, such as -1d, +2h or -1d12h. Weeks and days are calendar days in
// the zone, so -1d is yesterday's date also across a daylight saving
// change; hours, minutes and seconds are elapsed time. The truncation, d,
// h or m, is the start of the day, hour or minute the moment falls in, in
// the zone, after the offset. The zone is an IANA name, UTC when left out,
// or Local, the exporter's own. The filter is a body placeholder's, as for
// {{param_...}} (requesttemplate.go).
//
// A clock placeholder stands where a {{param_...}} one may, in a request,
// and its value is written as that place writes every value. It is no
// probe parameter: no probe or static target fills it, and none may be
// given for it. A label value takes none, since a series a day, or a
// second, is no series at all.

// clockFormats are the formats a clock placeholder is named by, each the
// layout Go writes a time with, or, for unix and unix_ms, the seconds or
// milliseconds since the epoch.
var clockFormats = map[string]string{
	"date":         "2006-01-02",
	"date_compact": "20060102",
	"date_eu":      "02.01.2006",
	"date_us":      "01/02/2006",
	"month":        "2006-01",
	"year":         "2006",
	"time":         "15:04:05",
	"datetime":     time.RFC3339,
	"datetime_ms":  "2006-01-02T15:04:05.000Z07:00",
	"unix":         "",
	"unix_ms":      "",
}

// clockFormatNames are the names of clockFormats, sorted, for the errors
// that list them.
func clockFormatNames() []string {
	names := make([]string, 0, len(clockFormats))
	for name := range clockFormats {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// clockNow is the clock a placeholder reads, set by tests.
var clockNow = time.Now

// clockSample is the moment a clock placeholder stands for when the
// configuration is checked (clockPlaceholder.sample): a value of every
// format, each of its parts unlike the others, which a check of the place
// it stands in holds as it holds a default.
var clockSample = time.Date(2026, time.November, 9, 13, 4, 5, 6000000, time.UTC)

// clockPlaceholder is a parsed clock placeholder.
type clockPlaceholder struct {
	format   string
	days     int // weeks and days, calendar days in zone
	elapsed  time.Duration
	truncate string
	zone     *time.Location
}

var clockPattern = regexp.MustCompile(`^([a-z_]+)(?::([+-][0-9a-z]+))?(?:/([a-z]+))?(?:@(.+))?$`)

var (
	clockOffset     = regexp.MustCompile(`^[+-](?:[0-9]+[wdhms])+$`)
	clockOffsetPart = regexp.MustCompile(`([0-9]+)([wdhms])`)
)

// clockNameAt reports whether text, what follows a {{, begins with the
// name of a clock format followed by what may follow one in a placeholder:
// the closing braces, an offset, a truncation, a zone or a filter.
func clockNameAt(text string) bool {
	end := 0
	for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' || text[end] == '_') {
		end++
	}
	if _, ok := clockFormats[text[:end]]; !ok || end == len(text) {
		return false
	}
	return strings.ContainsRune(":/@|", rune(text[end])) || strings.HasPrefix(text[end:], "}}")
}

// parseClock reads inner, a placeholder's text between its braces without
// its filter, as a clock placeholder. ok is false when inner names no clock
// format, so that it is read as what else it may be; an error is a clock
// placeholder written wrong.
func parseClock(where, inner string) (c *clockPlaceholder, ok bool, err error) {
	m := clockPattern.FindStringSubmatch(inner)
	if m == nil {
		if !clockNameAt(inner + "}}") {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("%s placeholder {{%s}} is a clock placeholder written wrong; write {{name[:offset][/unit][@zone]}}, each part optional and in that order, such as {{date:-1d}}, {{datetime:-1d/d}} or {{date@Europe/Sofia}}", where, inner)
	}
	if _, known := clockFormats[m[1]]; !known {
		return nil, false, nil
	}
	c = &clockPlaceholder{format: m[1], zone: time.UTC}
	if offset := m[2]; offset != "" {
		if !clockOffset.MatchString(offset) {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} has the offset %q; an offset is + or - and amounts of w, d, h, m and s, such as -1d, +2h or -1d12h", where, inner, offset)
		}
		sign := 1
		if offset[0] == '-' {
			sign = -1
		}
		for _, p := range clockOffsetPart.FindAllStringSubmatch(offset[1:], -1) {
			n, err := strconv.Atoi(p[1])
			if err != nil || n > 100000 {
				return nil, true, fmt.Errorf("%s placeholder {{%s}} has the offset %q, more than 100000 of a unit", where, inner, offset)
			}
			switch p[2] {
			case "w":
				c.days += sign * 7 * n
			case "d":
				c.days += sign * n
			case "h":
				c.elapsed += time.Duration(sign*n) * time.Hour
			case "m":
				c.elapsed += time.Duration(sign*n) * time.Minute
			case "s":
				c.elapsed += time.Duration(sign*n) * time.Second
			}
		}
	}
	if unit := m[3]; unit != "" {
		if unit != "d" && unit != "h" && unit != "m" {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} truncates to %q; truncate to d, h or m, the start of the day, hour or minute", where, inner, unit)
		}
		c.truncate = unit
	}
	if zone := m[4]; zone != "" {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} has the time zone %q, which is no IANA time zone name, such as Europe/Sofia, UTC or Local", where, inner, zone)
		}
		c.zone = loc
	}
	return c, true, nil
}

// at is the value of the placeholder at now.
func (c *clockPlaceholder) at(now time.Time) string {
	t := now.In(c.zone)
	t = t.AddDate(0, 0, c.days).Add(c.elapsed)
	switch c.truncate {
	case "d":
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, c.zone)
	case "h":
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, c.zone)
	case "m":
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, c.zone)
	}
	switch c.format {
	case "unix":
		return strconv.FormatInt(t.Unix(), 10)
	case "unix_ms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	return t.Format(clockFormats[c.format])
}

// sample is the value the placeholder stands for when the configuration is
// checked: what it writes at clockSample.
func (c *clockPlaceholder) sample() string {
	return c.at(clockSample)
}
