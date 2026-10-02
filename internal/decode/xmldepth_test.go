package decode

import (
	"strings"
	"testing"
)

// An XML document may nest its elements MaxXMLDepth deep, as an HTML one
// may, and no deeper: one more is refused in words that give the bound,
// whatever its size. Text and comments inside the deepest element are not
// another level.
func TestXMLNestedTooDeepIsRefused(t *testing.T) {
	nested := func(depth int, content string) string {
		return `<?xml version="1.0"?>` + strings.Repeat("<a>", depth) + content + strings.Repeat("</a>", depth)
	}
	d, err := decodeAs(t, "xml", "", nested(MaxXMLDepth, "<!-- deepest -->5<![CDATA[ ]]>"))
	if err != nil || d.Kind != "xml" {
		t.Fatalf("%d levels: %v", MaxXMLDepth, err)
	}
	const want = "XML decode: the document nests elements more than 512 deep, which is the most the exporter reads"
	for _, depth := range []int{MaxXMLDepth + 1, 100000} {
		if _, err := decodeAs(t, "xml", "", nested(depth, "5")); err == nil || err.Error() != want {
			t.Fatalf("%d levels: %v, want %s", depth, err, want)
		}
	}
	// The bound is on nesting, not on how many elements there are.
	wide := "<r>" + strings.Repeat("<a><b>1</b></a>", 5000) + "</r>"
	if _, err := decodeAs(t, "xml", "", wide); err != nil {
		t.Fatalf("a wide document: %v", err)
	}
	// The HTML parser has the same bound, of its own.
	if _, err := decodeAs(t, "html", "", "<html><body>"+strings.Repeat("<div>", MaxXMLDepth+1)+"1"); err == nil || !strings.Contains(err.Error(), "512") {
		t.Fatalf("HTML nested as deep: %v", err)
	}
}
