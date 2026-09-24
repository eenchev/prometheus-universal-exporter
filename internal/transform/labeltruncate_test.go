package transform

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateLabelValue(t *testing.T) {
	tests := []struct {
		value string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"abcdefghijk", 10, "abcdefg…"},    // 7 bytes and the 3-byte mark
		{"ünïcödé-ünïcödé", 12, "ünïcöd…"}, // never splits a character
		{"abcdef", 2, "ab"},                // no room for the mark
		{"日本語のテキスト", 10, "日本…"},            // 3-byte characters
	}
	for _, test := range tests {
		got := truncateLabelValue(test.value, test.limit)
		if got != test.want || len(got) > test.limit || !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q (%d bytes), want %q", test.value, test.limit, got, len(got), test.want)
		}
	}
}
