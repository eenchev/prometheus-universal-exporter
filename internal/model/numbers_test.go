package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// formerParseNumber is parseNumber as it was, reading whatever
// strconv.ParseFloat reads, for the differential test below.
func formerParseNumber(text string) (float64, error) {
	f, err := strconv.ParseFloat(text, 64)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, strconv.ErrRange):
		return 0, fmt.Errorf("value %s is beyond the range of a 64-bit float", QuoteValue(text))
	default:
		return 0, fmt.Errorf("value %s is not a number; map text to numbers with value_map", QuoteValue(text))
	}
}

// goNumberSyntax reports whether text is written in one of the two forms
// that are Go's alone: with an underscore, or, after its sign, as a
// hexadecimal number.
func goNumberSyntax(text string) bool {
	unsigned := strings.TrimLeft(text, "+-")
	return strings.Contains(text, "_") || strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X")
}

// Text is read as a number as it was but for the two forms that are Go's own
// way of writing one: of some thirty thousand texts — every sign before every
// way of writing digits, a point, an exponent, a base, an underscore, NaN and
// the infinities in any case, and what is no number at all, with blanks
// around them or without — every one without an underscore that is not
// hexadecimal reads as the same number, bit for bit, or fails with the same
// words. One with an underscore, or in hexadecimal, that was read as a number
// or found beyond a float's range is now text that is no number, in the words
// every such text gets.
func TestTextIsReadAsANumberAsBeforeButForGoSyntax(t *testing.T) {
	signs := []string{"", "+", "-", "--", "+-"}
	digits := []string{
		"0", "1", "42", "007", "123456789012345678901234567890", ".5", "5.", "1.5", "0.1", ".", "",
		"1_000", "1_0.5", "1__0", "_1", "1_", "0_1", "1.5_5", "1._5",
		"0x1", "0X1F", "0x1.8", "0x.8", "0x", "0x_1", "0x1_f", "0b101", "0o17", "0B1", "0O7", "017",
		"inf", "Inf", "INF", "infinity", "Infinity", "INFINITY", "infi", "in", "nan", "NaN", "NAN", "na",
		"1,5", "1 5", "x", "1x", "x1", "٣", "１", "1e", "e", "true", "N/A",
	}
	exponents := []string{"", "e3", "E3", "e-3", "e+3", "e03", "e400", "e-400", "e1_0", "e_1", "e", "p-2", "P4", "p0", "p+1", "p1_0", "p99999", "p-99999", "p", "f", "e3e3", ".0"}
	blanks := [][2]string{{"", ""}, {" ", ""}, {"", " "}, {"\t ", " \n"}, {" ", ""}}
	same, changed, underscores, hexadecimal := 0, 0, 0, 0
	for _, sign := range signs {
		for _, digit := range digits {
			for _, exponent := range exponents {
				for _, blank := range blanks {
					written := blank[0] + sign + digit + exponent + blank[1]
					// Number trims the text it is given, as it did.
					text := strings.TrimSpace(written)
					want, wantErr := formerParseNumber(text)
					got, err := Number(written)
					if math.Float64bits(got) == math.Float64bits(want) && fmt.Sprint(err) == fmt.Sprint(wantErr) {
						same++
						continue
					}
					changed++
					if !goNumberSyntax(text) {
						t.Fatalf("%q: got %v, %v; was %v, %v, and it has no underscore and is not hexadecimal", written, got, err, want, wantErr)
					}
					if wantErr != nil && !strings.Contains(wantErr.Error(), "beyond the range") {
						t.Fatalf("%q was refused in other words: %v, and is now %v", written, wantErr, err)
					}
					if notANumber := fmt.Sprintf("value %s is not a number; map text to numbers with value_map", QuoteValue(text)); err == nil || err.Error() != notANumber || got != 0 {
						t.Fatalf("%q: got %v, %v; want %s", written, got, err, notANumber)
					}
					if strings.Contains(text, "_") {
						underscores++
					} else {
						hexadecimal++
					}
				}
			}
		}
	}
	if same < 25000 || underscores < 100 || hexadecimal < 100 {
		t.Errorf("%d texts read as before, %d with an underscore and %d in hexadecimal changed: the table does not cover all three", same, underscores, hexadecimal)
	}
	// And the other way round: no text in either form is a number.
	for _, sign := range signs {
		for _, digit := range digits {
			for _, exponent := range exponents {
				if text := sign + digit + exponent; goNumberSyntax(text) {
					if got, err := Number(text); err == nil {
						t.Errorf("%q is read as %v", text, got)
					}
				}
			}
		}
	}
}

