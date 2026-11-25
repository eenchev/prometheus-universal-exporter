package decode

import (
	"math"
	"math/big"
	"strconv"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The influx decoder reads InfluxDB line protocol: the text InfluxDB is
// written with and Telegraf's influx output writes. It has one point a line,
//
//	measurement,tag=value,tag=value field=value,field=value timestamp
//
// read as InfluxDB's own parser reads it. No InfluxDB module is a
// dependency, as the grammar is a page. The document is the list of the
// points, in the order of their lines, each a mapping of its measurement,
// its tags, its fields and its timestamp, so that a jq rule reads it as it
// reads a JSON array of objects.
//
//   - A line of only blanks is skipped, and so is a line that starts with
//     #, a comment. A line ends at a line feed (a carriage return before it
//     is dropped), except a line feed inside a quoted field value, which is
//     part of the value.
//   - The measurement runs to the first comma or blank with no backslash
//     before it; \, and \ followed by a blank write those in it. The tags
//     follow, each key=value after a comma, with \, \= and \ followed by a
//     blank for those characters in a key and in a value. A point may have
//     no tags. Tags are text.
//   - The fields follow after blanks: at least one key=value, separated by
//     commas. A value is a float, written as a number; an integer, a whole
//     number followed by i; an unsigned integer, one followed by u; a
//     boolean, t, T, true, True, TRUE or the same of false; or text in double
//     quotes, with \" and \\ for a quote and a backslash in it. A float is
//     a number here, and an integer is a number that keeps every digit, as
//     the json decoder keeps a whole number's.
//   - The timestamp follows after blanks: a whole number, as it is written.
//     The line does not say its precision (nanoseconds unless the writer
//     chose another), so the document does not either. A point without one
//     has null.
//   - A key written twice in a point's tags or in its fields keeps the
//     later value.
//   - An error names the line and column and what cannot be there, never
//     more of the body.

// decodeInflux decodes body as InfluxDB line protocol.
func decodeInflux(body []byte) ([]any, error) {
	d := influxDecoder{body: body, line: 1}
	points := []any{}
	for d.at < len(d.body) {
		point, err := d.point()
		if err != nil {
			return nil, err
		}
		if point != nil {
			points = append(points, point)
		}
	}
	return points, nil
}

// influxDecoder reads a body point by point: at is the byte it is at, line
// the line that is on, and start where that line began.
type influxDecoder struct {
	body  []byte
	at    int
	line  model.Position
	start int
	// text is where an escaped name is put together.
	text []byte
}

// unexpected is the error of the byte the decoder is at, or of the line or
// the body ending there.
func (d *influxDecoder) unexpected(where string) error {
	if d.at >= len(d.body) || d.body[d.at] == '\n' || d.body[d.at] == '\r' && d.at+1 < len(d.body) && d.body[d.at+1] == '\n' {
		return model.Errorf("line %d: the line ends %s", d.line, where)
	}
	return model.Errorf("line %d, column %d: unexpected %s %s", d.line, model.Position(d.at-d.start+1), quoteJSONByte(d.body[d.at]), where)
}

// blanks passes over spaces and tabs, and reports whether there were any.
func (d *influxDecoder) blanks() bool {
	at := d.at
	for d.at < len(d.body) && (d.body[d.at] == ' ' || d.body[d.at] == '\t') {
		d.at++
	}
	return d.at > at
}

// lineEnd reports whether the decoder is at the end of the line, and
// passes over it: the body's end, a line feed, or a carriage return and
// one.
func (d *influxDecoder) lineEnd() bool {
	switch {
	case d.at >= len(d.body):
		return true
	case d.body[d.at] == '\r' && d.at+1 == len(d.body):
		// A carriage return that ends the body ends its last line.
	case d.body[d.at] == '\r' && d.body[d.at+1] == '\n':
		d.at++
	case d.body[d.at] != '\n':
		return false
	}
	d.at++
	d.line++
	d.start = d.at
	return true
}

// name reads a measurement, a tag's key or value, or a field's key: the
// bytes up to the first of stops with no backslash before it. A backslash
// before a comma, an equals sign or a blank is dropped. It ends at a blank
// and at the line's end, whatever stops holds.
func (d *influxDecoder) name(stops string) string {
	d.text = d.text[:0]
	for d.at < len(d.body) {
		c := d.body[d.at]
		if c == '\\' && d.at+1 < len(d.body) {
			if next := d.body[d.at+1]; next == ',' || next == '=' || next == ' ' {
				d.text = append(d.text, next)
				d.at += 2
				continue
			}
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' && d.at+1 < len(d.body) && d.body[d.at+1] == '\n' {
			break
		}
		stopped := false
		for i := range len(stops) {
			stopped = stopped || c == stops[i]
		}
		if stopped {
			break
		}
		d.text = append(d.text, c)
		d.at++
	}
	if utf8.Valid(d.text) {
		return string(d.text)
	}
	return string(bytes8ToValid(d.text))
}

// bytes8ToValid is b with each byte that is no UTF-8 as the replacement
// character.
func bytes8ToValid(b []byte) []byte {
	valid := make([]byte, 0, len(b)+8)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		valid = utf8.AppendRune(valid, r)
		b = b[size:]
	}
	return valid
}

