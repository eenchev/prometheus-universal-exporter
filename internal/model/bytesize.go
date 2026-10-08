package model

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ByteSize is a size in bytes. The configuration may write it as a plain
// number of bytes, 10485760, or with a unit, 10MiB: B; kB, MB, GB and TB,
// powers of 1000; KiB, MiB, GiB and TiB, powers of 1024. The unit is case
// insensitive and may follow a space, and the number may have a fraction,
// 1.5GiB, rounded down to whole bytes. A size is never negative, a number
// without a unit is a whole number of bytes, and a size is under 2^63 bytes,
// the most a ByteSize holds: anything else is refused rather than read as
// some other size. A number of bytes written without quotes is the whole
// number it equals however YAML writes it, with a point or an exponent too:
// 1e6 and 1048576.0 are sizes, as they are whole numbers at every other key
// that takes one and to the schema, which is handed the number and not how
// it was written.
type ByteSize int64

// ByteSizePattern is what UnmarshalYAML accepts as a string, exactly, but
// for the range, which no pattern can hold: a whole number of bytes, or a
// number, whole or with a fraction, followed by a unit, after one space or
// none. It is the pattern of a size in the configuration schema, so an editor
// flags the spellings the exporter refuses: a fraction without a unit, a
// sign, an exponent, an unknown unit, space around the size. It is of text
// alone: a number YAML reads as one is no string to a schema, and is read by
// wholeBytes.
const ByteSizePattern = `^([0-9]+|[0-9]+(\.[0-9]+)? ?([kKmMgGtT][iI]?[bB]?|[bB]))$`

var byteSizeRE = regexp.MustCompile(ByteSizePattern)

// fractionAloneRE is a number with a fraction and no unit, which gets a
// message of its own.
var fractionAloneRE = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

var byteSizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
	"ki": 1 << 10, "kib": 1 << 10, "mi": 1 << 20, "mib": 1 << 20, "gi": 1 << 30, "gib": 1 << 30, "ti": 1 << 40, "tib": 1 << 40,
}

// parseByteSize reads a size as the configuration writes it: what
// ByteSizePattern matches, under 2^63 bytes.
func parseByteSize(s string) (ByteSize, error) {
	// A fraction is for a unit to scale, as in 1.5GiB; there is no half
	// byte, and 1.5 alone is more likely a unit left off than one byte.
	if fractionAloneRE.MatchString(s) {
		return 0, fmt.Errorf("size %q is not a whole number of bytes; write a whole number, or a number with a unit such as 1.5KiB", s)
	}
	if !byteSizeRE.MatchString(s) {
		return 0, fmt.Errorf("size %q is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB", s)
	}
	end := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if end < 0 {
		end = len(s)
	}
	number, unit := s[:end], strings.ToLower(strings.TrimSpace(s[end:]))
	// A number of bytes is read as the whole number it is: as a float64 it
	// would be rounded past 2^53, and the largest size taken for 2^63.
	if unit == "" {
		bytes, err := strconv.ParseInt(number, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("size %q is too large; a size is under 8EiB, which is 2^63 bytes", s)
		}
		return ByteSize(bytes), nil
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	scale, ok := byteSizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("size %q has an unknown unit %q", s, unit)
	}
	bytes := math.Floor(value * scale)
	// 2^63 itself is past the most an int64 holds, and as a float64 it is
	// what math.MaxInt64 rounds to, so the comparison includes it.
	if bytes >= math.MaxInt64 {
		return 0, fmt.Errorf("size %q is too large; a size is under 8EiB, which is 2^63 bytes", s)
	}
	return ByteSize(bytes), nil
}

// numberTooLarge and numberNegative refuse a number YAML reads as one, shown
// as it is written, whether it is written as an integer or with a point or
// an exponent.
const (
	numberTooLarge = "size %s is too large; a size is under 8EiB, which is 2^63 bytes"
	numberNegative = "size %s is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB"
)

// UnmarshalYAML reads a size given as a number of bytes or as a string with
// a unit, such as 10MiB, reporting a bad one with its line.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return lineError(n, "expected a size, a number of bytes or a string such as 10MiB, not %s", describeNode(n))
	}
	if n.Tag == "!!int" {
		v, err := strconv.ParseInt(n.Value, 0, 64)
		switch {
		case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(n.Value, "-"):
			return lineError(n, numberTooLarge, n.Value)
		case err != nil:
			return lineError(n, "size %q: %v", n.Value, err)
		case v < 0:
			return lineError(n, numberNegative, n.Value)
		}
		*b = ByteSize(v)
		return nil
	}
	v, err := parseByteSize(n.Value)
	if err != nil {
		// What is no size as text is one as a number YAML reads with a
		// point or an exponent, when it is a whole number of bytes: the
		// schema takes 1e3 and 1.0 for the 1000 and the 1 it is handed, and
		// so does every other key of a whole number.
		if n.ShortTag() == "!!float" {
			// And a whole number no size holds is refused for what it is, in
			// the words of one written without a point or an exponent: 1e19
			// is too large and -1e3 negative, and neither is a spelling to
			// correct. Digits alone, which the text refuses as too large,
			// keep its words.
			switch whole, is := wholeBytes(n.Value); {
			case is == wholeSize:
				*b = whole
				return nil
			case is == wholeTooLarge && !byteSizeRE.MatchString(n.Value):
				return lineError(n, numberTooLarge, n.Value)
			case is == wholeNegative:
				return lineError(n, numberNegative, n.Value)
			}
		}
		return lineError(n, "%v", err)
	}
	*b = v
	return nil
}

