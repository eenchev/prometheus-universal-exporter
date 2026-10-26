package decode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The toml decoder reads a TOML document with github.com/BurntSushi/toml,
// into the values the json decoder makes, so that a jq rule reads it as it
// reads JSON: a table as a map[string]any, an array, an array of tables
// among them, as a []any, a string, a boolean, an integer as an int, and a
// float as a float64, nan and inf included, which the rules read as NaN and
// +Inf. A date or a time, which JSON has not, is its text: an offset
// date-time in RFC 3339 (1979-05-27T07:32:00Z, 1979-05-27T00:32:00-07:00,
// with the fraction of a second it was written with), a local date-time as
// 1979-05-27T07:32:00, a local date as 1979-05-27 and a local time as
// 07:32:00, so that time_format reads it as written.
//
// The library keeps no bound of its own on what a document costs, and two
// things a target's answer may hold cost it far more than their size:
//
//   - Arrays and inline tables nested one inside another are parsed by a
//     function that calls itself for each, so a body of nothing but [ would
//     take a stack frame for each byte. Nesting is bounded at MaxDepth, as
//     every decoder's is, before the library sees the body (tomlNesting).
//   - A key is looked up from the root for each of its parts, and each
//     table the parts open is remembered by its whole name, so a key costs
//     the square of how deep its tables are: a mebibyte of keys each 32
//     tables deep took three seconds, and a single key 10,000 deep three. So
//     the parts a value is named by from the root, those of its [table]
//     header, of its key and of the keys of the inline tables around it, may
//     be no more than maxTOMLTables, also counted before the library sees
//     the body. A real document is a few tables deep.

// maxTOMLTables is how many parts a value of a TOML document may be named
// by from the root, through its table header, its key and the keys of the
// inline tables around it: the tables it lies in, and its own key. Within
// it, a document costs the library less than ten times what a flat one of
// its size does: a mebibyte of keys of 16 parts each, or of inline tables
// nested 15 deep, took it a second, where one of flat keys took 0.13.
const maxTOMLTables = 16

// decodeTOML decodes body as a TOML document.
func decodeTOML(body []byte) (any, error) {
	if err := tomlNesting(body); err != nil {
		return nil, err
	}
	var v any
	if _, err := toml.Decode(string(body), &v); err != nil {
		var parse toml.ParseError
		if errors.As(err, &parse) {
			return nil, tomlParseError(parse)
		}
		return nil, err
	}
	// A document of nothing but comments is the empty table, as the
	// library makes it.
	return tomlValue(v, 0)
}

// maxTOMLMessage is how much of the library's message a TOML error keeps:
// every message the library writes of its own is shorter, and what makes
// one longer is a token of the body it quotes.
const maxTOMLMessage = 256

// tomlParseError is the library's error with no more of the body in it than
// a failure may quote: its message may quote a token of any length, and is
// cut past maxTOMLMessage bytes, with its length, as a decoder cuts a value
// it quotes; the last key the library read, which it adds, is left out, as
// it may be any length too.
func tomlParseError(parse toml.ParseError) error {
	// The message is the library's own, or that of the error it wraps,
	// which it does not export but writes after its prefix.
	parse.LastKey = ""
	message := strings.TrimPrefix(parse.Error(), fmt.Sprintf("toml: line %d: ", parse.Position.Line))
	line := model.Position(parse.Position.Line)
	head := headOf(message, maxTOMLMessage)
	var err error
	if head == len(message) {
		err = model.Errorf("line %d: %s", line, message)
	} else {
		start := message[:head]
		err = model.SameFailureAs(model.Errorf("line %d: %s%s", line, start, cutMark(len(message), false)), "line "+model.MovingMark+": "+start+cutMark(len(message), true))
	}
	// The library quotes what it found, as Go quotes a text: a message
	// with a quote in it holds a token of the body, which a probe's caller
	// is not told.
	if strings.Contains(message, `"`) {
		err = model.WithOperatorDetail(err, "line "+strconv.Itoa(parse.Position.Line)+": "+model.WithheldMark)
	}
	return err
}

