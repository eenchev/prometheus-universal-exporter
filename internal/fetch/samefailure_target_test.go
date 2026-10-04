package fetch

import (
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A gRPC status message is the target's as often as the client's, and a
// value the target sent is part of what the failure is: a server's own
// message that names two addresses with an arrow between them, a time it
// calls the current one, or a size over a limit of its own, is another
// failure for another value, recognised by the whole of its text and handed
// on as it is. Only what stands after the client libraries' own words is
// left out, which holds for a server that sends those very words too.
func TestAValueTheTargetSentInAStatusMessageTellsFailuresApart(t *testing.T) {
	status := func(format string, args ...any) error {
		return &CallStatusError{Code: 13, CodeName: "INTERNAL", Message: fmt.Sprintf(format, args...)}
	}
	for _, format := range []string{
		"replica 10.0.0.%d:5432->10.0.0.9:5432 is lagging",
		"replica [fd00::%d]:5432->[fd00::2]:5432 is lagging",
		"route 10.0.0.%d:80->backend failed",
		`moved "10.0.0.1:%d->10.0.0.2:6"`,
		"lease: current time 2026-10-04T08:00:0%dZ is after 2026-10-03T00:00:00Z",
		"clock skew; current time 2026-10-04T08:00:0%dZ is before 2026-10-05T00:00:00Z",
		"upstream: batch larger than max (%d vs. 1000)",
		"trying to send message larger than max (%d vs. 1000)",
		"a larger than max; b larger than max (%d vs. 4)",
	} {
		a, b := status(format, 7), status(format, 8)
		gotA, gotB := sameFetchFailure(a), sameFetchFailure(b)
		if gotA != a || gotB != b { //nolint:errorlint // the error itself: it is handed on, not wrapped
			t.Errorf("%v is handed on as %T", a, gotA)
		}
		if sameA, sameB := model.SameFailureText(gotA), model.SameFailureText(gotB); sameA == sameB || sameA != a.Error() || sameB != b.Error() {
			t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant each by its own text", a, b, sameA, sameB)
		}
	}
	for format, same := range map[string]string{
		"relayed: read tcp 10.0.0.1:%d->10.0.0.2:80: read: connection reset by peer":                                                   "gRPC call failed: INTERNAL: relayed: read tcp #->10.0.0.2:80: read: connection reset by peer",
		"relayed: x509: certificate has expired or is not yet valid: current time 2026-10-04T08:00:0%dZ is after 2026-10-03T00:00:00Z": "gRPC call failed: INTERNAL: relayed: x509: certificate has expired or is not yet valid: current time # is after 2026-10-03T00:00:00Z",
		"relayed: grpc: received message larger than max (%d vs. 1000)":                                                                "gRPC call failed: INTERNAL: relayed: grpc: received message larger than max (# vs. 1000)",
	} {
		a, b := sameFetchFailure(status(format, 7)), sameFetchFailure(status(format, 8))
		if model.SameFailureText(a) != same || model.SameFailureText(b) != same || a.Error() == b.Error() {
			t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), same)
		}
	}
}

