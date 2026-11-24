package decode

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"unicode"
	"unicode/utf8"
)

// readCSV reads every record of a CSV body.
//
// A quote inside a field that does not start with one, as in `5" disk`, is
// kept as written instead of failing the whole file. encoding/csv allows
// that only through LazyQuotes, which also relaxes quoted fields: a quoted
// field with a stray quote, or one left open, then reads on to the next
// quote or to the end of the body. The rows on the way silently become part
// of that one field. So when the strict reader refuses a body for a bare
// quote, the body is read again leniently. Every quoted field of that
// reading is then checked against the body (checkQuotedField). A field not
// written as the strict reader requires fails the decode with the strict
// reader's error for it, whatever the other rows hold.
//
// trimSpace makes the reader skip the white space at the start of a field,
// so a quoted field can begin after blanks. Fields are trimmed on both sides
// once read (decodeCSV). If the delimiter is itself white space, the reader
// takes a delimiter at the start of a field for such a blank:
//
//   - A space is left to the reader, as before. A run of spaces is then one
//     delimiter. This is how space-aligned columns are read, and an empty
//     field cannot be written.
//   - A tab, or another blank that is not a space, is not: the delimiter
//     after an empty field would be skipped, and every later field of the
//     row would move one column left. The reader then skips nothing. Instead,
//     the blanks between the start of a field and the quote of a quoted one
//     are removed from the body before it is read (withoutBlanksAroundQuotes).
//     Such a field is still read as quoted, with its delimiters kept.
//
// The reader accepts no blanks after a quoted field: the closing quote must
// be followed by the delimiter or the line end, or the field is refused. So
// the blanks between a closing quote and the delimiter or line end after it
// are removed from the body too, whatever the delimiter. An error's column
// is given as in the original body (originalColumn). For that the body is
// scanned a second time, to learn what was removed and where. Only an error
// needs this: recording it for every body cost several times the body's
// size for a body padded with such blanks.
//
// A lone carriage return also ends a record (carriageReturnsAsLineEnds).
// The reader does not support this, so it is made a line feed before
// anything else reads the body.
func readCSV(body []byte, delimiter rune, trimSpace bool) ([][]string, error) {
	body = carriageReturnsAsLineEnds(body, delimiter, trimSpace)
	if !trimSpace {
		return readCSVBody(body, delimiter, false)
	}
	readerSkips := delimiter == ' ' || !unicode.IsSpace(delimiter)
	read, _ := takeBlanksAroundQuotes(body, delimiter, !readerSkips, false)
	rows, err := readCSVBody(read, delimiter, readerSkips)
	if err == nil || len(read) == len(body) {
		return rows, err
	}
	_, removed := withoutBlanksAroundQuotes(body, delimiter, !readerSkips)
	return rows, originalColumn(err, read, removed)
}

// readCSVBody is readCSV of a body the reader is to read as it is, skipping
// the white space fields start with or not.
func readCSVBody(body []byte, delimiter rune, trimLeadingSpace bool) ([][]string, error) {
	reader := func() *csv.Reader {
		cr := csv.NewReader(bytes.NewReader(body))
		cr.Comma = delimiter
		cr.FieldsPerRecord = -1
		cr.TrimLeadingSpace = trimLeadingSpace
		return cr
	}
	rows, err := reader().ReadAll()
	if !errors.Is(err, csv.ErrBareQuote) {
		return rows, err
	}
	lenient := reader()
	lenient.LazyQuotes = true
	lines := lineOffsets{body: body, line: 1}
	rows = nil
	for {
		record, err := lenient.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		recordLine, _ := lenient.FieldPos(0)
		for i, field := range record {
			line, column := lenient.FieldPos(i)
			start := lines.offset(line) + column - 1
			// A field that does not start with a quote is not a quoted
			// field: it ends at the next delimiter or line end, and a
			// quote in it is the leniency asked for.
			if start >= len(body) || body[start] != '"' {
				continue
			}
			if err := checkQuotedField(body, start, field, recordLine, line); err != nil {
				return nil, err
			}
		}
		rows = append(rows, record)
	}
}

