package transform

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	// The zones the tests name, on a host without zone files, as the main
	// package carries them for the binary.
	_ "time/tzdata"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// unix is the Unix seconds of a time written as RFC 3339.
func unix(t *testing.T, written string) float64 {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, written)
	if err != nil {
		t.Fatal(err)
	}
	return float64(at.Unix()) + float64(at.Nanosecond())/1e9
}

// A rule with time_format reads the text as a time and gives its Unix
// seconds: by the two names, in either case, with and without a fraction and
// with the zone the text gives; by a layout of the reference time; in
// time_zone when the text names no zone, through a change of clocks; with
// the fraction kept and the blanks around the text dropped.
func TestTimeFormatReadsTextAsUnixSeconds(t *testing.T) {
	for _, tc := range []struct {
		format, zone, text, want string
	}{
		{"rfc3339", "", "2026-10-03T09:00:00Z", "2026-10-03T09:00:00Z"},
		{"rfc3339", "", "2026-10-03T09:00:00.25Z", "2026-10-03T09:00:00.25Z"},
		{"rfc3339", "", "2026-10-03T12:00:00.123456789+03:00", "2026-10-03T09:00:00.123456789Z"},
		{"rfc3339", "", "2026-10-03T04:30:00-04:30", "2026-10-03T09:00:00Z"},
		{"RFC3339", "", "2026-10-03T09:00:00Z", "2026-10-03T09:00:00Z"},
		// The text gives its zone, so time_zone has nothing to say.
		{"rfc3339", "Europe/Sofia", "2026-10-03T09:00:00Z", "2026-10-03T09:00:00Z"},
		{"rfc1123", "", "Sat, 03 Oct 2026 09:00:00 GMT", "2026-10-03T09:00:00Z"},
		{"rfc1123", "", "Sat, 03 Oct 2026 09:00:00 UTC", "2026-10-03T09:00:00Z"},
		{"rfc1123", "", "Sat, 03 Oct 2026 12:00:00 +0300", "2026-10-03T09:00:00Z"},
		{"RFC1123", "", "Sat, 03 Oct 2026 02:00:00 -0700", "2026-10-03T09:00:00Z"},
		{"rfc1123", "Europe/Sofia", "Sat, 03 Oct 2026 12:00:00 EEST", "2026-10-03T09:00:00Z"},
		{"rfc1123", "Europe/Sofia", "Sat, 03 Jan 2026 11:00:00 EET", "2026-01-03T09:00:00Z"},
		{"rfc1123", "Europe/Sofia", "Sat, 03 Oct 2026 09:00:00 GMT", "2026-10-03T09:00:00Z"},

		{"2006-01-02", "", "2026-10-02", "2026-10-02T00:00:00Z"},
		{"2006/01/02 15:04", "", "2026/10/03 09:00", "2026-10-03T09:00:00Z"},
		{"02.01.2006 15:04:05", "", "03.10.2026 09:00:07", "2026-10-03T09:00:07Z"},
		{"2006-01-02 15:04:05", "", "2026-10-03 09:00:07.5", "2026-10-03T09:00:07.5Z"},
		{"2006-01-02T15:04:05.000Z07:00", "", "2026-10-03T12:00:07.250+03:00", "2026-10-03T09:00:07.25Z"},
		{"Jan 2, 2006 3:04 PM", "", "Oct 3, 2026 9:05 PM", "2026-10-03T21:05:00Z"},
		{"20060102", "", "20261003", "2026-10-03T00:00:00Z"},
		{"20060102T150405Z07:00", "", "20261003T090007Z", "2026-10-03T09:00:07Z"},
		{"20060102T150405Z07:00", "", "20261003T120007+03:00", "2026-10-03T09:00:07Z"},
		{"2006-01-02 15:04:05 -07:00", "", "2026-10-03 12:00:07 +03:00", "2026-10-03T09:00:07Z"},
		{"2006-002", "", "2026-276", "2026-10-03T00:00:00Z"},
		{"06-01-02", "", "26-10-03", "2026-10-03T00:00:00Z"},
		// A year of two digits is one of 1969 to 2068.
		{"06-01-02", "", "68-12-31", "2068-12-31T00:00:00Z"},
		{"06-01-02", "", "69-01-01", "1969-01-01T00:00:00Z"},
		{"06-01-02", "", "99-12-31", "1999-12-31T00:00:00Z"},
		{"06-01-02", "", "00-01-01", "2000-01-01T00:00:00Z"},
		{"2006-01-02 15:04 MST", "", "2026-10-03 09:00 UTC", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04 -0700 MST", "Europe/Sofia", "2026-10-03 09:00 +0000 WET", "2026-10-03T09:00:00Z"},
		{"2006-01-02", "", "1969-12-31", "1969-12-31T00:00:00Z"},
		{"2006-01-02 15:04:05", "", "1969-12-31 23:59:59.5", "1969-12-31T23:59:59.5Z"},

		// A text without a zone is read in time_zone: UTC unless set, and
		// in a named zone by the offset it has on that day.
		{"2006-01-02 15:04", "UTC", "2026-10-03 09:00", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04", "Europe/Sofia", "2026-07-01 12:00", "2026-07-01T09:00:00Z"},
		{"2006-01-02 15:04", "Europe/Sofia", "2026-01-01 12:00", "2026-01-01T10:00:00Z"},
		{"2006-01-02 15:04", "Europe/Sofia", "2026-10-25 02:59", "2026-10-24T23:59:00Z"},
		{"2006-01-02 15:04", "Europe/Sofia", "2026-10-25 04:00", "2026-10-25T02:00:00Z"},
		// At the change itself a local time is one of two moments or none,
		// and is read as the time package's Date reads it, which promises
		// no more than one of the two offsets. In Europe/Sofia 03:30 of the
		// October night, which the clocks show twice, is the second, in
		// winter time, and 03:30 of the March night, which they skip, is the
		// moment an hour after 02:30: the documentation names these two.
		{"2006-01-02 15:04", "Europe/Sofia", "2026-10-25 03:30", "2026-10-25T01:30:00Z"},
		{"2006-01-02 15:04", "Europe/Sofia", "2026-03-29 02:30", "2026-03-29T00:30:00Z"},
		{"2006-01-02 15:04", "Europe/Sofia", "2026-03-29 03:30", "2026-03-29T01:30:00Z"},
		{"2006-01-02", "America/New_York", "2026-10-03", "2026-10-03T04:00:00Z"},
		{"2006-01-02 15:04", "Asia/Kolkata", "2026-10-03 14:30", "2026-10-03T09:00:00Z"},

		{"2006-01-02", "", "  2026-10-02\n", "2026-10-02T00:00:00Z"},
	} {
		rule := model.MetricRule{Name: "at", TimeFormat: tc.format, TimeZone: tc.zone}
		want := unix(t, tc.want)
		got, err := ruleTextValue(rule, tc.text)
		if err != nil || got != want {
			t.Errorf("%q as %q in %q: got %v, %v; want %v", tc.text, tc.format, tc.zone, strconv.FormatFloat(got, 'f', -1, 64), err, strconv.FormatFloat(want, 'f', -1, 64))
		}
		// The same text as a value of any type, as jq and a CSV row hand it.
		if got, err := ruleValue(rule, tc.text); err != nil || got != want {
			t.Errorf("%q as a value: got %v, %v; want %v", tc.text, got, err, want)
		}
	}
	// Seconds as they are written, to the fraction a float64 holds beside
	// them: a quarter second exactly.
	if got, _ := ruleTextValue(model.MetricRule{TimeFormat: "rfc3339"}, "2026-10-03T09:00:00.25Z"); got != 1791018000.25 {
		t.Errorf("got %v, want 1791018000.25", strconv.FormatFloat(got, 'f', -1, 64))
	}
}

// scale multiplies the seconds a time_format read, so a rule gives
// milliseconds or days as well.
func TestScaleAppliesAfterTimeFormat(t *testing.T) {
	thousand, perDay := 1000.0, 1.0/86400
	if got, err := ruleTextValue(model.MetricRule{TimeFormat: "2006-01-02 15:04:05", Scale: &thousand}, "2026-10-03 09:00:00.5"); err != nil || got != 1791018000500 {
		t.Errorf("milliseconds: %v, %v", got, err)
	}
	if got, err := ruleTextValue(model.MetricRule{TimeFormat: "2006-01-02", Scale: &perDay}, "1970-01-11"); err != nil || got != 10 {
		t.Errorf("days: %v, %v", got, err)
	}
}

// Text that is no time in the format fails the rule in words that quote the
// text, cut when it is long, and the format, and say what the format reads;
// a part out of its range is named. A zone the text names by an abbreviation
// that time_zone does not have is refused rather than read as UTC.
func TestTextThatIsNoTimeFailsTheRule(t *testing.T) {
	long := strings.Repeat("2026-10-03 ", 20)
	for _, tc := range []struct {
		format, zone, text, want string
	}{
		{"2006-01-02", "", "03.10.2026", `value "03.10.2026" is not a time in time_format "2006-01-02"; write the layout as the text writes the reference time, Mon Jan 2 15:04:05 MST 2006`},
		{"2006-01-02", "", "2026-10-03 09:00", `value "2026-10-03 09:00" is not a time in time_format "2006-01-02"; write the layout as the text writes the reference time, Mon Jan 2 15:04:05 MST 2006`},
		{"2006-01-02", "", "never", `value "never" is not a time in time_format "2006-01-02"; write the layout`},
		{"2006-01-02", "", "1791018000", `value "1791018000" is not a time in time_format "2006-01-02"; write the layout`},
		{"2006-01-02", "", "2026-02-30", `value "2026-02-30" is not a time in time_format "2006-01-02": day out of range`},
		{"2006-01-02", "", "2026-13-01", `value "2026-13-01" is not a time in time_format "2006-01-02": month out of range`},
		{"2006-01-02 15:04", "", "2026-10-03 25:00", `value "2026-10-03 25:00" is not a time in time_format "2006-01-02 15:04": hour out of range`},
		{"2006-01-02", "", long, `value "2026-10-03 2026-10-03 2026-10-03 2026-10-03 2026-10-03 2026-10-0"... (219 bytes) is not a time in time_format "2006-01-02"; write the layout`},
		{"rfc3339", "", "2026-10-03 09:00:00", `value "2026-10-03 09:00:00" is not a time in time_format "rfc3339", which reads times such as 2006-01-02T15:04:05Z and 2006-01-02T15:04:05.999+02:00`},
		{"rfc3339", "", "2026-10-03T09:00:00", `value "2026-10-03T09:00:00" is not a time in time_format "rfc3339", which reads times such as`},
		{"rfc1123", "", "2026-10-03T09:00:00Z", `value "2026-10-03T09:00:00Z" is not a time in time_format "rfc1123", which reads times such as Mon, 02 Jan 2006 15:04:05 GMT and Mon, 02 Jan 2006 15:04:05 +0200`},
		{"rfc1123", "", "Sat, 3 Oct 2026 09:00:00 GMT", `value "Sat, 3 Oct 2026 09:00:00 GMT" is not a time in time_format "rfc1123", which reads times such as`},
		{"rfc1123", "", "Sat, 03 Oct 2026 09:00:00 EST", `value "Sat, 03 Oct 2026 09:00:00 EST" names the time zone EST, whose offset is not known: an abbreviation is read as one of time_zone's own, UTC or GMT; set time_zone to the zone the text is written in, such as Europe/Sofia`},
		{"rfc1123", "Europe/Sofia", "Sat, 03 Oct 2026 09:00:00 PDT", `value "Sat, 03 Oct 2026 09:00:00 PDT" names the time zone PDT, whose offset is not known`},
		{"2006-01-02 15:04 MST", "", "2026-10-03 09:00 EEST", `value "2026-10-03 09:00 EEST" names the time zone EEST, whose offset is not known`},
	} {
		rule := model.MetricRule{Name: "at", TimeFormat: tc.format, TimeZone: tc.zone}
		_, err := ruleTextValue(rule, tc.text)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%q as %q: got %v\nwant %s", tc.text, tc.format, err, tc.want)
		}
		if strings.Contains(fmt.Sprint(err), "parsing time") || strings.Contains(fmt.Sprint(err), "cannot parse") {
			t.Errorf("%q as %q: the error is in the time package's words: %v", tc.text, tc.format, err)
		}
	}
}

// A zone the text writes as GMT and hours says its offset: GMT+3 is three
// hours ahead of UTC and GMT-5 five behind, so 09:00 there is 06:00 and
// 14:00 UTC, whatever time_zone is and across midnight, with the fraction
// kept. The time package names the zone and leaves the time at UTC's, which
// read as it stood was hours off without a word. GMT alone, and with no
// hours, is UTC, and beside a numeric zone the numeric zone is the offset.
func TestGMTWithHoursIsReadByItsOffset(t *testing.T) {
	const layout = "2006-01-02 15:04:05 MST"
	for _, tc := range []struct {
		format, zone, text, want string
	}{
		{layout, "", "2026-10-03 09:00:00 GMT+3", "2026-10-03T06:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT-5", "2026-10-03T14:00:00Z"},
		{layout, "UTC", "2026-10-03 09:00:00 GMT+3", "2026-10-03T06:00:00Z"},
		{layout, "Europe/Sofia", "2026-10-03 09:00:00 GMT+3", "2026-10-03T06:00:00Z"},
		{layout, "Europe/Sofia", "2026-10-03 09:00:00 GMT-5", "2026-10-03T14:00:00Z"},
		{layout, "America/New_York", "2026-10-03 09:00:00 GMT+3", "2026-10-03T06:00:00Z"},
		{layout, "America/New_York", "2026-10-03 09:00:00 GMT-5", "2026-10-03T14:00:00Z"},
		// The zone three hours ahead that the IANA database names the other
		// way round, whose own abbreviation is +03.
		{layout, "Etc/GMT-3", "2026-10-03 09:00:00 GMT+3", "2026-10-03T06:00:00Z"},
		{layout, "Etc/GMT-3", "2026-10-03 09:00:00 GMT-5", "2026-10-03T14:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT+03", "2026-10-03T06:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT+1", "2026-10-03T08:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT-1", "2026-10-03T10:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT+12", "2026-10-02T21:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT-11", "2026-10-03T20:00:00Z"},
		{layout, "", "2026-10-03 01:30:00 GMT+3", "2026-10-02T22:30:00Z"},
		{layout, "Europe/Sofia", "2026-12-31 23:30:00 GMT-5", "2027-01-01T04:30:00Z"},
		{layout, "", "2026-10-03 09:00:00.25 GMT+3", "2026-10-03T06:00:00.25Z"},
		{"Mon, 02 Jan 2006 15:04:05 MST", "", "Sat, 03 Oct 2026 09:00:00 GMT-5", "2026-10-03T14:00:00Z"},

		{layout, "", "2026-10-03 09:00:00 GMT", "2026-10-03T09:00:00Z"},
		{layout, "Europe/Sofia", "2026-10-03 09:00:00 GMT", "2026-10-03T09:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 GMT+0", "2026-10-03T09:00:00Z"},
		{layout, "Europe/Sofia", "2026-10-03 09:00:00 GMT-0", "2026-10-03T09:00:00Z"},
		{layout, "", "2026-10-03 09:00:00 UTC", "2026-10-03T09:00:00Z"},
		{layout, "Europe/Sofia", "2026-10-03 09:00:00 UTC", "2026-10-03T09:00:00Z"},

		{"2006-01-02 15:04:05 -0700 MST", "", "2026-10-03 09:00:00 +0300 GMT+3", "2026-10-03T06:00:00Z"},
		{"2006-01-02 15:04:05 MST -0700", "Europe/Sofia", "2026-10-03 09:00:00 GMT+3 +0200", "2026-10-03T07:00:00Z"},
		{"2006-01-02 15:04:05 MST Z07:00", "", "2026-10-03 09:00:00 GMT-5 Z", "2026-10-03T09:00:00Z"},
	} {
		rule := model.MetricRule{Name: "at", TimeFormat: tc.format, TimeZone: tc.zone}
		want := unix(t, tc.want)
		if got, err := ruleTextValue(rule, tc.text); err != nil || got != want {
			t.Errorf("%q as %q in %q: got %v, %v; want %v", tc.text, tc.format, tc.zone, strconv.FormatFloat(got, 'f', -1, 64), err, strconv.FormatFloat(want, 'f', -1, 64))
		}
	}
	// GMT with what is no whole number of hours up to 23 is no time in the
	// layout, and an abbreviation that says no offset is still refused.
	for text, want := range map[string]string{
		"2026-10-03 09:00:00 GMT+24":   `is not a time in time_format`,
		"2026-10-03 09:00:00 GMT+3:30": `is not a time in time_format`,
		"2026-10-03 09:00:00 GMT3":     `is not a time in time_format`,
		"2026-10-03 09:00:00 UTC+3":    `is not a time in time_format`,
		"2026-10-03 09:00:00 +03":      `names the time zone +03, whose offset is not known`,
		"2026-10-03 09:00:00 EEST":     `names the time zone EEST, whose offset is not known`,
	} {
		if _, err := ruleTextValue(model.MetricRule{TimeFormat: layout}, text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error with %q", text, err, want)
		}
	}
	// The same through a rule: a regex captures the time with its zone.
	c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"}, Metrics: []model.MetricRule{
		{Name: "at", Type: model.GaugeMetricType, Expression: `at=(.+)`, TimeFormat: layout, TimeZone: "Europe/Sofia", ErrorMode: model.ErrorModeFail},
	}}
	set, err := runBody(t, c, "text/plain", "at=2026-10-03 09:00:00 GMT+3\nat=2026-10-03 09:00:00 GMT-5\n")
	if err != nil || len(set.Metrics) != 2 || set.Metrics[0].Value != unix(t, "2026-10-03T06:00:00Z") || set.Metrics[1].Value != unix(t, "2026-10-03T14:00:00Z") {
		t.Errorf("got %+v, %v; want 06:00 and 14:00 UTC", set, err)
	}
}

