package transform

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A request to a worker is one line of JSON, and writing it was a third of
// what handing a document of five thousand items to a script cost the
// exporter: json.Marshal of a pythonInput first copied the whole of data to
// put markers where its floats are not finite, then walked it by reflection,
// made a slice of each object's keys to sort them, and copied the body to a
// string to write it. pythonEncoder writes the same line — the same bytes,
// which pythonrequest_test.go compares over every kind of value — from the
// values a decoder makes, into a buffer it keeps, and what it does not know
// how to write is written as it was (pythonRequestMarshalled). The one
// request it writes otherwise than json.Marshal is one of csv rows by
// header, which it sends as their table, and the worker makes into the rows
// the line once carried (csvTable).
//
// What a script reads is therefore what it read: an object's keys in order
// of their bytes, which is the order a script iterates a dict in; a whole
// float, 1.0 in the response, as the int 1; a string that is not UTF-8 with
// U+FFFD for each byte that is not. A JSON response is not handed over as
// the bytes the target sent, although they are at hand, because a script
// would then read another document: its keys in the target's order, 1.0 as
// a float, 1e400 as inf rather than the text the json decoder keeps, and a
// whole number of five thousand digits as an int, or an error, rather than
// its text.

// pythonEncoder writes requests.
type pythonEncoder struct {
	// buf is the request being written, kept from one request to the next.
	buf []byte
	// members holds the members of the objects being written, innermost
	// last, each object's sorted by key.
	members []pythonMember
	// nul says a string of the request holds a NUL, without which none is
	// the marker of a float that is not finite.
	nul bool
	// limit is how long the request may be (pythonRequestLimit), and over
	// says the encoder stopped writing it for being longer.
	limit int
	over  bool
}

// pythonMember is a member of an object.
type pythonMember struct {
	key   string
	value any
}

// pythonEncoders keeps encoders, and with them their buffers, for the next
// request: a request is as large as the response it hands over, and one
// buffer grown to that size serves every scrape of the collector.
var pythonEncoders = sync.Pool{New: func() any { return &pythonEncoder{} }}

// pythonEncoderKept is the largest buffer an encoder is put back with; a
// larger one is left to the garbage collector rather than held for the next
// request of whatever size.
const pythonEncoderKept = 16 << 20

// pythonEncoderDepth is how deep the encoder follows data: as deep as a
// decoder makes a value, so that a response nested as deep as one decodes
// is handed to its script. It was 1,000, the depth at which json.Marshal
// starts looking for a value that holds itself, and deeper data was left
// to json.Marshal. A Go release that bounds the nesting json.Marshal
// writes then refused, with "exceeded max depth", a JSON document nested
// as deep as the decoder reads one, the request being one object deeper
// than its data. What nests deeper than any decoder's value no decoder
// made: it is written as it was, by json.Marshal, and refused as it was
// where json.Marshal refuses it, for holding itself or for its depth.
const pythonEncoderDepth = decode.MaxDepth

// pythonRequestFactor is how many times the response limit and
// limits.max_output_bytes together a request may be long. A request holds
// the body once and the data once, and the data of a pre-script, which is
// no longer than limits.max_output_bytes as the worker wrote it; JSON writes
// a byte as six at most (a control character as \u0001, < as \u003c), and
// a decoder makes more of a short line than its bytes — the prometheus
// decoder makes "a 1" a dict of five keys, some sixty bytes. A request
// longer than all that holds data a decoder made far more of than its
// response, and is refused before it is written to its end, rather than
// built in the exporter's memory and parsed in the worker's: the csv rows
// of a header of 5,000 columns over 5,000 short lines, 39 kB, were once
// written as a dict of every column for each row, a request of 257 MiB.
const pythonRequestFactor = 32

// pythonRequestLimit is how long the request that runs script for a
// collector c may be: pythonRequestFactor times its response limit and its
// limits.max_output_bytes together, and the script.
func pythonRequestLimit(script string, c *model.Collector) int {
	output := int64(c.Limits.MaxOutputBytes)
	if output <= 0 {
		output = 1 << 20
	}
	room := fetch.ResponseLimit(c)
	if room > math.MaxInt/(2*pythonRequestFactor) {
		return math.MaxInt
	}
	room = (room + min(output, math.MaxInt/(2*pythonRequestFactor))) * pythonRequestFactor
	return int(min(room, int64(math.MaxInt-len(script)))) + len(script)
}

