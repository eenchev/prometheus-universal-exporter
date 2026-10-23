//go:build request_type_ldap

package fetch

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A search's filter is written as RFC 4515 writes one, (&(objectClass=user)
// (cn=a*)), and sent as RFC 4511 encodes it, in BER. ldapFilter parses the
// text and writes the BER at once: and, or, not, equality, substrings, >=,
// <=, presence, approximate match and extensible match, such as Active
// Directory's bitwise AND, (userAccountControl:1.2.840.113556.1.4.803:=2).
// A placeholder may stand in a filter's values alone, filled with its value
// escaped as RFC 4515 escapes one (ldapEscapeFilterValue), so a probe can
// add no parenthesis, wildcard or other part to the filter, and in a base
// DN's attribute values alone, escaped as RFC 4514 escapes one
// (ldapEscapeDNValue), so it can add no RDN.

// ldapMaxFilterDepth is how deeply &, | and ! may nest in a filter.
const ldapMaxFilterDepth = 32

// ldapPlaceholderMark stands in for each placeholder of a filter or a base
// DN when it is checked at load: a character of Unicode's private use area,
// which may stand in a value and nowhere else, so a placeholder elsewhere
// is found where it stands.
const ldapPlaceholderMark = '\uE000'

// ldapAttributeDescription is what a filter's attribute is: a name or an
// OID, with options after ;.
var ldapAttributeDescription = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*|[0-9]+(\.[0-9]+)+)(;[A-Za-z0-9-]+)*$`)

// ldapAttributeName is what a requested attribute is written with
// (model.LDAPAttributePattern).
var ldapAttributeName = regexp.MustCompile(model.LDAPAttributePattern)

// errLDAPPlaceholderPlace is the refusal of a placeholder outside a value.
var errLDAPPlaceholderPlace = errors.New("a placeholder may stand only in a value, where its value is escaped, never in an attribute's name or in the filter's structure")

// ldapFilterParser reads a filter's text. filled says the text is a
// filter with its placeholders filled in, at a scrape, rather than the
// filter as written, at load (ldapSubstrings).
type ldapFilterParser struct {
	text   string
	pos    int
	filled bool
}

// parseLDAPFilter parses a filter as it is written, its placeholders marked
// (ldapMarked), and returns its BER.
func parseLDAPFilter(text string) ([]byte, error) {
	return (&ldapFilterParser{text: text}).parse()
}

// parseFilledLDAPFilter parses a filter whose placeholders a scrape filled
// in, and returns its BER.
func parseFilledLDAPFilter(text string) ([]byte, error) {
	return (&ldapFilterParser{text: text, filled: true}).parse()
}

// parse reads the whole of p's text as one filter.
func (p *ldapFilterParser) parse() ([]byte, error) {
	text := p.text
	if text == "" {
		return nil, errors.New("the filter is empty")
	}
	if !strings.HasPrefix(text, "(") {
		return nil, fmt.Errorf("the filter %q does not begin with (; write it in parentheses, as (%s)", text, text)
	}
	ber, err := p.filter(0)
	if err != nil {
		return nil, err
	}
	if p.pos != len(text) {
		return nil, fmt.Errorf("the filter %q goes on after its closing parenthesis, at %q; join filters with (&...) or (|...)", text, text[p.pos:])
	}
	return ber, nil
}

// filter reads ( filtercomp ) at the depth of the &, | and ! around it.
func (p *ldapFilterParser) filter(depth int) ([]byte, error) {
	if depth > ldapMaxFilterDepth {
		return nil, fmt.Errorf("the filter nests &, | and ! more than %d deep", ldapMaxFilterDepth)
	}
	if p.pos >= len(p.text) || p.text[p.pos] != '(' {
		return nil, fmt.Errorf("the filter %q wants ( at offset %d", p.text, p.pos)
	}
	p.pos++
	if p.pos >= len(p.text) {
		return nil, fmt.Errorf("the filter %q ends before its closing parenthesis", p.text)
	}
	var ber []byte
	var err error
	switch p.text[p.pos] {
	case '&', '|':
		tag := byte(0xa0)
		if p.text[p.pos] == '|' {
			tag = 0xa1
		}
		p.pos++
		var parts [][]byte
		for p.pos < len(p.text) && p.text[p.pos] == '(' {
			part, err := p.filter(depth + 1)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("the filter %q has a %c with no filter in it", p.text, p.text[p.pos-1])
		}
		ber = berElement(tag, parts...)
	case '!':
		p.pos++
		part, err := p.filter(depth + 1)
		if err != nil {
			return nil, err
		}
		ber = berElement(0xa2, part)
	default:
		if ber, err = p.item(); err != nil {
			return nil, err
		}
	}
	if p.pos >= len(p.text) || p.text[p.pos] != ')' {
		return nil, fmt.Errorf("the filter %q wants ) at offset %d", p.text, p.pos)
	}
	p.pos++
	return ber, nil
}

// item reads an attribute, its operator and its value, up to the ) that
// closes it.
func (p *ldapFilterParser) item() ([]byte, error) {
	end := strings.IndexByte(p.text[p.pos:], ')')
	if end < 0 {
		return nil, fmt.Errorf("the filter %q ends before its closing parenthesis", p.text)
	}
	item := p.text[p.pos : p.pos+end]
	if strings.ContainsRune(item, '(') {
		return nil, fmt.Errorf("the filter %q has ( inside %q; a value writes it as \\28", p.text, item)
	}
	p.pos += end
	eq := strings.IndexByte(item, '=')
	if eq < 0 {
		return nil, fmt.Errorf("the filter's item %q has no =", item)
	}
	attribute, value := item[:eq], item[eq+1:]
	tag := byte(0xa3)
	switch {
	case strings.HasSuffix(attribute, "~"):
		tag, attribute = 0xa8, attribute[:len(attribute)-1]
	case strings.HasSuffix(attribute, ">"):
		tag, attribute = 0xa5, attribute[:len(attribute)-1]
	case strings.HasSuffix(attribute, "<"):
		tag, attribute = 0xa6, attribute[:len(attribute)-1]
	}
	if strings.ContainsRune(attribute, ldapPlaceholderMark) {
		return nil, fmt.Errorf("the filter's item %q: %w", item, errLDAPPlaceholderPlace)
	}
	if tag == 0xa3 && strings.HasSuffix(attribute, ":") {
		return ldapExtensible(item, attribute[:len(attribute)-1], value)
	}
	if !ldapAttributeDescription.MatchString(attribute) {
		return nil, fmt.Errorf("the filter's item %q names the attribute %q, which is not an attribute's name or OID", item, attribute)
	}
	if tag != 0xa3 {
		if strings.ContainsRune(value, '*') {
			return nil, fmt.Errorf("the filter's item %q has * in a value of ~=, >= or <=, where it is no wildcard; write it as \\2a", item)
		}
		decoded, err := ldapUnescapeFilterValue(value, item)
		if err != nil {
			return nil, err
		}
		return berElement(tag, berString(berOctetString, attribute), berString(berOctetString, decoded)), nil
	}
	if value == "*" {
		return berString(0x87, attribute), nil
	}
	if !strings.ContainsRune(value, '*') {
		decoded, err := ldapUnescapeFilterValue(value, item)
		if err != nil {
			return nil, err
		}
		return berElement(0xa3, berString(berOctetString, attribute), berString(berOctetString, decoded)), nil
	}
	parts := strings.Split(value, "*")
	var substrings [][]byte
	for i, part := range parts {
		if part == "" {
			continue
		}
		decoded, err := ldapUnescapeFilterValue(part, item)
		if err != nil {
			return nil, err
		}
		tag := byte(0x81) // any
		switch i {
		case 0:
			tag = 0x80 // initial
		case len(parts) - 1:
			tag = 0x82 // final
		}
		substrings = append(substrings, berString(tag, decoded))
	}
	if len(substrings) == 0 {
		// RFC 4511 gives a SubstringFilter at least one substring, and a
		// server refuses one with none, so (cn=**) would fail every scrape.
		// As written it is refused at load. Made so by placeholders filled
		// with empty values, (cn=*{{param_x:}}*), it asks for a value
		// holding the empty string, which every value does: it is sent as
		// the presence filter, as (cn={{param_x:}}*) is, which fills in
		// to (cn=*).
		if !p.filled {
			return nil, fmt.Errorf("the filter's item %q is a substring match with no substring between its wildcards, which RFC 4511 does not allow; write (%s=*) for the entries that have the attribute", item, attribute)
		}
		return berString(0x87, attribute), nil
	}
	return berElement(0xa4, berString(berOctetString, attribute), berElement(berSequence, substrings...)), nil
}

// ldapRuleName is a matching rule: a name or an OID.
var ldapRuleName = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*|[0-9]+(\.[0-9]+)+)$`)

