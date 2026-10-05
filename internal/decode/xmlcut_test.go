package decode

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// xmlAsItWas is the error an XML document was refused with before a name in
// it was cut, and what it was recognised by: the library's error, and for a
// syntax error the same without its line.
func xmlAsItWas(body string) (text, same string) {
	_, err := xmlquery.Parse(strings.NewReader(body))
	if err == nil {
		return "", ""
	}
	text, same = err.Error(), err.Error()
	var syntax *xml.SyntaxError
	if errors.As(err, &syntax) {
		same = strings.Replace(text, syntax.Error(), "XML syntax error: "+syntax.Msg, 1)
	}
	return text, same
}

// An XML error that names an element, a name space, an entity, a version or
// an encoding of the document names it by its first 64 bytes and its length,
// in the words of the library and with what the library says after it: with
// a name of 1 MiB the error is under 300 bytes where it was the name whole,
// bounded then at 2,000 bytes without its end, and is the decoder's own, not
// one cut at the bound. It is recognised with the mark for each length and
// without the line, so a name half as long, a line further, is the same
// failure and a name that starts otherwise another. The error is a new one,
// and no longer the library's, which holds the name. A name of 64 bytes or
// fewer reads, and is recognised, as the library's error was.
func TestAnXMLErrorNamesTheStartOfANameAndKeepsWhatFollowsIt(t *testing.T) {
	long := strings.Repeat("a", longTokenBytes())
	const syntax, decode = "XML decode: XML syntax error on line L: ", "XML decode: "
	type show = func(string) string
	for name, tc := range map[string]struct {
		body func(token string) string
		want func(q, b show, token string) string
	}{
		"an end without a start": {func(t string) string { return "</" + t + ">" },
			func(_, b show, t string) string { return syntax + "unexpected end element </" + b(t) + ">" }},
		"an element closed by a long name": {func(t string) string { return "<a></" + t + ">" },
			func(_, b show, t string) string { return syntax + "element <a> closed by </" + b(t) + ">" }},
		"a long element closed by another": {func(t string) string { return "<r><" + t + "></b></r>" },
			func(_, b show, t string) string { return syntax + "element <" + b(t) + "> closed by </b>" }},
		"two long names": {func(t string) string { return "<" + t + "></b" + t + ">" },
			func(_, b show, t string) string {
				return syntax + "element <" + b(t) + "> closed by </" + b("b"+t) + ">"
			}},
		"an element closed in another name space": {func(t string) string { return "<x:" + t + " xmlns:x='u'></y:" + t + ">" },
			func(_, b show, t string) string {
				return syntax + "element <" + b(t) + "> in space x closed by </" + b(t) + "> in space y"
			}},
		"an element closed in another long name space": {func(t string) string {
			return "<" + t + ":e xmlns:" + t + "='u' xmlns:y" + t + "='v'></y" + t + ":e>"
		},
			func(_, b show, t string) string {
				return syntax + "element <e> in space " + b(t) + " closed by </e> in space " + b("y"+t)
			}},
		"an end that goes on": {func(t string) string { return "<a></" + t + " x>" },
			func(_, b show, t string) string { return syntax + "invalid characters between </" + b(t) + " and >" }},
		"an entity": {func(t string) string { return "<a>&" + t + ";</a>" },
			func(_, b show, t string) string { return syntax + "invalid character entity " + b("&"+t+";") }},
		"an entity without a semicolon": {func(t string) string { return "<a>&" + t + " </a>" },
			func(_, b show, t string) string {
				return syntax + "invalid character entity " + b("&"+t) + " (no semicolon)"
			}},
		"a name that is no name": {func(t string) string { return "<" + t + "\xff\xfe/>" },
			func(_, b show, t string) string { return syntax + "invalid XML name: " + b(t+"\xff\xfe") }},
		"a version": {func(t string) string { return "<?xml version='" + t + "'?><a/>" },
			func(q, _ show, t string) string {
				return decode + "xml: unsupported version " + q(t) + "; only version 1.0 is supported"
			}},
		// Past the kilobyte the declaration's encoding is looked for in; a
		// shorter name is refused before the parser reads the document.
		"an encoding": {func(t string) string {
			return "<?xml version='1.0' encoding='" + t + strings.Repeat("e", 1000) + "'?><a/>"
		},
			func(q, _ show, t string) string {
				name := q(t + strings.Repeat("e", 1000))
				return decode + "xml: opening charset " + name + ": unsupported charset: " + name
			}},
		"a name space that is not declared": {func(t string) string { return "<" + t + ":a/>" },
			func(_, b show, t string) string {
				return decode + "xmlquery: invalid XML document, namespace " + b(t) + " is missing"
			}},
	} {
		quote, bare := model.QuoteValue, func(value string) string {
			if len(value) <= 64 {
				return value
			}
			return bareCut(value)
		}
		on := func(text, line string) string { return strings.Replace(text, " on line L", line, 1) }
		err, made := decodeError(t, "xml", tc.body(long), nil)
		text, want := err.Error(), on(tc.want(quote, bare, long), " on line 1")
		if text != want || made.Error() != want || len(text) > 300 {
			t.Errorf("%s: a name of 1 MiB is refused in %d bytes, %.400q, want %q", name, len(text), text, want)
			continue
		}
		same := model.SameFailureText(err)
		if want := lengthsMarked(on(tc.want(quote, bare, long), "")); same != want {
			t.Errorf("%s: the failure is recognised by %q, want %q", name, same, want)
		}
		var library *xml.SyntaxError
		if errors.As(made, &library) {
			t.Errorf("%s: the error still is the library's, which holds the name: %.200v", name, made)
		}
		half, _ := decodeError(t, "xml", "\n"+tc.body(long[:len(long)/2]), nil)
		if want := on(tc.want(quote, bare, long[:len(long)/2]), " on line 2"); half.Error() != want || model.SameFailureText(half) != same {
			t.Errorf("%s: a name half as long, a line further, is refused with %.400q, recognised by %q, want %q and the same failure", name, half, model.SameFailureText(half), want)
		}
		if other, _ := decodeError(t, "xml", tc.body("b"+long[1:]), nil); model.SameFailureText(other) == same {
			t.Errorf("%s: a name that starts otherwise is the same failure: %q", name, same)
		}
		for _, token := range []string{long[:64], long[:63], "abc"} {
			body := tc.body(token)
			if name == "an encoding" {
				// The name is past 64 bytes whatever the token.
				continue
			}
			short, made := decodeError(t, "xml", body, nil)
			was, recognised := xmlAsItWas(body)
			if want := tc.want(quote, bare, token); len(want) < 300 && short.Error() != on(want, " on line 1") {
				t.Errorf("%s: a name of %d bytes is refused with %q, want %q", name, len(token), short, on(want, " on line 1"))
			}
			if short.Error() != made.Error() || !isCutOf(short.Error(), "XML decode: "+was) || model.SameFailureText(short) != lengthsMarked(strings.Replace(short.Error(), " on line 1", "", 1)) {
				t.Errorf("%s: a name of %d bytes is refused with %q, recognised by %q; the library's error is %q", name, len(token), short, model.SameFailureText(short), was)
			}
			if !strings.Contains(short.Error(), " bytes)") && (short.Error() != "XML decode: "+was || model.SameFailureText(short) != "XML decode: "+recognised) {
				t.Errorf("%s: a name of %d bytes is refused with %q, recognised by %q; it was %q, recognised by %q", name, len(token), short, model.SameFailureText(short), was, recognised)
			}
		}
	}
}

