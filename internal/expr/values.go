package expr

import (
	"math/big"
	"regexp"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/itchyny/gojq"
)

// The value-parsing helpers read the human forms of sizes, durations, times
// and percentages that targets write in place of a plain number — Redis's
// used_memory_human "1.00M", an API's "1h2m" or "55.12%", a page's "Sat, 03
// Oct 2026 09:00:00 GMT" — so a rule does not take them apart with a regular
// expression of its own. They are jq functions here and plain functions of
// the Python transform (pythonworker.go), and the two must give the same
// number, and the same error, for every input: both are tested against the
// vectors of testdata/values/helpers.json.
//
// Each reads its number exactly, as a fraction of whole numbers, and rounds
// it to a float once, at the end: 1.1GB is 1100000000 and 0.1s0.2s is 0.3,
// where multiplying and adding floats on the way gives 1100000000.0000002
// and 0.30000000000000004. Python's division of two integers rounds the same
// way big.Rat does, to the nearest float, so the two cannot differ by the
// order of their operations.

// ValueHelpers are the names of the helpers, each a jq function of no
// arguments that reads its input.
var ValueHelpers = []string{"parse_bytes", "parse_duration", "parse_time", "parse_percent"}

// valueFunctions registers the helpers with a jq program. They are custom
// functions of the compiler, not jq definitions, so they cost nothing to an
// expression that does not call them; none is the name of a gojq builtin,
// which TestValueHelpersShadowNoBuiltin keeps so.
var valueFunctions = []gojq.CompilerOption{
	gojq.WithFunction("parse_bytes", 0, 0, func(v any, _ []any) any { return parseValue("parse_bytes", v, true, ParseBytes) }),
	gojq.WithFunction("parse_duration", 0, 0, func(v any, _ []any) any { return parseValue("parse_duration", v, true, ParseDuration) }),
	gojq.WithFunction("parse_time", 0, 0, func(v any, _ []any) any { return parseValue("parse_time", v, false, ParseTime) }),
	gojq.WithFunction("parse_percent", 0, 0, func(v any, _ []any) any { return parseValue("parse_percent", v, false, ParsePercent) }),
}

// parseValue runs a helper on a jq value: text is read by parse, and a
// number, where numbers is set, is taken as already being bytes or seconds,
// as an API that writes "1.5K" for one value may write 1536 for another.
// Anything else is an error saying what the helper reads. The error is the
// value of the jq program, which fails the rule as any jq error does.
func parseValue(helper string, v any, numbers bool, parse func(string) (float64, error)) any {
	switch x := v.(type) {
	case string:
		f, err := parse(x)
		if err != nil {
			return err
		}
		return f
	case int, float64, *big.Int:
		if numbers {
			return v
		}
	}
	reads := "text"
	if numbers {
		reads = "text or a number"
	}
	return model.Errorf("%s reads %s, not %s", helper, reads, valueKind(v))
}

// valueKind names what a jq value is, for the error of a helper that cannot
// read it.
func valueKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case int, float64, *big.Int:
		return "a number"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	}
	return "a value of another kind"
}

// maxValueDigits is how many digits a number in a helper's text may have.
// More than enough for any value a target writes, and what keeps reading one
// cheap and the same in Python, whose int() refuses a string of more than
// 4300 digits.
const maxValueDigits = 100

// valueError is the error of text a helper cannot read: the helper, the text
// as it came, quoted and cut to its first 64 bytes (model.Quoted), what the
// helper reads and why it could not.
func valueError(helper, text, what, why string) error {
	return model.Errorf("%s cannot read %s as %s: %s", helper, model.Quoted(text), what, why)
}

const (
	bytesHint    = "write a number, then optionally blanks and a unit: B, KiB to EiB, kB to EB, or K to E"
	durationHint = "write it as Go does (1h2m3.5s, 2d4h), as a clock (01:02:03, 1-02:03:04) or in ISO 8601 (PT1H2M3S)"
	timeHint     = "write it in RFC 3339 (2026-10-03T09:00:00Z) or as an HTTP date (Sat, 03 Oct 2026 09:00:00 GMT)"
	percentHint  = "write a number followed by %, such as 55.12%"
	digitsHint   = "a number in it has more than 100 digits"
)

// trimValue drops the blanks around a helper's text — spaces, tabs and line
// ends, as the text of an HTML cell or a line of a report has — and no other
// space, so Python, whose strip() drops more, reads the same text.
func trimValue(text string) string { return strings.Trim(text, " \t\r\n") }

