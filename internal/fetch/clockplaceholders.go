package fetch

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
// this order, the last success it starts from, an offset after a colon, a
// truncation after a slash, and a time zone after an at sign:
//
//	{{name[~last_success(fallback)][:offset][/unit][@zone][|filter]}}
//
// A placeholder starts from the time of the trip, which every placeholder
// of the trip reads alike (ScrapeClock), so that {{date}} in a path and
// {{datetime}} in a query are of the same moment, and a retry or a page
// after the first is sent the same values. ~last_success starts from the
// time of the last successful trip of the same collector, target and
// parameters instead, which the exporter keeps; while there is none, after
// a start or for a new target, it starts from the trip's time moved by the
// fallback, an offset into the past such as -1h. Two placeholders,
// from={{datetime~last_success(-1h)}}&to={{datetime}}, then ask for each
// stretch of time once, with no gap between a trip and the next that
// succeeded.
//
// The offset, as the fallback, is a sign and one or more amounts of w (weeks), d (days), h,
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

// ScrapeClock is the clock of one trip: Now, read once when the trip
// starts, and Since, the Now of the last successful trip of the same
// collector, target and parameters, zero when none is known.
type ScrapeClock struct {
	Now, Since time.Time
}

// NewScrapeClock is a trip's clock, read now, with since the last success
// known, zero for none.
func NewScrapeClock(since time.Time) ScrapeClock {
	return ScrapeClock{Now: clockNow(), Since: since}
}

type scrapeClockKey struct{}

// WithScrapeClock returns a context whose trip fills its clock
// placeholders from clock (FetchCollector).
func WithScrapeClock(ctx context.Context, clock ScrapeClock) context.Context {
	return context.WithValue(ctx, scrapeClockKey{}, clock)
}

// scrapeClockFrom is the trip's clock its context carries, or one read now
// with no last success.
func scrapeClockFrom(ctx context.Context) ScrapeClock {
	if clock, ok := ctx.Value(scrapeClockKey{}).(ScrapeClock); ok {
		return clock
	}
	return NewScrapeClock(time.Time{})
}

// The trip's clock reaches its placeholders inside its parameters, under
// keys no probe parameter can have: every place that fills a placeholder
// is handed the parameters, and a probe parameter is param_<name>.
const (
	clockNowParam   = "\x00now"
	clockSinceParam = "\x00since"
)

// isClockParam reports whether a parameter's name is one the trip's clock
// is carried under, which no check of the probe's parameters looks at.
func isClockParam(name string) bool {
	return strings.HasPrefix(name, "\x00")
}

// withScrapeClock is params with the trip's clock among them, a copy: the
// probe's own are shared with whatever else reads them.
func withScrapeClock(params map[string]string, clock ScrapeClock) map[string]string {
	out := make(map[string]string, len(params)+2)
	for name, value := range params {
		out[name] = value
	}
	out[clockNowParam] = strconv.FormatInt(clock.Now.UnixNano(), 10)
	if !clock.Since.IsZero() {
		out[clockSinceParam] = strconv.FormatInt(clock.Since.UnixNano(), 10)
	}
	return out
}

// scrapeClockOf is the trip's clock params carry, read now with no last
// success when they carry none: a request filled in outside a trip, to be
// shown or checked.
func scrapeClockOf(params map[string]string) ScrapeClock {
	read := func(name string) time.Time {
		n, err := strconv.ParseInt(params[name], 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(0, n)
	}
	clock := ScrapeClock{Now: read(clockNowParam), Since: read(clockSinceParam)}
	if clock.Now.IsZero() {
		clock.Now = clockNow()
	}
	return clock
}

// clockPlaceholder is a parsed clock placeholder.
type clockPlaceholder struct {
	format string
	// since starts it from the last success, and fallback from the trip's
	// time moved back, while there is none.
	since    bool
	fallback clockOffset
	offset   clockOffset
	truncate string
	zone     *time.Location
}

// clockOffset is an offset or a fallback: days, weeks among them, are
// calendar days in the zone, the rest elapsed time.
type clockOffset struct {
	days    int
	elapsed time.Duration
}

// from is t moved by the offset. Without days it is t and the elapsed
// time alone: AddDate reads the wall clock and makes a time of it again,
// which in the hour a zone repeats when its summer time ends is the hour's
// second pass whichever t was in, an hour late for the whole first pass.
func (o clockOffset) from(t time.Time) time.Time {
	if o.days != 0 {
		t = t.AddDate(0, 0, o.days)
	}
	return t.Add(o.elapsed)
}

// startOfDay is the first moment of t's day in its zone. Where the zone's
// clock is set back over midnight the day's first midnight is the earlier
// one, and where it is set forward over midnight, so that the day has no
// 00:00, the moment the day starts at, as 01:00: time.Date alone says
// neither which it gives.
func startOfDay(t time.Time) time.Time {
	year, month, day := t.Date()
	wall := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	guess := time.Date(year, month, day, 0, 0, 0, 0, t.Location())
	// Midnight on the wall clock under each offset the zone has around the
	// day: the day starts at the earliest of them that is in the day.
	start := t
	for _, near := range []time.Time{guess.Add(-24 * time.Hour), guess, guess.Add(24 * time.Hour)} {
		_, offset := near.Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second).In(t.Location())
		if y, m, d := candidate.Date(); y == year && m == month && d == day && candidate.Before(start) {
			start = candidate
		}
	}
	return start
}

