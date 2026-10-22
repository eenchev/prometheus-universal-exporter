//go:build !select_request_types || request_type_grpc

package fetch

import (
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// An answer is rendered as JSON with every zero value written, which can make
// the JSON far larger than the message: an empty element of a repeated field
// is two bytes on the wire, and its every field in the JSON. The JSON's size
// is therefore counted from the message as it arrived, before the message is
// decoded, and an answer whose JSON is over the response limit is refused
// then: neither the decoding nor the rendering of an answer that is refused
// costs more than reading it. The count is a lower bound of the JSON, made as
// protojson writes it: braces, field names, separators, every field the
// answer leaves out at its zero value, integers, strings with their escapes,
// bytes in base64 and enum names exactly; floating-point numbers and the
// well-known types at their shortest. An answer the count lets through is
// rendered and measured as before, so that what the count leaves short is
// bounded too, and only by a small factor of what was read.
//
// A field the answer carries more than once is counted each time, as is a
// map key: no encoder writes either, and counting them once would need what
// the decoder does to them, which costs as much as decoding.

// answerCodec is the codec a call's answer is read with: grpc-go's proto
// codec, with the answer's JSON counted (jsonCount) before it is decoded.
// Each call has its own, which keeps the refusal for the call to report:
// grpc-go turns what Unmarshal returns into an INTERNAL status.
type answerCodec struct {
	limit    int64
	resolver anyResolver
	// over is the refusal of an answer whose JSON is over the limit.
	over error
}

// anyResolver finds the type a google.protobuf.Any names, as the renderer
// does.
type anyResolver interface {
	FindMessageByURL(url string) (protoreflect.MessageType, error)
}

// Name is empty, so the call's content-type stays application/grpc: grpc-go
// adds a forced codec's name to it.
func (*answerCodec) Name() string { return "" }

func (*answerCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("proto: failed to marshal, message is %T, want proto.Message", v)
	}
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	return mem.BufferSlice{mem.SliceBuffer(b)}, nil
}

func (c *answerCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}
	buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
	defer buf.Free()
	raw := buf.ReadOnlyData()
	count := jsonCount{limit: c.limit, resolver: c.resolver}
	count.message(raw, m.ProtoReflect().Descriptor(), 0)
	// A message that does not parse is left to the decoder, to fail with
	// its own words.
	if !count.bad && count.n > c.limit {
		c.over = model.MarkError(model.Errorf("the answer is at least %d bytes as JSON, over the response limit of %d; raise request.max_response_bytes or limits.max_response_bytes", model.Size(count.n), c.limit), model.ErrLimitExceeded)
		return c.over
	}
	return proto.Unmarshal(raw, m)
}

// jsonCountDepth is how deep jsonCount follows messages: the decoder's own
// recursion limit (proto.UnmarshalOptions.RecursionLimit), which counts each
// map entry as a level as well, so a message too deep to count is too deep
// to decode.
const jsonCountDepth = 10000

// jsonCount counts the JSON a message becomes (see answerCodec). It stops
// as soon as the count is over the limit, so the work it does is bounded by
// the message and the limit: every step adds at least a byte to the count,
// or reads one of the message's.
type jsonCount struct {
	n, limit int64
	resolver anyResolver
	// bad says the message does not parse, or is too deep.
	bad bool
	// The type the last google.protobuf.Any named, kept for the next.
	anyURL  string
	anyType protoreflect.MessageDescriptor
}

func (c *jsonCount) add(k int) bool {
	c.n += int64(k)
	return c.n <= c.limit
}

// fieldSet is the fields of a message seen so far, by index; the first 64
// need no allocation.
type fieldSet struct {
	small uint64
	big   []uint64
}

func (s *fieldSet) has(i int) bool {
	if i < 64 {
		return s.small&(1<<i) != 0
	}
	i -= 64
	return i/64 < len(s.big) && s.big[i/64]&(1<<(i%64)) != 0
}

func (s *fieldSet) set(i, fields int) {
	if i < 64 {
		s.small |= 1 << i
		return
	}
	if s.big == nil {
		s.big = make([]uint64, (fields-64)/64+1)
	}
	i -= 64
	s.big[i/64] |= 1 << (i % 64)
}

