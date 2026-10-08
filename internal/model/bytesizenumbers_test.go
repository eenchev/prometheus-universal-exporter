package model

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// sizeOf decodes a document of one key, a, whose value is a size as written.
func sizeOf(written string) (ByteSize, error) {
	var doc struct {
		A ByteSize `yaml:"a"`
	}
	err := yaml.Unmarshal([]byte("a: "+written+"\n"), &doc)
	return doc.A, err
}

// A number of bytes written without quotes is the whole number it equals
// however YAML writes it. A schema is handed the number and not how it was
// written, so it takes 1e3 and 1.0 for the 1000 and the 1 they are, and so
// does every key of a whole number; a size refused them, one as no size and
// the other as a fraction, so that an editor showed as valid a configuration
// that did not load.
func TestAWholeNumberOfBytesIsASizeHoweverYAMLWritesIt(t *testing.T) {
	for written, want := range map[string]ByteSize{
		"1e3": 1000, "1E3": 1000, "1e+3": 1000, "+1e3": 1000, "1.0": 1, "1.": 1, "1.000": 1, "1000.0": 1000,
		"2.5e2": 250, "2.50e2": 250, "0.5e1": 5, ".5e1": 5, "+.5e1": 5, "10e-1": 1, "1000e-3": 1, "12.50e+2": 1250,
		"1e6": 1_000_000, "1.048576e6": 1 << 20, "64e6": 64_000_000, "1e18": 1_000_000_000_000_000_000,
		"0.0": 0, "0e0": 0, "0e9": 0, "-0.0": 0, "-0e3": 0, "0.000": 0, ".0": 0,
		// An underscore is left out of a number, as it is of 1_000.
		"1_000.0": 1000, "1_0e2": 1000, "0_8": 8,
		// A tag says what the value is as well as its spelling does.
		"!!float 1e3": 1000, "!!float '1e3'": 1000, "!!float 1000": 1000,
		// What was a size is the size it was.
		"1000": 1000, "+1000": 1000, "1_000": 1000, "0x10": 16, "0o17": 15, "017": 15, "0b101": 5, "08": 8, "-0": 0,
		"'512'": 512, "1.5KiB": 1536, "'1.5 KiB'": 1536,
	} {
		if got, err := sizeOf(written); err != nil || got != want {
			t.Errorf("%s is read as %d, %v; want %d", written, got, err, want)
		}
	}
}

// A number is read by its digits and not by the float64 YAML makes of them,
// which holds no whole number past 2^53 exactly: a size is the number that
// is written, or it is refused, and never the float nearest to it.
func TestAWholeNumberOfBytesIsReadExactlyPastWhatAFloatHolds(t *testing.T) {
	for written, want := range map[string]ByteSize{
		"9007199254740993.0":       1<<53 + 1,
		"9.007199254740993e15":     1<<53 + 1,
		"9007199254740993":         1<<53 + 1,
		"9223372036854775807.0":    1<<63 - 1,
		"9.223372036854775807e18":  1<<63 - 1,
		"92233720368547758.07e2":   1<<63 - 1,
		"9223372036854775807":      1<<63 - 1,
		"9.223372036854775806e18":  1<<63 - 2,
		"4611686018427387904.0000": 1 << 62,
	} {
		if got, err := sizeOf(written); err != nil || got != want {
			t.Errorf("%s is read as %d, %v; want %d", written, got, err, want)
		}
	}
	// 2^63 and more is past what a size holds, a fraction a float64 rounds
	// away is one, and so is a number too small for a float64 to tell from 0.
	for _, written := range []string{
		"9223372036854775808.0", "9.223372036854775808e18", "9.3e18", "1e19", "1e300",
		"9223372036854775807.5", "1.00000000000000000001", "9007199254740992.5", "1e-400", "1e-9223372036854775808",
	} {
		if got, err := sizeOf(written); err == nil || !strings.Contains(err.Error(), "line 1: size ") {
			t.Errorf("%s is read as %d, %v; want it refused as no size", written, got, err)
		}
	}
	// An exponent no number of bytes has is read without the number being
	// written out. YAML takes a number with one for text, so only a tag
	// brings it here as a number.
	for _, written := range []string{"1e9223372036854775807", "1e-9223372036854775807", "1e99999999999999999999", "10e9223372036854775807", "0.1e9223372036854775807", "1e-99999999999999999999"} {
		if got, err := sizeOf("!!float " + written); err == nil || !strings.Contains(err.Error(), "line 1: size ") {
			t.Errorf("%s is read as %d, %v; want it refused as no size", written, got, err)
		}
	}
	if got, err := sizeOf("!!float 0e99999999999999999999"); err != nil || got != 0 {
		t.Errorf("0e99999999999999999999 is read as %d, %v; want 0", got, err)
	}
}