// decimalPattern is a number as the helpers read it: digits with or without
// a fraction, at least one digit, no sign or exponent.
const decimalPattern = `([0-9]+(?:\.[0-9]*)?|\.[0-9]+)`

// decimal reads a number decimalPattern matched exactly. ok is false when it
// has more than maxValueDigits digits.
func decimal(text string) (*big.Rat, bool) {
	whole, fraction, _ := strings.Cut(text, ".")
	if len(whole)+len(fraction) > maxValueDigits {
		return nil, false
	}
	n, _ := new(big.Int).SetString("0"+whole+fraction, 10)
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fraction))), nil)
	return new(big.Rat).SetFrac(n, d), true
}

// float rounds an exact value to the nearest float. No value the helpers
// read is beyond a float's range: its numbers have 100 digits at most.
func float(r *big.Rat) float64 {
	f, _ := r.Float64()
	return f
}

var bytesSyntax = regexp.MustCompile(`^` + decimalPattern + `[ \t]*([A-Za-z]*)$`)

// byteUnits are the units of a size by their name in lower case: B and
// none, IEC binary units, SI decimal units, and the bare letters Redis, ls
// -h, df -h and Java's -Xmx write, which are binary.
var byteUnits = map[string]*big.Rat{"": rat(1, 0), "b": rat(1, 0)}

// rat is base to the power of n, or 1 for n 0.
func rat(base, n int64) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(base), big.NewInt(n), nil))
}

func init() {
	for i, letter := range []string{"k", "m", "g", "t", "p", "e"} {
		n := int64(i + 1)
		byteUnits[letter+"ib"] = rat(1024, n)
		byteUnits[letter+"b"] = rat(1000, n)
		byteUnits[letter] = rat(1024, n)
	}
}

// ParseBytes reads a size in bytes: a number, then blanks or none, then a
// unit in any case or none. B and none are bytes; KiB, MiB, GiB, TiB, PiB and
// EiB are powers of 1024; kB (or KB), MB, GB, TB, PB and EB powers of 1000;
// and the bare letters K, M, G, T, P and E powers of 1024.
func ParseBytes(text string) (float64, error) {
	m := bytesSyntax.FindStringSubmatch(trimValue(text))
	if m == nil {
		return 0, valueError("parse_bytes", text, "a size", bytesHint)
	}
	// The unit is ASCII letters, which the pattern says, so lower-casing
	// it cannot fold another letter, such as the Kelvin sign, into one.
	unit, ok := byteUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, valueError("parse_bytes", text, "a size", bytesHint)
	}
	n, ok := decimal(m[1])
	if !ok {
		return 0, valueError("parse_bytes", text, "a size", digitsHint)
	}
	return float(n.Mul(n, unit)), nil
}

var (
	goDurationSyntax  = regexp.MustCompile(`^[+-]?(?:` + decimalPattern + `(ns|us|µs|μs|ms|s|m|h|d))+$`)
	goDurationPart    = regexp.MustCompile(decimalPattern + `(ns|us|µs|μs|ms|s|m|h|d)`)
	clockSyntax       = regexp.MustCompile(`^((?:[0-9]+[-.])?[0-9]+(?::[0-9]+){1,2})(\.[0-9]+)?$`)
	isoDurationSyntax = regexp.MustCompile(`^P(?:` + decimalPattern + `W)?(?:` + decimalPattern + `D)?(?:T(?:` + decimalPattern + `H)?(?:` + decimalPattern + `M)?(?:` + decimalPattern + `S)?)?$`)
	isoYearsMonths    = regexp.MustCompile(`^P[^T]*[YM]`)
)

// goDurationUnits are the seconds in each unit of Go's durations, with d
// for a day of 24 hours.
var goDurationUnits = map[string]*big.Rat{
	"ns": big.NewRat(1, 1e9), "us": big.NewRat(1, 1e6), "µs": big.NewRat(1, 1e6), "μs": big.NewRat(1, 1e6),
	"ms": big.NewRat(1, 1e3), "s": big.NewRat(1, 1), "m": big.NewRat(60, 1), "h": big.NewRat(3600, 1), "d": big.NewRat(86400, 1),
}

