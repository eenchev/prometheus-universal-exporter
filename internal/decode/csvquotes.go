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
// taken as written rather than failing the whole file. encoding/csv offers
// that only together with a second leniency, for quoted fields: with its
// LazyQuotes a quoted field with a stray quote, or left open, reads on to the
// next quote it finds, or to the end of the body, taking the rows on the way
// as part of the one field, without a word. So a body the strict reader
// refuses for a bare quote is read again leniently, and every quoted field of
// that reading is then held against the body (checkQuotedField): one that is
// not written as the strict reader requires fails the decode with the error
// the strict reader gives it, whatever the other rows hold.
//
// trimSpace has the reader skip the white space a field starts with, which
// lets a quoted field begin after blanks; the fields are trimmed on both
// sides once read (decodeCSV). With a delimiter that is white space itself
// the reader takes a delimiter at the start of a field for such a blank:
//
//   - A space is left to it, as it always was: a run of spaces is then one
//     delimiter, which is how columns aligned with spaces are read, and an
//     empty field cannot be written.
//   - A tab, or another blank that is no space, is not: the delimiter after
//     an empty field would be skipped, and every later field of the row move
//     a column to the left. The reader then skips nothing, and the blanks
//     that stand between the start of a field and the quote of a quoted one
//     are taken out of the body before it is read (withoutBlanksBeforeQuotes),
//     so such a field is still read as a quoted one, the delimiters in it
//     kept. An error's column is given as the body has it (originalColumn).
func readCSV(body []byte, delimiter rune, trimSpace bool) ([][]string, error) {
	if !trimSpace || delimiter == ' ' || !unicode.IsSpace(delimiter) {
		return readCSVBody(body, delimiter, trimSpace)
	}
	body, removed := withoutBlanksBeforeQuotes(body, delimiter)
	rows, err := readCSVBody(body, delimiter, false)
	return rows, originalColumn(err, body, removed)
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

// csvRemoved says that n bytes were taken out of a body before the byte that
// is at offset at in what is left.
type csvRemoved struct{ at, n int }

// withoutBlanksBeforeQuotes returns body without the white space, other than
// the delimiter and a line feed, that stands between the start of a field and
// a quote, and what it took out, in rising order; a body that has none is
// returned as it is. `a<tab>  "b<tab>c"` becomes `a<tab>"b<tab>c"`, which
// the reader reads as two fields, the second a quoted one.
//
// It follows the fields as the reader does: a field starts at the start of
// the body, after a delimiter and after a line feed; one that starts with a
// quote, once the blanks are gone, runs to its closing quote, a doubled quote
// being a quote in it, over delimiters and line ends; any other runs to the
// next delimiter or line feed, whatever quotes it holds. So blanks inside a
// quoted field, and before a quote inside a field, are left alone. In a
// quoted field that is not written as the reader requires the two may part;
// the reader fails on that field (readCSVBody), before what follows it is of
// any consequence.
func withoutBlanksBeforeQuotes(body []byte, delimiter rune) ([]byte, []csvRemoved) {
	var (
		out     []byte
		removed []csvRemoved
		// kept is how much of body is in out, or would be were one made.
		kept int
	)
	for i := 0; i < len(body); {
		// A field starts at i.
		start := i
		for i < len(body) {
			r, width := utf8.DecodeRune(body[i:])
			if r == delimiter || r == '\n' || !unicode.IsSpace(r) {
				break
			}
			i += width
		}
		quoted := i < len(body) && body[i] == '"'
		if quoted && i > start {
			if out == nil {
				out = make([]byte, 0, len(body))
			}
			out = append(out, body[kept:start]...)
			kept = i
			removed = append(removed, csvRemoved{at: len(out), n: i - start})
		}
		if quoted {
			for i++; i < len(body); i++ {
				if body[i] != '"' {
					continue
				}
				if i+1 < len(body) && body[i+1] == '"' {
					i++
					continue
				}
				i++
				break
			}
		}
		// The rest of the field, to the delimiter or line feed that ends it.
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

// originalColumn gives the error of reading a body that
// withoutBlanksBeforeQuotes shortened the column it has in the body as it
// was: what was taken out of the error's line before it is counted again.
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
// requires: every quote in it doubled, and a quote closing it. Then the two
// readings agree, and the field ends where its closing quote is. Otherwise
// the lenient reading took a stray quote as text and read on, or ran into the
// end of the body, and the error is the strict reader's, at the place it
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