// formerRuleTime is ruleTime as it was before GMT with hours was read by its
// offset, for the differential test below.
func formerRuleTime(rule model.MetricRule, text string) (float64, error) {
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
	if location := t.Location(); location != zone && location != time.UTC {
		if name, offset := t.Zone(); offset == 0 && name != "" && !strings.HasPrefix(name, "GMT") && !hasNumericZone(layout) {
			return 0, fmt.Errorf("value %s names the time zone %s, whose offset is not known: an abbreviation is read as one of time_zone's own, UTC or GMT; set time_zone to the zone the text is written in, such as Europe/Sofia", model.QuoteValue(text), name)
		}
	}
	return scaled(rule, float64(t.Unix())+float64(t.Nanosecond())/1e9), nil
}

// Every text but one that names GMT with hours is read as it was: over
// formats, zones, dates, times of day and ways of writing the zone, the same
// seconds, bit for bit, and the same error. The texts that name GMT with
// hours other than none differ, by those hours, in a layout without a
// numeric zone, and are the same in one with it.
func TestOnlyGMTWithHoursIsReadDifferently(t *testing.T) {
	half := 0.5
	zones := []string{"", "UTC", "Europe/Sofia", "Europe/London", "America/New_York", "Asia/Kolkata", "Etc/GMT-3"}
	suffixes := []string{"", "Z", " Z", " UTC", " GMT", " GMT+0", " GMT+3", " GMT-5", " GMT+03", " GMT-11", " GMT+24", " EET", " EEST", " BST", " EST", " IST", " PDT", " +03", " -05", " +0300", " -0500", " +03:00", " +0300 GMT+3", " GMT-5 +0200", " +0300 EEST", " UT", " gmt+3"}
	formats := []string{
		"rfc3339", "rfc1123", "2006-01-02 15:04:05", "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST", "2006-01-02 15:04:05 MST -0700", "2006-01-02 15:04:05 -07", "2006-01-02 15:04:05 -07:00", "2006-01-02 15:04:05 UTC", "2006-01-02 15:04:05Z",
	}
	stamps := []string{"2026-10-03 09:00:00", "2026-01-15 23:30:00.5", "2026-07-04 00:10:00", "2026-10-25 03:30:00", "1969-12-31 23:59:59"}
	compared, differing := 0, 0
	for _, format := range formats {
		for _, zone := range zones {
			for _, stamp := range stamps {
				for _, suffix := range suffixes {
					text := stamp + suffix
					switch format {
					case "rfc3339":
						text = strings.Replace(stamp, " ", "T", 1) + strings.TrimSpace(suffix)
					case "rfc1123":
						at, err := time.Parse("2006-01-02 15:04:05", stamp)
						if err != nil {
							at, _ = time.Parse("2006-01-02 15:04:05.9", stamp)
						}
						text = at.Format("Mon, 02 Jan 2006 15:04:05") + suffix
					}
					for _, rule := range []model.MetricRule{{TimeFormat: format, TimeZone: zone}, {TimeFormat: format, TimeZone: zone, Scale: &half}} {
						got, gotErr := ruleTime(rule, text)
						want, wantErr := formerRuleTime(rule, text)
						compared++
						hours := 0.0
						if !hasNumericZone(format) && wantErr == nil {
							switch {
							case strings.HasSuffix(text, " GMT+3"), strings.HasSuffix(text, " GMT+03"):
								hours = 3
							case strings.HasSuffix(text, " GMT-5"):
								hours = -5
							case strings.HasSuffix(text, " GMT-11"):
								hours = -11
							}
						}
						if hours != 0 {
							differing++
							want = scaled(rule, want/scaled(rule, 1)-hours*3600)
						}
						if math.Float64bits(got) != math.Float64bits(want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
							t.Errorf("%q as %q in %q: got %v, %v; want %v, %v", text, format, zone, got, gotErr, want, wantErr)
						}
					}
				}
			}
		}
	}
	// 12 formats, 7 zones, 5 times, 27 zones as written, 2 rules. The texts
	// read differently are those of the one format that takes the name of a
	// zone and no numeric one, the layout with MST, with the four ways of
	// writing GMT with hours. (rfc1123 reads a text that ends in a digit as
	// one with a numeric zone, which GMT+3 is not.)
	if compared != 12*7*5*27*2 || differing != 7*5*4*2 {
		t.Errorf("compared %d texts, %d of them read differently; want %d and %d", compared, differing, 12*7*5*27*2, 7*5*4*2)
	}
}