// message counts the message b of type md, and says whether to go on.
func (c *jsonCount) message(b []byte, md protoreflect.MessageDescriptor, depth int) bool {
	if depth > jsonCountDepth {
		c.bad = true
		return false
	}
	if md.FullName().Parent() == "google.protobuf" {
		if done, ok := c.wellKnown(b, md, depth); done {
			return ok
		}
	}
	fields := md.Fields()
	var seen, started fieldSet
	emitted := 0
	if !c.add(2) {
		return false
	}
	// head counts a field's name the first time it is seen, with the
	// separator before it and the brackets of a list or map.
	head := func(fd protoreflect.FieldDescriptor) bool {
		i := fd.Index()
		if seen.has(i) {
			return true
		}
		seen.set(i, fields.Len())
		emitted++
		k := len(fd.TextName()) + 3
		if emitted > 1 {
			k++
		}
		if fd.IsList() || fd.IsMap() {
			k += 2
		}
		return c.add(k)
	}
	// element counts the comma before each element of a list or map after
	// its first.
	element := func(fd protoreflect.FieldDescriptor) bool {
		i := fd.Index()
		if started.has(i) {
			return c.add(1)
		}
		started.set(i, fields.Len())
		return true
	}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || num > protowire.MaxValidNumber {
			c.bad = true
			return false
		}
		b = b[n:]
		fd := fields.ByNumber(num)
		if fd == nil || !wireFits(fd, typ) {
			// A field the type does not have, or not with this wire
			// type, is unknown to the decoder, and not rendered.
			n = protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				c.bad = true
				return false
			}
			b = b[n:]
			continue
		}
		if !head(fd) {
			return false
		}
		switch {
		case fd.IsList() && typ == protowire.BytesType && isPacked(fd.Kind()):
			packed, n := protowire.ConsumeBytes(b)
			if n < 0 {
				c.bad = true
				return false
			}
			b = b[n:]
			for len(packed) > 0 {
				if !element(fd) {
					return false
				}
				k, m := scalarLen(fd, typeOf(fd.Kind()), packed)
				if m < 0 {
					c.bad = true
					return false
				}
				packed = packed[m:]
				if !c.add(k) {
					return false
				}
			}
		case fd.IsMap():
			entry, n := protowire.ConsumeBytes(b)
			if n < 0 {
				c.bad = true
				return false
			}
			b = b[n:]
			if !element(fd) || !c.entry(entry, fd, depth) {
				return false
			}
		default:
			if fd.IsList() && !element(fd) {
				return false
			}
			n, ok := c.value(b, fd, typ, depth)
			if !ok {
				return false
			}
			b = b[n:]
		}
	}
	// The fields the answer left out, at their zero values; a oneof's are
	// not written.
	for i := 0; i < fields.Len(); i++ {
		if seen.has(i) {
			continue
		}
		fd := fields.Get(i)
		if fd.ContainingOneof() != nil {
			continue
		}
		emitted++
		k := len(fd.TextName()) + 3 + zeroLen(fd)
		if emitted > 1 {
			k++
		}
		if !c.add(k) {
			return false
		}
	}
	return true
}

// value counts one value of fd at the start of b, and returns its length on
// the wire.
func (c *jsonCount) value(b []byte, fd protoreflect.FieldDescriptor, typ protowire.Type, depth int) (int, bool) {
	switch fd.Kind() {
	case protoreflect.MessageKind:
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			c.bad = true
			return 0, false
		}
		return n, c.message(v, fd.Message(), depth+1)
	case protoreflect.GroupKind:
		v, n := protowire.ConsumeGroup(fd.Number(), b)
		if n < 0 {
			c.bad = true
			return 0, false
		}
		return n, c.message(v, fd.Message(), depth+1)
	}
	k, n := scalarLen(fd, typ, b)
	if n < 0 {
		c.bad = true
		return 0, false
	}
	return n, c.add(k)
}