// What is not a whole number stays refused, with the message it had: a
// fraction, of a number in the range, past it or under 0, infinity and what
// is no number. And text is text: quoted, a number with a point or an
// exponent is held to the pattern of a size, which has neither without a
// unit, as the schema holds it.
func TestANumberThatIsNoWholeNumberOfBytesIsRefusedAsItWas(t *testing.T) {
	const (
		noSize   = `line 1: size %q is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`
		fraction = `line 1: size %q is not a whole number of bytes; write a whole number, or a number with a unit such as 1.5KiB`
	)
	for written, message := range map[string]string{
		"1.5": fraction, "0.5": fraction, "1000.001": fraction, "1.00000000000000000001": fraction,
		"9223372036854775807.5": fraction, "9223372036854775808.5": fraction, "99999999999999999999.5": fraction,
		"1e-1": noSize, "1.5e0": noSize, "2.55e1": noSize, "15e-1": noSize, ".5": noSize, "1.e-1": noSize, "1e-400": noSize, "92233720368547758085e-1": noSize,
		"-1.5": noSize, "-0.5": noSize, "-1e-1": noSize, "-15e-1": noSize, "-.5": noSize, "-1e-400": noSize, "-9223372036854775808.5": noSize,
		".inf": noSize, "+.inf": noSize, "-.inf": noSize, ".Inf": noSize, ".nan": noSize, ".NaN": noSize,
		"0x1p3": noSize, "1e3KiB": noSize, "-1e3KiB": noSize, "1e19KiB": noSize,
		// A number past what a float64 holds is text to YAML, and no number.
		"1e400": noSize, "-1e400": noSize,
	} {
		_, err := sizeOf(written)
		if want := fmt.Sprintf(message, written); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want %s", written, err, want)
		}
	}
	for quoted, message := range map[string]string{
		"1e3": noSize, "1E3": noSize, "1e+3": noSize, "2.5e2": noSize, "1.": noSize, "+1000": noSize, "0e0": noSize,
		"1.0": fraction, "1.5": fraction, "0.0": fraction, "1000.0": fraction,
	} {
		want := fmt.Sprintf(message, quoted)
		for _, written := range []string{"'" + quoted + "'", `"` + quoted + `"`, "!!str " + quoted} {
			if got, err := sizeOf(written); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s is read as %d, %v; want %s", written, got, err, want)
			}
		}
	}
	// A number of digits alone that no size holds was refused for what it
	// is, and is in the words it had: in quotes where the number is too long
	// for YAML to read as an integer, which the text of it refuses.
	for written, want := range map[string]string{
		"-1":                          "line 1: size -1 is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB",
		"9223372036854775808":         "line 1: size 9223372036854775808 is too large; a size is under 8EiB, which is 2^63 bytes",
		"99999999999999999999":        `line 1: size "99999999999999999999" is too large; a size is under 8EiB, which is 2^63 bytes`,
		"!!float 9223372036854775808": `line 1: size "9223372036854775808" is too large; a size is under 8EiB, which is 2^63 bytes`,
		"'9223372036854775808'":       `line 1: size "9223372036854775808" is too large; a size is under 8EiB, which is 2^63 bytes`,
		"!!float 8388608TiB":          `line 1: size "8388608TiB" is too large; a size is under 8EiB, which is 2^63 bytes`,
		"'-1'":                        `line 1: size "-1" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`,
		"'-1e3'":                      `line 1: size "-1e3" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`,
		"'1e19'":                      `line 1: size "1e19" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`,
		"!!str 1e19":                  `line 1: size "1e19" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`,
		"'9223372036854775808.0'":     `line 1: size "9223372036854775808.0" is not a whole number of bytes; write a whole number, or a number with a unit such as 1.5KiB`,
		"[1e3]":                       "line 1: expected a size, a number of bytes or a string such as 10MiB, not a list",
		"!!int 1e3":                   `line 1: size "1e3": strconv.ParseInt: parsing "1e3": invalid syntax`,
		"!!int 1e19":                  `line 1: size "1e19": strconv.ParseInt: parsing "1e19": invalid syntax`,
		"!!int -1e3":                  `line 1: size "-1e3": strconv.ParseInt: parsing "-1e3": invalid syntax`,
		"!!int -99999999999999999999": `line 1: size "-99999999999999999999": strconv.ParseInt: parsing "-99999999999999999999": value out of range`,
	} {
		if got, err := sizeOf(written); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%s is read as %d, %v; want %s", written, got, err, want)
		}
	}
}

