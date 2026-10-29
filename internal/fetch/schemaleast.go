package fetch

import (
	"math/big"
	"strconv"
	"strings"
	"time"
)

// A key with a least value, a duration such as limits.script_timeout or a
// size such as limits.max_script_memory, was held by the schemas to its
// least only where it is written as a number: a schema compares numbers, and
// a duration or a size written as text, 500us or 16MiB, is text to it. Text
// can still be held to a least by its pattern: a number with one unit is
// under the least when the number is under the least divided by the unit,
// and whether a decimal number is under a given one is a matter of its
// digits, which a regular expression can say (decimalBelow).
// SchemaLeastDuration and SchemaLeastSize are what a key with such a least
// refuses, under not, so an editor flags what the exporter refuses when the
// configuration loads.
//
// What is left to the exporter is a value of several parts, 500us500us,
// whose parts a pattern cannot add up: each part under the least says
// nothing of their sum. Such a value is rare, and the exporter refuses it
// with the same message.

// durationUnits are the units of a duration, as time.ParseDuration reads
// them, with the nanoseconds in each.
var durationUnits = []struct {
	pattern string
	scale   int64
}{
	{"ns", 1}, {"(us|µs|μs)", int64(time.Microsecond)}, {"ms", int64(time.Millisecond)},
	{"s", int64(time.Second)}, {"m", int64(time.Minute)}, {"h", int64(time.Hour)},
}

// sizeUnits are the units of a size, as model.ByteSize reads them, in any
// case and after one space or none, with the bytes in each; a size without a
// unit is a number of bytes.
var sizeUnits = []struct {
	pattern string
	scale   int64
}{
	{"", 1}, {" ?[bB]", 1},
	{" ?[kK][bB]?", 1e3}, {" ?[mM][bB]?", 1e6}, {" ?[gG][bB]?", 1e9}, {" ?[tT][bB]?", 1e12},
	{" ?[kK][iI][bB]?", 1 << 10}, {" ?[mM][iI][bB]?", 1 << 20}, {" ?[gG][iI][bB]?", 1 << 30}, {" ?[tT][iI][bB]?", 1 << 40},
}

// belowPattern is the pattern of text that is one number with one of the
// units, after a + or not when sign says, whose number times the unit's
// scale is under least: a value of under least nanoseconds or bytes, which
// the exporter, which rounds a fraction of a nanosecond or a byte down,
// reads as less than least.
func belowPattern(least int64, sign string, units []struct {
	pattern string
	scale   int64
}) string {
	alternatives := make([]string, 0, len(units))
	for _, unit := range units {
		alternatives = append(alternatives, "("+decimalBelow(big.NewRat(least, unit.scale))+")"+unit.pattern)
	}
	return "^" + sign + "(" + strings.Join(alternatives, "|") + ")$"
}

// SchemaLeastDuration is what a duration key whose value is at least least,
// such as limits.script_timeout's 1ms, refuses, to go under not; zero says
// whether the key takes zero, as the default or as nothing. Text of one part
// under the least is refused, but, where zero is taken, a part that comes to
// no whole nanosecond, which is the zero the exporter reads it as. Where
// zero is refused, as by failure_backoff.initial, the number 0 and a zero
// written as text in any way, -0s and 0s0m too, are refused with it.
func SchemaLeastDuration(least time.Duration, zero bool) map[string]any {
	below := map[string]any{"type": "string", "pattern": belowPattern(int64(least), `\+?`, durationUnits)}
	if zero {
		below["not"] = map[string]any{"pattern": belowPattern(1, `\+?`, durationUnits)}
		return below
	}
	return map[string]any{"anyOf": []any{
		map[string]any{"const": 0},
		map[string]any{"type": "string", "pattern": schemaZeroDuration},
		below,
	}}
}

// schemaZeroDuration is a zero written as a duration in any way: 0, with a
// sign or not, or parts of zero with units, such as 0s, -0.0m and 0s0ms.
const schemaZeroDuration = `^[-+]?(0|((0+(\.0*)?|\.0+)(ns|us|µs|μs|ms|s|m|h))+)$`

