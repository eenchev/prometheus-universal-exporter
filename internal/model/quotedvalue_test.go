package model

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// quotedValues are values an error may show: of the lengths around the 64
// bytes a value is cut at, of characters of two, three and four bytes the
// cut would fall in, with quotes, backslashes and control characters, and
// of bytes that are no UTF-8.
func quotedValues() []string {
	values := []string{"", "a", "n/a", `a"b`, `a\b`, "a\nb\t\x00\x7f", "é", "日本", "\xff", "a\xffb", "\x80\x80", `"`, `\`, "'", "`", " ", "�", "\U0001F600"}
	for _, unit := range []string{"a", "é", "日", "\U0001F600", `"`, "\x01", "\xff", "\x80"} {
		for _, lead := range []string{"", "a", "aa", "aaa"} {
			for _, bytes := range []int{60, 61, 62, 63, 64, 65, 66, 67, 68, 100, 1000} {
				values = append(values, lead+strings.Repeat(unit, (bytes-len(lead))/len(unit)))
			}
		}
	}
	random := rand.New(rand.NewPCG(18, 64))
	const alphabet = "ab \"\\\n\x00é日\xff\x80\U0001F600"
	for range 3000 {
		var b strings.Builder
		for n := random.IntN(140); b.Len() < n; {
			at := random.IntN(len(alphabet))
			b.WriteByte(alphabet[at])
		}
		values = append(values, b.String())
	}
	return values
}

// A value an error shows through Quoted reads as QuoteValue quotes it,
// whatever it holds, given as text or as bytes: as %q quotes it up to 64
// bytes, and past that by its first 64 bytes, to a character boundary, and
// its length. Bare shows it without quotes: as %s writes it up to 64 bytes,
// and cut alike past that. What each is recognised by is the same text with
// the mark in place of the length, and so the text itself for a value that
// is not cut.
func TestQuotedAndBareShowAValueAsQuoteValueCutsIt(t *testing.T) {
	cut := 0
	for _, value := range quotedValues() {
		quoted, bare := Quoted(value), Bare(value)
		if got := quoted.String(); got != QuoteValue(value) || Quoted([]byte(value)).String() != got {
			t.Fatalf("Quoted(%q) is %s, and %s of its bytes; QuoteValue gives %s", value, got, Quoted([]byte(value)), QuoteValue(value))
		}
		if Bare([]byte(value)) != bare {
			t.Fatalf("Bare(%q) is %s of the text and %s of its bytes", value, bare, Bare([]byte(value)))
		}
		if len(value) <= 64 {
			if quoted.String() != fmt.Sprintf("%q", value) || quoted.String() != fmt.Sprintf("%q", []byte(value)) || quoted.Same() != quoted.String() {
				t.Errorf("Quoted(%q) is %s, recognised by %s; %%q writes %q", value, quoted, quoted.Same(), value)
			}
			if bare.String() != value || bare.Same() != value {
				t.Errorf("Bare(%q) is %s, recognised by %s", value, bare, bare.Same())
			}
			continue
		}
		cut++
		length := fmt.Sprintf("... (%d bytes)", len(value))
		head, ok := strings.CutSuffix(quoted.String(), length)
		shown, err := strconv.Unquote(head)
		if !ok || err != nil || !strings.HasPrefix(value, shown) || len(shown) > 64 {
			t.Fatalf("Quoted(%q) is %s", value, quoted)
		}
		if bare.String() != shown+length || bare.Same() != shown+"... (# bytes)" || quoted.Same() != head+"... (# bytes)" {
			t.Errorf("of %q, Bare is %s, recognised by %s, and Quoted is recognised by %s; want the start %q and the mark", value, bare, bare.Same(), quoted.Same(), shown)
		}
	}
	if cut < 1000 {
		t.Errorf("%d of the values are over 64 bytes: the table should hold many", cut)
	}
}

// Errorf writes a QuotedValue as it shows the value whatever verb formats
// it, among positions and wrapped errors, and the failure is recognised with
// the mark for the length of a value that was cut: a value twice as long
// that starts the same, on another line, is the same failure, one that
// starts otherwise another, and a value within 64 bytes is part of what the
// failure is, as it was.
func TestErrorfShowsAQuotedValueAndRecognisesItWithoutItsLength(t *testing.T) {
	long := strings.Repeat("a", 1000)
	made := func(line int, value string) error {
		inner := Errorf("expected float as value, got %s", Quoted(value))
		return fmt.Errorf("decoding: %w", Errorf("line %d: %w; the %v of %[4]s", Position(line), inner, Bare(value), Bare(value)))
	}
	err := made(3, long)
	head := strings.Repeat("a", 64)
	if want := `decoding: line 3: expected float as value, got "` + head + `"... (1000 bytes); the ` + head + `... (1000 bytes) of ` + head + `... (1000 bytes)`; err.Error() != want {
		t.Errorf("the error is %q, want %q", err, want)
	}
	same := SameFailureText(err)
	if want := `decoding: line #: expected float as value, got "` + head + `"... (# bytes); the ` + head + `... (# bytes) of ` + head + `... (# bytes)`; same != want {
		t.Errorf("the failure is recognised by %q, want %q", same, want)
	}
	if again := made(7, long+long); SameFailureText(again) != same || again.Error() == err.Error() {
		t.Errorf("a value twice as long, on another line, is %q, recognised by %q", again, SameFailureText(again))
	}
	if other := made(3, "b"+long); SameFailureText(other) == same {
		t.Errorf("a value that starts otherwise is recognised as the same failure: %q", SameFailureText(other))
	}
	short := made(3, "n/a")
	if short.Error() != `decoding: line 3: expected float as value, got "n/a"; the n/a of n/a` || SameFailureText(short) != `decoding: line #: expected float as value, got "n/a"; the n/a of n/a` {
		t.Errorf("a short value is shown as %q, recognised by %q", short, SameFailureText(short))
	}
	if SameFailureText(made(3, "n/b")) == SameFailureText(short) {
		t.Error("two short values are one failure")
	}
	for _, verb := range []string{"%s", "%v", "%q", "%d", "%x", "%+v"} {
		if got := fmt.Sprintf(verb, Quoted("n/a")); got != `"n/a"` {
			t.Errorf("%s writes Quoted(\"n/a\") as %s", verb, got)
		}
	}
	var wrapped *movingError
	if !errors.As(err, &wrapped) || errors.Unwrap(wrapped) == nil {
		t.Errorf("the error does not wrap what its %%w names: %#v", err)
	}
	joined := ShownAs(Bare("m").String()+"{a="+Quoted(long).String()+"}", Bare("m").Same()+"{a="+Quoted(long).Same()+"}")
	series := Errorf("second sample for the %s", joined)
	if series.Error() != `second sample for the m{a="`+head+`"... (1000 bytes)}` || SameFailureText(series) != `second sample for the m{a="`+head+`"... (# bytes)}` {
		t.Errorf("a text of several values is shown as %q, recognised by %q", series, SameFailureText(series))
	}
}

// A QuotedValue holds nothing of the value it was made of: a megabyte of
// text is shown, quoted or bare, in a text of its own under 100 bytes made
// in a few allocations, none of them of the value's size, and a short value
// shown bare is a copy, not the part of the response it was given.
func TestAQuotedValueHoldsNothingOfTheValue(t *testing.T) {
	long := strings.Repeat("a", 1<<20)
	asBytes := []byte(long)
	var shown QuotedValue
	for name, show := range map[string]func(){
		"Quoted of text":  func() { shown = Quoted(long) },
		"Quoted of bytes": func() { shown = Quoted(asBytes) },
		"Bare of text":    func() { shown = Bare(long) },
		"Bare of bytes":   func() { shown = Bare(asBytes) },
	} {
		if allocs := alloctest.AllocsAtMost(20, 6, show); allocs > 6 || len(shown.String()) > 100 || len(shown.Same()) > 100 {
			t.Errorf("%s of a megabyte allocates %v times for texts of %d and %d bytes", name, allocs, len(shown.String()), len(shown.Same()))
		}
	}
	body := strings.Repeat("name ", 1000)
	part := body[5:9]
	if bare := Bare(part); bare.String() != "name" || unsafe.StringData(bare.String()) == unsafe.StringData(part) {
		t.Errorf("a short value shown bare is %q, and is the part of the text it was given", bare)
	}
}