// A layout's seconds written 05 read a text with a fraction of any length
// and one with none, after a dot or a comma, and so do seconds written with
// nines after them, 05.999. Zeros after the seconds, 05.000, stand for
// exactly that many digits: a text with fewer, more or none is no time in
// the layout. A minute takes no fraction.
func TestWhichFractionsOfASecondALayoutReads(t *testing.T) {
	for _, tc := range []struct {
		format, text, want string
	}{
		{"2006-01-02 15:04:05", "2026-10-03 09:00:00", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04:05", "2026-10-03 09:00:00.5", "2026-10-03T09:00:00.5Z"},
		{"2006-01-02 15:04:05", "2026-10-03 09:00:00.125", "2026-10-03T09:00:00.125Z"},
		{"2006-01-02 15:04:05", "2026-10-03 09:00:00,125", "2026-10-03T09:00:00.125Z"},
		{"2006-01-02 15:04:05", "2026-10-03 09:00:00.123456789", "2026-10-03T09:00:00.123456789Z"},
		{"2006-01-02T15:04:05Z07:00", "2026-10-03T09:00:00.5Z", "2026-10-03T09:00:00.5Z"},
		{"2006-01-02T15:04:05Z07:00", "2026-10-03T12:00:00.125+03:00", "2026-10-03T09:00:00.125Z"},

		{"2006-01-02 15:04:05.999", "2026-10-03 09:00:00", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04:05.999", "2026-10-03 09:00:00.5", "2026-10-03T09:00:00.5Z"},
		{"2006-01-02 15:04:05.999", "2026-10-03 09:00:00.125", "2026-10-03T09:00:00.125Z"},
		{"2006-01-02 15:04:05.999", "2026-10-03 09:00:00.123456", "2026-10-03T09:00:00.123456Z"},
		{"2006-01-02 15:04:05.999999999", "2026-10-03 09:00:00", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04:05.999999999", "2026-10-03 09:00:00.5", "2026-10-03T09:00:00.5Z"},
		{"2006-01-02 15:04:05.999999999", "2026-10-03 09:00:00.123456789", "2026-10-03T09:00:00.123456789Z"},
		{"2006-01-02T15:04:05.999999999Z07:00", "2026-10-03T09:00:00Z", "2026-10-03T09:00:00Z"},

		{"2006-01-02 15:04:05.000", "2026-10-03 09:00:00.125", "2026-10-03T09:00:00.125Z"},
		{"2006-01-02 15:04:05.000", "2026-10-03 09:00:00,125", "2026-10-03T09:00:00.125Z"},
		{"2006-01-02 15:04:05,000", "2026-10-03 09:00:00,125", "2026-10-03T09:00:00.125Z"},
		{"2006-01-02 15:04:05.000000", "2026-10-03 09:00:00.123456", "2026-10-03T09:00:00.123456Z"},
		{"2006-01-02 15:04:05.000", "2026-10-03 09:00:00", ""},
		{"2006-01-02 15:04:05.000", "2026-10-03 09:00:00.5", ""},
		{"2006-01-02 15:04:05.000", "2026-10-03 09:00:00.1234", ""},
		{"2006-01-02 15:04:05.000", "2026-10-03 09:00:00.123456", ""},
		{"2006-01-02 15:04:05.000000", "2026-10-03 09:00:00.125", ""},
		{"2006-01-02T15:04:05.000Z07:00", "2026-10-03T09:00:00Z", ""},

		{"2006-01-02 15:04", "2026-10-03 09:00.5", ""},
	} {
		rule := model.MetricRule{Name: "at", TimeFormat: tc.format}
		got, err := ruleTextValue(rule, tc.text)
		switch {
		case tc.want == "":
			if err == nil || !strings.HasPrefix(err.Error(), "value "+strconv.Quote(tc.text)+" is not a time in time_format "+strconv.Quote(tc.format)) {
				t.Errorf("%q as %q: got %v, %v; want it refused as no time in the format", tc.text, tc.format, got, err)
			}
		case err != nil || got != unix(t, tc.want):
			t.Errorf("%q as %q: got %v, %v; want %v", tc.text, tc.format, strconv.FormatFloat(got, 'f', -1, 64), err, tc.want)
		}
	}
}

// Text in a layout that is one of the reference time's elements is read as
// that element wherever it stands: the 4 of a Q4 is the minute, so the text
// has any minute there, and the quarter is not checked. A zone typed into a
// layout as the text has it — Z, UTC, GMT — is text to match and no zone, so
// the time is read in time_zone; written Z07:00 or MST it is read as the
// zone the text names.
func TestTextInALayoutThatIsAnElementIsReadAsOne(t *testing.T) {
	for _, tc := range []struct {
		format, zone, text, want string
	}{
		{"Q4 2006-01-02", "", "Q4 2026-10-03", "2026-10-03T00:04:00Z"},
		{"Q4 2006-01-02", "", "Q3 2026-10-03", "2026-10-03T00:03:00Z"},
		{"2006-01-02T15:04:05Z", "", "2026-10-03T09:00:00Z", "2026-10-03T09:00:00Z"},
		{"2006-01-02T15:04:05Z", "America/New_York", "2026-10-03T09:00:00Z", "2026-10-03T13:00:00Z"},
		{"2006-01-02 15:04:05 UTC", "Europe/Sofia", "2026-10-03 09:00:00 UTC", "2026-10-03T06:00:00Z"},
		{"2006-01-02 15:04:05 GMT", "Europe/Sofia", "2026-10-03 09:00:00 GMT", "2026-10-03T06:00:00Z"},
		{"2006-01-02T15:04:05Z07:00", "America/New_York", "2026-10-03T09:00:00Z", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04:05 MST", "Europe/Sofia", "2026-10-03 09:00:00 UTC", "2026-10-03T09:00:00Z"},
		{"2006-01-02 15:04:05 MST", "Europe/Sofia", "2026-10-03 09:00:00 GMT", "2026-10-03T09:00:00Z"},
	} {
		rule := model.MetricRule{Name: "at", Expression: ".at", TimeFormat: tc.format, TimeZone: tc.zone}
		if err := CheckMetricRule(&model.Collector{Name: "c", Transform: model.TransformConfig{Type: "jq"}}, &rule); err != nil {
			t.Errorf("%q: refused: %v", tc.format, err)
		}
		if got, err := ruleTextValue(rule, tc.text); err != nil || got != unix(t, tc.want) {
			t.Errorf("%q as %q in %q: got %v, %v; want %v", tc.text, tc.format, tc.zone, strconv.FormatFloat(got, 'f', -1, 64), err, tc.want)
		}
	}
}

// time_format reads text. What an expression gives that is no text — a
// number, which may well be Unix seconds already, a boolean, an object, an
// array — fails the rule, saying so and what the value is.
func TestTimeFormatReadsTextOnly(t *testing.T) {
	rule := model.MetricRule{Name: "at", TimeFormat: "rfc3339"}
	for value, shown := range map[any]string{
		1791018000.0: "1.791018e+09",
		42:           "42",
		true:         "true",
	} {
		_, err := ruleValue(rule, value)
		want := "value is " + shown + `, which time_format cannot read: it reads text, such as "2026-10-03T09:00:00Z"; select the text of the time, or leave time_format out for a number that is Unix seconds already`
		if err == nil || err.Error() != want {
			t.Errorf("%v: got %v\nwant %s", value, err, want)
		}
	}
	for shown, value := range map[string]any{
		"an object with 1 key": map[string]any{"at": "2026-10-03T09:00:00Z"},
		"an array of 2 items":  []any{"2026-10-03T09:00:00Z", "2026-10-04T09:00:00Z"},
		"null":                 nil,
	} {
		_, err := ruleValue(rule, value)
		if err == nil || !strings.HasPrefix(err.Error(), "value is "+shown+", which time_format cannot read: it reads text") {
			t.Errorf("%s: got %v", shown, err)
		}
	}
}

// What the configuration's load refuses of time_format and time_zone, each
// in the words that say what to write instead: a layout in another
// convention, one without a whole date, one that cannot read what it writes,
// one with a 12-hour clock's hour and no PM, one with blanks around it,
// time_zone alone or naming no zone, value_map beside time_format, and
// either transform that sets its values itself. The refusal of what may be
// no layout at all ends with what a layout is. What it takes loads.
func TestTimeFormatIsCheckedWhenTheConfigurationLoads(t *testing.T) {
	const where = `collector "c" metric "at" `
	// What a layout is, which ends every refusal of a layout that may well
	// be something else.
	const layoutIs = `a layout is the reference time, Mon Jan 2 15:04:05 MST 2006, written as the text writes its times, such as "2006-01-02 15:04:05" for text like 2026-10-03 09:00:00`
	noLayout := func(format string) string {
		return where + `time_format ` + strconv.Quote(format) + ` is neither the name rfc3339 or rfc1123 nor a layout: ` + layoutIs
	}
	noDate := func(format, missing string) string {
		return where + `time_format ` + strconv.Quote(format) + ` has no ` + missing + `: a time without a date has no Unix time, so a layout needs the year (2006), the month (01 or Jan) and the day (02); ` + layoutIs
	}
	unread := func(format, written string) string {
		return where + `time_format ` + strconv.Quote(format) + ` cannot read a time it writes, ` + strconv.Quote(written) + `, so keep its elements apart: ` + layoutIs
	}
	noAfternoon := func(format string) string {
		return where + `time_format ` + strconv.Quote(format) + ` writes the hour on a 12-hour clock (03 or 3) without PM, so it would read no afternoon: write the hour as 15 for a 24-hour clock, or add PM for a 12-hour one`
	}
	blanks := func(format string) string {
		return where + `time_format ` + strconv.Quote(format) + ` begins or ends with a blank, which the text never does: it is read without its surrounding blanks, so write the layout without them`
	}
	unknownZone := func(zone string) string {
		return where + `time_zone ` + strconv.Quote(zone) + ` is not a time zone the exporter knows; write an IANA name, such as Europe/Sofia or America/New_York, or UTC`
	}
	half := 0.5
	for _, tc := range []struct {
		transform string
		rule      model.MetricRule
		want      string
	}{
		{"jq", model.MetricRule{TimeFormat: "yyyy-mm-dd"}, noLayout("yyyy-mm-dd")},
		{"jq", model.MetricRule{TimeFormat: "%Y-%m-%d"}, noLayout("%Y-%m-%d")},
		{"jq", model.MetricRule{TimeFormat: "YYYY-MM-DD HH:mm:ss"}, noLayout("YYYY-MM-DD HH:mm:ss")},
		// A name that is none of the two is read as a layout, and its digits
		// as elements of the reference time.
		{"jq", model.MetricRule{TimeFormat: "iso8601"}, noDate("iso8601", "year or day")},
		{"jq", model.MetricRule{TimeFormat: "unix"}, noLayout("unix")},
		{"jq", model.MetricRule{TimeFormat: "unix_ms"}, noLayout("unix_ms")},
		{"regex", model.MetricRule{TimeFormat: "15:04"}, noDate("15:04", "year, month or day")},
		{"regex", model.MetricRule{TimeFormat: "15:04:05 MST"}, noDate("15:04:05 MST", "year, month or day")},
		{"regex", model.MetricRule{TimeFormat: "Jan 2 15:04:05"}, noDate("Jan 2 15:04:05", "year")},
		{"regex", model.MetricRule{TimeFormat: "2006-01"}, noDate("2006-01", "day")},
		{"regex", model.MetricRule{TimeFormat: "2006"}, noDate("2006", "month or day")},
		{"regex", model.MetricRule{TimeFormat: "02 15:04"}, noDate("02 15:04", "year or month")},
		{"regex", model.MetricRule{TimeFormat: "12006"}, unread("12006", "22001")},
		// A sample date and the name of a format the exporter does not have
		// are read as layouts, their digits as elements, and refused in
		// words that end with what a layout is.
		{"jq", model.MetricRule{TimeFormat: "2026-10-03"}, unread("2026-10-03", "3036-20-04")},
		{"jq", model.MetricRule{TimeFormat: "RFC3339Nano"}, unread("RFC3339Nano", "RFC4449Nano")},
		{"jq", model.MetricRule{TimeFormat: "rfc822"}, unread("rfc822", "rfc833")},
		// The hour of a 12-hour clock without PM would read the morning's
		// times and fail on every afternoon's.
		{"jq", model.MetricRule{TimeFormat: "2006-01-02 03:04:05"}, noAfternoon("2006-01-02 03:04:05")},
		{"jq", model.MetricRule{TimeFormat: "2006-01-02 3:04"}, noAfternoon("2006-01-02 3:04")},
		{"jq", model.MetricRule{TimeFormat: "Jan 2, 2006 3:04", TimeZone: "Europe/Sofia"}, noAfternoon("Jan 2, 2006 3:04")},
		// A 3 meant as itself is the hour too, beside a 24-hour one as well.
		{"jq", model.MetricRule{TimeFormat: "v3 2006-01-02"}, noAfternoon("v3 2006-01-02")},
		{"jq", model.MetricRule{TimeFormat: "2006-01-02 15:04:05 UTC+3"}, noAfternoon("2006-01-02 15:04:05 UTC+3")},
		// The text is read without its surrounding blanks, so a layout with
		// its own would read none.
		{"jq", model.MetricRule{TimeFormat: " 2006-01-02 "}, blanks(" 2006-01-02 ")},
		{"jq", model.MetricRule{TimeFormat: "2006-01-02 15:04\n"}, blanks("2006-01-02 15:04\n")},
		{"jq", model.MetricRule{TimeFormat: "\t2006-01-02"}, blanks("\t2006-01-02")},
		{"jq", model.MetricRule{TimeFormat: " "}, noLayout(" ")},
		{"csv", model.MetricRule{TimeZone: "Europe/Sofia"}, where + `sets time_zone without time_format; time_zone is the zone a time_format reads text in that names no zone of its own`},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "Europe/Sofija"}, unknownZone("Europe/Sofija")},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "EEST"}, unknownZone("EEST")},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "PST"}, unknownZone("PST")},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "+03:00"}, unknownZone("+03:00")},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "Local"}, unknownZone("Local")},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "utc"}, unknownZone("utc")},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "../etc/passwd"}, unknownZone("../etc/passwd")},
		{"xpath", model.MetricRule{TimeFormat: "2006-01-02", ValueMap: map[string]float64{"never": 0}}, where + `sets both time_format and value_map; each turns the text into the value, so a rule takes one of them`},
		{"css", model.MetricRule{TimeFormat: "rfc3339", ValueMap: map[string]float64{}}, where + `sets both time_format and value_map; each turns the text into the value, so a rule takes one of them`},
		{"python", model.MetricRule{TimeFormat: "2006-01-02"}, where + `sets time_format, which the python transform does not use: its script sets each value with metric(...)`},
		{"python", model.MetricRule{TimeFormat: "yyyy", TimeZone: "Nowhere"}, where + `sets time_format, which the python transform does not use: its script sets each value with metric(...)`},
		{"python", model.MetricRule{TimeZone: "UTC"}, where + `sets time_zone without time_format`},
		{"prometheus", model.MetricRule{TimeFormat: "rfc3339"}, where + `sets time_format, which the prometheus transform does not use: its values are numbers already; scale applies`},

		{"jq", model.MetricRule{TimeFormat: "rfc3339"}, ""},
		{"jq", model.MetricRule{TimeFormat: "RFC1123", TimeZone: "UTC"}, ""},
		{"yq", model.MetricRule{TimeFormat: "2006-01-02"}, ""},
		{"regex", model.MetricRule{TimeFormat: "2006/01/02 15:04", TimeZone: "Europe/Sofia"}, ""},
		{"regex", model.MetricRule{TimeFormat: "02.01.2006 15:04:05", Scale: &half}, ""},
		{"css", model.MetricRule{TimeFormat: "Mon Jan 2 15:04:05 MST 2006"}, ""},
		{"css", model.MetricRule{TimeFormat: "Monday, 02-Jan-06 15:04:05 MST"}, ""},
		{"xpath", model.MetricRule{TimeFormat: "January 2, 2006 3:04 PM"}, ""},
		{"xpath", model.MetricRule{TimeFormat: "2006-002"}, ""},
		{"csv", model.MetricRule{TimeFormat: "20060102150405", TimeZone: "America/New_York"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02T15:04:05.000Z07:00"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2 Jan 06"}, ""},
		// Either way of writing a 12-hour clock's half of the day, and a
		// 24-hour clock with or without it.
		{"csv", model.MetricRule{TimeFormat: "2006-01-02 03:04:05 PM"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02 3:04pm"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02 15:04 PM"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02 15:04:05.999999999"}, ""},
		// A zone is an IANA name, and a few abbreviations are such names.
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "EET"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "CET"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "EST"}, ""},
		{"csv", model.MetricRule{TimeFormat: "2006-01-02", TimeZone: "GMT"}, ""},
	} {
		c := &model.Collector{Name: "c", Transform: model.TransformConfig{Type: tc.transform}}
		rule := tc.rule
		rule.Name = "at"
		switch tc.transform {
		case "regex":
			rule.Expression = `at: (.+)`
		case "jq", "yq":
			rule.Expression = ".at"
		case "xpath":
			rule.Expression = "//at"
		default:
			rule.Expression = "at"
		}
		err := CheckMetricRule(c, &rule)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s %+v: refused: %v", tc.transform, tc.rule, err)
		case tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)):
			t.Errorf("%s %+v:\ngot  %v\nwant %s", tc.transform, tc.rule, err, tc.want)
		}
	}
	// A rule's other mistakes are still reported beside it.
	c := &model.Collector{Name: "c", Transform: model.TransformConfig{Type: "jq"}}
	err := CheckMetricRule(c, &model.MetricRule{Name: "at", Expression: ".at[", TimeFormat: "hh:mm", Scale: new(float64)})
	for _, want := range []string{`expression ".at["`, `time_format "hh:mm" is neither`, `scale must be a finite number other than 0`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q among the mistakes", err, want)
		}
	}
}