// withoutSourceAddressesAnywhere, withoutCurrentTimeAnywhere and
// withoutReceivedSizeAnywhere are the three readings of a text as they were
// when each took its value wherever its shape stood: an address before `->`,
// a time after `current time`, a size after `larger than max`.
func withoutSourceAddressesAnywhere(text string) string {
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
		if _, err := netip.ParseAddrPort(text[start:end]); err != nil {
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

func withoutCurrentTimeAnywhere(text string) string {
	const before = "current time "
	at := strings.Index(text, before)
	if at < 0 {
		return text
	}
	start := at + len(before)
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

func withoutReceivedSizeAnywhere(message string) string {
	const phrase, versus = "larger than max", " vs. "
	at := strings.Index(message, phrase)
	if at < 0 {
		return message
	}
	open := strings.IndexByte(message[at:], '(')
	if open < 0 {
		return message
	}
	start := at + open + 1
	end := start
	for end < len(message) && message[end] >= '0' && message[end] <= '9' {
		end++
	}
	if end == start || !strings.HasPrefix(message[end:], versus) {
		return message
	}
	return message[:start] + model.MovingMark + message[end:]
}

// What the client's libraries write is read as it was: the errors net makes
// of a read or a write on a TCP or UDP connection, of either family, with a
// zone, on a Unix socket, and of a dial; the detail crypto/x509 gives an
// expired certificate and its error; and what grpc-go says of an answer over
// the limit, each alone and behind the words gRPC and the resolver put
// before it — three thousand texts drawn at random, of which every one reads
// as it did when a value was taken wherever its shape stood. And a text with
// none of the libraries' words is left whole, where it was read for its
// shape.
func TestWhatTheClientsLibrariesWriteIsReadAsBeforeTheirWordsWereAskedFor(t *testing.T) {
	random := rand.New(rand.NewPCG(15, 4)) //nolint:gosec // texts for a test
	address := func() netip.AddrPort {
		port := uint16(1 + random.IntN(65535)) //nolint:gosec // a port
		switch random.IntN(3) {
		case 0:
			return netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, byte(random.IntN(256)), byte(random.IntN(256))}), port)
		case 1:
			return netip.AddrPortFrom(netip.MustParseAddr(fmt.Sprintf("fd00::%x", 1+random.IntN(65535))), port)
		}
		return netip.AddrPortFrom(netip.MustParseAddr(fmt.Sprintf("fe80::%x%%eth0", 1+random.IntN(65535))), port)
	}
	end := func(network string, at netip.AddrPort) net.Addr {
		if strings.HasPrefix(network, "udp") {
			return net.UDPAddrFromAddrPort(at)
		}
		return net.TCPAddrFromAddrPort(at)
	}
	behind := []string{"%s", "transport: authentication handshake failed: %s", "error reading from server: %s", `connection error: desc = "transport: %s"`,
		"lookup api.internal on 10.0.0.53:53: %s", "asking the reflection service: %s", "tls: failed to verify certificate: %s"}
	networks := []string{"tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"}
	moved := 0
	for i := range 3000 {
		var said string
		switch i % 6 {
		case 0, 1:
			network := networks[random.IntN(len(networks))]
			said = (&net.OpError{Op: []string{"read", "write"}[i%2], Net: network, Source: end(network, address()), Addr: end(network, address()), Err: syscall.ECONNRESET}).Error()
		case 2:
			unix := &net.OpError{Op: "read", Net: "unix", Source: &net.UnixAddr{Name: []string{"", "@", "/run/own.sock"}[random.IntN(3)], Net: "unix"}, Addr: &net.UnixAddr{Name: "/run/x.sock", Net: "unix"}, Err: syscall.ECONNRESET}
			dial := &net.OpError{Op: "dial", Net: "tcp", Addr: end("tcp", address()), Err: syscall.ECONNREFUSED}
			said = []error{unix, dial}[random.IntN(2)].Error()
		case 3:
			now := time.Date(2026, 10, 4, 8, random.IntN(60), random.IntN(60), 0, []*time.Location{time.UTC, time.FixedZone("", 3*3600)}[random.IntN(2)])
			detail := fmt.Sprintf("current time %s is %s %s", now.Format(time.RFC3339), []string{"after", "before"}[random.IntN(2)], now.AddDate(0, 0, random.IntN(9)-4).Format(time.RFC3339))
			if got, was := withoutCurrentTime(detail), withoutCurrentTimeAnywhere(detail); got != was || got == detail {
				t.Fatalf("the detail %q is read as %q; it was as %q", detail, got, was)
			}
			said = x509.CertificateInvalidError{Reason: x509.Expired, Detail: detail}.Error()
		case 4:
			size := 1001 + random.IntN(100000)
			said = []string{
				fmt.Sprintf("grpc: received message larger than max (%d vs. %d)", size, 1000),
				fmt.Sprintf("grpc: message after decompression larger than max (%d vs. %d)", size, 1000),
				fmt.Sprintf("grpc: received message larger than max length allowed on current machine (%d vs. %d)", size, 1000),
				fmt.Sprintf("grpc: received message after decompression larger than max %d", 1000),
			}[random.IntN(4)]
		case 5:
			said = []string{"EOF", "context deadline exceeded", "no such host", "server misbehaving", "value went 10->20", "a -> b", "the current time is unknown"}[random.IntN(7)]
		}
		text := fmt.Sprintf(behind[random.IntN(len(behind))], said)
		got := withoutReceivedSize(withoutExpiryTime(withoutSourceAddresses(text)))
		was := withoutReceivedSizeAnywhere(withoutCurrentTimeAnywhere(withoutSourceAddressesAnywhere(text)))
		if got != was {
			t.Fatalf("%q is read as\n%s\nit was as\n%s", text, got, was)
		}
		if got != text {
			moved++
		}
		// The whole of it: as a call's status, and as what a resolver
		// keeps of its exchange with a name server.
		for _, err := range []error{&CallStatusError{Code: 14, CodeName: "UNAVAILABLE", Message: text}, &net.DNSError{Err: text, Name: "api.internal", Server: "10.0.0.53:53"}} {
			want := err.Error()
			if _, dns := err.(*net.DNSError); dns { //nolint:errorlint // the error itself
				want = strings.Replace(want, text, withoutSourceAddressesAnywhere(text), 1)
			} else {
				want = strings.Replace(want, text, was, 1)
			}
			if same := model.SameFailureText(sameFetchFailure(err)); same != want {
				t.Fatalf("%v is recognised by\n%s\nit was by\n%s", err, same, want)
			}
		}
	}
	if moved < 1500 {
		t.Errorf("%d of the texts have something left out: too few to show it", moved)
	}
	for _, text := range []string{"replica 10.0.0.7:5432->10.0.0.9:5432 is lagging", "lease: current time 2026-10-04T08:00:00Z is after 2026-10-03T00:00:00Z", "upstream: batch larger than max (5007 vs. 1000)"} {
		if got := withoutReceivedSize(withoutExpiryTime(withoutSourceAddresses(text))); got != text {
			t.Errorf("%q is read as %q", text, got)
		}
		if was := withoutReceivedSizeAnywhere(withoutCurrentTimeAnywhere(withoutSourceAddressesAnywhere(text))); was == text {
			t.Errorf("%q was read as it is: it shows nothing", text)
		}
	}
}

