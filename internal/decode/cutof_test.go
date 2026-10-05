package decode

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What the differential tests of the decoders' errors share: an error that
// shows a value of the body shows no more than its first 64 bytes, so where
// the decoder as it was wrote the value whole, the two errors are compared
// by isCutOf, and the recognised text is the error's with the mark for each
// length (lengthsMarked).

// cutLength is the length an error gives of a value it cut.
var cutLength = regexp.MustCompile(`\.\.\. \((\d+) bytes\)`)

// bareCut is a value longer than 64 bytes as an error shows it where it
// writes it without quotes: its first 64 bytes, to a character boundary, and
// its length, which is how model.QuoteValue cuts a value within its quotes.
func bareCut(value string) string {
	cut := 64
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes)", value[:cut], len(value))
}

// isCutOf reports whether cut is whole with some of the values whole holds
// cut as an error cuts one: a quoted value of more than 64 bytes as
// model.QuoteValue cuts it, and a bare one as bareCut does. Texts that are
// equal are, and so are two that differ in nothing else: where cut gives a
// length, whole has there the same words, or a quoted text or a run of bytes
// of that length which is cut to what cut has.
func isCutOf(cut, whole string) bool {
	at := cutLength.FindStringSubmatchIndex(cut)
	if at == nil {
		return cut == whole
	}
	length, err := strconv.Atoi(cut[at[2]:at[3]])
	if err != nil {
		return false
	}
	before, shown, after := cut[:at[0]], cut[:at[1]], cut[at[1]:]
	// The length may be one whole has too, of a value cut in both.
	if rest, ok := strings.CutPrefix(whole, shown); ok && isCutOf(after, rest) {
		return true
	}
	// Or the value starts somewhere in what stands before the length.
	for from := len(before); from >= 0; from-- {
		if !strings.HasPrefix(whole, before[:from]) {
			continue
		}
		rest := whole[from:]
		if quoted, err := strconv.QuotedPrefix(rest); err == nil {
			if value, err := strconv.Unquote(quoted); err == nil && len(value) == length && model.QuoteValue(value) == shown[from:] && isCutOf(after, rest[len(quoted):]) {
				return true
			}
		}
		if length > 64 && length <= len(rest) && bareCut(rest[:length]) == shown[from:] && isCutOf(after, rest[length:]) {
			return true
		}
	}
	return false
}

// lengthsMarked is text with the mark for a size in place of each length it
// gives of a value that was cut.
func lengthsMarked(text string) string {
	return cutLength.ReplaceAllString(text, "... ("+model.MovingMark+" bytes)")
}

// isCutOf tells an error that cuts the values of another from one that
// differs in anything else: in a word, in the start that is shown, in the
// length, in a value cut that is not over 64 bytes, in what follows.
func TestIsCutOfTakesOnlyAValueCutToItsStartAndItsLength(t *testing.T) {
	long, wide := strings.Repeat("a", 100), strings.Repeat("é", 50)
	for _, tc := range []struct {
		cut, whole string
		want       bool
	}{
		{"x", "x", true}, {"x", "y", false}, {"", "", true},
		{"got " + model.QuoteValue(long), fmt.Sprintf("got %q", long), true},
		{fmt.Sprintf("got %s end", model.QuoteValue(long)), fmt.Sprintf("got %q end", long), true},
		{fmt.Sprintf("got %s end", model.QuoteValue(long)), fmt.Sprintf("got %q END", long), false},
		{"got " + model.QuoteValue(long), fmt.Sprintf("got %q", long+"a"), false},
		{"got " + model.QuoteValue(long), fmt.Sprintf("got %q", "b"+long[1:]), false},
		{"got " + model.QuoteValue(wide), fmt.Sprintf("got %q", wide), true},
		{"got " + model.QuoteValue("a\"b\\"+long), fmt.Sprintf("got %q", "a\"b\\"+long), true},
		{fmt.Sprintf("got %s and %s", model.QuoteValue(long), model.QuoteValue(wide)), fmt.Sprintf("got %q and %q", long, wide), true},
		{fmt.Sprintf("got %s and %q", model.QuoteValue(long), "b"), fmt.Sprintf("got %q and %q", long, "b"), true},
		{fmt.Sprintf("the %s_sum of %s", bareCut(long), bareCut(wide)), fmt.Sprintf("the %s_sum of %s", long, wide), true},
		{fmt.Sprintf("the %s_sum", bareCut(long)), fmt.Sprintf("the %s_sum", long+"a"), false},
		{fmt.Sprintf("the %s_sum", bareCut(long)), fmt.Sprintf("the %s_count", long), false},
		// A length both have, of a value cut in both.
		{fmt.Sprintf("the %s{a=%s}", bareCut(long), model.QuoteValue(wide)), fmt.Sprintf("the %s{a=%s}", long, model.QuoteValue(wide)), true},
		// No more than 64 bytes are not cut.
		{`got "aaaa"... (10 bytes)`, `got "aaaaaaaaaa"`, false},
		{`got aaaa... (10 bytes)`, `got aaaaaaaaaa`, false},
	} {
		if got := isCutOf(tc.cut, tc.whole); got != tc.want {
			t.Errorf("isCutOf(%q, %q) = %v, want %v", tc.cut, tc.whole, got, tc.want)
		}
	}
	if got := lengthsMarked(`a "x"... (100 bytes) and y... (7 bytes) (3 bytes)`); got != `a "x"... (# bytes) and y... (# bytes) (3 bytes)` {
		t.Errorf("the lengths are marked as %q", got)
	}
}