// A whole number that no size holds is refused for what it is when it is
// written with a point or an exponent too, in the words of the same number
// written as an integer and shown as it is written: one of 2^63 or more is
// too large, and one under 0 is negative. Each was refused as a spelling —
// "not a number of bytes or a number with a unit", or "not a whole number of
// bytes" for 9223372036854775808.0, which is one — since the reading of a
// size once knew such a number as text alone.
func TestAWholeNumberNoSizeHoldsIsRefusedForWhatItIsHoweverItIsWritten(t *testing.T) {
	const (
		tooLarge = "line 1: size %s is too large; a size is under 8EiB, which is 2^63 bytes"
		negative = "line 1: size %s is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB"
		noSize   = `line 1: size %q is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`
	)
	for written, message := range map[string]string{
		"9223372036854775808.0": tooLarge, "9223372036854775808.": tooLarge, "9.223372036854775808e18": tooLarge, "9.3e18": tooLarge,
		"1e19": tooLarge, "1E19": tooLarge, "1e+19": tooLarge, "+1e19": tooLarge, "0.1e20": tooLarge, ".1e20": tooLarge, "100e17": tooLarge,
		"1e300": tooLarge, "1.5e19": tooLarge, "99999999999999999999.000": tooLarge, "1000000000000000000000e-2": tooLarge,
		// YAML leaves an underscore out of a number, and a sign makes a
		// number too long for an integer no text.
		"1_0e18": tooLarge, "1e3_0": tooLarge, "9_223_372_036_854_775_808.0": tooLarge, "+99999999999999999999": tooLarge,
		"-1e3": negative, "-1E3": negative, "-1.0": negative, "-1.": negative, "-1000.000": negative, "-.5e1": negative, "-0.5e1": negative, "-10e-1": negative,
		"-1e19": negative, "-1e300": negative, "-9223372036854775808.0": negative, "-9.223372036854775809e18": negative,
		"-1_000.0": negative, "-9223372036854775809": negative, "-99999999999999999999": negative,
		// An exponent no number of bytes has is read without the number
		// being written out, and says which way it is out of the range.
		"!!float 1e9223372036854775807": tooLarge, "!!float 10e9223372036854775807": tooLarge, "!!float 0.1e9223372036854775807": tooLarge,
		"!!float 1e400": tooLarge, "!!float -1e400": negative, "!!float 1e99999999999999999999": tooLarge, "!!float 0.000001e99999999999999999999": tooLarge, "!!float -1e99999999999999999999": negative,
		"!!float -10e9223372036854775807": negative, "!!float '1e19'": tooLarge, "!!float '-1e3'": negative,
		"!!float 1e-9223372036854775807": noSize, "!!float 1e-99999999999999999999": noSize, "!!float -1e-99999999999999999999": noSize,
		"!!float 1000000e-99999999999999999999": noSize, "!!float -10e-9223372036854775808": noSize,
	} {
		_, err := sizeOf(written)
		want := fmt.Sprintf(message, strings.Trim(strings.TrimPrefix(written, "!!float "), "'"))
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%s: %v; want %s", written, err, want)
		}
	}
	// The words are those of the same number written as an integer, but for
	// how the number is written.
	for written, integer := range map[string]string{
		"-1e3": "-1000", "-1.0": "-1", "-9223372036854775808.0": "-9223372036854775808",
		"9223372036854775808.0": "9223372036854775808", "9.223372036854775808e18": "9223372036854775808", "1e19": "10000000000000000000",
	} {
		_, err := sizeOf(written)
		_, integerErr := sizeOf(integer)
		if err == nil || integerErr == nil || err.Error() != strings.ReplaceAll(integerErr.Error(), "size "+integer+" is", "size "+written+" is") {
			t.Errorf("%s: %v; want what %s is refused with: %v", written, err, integer, integerErr)
		}
	}
	// Zero is no negative number, whatever its sign.
	for _, written := range []string{"-0.0", "-0e3", "-0.", "-.0", "-0e-3", "-0", "!!float -0e99999999999999999999"} {
		if got, err := sizeOf(written); err != nil || got != 0 {
			t.Errorf("%s is read as %d, %v; want 0", written, got, err)
		}
	}
}

