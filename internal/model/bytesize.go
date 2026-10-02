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
// some other size.
type ByteSize int64

// ByteSizePattern is what UnmarshalYAML accepts as a string, exactly, but
// for the range, which no pattern can hold: a whole number of bytes, or a
// number, whole or with a fraction, followed by a unit, after one space or
// none. It is the pattern of a size in the configuration schema, so an editor
// flags the spellings the exporter refuses: a fraction without a unit, a
// sign, an exponent, an unknown unit, space around the size.
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
			return lineError(n, "size %s is too large; a size is under 8EiB, which is 2^63 bytes", n.Value)
		case err != nil:
			return lineError(n, "size %q: %v", n.Value, err)
		case v < 0:
			return lineError(n, "size %s is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB", n.Value)
		}
		*b = ByteSize(v)
		return nil
	}
	v, err := parseByteSize(n.Value)
	if err != nil {
		return lineError(n, "%v", err)
	}
	*b = v
	return nil
}