// carriageReturnsAsLineEnds returns body with every carriage return that
// ends a record on its own (no line feed after it) made a line feed. This is
// the line end of a spreadsheet's "CSV (Macintosh)" and of some instruments.
// Python's csv module reads it as a line end; encoding/csv reads it as a
// character of the field, so such a body read as a header and no rows. A
// carriage return inside a quoted field is the field's text and stays. One
// byte replaces the other, so every line and column of the body stays where
// it was, and the lines the reader counts in an error match the file.
//
// A body without such a carriage return is returned as it is. Looking for
// one costs nothing for a body without any carriage return, and a look at
// the byte after each one for a body with CRLF line ends. Only a body that
// has one is copied. Only a body that also has a quote is scanned field by
// field, following the fields as withoutBlanksAroundQuotes does, with the
// same rule for a quoted field: one that starts with a quote, after blanks
// under trim_space. A carriage return that ends the record is not such a
// blank.
//
// A carriage return at the end of the body is left to the reader, which
// drops it. No copy is made for that one.
//
// A quoted field left open has no end, so every carriage return after its
// quote would be its text. The reader fails on such a field at the end of
// the body, and would report that end as a column of the field's own line:
// the length of the rest of the file. The decode fails either way. So the
// carriage returns after the quote of a field still open at the end of the
// body also end lines, the last byte among them. The error is then the one
// the same body gives with line feeds: the line the body ends on, and the
// column there. A body the reader accepts has no such field, so no body
// it accepts is read differently.
func carriageReturnsAsLineEnds(body []byte, delimiter rune, trimSpace bool) []byte {
	last := len(body) - 1
	first := loneCarriageReturn(body, 0)
	if first < 0 {
		return body
	}
	if bytes.IndexByte(body, '"') < 0 {
		if first == last {
			return body
		}
		out := bytes.Clone(body)
		for at := first; at >= 0 && at < last; at = loneCarriageReturn(body, at+1) {
			out[at] = '\n'
		}
		return out
	}
	// alone reports whether the byte at i is a carriage return with no line
	// feed after it.
	alone := func(i int) bool {
		return body[i] == '\r' && (i+1 == len(body) || body[i+1] != '\n')
	}
	// out is the copy, made for the first of them that ends a record: a body
	// that has them only inside quoted fields is returned as it is.
	out, copied := body, false
	for i := 0; i < len(body); {
		// A field starts at i.
		if trimSpace {
			for i < len(body) {
				r, width := utf8.DecodeRune(body[i:])
				if r == delimiter || r == '\n' || !unicode.IsSpace(r) || alone(i) {
					break
				}
				i += width
			}
		}
		if i < len(body) && body[i] == '"' {
			opened, closed := i, false
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
				// The field is open at the end of the body.
				for at := loneCarriageReturn(body, opened); at >= 0; at = loneCarriageReturn(body, at+1) {
					if !copied {
						out, copied = bytes.Clone(body), true
					}
					out[at] = '\n'
				}
				return out
			}
		}
		// The rest of the field, to the delimiter or the line end after it.
		for i < len(body) {
			c := body[i]
			if c == '\r' && alone(i) {
				if i < last {
					if !copied {
						out, copied = bytes.Clone(body), true
					}
					out[i] = '\n'
				}
				i++
				break
			}
			if c < utf8.RuneSelf {
				i++
				if rune(c) == delimiter || c == '\n' {
					break
				}
				continue
			}
			r, width := utf8.DecodeRune(body[i:])
			i += width
			if r == delimiter {
				break
			}
		}
	}
	return out
}

// loneCarriageReturn is where the first carriage return of body at or after
// from is that no line feed follows, or -1.
func loneCarriageReturn(body []byte, from int) int {
	for from < len(body) {
		i := bytes.IndexByte(body[from:], '\r')
		if i < 0 {
			break
		}
		from += i + 1
		if from == len(body) || body[from] != '\n' {
			return from - 1
		}
	}
	return -1
}

// csvRemoved says that n bytes were taken out of a body before the byte that
// is at offset at in what is left.
type csvRemoved struct{ at, n int }