// tomlValue is v, a value the library made, as the json decoder makes one,
// at depth, the number of tables and arrays around it.
func tomlValue(v any, depth int) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		if depth++; depth > MaxDepth {
			return nil, errTOMLTooDeep
		}
		for k, item := range x {
			value, err := tomlValue(item, depth)
			if err != nil {
				return nil, err
			}
			x[k] = value
		}
		return x, nil
	case []map[string]any:
		if depth++; depth > MaxDepth {
			return nil, errTOMLTooDeep
		}
		out := make([]any, len(x))
		for i, item := range x {
			value, err := tomlValue(item, depth)
			if err != nil {
				return nil, err
			}
			out[i] = value
		}
		return out, nil
	case []any:
		if depth++; depth > MaxDepth {
			return nil, errTOMLTooDeep
		}
		for i, item := range x {
			value, err := tomlValue(item, depth)
			if err != nil {
				return nil, err
			}
			x[i] = value
		}
		return x, nil
	case int64:
		return int(x), nil
	case float64, string, bool:
		return x, nil
	case time.Time:
		return tomlTime(x), nil
	}
	return nil, fmt.Errorf("the TOML library made a value of type %T, which the exporter does not read; this is a defect of the exporter, please report it", v)
}

// errTOMLTooDeep is the error of a document whose tables and arrays nest
// deeper than MaxDepth together, which tomlNesting, bounding each apart,
// lets by: arrays nested 10,000 deep in a table.
var errTOMLTooDeep = model.Errorf("the document is nested more than %d deep; a response may nest %d deep at most", MaxDepth, MaxDepth)

// tomlTime is a date or a time as TOML writes it. The library tells a local
// date-time, date and time apart by the name of the zone it gives them.
func tomlTime(t time.Time) string {
	switch t.Location().String() {
	case "datetime-local":
		return t.Format("2006-01-02T15:04:05.999999999")
	case "date-local":
		return t.Format(time.DateOnly)
	case "time-local":
		return t.Format("15:04:05.999999999")
	}
	return t.Format(time.RFC3339Nano)
}

// tomlNesting refuses a body whose arrays and inline tables nest deeper than
// MaxDepth, or a value of which is named by more than maxTOMLTables parts,
// before the library reads it. It reads the body as the
// library's lexer does as far as the two depend on it: comments, the four
// kinds of string, the brackets of table headers, arrays and inline tables,
// and where a key stands. Where it cannot follow the body it refuses it,
// rather than leave the rest to the library: what lies past that place is
// unscanned, and bounded by nothing, so a body the library reads otherwise
// than the scan does, as it once read one starting with a second byte order
// mark, would reach the library's recursion with no bound at all. A body the
// scan cannot follow is no TOML, and the library would fail at the same
// place; the message is the exporter's.
func tomlNesting(body []byte) error {
	// The library passes over one byte order mark of its own, UTF-8 or
	// either UTF-16 one, after the one Decode took off the response.
	for _, mark := range []string{"\xef\xbb\xbf", "\xff\xfe", "\xfe\xff"} {
		if strings.HasPrefix(string(body), mark) {
			body = body[len(mark):]
			break
		}
	}
	var (
		line model.Position = 1
		// open is the brackets of the arrays and inline tables around
		// where the scan is.
		open []byte
		// header is the parts of the last table header; keys holds the
		// parts of the key of each level of keys around where the scan is:
		// the line's own, then one for each inline table open.
		header int
		keys   = []int{0}
		// atKey says a key comes next: at the start of a line outside any
		// array, and after the { or , of an inline table.
		atKey = true
	)
	tables := func() int {
		n := header
		for _, parts := range keys {
			n += parts
		}
		return n
	}
	for i := 0; i < len(body); {
		c := body[i]
		switch c {
		case '\n':
			line++
			i++
			if len(open) == 0 {
				atKey, keys = true, keys[:1]
				keys[0] = 0
			}
			continue
		case ' ', '\t', '\r':
			i++
			continue
		case '#':
			for i < len(body) && body[i] != '\n' {
				i++
			}
			continue
		}
		if atKey && c == '[' && len(open) == 0 {
			// A table header, [a.b] or [[a.b]].
			i++
			if i < len(body) && body[i] == '[' {
				i++
			}
			parts, next, ok := tomlKey(body, i)
			if !ok || next >= len(body) || body[next] != ']' {
				return model.Errorf("line %d: expected a table header, [name] or [[name]]", line)
			}
			header, i = parts, next+1
			if i < len(body) && body[i] == ']' {
				i++
			}
			if header > maxTOMLTables {
				return tomlTooManyTables(line)
			}
			atKey = false
			continue
		}
		if atKey {
			if c == '}' && len(open) > 0 && open[len(open)-1] == '{' {
				// The inline table is empty, or ends after a comma.
				open, keys = open[:len(open)-1], keys[:len(keys)-1]
				atKey = false
				i++
				continue
			}
			parts, next, ok := tomlKey(body, i)
			if !ok || next >= len(body) || body[next] != '=' {
				return model.Errorf("line %d: expected a key and = after it", line)
			}
			keys[len(keys)-1] = parts
			if tables() > maxTOMLTables {
				return tomlTooManyTables(line)
			}
			atKey, i = false, next+1
			continue
		}
		switch c {
		case '"', '\'':
			next, lines, ok := tomlString(body, i)
			if !ok {
				return model.Errorf("line %d: a string that does not end", line)
			}
			i, line = next, line+model.Position(lines)
			continue
		case '[', '{':
			if open = append(open, c); len(open) > MaxDepth {
				return model.Errorf("line %d: arrays and inline tables nested more than %d deep; a response may nest %d deep at most", line, MaxDepth, MaxDepth)
			}
			if c == '{' {
				keys = append(keys, 0)
				atKey = true
			}
		case ']', '}':
			want := byte('[')
			if c == '}' {
				want = '{'
			}
			if len(open) == 0 || open[len(open)-1] != want {
				what := "array"
				if c == '}' {
					what = "inline table"
				}
				return model.Errorf("line %d: a %c that closes no %s", line, c, what)
			}
			open = open[:len(open)-1]
			if c == '}' {
				keys = keys[:len(keys)-1]
			}
		case ',':
			if len(open) > 0 && open[len(open)-1] == '{' {
				keys[len(keys)-1] = 0
				atKey = true
			}
		}
		i++
	}
	return nil
}

