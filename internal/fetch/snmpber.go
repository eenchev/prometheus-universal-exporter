//go:build request_type_snmp

package fetch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The Basic Encoding Rules of ASN.1 as SNMP writes its messages (RFC 3416,
// RFC 3417): a tag, a length and the content, a constructed value's content
// the values it holds. encoding/asn1 reads DER, not the application tags of
// SNMP's types, nor values a lax agent writes longer than they need be, so
// the type reads and writes them itself.

// The tags of the values SNMP sends.
const (
	snmpInteger     = 0x02
	snmpOctetString = 0x04
	snmpNull        = 0x05
	snmpTagOID      = 0x06
	snmpSequence    = 0x30
	// The application types of SNMPv2-SMI.
	snmpIPAddress  = 0x40
	snmpCounter32  = 0x41
	snmpGauge32    = 0x42
	snmpTimeTicks  = 0x43
	snmpOpaque     = 0x44
	snmpNsapAddr   = 0x45
	snmpCounter64  = 0x46
	snmpUinteger32 = 0x47
	// The exceptions a variable binding holds in place of a value
	// (RFC 3416 § 3).
	snmpNoSuchObject   = 0x80
	snmpNoSuchInstance = 0x81
	snmpEndOfMibView   = 0x82
)

// The PDUs (RFC 3416 § 3).
const (
	pduGetRequest     = 0xa0
	pduGetNextRequest = 0xa1
	pduResponse       = 0xa2
	pduGetBulkRequest = 0xa5
	pduReport         = 0xa8
)

// snmpTLV is a value: its tag, its length and its content.
func snmpTLV(tag byte, content []byte) []byte {
	out := make([]byte, 0, len(content)+6)
	out = append(out, tag)
	out = append(out, snmpLength(len(content))...)
	return append(out, content...)
}

// snmpLength is a length, in the short form below 128 and the long one
// above it.
func snmpLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)} //nolint:gosec // G115: below 128
	}
	var digits []byte
	for v := n; v > 0; v >>= 8 {
		digits = append([]byte{byte(v)}, digits...)
	}
	return append([]byte{0x80 | byte(len(digits))}, digits...) //nolint:gosec // G115: at most 8 digits
}

// snmpInt is an integer in the fewest bytes of two's complement.
func snmpInt(v int64) []byte {
	var content []byte
	for {
		content = append([]byte{byte(v)}, content...) //nolint:gosec // G115: the low byte, as two's complement is cut
		if (v >= -128 && v < 128) || len(content) == 8 {
			break
		}
		v >>= 8
	}
	// v was cut a byte at a time, the sign held by the shift; the bytes
	// written are its fewest.
	return snmpTLV(snmpInteger, snmpMinimal(content))
}

// snmpMinimal drops the leading bytes of two's complement that repeat the
// sign of the next.
func snmpMinimal(b []byte) []byte {
	for len(b) > 1 && ((b[0] == 0 && b[1]&0x80 == 0) || (b[0] == 0xff && b[1]&0x80 != 0)) {
		b = b[1:]
	}
	return b
}

// snmpSeq is a SEQUENCE of parts.
func snmpSeq(tag byte, parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	content := make([]byte, 0, n)
	for _, p := range parts {
		content = append(content, p...)
	}
	return snmpTLV(tag, content)
}

// snmpOIDBytes is an OBJECT IDENTIFIER: its first two arcs one number,
// each arc in base 128, the high bit of every byte but its last set.
func snmpOIDBytes(oid snmpOID) []byte {
	content := snmpArc(nil, oid[0]*40+oid[1])
	for _, arc := range oid[2:] {
		content = snmpArc(content, arc)
	}
	return snmpTLV(snmpTagOID, content)
}

// snmpArc appends an arc in base 128.
func snmpArc(b []byte, arc uint32) []byte {
	var digits [5]byte
	i := len(digits) - 1
	digits[i] = byte(arc & 0x7f)
	for arc >>= 7; arc > 0; arc >>= 7 {
		i--
		digits[i] = byte(arc&0x7f) | 0x80
	}
	return append(b, digits[i:]...)
}

// errSNMPShort is a message that ends inside a value.
var errSNMPShort = errors.New("the message ends inside a value")

// snmpNext reads the value at pos of buf, within end: its tag, and where
// its content starts and ends. A length in the indefinite form, which SNMP
// does not send, and one past end are refused.
func snmpNext(buf []byte, pos, end int) (tag byte, start, stop int, err error) {
	if pos+2 > end {
		return 0, 0, 0, errSNMPShort
	}
	tag = buf[pos]
	if tag&0x1f == 0x1f {
		// A tag of more than one byte: no SNMP value has one, but an
		// Opaque's content may, and is read by snmpOpaqueFloat.
		return 0, 0, 0, fmt.Errorf("the tag 0x%02x at byte %d has more bytes, which SNMP does not send", tag, pos)
	}
	n := int(buf[pos+1])
	pos += 2
	if n&0x80 != 0 {
		digits := n & 0x7f
		if digits == 0 {
			return 0, 0, 0, fmt.Errorf("the value at byte %d has an indefinite length, which SNMP does not send", pos-2)
		}
		if digits > 4 || pos+digits > end {
			return 0, 0, 0, errSNMPShort
		}
		n = 0
		for _, d := range buf[pos : pos+digits] {
			n = n<<8 | int(d)
		}
		pos += digits
	}
	if n < 0 || pos+n > end {
		return 0, 0, 0, errSNMPShort
	}
	return tag, pos, pos + n, nil
}

