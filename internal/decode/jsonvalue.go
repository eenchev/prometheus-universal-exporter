package decode

import (
	"bytes"
	"errors"
	"io"
	"math/big"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The json decoder reads a body in one pass, here, into the values the
// transforms use: an object as a map[string]any, an array as a []any, text,
// true, false and null as a string, a bool and nil, and a number as
// model.Normalize makes it — a whole number an int, or a *big.Int beyond
// int64, so an ID of any length keeps every digit, or its text beyond
// model.MaxWholeNumberDigits digits; any other number a float64, or its text
// when no float64 holds it, as 1e400.
//
// It was encoding/json's Decoder with UseNumber, decoding into an any, and
// then model.Normalize over what it made, and that is what it still accepts,
// refuses and returns (jsondiff_test.go compares the two): the standard
// library built every object and array through reflection, every number
// first as its text and every key as a string of its own, and Normalize
// walked the whole document a second time to rewrite the numbers. Half the
// time of a jq probe over a large document went there. The grammar is the
// one of RFC 8259 as encoding/json reads it:
//
//   - One value, with nothing but spaces, tabs and line ends before and
//     after it. An empty body is io.EOF, a body that ends inside its value
//     io.ErrUnexpectedEOF, and any other error says the line and column of
//     the byte that cannot be there.
//   - Arrays and objects nest 10,000 deep at most.
//   - Of two members of an object with one key, the later is kept.
//   - Bytes that are not UTF-8 inside a string are each read as U+FFFD, as
//     is a \u escape of half a surrogate pair; a control character must be
//     escaped, and the only escapes are \" \\ \/ \b \f \n \r \t and \uXXXX.
//   - A number has no leading zero, no leading +, and a digit on each side
//     of its point and after its e.

// MaxDepth is how deep the lists and mappings of a decoded value nest at
// most, one inside another, whichever decoder made it: a response whose
// value would nest deeper is refused when it is decoded, saying so. It is
// encoding/json's limit on arrays and objects, which the json decoder had
// from the start, and one limit for every format so that what reads a value
// need not ask which decoder made it. What has to follow a value as deep as
// it goes takes the depth from here: a value is handed to a Python script,
// and what a script leaves is read back, to this depth
// (transform/pythonrequest.go, pythonanswer.go, pythonworker.go), with no
// limit of the hand-over's own for a response to decode within and then
// fail on. Each decoder is within it:
//
//   - JSON counts the arrays and objects around what it is reading, and
//     refuses the document at one more (enter).
//   - YAML refuses a document whose sequences and mappings nest deeper, an
//     alias counted as what it stands for, before anything decodes it
//     (yamlkeys.go, yamlTooDeep). The library's parser does not keep a
//     document within the limit: it bounds what is nested by indentation and
//     what is nested in brackets apart, at 10,000 levels each (scannerc.go,
//     max_indents and max_flow_level), and a level of indentation is two
//     collections where a mapping's value is a sequence written no further
//     in than its key, so a document without an alias parses nested up to
//     30,000 deep; and it bounds what aliases expand to by its size, not by
//     its depth.
//   - CSV is a list of rows, each a list or a mapping of its fields;
//     Graphite's series and a Prometheus exposition as a script reads it
//     nest a few levels, of the exporter's own making.
//   - Text is text, and so are HTML and XML to a script; as documents their
//     elements nest MaxXMLDepth deep at most.
//   - What a pre-script leaves in data is read by JSONValue, to this depth,
//     and the worker refuses to write it nested deeper (pythonworker.go,
//     deep_check).
const MaxDepth = 10000

// errJSONTrailingData is the error of a body that goes on after its value.
// Only whitespace may follow it: a second record, as in NDJSON, or anything
// else would otherwise be dropped unseen, and the collector would report the
// first record as the whole answer.
var errJSONTrailingData = errors.New("trailing data after the JSON value (NDJSON, one value per line, is not supported)")

// decodeJSON decodes body, which must be one JSON value.
func decodeJSON(body []byte) (any, error) {
	d := jsonDecoder{data: body}
	return d.document()
}

// sniffJSON decodes body as decodeJSON does, for the detection of a format
// (detectFormat), and reports whether body is the JSON the detection takes
// for JSON: one value, with no number in it beyond a float64. The detection
// asked encoding/json's Unmarshal, which reads every number into a float64
// and refuses the document over one that none holds, 1e400 or a whole
// number past 1.8e308, and what it took for JSON then it takes for JSON
// now. The value is what decodeJSON returns for body, so the decode that
// follows a detection need not read the body a second time.
func sniffJSON(body []byte) (any, bool) {
	d := jsonDecoder{data: body, sniffing: true}
	v, err := d.document()
	return v, err == nil && !d.beyondFloat
}

// JSONValue reads the JSON value body starts with as the json decoder reads
// a document, and returns it and what of body follows it. It is how the
// answer of a Python worker is read (transform/pythonanswer.go): what a
// pre-script leaves in data is a value inside the answer, and is read as a
// response's JSON is, nested MaxDepth deep at most whatever lies around it
// in the answer: a script may leave what it was given, as deep as a decoder
// made it.
func JSONValue(body []byte) (value any, rest []byte, err error) {
	d := jsonDecoder{data: body}
	d.space()
	d.sizeCache()
	if value, err = d.value(); err != nil {
		return nil, nil, err
	}
	return value, body[d.pos:], nil
}

// document reads the decoder's body, which must be one JSON value.
func (d *jsonDecoder) document() (any, error) {
	body := d.data
	d.space()
	if d.pos == len(body) {
		return nil, io.EOF
	}
	d.sizeCache()
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	d.space()
	if d.pos != len(body) {
		return nil, errJSONTrailingData
	}
	return v, nil
}

// sizeCache makes the cache of short strings (remembered), sized by the
// body, so that a small answer does not pay for a table larger than itself:
// a slot is 32 bytes, and there is one for every 64 bytes of body, 512 at
// most.
func (d *jsonDecoder) sizeCache() {
	slots := uint32(16)
	for slots < 512 && int(slots)*64 < len(d.data) {
		slots *= 2
	}
	d.cache, d.cacheMask = make([]jsonCached, slots), slots-1
}

// jsonDecoder is the state of one decodeJSON.
type jsonDecoder struct {
	data []byte
	// pos is the next byte to read.
	pos int
	// depth is how many arrays and objects the value being read is inside.
	depth int
	// values holds the elements of the arrays, and the member values of the
	// objects, being read, innermost last, and names the keys of those
	// members. An array or an object is made when its closing bracket is
	// read, of exactly the size it has, rather than grown an element at a
	// time: growing a slice copies it again and again, and a map of more
	// than eight keys is rebuilt as it fills.
	values []any
	names  []string
	// unquoted is where a string with escapes, or with bytes that are not
	// UTF-8, is written out before it is made a string.
	unquoted []byte
	// cache remembers short strings (remembered); it has a power of two of
	// slots, and cacheMask is one less.
	cache     []jsonCached
	cacheMask uint32
	// sniffing says the body is read to detect its format (sniffJSON), and
	// beyondFloat then that it holds a number no float64 does.
	sniffing    bool
	beyondFloat bool
}

// jsonCached is a short string the document held, and that string as an any
// once a value needed it so.
type jsonCached struct {
	text  string
	boxed any
}

// jsonCachedLength is the longest string the cache keeps: keys, and the
// values that repeat, an enumeration's or a unit's, are short.
const jsonCachedLength = 32

// remembered returns the cache's slot for text, holding it. The items of an
// array mostly have the same keys, and many of the same values: making one
// string of each, where encoding/json made one for every occurrence, saves
// most of the strings of a large document. The cache is a table of fixed
// size in which a string has one slot, by its hash, and replaces what the
// slot held, so a document of ten thousand different IDs costs it nothing
// but the lookups, and the keys those IDs push out come back at their next
// use.
func (d *jsonDecoder) remembered(text []byte) *jsonCached {
	// FNV-1a, which needs no seed: the same document makes the same
	// strings, and allocates as much, on every run.
	h := uint32(2166136261)
	for _, c := range text {
		h = (h ^ uint32(c)) * 16777619
	}
	slot := &d.cache[(h^h>>15)&d.cacheMask]
	// A slot never used holds the empty string, which is what it then is
	// the slot of. The comparison converts nothing: the compiler compares
	// the bytes in place.
	if slot.text != string(text) {
		*slot = jsonCached{text: string(text)}
	}
	return slot
}

// name is text as an object's key.
func (d *jsonDecoder) name(text []byte) string {
	if len(text) > jsonCachedLength {
		return string(text)
	}
	return d.remembered(text).text
}

// text is text as a value.
func (d *jsonDecoder) text(text []byte) any {
	if len(text) > jsonCachedLength {
		return string(text)
	}
	slot := d.remembered(text)
	if slot.boxed == nil {
		slot.boxed = slot.text
	}
	return slot.boxed
}

// space passes over whitespace.
func (d *jsonDecoder) space() {
	for d.pos < len(d.data) {
		switch d.data[d.pos] {
		case ' ', '\t', '\r', '\n':
			d.pos++
		default:
			return
		}
	}
}

// invalid is the error of the byte at pos, which cannot be where it is;
// where says where that is, as in "after object key". Past the end of the
// body it is the error of a body that ends too early.
func (d *jsonDecoder) invalid(pos int, where string) error {
	if pos >= len(d.data) {
		return io.ErrUnexpectedEOF
	}
	line, column := d.position(pos)
	return model.Errorf("invalid character %s %s, at line %d, column %d", quoteJSONByte(d.data[pos]), where, line, column)
}

// position says where pos is, for an error: its line and, counted in bytes
// from 1, its column, which in a document written on one line is how far
// into the document it is. It is no part of what the failure is to the log
// (model.SameFailureText).
func (d *jsonDecoder) position(pos int) (line, column model.Position) {
	before := d.data[:pos]
	return model.Position(1 + bytes.Count(before, []byte("\n"))), model.Position(pos - bytes.LastIndexByte(before, '\n'))
}

// quoteJSONByte quotes c for an error as encoding/json does: in single
// quotes, escaped as Go escapes it.
func quoteJSONByte(c byte) string {
	switch c {
	case '\'':
		return `'\''`
	case '"':
		return `'"'`
	}
	quoted := strconv.Quote(string(rune(c)))
	return "'" + quoted[1:len(quoted)-1] + "'"
}

// value reads the value that starts at pos, which is not whitespace.
func (d *jsonDecoder) value() (any, error) {
	if d.pos >= len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	switch c := d.data[d.pos]; {
	case c == '"':
		text, err := d.quoted()
		if err != nil {
			return nil, err
		}
		return d.text(text), nil
	case c == '{':
		return d.object()
	case c == '[':
		return d.array()
	case c == '-' || c >= '0' && c <= '9':
		return d.number()
	case c == 't':
		return true, d.literal("true")
	case c == 'f':
		return false, d.literal("false")
	case c == 'n':
		return nil, d.literal("null")
	}
	return nil, d.invalid(d.pos, "looking for beginning of value")
}

// literal reads word, whose first byte is at pos.
func (d *jsonDecoder) literal(word string) error {
	for i := 1; i < len(word); i++ {
		if at := d.pos + i; at >= len(d.data) || d.data[at] != word[i] {
			return d.invalid(at, "in literal "+word+" (expecting "+quoteJSONByte(word[i])+")")
		}
	}
	d.pos += len(word)
	return nil
}

// enter counts one more array or object around what is read next.
func (d *jsonDecoder) enter() error {
	if d.depth++; d.depth > MaxDepth {
		line, column := d.position(d.pos)
		return model.Errorf("arrays and objects nested more than %d deep, at line %d, column %d", MaxDepth, line, column)
	}
	return nil
}

// array reads the array whose bracket is at pos.
func (d *jsonDecoder) array() (any, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	d.pos++
	d.space()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		d.depth--
		return []any{}, nil
	}
	base := len(d.values)
	for {
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		d.values = append(d.values, v)
		d.space()
		if d.pos < len(d.data) && d.data[d.pos] == ',' {
			d.pos++
			d.space()
			continue
		}
		if d.pos < len(d.data) && d.data[d.pos] == ']' {
			break
		}
		return nil, d.invalid(d.pos, "after array element")
	}
	d.pos++
	d.depth--
	out := make([]any, len(d.values)-base)
	copy(out, d.values[base:])
	d.values = d.values[:base]
	return out, nil
}