// A document the XML decoder refuses with no name over 64 bytes is refused
// with the library's error to the letter and recognised by what it was: the
// repository's XML files cut off after every few bytes, and documents with
// each mistake the parser names.
func TestAnXMLErrorWithoutALongNameIsTheLibrarysOwn(t *testing.T) {
	documents := []string{
		"", " ", "<", "<a", "<a>", "<a><b></a>", "<a>\n\n<b></a>", "<a><b></c>", "</a>", "<a></a x>", "<a>&nope;</a>", "<a>&nope</a>", "<a>&#;</a>", "<a>&#xZZ;</a>", "<a>&</a>",
		"<a b></a>", "<a b=></a>", "<a b=c></a>", "<a b='<'></a>", "<a><!-x></a>", "<a><![x></a>", "<a><![CDATA[x</a>", "<a>]]></a>", "<a>\x00</a>", "<a>\xff</a>", "<1/>", "<a\xff/>", "<a:b:c/>",
		"<x:a/>", "<x:a xmlns:x='u'></y:a>", "<x:a xmlns:x='u' xmlns:y='v'></y:a>", "<a xmlns='u'></x:a>", "<?xml version='1.1'?><a/>", "<?xml version='1.0' encoding='nope'?><a/>", "<??>", "<? ?>", "<?a", "<a/><b/>", "<a/>x", "<!DOCTYPE a [<!ENTITY", "<a></b>\n</a>",
	}
	files, err := filepath.Glob("../../testdata/xml/*.xml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no XML files: %v", err)
	}
	for _, file := range files {
		whole, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for cut := 1; cut < len(whole); cut += 7 {
			documents = append(documents, string(whole[:cut]), string(whole[:cut])+"</nope>")
		}
	}
	refused := 0
	for _, document := range documents {
		was, recognised := xmlAsItWas(document)
		_, err := ParseXML([]byte(document))
		if (err == nil) != (was == "") {
			t.Fatalf("%.100q is refused with %v, was with %q", document, err, was)
		}
		if err == nil {
			continue
		}
		refused++
		if err.Error() != was || model.SameFailureText(err) != recognised || strings.Contains(was, " bytes)") {
			t.Errorf("%.100q is refused with %q, recognised by %q; it was %q, recognised by %q", document, err, model.SameFailureText(err), was, recognised)
		}
		var syntax *xml.SyntaxError
		if _, plain := xmlquery.Parse(bytes.NewReader([]byte(document))); errors.As(plain, &syntax) != errors.As(err, &syntax) {
			t.Errorf("%.100q: the error %q is a syntax error to errors.As where the library's is not, or the reverse", document, err)
		}
	}
	if refused < 500 {
		t.Errorf("%d documents are refused: the table should hold many", refused)
	}
	t.Logf("%d documents, %d refused", len(documents), refused)
}