// entry counts a map entry, "key":value, with what the decoder makes of a
// key or value the entry leaves out.
func (c *jsonCount) entry(b []byte, fd protoreflect.FieldDescriptor, depth int) bool {
	keyField, valueField := fd.MapKey(), fd.MapValue()
	var (
		key, value             []byte
		keyType, valueType     protowire.Type
		haveKey, haveValue     bool
		keyLength, valueLength int
	)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			c.bad = true
			return false
		}
		b = b[n:]
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			c.bad = true
			return false
		}
		switch {
		case num == 1 && wireFits(keyField, typ):
			key, keyType, keyLength, haveKey = b, typ, n, true
		case num == 2 && wireFits(valueField, typ):
			value, valueType, valueLength, haveValue = b, typ, n, true
		}
		b = b[n:]
	}
	// A key is written as a string, whatever its type.
	k := 1 // the colon
	switch {
	case !haveKey:
		k += zeroScalarLen(keyField, true)
	case keyField.Kind() == protoreflect.StringKind:
		k += escapedLen(protowireBytes(key[:keyLength]))
	default:
		n, m := scalarLen(keyField, keyType, key[:keyLength])
		if m < 0 {
			c.bad = true
			return false
		}
		k += n
		if keyField.Kind() != protoreflect.Int64Kind && keyField.Kind() != protoreflect.Sint64Kind &&
			keyField.Kind() != protoreflect.Uint64Kind && keyField.Kind() != protoreflect.Fixed64Kind &&
			keyField.Kind() != protoreflect.Sfixed64Kind {
			k += 2
		}
	}
	if !c.add(k) {
		return false
	}
	switch {
	case haveValue:
		_, ok := c.value(value[:valueLength], valueField, valueType, depth)
		return ok
	case valueField.Kind() == protoreflect.MessageKind || valueField.Kind() == protoreflect.GroupKind:
		// The decoder makes an empty message of a value left out.
		return c.message(nil, valueField.Message(), depth+1)
	default:
		return c.add(zeroScalarLen(valueField, false))
	}
}

// protowireBytes is the content of a length-delimited value.
func protowireBytes(b []byte) []byte {
	v, _ := protowire.ConsumeBytes(b)
	return v
}

// wellKnown counts the well-known types protojson writes in a form of their
// own, at their shortest, and google.protobuf.Any as the message it carries.
// done is false for a type it writes as any other message.
func (c *jsonCount) wellKnown(b []byte, md protoreflect.MessageDescriptor, depth int) (done, ok bool) {
	switch md.Name() {
	case "Any":
		return true, c.anyValue(b, depth)
	case "Timestamp":
		// "1970-01-01T00:00:00Z"
		return true, c.add(22)
	case "Duration":
		// "0s"
		return true, c.add(4)
	case "Empty", "Struct", "ListValue", "FieldMask":
		return true, c.add(2)
	case "Value", "BoolValue", "Int32Value", "Int64Value", "UInt32Value", "UInt64Value",
		"FloatValue", "DoubleValue", "StringValue", "BytesValue":
		return true, c.add(1)
	}
	return false, true
}

// anyValue counts a google.protobuf.Any: {"@type":"url", then the fields of
// the message it carries, or "value": and the well-known type it carries.
// One whose type is not known fails to render, and counts as {}.
func (c *jsonCount) anyValue(b []byte, depth int) bool {
	var url, value []byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			c.bad = true
			return false
		}
		b = b[n:]
		if typ == protowire.BytesType && (num == 1 || num == 2) {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				c.bad = true
				return false
			}
			if num == 1 {
				url = v
			} else {
				value = v
			}
			b = b[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			c.bad = true
			return false
		}
		b = b[n:]
	}
	if len(url) == 0 || c.resolver == nil {
		return c.add(2)
	}
	if c.anyType == nil || string(url) != c.anyURL {
		t, err := c.resolver.FindMessageByURL(string(url))
		if err != nil {
			return c.add(2)
		}
		c.anyURL, c.anyType = string(url), t.Descriptor()
	}
	// "@type": and the URL; the message's own braces stand for the outer
	// ones, and the comma after the URL is not counted.
	if !c.add(8 + escapedLen(url)) {
		return false
	}
	return c.message(value, c.anyType, depth+1)
}

// wireFits says whether the decoder reads a field of fd's type with wire type
// typ; one it does not is an unknown field to it.
func wireFits(fd protoreflect.FieldDescriptor, typ protowire.Type) bool {
	if fd.IsList() && typ == protowire.BytesType && isPacked(fd.Kind()) {
		return true
	}
	return typeOf(fd.Kind()) == typ
}

// typeOf is the wire type of a value of kind k.
func typeOf(k protoreflect.Kind) protowire.Type {
	switch k {
	case protoreflect.BoolKind, protoreflect.EnumKind, protoreflect.Int32Kind, protoreflect.Sint32Kind,
		protoreflect.Uint32Kind, protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind:
		return protowire.VarintType
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	case protoreflect.GroupKind:
		return protowire.StartGroupType
	}
	return protowire.BytesType
}

// isPacked says whether a list of kind k may arrive packed.
func isPacked(k protoreflect.Kind) bool {
	t := typeOf(k)
	return t == protowire.VarintType || t == protowire.Fixed32Type || t == protowire.Fixed64Type
}

