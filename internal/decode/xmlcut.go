package decode

import (
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The XML parser (encoding/xml, and github.com/antchfx/xmlquery v1.5.1
// around it) puts names of the document into some of its errors in full, and
// a name is as long as the target writes it:
//
//   - a syntax error's `unexpected end element </name>`, `element <name>
//     closed by </name>`, `element <name> in space prefix closed by </name>
//     in space prefix`, `invalid characters between </name and >`, `invalid
//     character entity &name;`, with ` (no semicolon)` in place of the
//     semicolon when there is none, and `invalid XML name: name`
//     (encoding/xml, xml.go);
//   - `xml: unsupported version "1.1"; only version 1.0 is supported` and
//     `xml: opening charset "name": unsupported charset: "name"`, of the XML
//     declaration, quoted as Go quotes a text (encoding/xml, xml.go, and
//     golang.org/x/net/html/charset for what follows the second colon). The
//     encoding of a declaration within the first kilobyte is refused before
//     the parser sees it, by name (textencoding.go);
//   - `xmlquery: invalid XML document, namespace prefix is missing`
//     (xmlquery, parse.go).
//
// The parser's other errors are fixed texts. So those are read here, and
// only here, in the forms the libraries write them, and each name is cut as
// model.QuoteValue cuts a value, to its first 64 bytes and its length: what
// the error says after the name, and the second name, are kept. A name the
// libraries write bare has no blank and no angle bracket in it, the bytes a
// name ends at, so it ends where the words after it start; a quoted one ends
// with its quote. An error in none of the forms, and one whose names are all
// within 64 bytes, is left as it is, and is bounded with every other decode
// error (boundedFailure).

// xmlForm is one form of error that holds names of the document: the words
// of it, which are what the library writes before the first name, between
// two names and after the last, and whether the names are quoted as Go
// quotes a text.
type xmlForm struct {
	words  []string
	quoted bool
}

// xmlSyntaxErrors are the forms of what a syntax error says after its line
// (xml.SyntaxError.Msg). An entity without a semicolon stands before one
// with: the form of that takes all that follows its words.
var xmlSyntaxErrors = []xmlForm{
	{words: []string{"unexpected end element </", ">"}},
	{words: []string{"element <", "> closed by </", ">"}},
	{words: []string{"element <", "> in space ", " closed by </", "> in space ", ""}},
	{words: []string{"invalid characters between </", " and >"}},
	{words: []string{"invalid character entity ", " (no semicolon)"}},
	{words: []string{"invalid character entity ", ""}},
	{words: []string{"invalid XML name: ", ""}},
}

// xmlOtherErrors are the forms of the errors that are no syntax error, whole.
var xmlOtherErrors = []xmlForm{
	{words: []string{"xml: unsupported version ", "; only version 1.0 is supported"}, quoted: true},
	{words: []string{"xml: opening charset ", ": unsupported charset: ", ""}, quoted: true},
	{words: []string{"xmlquery: invalid XML document, namespace ", " is missing"}},
}

// xmlNamesCut is text, when it is of one of forms and holds a name longer
// than model.QuoteValue shows, with each such name cut to its start and its
// length; the same with the mark for each length, which is what the failure
// is recognised by; and whether that is so. Both are made anew, and hold
// nothing of text. The text is of the first form it reads as, and of no
// other.
func xmlNamesCut(forms []xmlForm, text string) (short, same string, cut bool) {
	for _, form := range forms {
		if short, same, cut, is := form.namesCut(text); is {
			return short, same, cut
		}
	}
	return "", "", false
}

// namesCut is xmlNamesCut for one form, and whether text is of the form: one
// that is not of it to its last byte is not.
func (f xmlForm) namesCut(text string) (short, same string, cut, is bool) {
	rest, ok := strings.CutPrefix(text, f.words[0])
	if !ok {
		return "", "", false, false
	}
	var shown, recognised strings.Builder
	shown.WriteString(f.words[0])
	recognised.WriteString(f.words[0])
	for _, word := range f.words[1:] {
		var name model.QuotedValue
		if f.quoted {
			quoted, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return "", "", false, false
			}
			written, err := strconv.Unquote(quoted)
			if err != nil {
				return "", "", false, false
			}
			name, rest = model.Quoted(written), rest[len(quoted):]
		} else {
			// The last name of a form that ends with it runs to the end.
			end := len(rest)
			if word != "" {
				end = strings.Index(rest, word)
			}
			if end < 0 {
				return "", "", false, false
			}
			name, rest = model.Bare(rest[:end]), rest[end:]
		}
		if rest, ok = strings.CutPrefix(rest, word); !ok {
			return "", "", false, false
		}
		cut = cut || name.String() != name.Same()
		shown.WriteString(name.String())
		shown.WriteString(word)
		recognised.WriteString(name.Same())
		recognised.WriteString(word)
	}
	if rest != "" {
		return "", "", false, false
	}
	if !cut {
		return "", "", false, true
	}
	return shown.String(), recognised.String(), true, true
}
