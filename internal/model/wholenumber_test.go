package model

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

// wholeBytesAsItWas is wholeBytes as it was before it was made of
// ReadWholeNumber, kept as the oracle of what a size reads a number written
// with a point or an exponent as.
func wholeBytesAsItWas(written string) (ByteSize, wholeNumber) {
	if !strings.HasPrefix(written, ".") {
		written = strings.ReplaceAll(written, "_", "")
	}
	parts := yamlFloatRE.FindStringSubmatch(written)
	if parts == nil || parts[2]+parts[3] == "" {
		return 0, notWhole
	}
	sign, fraction, exponent := parts[1], parts[3], parts[4]
	digits := strings.TrimLeft(parts[2]+fraction, "0")
	if digits == "" {
		return 0, wholeSize
	}
	significant := strings.TrimRight(digits, "0")
	zeros := len(digits) - len(significant) - len(fraction)
	if exponent != "" {
		shift, _ := strconv.Atoi(exponent)
		far := len(written) + 19
		zeros += min(max(shift, -far), far)
	}
	switch {
	case zeros < 0:
		return 0, notWhole
	case sign == "-":
		return 0, wholeNegative
	case zeros > 19-len(significant):
		return 0, wholeTooLarge
	}
	bytes, err := strconv.ParseInt(significant+strings.Repeat("0", zeros), 10, 64)
	if err != nil {
		return 0, wholeTooLarge
	}
	return ByteSize(bytes), wholeSize
}

// numbersWritten are numbers as YAML writes them with a point or an
// exponent, and some that are no number, made of every sign, run of digits,
// fraction and exponent below: in the range of an int64 and of a uint64,
// just past each, far past, and fractions too small for a float64 to hold.
func numbersWritten() []string {
	var written []string
	for _, sign := range []string{"", "+", "-"} {
		for _, whole := range []string{"", "0", "1", "08", "10", "1_000", "9007199254740993", "9223372036854775807", "9223372036854775808", "18446744073709551615", "18446744073709551616", "99999999999999999999"} {
			for _, fraction := range []string{"", ".", ".0", ".5", ".25", ".000000000000000000001"} {
				for _, exponent := range []string{"", "e0", "e3", "E+3", "e-1", "e-3", "e18", "e19", "e20", "e400", "e-400", "e9223372036854775807", "e99999999999999999999"} {
					written = append(written, sign+whole+fraction+exponent)
				}
			}
		}
	}
	return append(written, ".inf", "-.inf", ".nan", "lots", "1e", "e3", ".", "-", "", "0x10", "1.5.5", "9.223372036854775807e18", "-9.223372036854775808e18", "-9.223372036854775809e18", "1.8446744073709551615e19", "1.8446744073709551616e19")
}

// A size reads a number written with a point or an exponent as it did when
// the reading was its own: ReadWholeNumber, which the loader now reads a
// whole number at a key of one by too, gives wholeBytes the same verdict
// and the same number of bytes as the code it was, over every number of
// numbersWritten.
func TestASizeReadsAWholeNumberAsItDidBeforeTheReadingWasShared(t *testing.T) {
	for _, written := range numbersWritten() {
		wasBytes, was := wholeBytesAsItWas(written)
		if nowBytes, now := wholeBytes(written); now != was || nowBytes != wasBytes {
			t.Errorf("%q was %d, %v and is %d, %v", written, wasBytes, was, nowBytes, now)
		}
	}
}

// ReadWholeNumber gives the whole number that is written, by its digits,
// negative too, up to 2^64 - 1 from 0 either way, and reports a fraction and
// what is no number as none: the arithmetic of numbers of any length is the
// oracle. And Int holds a whole number to the range of an integer of so many
// bits, the least among them.
func TestAWholeNumberIsReadByItsDigitsWithItsSign(t *testing.T) {
	limit := new(big.Int).Lsh(big.NewInt(1), 64)
	for _, written := range numbersWritten() {
		got, isWhole := ReadWholeNumber(written)
		want, wantWhole := wholeNumberWritten(written)
		switch {
		case written == "-" || written == ".":
			// Neither is a number to either.
			if isWhole {
				t.Errorf("%q is read as the whole number %+v", written, got)
			}
			continue
		case isWhole != wantWhole:
			t.Errorf("%q is a whole number %v, %+v; want %v, %v", written, isWhole, got, wantWhole, want)
			continue
		case !isWhole:
			continue
		}
		magnitude := new(big.Int).Abs(want)
		if got.Negative != (want.Sign() < 0) || got.Huge != (magnitude.Cmp(limit) >= 0) || (!got.Huge && got.Magnitude != magnitude.Uint64()) {
			t.Errorf("%q is read as %+v; want %v", written, got, want)
		}
		for _, bits := range []int{8, 32, 64} {
			least, most := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), uint(bits-1))), new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)), big.NewInt(1))
			held := want.Cmp(least) >= 0 && want.Cmp(most) <= 0
			if v, ok := got.Int(bits); ok != held || (held && v != want.Int64()) {
				t.Errorf("%q, %v, is %d, %v as an integer of %d bits; want %v", written, want, v, ok, bits, held)
			}
		}
	}
	for written, want := range map[string]int64{"9.223372036854775807e18": math.MaxInt64, "-9.223372036854775808e18": math.MinInt64, "9007199254740993.0": 1<<53 + 1, "-0.0": 0, "-1e3": -1000} {
		if got, isWhole := ReadWholeNumber(written); !isWhole {
			t.Errorf("%s is no whole number", written)
		} else if v, ok := got.Int(64); !ok || v != want {
			t.Errorf("%s is %d, %v; want %d", written, v, ok, want)
		}
	}
}
