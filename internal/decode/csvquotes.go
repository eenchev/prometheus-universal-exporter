package decode

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"unicode"
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
func readCSV(body []byte, delimiter rune, trimLeadingSpace bool) ([][]string, error) {
	reader := func() *csv.Reader {
		cr := csv.NewReader(bytes.NewReader(body))
		cr.Comma = delimiter
		cr.FieldsPerRecord = -1
		// Not when the delimiter is itself white space, a tab above all:
		// the reader would take the delimiter after an empty field for a
		// leading blank of the next one, and every later field of the row
		// would move a column to the left. The fields are trimmed on both
		// sides once read (decodeCSV), which is all trim_space asks for;
		// here it only lets a quoted field begin after blanks.
		cr.TrimLeadingSpace = trimLeadingSpace && !unicode.IsSpace(delimiter)
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