// object reads the object whose brace is at pos.
func (d *jsonDecoder) object() (any, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	d.pos++
	d.space()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		d.depth--
		return map[string]any{}, nil
	}
	base, firstName := len(d.values), len(d.names)
	for {
		if d.pos >= len(d.data) || d.data[d.pos] != '"' {
			return nil, d.invalid(d.pos, "looking for beginning of object key string")
		}
		text, err := d.quoted()
		if err != nil {
			return nil, err
		}
		// The key is made a string before its value is read: the value may
		// be a string with escapes, written where the key's was.
		name := d.name(text)
		d.space()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return nil, d.invalid(d.pos, "after object key")
		}
		d.pos++
		d.space()
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		d.names = append(d.names, name)
		d.values = append(d.values, v)
		d.space()
		if d.pos < len(d.data) && d.data[d.pos] == ',' {
			d.pos++
			d.space()
			continue
		}
		if d.pos < len(d.data) && d.data[d.pos] == '}' {
			break
		}
		return nil, d.invalid(d.pos, "after object key:value pair")
	}
	d.pos++
	d.depth--
	names, values := d.names[firstName:], d.values[base:]
	// In the order they were written, so that of two members with one key
	// the later is the one kept.
	out := make(map[string]any, len(names))
	for i, name := range names {
		out[name] = values[i]
	}
	d.names, d.values = d.names[:firstName], d.values[:base]
	return out, nil
}

