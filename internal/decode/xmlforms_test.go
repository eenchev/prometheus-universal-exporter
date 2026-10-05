package decode

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// xmlNamesCut cuts a text only when it is of a form to its last byte and
// holds a name over 64 bytes: a text that ends otherwise, that starts
// otherwise, whose quoted name is not closed, and one with short names are
// left as they are, for the bound every decode error has.
func TestAnXMLErrorOfNoKnownFormIsLeftAsItIs(t *testing.T) {
	long := strings.Repeat("a", 100)
	for _, text := range []string{
		"", "unexpected EOF", "expected element name after <", "element <a> closed by </b>", "invalid XML name: abc",
		"element <" + long + "> closed by </b> and more", "element <" + long + "> closed by <b>", "an element <" + long + "> closed by </b>",
		"element <" + long + "> in space x closed by </" + long + ">", "unexpected end element </" + long, "invalid characters between </" + long + " and",
		"attribute name without = in element", long,
		// The words of an entity without a semicolon are no part of its name.
		"invalid character entity &" + long[:60] + " (no semicolon)", "invalid character entity &" + long[:62] + ";",
	} {
		if short, same, cut := xmlNamesCut(xmlSyntaxErrors, text); cut {
			t.Errorf("the syntax error %q is cut to %q, recognised by %q", text, short, same)
		}
	}
	for _, text := range []string{
		`xml: unsupported version "1.1"; only version 1.0 is supported`, `xml: unsupported version "` + long + `; only version 1.0 is supported`,
		`xml: unsupported version "` + long + `"; only version 1.0 is supported, nor is this`, `xml: unsupported version ` + long + `; only version 1.0 is supported`,
		`xml: opening charset "` + long + `": no such charset`, `xml: encoding "` + long + `" declared but Decoder.CharsetReader is nil`,
		"xmlquery: invalid XML document, namespace abc is missing", "xmlquery: invalid XML document, namespace " + long + " is not there", "xmlquery: invalid XML document",
	} {
		if short, same, cut := xmlNamesCut(xmlOtherErrors, text); cut {
			t.Errorf("the error %q is cut to %q, recognised by %q", text, short, same)
		}
	}
	// A quoted name is read by its quotes, with what it escapes.
	name := strings.Repeat(`a"; only version 1.0 is supported`, 4)
	short, same, cut := xmlNamesCut(xmlOtherErrors, "xml: unsupported version "+model.QuoteValue(name)[:0]+`"`+strings.ReplaceAll(name, `"`, `\"`)+`"; only version 1.0 is supported`)
	if want := "xml: unsupported version " + model.QuoteValue(name) + "; only version 1.0 is supported"; !cut || short != want || same != lengthsMarked(want) {
		t.Errorf("a version that holds the words after it is cut to %q, recognised by %q, want %q", short, same, want)
	}
}