// ldapExtensible writes an extensible match, attr[:dn][:rule]:=value or
// [:dn]:rule:=value (RFC 4515 § 3), left being what stands before :=.
func ldapExtensible(item, left, value string) ([]byte, error) {
	parts := strings.Split(left, ":")
	attribute, rest := parts[0], parts[1:]
	dn := false
	if len(rest) > 0 && strings.EqualFold(rest[0], "dn") {
		dn, rest = true, rest[1:]
	}
	rule := ""
	if len(rest) > 0 {
		rule, rest = rest[0], rest[1:]
	}
	switch {
	case len(rest) > 0:
		return nil, fmt.Errorf("the filter's item %q is an extensible match of more parts than attr:dn:rule:=value", item)
	case attribute == "" && rule == "":
		return nil, fmt.Errorf("the filter's item %q is an extensible match with neither an attribute nor a matching rule", item)
	case attribute != "" && !ldapAttributeDescription.MatchString(attribute):
		return nil, fmt.Errorf("the filter's item %q names the attribute %q, which is not an attribute's name or OID", item, attribute)
	case rule != "" && !ldapRuleName.MatchString(rule):
		return nil, fmt.Errorf("the filter's item %q names the matching rule %q, which is not a rule's name or OID", item, rule)
	case strings.ContainsRune(value, '*'):
		return nil, fmt.Errorf("the filter's item %q has * in the value of an extensible match, where it is no wildcard; write it as \\2a", item)
	}
	decoded, err := ldapUnescapeFilterValue(value, item)
	if err != nil {
		return nil, err
	}
	var fields [][]byte
	if rule != "" {
		fields = append(fields, berString(0x81, rule))
	}
	if attribute != "" {
		fields = append(fields, berString(0x82, attribute))
	}
	fields = append(fields, berString(0x83, decoded))
	if dn {
		fields = append(fields, berElement(0x84, []byte{0xff}))
	}
	return berElement(0xa9, fields...), nil
}

