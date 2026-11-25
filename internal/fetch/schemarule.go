package fetch

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// The configuration schema (internal/config) checks each request type's
// collector for what Validate requires beyond its keys, using the type's
// SchemaRule. These are the building blocks of such rules, shared by the
// types' rules and the rest of the schema.

// SchemaWrittenKey is the rule that an object has the key and that it is
// not "". required alone cannot say this, since it only checks that the key
// is present, whatever its value. Every rule about a key that may be
// written "" — one that requires it, or that forbids it beside another — is
// built from it. So key: "" means the key is left out, to the rule as to
// the exporter.
func SchemaWrittenKey(key string) map[string]any {
	return map[string]any{"required": []string{key}, "properties": map[string]any{key: map[string]any{"not": map[string]any{"const": ""}}}}
}

// SchemaSurroundingBlank is a regular expression class of the characters
// strings.TrimSpace removes from a text, written so Go and JavaScript read
// it the same way. The two non-ASCII characters below U+0100 are written
// as escapes, and the rest as themselves.
const SchemaSurroundingBlank = `\t\n\v\f\r \x85\xA0` + "  -     　"

// SchemaCaseless is a regular expression matching text without regard to
// case, written so Go and JavaScript read it the same way. Each letter
// becomes a class of its two cases, and anything else is escaped as
// regexp.QuoteMeta escapes it.
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

// SchemaCaselessOneOf is the pattern of a text naming one of names, matched
// the way the exporter matches a name such as request.driver or
// request.record_type: in any case, with or without blanks around it. The
// spec matches a driver and a DNS record type this way, because their
// databases and RFCs name them in any case. So the schema accepts what the
// exporter accepts, rather than refusing what an enum would.
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
