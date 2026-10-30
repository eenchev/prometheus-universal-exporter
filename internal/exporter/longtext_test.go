package exporter

import (
	"strings"
	"testing"
)

// A text is long past longPreviewRunes characters or longPreviewLines
// lines, and its preview is its start, cut at whole characters, with an
// ellipsis; a shorter text is not cut.
func TestALongTextIsPreviewedByItsStart(t *testing.T) {
	short := strings.Repeat("é", longPreviewRunes)
	if got := clipLong("error", short); got.Long || got.Preview != "" || got.Text != short {
		t.Errorf("a text of %d characters is cut: %+v", longPreviewRunes, got)
	}
	if got := clipLong("error", "one\ntwo\nthree"); got.Long {
		t.Errorf("a text of three lines is cut: %+v", got)
	}
	long := strings.Repeat("é", longPreviewRunes+1)
	if got := clipLong("error", long); !got.Long || got.Preview != strings.Repeat("é", longPreviewRunes)+"…" || got.Text != long || got.Kind != "error" {
		t.Errorf("a text of %d characters is %+v", longPreviewRunes+1, got)
	}
	if got := clipLong("target", "one\ntwo\nthree\nfour"); !got.Long || got.Preview != "one\ntwo\nthree…" {
		t.Errorf("a text of four lines is %+v", got)
	}
}