// ParseDuration reads a duration in seconds, written as Go writes one, with
// d for 24 hours and a sign; as a clock, MM:SS, HH:MM:SS, or D-HH:MM:SS and
// D.HH:MM:SS with days, as ps and Slurm write elapsed time; or in ISO 8601,
// PnWnDTnHnMnS, without years and months, which have no fixed length.
func ParseDuration(text string) (float64, error) {
	s := trimValue(text)
	total := new(big.Rat)
	add := func(number string, unit *big.Rat) bool {
		n, ok := decimal(number)
		if ok {
			total.Add(total, n.Mul(n, unit))
		}
		return ok
	}
	switch {
	case s == "0" || s == "+0" || s == "-0":
		return 0, nil
	case goDurationSyntax.MatchString(s):
		for _, part := range goDurationPart.FindAllStringSubmatch(s, -1) {
			if !add(part[1], goDurationUnits[part[2]]) {
				return 0, valueError("parse_duration", text, "a duration", digitsHint)
			}
		}
		if s[0] == '-' {
			total.Neg(total)
		}
		return float(total), nil
	case clockSyntax.MatchString(s):
		// The fields after the first are two digits each, under 24 for the
		// hours after days and under 60 for minutes and seconds; the first,
		// days, hours or minutes, is as long as it needs to be.
		m := clockSyntax.FindStringSubmatch(s)
		fields, fraction := strings.Split(m[1], ":"), m[2]
		if days, hours, ok := strings.Cut(fields[0], "-"); ok || strings.Contains(fields[0], ".") {
			if !ok {
				days, hours, _ = strings.Cut(fields[0], ".")
			}
			if len(fields) != 3 || len(hours) != 2 || hours > "23" {
				return 0, valueError("parse_duration", text, "a duration", durationHint)
			}
			fields = append([]string{days, hours}, fields[1:]...)
		}
		units := []int64{86400, 3600, 60, 1}[4-len(fields):]
		for i, field := range fields {
			if i > 0 && (len(field) != 2 || (units[i] < 3600 && field > "59")) {
				return 0, valueError("parse_duration", text, "a duration", durationHint)
			}
			if i == len(fields)-1 {
				field += fraction
			}
			if !add(field, big.NewRat(units[i], 1)) {
				return 0, valueError("parse_duration", text, "a duration", digitsHint)
			}
		}
		return float(total), nil
	case isoYearsMonths.MatchString(s):
		return 0, valueError("parse_duration", text, "a duration", "years and months have no fixed length in seconds")
	case isoDurationSyntax.MatchString(s) && s != "P" && !strings.HasSuffix(s, "T"):
		m := isoDurationSyntax.FindStringSubmatch(s)
		units := []int64{7 * 86400, 86400, 3600, 60, 1}
		last := -1
		for i := range units {
			if m[i+1] != "" {
				last = i
			}
		}
		for i, unit := range units {
			number := m[i+1]
			if number == "" {
				continue
			}
			// Only the last part may have a fraction, as ISO 8601 has it.
			if i != last && strings.Contains(number, ".") {
				return 0, valueError("parse_duration", text, "a duration", durationHint)
			}
			if !add(number, big.NewRat(unit, 1)) {
				return 0, valueError("parse_duration", text, "a duration", digitsHint)
			}
		}
		return float(total), nil
	}
	return 0, valueError("parse_duration", text, "a duration", durationHint)
}