// number reads the number that starts at pos, with a minus sign or a digit.
func (d *jsonDecoder) number() (any, error) {
	data, start := d.data, d.pos
	i := start
	if data[i] == '-' {
		i++
	}
	switch {
	case i < len(data) && data[i] == '0':
		// A zero is the whole integer part: 01 is a 0 and then a 1 that
		// cannot follow it.
		i++
	case i < len(data) && data[i] >= '1' && data[i] <= '9':
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return nil, d.invalid(i, "in numeric literal")
	}
	digits := data[start:i]
	whole := true
	if i < len(data) && data[i] == '.' {
		whole = false
		i++
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return nil, d.invalid(i, "after decimal point in numeric literal")
		}
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		whole = false
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return nil, d.invalid(i, "in exponent of numeric literal")
		}
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	d.pos = i
	text := data[start:i]
	if !whole {
		// A float64, or, when the number is beyond one, its text, which is
		// what model.Normalize leaves of a json.Number it cannot read.
		if f, err := strconv.ParseFloat(string(text), 64); err == nil {
			return f, nil
		}
		d.beyondFloat = true
		return string(text), nil
	}
	// Eighteen digits are below 2^63 whatever they are, so they are added
	// up here; longer ones are left to strconv to tell an int64 from a
	// number beyond it.
	if negative := digits[0] == '-'; len(digits) <= 18 || negative && len(digits) <= 19 {
		var n int64
		for _, c := range digits {
			if c != '-' {
				n = n*10 + int64(c-'0')
			}
		}
		if negative {
			n = -n
		}
		return int(n), nil
	}
	if n, err := strconv.ParseInt(string(text), 10, 64); err == nil {
		return int(n), nil
	}
	// A whole number of up to 308 digits is below the largest float64; a
	// longer one is asked about only by a detection, which alone needs to
	// know.
	if d.sniffing && len(digits) > 308 {
		if _, err := strconv.ParseFloat(string(text), 64); err != nil {
			d.beyondFloat = true
		}
	}
	// Beyond model.MaxWholeNumberDigits the digits are kept as text: a
	// *big.Int of them would take time growing with their square.
	if len(text) > model.MaxWholeNumberDigits {
		return string(text), nil
	}
	n, _ := new(big.Int).SetString(string(text), 10)
	return n, nil
}