// yamlFloatRE is a number as YAML writes one with a point or an exponent, in
// its parts: the sign, the digits before the point, those after it and the
// exponent.
var yamlFloatRE = regexp.MustCompile(`^([-+]?)([0-9]*)(?:\.([0-9]*))?(?:[eE]([-+]?[0-9]+))?$`)

// wholeNumber is what a number written with a point or an exponent is to a
// size: no whole number, a size, or a whole number no size holds.
type wholeNumber int

const (
	// notWhole is a number with a fraction, and what is no number: infinity,
	// not-a-number, text under the tag of a number.
	notWhole wholeNumber = iota
	// wholeSize is a whole number of bytes from 0 up to 2^63.
	wholeSize
	// wholeTooLarge is a whole number of 2^63 or more.
	wholeTooLarge
	// wholeNegative is a whole number under 0, however far.
	wholeNegative
)

// wholeBytes reads a number written with a point or an exponent, such as
// 1e3, 1.0 or 2.5e2, as the whole number of bytes it is, and says of one that
// is no size why: it has a fraction, it is negative, or it is 2^63 or more.
// It goes by the digits as ReadWholeNumber does.
func wholeBytes(written string) (ByteSize, wholeNumber) {
	whole, ok := ReadWholeNumber(written)
	switch {
	case !ok:
		return 0, notWhole
	case whole.Negative:
		return 0, wholeNegative
	case whole.Huge || whole.Magnitude > math.MaxInt64:
		return 0, wholeTooLarge
	}
	return ByteSize(whole.Magnitude), wholeSize
}

// WholeNumber is a whole number as a number YAML writes with a point or an
// exponent is one: its sign, and how far it is from 0.
type WholeNumber struct {
	// Negative is set of a whole number under 0: never of 0, however its
	// sign is written.
	Negative bool
	// Magnitude is how far the number is from 0, when that is under 2^64;
	// Huge is set, and Magnitude 0, when it is not.
	Magnitude uint64
	Huge      bool
}

// Int is the whole number as an integer of that many bits, and whether it is
// one: an int64 of the number when bits is 64, as it is of an int on the
// platforms the exporter is built for.
func (w WholeNumber) Int(bits int) (int64, bool) {
	// The least integer of the bits is as far from 0 as the largest is
	// plus one.
	least := uint64(1) << (bits - 1)
	switch {
	case w.Huge:
		return 0, false
	case w.Negative && w.Magnitude <= least:
		return -int64(w.Magnitude-1) - 1, true //nolint:gosec // at most 2^63 - 1 before the sign
	case !w.Negative && w.Magnitude < least:
		return int64(w.Magnitude), true //nolint:gosec // under the least's distance from 0, 2^63
	}
	return 0, false
}

// ReadWholeNumber reads a number written with a point or an exponent, such
// as 1e3, 1.0 or -2.5e2, as the whole number it is, and reports false of one
// that has a fraction and of what is no number: infinity, not-a-number, text
// under the tag of a number. It goes by the digits as they are written and
// not by the float64 YAML makes of them, which holds no whole number past
// 2^53 exactly: 9007199254740993.0 is that number and not the one below it,
// 9.223372036854775807e18 is the largest int64 and not 2^63, and a fraction
// too small for a float64 to tell from none is one. An underscore between
// the digits is left out, as the YAML decoder leaves it out of a number that
// does not start with its point. A size reads its numbers by it
// (wholeBytes), and so does the configuration's loader a whole number at a
// key of one.
func ReadWholeNumber(written string) (WholeNumber, bool) {
	if !strings.HasPrefix(written, ".") {
		written = strings.ReplaceAll(written, "_", "")
	}
	parts := yamlFloatRE.FindStringSubmatch(written)
	if parts == nil || parts[2]+parts[3] == "" {
		return WholeNumber{}, false
	}
	sign, fraction, exponent := parts[1], parts[3], parts[4]
	digits := strings.TrimLeft(parts[2]+fraction, "0")
	// Zero is zero whatever its sign and its exponent: -0.0 and 0e9 are 0,
	// as -0 is.
	if digits == "" {
		return WholeNumber{}, true
	}
	// The number is its digits without the zeros they end in, followed by
	// as many zeros as the exponent, the digits after the point and the
	// zeros taken off leave: fewer than none is a fraction.
	significant := strings.TrimRight(digits, "0")
	zeros := len(digits) - len(significant) - len(fraction)
	if exponent != "" {
		// An exponent counts no further than past every digit written and
		// the 20 of 2^64, where the number is too large or a fraction
		// whatever its digits; one too long to be read, which Atoi gives
		// as the furthest it reads, is further still. So the sum stays one
		// an int holds.
		shift, _ := strconv.Atoi(exponent)
		far := len(written) + 20
		zeros += min(max(shift, -far), far)
	}
	if zeros < 0 {
		return WholeNumber{}, false
	}
	whole := WholeNumber{Negative: sign == "-"}
	// 2^64 has 20 digits: more are too large, and are not written out.
	if zeros > 20-len(significant) {
		whole.Huge = true
		return whole, true
	}
	magnitude, err := strconv.ParseUint(significant+strings.Repeat("0", zeros), 10, 64)
	if err != nil {
		whole.Huge = true
		return whole, true
	}
	whole.Magnitude = magnitude
	return whole, true
}
