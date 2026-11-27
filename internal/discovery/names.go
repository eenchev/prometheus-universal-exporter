package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// maxEscapedAddress is the longest EscapeAddress writes; a longer address,
// such as a long host name, is named by HashedAddress, so a name stays of a
// size fit for a label value and a log line.
const maxEscapedAddress = 128

// Name is the name of the target that the entry named entry makes
// of address. It is <entry>_ and the escaped address, or its HashedAddress
// when EscapeAddress does not take it. It depends on the two alone, never on
// the other targets, so a target is never renamed when another comes or
// goes. It is injective: two addresses of one entry never share a name. Since
// no entry's name is another's followed by _ (checked at load), the targets
// of two entries never share one either. EscapeAddress is injective on what
// it takes. A HashedAddress always holds _ followed by an upper-case letter,
// and an escaped address never does, so the two forms never meet. Two
// HashedAddresses meet only if two addresses share 64 bits of SHA-256.
func Name(entry, address string) string {
	if escaped, ok := EscapeAddress(address); ok {
		return entry + "_" + escaped
	}
	return entry + "_" + HashedAddress(address)
}

// EscapeAddress writes an address made of lower-case letters, digits, ., :,
// -, [ and ] in the characters of a name. It keeps an IPv4 or IPv6 address
// with its port, and a lower-case host name, readable. A letter or digit is
// written as itself, . as _, and :, -, [ and ] as _ and their byte in
// upper-case hex (_3A, _2D, _5B, _5D). So 10.0.0.5:9100 is 10_0_0_5_3A9100.
// It is injective. Reading left to right, _ followed by a digit and an
// upper-case letter is one of the four escapes, since an upper-case letter
// appears in nothing else. Any other _ is a dot. So the address is read back
// whole. Any other address, or one escaped longer than maxEscapedAddress, is
// not taken.
func EscapeAddress(address string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(address); i++ {
		switch c := address[i]; {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '.':
			b.WriteByte('_')
		case c == ':' || c == '-' || c == '[' || c == ']':
			_, _ = fmt.Fprintf(&b, "_%02X", c)
		default:
			return "", false
		}
		if b.Len() > maxEscapedAddress {
			return "", false
		}
	}
	return b.String(), true
}

// HashedAddressPrefix is how much of the address HashedAddress keeps.
const HashedAddressPrefix = 48

// HashedAddress names an address that EscapeAddress does not take. The name
// is the address's first HashedAddressPrefix bytes, lower-cased, with every
// byte but a letter or a digit written _, which only helps a reader. Then
// come _X and 16 hex digits of the address's SHA-256, which tell it apart.
func HashedAddress(address string) string {
	prefix := address[:min(len(address), HashedAddressPrefix)]
	readable := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '_'
	}, prefix)
	sum := sha256.Sum256([]byte(address))
	return readable + "_X" + hex.EncodeToString(sum[:8])
}