// formerLayoutVerdict is what checkTimeLayout decided before it refused a
// 12-hour clock without PM and blanks around a layout: whether it took the
// layout, as the checks it had then decide it.
func formerLayoutVerdict(format string) bool {
	if _, named := namedTimeLayout(format, ""); named {
		return true
	}
	written := timeProbe.Format(format)
	if written == format {
		return false
	}
	read, err := time.Parse(format, written)
	return err == nil && read.Year() == timeProbe.Year() && read.Month() == timeProbe.Month() && read.Day() == timeProbe.Day()
}

// The load check takes no layout it refused before, and refuses of those it
// took only the ones with blanks around them or with a 12-hour clock's hour
// and no PM: over names, layouts of every kind, layouts of other conventions
// and text that is no layout at all.
func TestTheLayoutCheckRefusesOnlyBlanksAndAnHourWithoutPMMore(t *testing.T) {
	refusedNow := map[string]string{
		"2006-01-02 03:04:05":       "12-hour clock",
		"2006-01-02 3":              "12-hour clock",
		"2006-01-02 3:04":           "12-hour clock",
		"01/02 03:04:05 2006":       "12-hour clock",
		"v3 2006-01-02":             "12-hour clock",
		"2006-01-02 15:04:05 UTC+3": "12-hour clock",
		" 2006-01-02 ":              "begins or ends with a blank",
		"2006-01-02\n":              "begins or ends with a blank",
		"\t2006-01-02 15:04":        "begins or ends with a blank",
	}
	formats := []string{
		"rfc3339", "RFC3339", "rfc1123", "RFC1123Z", "rfc822", "rfc3339nano", "unix", "unix_ms", "iso8601", "ISO8601", "RFC3339Nano",
		"2006-01-02", "2006-01-02 15:04:05", "2006/01/02 15:04", "02.01.2006 15:04:05", "Jan 2, 2006 3:04 PM", "20060102T150405Z07:00",
		"15:04", "15:04:05", "Jan 2", "2006-01", "2006", "01-02", "Jan 2 15:04:05", "Mon Jan 2 15:04:05 MST 2006", "Mon Jan _2 15:04:05 2006",
		"yyyy-mm-dd", "%Y-%m-%d", "YYYY-MM-DD HH:mm:ss", "2026-10-03", "2006-10-03", "2006-01-03", "2006-002", "002", "2006-002 15:04",
		"06-01-02", "06-1-2", "060102", "20060102", "20060102150405", "Monday, 02-Jan-06 15:04:05 MST", "Mon, 02 Jan 2006 15:04:05 -0700",
		"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05.999999999Z07:00", "3:04PM", "Jan _2 15:04:05", "01/02 03:04:05PM '06 -0700",
		"2006-01-02 Mon", "Monday 2006", "Mon 2006-01", "2006 January", "2006 Jan 02", "January 2, 2006", "2. January 2006", "2 Jan 06",
		"02-Jan-2006", "2006-01-02 15:04:05.000", "2006-01-02 15:04:05,000", "2006-01-02T15", "2006-01-0215", "200601021504", "2006012",
		"2006-1-2 15:4:5", "1/2/2006", "1/2/06", "2/1/2006", "__2 2006", "2006-__2", "2006 Jan __2", "Local", "UTC", "true", "1", "2", "0",
		"-", "", " ", "2006-01-02 MST", "2006-01-02 -0700", "2006-01-02 Z07:00", "2006-01-02 -07", "MST 2006-01-02", "Monitor 2006-01-02",
		"Q4 2006-01-02", "date 2006-01-02", "2006-01-02 03:04:05 PM", "2006-01-02 3:04pm", "2006-01-02 03:04 pm", "2006-01-02 15:04 PM",
		"2006-01-02T15:04:05Z", "2006-01-02 15:04:05 UTC", "12006", "2006-01-02 15h04", "2006年01月02日", "02/01/2006", "01/02/2006",
	}
	for format := range refusedNow {
		formats = append(formats, format)
	}
	for _, format := range formats {
		err := checkTimeLayout(format)
		before := formerLayoutVerdict(format)
		why, newly := refusedNow[format]
		switch {
		case newly && (!before || err == nil || !strings.Contains(err.Error(), why)):
			t.Errorf("%q: taken before: %v, now: %v; want it taken before and refused now for its %s", format, before, err, why)
		case !newly && before != (err == nil):
			t.Errorf("%q: taken before: %v, now: %v; want the same verdict", format, before, err)
		}
	}
}