var clockPattern = regexp.MustCompile(`^([a-z_]+)(~[a-z_]+(?:\(([^)]*)\))?)?(?::([+-][0-9a-z]+))?(?:/([a-z]+))?(?:@(.+))?$`)

var (
	clockOffsetPattern = regexp.MustCompile(`^[+-](?:[0-9]+[wdhms])+$`)
	clockOffsetPart    = regexp.MustCompile(`([0-9]+)([wdhms])`)
)

// clockNameAt reports whether text, what follows a {{, begins with the
// name of a clock format followed by what may follow one in a placeholder:
// the closing braces, a last success, an offset, a truncation, a zone or a
// filter.
func clockNameAt(text string) bool {
	end := 0
	for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' || text[end] == '_') {
		end++
	}
	if _, ok := clockFormats[text[:end]]; !ok || end == len(text) {
		return false
	}
	return strings.ContainsRune("~:/@|", rune(text[end])) || strings.HasPrefix(text[end:], "}}")
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
		return nil, true, fmt.Errorf("%s placeholder {{%s}} is a clock placeholder written wrong; write {{name[~last_success(fallback)][:offset][/unit][@zone]}}, each part optional and in that order, such as {{date:-1d}}, {{datetime:-1d/d}}, {{date@Europe/Sofia}} or {{datetime~last_success(-1h)}}", where, inner)
	}
	if _, known := clockFormats[m[1]]; !known {
		return nil, false, nil
	}
	c = &clockPlaceholder{format: m[1], zone: time.UTC}
	if since := m[2]; since != "" {
		if !strings.HasPrefix(since, "~last_success(") {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} starts from %q; the one time a clock placeholder starts from besides the trip's is the last success, written ~last_success(fallback), such as ~last_success(-1h)", where, inner, since)
		}
		if !strings.HasPrefix(m[3], "-") {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} has the fallback %q; ~last_success needs one, the time before the trip a window starts at while no trip has succeeded, such as ~last_success(-1h)", where, inner, m[3])
		}
		if c.fallback, err = parseClockOffset(where, inner, "fallback", m[3]); err != nil {
			return nil, true, err
		}
		c.since = true
	}
	if offset := m[4]; offset != "" {
		if c.offset, err = parseClockOffset(where, inner, "offset", offset); err != nil {
			return nil, true, err
		}
	}
	if unit := m[5]; unit != "" {
		if unit != "d" && unit != "h" && unit != "m" {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} truncates to %q; truncate to d, h or m, the start of the day, hour or minute", where, inner, unit)
		}
		c.truncate = unit
	}
	if zone := m[6]; zone != "" {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return nil, true, fmt.Errorf("%s placeholder {{%s}} has the time zone %q, which is no IANA time zone name, such as Europe/Sofia, UTC or Local", where, inner, zone)
		}
		c.zone = loc
	}
	return c, true, nil
}

// parseClockOffset reads an offset or a fallback, what, of the placeholder
// inner.
func parseClockOffset(where, inner, what, text string) (clockOffset, error) {
	var o clockOffset
	if !clockOffsetPattern.MatchString(text) {
		return o, fmt.Errorf("%s placeholder {{%s}} has the %s %q; an offset is + or - and amounts of w, d, h, m and s, such as -1d, +2h or -1d12h", where, inner, what, text)
	}
	sign := 1
	if text[0] == '-' {
		sign = -1
	}
	for _, p := range clockOffsetPart.FindAllStringSubmatch(text[1:], -1) {
		n, err := strconv.Atoi(p[1])
		if err != nil || n > 100000 {
			return o, fmt.Errorf("%s placeholder {{%s}} has the %s %q, more than 100000 of a unit", where, inner, what, text)
		}
		switch p[2] {
		case "w":
			o.days += sign * 7 * n
		case "d":
			o.days += sign * n
		case "h":
			o.elapsed += time.Duration(sign*n) * time.Hour
		case "m":
			o.elapsed += time.Duration(sign*n) * time.Minute
		case "s":
			o.elapsed += time.Duration(sign*n) * time.Second
		}
	}
	return o, nil
}