// quoted reads the string whose opening quote is at pos, and returns what
// it says: a part of the body when it is written without escapes and is
// UTF-8, which nearly every string is, and otherwise d.unquoted, which the
// next string with escapes overwrites.
func (d *jsonDecoder) quoted() ([]byte, error) {
	data := d.data
	start := d.pos + 1
	i := start
	for i < len(data) {
		c := data[i]
		if c == '"' {
			d.pos = i + 1
			return data[start:i], nil
		}
		if c == '\\' || c < ' ' {
			break
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		i += size
	}
	out := append(d.unquoted[:0], data[start:i]...)
	for i < len(data) {
		c := data[i]
		switch {
		case c == '"':
			d.pos = i + 1
			d.unquoted = out
			return out, nil
		case c < ' ':
			return nil, d.invalid(i, "in string literal")
		case c == '\\':
			i++
			if i >= len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			switch data[i] {
			case '"', '\\', '/':
				out = append(out, data[i])
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				for j := i + 1; j <= i+4; j++ {
					if j >= len(data) || !isHexDigit(data[j]) {
						return nil, d.invalid(j, "in \\u hexadecimal character escape")
					}
				}
				r := hexRune(data[i+1 : i+5])
				i += 4
				if utf16.IsSurrogate(r) {
					// The two halves of a pair are one character. Half a
					// pair, or two halves the wrong way round, is U+FFFD,
					// and what follows it is read for itself.
					if pair := utf16.DecodeRune(r, escapedRune(data[i+1:])); pair != utf8.RuneError {
						r = pair
						i += 6
					} else {
						r = utf8.RuneError
					}
				}
				out = utf8.AppendRune(out, r)
			default:
				return nil, d.invalid(i, "in string escape code")
			}
			i++
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			// A byte that is not UTF-8 is U+FFFD, each one by itself.
			r, size := utf8.DecodeRune(data[i:])
			out = utf8.AppendRune(out, r)
			i += size
		}
	}
	return nil, io.ErrUnexpectedEOF
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// hexRune reads four hexadecimal digits.
func hexRune(digits []byte) rune {
	var r rune
	for _, c := range digits {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		default:
			c -= 'A' - 10
		}
		r = r<<4 | rune(c)
	}
	return r
}

// escapedRune reads the \uXXXX that s starts with, or returns -1 when it
// starts with none.
func escapedRune(s []byte) rune {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return -1
	}
	for _, c := range s[2:6] {
		if !isHexDigit(c) {
			return -1
		}
	}
	return hexRune(s[2:6])
}