// The look-alikes of the HTTP/2 client's errors: types named for a stream
// error or a GOAWAY that keep the stream's number otherwise than the
// client's do.
type (
	innerStream struct{ StreamID uint32 }
	// embeddedStreamError has its StreamID from an embedded pointer, which
	// is nil: asking the value for the field panics.
	embeddedStreamError struct {
		*innerStream
		Code uint32
	}
	// heldStreamError has it from an embedded struct.
	heldStreamError struct {
		innerStream
		Code uint32
	}
	renamedStreamError  struct{ ID uint32 }
	wideStreamError     struct{ StreamID uint64 }
	pointerStreamError  struct{ StreamID uint32 }
	numberStreamError   uint32
	lowerStreamError    struct{ streamID uint32 }
	textStreamError     struct{ StreamID string }
	innerGoAway         struct{ LastStreamID uint32 }
	embeddedGoAwayError struct {
		*innerGoAway
		ErrCode uint32
	}
)

func (embeddedStreamError) Error() string { return "stream error: stream ID 5; CANCEL" }
func (heldStreamError) Error() string     { return "stream error: stream ID 5; CANCEL" }
func (renamedStreamError) Error() string  { return "stream error: stream ID 5; CANCEL" }
func (wideStreamError) Error() string     { return "stream error: stream ID 5; CANCEL" }
func (e *pointerStreamError) Error() string {
	return fmt.Sprintf("stream error: stream ID %d; CANCEL", e.StreamID)
}
func (numberStreamError) Error() string { return "stream error: stream ID 5; CANCEL" }
func (e lowerStreamError) Error() string {
	return fmt.Sprintf("stream error: stream ID %d; CANCEL", e.streamID)
}
func (textStreamError) Error() string { return "stream error: stream ID 5; CANCEL" }
func (embeddedGoAwayError) Error() string {
	return "http2: server sent GOAWAY and closed the connection; LastStreamID=5, ErrCode=NO_ERROR, debug=\"\""
}

