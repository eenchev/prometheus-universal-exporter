//go:build request_type_tcp || request_type_sshexec

package fetch

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// lineValue is the characters a placeholder's value in a tcp collector's
// send, or in an sshexec collector's command, may hold. A tcp value lands
// in a line of a text protocol, and those protocols split it on more than
// the line break: HAProxy's runtime API runs each ';'-separated command of
// a line, and memcached, ZooKeeper and NUT split a command's arguments on
// blanks. An sshexec value is quoted for the remote shell, but lands in an
// argument of a program the exporter knows nothing of, and a forced
// command's wrapper may split $SSH_ORIGINAL_COMMAND on blanks again. So a
// value is held to what names a thing, a proxy, a server, a UPS, a key, a
// unit or a path, and nothing that separates or quotes: a probe's value
// stays one argument and adds none.
var lineValue = regexp.MustCompile(`^[A-Za-z0-9._\-:/@+=,%]*$`)

// lineValueRefused names the first character of value lineValue does not
// take, and says whether there is one. A blank, a line break and a control
// character are named, as a quoted glyph would not show them; any other
// is shown with its code point.
func lineValueRefused(value string) (string, bool) {
	if lineValue.MatchString(value) {
		return "", false
	}
	for rest := value; rest != ""; {
		r, size := utf8.DecodeRuneInString(rest)
		rest = rest[size:]
		switch {
		case r == utf8.RuneError && size == 1:
			return "a byte that is not UTF-8", true
		case lineValue.MatchString(string(r)):
			continue
		case r == ' ':
			return "a blank (U+0020)", true
		case r == '\r' || r == '\n':
			return fmt.Sprintf("a line break (%U)", r), true
		case unicode.IsControl(r):
			return fmt.Sprintf("a control character (%U)", r), true
		}
		return fmt.Sprintf("%q (%U)", r, r), true
	}
	return "", false
}