// at is the value of the placeholder on the trip's clock.
func (c *clockPlaceholder) at(clock ScrapeClock) string {
	t := clock.Now.In(c.zone)
	if c.since {
		if clock.Since.IsZero() {
			t = c.fallback.from(t)
		} else {
			t = clock.Since.In(c.zone)
		}
	}
	t = c.offset.from(t)
	switch c.truncate {
	// The start of the hour and of the minute are t less what its wall
	// clock shows past them, not a time made of the wall clock again: that
	// would be an hour off in one pass of an hour the zone repeats.
	case "d":
		t = startOfDay(t)
	case "h":
		t = t.Add(-time.Duration(t.Minute())*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
	case "m":
		t = t.Add(-time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
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
// checked: what it writes at clockSample, with no last success.
func (c *clockPlaceholder) sample() string {
	return c.at(ScrapeClock{Now: clockSample})
}

// ClockUseOf is what the request of c holds of clock placeholders, for
// Collector.Clock: its path's, its body's, header and query values' and
// its type's own fields'. A request whose placeholders cannot be parsed
// holds none; the configuration refuses it before it gets here.
func ClockUseOf(c *model.Collector) model.ClockUse {
	var use model.ClockUse
	note := func(placeholders []pathPlaceholder) {
		for _, p := range placeholders {
			if p.Clock != nil {
				use.Any = true
				use.Since = use.Since || p.Clock.since
			}
		}
	}
	if HasPathParams(c.Request.Path) {
		found, _ := parsePathParams(c.Request.Path)
		note(found)
	}
	for _, f := range requestTemplates(c, RequestOverrides{}) {
		found, _ := f.parse()
		note(found)
	}
	return use
}

// A static target's parameter value may hold clock placeholders, filled in
// at each of its trips: params: {param_day: "{{date@Europe/Sofia}}"} gives
// a collector's {{param_day}} the target's own day. It holds no other
// placeholder, and takes no filter: the placeholder it fills writes it as
// it writes every value.

// targetParamUnfiltered is why a static target's parameter value takes no
// filter.
const targetParamUnfiltered = "the value fills a placeholder of the collector, which writes it as its place writes every value"

// targetParamPlaceholders are the placeholders of a static target's
// parameter value.
func targetParamPlaceholders(name, value string) ([]pathPlaceholder, error) {
	return parsePlaceholdersOf("params."+name, value, false, false, targetParamUnfiltered)
}

// renderTargetParams is a static target's parameters with the clock
// placeholders of their values filled in from clock, a copy.
func renderTargetParams(params map[string]string, clock ScrapeClock) (map[string]string, error) {
	out := make(map[string]string, len(params))
	for name, value := range params {
		out[name] = value
		if !strings.Contains(value, "{{") {
			continue
		}
		placeholders, err := targetParamPlaceholders(name, value)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		previous := 0
		for _, p := range placeholders {
			if p.Clock == nil {
				continue
			}
			b.WriteString(value[previous:p.start])
			b.WriteString(p.Clock.at(clock))
			previous = p.end
		}
		b.WriteString(value[previous:])
		out[name] = b.String()
	}
	return out, nil
}

// CheckTargetParams refuses, when the static targets load, a target's
// parameter value with a placeholder written wrong, with a {{param_...}}
// placeholder, which nothing fills, or with a clock placeholder when the
// parameter fills a label value of c, which would make a series of each
// day, hour or second.
func CheckTargetParams(c *model.Collector, t *model.StaticTarget) error {
	var labelled map[string]bool
	for _, name := range model.SortedKeys(t.Params) {
		value := t.Params[name]
		if !strings.Contains(value, "{{") {
			continue
		}
		placeholders, err := targetParamPlaceholders(name, value)
		if err != nil {
			return fmt.Errorf("target %q %w", t.Name, err)
		}
		for _, p := range placeholders {
			if p.Clock == nil {
				return fmt.Errorf("target %q params.%s holds {{%s}}; a static target's parameter value may hold clock placeholders, such as {{date}}, which each scrape fills in, and no {{param_...}} one, which nothing fills: write the value out in full", t.Name, name, p.Name)
			}
		}
		if len(placeholders) == 0 {
			continue
		}
		if labelled == nil {
			labelled = map[string]bool{}
			for _, p := range labelPlaceholders(c.LabelParams) {
				labelled[p.Name] = true
			}
		}
		if labelled[name] {
			return fmt.Errorf("target %q params.%s holds a clock placeholder, and fills a label value of collector %q: a label whose value changes with the clock makes a new series every day, hour or second; give the label a value of its own", t.Name, name, c.Name)
		}
	}
	return nil
}

// TargetClockUse is what a static target's parameter values hold of clock
// placeholders; a value written wrong holds none, which CheckTargetParams
// refuses.
func TargetClockUse(t *model.StaticTarget) model.ClockUse {
	var use model.ClockUse
	for name, value := range t.Params {
		if !strings.Contains(value, "{{") {
			continue
		}
		placeholders, _ := targetParamPlaceholders(name, value)
		for _, p := range placeholders {
			if p.Clock != nil {
				use.Any = true
				use.Since = use.Since || p.Clock.since
			}
		}
	}
	return use
}
