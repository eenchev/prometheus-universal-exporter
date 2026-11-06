package decode

import (
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// decodeBinaryBody is a binary decode of body by fields, the layout's byte
// order order.
func decodeBinaryBody(t *testing.T, body []byte, order string, fields ...model.BinaryField) (*Decoded, error) {
	t.Helper()
	c := &model.Collector{Name: "b", Decoder: model.DecoderConfig{Type: "binary"}, Response: model.ResponseConfig{Binary: model.BinaryConfig{ByteOrder: order, Fields: fields}}}
	return Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{}}, c)
}

func offset(n int) *int { return &n }

// A binary answer is a mapping of its fields' names to their values, read in
// order, big-endian unless the layout or the field says little: a Modbus TCP
// answer's header, unit and registers, a list by count; integers are ints,
// signed ones read as two's complement, floats float64s; a skip passes bytes
// over, an offset jumps, back as well, a string is its length of text and a
// cstring the text up to its NUL, bytes that are no UTF-8 read as U+FFFD;
// bytes after the last field are left unread.
func TestTheBinaryDecoderReadsTheLayout(t *testing.T) {
	body := []byte{0, 1, 0, 0, 0, 7, 1, 3, 4, 0, 42, 0xff, 0xfe}
	d, err := decodeBinaryBody(t, body, "",
		model.BinaryField{Name: "transaction", Type: "u16"},
		model.BinaryField{Type: "skip", Length: 2},
		model.BinaryField{Name: "length", Type: "u16"},
		model.BinaryField{Name: "unit", Type: "u8"},
		model.BinaryField{Name: "function", Type: "u8", Offset: offset(7)},
		model.BinaryField{Name: "bytes", Type: "u8"},
		model.BinaryField{Name: "registers", Type: "u16", Count: 2},
		model.BinaryField{Name: "signed", Type: "i16", Offset: offset(11)},
		model.BinaryField{Name: "little", Type: "u16", Offset: offset(0), ByteOrder: "little"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"transaction": 1, "length": 7, "unit": 1, "function": 3, "bytes": 4, "registers": []any{42, 65534}, "signed": -2, "little": 256}
	if d.Kind != "binary" || !reflect.DeepEqual(d.Data, want) {
		t.Errorf("decoded %s %#v, want %#v", d.Kind, d.Data, want)
	}
	f32 := math.Float32bits(1.5)
	body = []byte{byte(f32), byte(f32 >> 8), byte(f32 >> 16), byte(f32 >> 24), 'h', 'i', 0, 'a', 0xff, 'b', 'x', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	d, err = decodeBinaryBody(t, body, "little",
		model.BinaryField{Name: "ratio", Type: "f32"},
		model.BinaryField{Name: "name", Type: "cstring"},
		model.BinaryField{Name: "map", Type: "string", Length: 3},
		model.BinaryField{Name: "x", Type: "string", Length: 1},
		model.BinaryField{Name: "minus", Type: "i64"},
		model.BinaryField{Name: "huge", Type: "u64", Offset: offset(11)},
	)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"ratio": 1.5, "name": "hi", "map": "a�b", "x": "x", "minus": -1, "huge": float64(math.MaxUint64)}
	if !reflect.DeepEqual(d.Data, want) {
		t.Errorf("decoded %#v, want %#v", d.Data, want)
	}
}

// A field past the end of the answer fails the decode, naming it and the
// answer's length, and so does a cstring whose NUL does not come within the
// answer or its length; a charset is never applied to a binary answer.
func TestTheBinaryDecoderRefusesAShortAnswer(t *testing.T) {
	for _, c := range []struct {
		body  []byte
		field model.BinaryField
		want  string
	}{
		{[]byte{1, 2, 3}, model.BinaryField{Name: "n", Type: "u32"}, `field "n" needs 4 bytes from byte 0, and the answer is 3 bytes`},
		{[]byte{1}, model.BinaryField{Name: "r", Type: "u8", Count: 2}, `field "r" needs 2 bytes from byte 0`},
		{[]byte{1}, model.BinaryField{Name: "s", Type: "string", Length: 2}, `field "s" needs 2 bytes`},
		{[]byte{1}, model.BinaryField{Type: "skip", Length: 2}, `field "skip 1" needs 2 bytes`},
		{[]byte{1}, model.BinaryField{Name: "n", Type: "u8", Offset: offset(1)}, `field "n" needs 1 bytes from byte 1, and the answer is 1 bytes`},
		{[]byte("abc"), model.BinaryField{Name: "c", Type: "cstring"}, `field "c" is text up to a NUL byte from byte 0, and no NUL comes in the 3 bytes after it`},
		{[]byte("abc\x00"), model.BinaryField{Name: "c", Type: "cstring", Length: 2}, "no NUL comes in the 2 bytes after it"},
		{[]byte{}, model.BinaryField{Name: "c", Type: "cstring"}, `field "c" needs 1 bytes from byte 0, and the answer is 0 bytes`},
	} {
		if _, err := decodeBinaryBody(t, c.body, "", c.field); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%x %+v: %v, want %q", c.body, c.field, err, c.want)
		}
	}
	c := &model.Collector{Name: "b", Decoder: model.DecoderConfig{Type: "binary"}, Response: model.ResponseConfig{Charset: "iso-8859-1", Binary: model.BinaryConfig{Fields: []model.BinaryField{{Name: "s", Type: "string", Length: 2}}}}}
	d, err := Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte{0xe9, 'a'}, Headers: http.Header{"Content-Type": {"text/plain; charset=iso-8859-1"}}}, c)
	if err != nil || d.Data.(map[string]any)["s"] != "�a" {
		t.Errorf("a binary answer was converted from its charset: %#v, %v", d, err)
	}
}
