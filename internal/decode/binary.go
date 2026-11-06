package decode

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The binary decoder reads the answer of a binary protocol — a Modbus TCP
// read, a game server's query, a device's status frame — by the layout
// response.binary writes, as a small reader of its own: its document is a
// mapping of each field's name to its value, so that a jq rule reads
// .registers[0] and a Python script data["players"], and the rules never
// take bytes apart themselves.
//
//   - The fields are read in order, each from the byte after the field
//     before it, or from its offset, counted from the start of the body,
//     when it writes one: a layout may jump ahead, or back to read a byte
//     again.
//   - u8 to u64 and i8 to i64 are integers of 1 to 8 bytes, unsigned and
//     two's complement, f32 and f64 IEEE 754 floats, each in the field's
//     byte order, or the layout's, big by default, as network protocols
//     write numbers. An integer is an int in the document, as the json
//     decoder makes a whole number, but for a u64 past what an int holds,
//     which is a float64, as a float is.
//   - string is length bytes of text, and cstring the text up to a NUL byte,
//     the NUL read but not kept, at most length bytes when length is set:
//     bytes that are not UTF-8 are each read as U+FFFD, since a document
//     holds text. skip passes length bytes over and is in no document.
//   - count makes the field a list of that many values of its type, read one
//     after the other, as a run of registers is.
//   - A field that runs past the end of the body fails the decode, naming the
//     field and the bytes the body has; bytes after the last field are left
//     unread, as an answer longer than its layout often is.

// decodeBinary decodes body by layout.
func decodeBinary(body []byte, layout model.BinaryConfig) (map[string]any, error) {
	document := map[string]any{}
	at := 0
	for i, f := range layout.Fields {
		if f.Offset != nil {
			at = *f.Offset
		}
		order := binaryOrder(f.ByteOrder, layout.ByteOrder)
		name := f.Name
		if name == "" {
			name = "skip " + strconv.Itoa(i+1)
		}
		need := func(n int) error {
			if at < 0 || n > len(body)-at {
				return model.Errorf("response.binary field %s needs %d bytes from byte %d, and the answer is %d bytes", model.Quoted([]byte(name)), n, at, len(body))
			}
			return nil
		}
		switch f.Type {
		case "skip":
			if err := need(f.Length); err != nil {
				return nil, err
			}
			at += f.Length
			continue
		case "string":
			if err := need(f.Length); err != nil {
				return nil, err
			}
			document[f.Name] = strings.ToValidUTF8(string(body[at:at+f.Length]), "�")
			at += f.Length
			continue
		case "cstring":
			end := len(body)
			if f.Length > 0 {
				end = min(end, at+f.Length)
			}
			if at >= len(body) {
				return nil, need(1)
			}
			nul := bytes.IndexByte(body[at:end], 0)
			if nul < 0 {
				return nil, model.Errorf("response.binary field %s is text up to a NUL byte from byte %d, and no NUL comes in the %d bytes after it", model.Quoted([]byte(name)), at, end-at)
			}
			document[f.Name] = strings.ToValidUTF8(string(body[at:at+nul]), "�")
			at += nul + 1
			continue
		}
		size := binarySize(f.Type)
		n := max(f.Count, 1)
		if err := need(size * n); err != nil {
			return nil, err
		}
		values := make([]any, n)
		for j := range values {
			values[j] = binaryNumber(f.Type, order, body[at:at+size])
			at += size
		}
		if f.Count > 0 {
			document[f.Name] = values
		} else {
			document[f.Name] = values[0]
		}
	}
	return document, nil
}

// binaryOrder is a field's byte order: its own, or else the layout's, or
// else big-endian.
func binaryOrder(field, layout string) binary.ByteOrder {
	order := field
	if order == "" {
		order = layout
	}
	if order == "little" {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

// binarySize is how many bytes a number of the type is.
func binarySize(kind string) int {
	switch kind {
	case "u8", "i8":
		return 1
	case "u16", "i16":
		return 2
	case "u32", "i32", "f32":
		return 4
	}
	return 8
}

// binaryNumber reads a number of the type from b, which is as long as the
// type is: an integer as an int, as the json decoder makes a whole number,
// but a u64 past what an int holds, which is a float64, and a float as a
// float64.
func binaryNumber(kind string, order binary.ByteOrder, b []byte) any {
	switch kind {
	case "u8":
		return int(b[0])
	case "i8":
		return int(int8(b[0])) //nolint:gosec // G115: a byte read as two's complement, as the type says
	case "u16":
		return int(order.Uint16(b))
	case "i16":
		return int(int16(order.Uint16(b))) //nolint:gosec // G115: as above
	case "u32":
		return int(order.Uint32(b))
	case "i32":
		return int(int32(order.Uint32(b))) //nolint:gosec // G115: as above
	case "f32":
		return float64(math.Float32frombits(order.Uint32(b)))
	case "u64":
		if v := order.Uint64(b); v <= math.MaxInt64 {
			return int(v)
		}
		return float64(order.Uint64(b))
	case "i64":
		return int(int64(order.Uint64(b))) //nolint:gosec // G115: as above
	}
	return math.Float64frombits(order.Uint64(b))
}
