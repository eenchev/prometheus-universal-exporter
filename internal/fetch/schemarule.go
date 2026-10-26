package fetch

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// The configuration schema (internal/config) holds a collector of each
// request type to what Validate holds it to beyond its keys, by the type's
// SchemaRule. These are the pieces such a rule is made of, shared by the
// rules of the types and the rest of the schema.

// SchemaWrittenKey is the rule that an object has the key and that it is
// not "": what required alone cannot say, which goes by the key being
// there, whatever its value. Every rule about a key that may be written ""
// — one that requires it, or that forbids it beside another — is made of
// it, so that key: "" is the key left out to the rule as it is to the
// exporter.
func SchemaWrittenKey(key string) map[string]any {
	return map[string]any{"required": []string{key}, "properties": map[string]any{key: map[string]any{"not": map[string]any{"const": ""}}}}
}

// SchemaSurroundingBlank is a regular expression class of the characters
// strings.TrimSpace takes off a text, written so that Go and JavaScript read
// it alike: the two below U+0100 that are no ASCII as escapes, the rest as
// themselves.
const SchemaSurroundingBlank = `\t\n\v\f\r \x85\xA0` + "  -     　"

// SchemaCaseless is a regular expression of text matched without regard to
// case, written so that Go and JavaScript read it alike: each letter as a
// class of its two cases, anything else escaped as regexp.QuoteMeta escapes
// it.
func SchemaCaseless(text string) string {
	var b strings.Builder
	for _, r := range text {
		if lower, upper := unicode.ToLower(r), unicode.ToUpper(r); lower != upper {
			b.WriteByte('[')
			b.WriteRune(upper)
			b.WriteRune(lower)
			b.WriteByte(']')
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(r)))
	}
	return b.String()
}

// SchemaCaselessOneOf is the pattern of a text naming one of names as the
// exporter matches a name such as request.driver or request.record_type: in
// any case, with blanks around it or not. The spec has a driver matched so,
// as it has request.type, so the schema takes what the exporter takes
// rather than the exporter refusing what an enum would.
func SchemaCaselessOneOf(names ...string) string {
	alternatives := make([]string, len(names))
	for i, name := range names {
		alternatives[i] = SchemaCaseless(name)
	}
	blanks := "[" + SchemaSurroundingBlank + "]*"
	return "^" + blanks + "(" + strings.Join(alternatives, "|") + ")" + blanks + "$"
}

// SchemaOptionalEnum is values with "", which is the key left out, beside
// them: the allowed values of a key that may be left out.
func SchemaOptionalEnum(values []string) []string {
	return append(slices.Clone(values), "")
}

// SchemaOptionalPattern is pattern taking "" too, which is the key left
// out.
func SchemaOptionalPattern(pattern string) string {
	return "^$|" + pattern
}

// SchemaOnlyBlanks is text of one blank or more and nothing else: what a
// key that names something may not be. It goes under not, so that the key
// keeps the types it takes, and "" stays the key left out.
func SchemaOnlyBlanks() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[" + SchemaSurroundingBlank + "]+$"}
}