// withoutBlanksAroundQuotes returns body without some of its white space
// (never the delimiter or a line feed), and what it removed, in rising
// order. It removes the blanks between a quoted field's closing quote and
// the delimiter or line end after it. When leading is set, it also removes
// the blanks between the start of a field and its opening quote; when it is
// not set, the reader skips those itself. A body with none is returned as it
// is. With leading, `a<tab>  "b<tab>c" <tab>d` becomes `a<tab>"b<tab>c"<tab>d`,
// which the reader reads as three fields, the second one quoted.
//
// It follows the fields as the reader does. A field starts at the start of
// the body, after a delimiter and after a line feed. A field that starts with
// a quote (after blanks or not) runs to its closing quote, over delimiters
// and line ends; a doubled quote is a quote inside it. Any other field runs
// to the next delimiter or line feed, whatever quotes it holds. So these
// blanks are left alone: blanks inside a quoted field, blanks around a quote
// inside a field, and blanks after a closing quote that something other than
// the delimiter or line end follows (the reader still refuses those). The
// carriage return of a line end is not a blank after the quote before it.
// With a space as the delimiter, the blanks after a closing quote are tabs
// and similar: a space there is the delimiter. In a quoted field not written
// as the reader requires, the two may disagree. The reader fails on that
// field (readCSVBody) before what follows it matters.
func withoutBlanksAroundQuotes(body []byte, delimiter rune, leading bool) ([]byte, []csvRemoved) {
	return takeBlanksAroundQuotes(body, delimiter, leading, true)
}

// takeBlanksAroundQuotes is withoutBlanksAroundQuotes, but it reports what
// it removed only when record is set. Without record, the copy of the body
// is its only allocation, however many runs of blanks it removes. A list of
// them is four times the size of a body of `"" ,` repeated over and over.
func takeBlanksAroundQuotes(body []byte, delimiter rune, leading, record bool) ([]byte, []csvRemoved) {
	// Only a quoted field has blanks to take out.
	if bytes.IndexByte(body, '"') < 0 {
		return body, nil
	}
	var (
		out     []byte
		removed []csvRemoved
		// kept is how much of body is in out, or would be were one made.
		kept int
	)
	// remove takes body[from:to] out.
	remove := func(from, to int) {
		if out == nil {
			out = make([]byte, 0, len(body))
		}
		out = append(out, body[kept:from]...)
		kept = to
		if record {
			removed = append(removed, csvRemoved{at: len(out), n: to - from})
		}
	}
	// blanks is where the white space starting at i ends, short of the
	// delimiter and a line feed, and whether one of those two, or the end
	// of the body, is what it ends at.
	blanks := func(i int) (end int, ends bool) {
		for i < len(body) {
			r, width := utf8.DecodeRune(body[i:])
			if r == delimiter || r == '\n' {
				return i, true
			}
			if !unicode.IsSpace(r) {
				return i, false
			}
			i += width
		}
		return i, true
	}
	for i := 0; i < len(body); {
		// A field starts at i. White space is, in ASCII, the space and what
		// comes before it, and any other byte of ASCII starts the field's
		// text.
		start := i
		if c := body[i]; c <= ' ' || c >= utf8.RuneSelf {
			i, _ = blanks(i)
		}
		quoted := i < len(body) && body[i] == '"'
		if quoted && leading && i > start {
			remove(start, i)
		}
		if quoted {
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
			// Nearly always the delimiter or the line's end comes next.
			if closed && i < len(body) && body[i] != '\n' && rune(body[i]) != delimiter {
				if end, ends := blanks(i); ends {
					if end > i && body[end-1] == '\r' && (end == len(body) || body[end] == '\n') {
						end--
					}
					if end > i {
						remove(i, end)
						i = end
					}
				}
			}
		}
		// The rest of the field, to the delimiter or line feed that ends it.
		// A byte of a character beyond ASCII is never one of ASCII's, so a
		// delimiter of ASCII is looked for byte by byte.
		if delimiter >= 0 && delimiter < utf8.RuneSelf {
			single := byte(delimiter) //nolint:gosec // G115: within ASCII, as checked
			for i < len(body) {
				c := body[i]
				i++
				if c == single || c == '\n' {
					break
				}
			}
			continue
		}
		for i < len(body) {
			r, width := utf8.DecodeRune(body[i:])
			i += width
			if r == delimiter || r == '\n' {
				break
			}
		}
	}
	if out == nil {
		return body, nil
	}
	return append(out, body[kept:]...), removed
}

