package transform

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A metric rule's time_format says the text its expression gives is a time,
// and the series' value is that time in Unix seconds, its fraction kept: the
// date a report was written on, the time a job last ran. It is a name —
// rfc3339, rfc1123 — or a layout: the reference time written the way the
// text writes its times, as Go's time package, Promtail and Telegraf take
// theirs. time_zone is the zone a text that names none of its own is read
// in, UTC unless set. scale applies to the seconds; value_map does not go
// with it, since each of the two turns the text into the number.

// The names a time_format can have in place of a layout.
const (
	timeFormatRFC3339 = "rfc3339"
	timeFormatRFC1123 = "rfc1123"
)

// timeReference is the time a layout is written with.
const timeReference = "Mon Jan 2 15:04:05 MST 2006"

// namedTimeLayout is the layout a time_format that is a name reads text
// with. rfc3339 reads fractional seconds though its layout has none, as any
// layout does whose seconds have no zeros after them. rfc1123 is the date of
// an HTTP header, whose zone is GMT, and the same with a numeric zone, which
// the text's last character tells apart.
func namedTimeLayout(format, text string) (string, bool) {
	switch {
	case strings.EqualFold(format, timeFormatRFC3339):
		return time.RFC3339, true
	case strings.EqualFold(format, timeFormatRFC1123):
		if n := len(text); n > 0 && text[n-1] >= '0' && text[n-1] <= '9' {
			return time.RFC1123Z, true
		}
		return time.RFC1123, true
	}
	return "", false
}

// ruleTime is the text a rule with time_format extracted as the series'
// value: the time it writes, in Unix seconds, scaled. The text is trimmed as
// value_map's is. Text that is no time in the format is the rule's failure,
// naming the text and the format.
func ruleTime(rule model.MetricRule, text string) (float64, error) {
	text = strings.TrimSpace(text)
	layout, named := namedTimeLayout(rule.TimeFormat, text)
	if !named {
		layout = rule.TimeFormat
	}
	zone, err := timeLocation(rule.TimeZone)
	if err != nil {
		return 0, err
	}
	t, err := time.ParseInLocation(layout, text, zone)
	if err != nil {
		return 0, unreadTime(rule.TimeFormat, text, named, err)
	}
	seconds := t.Unix()
	// A zone the text names by an abbreviation, EET or PST, is one of
	// time_zone's own, or UTC or GMT. Any other the time package reads as a
	// zone of that name at UTC, hours off without a word: an abbreviation
	// alone says no offset, so that is the rule's failure instead. GMT with
	// hours after it, GMT+3 or GMT-5, does say its offset, and the time
	// package gives the zone it makes up that offset but leaves the time
	// where UTC would have it, so the hours are taken off here: 09:00 GMT+3
	// is 06:00 UTC. For GMT alone they are none.
	if location := t.Location(); location != zone && location != time.UTC && !hasNumericZone(layout) {
		switch name, offset := t.Zone(); {
		case strings.HasPrefix(name, "GMT"):
			seconds -= int64(offset)
		case offset == 0 && name != "":
			return 0, fmt.Errorf("value %s names the time zone %s, whose offset is not known: an abbreviation is read as one of time_zone's own, UTC or GMT; set time_zone to the zone the text is written in, such as Europe/Sofia", model.QuoteValue(text), name)
		}
	}
	return scaled(rule, float64(seconds)+float64(t.Nanosecond())/1e9), nil
}

// hasNumericZone reports whether a layout reads a numeric zone, -0700,
// -07:00, Z07:00 and their like, which all start one of two ways. Such a
// zone says its offset, whatever abbreviation stands beside it.
func hasNumericZone(layout string) bool {
	return strings.Contains(layout, "-07") || strings.Contains(layout, "Z07")
}

// unreadTime is the failure of text that is no time in a time_format. It
// says what is wrong in the exporter's own words and quotes the text cut as
// every such error cuts it, where the time package's error quotes all of it;
// only a part out of its range, the 30th of February, is named as the
// package names it.
func unreadTime(format, text string, named bool, err error) error {
	var parse *time.ParseError
	if errors.As(err, &parse) && strings.HasSuffix(parse.Message, "out of range") {
		return fmt.Errorf("value %s is not a time in time_format %q: %s", model.QuoteValue(text), format, strings.TrimPrefix(parse.Message, ": "))
	}
	switch {
	case !named:
		return fmt.Errorf("value %s is not a time in time_format %q; write the layout as the text writes the reference time, %s", model.QuoteValue(text), format, timeReference)
	case strings.EqualFold(format, timeFormatRFC3339):
		return fmt.Errorf("value %s is not a time in time_format %q, which reads times such as 2006-01-02T15:04:05Z and 2006-01-02T15:04:05.999+02:00", model.QuoteValue(text), format)
	}
	return fmt.Errorf("value %s is not a time in time_format %q, which reads times such as Mon, 02 Jan 2006 15:04:05 GMT and Mon, 02 Jan 2006 15:04:05 +0200", model.QuoteValue(text), format)
}

// timeZones holds every named zone a rule has asked for, so a zone's rules
// are read from its data once and not for every value. The map is replaced,
// never changed, so reading it takes no lock.
var (
	timeZones   atomic.Pointer[map[string]*time.Location]
	timeZonesMu sync.Mutex
)