// time_format works in every transform with rules, wherever the rule's text
// comes from: a regex capture, a CSS element with and without items, an
// XPath node, attribute and computed string, a CSV cell, and a jq or yq
// string, a YAML date written without quotes included. Each rule's series
// carry its labels as any rule's do.
func TestTimeFormatInEveryTransform(t *testing.T) {
	const day, minute = "2006-01-02", "2006-01-02 15:04"
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body string
		rules                                 []model.MetricRule
		want                                  []string
	}{
		"regex": {"text", "regex", "text/plain", "2026/10/03 09:00\nLBSF 030900Z 14/08 Q1021\nbuilt: 2026-10-02 by ci\n", []model.MetricRule{
			{Name: "observed", Expression: `\A(\d{4}/\d{2}/\d{2} \d{2}:\d{2})`, TimeFormat: "2006/01/02 15:04"},
			{Name: "built", Expression: `built: (?P<value>\S+) by (\w+)`, TimeFormat: day, Labels: []model.LabelRule{{Name: "by", Expression: "2"}}},
		}, []string{`observed 1791018000`, `built{by="ci"} 1790899200`}},
		"css": {"html", "css", "text/html", `<p id="built"> 2026-10-02 </p><table><tr><td class="job">backup</td><td class="at">2026-10-03 09:00</td></tr><tr><td class="job">sync</td><td class="at">2026-10-03 12:00</td></tr></table>`, []model.MetricRule{
			{Name: "built", Expression: "#built", TimeFormat: day},
			{Name: "last_run", Items: "tr", Expression: "td.at", TimeFormat: minute, TimeZone: "Europe/Sofia", Labels: []model.LabelRule{{Name: "job", Expression: "td.job"}}},
		}, []string{`built 1790899200`, `last_run{job="backup"} 1791007200`, `last_run{job="sync"} 1791018000`}},
		"xpath": {"xml", "xpath", "application/xml", `<r><Cube time="2026-10-02"/><job name="backup"><at>2026-10-03T09:00:00Z</at></job><job name="sync"><at>2026-10-03T12:00:00+03:00</at></job></r>`, []model.MetricRule{
			{Name: "rates_day", Expression: "//Cube/@time", TimeFormat: day},
			{Name: "last_run", Expression: "//job/at", TimeFormat: "rfc3339", Labels: []model.LabelRule{{Name: "job", Expression: "../@name"}}},
			{Name: "computed", Expression: "string(//Cube/@time)", TimeFormat: day},
		}, []string{`rates_day 1790899200`, `last_run{job="backup"} 1791018000`, `last_run{job="sync"} 1791018000`, `computed 1790899200`}},
		"csv": {"csv", "csv", "text/csv", "job,at,day\nbackup,03.10.2026 09:00:00,2026-10-02\nsync,03.10.2026 09:00:30,2026-10-03\n", []model.MetricRule{
			{Name: "last_run", Expression: "at", TimeFormat: "02.01.2006 15:04:05", Labels: []model.LabelRule{{Name: "job", Expression: "job"}}},
			{Name: "day", Expression: "day", TimeFormat: day, TimeZone: "UTC", Labels: []model.LabelRule{{Name: "job", Expression: "job"}}},
		}, []string{`last_run{job="backup"} 1791018000`, `last_run{job="sync"} 1791018030`, `day{job="backup"} 1790899200`, `day{job="sync"} 1790985600`}},
		"jq": {"json", "jq", "application/json", `{"date": "2026-10-02", "jobs": [{"name": "backup", "at": "Sat, 03 Oct 2026 09:00:00 GMT"}, {"name": "sync", "at": "Sat, 03 Oct 2026 12:00:00 +0300"}]}`, []model.MetricRule{
			{Name: "day", Expression: ".date", TimeFormat: day},
			{Name: "last_run", Items: ".jobs[]", Expression: ".at", TimeFormat: "rfc1123", Labels: []model.LabelRule{{Name: "job", Expression: ".name"}}},
			{Name: "each", Expression: ".jobs[].at", TimeFormat: "rfc1123", Labels: []model.LabelRule{{Name: "job", Expression: ".jobs[].name"}}},
		}, []string{`day 1790899200`, `last_run{job="backup"} 1791018000`, `last_run{job="sync"} 1791018000`, `each{job="backup"} 1791018000`, `each{job="sync"} 1791018000`}},
		"yq": {"yaml", "yq", "application/yaml", "updated: 2026-10-02\nstarted: \"2026-10-03 09:00\"\nstamp: 2026-10-03T09:00:00.5Z\n", []model.MetricRule{
			{Name: "updated", Expression: ".updated", TimeFormat: day},
			{Name: "started", Expression: ".started", TimeFormat: minute},
			{Name: "stamp", Expression: ".stamp", TimeFormat: "rfc3339"},
		}, []string{`updated 1790899200`, `started 1791018000`, `stamp 1791018000.5`}},
	} {
		t.Run(name, func(t *testing.T) {
			for i := range tc.rules {
				tc.rules[i].Type = model.GaugeMetricType
				tc.rules[i].ErrorMode = model.ErrorModeFail
			}
			c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: tc.rules}
			for i := range c.Metrics {
				if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
					t.Fatal(err)
				}
			}
			set, err := runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range set.Metrics {
				// No rule here gives a series more than one label.
				labels := ""
				for label, value := range m.Labels {
					labels = fmt.Sprintf("{%s=%q}", label, value)
				}
				got = append(got, m.Name+labels+" "+strconv.FormatFloat(m.Value, 'f', -1, 64))
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// In each transform a text that is no time is the rule's failure, handled by
// its error_mode: under fail the scrape fails naming the rule, the text and
// the format; under log the series is left out and the others are kept. A
// jq number under time_format fails the same way, saying time_format reads
// text. An empty or absent text stays a missing value, which an optional
// rule leaves out without failing.
func TestAnUnreadableTimeIsHandledByTheErrorMode(t *testing.T) {
	testutil.CaptureLogs(t)
	optional := false
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body, expression, items string
		want                                                     string
		kept                                                     int
	}{
		"regex":     {"text", "regex", "text/plain", "at: 2026-10-03\nat: soon\nat: 2026-10-05\n", `(?m)^at: (.+)$`, "", `metric "at": value "soon" is not a time in time_format "2006-01-02"`, 2},
		"css":       {"html", "css", "text/html", `<ul><li><b>2026-10-03</b></li><li><b>soon</b></li><li><b></b></li></ul>`, "b", "li", `metric "at" item 1: value "soon" is not a time in time_format "2006-01-02"`, 1},
		"xpath":     {"xml", "xpath", "application/xml", `<r><at>2026-10-03</at><at>soon</at><at/></r>`, "//at", "", `metric "at" node 1: value "soon" is not a time in time_format "2006-01-02"`, 1},
		"csv":       {"csv", "csv", "text/csv", "at\n2026-10-03\nsoon\n\"\"\n", "at", "", `metric "at": value "soon" is not a time in time_format "2006-01-02"`, 1},
		"jq":        {"json", "jq", "application/json", `{"at": ["2026-10-03", "soon", "", null]}`, ".at[]", "", `metric "at": value "soon" is not a time in time_format "2006-01-02"`, 1},
		"jq number": {"json", "jq", "application/json", `{"at": ["2026-10-03", 1791018000]}`, ".at[]", "", `metric "at": value is 1791018000, which time_format cannot read: it reads text`, 1},
		"yq":        {"yaml", "yq", "application/yaml", "at: [2026-10-03, soon, '']\n", ".at[]", "", `metric "at": value "soon" is not a time in time_format "2006-01-02"`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			rule := model.MetricRule{Name: "at", Type: model.GaugeMetricType, Expression: tc.expression, Items: tc.items, TimeFormat: "2006-01-02", Required: &optional}
			c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: []model.MetricRule{rule}}
			c.Metrics[0].ErrorMode = model.ErrorModeFail
			_, err := runBody(t, c, tc.contentType, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("under fail: got %v, want %s", err, tc.want)
			}
			c.Metrics[0].ErrorMode = model.ErrorModeLog
			set, err := runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatalf("under log: %v", err)
			}
			if len(set.Metrics) != tc.kept || set.Metrics[0].Value != 1790985600 {
				t.Fatalf("under log: got %#v, want %d series, the first of 2026-10-03", set.Metrics, tc.kept)
			}
		})
	}
}