// pythonRequestTooLarge is the error of a request longer than limit.
func pythonRequestTooLarge(limit int, c *model.Collector) error {
	output := int64(c.Limits.MaxOutputBytes)
	if output <= 0 {
		output = 1 << 20
	}
	return model.Errorf("the response and the data decoded from it come to more than %d bytes written out for the worker, %d times the response limit (%d bytes) and limits.max_output_bytes (%d bytes) together, and the script; the decoder made far more of the response than its size", limit, pythonRequestFactor, fetch.ResponseLimit(c), output)
}

// errPythonRequestTooLarge marks the error of a request longer than its
// limit, which runPython says the script did not run for.
var errPythonRequestTooLarge = errors.New("python request exceeds limit")

// request writes the request line that has a worker run script: the
// response, with its body once, and the data, unless it is the body. The
// line is the encoder's buffer, and is overwritten by its next request.
func (e *pythonEncoder) request(mode, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) ([]byte, error) {
	data := pythonScriptData(d)
	isBody := false
	if text, ok := data.(string); ok && text == string(r.Body) {
		isBody = true
	}
	e.nul, e.over, e.limit = false, false, pythonRequestLimit(script, c)
	dst := append(e.buf[:0], `{"mode":`...)
	dst = appendPythonString(dst, mode, &e.nul)
	dst = append(dst, `,"script":`...)
	dst = appendPythonString(dst, script, &e.nul)
	dst = append(dst, `,"data":`...)
	written := true
	rows, isRows := data.(*decode.CSVRows)
	switch {
	case isBody:
		dst = append(dst, `null,"data_is_body":true`...)
	case isRows && rows.Named():
		// The rows by header are sent as the header's columns once and
		// each row's cells, and the worker makes each row the dict a
		// script reads (csvTable).
		dst = e.csvTable(dst, rows)
		dst = append(dst, `,"data_is_csv":true`...)
	default:
		e.members = e.members[:0]
		dst, written = e.value(dst, data, 0)
	}
	if e.over {
		clear(e.members[:cap(e.members)])
		e.buf = dst[:0]
		return nil, model.MarkError(pythonRequestTooLarge(e.limit, c), errPythonRequestTooLarge)
	}
	dst = append(dst, `,"response":{"status_code":`...)
	switch status := r.Status().(type) {
	case nil:
		dst = append(dst, "null"...)
	case int:
		dst = strconv.AppendInt(dst, int64(status), 10)
	default:
		written = false
	}
	dst = append(dst, `,"headers":`...)
	dst = appendPythonHeaders(dst, r.Headers, &e.nul)
	dst = append(dst, `,"body":`...)
	dst = appendPythonString(dst, r.Body, &e.nul)
	dst = append(dst, `},"target":`...)
	dst = appendPythonString(dst, r.Target, &e.nul)
	dst = append(dst, `,"collector":`...)
	dst = appendPythonString(dst, c.Name, &e.nul)
	dst = append(dst, '}')
	e.buf = dst
	if written && len(dst) > e.limit {
		return nil, model.MarkError(pythonRequestTooLarge(e.limit, c), errPythonRequestTooLarge)
	}
	if !written {
		// What was being written is not kept from the garbage collector.
		clear(e.members[:cap(e.members)])
		return pythonRequestMarshalled(mode, script, d, r, c)
	}
	// A string that is a marker of a float that is not finite is rewritten
	// as it always was, wherever it stands; only a request that holds one
	// is, which copies it. A marker starts with a NUL, so a request none of
	// whose strings holds one is not searched: looking through a megabyte
	// for the marker took as long as writing it.
	if e.nul && bytes.Contains(dst, nonFiniteJSONMarker) {
		return []byte(nonFiniteJSON.Replace(string(dst))), nil
	}
	return dst, nil
}

