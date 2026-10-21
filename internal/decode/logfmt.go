package decode

import (
	"bytes"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The logfmt decoder reads logfmt, the key=value lines of Heroku's logs and
// of the Go loggers that write them, as github.com/go-logfmt/logfmt reads
// them; the module is not a dependency, the grammar being a few lines. Its
// document is the list of the records, one for each line that holds one, in
// the order of their lines, so that a jq rule reads it as it reads a JSON
// array of objects.
//
//   - A line is what lies between two line feeds. A line of nothing but
//     spaces and control characters, any byte up to and including the space,
//     is passed over; those bytes are also what separates the pairs of a
//     line.
//   - A pair is a key, then = and its value. A key is a run of bytes above
//     the space other than = and ". A value is a run of the same bytes, or a
//     quoted string: in double quotes, with the escapes of a JSON string (\"
//     \\ \/ \b \f \n \r \t \uXXXX), and no raw line feed.
//   - A record is a mapping of the line's keys to their values, as text: a
//     number is a value the rules read as a number (What counts as a number
//     in docs/CONFIGURATION.md). A key written without = is null, so that
//     has("key") tells it from a key the line does not write, and a key
//     written with = and nothing after it is the empty string.
//   - Of a key written twice on one line, the later is kept, as the json
//     decoder keeps the later member: a logger may well add a key a caller
//     already gave, and refusing the whole body over one such line would
//     lose every other record.
//   - An error names the line and column, as model.Position, and the byte
//     that cannot be there, never more of the body.

// decodeLogfmt decodes body as logfmt.
func decodeLogfmt(body []byte) ([]any, error) {
	records := []any{}
	var unquoted []byte
	for start, line := 0, model.Position(1); start <= len(body); line++ {
		end := bytes.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		text := body[start:end]
		start = end + 1
		var record map[string]any
		for i := 0; ; {
			for i < len(text) && text[i] <= ' ' {
				i++
			}
			if i == len(text) {
				break
			}
			at := i
			for i < len(text) && text[i] > ' ' && text[i] != '=' && text[i] != '"' {
				i++
			}
			if i == at || i < len(text) && text[i] == '"' {
				return nil, logfmtUnexpected(text, line, i, "where a key")
			}
			key := string(text[at:i])
			if record == nil {
				record = map[string]any{}
			}
			if i == len(text) || text[i] != '=' {
				record[key] = nil
				continue
			}
			i++
			switch {
			case i == len(text) || text[i] <= ' ':
				record[key] = ""
			case text[i] == '"':
				value, next, err := logfmtQuoted(text, i, line, unquoted[:0])
				if err != nil {
					return nil, err
				}
				record[key], i, unquoted = string(value), next, value
				if i < len(text) && text[i] > ' ' {
					return nil, logfmtUnexpected(text, line, i, "after a quoted value")
				}
			default:
				at = i
				for i < len(text) && text[i] > ' ' {
					if text[i] == '=' || text[i] == '"' {
						return nil, logfmtUnexpected(text, line, i, "in a value; quote a value that holds it")
					}
					i++
				}
				record[key] = string(text[at:i])
			}
		}
		if record != nil {
			records = append(records, record)
		}
	}
	return records, nil
}

// logfmtUnexpected is the error of the byte at i of a line, or of the line
// ending there.
func logfmtUnexpected(text []byte, line model.Position, i int, where string) error {
	if i >= len(text) {
		return model.Errorf("line %d: the line ends %s", line, where)
	}
	return model.Errorf("line %d, column %d: unexpected %s %s", line, model.Position(i+1), quoteJSONByte(text[i]), where)
}

// logfmtQuoted reads the quoted value whose opening quote is at i of a line,
// appending it to dst, and returns it and the place after its closing quote.
func logfmtQuoted(text []byte, i int, line model.Position, dst []byte) ([]byte, int, error) {
	at := i
	for i++; i < len(text); {
		c := text[i]
		switch {
		case c == '"':
			return dst, i + 1, nil
		case c < ' ':
			return nil, 0, model.Errorf("line %d, column %d: control character %s in a quoted value; escape it", line, model.Position(i+1), quoteJSONByte(c))
		case c != '\\':
			dst = append(dst, c)
			i++
			continue
		}
		if i+1 == len(text) {
			break
		}
		switch e := text[i+1]; e {
		case '"', '\\', '/':
			dst = append(dst, e)
		case 'b':
			dst = append(dst, '\b')
		case 'f':
			dst = append(dst, '\f')
		case 'n':
			dst = append(dst, '\n')
		case 'r':
			dst = append(dst, '\r')
		case 't':
			dst = append(dst, '\t')
		case 'u':
			r, ok := logfmtHex(text, i+2)
			if !ok {
				return nil, 0, model.Errorf("line %d, column %d: \\u in a quoted value is not followed by four hexadecimal digits", line, model.Position(i+1))
			}
			i += 6
			if utf16.IsSurrogate(r) {
				// The second half of a pair, written as an escape
				// too, or U+FFFD, as encoding/json reads them.
				if low, ok := logfmtHex(text, i+2); ok && i+1 < len(text) && text[i] == '\\' && text[i+1] == 'u' {
					if pair := utf16.DecodeRune(r, low); pair != utf8.RuneError {
						dst = utf8.AppendRune(dst, pair)
						i += 6
						continue
					}
				}
				r = utf8.RuneError
			}
			dst = utf8.AppendRune(dst, r)
			continue
		default:
			return nil, 0, model.Errorf("line %d, column %d: invalid escape \\%s in a quoted value", line, model.Position(i+1), quoteJSONByte(e)[1:len(quoteJSONByte(e))-1])
		}
		i += 2
	}
	return nil, 0, model.Errorf("line %d, column %d: the quoted value has no closing quote on its line", line, model.Position(at+1))
}

// logfmtHex reads the four hexadecimal digits at i of a line.
func logfmtHex(text []byte, i int) (rune, bool) {
	if i+4 > len(text) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(text[i:i+4]), 16, 16)
	return rune(n), err == nil
}