// formerRuleValue and formerRuleTextValue are the two as they were before
// time_format, for the differential test below. Both read a number as
// model.Number does, which has since stopped reading Go's own forms of one,
// 1_000 and 0x1p-2 (TestTextIsReadAsANumberAsBeforeButForGoSyntax in
// internal/model).
func formerRuleValue(rule model.MetricRule, raw any) (float64, error) {
	if len(rule.ValueMap) > 0 {
		key, err := labelText(raw)
		if err == nil {
			if mapped, ok := rule.ValueMap[strings.TrimSpace(key)]; ok {
				return scaled(rule, mapped), nil
			}
		}
		if mapped, ok := rule.ValueMap[valueMapDefault]; ok {
			return scaled(rule, mapped), nil
		}
		n, numberErr := model.Number(raw)
		if numberErr != nil {
			if err != nil {
				return 0, fmt.Errorf("value is %s, which is neither text value_map can look up nor a number; select one value inside it", model.ShowValue(raw))
			}
			return 0, fmt.Errorf("value %s is neither in value_map nor a number; add it to value_map, or map \"*\" for any other value", model.QuoteValue(key))
		}
		return scaled(rule, n), nil
	}
	n, err := model.Number(raw)
	if err != nil {
		return 0, err
	}
	return scaled(rule, n), nil
}