// pythonRequestMarshalled is the request line as json.Marshal writes it,
// for data the encoder does not write itself: a value of a type no decoder
// makes.
func pythonRequestMarshalled(mode, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) ([]byte, error) {
	input := pythonInput{Mode: mode, Script: script, Target: r.Target, Collector: c.Name, Response: pythonResponse{StatusCode: r.Status(), Headers: r.Headers, Body: string(r.Body)}}
	if data := pythonScriptData(d); dataIsBody(data, input.Response.Body) {
		input.DataIsBody = true
	} else {
		input.Data = withNonFiniteMarkers(data)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if limit := pythonRequestLimit(script, c); len(payload) > limit {
		return nil, model.MarkError(pythonRequestTooLarge(limit, c), errPythonRequestTooLarge)
	}
	// Only a request that holds a marker is rewritten, which copies it.
	if bytes.Contains(payload, nonFiniteJSONMarker) {
		payload = []byte(nonFiniteJSON.Replace(string(payload)))
	}
	return payload, nil
}

// value writes v as json.Marshal writes it, but for a float that is not
// finite, which is written as the token Python's json reads as that float,
// as the request always carried it. It reports false for a value it does
// not write, of a type no decoder makes.
func (e *pythonEncoder) value(dst []byte, v any, depth int) ([]byte, bool) {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...), true
	case string:
		return appendPythonString(dst, x, &e.nul), true
	case int:
		return strconv.AppendInt(dst, int64(x), 10), true
	case float64:
		return appendPythonFloat(dst, x), true
	case bool:
		return strconv.AppendBool(dst, x), true
	case *big.Int:
		if x == nil {
			return append(dst, "null"...), true
		}
		return x.Append(dst, 10), true
	case []any:
		// A list that is nil is an empty one, and so is an object: the
		// request was written from a copy of data, which had none that is
		// nil.
		if depth >= pythonEncoderDepth {
			return dst, false
		}
		dst = append(dst, '[')
		for i, element := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			if len(dst) > e.limit {
				e.over = true
				return dst, false
			}
			var ok bool
			if dst, ok = e.value(dst, element, depth+1); !ok {
				return dst, false
			}
		}
		return append(dst, ']'), true
	case *decode.CSVRows:
		// The csv decoder's rows without a header, written as the list of
		// rows they stand for (decode.CSVRows.Document) without making it:
		// a list of each row's fields. Rows by header are written as their
		// table where they are the data (csvTable), and are nowhere else.
		if depth+2 > pythonEncoderDepth || x.Named() {
			return dst, false
		}
		return e.csvRows(dst, x)
	case map[string]any:
		if depth >= pythonEncoderDepth {
			return dst, false
		}
		// The members are sorted by key, as json.Marshal sorts them, where
		// the members of the objects around this one are kept, and are
		// read from there by their place: writing a member's value may move
		// them all to a larger slice.
		first := len(e.members)
		for key, value := range x {
			e.members = append(e.members, pythonMember{key, value})
		}
		slices.SortFunc(e.members[first:], func(a, b pythonMember) int { return strings.Compare(a.key, b.key) })
		last := len(e.members)
		dst = append(dst, '{')
		for i := first; i < last; i++ {
			if i > first {
				dst = append(dst, ',')
			}
			if len(dst) > e.limit {
				e.over = true
				return dst, false
			}
			dst = appendPythonString(dst, e.members[i].key, &e.nul)
			dst = append(dst, ':')
			var ok bool
			if dst, ok = e.value(dst, e.members[i].value, depth+1); !ok {
				return dst, false
			}
		}
		clear(e.members[first:last])
		e.members = e.members[:first]
		return append(dst, '}'), true
	}
	return dst, false
}

// csvRows writes the csv decoder's rows read without a header as
// json.Marshal writes the list of rows they stand for: a list of each row's
// fields. It reports false for rows longer written than the request may be.
func (e *pythonEncoder) csvRows(dst []byte, rows *decode.CSVRows) ([]byte, bool) {
	dst = append(dst, '[')
	for i := range rows.Len() {
		if i > 0 {
			dst = append(dst, ',')
		}
		if len(dst) > e.limit {
			e.over = true
			return dst, false
		}
		dst = e.cells(dst, rows.Row(i))
	}
	return append(dst, ']'), true
}