// sizeAsItWasRead is ByteSize.UnmarshalYAML as it was before a number
// written with a point or an exponent came to be read as the whole number it
// is, and to be refused as one, kept as the oracle of the test below.
// parseByteSize, which reads the text of a size, is the one it called.
func sizeAsItWasRead(n *yaml.Node) (ByteSize, error) {
	if n.Kind != yaml.ScalarNode {
		return 0, lineError(n, "expected a size, a number of bytes or a string such as 10MiB, not %s", describeNode(n))
	}
	if n.Tag == "!!int" {
		v, err := strconv.ParseInt(n.Value, 0, 64)
		switch {
		case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(n.Value, "-"):
			return 0, lineError(n, "size %s is too large; a size is under 8EiB, which is 2^63 bytes", n.Value)
		case err != nil:
			return 0, lineError(n, "size %q: %v", n.Value, err)
		case v < 0:
			return 0, lineError(n, "size %s is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB", n.Value)
		}
		return ByteSize(v), nil
	}
	v, err := parseByteSize(n.Value)
	if err != nil {
		return 0, lineError(n, "%v", err)
	}
	return v, nil
}

// yamlWritesAFloat is a number as YAML writes one with a point or an
// exponent, or with neither: what the decoder reads as a number of the tag
// !!float where it is no integer to it.
var yamlWritesAFloat = regexp.MustCompile(`^([-+]?)(\.[0-9]+|[0-9]+(?:\.[0-9]*)?)(?:[eE]([-+]?[0-9]+))?$`)

// wholeNumberWritten is the whole number that a number written with a point
// or an exponent equals, in the range of a size or out of it, by the
// arithmetic of whole numbers of any length: its digits as one number,
// multiplied or divided by the power of ten its point and its exponent make.
// An exponent counts up to five thousand, which is further than any number
// the test below writes has digits.
func wholeNumberWritten(written string) (*big.Int, bool) {
	if !strings.HasPrefix(written, ".") {
		written = strings.ReplaceAll(written, "_", "")
	}
	parts := yamlWritesAFloat.FindStringSubmatch(written)
	if parts == nil {
		return nil, false
	}
	whole, fraction, _ := strings.Cut(parts[2], ".")
	digits, ok := new(big.Int).SetString(whole+fraction, 10)
	if !ok {
		return nil, false
	}
	power := -len(fraction)
	if parts[3] != "" {
		exponent, _ := strconv.Atoi(parts[3])
		power += min(max(exponent, -5000), 5000)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(power, -power))), nil)
	if power >= 0 {
		digits.Mul(digits, scale)
	} else if _, rest := digits.QuoRem(digits, scale, new(big.Int)); rest.Sign() != 0 {
		return nil, false
	}
	if parts[1] == "-" {
		digits.Neg(digits)
	}
	return digits, true
}