func formerRuleTextValue(rule model.MetricRule, text string) (float64, error) {
	trimmed := strings.TrimSpace(text)
	if len(rule.ValueMap) > 0 {
		if mapped, ok := rule.ValueMap[trimmed]; ok {
			return scaled(rule, mapped), nil
		}
		if mapped, ok := rule.ValueMap[valueMapDefault]; ok {
			return scaled(rule, mapped), nil
		}
	}
	if n, err := model.ParseFloat(trimmed); err == nil {
		return scaled(rule, n), nil
	}
	return formerRuleValue(rule, text)
}

// A rule without time_format reads its value as it did before there was
// one: the same number, bit for bit, and the same error, for text and for
// values of every type, mapped, scaled and plain, times written as text
// among them. Reading a number still allocates nothing, and reading a time
// allocates nothing either, in UTC and in a named zone.
func TestARuleWithoutTimeFormatReadsItsValueAsBefore(t *testing.T) {
	milli := 0.001
	rules := []model.MetricRule{
		{},
		{Scale: &milli},
		{ValueMap: map[string]float64{"up": 1, "true": 2, "7": 70, "2026-10-03": 3}},
		{ValueMap: map[string]float64{"down": 0, "*": -1}, Scale: &milli},
		{TimeZone: "Europe/Sofia"},
	}
	values := []any{
		"7", " 7 ", "0.5", "1e3", "-0", "NaN", "+Inf", "up", "down", "", "  ", "n/a", "2026-10-03", "2026-10-03T09:00:00Z", "0x10", "1_000",
		strings.Repeat("9", 400), strings.Repeat("x", 200),
		7.0, 0.1, math.Inf(1), 7, int64(-3), uint64(9), float32(0.5), true, false, nil,
		map[string]any{"a": 1}, []any{1, 2}, []any{},
	}
	same := func(a, b float64) bool { return math.Float64bits(a) == math.Float64bits(b) }
	for _, rule := range rules {
		for _, value := range values {
			got, gotErr := ruleValue(rule, value)
			want, wantErr := formerRuleValue(rule, value)
			if !same(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("ruleValue(%+v, %#v): got %v, %v; want %v, %v", rule, value, got, gotErr, want, wantErr)
			}
			text, isText := value.(string)
			if !isText {
				continue
			}
			got, gotErr = ruleTextValue(rule, text)
			want, wantErr = formerRuleTextValue(rule, text)
			if !same(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("ruleTextValue(%+v, %q): got %v, %v; want %v, %v", rule, text, got, gotErr, want, wantErr)
			}
		}
	}
	for name, read := range map[string]func(){
		"a number":           func() { _, _ = ruleTextValue(rules[1], " 412 ") },
		"a mapped text":      func() { _, _ = ruleTextValue(rules[3], "gone") },
		"a time in UTC":      func() { _, _ = ruleTextValue(model.MetricRule{TimeFormat: "2006-01-02 15:04"}, "2026-10-03 09:00") },
		"a time by its name": func() { _, _ = ruleTextValue(model.MetricRule{TimeFormat: "rfc3339"}, "2026-10-03T09:00:00.5Z") },
		"a time in a named zone": func() {
			_, _ = ruleTextValue(model.MetricRule{TimeFormat: "2006-01-02 15:04", TimeZone: "Europe/Sofia"}, "2026-10-03 09:00")
		},
		"a time as an HTTP date": func() {
			_, _ = ruleTextValue(model.MetricRule{TimeFormat: "rfc1123", TimeZone: "Europe/Sofia"}, "Sat, 03 Oct 2026 12:00:00 EEST")
		},
	} {
		// Under the race detector nothing can be said of allocations
		// (race_test.go).
		if raceDetector {
			break
		}
		read()
		if allocs := alloctest.AllocsAtMost(100, 0, read); allocs != 0 {
			t.Errorf("reading %s allocates %v times, want none", name, allocs)
		}
	}
}

// A zone is read from its data once, and every rule that names it is handed
// the one location, from however many scrapes at once.
func TestATimeZoneIsLoadedOnce(t *testing.T) {
	first, err := timeLocation("Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *time.Location)
	for range 8 {
		go func() {
			zone, _ := timeLocation("Australia/Lord_Howe")
			other, _ := timeLocation("Pacific/Chatham")
			if other == nil || other.String() != "Pacific/Chatham" {
				zone = nil
			}
			done <- zone
		}()
	}
	for range 8 {
		if zone := <-done; zone != first {
			t.Errorf("got the location %p, want the first one, %p", zone, first)
		}
	}
	for name, want := range map[string]*time.Location{"": time.UTC, "UTC": time.UTC} {
		if zone, err := timeLocation(name); err != nil || zone != want {
			t.Errorf("%q: %v, %v", name, zone, err)
		}
	}
}