// csvTable writes the csv decoder's rows read by header as the table they
// are: {"columns": [...], "places": [...], "rows": [[...], ...]}, the names
// of the header's columns in the order of their bytes, the place of each in
// a row, and each row's cells as its line holds them. The worker makes of
// it the list of rows a script reads, a dict of every column the header
// names to its cell, "" where the row is short of it, as json.loads made of
// that list written out (decode.CSVRows.Document). Written out, that list
// is as long as the header times the rows, which a body of a long header
// and many short lines makes far more than its size; the table is as long
// as the body. It sets over for a table longer than the request may be.
func (e *pythonEncoder) csvTable(dst []byte, rows *decode.CSVRows) []byte {
	columns := rows.Columns()
	dst = append(dst, `{"columns":[`...)
	for j, column := range columns {
		if j > 0 {
			dst = append(dst, ',')
		}
		dst = appendPythonString(dst, column.Name, &e.nul)
	}
	dst = append(dst, `],"places":[`...)
	for j, column := range columns {
		if j > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendInt(dst, int64(column.Place), 10)
	}
	dst = append(dst, `],"rows":[`...)
	for i := range rows.Len() {
		if i > 0 {
			dst = append(dst, ',')
		}
		if len(dst) > e.limit {
			e.over = true
			return dst
		}
		dst = e.cells(dst, rows.Row(i))
	}
	return append(dst, "]}"...)
}

// cells writes a row's cells as a list of strings.
func (e *pythonEncoder) cells(dst []byte, row []string) []byte {
	dst = append(dst, '[')
	for j, field := range row {
		if j > 0 {
			dst = append(dst, ',')
		}
		dst = appendPythonString(dst, field, &e.nul)
	}
	return append(dst, ']')
}

// appendPythonHeaders writes a response's headers as json.Marshal writes a
// map of lists of strings: sorted by name.
func appendPythonHeaders(dst []byte, headers map[string][]string, nul *bool) []byte {
	if headers == nil {
		return append(dst, "null"...)
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	dst = append(dst, '{')
	for i, name := range names {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendPythonString(dst, name, nul)
		dst = append(dst, ':')
		values := headers[name]
		if values == nil {
			dst = append(dst, "null"...)
			continue
		}
		dst = append(dst, '[')
		for j, value := range values {
			if j > 0 {
				dst = append(dst, ',')
			}
			dst = appendPythonString(dst, value, nul)
		}
		dst = append(dst, ']')
	}
	return append(dst, '}')
}

// appendPythonFloat writes a float as json.Marshal writes it, as JavaScript
// does: without an exponent from a millionth up to 1e21, and with one, of
// as few digits as it has, beyond. One that is not finite is written as the
// token Python's json reads it from.
func appendPythonFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, "NaN"...)
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...)
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// e-09 is written e-9.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

// pythonStringSafe says which bytes below 0x80 json.Marshal writes in a
// string as they are: all but the control characters, the quote, the
// backslash and, as it escapes what a browser could read as markup, <, >
// and &.
var pythonStringSafe = func() (safe [utf8.RuneSelf]bool) {
	for c := byte(' '); c < utf8.RuneSelf; c++ {
		safe[c] = c != '"' && c != '\\' && c != '<' && c != '>' && c != '&'
	}
	return safe
}()

// pythonReplacement is what json.Marshal writes for a byte of a string that
// is not UTF-8: U+FFFD, as its escape or as the character itself, which
// depends on the Go release the exporter is built with. A script reads the
// same character of either, and the request is written as the json.Marshal
// of this build writes it, so that the two are one line on every release.
var pythonReplacement = func() string {
	quoted, err := json.Marshal("\xff")
	if err != nil || len(quoted) < 2 {
		return `\ufffd`
	}
	return string(quoted[1 : len(quoted)-1])
}()

// appendPythonString writes text, a string or the bytes of a body, as the
// JSON string json.Marshal writes of it: a byte that is not UTF-8 as
// U+FFFD, and U+2028 and U+2029 escaped. It sets nul when text holds a NUL.
func appendPythonString[Text []byte | string](dst []byte, text Text, nul *bool) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(text); {
		if c := text[i]; c < utf8.RuneSelf {
			if pythonStringSafe[c] {
				i++
				continue
			}
			dst = append(dst, text[start:i]...)
			switch c {
			case '\\', '"':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				if c == 0 {
					*nul = true
				}
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			start = i
			continue
		}
		// Up to four bytes are one character; converting so few to a
		// string allocates nothing.
		r, size := utf8.DecodeRuneInString(string(text[i:min(len(text), i+utf8.UTFMax)]))
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, text[start:i]...)
			dst = append(dst, pythonReplacement...)
			i += size
			start = i
		case r == '\u2028' || r == '\u2029':
			dst = append(dst, text[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0xF])
			i += size
			start = i
		default:
			i += size
		}
	}
	dst = append(dst, text[start:]...)
	return append(dst, '"')
}