// A size is read as it was, but for the whole numbers written with a point
// or an exponent: over every number, text and tag the parts below make,
// what was a size is the same size, and what was refused is refused in the
// same words, unless YAML reads it as a number, of the tag !!float, that is
// a whole number. Such a number from 0 up to 2^63 is now that size; one
// under 0 is refused as negative and one of 2^63 or more as too large, in
// the words of an integer, where each was refused as a spelling — but for a
// number of digits alone, too long for YAML to read as an integer, which
// was refused as too large and is in the same words. The old reading is the
// oracle, and the whole number is worked out a second way, by the
// arithmetic of numbers of any length.
func TestASizeIsReadAsItWasButForAWholeNumberWrittenWithAPointOrAnExponent(t *testing.T) {
	// Under the race detector the fractions and the exponents are fewer, of
	// every kind still: none, of zero, whole, a fraction, past the range.
	fractions := alloctest.UnlessRaced([]string{"", ".", ".0", ".5", ".00", ".25", ".000000000000000000001"}, []string{"", ".0", ".5", ".000000000000000000001"})
	exponents := alloctest.UnlessRaced([]string{"", "e0", "e3", "E3", "e+3", "e-1", "e-3", "e18", "e19", "e400", "e-400"}, []string{"", "E3", "e-1", "e18", "e19", "e-400"})
	var written []string
	for _, sign := range []string{"", "+", "-"} {
		for _, whole := range []string{"", "0", "1", "08", "10", "250", "1_000", "9007199254740993", "9223372036854775807", "9223372036854775808", "99999999999999999999", "0x10", "0b101", "017"} {
			for _, fraction := range fractions {
				for _, exponent := range exponents {
					for _, unit := range []string{"", "KiB", " MB"} {
						number := sign + whole + fraction + exponent + unit
						// A dash alone opens a list, and nothing at all is
						// no value: neither is a way to write a number.
						if whole+fraction+exponent+unit == "" || strings.Contains(number, " ") {
							written = append(written, "'"+number+"'")
							continue
						}
						written = append(written, number, "'"+number+"'", "!!float "+number, "!!str "+number)
					}
				}
			}
		}
	}
	written = append(written, ".inf", "-.inf", ".nan", "~", "null", "true", "[1e3]", "{a: 1e3}", "lots", "!!int 1e3", "!!float '1e3'", "!!float lots", "2001-01-01", "1:30",
		"!!int 1e19", "!!int -1e3", "!!float '1e19'", "!!float '-1e3'", "!!float -lots", "-9223372036854775808", "-9223372036854775809", "18446744073709551616", "!!float 1e5000", "!!float -1e5000", "!!float 1e-5000")
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("- "+strings.Join(written, "\n- ")+"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	items := doc.Content[0].Content
	if len(items) != len(written) {
		t.Fatalf("the document has %d entries of the %d written", len(items), len(written))
	}
	same, refused, whole, negative, tooLarge := 0, 0, 0, 0, 0
	for i, n := range items {
		was, wasErr := sizeAsItWasRead(n)
		var now ByteSize
		nowErr := now.UnmarshalYAML(n)
		number, isWhole := wholeNumberWritten(n.Value)
		isWhole = isWhole && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!float"
		// The words of a whole number out of the range, when they are new.
		reason := ""
		switch {
		case wasErr == nil:
			same++
			if nowErr != nil || now != was {
				t.Errorf("%s was the size %d and is now %d, %v", written[i], was, now, nowErr)
			}
			if isWhole && (!number.IsInt64() || number.Int64() != int64(was)) {
				t.Errorf("%s was the size %d, and the whole number it is written as is %d", written[i], was, number)
			}
		case isWhole && number.Sign() >= 0 && number.IsInt64():
			whole++
			if nowErr != nil || int64(now) != number.Int64() {
				t.Errorf("%s, the whole number %d, is read as %d, %v", written[i], number, now, nowErr)
			}
		case isWhole && number.Sign() < 0:
			negative++
			reason = "size %s is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB"
		case isWhole && strings.Trim(n.Value, "0123456789") != "":
			tooLarge++
			reason = "size %s is too large; a size is under 8EiB, which is 2^63 bytes"
		default:
			refused++
			if nowErr == nil || nowErr.Error() != wasErr.Error() {
				t.Errorf("%s was refused with %q and is now read as %d, %v", written[i], wasErr, now, nowErr)
			}
			if isWhole && !strings.HasSuffix(wasErr.Error(), "is too large; a size is under 8EiB, which is 2^63 bytes") {
				t.Errorf("%s, the whole number %d, was refused with %q and is still", written[i], number, wasErr)
			}
		}
		if reason == "" {
			continue
		}
		if want := lineError(n, reason, n.Value).Error(); nowErr == nil || nowErr.Error() != want {
			t.Errorf("%s, the whole number %d, is read as %d, %v; want %q", written[i], number, now, nowErr, want)
		}
		if strings.Contains(wasErr.Error(), "is negative") || strings.Contains(wasErr.Error(), "is too large") {
			t.Errorf("%s, the whole number %d, was refused with %q: the words were of its reason already", written[i], number, wasErr)
		}
	}
	if same < len(written)/200 || refused < len(written)*2/3 || whole < len(written)/40 || negative < len(written)/50 || tooLarge < len(written)/50 {
		t.Errorf("of %d spellings %d were sizes, %d are refused as they were, %d are whole numbers that were refused, %d are negative ones and %d ones too large: the parts no longer make each kind", len(written), same, refused, whole, negative, tooLarge)
	}
	t.Logf("%d spellings: %d were sizes, %d are refused as they were, %d are whole numbers that were refused, %d are whole numbers under 0 and %d of 2^63 or more, refused for that now", len(written), same, refused, whole, negative, tooLarge)
}
