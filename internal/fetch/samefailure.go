package fetch

import (
	"compress/flate"
	"crypto/x509"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A fetch that fails says what the network said, and the network's errors
// name the connection they happened on: `read tcp
// 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer` has the port
// the exporter's end of that one connection had, which is another on the
// next. The failure log tells a failure that keeps happening from a new one
// by its text (model.SameFailureText), so a target that resets every
// connection was a new failure on every probe: logged in full each time,
// never summed up as a repeat, and its recovery counted from the last one.
//
// So a failed fetch's error is given the text it is recognised by
// (model.SameFailureAs): its text with what belongs to the one connection or
// the one attempt replaced by the mark of a moving part. The text itself,
// which is what is logged and what a scraper is answered, is unchanged.
// What is left out:
//
//   - the address a connection was made from, of every *net.OpError in the
//     chain that has one (Source): the exporter's own address and the port
//     the system picked for that connection. A SOCKS proxy's error has the
//     proxy there, which is the same on every attempt and is left out with
//     it. The address the connection went to stays: it is what was asked;
//   - the same address in the text of an error a resolver's exchange with
//     its name server failed with, which *net.DNSError keeps as text (Err);
//   - the stream of an HTTP/2 stream error, and the last stream a GOAWAY
//     names: the requests of one connection are numbered as they are made;
//   - the time an expired certificate, or one not valid yet, was held
//     against, which x509.CertificateInvalidError has in its Detail;
//   - the offset at which a compressed body turned out to be corrupt
//     (flate.CorruptInputError), a position in the response;
//   - and, in the message of a gRPC status, which is text and nothing else,
//     what the client's own libraries wrote there, each after the words that
//     library leads up to it with: the address a connection was made from,
//     after `read tcp ` or `write tcp `; that time, after `x509: certificate
//     has expired or is not yet valid: current time `; and the size of an
//     answer over the limit, in `grpc: received message larger than max (5007
//     vs. 1000)`.
//
// Where the error's type has the value, the value is taken from the type.
// Where a library keeps it only in a text — the name server exchange, the
// certificate's Detail, and everything gRPC says, which reaches a call as a
// status message — the text is read for the one form that library writes it
// in, here and nowhere else, and a text that is not of that form is left as
// it is, so the failure is then recognised by the whole of its text, as it
// was.
//
// A status message is the target's as often as the client's: what a server
// answers a call with reaches the call the same way, and a value the target
// sent is part of what the failure is — `replica 10.0.0.7:5432->10.0.0.9:5432
// is lagging` is another failure for another replica. So nothing is taken
// for the client's by its shape alone, an address before `->`, a time, a size
// in brackets: only what stands after the library's own words, in full, is.
// A server that sends those very words, with the value after them, has that
// value left out as the client's would be; that is accepted.

// sameFetchFailure gives a failed fetch's error the text it is recognised
// by: err itself when nothing in it belongs to one connection or attempt,
// and nil for nil. Only a failure pays for it.
func sameFetchFailure(err error) (recognised error) {
	if err == nil {
		return nil
	}
	// Reading an error for what moves must never be what takes a probe
	// down: some of it is read from types that are internal to net/http, by
	// their shape (uint32Field), and an error may be a nil pointer of its
	// type, or one a later Go lays out otherwise. So whatever panics here,
	// the error is handed on as it came, and the failure is recognised by
	// the whole of its text.
	defer func() {
		if recover() != nil {
			recognised = err
		}
	}()
	var parts []string
	walkErrors(err, func(e error) { parts = appendMovingParts(parts, e) })
	if len(parts) == 0 {
		return err
	}
	// The parts the errors of the exporter's own name, a size or an age,
	// are out of the text already.
	text := model.SameFailureText(err)
	same := text
	for i := 0; i < len(parts); i += 2 {
		same = strings.ReplaceAll(same, parts[i], parts[i+1])
	}
	if same == text {
		return err
	}
	return model.SameFailureAs(err, same)
}

// appendMovingParts adds to parts what e, one error of a chain, wrote into
// the chain's text that belongs to one connection or attempt, and what
// stands for it in the recognised text: the two as a pair.
func appendMovingParts(parts []string, e error) []string {
	add := func(written, same string) {
		if written != "" && written != same {
			parts = append(parts, written, same)
		}
	}
	switch e := e.(type) { //nolint:errorlint // walkErrors hands over each error of the chain itself
	case *net.OpError:
		// As (*net.OpError).Error writes it: after the network, and before
		// the address the connection went to when there is one.
		if e == nil || e.Source == nil {
			break
		}
		if source := e.Source.String(); e.Addr != nil {
			add(" "+source+"->", " "+model.MovingMark+"->")
		} else {
			add(" "+source+": ", " "+model.MovingMark+": ")
		}
	case *net.DNSError:
		if e != nil {
			add(e.Err, withoutSourceAddresses(e.Err))
		}
	case x509.CertificateInvalidError:
		if e.Reason == x509.Expired {
			add(e.Detail, withoutCurrentTime(e.Detail))
		}
	case flate.CorruptInputError:
		written := e.Error()
		add(written, strings.TrimSuffix(written, strconv.FormatInt(int64(e), 10))+model.MovingMark)
	case *CallStatusError:
		if e != nil {
			add(e.Message, withoutReceivedSize(withoutExpiryTime(withoutSourceAddresses(e.Message))))
		}
	default:
		// The HTTP/2 client's errors are of types internal to net/http, as
		// its connection error is (http2ProtocolError), so they are told
		// by what they are: a struct named for a stream error with the
		// stream's number, and one named for a GOAWAY with the last
		// stream's.
		v := reflect.ValueOf(e)
		if id, ok := uint32Field(v, "StreamError", "StreamID"); ok {
			add("stream ID "+id+";", "stream ID "+model.MovingMark+";")
		}
		if id, ok := uint32Field(v, "GoAwayError", "LastStreamID"); ok {
			add("LastStreamID="+id+",", "LastStreamID="+model.MovingMark+",")
		}
	}
	return parts
}

// uint32Field is the field of v by that name as text, when v is a struct
// whose type is named, or ends with, typeName and has such a field of 32
// unsigned bits, a field of its own.
//
// The field is looked up in the type, not in the value: a field of that name
// that an embedded struct brings is none of the HTTP/2 client's, and asking
// the value for one brought through an embedded pointer that is nil panics.
func uint32Field(v reflect.Value, typeName, field string) (string, bool) {
	if !v.IsValid() || v.Kind() != reflect.Struct || !strings.HasSuffix(v.Type().Name(), typeName) {
		return "", false
	}
	f, ok := v.Type().FieldByName(field)
	if !ok || len(f.Index) != 1 || f.Type.Kind() != reflect.Uint32 {
		return "", false
	}
	return strconv.FormatUint(v.Field(f.Index[0]).Uint(), 10), true
}

// withoutSourceAddresses is text with the mark in place of each address a
// connection was made from, in the form (*net.OpError).Error writes it for a
// read or a write that failed: the operation, the network, an address and a
// port, `->`, and the address the connection went to, as in `read udp
// 10.0.0.5:41234->10.0.0.1:53: i/o timeout`. What stands before a `->` is
// taken for one only when it reads as an IP address with a port and stands
// after those words (afterReadOrWrite).
func withoutSourceAddresses(text string) string {
	const arrow = "->"
	var b strings.Builder
	done := 0
	for from := 0; ; {
		i := strings.Index(text[from:], arrow)
		if i < 0 {
			break
		}
		end := from + i
		from = end + len(arrow)
		start := strings.LastIndexByte(text[:end], ' ') + 1
		if start < done {
			continue
		}
		if _, err := netip.ParseAddrPort(text[start:end]); err != nil || !afterReadOrWrite(text[:start]) {
			continue
		}
		b.WriteString(text[done:start])
		b.WriteString(model.MovingMark)
		done = end
	}
	if done == 0 {
		return text
	}
	b.WriteString(text[done:])
	return b.String()
}

// afterReadOrWrite reports whether text ends as (*net.OpError).Error begins
// for a read or a write on a connection that has ports: `read tcp `, `write
// udp `, the operation as a word of its own and the network one of TCP's or
// UDP's, which net names with the family asked for too (tcp4, udp6). The
// address of a Unix socket's end is a path, or nothing, and is left.
func afterReadOrWrite(text string) bool {
	text, ok := strings.CutSuffix(text, " ")
	if !ok {
		return false
	}
	space := strings.LastIndexByte(text, ' ')
	if space < 0 {
		return false
	}
	switch text[space+1:] {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return false
	}
	for _, operation := range [...]string{"read", "write"} {
		if before, ok := strings.CutSuffix(text[:space], operation); ok {
			return before == "" || !asciiLetter(before[len(before)-1])
		}
	}
	return false
}

// asciiLetter reports whether c is a letter of the English alphabet.
func asciiLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// withoutExpiryTime is a gRPC status message with the mark in place of the
// time a certificate was held against, where the message says what
// crypto/x509 says of an expired certificate, in full: `x509: certificate has
// expired or is not yet valid: ` and the detail withoutCurrentTime reads.
func withoutExpiryTime(message string) string {
	const said = "x509: certificate has expired or is not yet valid: "
	at := strings.Index(message, said)
	if at < 0 {
		return message
	}
	detail := at + len(said)
	return message[:detail] + withoutCurrentTime(message[detail:])
}

// withoutCurrentTime is the detail of an expired certificate's error with
// the mark in place of the time the certificate was held against, in the
// form crypto/x509 writes it, which the detail starts with: `current time
// 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z`, or `is before`. The
// time the certificate is valid to, or from, stays: it is the certificate's.
// What stands there is taken for the time only when it reads as one.
func withoutCurrentTime(text string) string {
	const before = "current time "
	if !strings.HasPrefix(text, before) {
		return text
	}
	start := len(before)
	length := strings.IndexByte(text[start:], ' ')
	if length < 0 {
		return text
	}
	end := start + length
	if !strings.HasPrefix(text[end:], " is after ") && !strings.HasPrefix(text[end:], " is before ") {
		return text
	}
	if !readsAsTime(text[start:end]) {
		return text
	}
	return text[:start] + model.MovingMark + text[end:]
}

// readsAsTime reports whether s is written as RFC 3339 writes a time: a
// date, a T and a time of day, in digits.
func readsAsTime(s string) bool {
	const form = "0000-00-00T00:00:00"
	if len(s) < len(form) {
		return false
	}
	for i := range len(form) {
		if digit := s[i] >= '0' && s[i] <= '9'; digit != (form[i] == '0') || !digit && s[i] != form[i] {
			return false
		}
	}
	return true
}

// withoutReceivedSize is a gRPC status message with the mark in place of
// the size of a message that was over a limit, after the words grpc-go's
// client says it with, in full (google.golang.org/grpc v1.84.0, rpc_util.go):
// `grpc: received message larger than max (5007 vs. 1000)`, the same of a
// message once decompressed, and of one larger than the machine can hold.
// The limit, which the configuration gave, stays. Any other words before a
// size and a limit, as a server's own `batch larger than max (5007 vs.
// 1000)` or the `trying to send message larger than max` of a grpc-go
// server, are the target's, and are left.
func withoutReceivedSize(message string) string {
	const versus = " vs. "
	start := -1
	for _, said := range [...]string{
		"grpc: received message larger than max (",
		"grpc: message after decompression larger than max (",
		"grpc: received message larger than max length allowed on current machine (",
	} {
		if at := strings.Index(message, said); at >= 0 {
			start = at + len(said)
			break
		}
	}
	if start < 0 {
		return message
	}
	end := start
	for end < len(message) && message[end] >= '0' && message[end] <= '9' {
		end++
	}
	if end == start || !strings.HasPrefix(message[end:], versus) {
		return message
	}
	return message[:start] + model.MovingMark + message[end:]
}