// scalarLen is the length of the JSON of a scalar value of fd at the start of
// b, of wire type typ, and its length on the wire (negative when it does not
// parse). Floating-point numbers count at their shortest, a digit.
func scalarLen(fd protoreflect.FieldDescriptor, typ protowire.Type, b []byte) (int, int) {
	switch typ {
	case protowire.VarintType:
		v, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return 0, n
		}
		switch fd.Kind() {
		case protoreflect.BoolKind:
			if v != 0 {
				return 4, n
			}
			return 5, n
		case protoreflect.Int32Kind:
			return intLen(int64(int32(v))), n //nolint:gosec // truncated, as the decoder reads it
		case protoreflect.Sint32Kind:
			return intLen(int64(int32(protowire.DecodeZigZag(v & 0xffffffff)))), n //nolint:gosec // truncated, as the decoder reads it
		case protoreflect.Uint32Kind:
			return uintLen(uint64(uint32(v))), n //nolint:gosec // truncated, as the decoder reads it
		case protoreflect.Int64Kind:
			return intLen(int64(v)) + 2, n //nolint:gosec // two's complement, as the decoder reads it
		case protoreflect.Sint64Kind:
			return intLen(protowire.DecodeZigZag(v)) + 2, n
		case protoreflect.Uint64Kind:
			return uintLen(v) + 2, n
		case protoreflect.EnumKind:
			return enumLen(fd, protoreflect.EnumNumber(int32(v))), n //nolint:gosec // truncated, as the decoder reads it
		}
	case protowire.Fixed32Type:
		v, n := protowire.ConsumeFixed32(b)
		if n < 0 {
			return 0, n
		}
		switch fd.Kind() {
		case protoreflect.Fixed32Kind:
			return uintLen(uint64(v)), n
		case protoreflect.Sfixed32Kind:
			return intLen(int64(int32(v))), n //nolint:gosec // truncated, as the decoder reads it
		}
		return 1, n
	case protowire.Fixed64Type:
		v, n := protowire.ConsumeFixed64(b)
		if n < 0 {
			return 0, n
		}
		switch fd.Kind() {
		case protoreflect.Fixed64Kind:
			return uintLen(v) + 2, n
		case protoreflect.Sfixed64Kind:
			return intLen(int64(v)) + 2, n //nolint:gosec // two's complement, as the decoder reads it
		}
		return 1, n
	case protowire.BytesType:
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return 0, n
		}
		if fd.Kind() == protoreflect.BytesKind {
			return (len(v)+2)/3*4 + 2, n
		}
		return escapedLen(v), n
	}
	return 0, -1
}

// enumLen is an enum value as protojson writes it: its name, its number
// when it has none, null for google.protobuf.NullValue.
func enumLen(fd protoreflect.FieldDescriptor, v protoreflect.EnumNumber) int {
	if fd.Enum().FullName() == "google.protobuf.NullValue" {
		return 4
	}
	if value := fd.Enum().Values().ByNumber(v); value != nil {
		return len(value.Name()) + 2
	}
	return intLen(int64(v))
}

// zeroLen is a field the answer left out as protojson writes it: null for
// one with presence, empty brackets for a list or map, else its zero value.
func zeroLen(fd protoreflect.FieldDescriptor) int {
	switch {
	case fd.IsList() || fd.IsMap():
		return 2
	case fd.HasPresence():
		return 4
	}
	return zeroScalarLen(fd, false)
}

// zeroScalarLen is a scalar's zero value, as a map value or a field without
// presence has it, or as a map key, written as a string, when key is true.
func zeroScalarLen(fd protoreflect.FieldDescriptor, key bool) int {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if key {
			return 7 // "false"
		}
		return 5
	case protoreflect.StringKind, protoreflect.BytesKind:
		return 2
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind:
		return 3
	case protoreflect.EnumKind:
		return enumLen(fd, fd.Default().Enum())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return 4
	}
	if key {
		return 3 // "0"
	}
	return 1
}

// escapedLen is the length of s as a JSON string as protojson writes it,
// quotes and escapes counted.
func escapedLen(s []byte) int {
	n := 2
	for _, c := range s {
		switch {
		case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
			n += 2
		case c < ' ':
			n += 6
		default:
			n++
		}
	}
	return n
}

func intLen(v int64) int {
	if v < 0 {
		return uintLen(uint64(-v)) + 1
	}
	return uintLen(uint64(v))
}

func uintLen(v uint64) int {
	n := 1
	for v >= 10 {
		v /= 10
		n++
	}
	return n
}