// originalColumn turns the column of an error from reading a body that
// withoutBlanksAroundQuotes shortened into the column in the original body.
// It adds back what was removed from the error's line before that column.
// The lines are the same in both.
func originalColumn(err error, body []byte, removed []csvRemoved) error {
	var parse *csv.ParseError
	if len(removed) == 0 || !errors.As(err, &parse) {
		return err
	}
	lines := lineOffsets{body: body, line: 1}
	start := lines.offset(parse.Line)
	at := start + parse.Column - 1
	for _, r := range removed {
		if r.at >= start && r.at <= at {
			parse.Column += r.n
		}
	}
	return err
}

// lineOffsets finds where the lines of a body start, for lines asked for in
// rising order, as the fields of a CSV body come.
type lineOffsets struct {
	body []byte
	// line, from 1, starts at start.
	line, start int
}

// offset is where line, at or after the last one asked for, starts.
func (l *lineOffsets) offset(line int) int {
	for l.line < line {
		end := bytes.IndexByte(l.body[l.start:], '\n')
		if end < 0 {
			l.start = len(l.body)
			break
		}
		l.start += end + 1
		l.line++
	}
	return l.start
}

// checkQuotedField reports whether the quoted field starting at body[start],
// which a lenient reading took as value, is written as the strict reader
// requires: every quote in it doubled, and a closing quote. If so, the two
// readings agree, and the field ends at its closing quote. If not, the
// lenient reading took a stray quote as text and read on, or reached the end
// of the body. The error is then the strict reader's, at the place where it
// would have stopped.
func checkQuotedField(body []byte, start int, value string, recordLine, fieldLine int) error {
	pos := start + 1
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '"':
			if !bytes.HasPrefix(body[pos:], []byte(`""`)) {
				return quoteError(body, start, pos, recordLine, fieldLine)
			}
			pos += 2
		case c == '\n' && bytes.HasPrefix(body[pos:], []byte("\r\n")):
			// The reader gives a line ending of \r\n as \n.
			pos += 2
		case pos < len(body) && body[pos] == c:
			pos++
		default:
			return quoteError(body, start, pos, recordLine, fieldLine)
		}
	}
	if pos >= len(body) || body[pos] != '"' {
		return quoteError(body, start, pos, recordLine, fieldLine)
	}
	return nil
}

// quoteError is the strict reader's error for a quoted field, starting at
// body[start] on fieldLine, that goes wrong at body[pos].
func quoteError(body []byte, start, pos, recordLine, fieldLine int) error {
	pos = min(pos, len(body))
	at := pos
	if at == len(body) && at > start && body[at-1] == '\n' {
		// The end of the body is the end of its last line, as the reader
		// counts it, not the start of a line after it.
		at--
	}
	return &csv.ParseError{
		StartLine: recordLine,
		Line:      fieldLine + bytes.Count(body[start:at], []byte("\n")),
		Column:    pos - bytes.LastIndexByte(body[:at], '\n'),
		Err:       csv.ErrQuote,
	}
}

// csvFieldLine returns the line (from 1) on which field number field of
// record number record (both from 0) starts, in the body as readCSV read it,
// or 0 when the body has no such field. It reads the body again, which only
// an error message is worth: the reader tracks fields one record at a time.
func csvFieldLine(body []byte, delimiter rune, trimSpace bool, record, field int) int {
	body = carriageReturnsAsLineEnds(body, delimiter, trimSpace)
	readerSkips := trimSpace
	if trimSpace {
		readerSkips = delimiter == ' ' || !unicode.IsSpace(delimiter)
		// The blanks taken out leave every line where it was.
		body, _ = takeBlanksAroundQuotes(body, delimiter, !readerSkips, false)
	}
	cr := csv.NewReader(bytes.NewReader(body))
	cr.Comma = delimiter
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = readerSkips
	// A body the strict reader reads is read the same leniently, and one it
	// refuses for a bare quote was read leniently (readCSVBody).
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	for i := 0; ; i++ {
		fields, err := cr.Read()
		if err != nil {
			return 0
		}
		if i == record {
			if field >= len(fields) {
				return 0
			}
			line, _ := cr.FieldPos(field)
			return line
		}
	}
}