// tomlTooManyTables is the error of a value at line named by more than
// maxTOMLTables parts.
func tomlTooManyTables(line model.Position) error {
	return model.Errorf("line %d: a value is named by more than %d parts, counting those of its table header, of its key and of the keys of the inline tables around it; a TOML response may name a value by %d at most", line, maxTOMLTables, maxTOMLTables)
}

// tomlKey reads the key that starts at i, after any blanks, up to the first
// byte after it and its blanks that is no part of it, and says how many
// parts it has. It reports false for what is no key.
func tomlKey(body []byte, i int) (parts, next int, ok bool) {
	for {
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		if i == len(body) {
			return 0, 0, false
		}
		switch c := body[i]; {
		case c == '"' || c == '\'':
			end, lines, ok := tomlString(body, i)
			if !ok || lines > 0 {
				return 0, 0, false
			}
			i = end
		case tomlBareKeyByte(c):
			for i < len(body) && tomlBareKeyByte(body[i]) {
				i++
			}
		default:
			return 0, 0, false
		}
		parts++
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		if i == len(body) || body[i] != '.' {
			return parts, i, true
		}
		i++
	}
}

// tomlBareKeyByte reports whether c may be part of a bare key.
func tomlBareKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// tomlString passes over the string whose opening quote is at i: basic or
// literal, on one line or in three quotes over several. It returns the
// place after its closing quotes and how many line feeds it holds, and
// false for a string that does not end.
func tomlString(body []byte, i int) (next, lines int, ok bool) {
	quote := body[i]
	escapes := quote == '"'
	if i+2 < len(body) && body[i+1] == quote && body[i+2] == quote {
		// Three quotes: the string ends at the next three, and up to two
		// more quotes right before them are part of it.
		for j := i + 3; j < len(body); j++ {
			switch body[j] {
			case '\\':
				if escapes {
					j++
					if j < len(body) && body[j] == '\n' {
						lines++
					}
				}
			case '\n':
				lines++
			case quote:
				if j+2 < len(body) && body[j+1] == quote && body[j+2] == quote {
					end := j + 3
					for k := 0; k < 2 && end < len(body) && body[end] == quote; k++ {
						end++
					}
					return end, lines, true
				}
			}
		}
		return 0, 0, false
	}
	for j := i + 1; j < len(body); j++ {
		switch body[j] {
		case '\\':
			if escapes {
				j++
			}
		case '\n':
			return 0, 0, false
		case quote:
			return j + 1, 0, true
		}
	}
	return 0, 0, false
}