// snmpExpect reads the value at pos, which must be of tag.
func snmpExpect(buf []byte, pos, end int, tag byte, what string) (start, stop int, err error) {
	got, start, stop, err := snmpNext(buf, pos, end)
	if err != nil {
		return 0, 0, fmt.Errorf("reading %s: %w", what, err)
	}
	if got != tag {
		return 0, 0, fmt.Errorf("%s has the tag 0x%02x, not 0x%02x", what, got, tag)
	}
	return start, stop, nil
}

// snmpIntValue reads an INTEGER's content, of at most 8 bytes, sign
// extended.
func snmpIntValue(content []byte) (int64, error) {
	if len(content) == 0 || len(content) > 8 {
		return 0, fmt.Errorf("an integer of %d bytes", len(content))
	}
	v := int64(int8(content[0])) //nolint:gosec // G115: the first byte's sign, as two's complement reads it
	for _, b := range content[1:] {
		v = v<<8 | int64(b)
	}
	return v, nil
}

// snmpUintValue reads the content of an unsigned type, Counter32 and the
// like and Counter64, of at most 8 bytes past a leading zero.
func snmpUintValue(content []byte) (uint64, error) {
	if len(content) > 1 && content[0] == 0 {
		content = content[1:]
	}
	if len(content) == 0 || len(content) > 8 {
		return 0, fmt.Errorf("an unsigned integer of %d bytes", len(content))
	}
	var v uint64
	for _, b := range content {
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// snmpIntAt reads the INTEGER at pos.
func snmpIntAt(buf []byte, pos, end int, what string) (int64, int, error) {
	start, stop, err := snmpExpect(buf, pos, end, snmpInteger, what)
	if err != nil {
		return 0, 0, err
	}
	v, err := snmpIntValue(buf[start:stop])
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", what, err)
	}
	return v, stop, nil
}

// snmpOIDValue reads an OBJECT IDENTIFIER's content.
func snmpOIDValue(content []byte) (snmpOID, error) {
	if len(content) == 0 {
		return nil, errors.New("an object identifier of no byte")
	}
	var arcs []uint32
	var arc uint64
	for i, b := range content {
		if arc == 0 && b == 0x80 {
			return nil, errors.New("an object identifier with an arc of a leading 0x80")
		}
		arc = arc<<7 | uint64(b&0x7f)
		// The first two arcs together are at most 4294967295 too, as
		// they are written again when a walk asks for what comes after.
		if arc > 0xffffffff {
			return nil, errors.New("an object identifier with an arc above 4294967295")
		}
		if b&0x80 == 0 {
			arcs = append(arcs, uint32(arc))
			arc = 0
		} else if i == len(content)-1 {
			return nil, errors.New("an object identifier whose last arc does not end")
		}
	}
	first := arcs[0]
	oid := make(snmpOID, 0, len(arcs)+1)
	switch {
	case first < 40:
		oid = append(oid, 0, first)
	case first < 80:
		oid = append(oid, 1, first-40)
	default:
		oid = append(oid, 2, first-80)
	}
	return append(oid, arcs[1:]...), nil
}

// snmpOID is an object identifier, its arcs in order.
type snmpOID []uint32

// String is the identifier in dotted form, 1.3.6.1.2.1.1.3.0.
func (o snmpOID) String() string {
	var b strings.Builder
	for i, arc := range o {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.FormatUint(uint64(arc), 10))
	}
	return b.String()
}

// compare orders identifiers as a walk meets them: arc by arc, a prefix
// before what it begins.
func (o snmpOID) compare(p snmpOID) int {
	for i := range min(len(o), len(p)) {
		switch {
		case o[i] < p[i]:
			return -1
		case o[i] > p[i]:
			return 1
		}
	}
	switch {
	case len(o) < len(p):
		return -1
	case len(o) > len(p):
		return 1
	}
	return 0
}

// under says whether o is in the subtree of root, root itself not.
func (o snmpOID) under(root snmpOID) bool {
	if len(o) <= len(root) {
		return false
	}
	for i, arc := range root {
		if o[i] != arc {
			return false
		}
	}
	return true
}

// parseSNMPOID reads an object identifier as a collector writes it:
// numbers by dots, a leading dot or not, at least two arcs, the first 0, 1
// or 2, the second below 40 under 0 and 1, each at most 4294967295, and at
// most 128 arcs, as SNMP sends no longer one.
func parseSNMPOID(text string) (snmpOID, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(text), ".")
	if trimmed == "" {
		return nil, errors.New("it is empty; write the numbers of the object identifier by dots, such as 1.3.6.1.2.1.1.3.0")
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 || len(parts) > 128 {
		return nil, fmt.Errorf("it has %d arcs; an object identifier has from 2 to 128, such as 1.3.6.1.2.1.1.3.0", len(parts))
	}
	oid := make(snmpOID, len(parts))
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != part {
			return nil, fmt.Errorf("its arc %q is not a number from 0 to 4294967295; an object identifier is numbers by dots, such as 1.3.6.1.2.1.1.3.0, as the names of a MIB are not read", part)
		}
		oid[i] = uint32(n)
	}
	switch {
	case oid[0] > 2:
		return nil, fmt.Errorf("it begins with %d; an object identifier begins with 0, 1 or 2", oid[0])
	case oid[0] < 2 && oid[1] >= 40:
		return nil, fmt.Errorf("its second arc is %d; under %d it is below 40", oid[1], oid[0])
	case oid[0] == 2 && oid[1] > 0xffffffff-80:
		return nil, fmt.Errorf("its second arc is %d, too large to be encoded under 2", oid[1])
	}
	return oid, nil
}