// unwrapsWithAPanic is an error that cannot be asked what it wraps.
type unwrapsWithAPanic struct{}

func (unwrapsWithAPanic) Error() string { return "read tcp 10.0.0.1:53412->10.0.0.2:80: EOF" }
func (unwrapsWithAPanic) Unwrap() error { panic("asked what it wraps") }

// The stream of an HTTP/2 error is read from types internal to net/http, by
// their shape, and a type of another shape must be left alone rather than
// take the probe down: one named for a stream error, or a GOAWAY, whose
// number is brought by an embedded pointer that is nil, which panicked, or
// by an embedded struct, or is under another name, 64 bits wide, a text, not
// exported, or whose type is a pointer or a number, is handed on as it is
// and recognised by the whole of its text. So is an error that is a nil
// pointer of its type, alone and wrapped, and one that panics when asked
// what it wraps: whatever panics while an error is read, the error is handed
// on as it came.
func TestAnErrorOfAnotherShapeThanTheHTTP2ClientsIsLeftAloneWithoutAPanic(t *testing.T) {
	var (
		nilPointer *pointerStreamError
		nilOp      *net.OpError
		nilDNS     *net.DNSError
		nilStatus  *CallStatusError
		nilURL     *url.Error
	)
	for name, err := range map[string]error{
		"a stream's number from an embedded nil pointer":      embeddedStreamError{},
		"a last stream's number from an embedded nil pointer": embeddedGoAwayError{},
		"from an embedded struct":                             heldStreamError{innerStream: innerStream{StreamID: 5}},
		"under another name":                                  renamedStreamError{ID: 5},
		"64 bits wide":                                        wideStreamError{StreamID: 5},
		"a text":                                              textStreamError{StreamID: "5"},
		"not exported":                                        lowerStreamError{streamID: 5},
		"a pointer type":                                      &pointerStreamError{StreamID: 5},
		"a number type":                                       numberStreamError(5),
		"a nil pointer of a look-alike":                       nilPointer,
		"a nil *net.OpError":                                  nilOp,
		"a nil *net.DNSError":                                 nilDNS,
		"a nil *CallStatusError":                              nilStatus,
		"a nil *url.Error":                                    nilURL,
		"a nil *net.OpError, wrapped":                         fmt.Errorf("reading response: %w", error(nilOp)),
		"a nil *url.Error, wrapped twice":                     fmt.Errorf("HTTP request failed: %w", fmt.Errorf("attempt 2: %w", error(nilURL))),
	} {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("%s: reading the error panics: %v", name, recovered)
				}
			}()
			want := fmt.Sprint(err)
			got := sameFetchFailure(RedactURLErrors(err))
			if got != err { //nolint:errorlint // the error itself: it is handed on, not wrapped
				t.Errorf("%s: %s is handed on as %T", name, want, got)
			}
			if same := model.SameFailureText(got); same != want {
				t.Errorf("%s: %s is recognised by %q", name, want, same)
			}
			if http2ProtocolError(err) {
				t.Errorf("%s: %s is taken for an HTTP/2 protocol error", name, want)
			}
		}()
	}
	for _, err := range []error{unwrapsWithAPanic{}, fmt.Errorf("reading response: %w", unwrapsWithAPanic{})} {
		if got := sameFetchFailure(err); got != err || model.SameFailureText(got) != err.Error() { //nolint:errorlint // the error itself
			t.Errorf("%v, which panics when asked what it wraps, is handed on as %T and recognised by %q", err, got, model.SameFailureText(got))
		}
	}
	// The client's own shape is still read.
	if got := model.SameFailureText(sameFetchFailure(fakeStreamError{StreamID: 5})); got != "stream error: stream ID #; INTERNAL_ERROR; received from peer" {
		t.Errorf("a stream error is recognised by %q", got)
	}
	// And an error that is a nil pointer is still found by errors.As where it was.
	var op *net.OpError
	if err := RedactURLErrors(fmt.Errorf("reading response: %w", error(nilOp))); !errors.As(err, &op) || op != nil {
		t.Errorf("%v no longer unwraps to what it did", err)
	}
}