// ldapUnescapeFilterValue reads a value of a filter: \ and two hex digits
// stand for a byte, and nothing else may follow \.
func ldapUnescapeFilterValue(value, item string) (string, error) {
	if !strings.ContainsRune(value, '\\') {
		return value, nil
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			b.WriteByte(value[i])
			continue
		}
		if i+2 >= len(value) {
			return "", fmt.Errorf("the filter's item %q ends in an escape; \\ is followed by two hex digits, as \\2a for *", item)
		}
		decoded, err := hex.DecodeString(value[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("the filter's item %q has the escape \\%s; \\ is followed by two hex digits, as \\2a for * and \\5c for \\", item, value[i+1:i+3])
		}
		b.Write(decoded)
		i += 2
	}
	return b.String(), nil
}

// ldapEscapeFilterValue writes a placeholder's value into a filter as RFC
// 4515 escapes a value: *, (, ), \ and NUL as \ and two hex digits, and so
// every byte that is not UTF-8, so nothing in it is read as the filter's.
func ldapEscapeFilterValue(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		switch {
		case r == utf8.RuneError && size == 1, r == '*', r == '(', r == ')', r == '\\', r == 0:
			fmt.Fprintf(&b, "\\%02x", value[i])
		default:
			b.WriteString(value[i : i+size])
		}
		i += size
	}
	return b.String()
}

// ldapEscapeDNValue writes a placeholder's value into a DN's attribute value
// as RFC 4514 escapes one: " + , ; < > \ = with \ before them, NUL as \00,
// and a # or blank at its start and a blank at its end with \ too, so
// nothing in it ends the value or the RDN. A value that is not UTF-8 is
// refused, a DN being text.
func ldapEscapeDNValue(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("the value is not UTF-8 text, which a DN is")
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == 0:
			b.WriteString(`\00`)
		case strings.IndexByte(`"+,;<>\=`, c) >= 0, i == 0 && (c == '#' || c == ' '), i == len(value)-1 && c == ' ':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// ldapAttributeType is an attribute type of a DN: a name or an OID.
var ldapAttributeType = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*|[0-9]+(\.[0-9]+)+)$`)

// checkLDAPDN checks a DN as RFC 4514 writes one: RDNs joined by commas,
// each one or more type=value joined by +, a value's special characters
// escaped with \, a blank allowed around a type. "" is the root DSE's DN.
func checkLDAPDN(dn string) error {
	if dn == "" {
		return nil
	}
	if !utf8.ValidString(dn) {
		return errors.New("it is not UTF-8 text")
	}
	start := 0
	for i := 0; i <= len(dn); i++ {
		if i < len(dn) && dn[i] == '\\' {
			if i+1 >= len(dn) {
				return errors.New("it ends in \\, which escapes the character after it")
			}
			if strings.IndexByte(` "#+,;<=>\`, dn[i+1]) < 0 {
				if i+2 >= len(dn) || !isHexDigit(dn[i+1]) || !isHexDigit(dn[i+2]) {
					return fmt.Errorf("it has the escape \\%c; \\ is followed by one of the characters  \"#+,;<=>\\ or two hex digits", dn[i+1])
				}
				i++
			}
			i++
			continue
		}
		if i < len(dn) && dn[i] != ',' && dn[i] != '+' {
			if strings.IndexByte(`";<>`, dn[i]) >= 0 || dn[i] == 0 {
				return fmt.Errorf("it has %q unescaped; a value writes it as \\%c", dn[i], dn[i])
			}
			continue
		}
		ava := dn[start:i]
		typ, _, ok := strings.Cut(ava, "=")
		typ = strings.TrimSpace(typ)
		switch {
		case strings.ContainsRune(typ, ldapPlaceholderMark):
			return errLDAPPlaceholderPlace
		case !ok:
			return fmt.Errorf("its part %q is not type=value", ava)
		case !ldapAttributeType.MatchString(typ):
			return fmt.Errorf("its part %q has the type %q, which is not an attribute's name or OID", ava, typ)
		}
		start = i + 1
	}
	return nil
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// ldapMarked is text with each of its placeholders replaced with
// ldapPlaceholderMark, for a check of where they stand.
func ldapMarked(f templateField) (string, error) {
	placeholders, err := f.parse()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	previous := 0
	for _, p := range placeholders {
		b.WriteString(f.text[previous:p.start])
		b.WriteRune(ldapPlaceholderMark)
		previous = p.end
	}
	b.WriteString(f.text[previous:])
	return b.String(), nil
}
