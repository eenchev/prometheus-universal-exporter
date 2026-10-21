package decode

import (
	"bytes"
	"errors"
	"io"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The ndjson decoder reads newline-delimited JSON, also called JSON Lines:
// one JSON value on each line, as a log shipper, a bulk API or an export of
// records writes them. Its document is the list of those values, in the
// order of their lines, so that a jq rule reads it as it reads a JSON array
// (`.[]`, items: `.[]`); a body of no value at all is the empty list.
//
//   - A line is what lies between two line feeds, a carriage return before
//     the line feed being whitespace. A line of nothing but whitespace is
//     passed over.
//   - Each other line is one value as the json decoder reads a document
//     (jsonvalue.go): the same grammar, numbers and strings made the same
//     way, and nothing but whitespace after it on its line. A value written
//     across lines, as a pretty-printed one is, is not one value per line,
//     and is refused at the line it ends early on.
//   - The list counts as the outermost level of the document, so a value of
//     a line nests MaxDepth - 1 deep at most, and the whole document is
//     within MaxDepth as every decoder's is.
//   - An error names the line, and the column, the json decoder's error
//     names, counted in the whole body, as model.Position so that the
//     failure is the same failure at whichever line it occurs.

// errNDJSONTrailingData is the error of a line that goes on after its value.
var errNDJSONTrailingData = errors.New("trailing data after the JSON value; NDJSON holds one value on each line")

// errNDJSONLineEnds is the error of a line that ends before its value does.
var errNDJSONLineEnds = errors.New("the line ends inside its JSON value; NDJSON holds one whole value on each line")

// decodeNDJSON decodes body as newline-delimited JSON.
func decodeNDJSON(body []byte) ([]any, error) {
	// One decoder for every line, so that the strings the lines share,
	// their keys above all, are made once (jsonDecoder.remembered).
	d := jsonDecoder{data: body}
	d.sizeCache()
	values := []any{}
	for start, line := 0, model.Position(1); start <= len(body); line++ {
		end := bytes.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		// The line is read in the whole body, cut at its end, so that the
		// line and column an error names are the body's.
		d.data, d.pos, d.depth = body[:end], start, 1
		d.space()
		if d.pos < end {
			v, err := d.value()
			switch {
			case errors.Is(err, io.ErrUnexpectedEOF):
				return nil, model.Errorf("line %d: %w", line, errNDJSONLineEnds)
			case err != nil:
				// The json decoder's errors say the line and column
				// themselves.
				return nil, err
			}
			if d.space(); d.pos < end {
				_, column := d.position(d.pos)
				return nil, model.Errorf("line %d, column %d: %w", line, column, errNDJSONTrailingData)
			}
			values = append(values, v)
		}
		start = end + 1
	}
	return values, nil
}