// point reads the line the decoder is at: nil for a blank line and for a
// comment, and otherwise the point it holds.
func (d *influxDecoder) point() (map[string]any, error) {
	d.blanks()
	if d.lineEnd() {
		return nil, nil
	}
	if d.body[d.at] == '#' {
		for !d.lineEnd() {
			d.at++
		}
		return nil, nil
	}
	measurement := d.name(",")
	if measurement == "" {
		return nil, d.unexpected("where a measurement begins the line")
	}
	tags := map[string]any{}
	for d.at < len(d.body) && d.body[d.at] == ',' {
		d.at++
		key := d.name(",=")
		if key == "" || d.at >= len(d.body) || d.body[d.at] != '=' {
			return nil, d.unexpected("where a tag is written key=value")
		}
		d.at++
		value := d.name(",=")
		if value == "" {
			return nil, d.unexpected("where the value of tag " + strconv.Quote(key) + " is")
		}
		tags[key] = value
	}
	if !d.blanks() {
		return nil, d.unexpected("after the measurement and its tags; a blank and the fields follow them")
	}
	fields := map[string]any{}
	for {
		key := d.name(",=")
		if key == "" || d.at >= len(d.body) || d.body[d.at] != '=' {
			return nil, d.unexpected("where a field is written key=value")
		}
		d.at++
		value, err := d.value(key)
		if err != nil {
			return nil, err
		}
		fields[key] = value
		if d.at >= len(d.body) || d.body[d.at] != ',' {
			break
		}
		d.at++
	}
	point := map[string]any{"measurement": measurement, "tags": tags, "fields": fields, "timestamp": nil}
	spaced := d.blanks()
	if d.lineEnd() {
		return point, nil
	}
	if !spaced {
		return nil, d.unexpected("after the value of a field")
	}
	at := d.at
	for d.at < len(d.body) && (d.body[d.at] >= '0' && d.body[d.at] <= '9' || d.at == at && d.body[d.at] == '-') {
		d.at++
	}
	timestamp, err := strconv.ParseInt(string(d.body[at:d.at]), 10, 64)
	if err != nil {
		if d.at > at && d.body[d.at-1] != '-' {
			return nil, model.Errorf("line %d, column %d: the timestamp is beyond a 64-bit whole number", d.line, model.Position(at-d.start+1))
		}
		return nil, d.unexpected("where the timestamp is, a whole number")
	}
	point["timestamp"] = int(timestamp)
	d.blanks()
	if !d.lineEnd() {
		return nil, d.unexpected("after the timestamp, which ends the line")
	}
	return point, nil
}

// value reads the value of the field key, which the decoder is at.
func (d *influxDecoder) value(key string) (any, error) {
	if d.at < len(d.body) && d.body[d.at] == '"' {
		return d.quoted(key)
	}
	at := d.at
	for d.at < len(d.body) {
		if c := d.body[d.at]; c == ',' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		d.at++
	}
	text := string(d.body[at:d.at])
	wrong := func(what string) error {
		return model.Errorf("line %d, column %d: the value of field %s is %s; want a number, a whole number and i or u, a boolean or text in double quotes", d.line, model.Position(at-d.start+1), strconv.Quote(key), what)
	}
	switch text {
	case "":
		d.at = at
		return nil, d.unexpected("where the value of field " + strconv.Quote(key) + " is")
	case "t", "T", "true", "True", "TRUE":
		return true, nil
	case "f", "F", "false", "False", "FALSE":
		return false, nil
	}
	switch text[len(text)-1] {
	case 'i':
		n, err := strconv.ParseInt(text[:len(text)-1], 10, 64)
		if err != nil {
			return nil, wrong("no 64-bit integer")
		}
		return int(n), nil
	case 'u':
		n, err := strconv.ParseUint(text[:len(text)-1], 10, 64)
		if err != nil {
			return nil, wrong("no unsigned 64-bit integer")
		}
		if n > math.MaxInt64 {
			return new(big.Int).SetUint64(n), nil
		}
		return int(n), nil //nolint:gosec // G115: within an int64, as checked
	}
	// A float as InfluxDB writes one: digits, a point, an exponent, and no
	// name of a number that is none, which strconv would take.
	for i := range len(text) {
		if c := text[i]; (c < '0' || c > '9') && c != '.' && c != '-' && c != '+' && c != 'e' && c != 'E' {
			return nil, wrong("no value of a field")
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) {
		return nil, wrong("no number a float holds")
	}
	return f, nil
}

// quoted reads the text value of the field key, whose opening quote the
// decoder is at: up to the quote no backslash is before, a backslash
// before a quote or a backslash dropped, a line feed in it part of it.
func (d *influxDecoder) quoted(key string) (any, error) {
	line, column := d.line, model.Position(d.at-d.start+1)
	value := []byte{}
	for d.at++; d.at < len(d.body); d.at++ {
		c := d.body[d.at]
		switch {
		case c == '"':
			d.at++
			if !utf8.Valid(value) {
				value = bytes8ToValid(value)
			}
			return string(value), nil
		case c == '\\' && d.at+1 < len(d.body) && (d.body[d.at+1] == '"' || d.body[d.at+1] == '\\'):
			d.at++
			c = d.body[d.at]
		case c == '\n':
			d.line++
			d.start = d.at + 1
		}
		value = append(value, c)
	}
	return nil, model.Errorf("line %d, column %d: the text of field %s is not closed by a quote", line, column, strconv.Quote(key))
}