// timeLocation is the zone a time_zone names: UTC when it names none, and
// otherwise a zone of the IANA database, which the binary carries
// (time/tzdata, in the main package) for a host that has no zone files.
// Local is refused: it would read the same text differently on each host.
func timeLocation(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	if known := timeZones.Load(); known != nil {
		if zone, ok := (*known)[name]; ok {
			return zone, nil
		}
	}
	zone, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return nil, fmt.Errorf("time_zone %q is not a time zone the exporter knows; write an IANA name, such as Europe/Sofia or America/New_York, or UTC", name)
	}
	timeZonesMu.Lock()
	defer timeZonesMu.Unlock()
	zones := map[string]*time.Location{}
	if known := timeZones.Load(); known != nil {
		for other, location := range *known {
			zones[other] = location
		}
	}
	// The zone another caller stored meanwhile is kept, so every rule of a
	// zone holds the one location, which ruleTime compares by.
	if stored, ok := zones[name]; ok {
		return stored, nil
	}
	zones[name] = zone
	timeZones.Store(&zones)
	return zone, nil
}

// timeProbe is a time each part of which is written differently from the
// reference time's, whatever element writes it: year, month, day, weekday,
// hour on either clock and its half of the day, minute, second, fraction
// and zone. A layout writes it as itself only when it has no element at
// all, and reads back from what it wrote the parts it has.
var timeProbe = time.Date(2001, time.February, 3, 4, 5, 6, 789000000, time.UTC)

// timeLayoutIs says what a layout is, at the end of every refusal of one that
// may well be something else: a sample date, the name of another format, a
// layout of another convention.
const timeLayoutIs = "a layout is the reference time, " + timeReference + `, written as the text writes its times, such as "2006-01-02 15:04:05" for text like 2026-10-03 09:00:00`

// checkTimeLayout checks a time_format at load: a name, or a layout with a
// year, a month and a day. A layout in another convention, yyyy-mm-dd or
// %Y-%m-%d, has no element of the reference time and would read no text at
// all, and one without a whole date reads a time of the year 0 or of the
// first of a month, which is no Unix time anyone meant. A layout with blanks
// around it reads no text either, since the text is read without its own,
// and one with the hour of a 12-hour clock and no PM reads the morning only.
func checkTimeLayout(format string) error {
	if _, named := namedTimeLayout(format, ""); named {
		return nil
	}
	written := timeProbe.Format(format)
	if written == format {
		return fmt.Errorf("time_format %q is neither the name %s or %s nor a layout: %s", format, timeFormatRFC3339, timeFormatRFC1123, timeLayoutIs)
	}
	if strings.TrimSpace(format) != format {
		return fmt.Errorf("time_format %q begins or ends with a blank, which the text never does: it is read without its surrounding blanks, so write the layout without them", format)
	}
	read, err := time.Parse(format, written)
	if err != nil {
		return fmt.Errorf("time_format %q cannot read a time it writes, %q, so keep its elements apart: %s", format, written, timeLayoutIs)
	}
	var missing []string
	if read.Year() != timeProbe.Year() {
		missing = append(missing, "year")
	}
	if read.Month() != timeProbe.Month() {
		missing = append(missing, "month")
	}
	if read.Day() != timeProbe.Day() {
		missing = append(missing, "day")
	}
	if len(missing) > 0 {
		list := missing[0]
		if len(missing) > 1 {
			list = strings.Join(missing[:len(missing)-1], ", ") + " or " + missing[len(missing)-1]
		}
		return fmt.Errorf("time_format %q has no %s: a time without a date has no Unix time, so a layout needs the year (2006), the month (01 or Jan) and the day (02); %s", format, list, timeLayoutIs)
	}
	// The probe twelve hours on, in the afternoon, is written with the same
	// hour as the probe by a 12-hour clock, and read back as the morning's
	// unless the layout has PM to tell the two apart.
	afternoon := timeProbe.Add(12 * time.Hour)
	if again, err := time.Parse(format, afternoon.Format(format)); err == nil && again.Hour() == timeProbe.Hour() {
		return fmt.Errorf("time_format %q writes the hour on a 12-hour clock (03 or 3) without PM, so it would read no afternoon: write the hour as 15 for a 24-hour clock, or add PM for a 12-hour one", format)
	}
	return nil
}

// checkTimeRules checks a rule's time_format and time_zone at load.
func checkTimeRules(x *model.Collector, r *model.MetricRule, where string) error {
	if r.TimeFormat == "" {
		if r.TimeZone != "" {
			return fmt.Errorf("%s sets time_zone without time_format; time_zone is the zone a time_format reads text in that names no zone of its own", where)
		}
		return nil
	}
	switch x.Transform.Type {
	case "python":
		return fmt.Errorf("%s sets time_format, which the python transform does not use: its script sets each value with metric(...)", where)
	case "prometheus":
		return fmt.Errorf("%s sets time_format, which the prometheus transform does not use: its values are numbers already; scale applies", where)
	}
	if r.ValueMap != nil {
		return fmt.Errorf("%s sets both time_format and value_map; each turns the text into the value, so a rule takes one of them", where)
	}
	if err := checkTimeLayout(r.TimeFormat); err != nil {
		return fmt.Errorf("%s %w", where, err)
	}
	if _, err := timeLocation(r.TimeZone); err != nil {
		return fmt.Errorf("%s %w", where, err)
	}
	return nil
}
