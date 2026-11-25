package decode

import (
	"bytes"
	"unicode"
	"unicode/utf8"
)

// withoutCommentLines returns body without the text of its comment lines:
// the lines that begin with comment where a record begins, as InfluxDB's
// annotated CSV begins its #datatype, #group and #default lines. Each keeps
// its line end, so the reader skips it as an empty line and every other
// line keeps its number and its columns. A body without the character is
// returned as it is, and so is one without a comment line; only a body that
// has one is copied.
//
// A line inside a quoted field is no line of its own, whatever it begins
// with. So the body is followed field by field, as carriageReturnsAsLineEnds
// follows it: a field that begins with a quote, after blanks under
// trimSpace, runs to its closing quote, over delimiters and line ends. A
// line ends at a line feed, and at a carriage return alone, which the reader
// is later given as a line feed. A comment line's quotes are then no quotes
// of a field for anything that reads the body after this.
//
// tables are the records, counted from 0 as the reader returns them, that
// begin a new table: the first record after comment lines that follow an
// empty line after a record. That is how annotated CSV begins a table whose
// columns differ from the table's before it, with its own annotation lines
// and header.
func withoutCommentLines(body []byte, comment, delimiter rune, trimSpace bool) (out []byte, tables []int) {
	if !bytes.ContainsRune(body, comment) {
		return body, nil
	}
	// kept is how much of body is in out, or would be were one made.
	kept := 0
	// records counts the records read; blank says an empty line came after
	// the last of them, and annotated that comment lines came after that.
	records, blank, annotated := 0, false, false
	// lineEnd is where the line that holds body[i] ends: at its line feed
	// or carriage return, or at the end of the body.
	lineEnd := func(i int) int {
		if end := bytes.IndexAny(body[i:], "\r\n"); end >= 0 {
			return i + end
		}
		return len(body)
	}
	for i := 0; i < len(body); {
		// A record begins at i, unless the line is empty, which the reader
		// skips.
		switch r, width := utf8.DecodeRune(body[i:]); {
		case r == '\n' || r == '\r':
			blank = records > 0
		case r == comment && width > 0:
			end := lineEnd(i)
			if out == nil {
				out = make([]byte, 0, len(body))
			}
			out = append(out, body[kept:i]...)
			kept, i = end, end
			annotated = blank
		default:
			if annotated {
				tables = append(tables, records)
			}
			records++
			blank, annotated = false, false
		}
		i = recordEnd(body, i, delimiter, trimSpace)
	}
	if out == nil {
		return body, tables
	}
	return append(out, body[kept:]...), tables
}

// recordEnd is where the record that begins at i ends: after the line end
// that ends its last field, or at the end of the body.
func recordEnd(body []byte, i int, delimiter rune, trimSpace bool) int {
	for i < len(body) {
		// A field begins at i.
		if trimSpace {
			for i < len(body) {
				r, width := utf8.DecodeRune(body[i:])
				if r == delimiter || r == '\n' || r == '\r' || !unicode.IsSpace(r) {
					break
				}
				i += width
			}
		}
		if i < len(body) && body[i] == '"' {
			closed := false
			for i++; i < len(body); i++ {
				if body[i] != '"' {
					continue
				}
				if i+1 < len(body) && body[i+1] == '"' {
					i++
					continue
				}
				i++
				closed = true
				break
			}
			if !closed {
				return len(body)
			}
		}
		// The rest of the field, to the delimiter or the line end after it.
		for i < len(body) {
			r, width := utf8.DecodeRune(body[i:])
			switch r {
			case '\n':
				return i + 1
			case '\r':
				if i+1 < len(body) && body[i+1] == '\n' {
					return i + 2
				}
				return i + 1
			}
			i += width
			if r == delimiter {
				break
			}
		}
	}
	return i
}