// The forms a number is written in are read as the numbers they are:
// integers, decimals with or without digits on either side of the point,
// exponents, a leading sign, blanks around the text, NaN and the infinities
// in any case, an integer beyond what a float64 holds exactly as the nearest
// it has. Digits separated by underscores and hexadecimal floating-point, the
// two forms only Go writes, are text that is no number, as a hexadecimal,
// octal or binary integer always was; the Prometheus decoder refuses both as
// well. json.Number, which a JSON response's numbers are, is read alike.
func TestTheFormsANumberIsWrittenInAreReadAndGoSyntaxIsNot(t *testing.T) {
	for text, want := range map[string]float64{
		"42": 42, "-17": -17, "+7": 7, "007": 7, "3.14159": 3.14159, ".5": 0.5, "5.": 5, "-.5": -0.5,
		"1.5e3": 1500, "2E-3": 0.002, "6.02e+23": 6.02e23,
		"Inf": math.Inf(1), "+Inf": math.Inf(1), "-inf": math.Inf(-1), "Infinity": math.Inf(1), "-INFINITY": math.Inf(-1), "+infinity": math.Inf(1),
		"9007199254740993": 9007199254740992, "1e-400": 0,
	} {
		for _, value := range []any{text, " " + text + " ", "\t" + text + "\n", json.Number(text)} {
			if got, err := Number(value); err != nil || got != want {
				t.Errorf("Number(%#v): got %v, %v; want %v", value, got, err, want)
			}
		}
	}
	for _, text := range []string{"NaN", "nan", "NAN"} {
		if got, err := Number(text); err != nil || !math.IsNaN(got) {
			t.Errorf("Number(%q): got %v, %v; want NaN", text, got, err)
		}
	}
	for _, text := range []string{"1_000", "1_000.5", "1e1_0", "-1_0", "0x1p-2", "0X1P-2", "+0x1p4", "0x1.8p1", "0x_1p0", "0x1p99999", "1_0e999"} {
		want := fmt.Sprintf("value %q is not a number; map text to numbers with value_map", text)
		for _, value := range []any{text, " " + text + " ", json.Number(text)} {
			if got, err := Number(value); err == nil || err.Error() != want || got != 0 {
				t.Errorf("Number(%#v): got %v, %v; want %s", value, got, err, want)
			}
		}
		if got, err := ParseFloat(text); !errors.Is(err, strconv.ErrSyntax) || got != 0 {
			t.Errorf("ParseFloat(%q): got %v, %v; want strconv's syntax error", text, got, err)
		}
	}
	for _, text := range []string{"0x1F", "0o17", "0b101", "1,234", "85%", "12.5 MB", "true", "-", "+nan", "１２"} {
		if got, err := Number(text); err == nil || !strings.Contains(err.Error(), "is not a number; map text to numbers with value_map") {
			t.Errorf("Number(%q): got %v, %v; want it refused as no number", text, got, err)
		}
	}
	if _, err := Number("1e999"); err == nil || err.Error() != `value "1e999" is beyond the range of a 64-bit float` {
		t.Errorf("a number beyond a float64: %v", err)
	}
	if got, err := ParseFloat("1e999"); !errors.Is(err, strconv.ErrRange) || !math.IsInf(got, 1) {
		t.Errorf("ParseFloat of a number beyond a float64: %v, %v", got, err)
	}
}

// Reading a number allocates nothing, with the look for Go's forms as
// without it.
func TestReadingANumberAllocatesNothing(t *testing.T) {
	for _, text := range []string{"42", "-17.25", "6.02e+23", "123456789012345678901234567890", "NaN", "+Inf"} {
		if allocs := testing.AllocsPerRun(100, func() {
			if _, err := ParseFloat(text); err != nil {
				t.Fatal(err)
			}
		}); allocs != 0 {
			t.Errorf("ParseFloat(%q) allocates %v times", text, allocs)
		}
	}
}