var (
	rfc3339Syntax  = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt ]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?([Zz]|[+-][0-9]{2}(?::?[0-9]{2})?)?$`)
	httpDateSyntax = regexp.MustCompile(`^([A-Za-z]{3}), ([0-9]{1,2}) ([A-Za-z]{3}) ([0-9]{4}) ([0-9]{2}):([0-9]{2}):([0-9]{2}) ([A-Za-z]{3}|[+-][0-9]{4})$`)
)

var (
	weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	months   = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
)

// ParseTime reads a moment as Unix seconds, with the fraction of a second
// the text has: RFC 3339 and its ISO 8601 forms, with Z or an offset, and
// the HTTP date of RFC 1123, with GMT, UTC or an offset. A time without a
// zone is refused rather than taken as UTC or the exporter's own zone: it
// names no one moment.
func ParseTime(text string) (float64, error) {
	s := trimValue(text)
	bad := func(why string) (float64, error) { return 0, valueError("parse_time", text, "a time", why) }
	if m := rfc3339Syntax.FindStringSubmatch(s); m != nil {
		if m[8] == "" {
			return bad("it names no zone or offset, so it is no single moment; add Z or an offset")
		}
		offset, ok := zoneOffset(m[8])
		if !ok {
			return bad(timeHint)
		}
		seconds, ok := civilSeconds(atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]))
		if !ok {
			return bad(timeHint)
		}
		r := new(big.Rat).SetInt64(seconds - offset)
		if m[7] != "" {
			fraction, ok := decimal("." + m[7])
			if !ok {
				return bad(digitsHint)
			}
			r.Add(r, fraction)
		}
		return float(r), nil
	}
	if m := httpDateSyntax.FindStringSubmatch(s); m != nil {
		month := index(months, strings.ToLower(m[3]))
		zone := strings.ToLower(m[8])
		var offset int64
		switch {
		case zone == "gmt" || zone == "utc":
		case zone[0] == '+' || zone[0] == '-':
			var ok bool
			if offset, ok = zoneOffset(zone); !ok {
				return bad(timeHint)
			}
		default:
			return bad(timeHint)
		}
		seconds, ok := civilSeconds(atoi(m[4]), month+1, atoi(m[2]), atoi(m[5]), atoi(m[6]), atoi(m[7]))
		weekday := index(weekdays, strings.ToLower(m[1]))
		if month < 0 || weekday < 0 || !ok {
			return bad(timeHint)
		}
		if int64(weekday) != floorMod(floorDiv(seconds, 86400)+4, 7) {
			return bad("the weekday is not that of the date")
		}
		return float(new(big.Rat).SetInt64(seconds - offset)), nil
	}
	return bad(timeHint)
}

// zoneOffset reads Z, or an offset written +HH:MM, +HHMM or +HH, as the
// seconds the time is ahead of UTC.
func zoneOffset(zone string) (int64, bool) {
	if zone == "Z" || zone == "z" {
		return 0, true
	}
	digits := strings.ReplaceAll(zone[1:], ":", "")
	hours, minutes := atoi(digits[:2]), 0
	if len(digits) == 4 {
		minutes = atoi(digits[2:])
	}
	if hours > 23 || minutes > 59 {
		return 0, false
	}
	offset := int64(hours*3600 + minutes*60)
	if zone[0] == '-' {
		offset = -offset
	}
	return offset, true
}

// civilSeconds is the Unix time of a date and a time of day in UTC; ok is
// false for a date or a time that does not exist. The days are counted as in
// Howard Hinnant's days_from_civil, in whole numbers, which the Python
// helper counts alike.
func civilSeconds(year, month, day, hour, minute, second int) (int64, bool) {
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	monthDays := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[month-1]
	if month == 2 && year%4 == 0 && (year%100 != 0 || year%400 == 0) {
		monthDays = 29
	}
	if day > monthDays {
		return 0, false
	}
	y := int64(year)
	if month <= 2 {
		y--
	}
	// January and February of year 0000 count from year -1.
	era := floorDiv(y, 400)
	yoe := y - era*400
	mp := int64((month + 9) % 12)
	doy := (153*mp+2)/5 + int64(day) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	days := era*146097 + doe - 719468
	return days*86400 + int64(hour*3600+minute*60+second), true
}

// floorDiv is a divided by b, b positive, rounded down as the Python
// helper's // rounds, where Go's / rounds towards zero: the Unix time of a
// moment before 1970 is negative, and its day is the one before.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

// floorMod is what is left of a after floorDiv(a, b), never negative.
func floorMod(a, b int64) int64 { return a - floorDiv(a, b)*b }

// atoi reads the digits a pattern matched, which are a few at most.
func atoi(digits string) int {
	n := 0
	for _, c := range digits {
		n = n*10 + int(c-'0')
	}
	return n
}

func index(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}

var percentSyntax = regexp.MustCompile(`^([+-]?)` + decimalPattern + `[ \t]*%$`)

// ParsePercent reads a percentage as a ratio: 55.12% is 0.5512. Prometheus
// keeps ratios in its base unit, 0 to 1, so the value goes with the other
// ratios of a target and a recording rule needs no division by 100.
func ParsePercent(text string) (float64, error) {
	m := percentSyntax.FindStringSubmatch(trimValue(text))
	if m == nil {
		return 0, valueError("parse_percent", text, "a percentage", percentHint)
	}
	n, ok := decimal(m[2])
	if !ok {
		return 0, valueError("parse_percent", text, "a percentage", digitsHint)
	}
	n.Quo(n, big.NewRat(100, 1))
	if m[1] == "-" {
		n.Neg(n)
	}
	return float(n), nil
}