// SchemaLeastSize is what a size key whose value, when it is not zero, is at
// least least bytes, such as limits.max_script_memory's 32MiB, refuses, to
// go under not: text of a number, with a unit or without, that comes to at
// least a byte and under the least. A size that comes to no whole byte, 0.5B, is the zero, and the
// default, it is to the exporter. A size written as a number is held to the
// least by the key's minimum.
func SchemaLeastSize(least int64) map[string]any {
	return map[string]any{
		"type":    "string",
		"pattern": belowPattern(least, "", sizeUnits),
		"not":     map[string]any{"pattern": belowPattern(1, "", sizeUnits)},
	}
}

// decimalBelow is a regular expression of the decimal numbers under c, a
// positive rational: digits, with a point and more digits or not, which
// leading zeros do not change. A number is under c when its whole part is
// under c's, whatever its fraction, or is c's and its fraction is under c's,
// digit by digit: at the first digit where the two differ, its digit is the
// smaller, or it has run out of digits while c's fraction has more that are
// not zero. c's fraction may have no end, as a third does, 0.333…: its
// digits then repeat from some place on, and the repeating ones are a group
// that the expression repeats.
func decimalBelow(c *big.Rat) string {
	whole := new(big.Int).Quo(c.Num(), c.Denom())
	remainder := new(big.Int).Rem(c.Num(), c.Denom())
	digits := whole.String()
	if digits == "0" {
		digits = ""
	}
	var alternatives []string
	if n := len(digits); n > 0 {
		// A whole part of fewer digits, or of as many and smaller, with
		// any fraction.
		shorter := ""
		if n > 1 {
			shorter = "([1-9][0-9]{0," + strconv.Itoa(n-2) + "})?"
		}
		alternatives = append(alternatives, shorter)
		for i := 0; i < n; i++ {
			low, high := byte('0'), digits[i]-1
			if i == 0 {
				low = '1'
			}
			if high < low {
				continue
			}
			alternatives = append(alternatives, digits[:i]+digitRange(low, high)+repeatAny(n-i-1))
		}
		for i := range alternatives {
			alternatives[i] += `(\.[0-9]*)?`
		}
	}
	if remainder.Sign() != 0 {
		alternatives = append(alternatives, digits+`(\.`+fractionBelow(remainder, c.Denom())+`)?`)
	}
	return "0*(" + strings.Join(alternatives, "|") + ")"
}

// fractionBelow is a regular expression of the digits of a fraction, after
// the point, under num/den, which is more than 0 and less than 1: the
// alternatives decimalBelow says, the digits that do not repeat first and
// then those that do, which long division finds as the remainders come
// round again.
func fractionBelow(num, den *big.Int) string {
	var fraction []byte
	seen := map[string]int{}
	remainder := new(big.Int).Set(num)
	ten := big.NewInt(10)
	start := -1
	for remainder.Sign() != 0 {
		key := remainder.String()
		if at, ok := seen[key]; ok {
			start = at
			break
		}
		seen[key] = len(fraction)
		remainder.Mul(remainder, ten)
		digit := new(big.Int)
		digit.QuoRem(remainder, den, remainder)
		// A digit of a fraction under 1 is from 0 to 9.
		fraction = append(fraction, "0123456789"[digit.Int64()])
	}
	// below is a digit of c's after the digits before it: a smaller digit
	// and any after it, or no digit at all, since what follows in c is not
	// all zeros.
	below := func(d byte) string {
		if d == '0' {
			return ""
		}
		return "(" + digitRange('0', d-1) + "[0-9]*)?"
	}
	var alternatives []string
	head := fraction
	if start >= 0 {
		head = fraction[:start]
	}
	for i := range head {
		alternatives = append(alternatives, string(head[:i])+below(head[i]))
	}
	if start >= 0 {
		cycle := fraction[start:]
		var ends []string
		for j := range cycle {
			ends = append(ends, string(cycle[:j])+below(cycle[j]))
		}
		alternatives = append(alternatives, string(head)+"("+string(cycle)+")*("+strings.Join(ends, "|")+")")
	}
	return "(" + strings.Join(alternatives, "|") + ")"
}

// digitRange is the class of the digits from low to high.
func digitRange(low, high byte) string {
	if low == high {
		return string(low)
	}
	return "[" + string(low) + "-" + string(high) + "]"
}

// repeatAny is n digits of any value.
func repeatAny(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "[0-9]"
	}
	return "[0-9]{" + strconv.Itoa(n) + "}"
}
