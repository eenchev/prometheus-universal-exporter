package fetch

import (
	"compress/flate"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// fakeStreamError and fakeGoAwayError have the shape of the HTTP/2 client's
// errors of those names, whose types are internal to net/http.
type fakeStreamError struct {
	StreamID uint32
	Code     uint32
	Cause    error
}

func (e fakeStreamError) Error() string {
	return fmt.Sprintf("stream error: stream ID %d; INTERNAL_ERROR; received from peer", e.StreamID)
}

type fakeGoAwayError struct {
	LastStreamID uint32
	ErrCode      uint32
	DebugData    string
}

func (e fakeGoAwayError) Error() string {
	return fmt.Sprintf("http2: server sent GOAWAY and closed the connection; LastStreamID=%v, ErrCode=ENHANCE_YOUR_CALM, debug=%q", e.LastStreamID, e.DebugData)
}

// misnamedStream has a stream's number and is no stream error.
type misnamedStream struct{ StreamID uint32 }

func (e misnamedStream) Error() string { return fmt.Sprintf("stream ID %d; closed", e.StreamID) }

// signedStreamError is named for a stream error and numbers its stream
// otherwise than the client's does.
type signedStreamError struct{ StreamID int }

func (e signedStreamError) Error() string { return fmt.Sprintf("stream ID %d; closed", e.StreamID) }

// tcpAddr is the address of a TCP connection's end, as net has it.
func tcpAddr(s string) net.Addr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

// resetFrom is the error of a read on a connection the other end reset, made
// from port, as net writes it.
func resetFrom(port int) *net.OpError {
	return &net.OpError{Op: "read", Net: "tcp", Source: tcpAddr(fmt.Sprintf("10.0.0.1:%d", port)), Addr: tcpAddr("10.0.0.2:80"), Err: &netSyscallError{"read", syscall.ECONNRESET}}
}

// netSyscallError reads as os.SyscallError does.
type netSyscallError struct {
	call string
	err  error
}

func (e *netSyscallError) Error() string { return e.call + ": " + e.err.Error() }
func (e *netSyscallError) Unwrap() error { return e.err }

// A failed fetch's error is recognised by its text without what belongs to
// the one connection or attempt: of each kind of error that has such a part,
// two that differ only in it read differently, each as the library wrote it,
// and are recognised by one text, which has the mark where the part was;
// the address the connection went to, the error itself, the limit and the
// certificate's own time stay, so another of those is another failure. The
// kinds: the address a connection was made from, alone, behind the wrappers
// a request's error has, without an address it went to, and of two
// connections in one error, as a SOCKS proxy's has; the same address in the
// text of a name server exchange; the stream of an HTTP/2 stream error and
// the last stream of a GOAWAY; the time an expired certificate was held
// against; the offset at which compressed data was corrupt; and, in a gRPC
// status message, the address, that time and the size of an answer over the
// limit.
func TestAFetchFailureIsRecognisedWithoutWhatBelongsToOneConnection(t *testing.T) {
	get := func(inner error) error {
		return fmt.Errorf("HTTP request failed: %w", &url.Error{Op: "Get", URL: "http://10.0.0.2:80/metrics", Err: inner})
	}
	expired := func(now string) error {
		return get(&tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired, Detail: "current time " + now + " is after 2026-10-03T05:06:25Z"}})
	}
	status := func(message string) error {
		return &CallStatusError{Code: 14, CodeName: "UNAVAILABLE", Message: message}
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: tcpAddr("10.0.0.2:80"), Err: &netSyscallError{"connect", syscall.ECONNREFUSED}}
	for name, tc := range map[string]struct {
		a, b, other error
		wantA, same string
	}{
		"the address a connection was made from": {resetFrom(53412), resetFrom(53413), refused,
			"read tcp 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer",
			"read tcp #->10.0.0.2:80: read: connection reset by peer"},
		"behind a request's wrappers": {get(resetFrom(53412)), get(resetFrom(40001)), get(refused),
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": read tcp 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer`,
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": read tcp #->10.0.0.2:80: read: connection reset by peer`},
		"while the body was read, behind a retry's wait": {
			fmt.Errorf("reading response: %w (the wait before retrying was cut short: %w)", resetFrom(53412), context.DeadlineExceeded),
			fmt.Errorf("reading response: %w (the wait before retrying was cut short: %w)", resetFrom(53413), context.DeadlineExceeded),
			fmt.Errorf("reading response: %w (the wait before retrying was cut short: %w)", resetFrom(53413), context.Canceled),
			"reading response: read tcp 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer (the wait before retrying was cut short: context deadline exceeded)",
			"reading response: read tcp #->10.0.0.2:80: read: connection reset by peer (the wait before retrying was cut short: context deadline exceeded)"},
		"without an address it went to": {
			&net.OpError{Op: "read", Net: "udp", Source: tcpAddr("10.0.0.1:5353"), Err: io.ErrUnexpectedEOF},
			&net.OpError{Op: "read", Net: "udp", Source: tcpAddr("10.0.0.1:5354"), Err: io.ErrUnexpectedEOF},
			&net.OpError{Op: "write", Net: "udp", Source: tcpAddr("10.0.0.1:5354"), Err: io.ErrUnexpectedEOF},
			"read udp 10.0.0.1:5353: unexpected EOF", "read udp #: unexpected EOF"},
		"of two connections in one error": {
			get(&net.OpError{Op: "socks connect", Net: "tcp", Source: tcpAddr("10.0.0.9:1080"), Addr: tcpAddr("10.0.0.2:80"), Err: &net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("10.0.0.1:38884"), Addr: tcpAddr("10.0.0.9:1080"), Err: syscall.ECONNRESET}}),
			get(&net.OpError{Op: "socks connect", Net: "tcp", Source: tcpAddr("10.0.0.9:1080"), Addr: tcpAddr("10.0.0.2:80"), Err: &net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("10.0.0.1:38892"), Addr: tcpAddr("10.0.0.9:1080"), Err: syscall.ECONNRESET}}),
			get(&net.OpError{Op: "socks connect", Net: "tcp", Source: tcpAddr("10.0.0.9:1080"), Addr: tcpAddr("10.0.0.2:80"), Err: &net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("10.0.0.1:38892"), Addr: tcpAddr("10.0.0.9:1080"), Err: syscall.EPIPE}}),
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": socks connect tcp 10.0.0.9:1080->10.0.0.2:80: read tcp 10.0.0.1:38884->10.0.0.9:1080: connection reset by peer`,
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": socks connect tcp #->10.0.0.2:80: read tcp #->10.0.0.9:1080: connection reset by peer`},
		"in the text of a name server exchange": {
			get(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "read udp 10.0.0.5:41234->10.0.0.53:53: i/o timeout", Name: "api.internal", Server: "10.0.0.53:53", IsTimeout: true}}),
			get(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "read udp 10.0.0.5:50001->10.0.0.53:53: i/o timeout", Name: "api.internal", Server: "10.0.0.53:53", IsTimeout: true}}),
			get(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.internal", Server: "10.0.0.53:53", IsNotFound: true}}),
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": dial tcp: lookup api.internal on 10.0.0.53:53: read udp 10.0.0.5:41234->10.0.0.53:53: i/o timeout`,
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": dial tcp: lookup api.internal on 10.0.0.53:53: read udp #->10.0.0.53:53: i/o timeout`},
		"the stream of an HTTP/2 stream error": {get(fakeStreamError{StreamID: 5}), get(fakeStreamError{StreamID: 117}), get(fakeGoAwayError{LastStreamID: 5}),
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": stream error: stream ID 5; INTERNAL_ERROR; received from peer`,
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": stream error: stream ID #; INTERNAL_ERROR; received from peer`},
		"the last stream of a GOAWAY": {get(fakeGoAwayError{LastStreamID: 5, DebugData: "slow down"}), get(fakeGoAwayError{LastStreamID: 9, DebugData: "slow down"}), get(fakeGoAwayError{LastStreamID: 9, DebugData: "maintenance"}),
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": http2: server sent GOAWAY and closed the connection; LastStreamID=5, ErrCode=ENHANCE_YOUR_CALM, debug="slow down"`,
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": http2: server sent GOAWAY and closed the connection; LastStreamID=#, ErrCode=ENHANCE_YOUR_CALM, debug="slow down"`},
		"the time a certificate was held against": {expired("2026-10-04T08:06:25+03:00"), expired("2026-10-04T08:07:25+03:00"),
			get(&tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired, Detail: "current time 2026-10-04T08:06:25+03:00 is after 2026-10-01T00:00:00Z"}}),
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T08:06:25+03:00 is after 2026-10-03T05:06:25Z`,
			`HTTP request failed: Get "http://10.0.0.2:80/metrics": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time # is after 2026-10-03T05:06:25Z`},
		"the offset of corrupt compressed data": {fmt.Errorf("reading response: %w", flate.CorruptInputError(20)), fmt.Errorf("reading response: %w", flate.CorruptInputError(4711)), fmt.Errorf("reading response: %w", io.ErrUnexpectedEOF),
			"reading response: flate: corrupt input before offset 20", "reading response: flate: corrupt input before offset #"},
		"the address in a gRPC status": {
			status(`connection error: desc = "transport: authentication handshake failed: read tcp 127.0.0.1:60494->127.0.0.1:33105: read: connection reset by peer"`),
			status(`connection error: desc = "transport: authentication handshake failed: read tcp 127.0.0.1:60508->127.0.0.1:33105: read: connection reset by peer"`),
			status(`connection error: desc = "transport: authentication handshake failed: read tcp 127.0.0.1:60508->127.0.0.1:33106: read: connection reset by peer"`),
			`gRPC call failed: UNAVAILABLE: connection error: desc = "transport: authentication handshake failed: read tcp 127.0.0.1:60494->127.0.0.1:33105: read: connection reset by peer"`,
			`gRPC call failed: UNAVAILABLE: connection error: desc = "transport: authentication handshake failed: read tcp #->127.0.0.1:33105: read: connection reset by peer"`},
		"an IPv6 address in a gRPC status": {
			status(`error reading from server: read tcp [fd00::1]:60494->[fd00::2]:9090: read: connection reset by peer`),
			status(`error reading from server: read tcp [fd00::1]:60495->[fd00::2]:9090: read: connection reset by peer`),
			status(`error reading from server: EOF`),
			`gRPC call failed: UNAVAILABLE: error reading from server: read tcp [fd00::1]:60494->[fd00::2]:9090: read: connection reset by peer`,
			`gRPC call failed: UNAVAILABLE: error reading from server: read tcp #->[fd00::2]:9090: read: connection reset by peer`},
		"the time in a gRPC status": {
			status(`connection error: desc = "transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T08:07:08Z is before 2027-01-01T00:00:00Z"`),
			status(`connection error: desc = "transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T09:00:00Z is before 2027-01-01T00:00:00Z"`),
			status(`connection error: desc = "transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T09:00:00Z is after 2026-01-01T00:00:00Z"`),
			`gRPC call failed: UNAVAILABLE: connection error: desc = "transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T08:07:08Z is before 2027-01-01T00:00:00Z"`,
			`gRPC call failed: UNAVAILABLE: connection error: desc = "transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time # is before 2027-01-01T00:00:00Z"`},
		"the size of a gRPC answer over the limit": {
			&CallStatusError{Code: 8, CodeName: "RESOURCE_EXHAUSTED", Message: "grpc: received message larger than max (5007 vs. 1000)", Err: model.ErrLimitExceeded},
			&CallStatusError{Code: 8, CodeName: "RESOURCE_EXHAUSTED", Message: "grpc: received message larger than max (6120 vs. 1000)", Err: model.ErrLimitExceeded},
			&CallStatusError{Code: 8, CodeName: "RESOURCE_EXHAUSTED", Message: "grpc: received message larger than max (6120 vs. 2000)", Err: model.ErrLimitExceeded},
			"gRPC call failed: RESOURCE_EXHAUSTED: grpc: received message larger than max (5007 vs. 1000)",
			"gRPC call failed: RESOURCE_EXHAUSTED: grpc: received message larger than max (# vs. 1000)"},
		"a gRPC answer over the limit once decompressed, asked of the reflection service": {
			status("asking the reflection service: grpc: message after decompression larger than max (5007 vs. 1000)"),
			status("asking the reflection service: grpc: message after decompression larger than max (5008 vs. 1000)"),
			status("grpc: message after decompression larger than max (5008 vs. 1000)"),
			"gRPC call failed: UNAVAILABLE: asking the reflection service: grpc: message after decompression larger than max (5007 vs. 1000)",
			"gRPC call failed: UNAVAILABLE: asking the reflection service: grpc: message after decompression larger than max (# vs. 1000)"},
	} {
		a, b, other := sameFetchFailure(tc.a), sameFetchFailure(tc.b), sameFetchFailure(tc.other)
		if a.Error() != tc.wantA || a.Error() != tc.a.Error() || b.Error() != tc.b.Error() || a.Error() == b.Error() {
			t.Errorf("%s: the failures read\n%v\n%v\nwant them as they were made, the first\n%s", name, a, b, tc.wantA)
		}
		if model.SameFailureText(a) != tc.same || model.SameFailureText(b) != tc.same {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", name, a, b, model.SameFailureText(a), model.SameFailureText(b), tc.same)
		}
		if model.SameFailureText(other) == tc.same {
			t.Errorf("%s: %v is recognised as the same failure, by %s", name, other, tc.same)
		}
		// What wraps the fetch's error afterwards, as a trip's budget does,
		// keeps what it is recognised by.
		budget := fmt.Errorf("%w (the probe ran out of its 5s budget: the scrape timeout)", a)
		if want := tc.same + " (the probe ran out of its 5s budget: the scrape timeout)"; model.SameFailureText(budget) != want || budget.Error() != tc.wantA+" (the probe ran out of its 5s budget: the scrape timeout)" {
			t.Errorf("%s: wrapped, %v is recognised by\n%s\nwant\n%s", name, budget, model.SameFailureText(budget), want)
		}
		// The error is still what it was to errors.Is and errors.As.
		var op *net.OpError
		if errors.As(tc.a, &op) != errors.As(a, &op) || errors.Is(tc.a, model.ErrLimitExceeded) != errors.Is(a, model.ErrLimitExceeded) || errors.Is(tc.a, context.DeadlineExceeded) != errors.Is(a, context.DeadlineExceeded) {
			t.Errorf("%s: %v no longer unwraps to what it did", name, a)
		}
	}
}

// An error with nothing in it that belongs to one connection is handed on
// as it is, the same value, and recognised as it was: nil, a refused
// connection, which names only where it went, a name that does not resolve,
// an end of file, a deadline, a certificate nobody signed, one that is not
// valid for another reason than its time, a gRPC status that names no
// connection, and an error of the exporter's own with a size in it, whose
// recognised text stays the one it had.
func TestAFetchFailureWithNothingOfOneConnectionIsHandedOnAsItIs(t *testing.T) {
	if sameFetchFailure(nil) != nil {
		t.Error("nil is no longer nil")
	}
	size := model.Errorf("response size %d exceeds limit %d", model.Size(4711), 1000)
	for _, err := range []error{
		errors.New("HTTP request failed"),
		&url.Error{Op: "Get", URL: "http://10.0.0.2:80/", Err: &net.OpError{Op: "dial", Net: "tcp", Addr: tcpAddr("10.0.0.2:80"), Err: syscall.ECONNREFUSED}},
		&url.Error{Op: "Get", URL: "http://api.internal/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.internal", Server: "10.0.0.53:53"}}},
		&url.Error{Op: "Get", URL: "http://10.0.0.2:80/", Err: io.EOF},
		&url.Error{Op: "Get", URL: "http://10.0.0.2:80/", Err: context.DeadlineExceeded},
		&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
		x509.CertificateInvalidError{Reason: x509.NameMismatch, Detail: "current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		x509.CertificateInvalidError{Reason: x509.Expired},
		&CallStatusError{Code: 14, CodeName: "UNAVAILABLE", Message: `connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:44283: connect: connection refused"`},
		&CallStatusError{Code: 14, CodeName: "UNAVAILABLE"},
		&StageError{Stage: "message", Err: errors.New("request.message does not fit")},
		misnamedStream{StreamID: 7},
		signedStreamError{StreamID: 7},
		size,
		fmt.Errorf("file /srv/a: %w", size),
	} {
		was := model.SameFailureText(err)
		got := sameFetchFailure(err)
		if got != err { //nolint:errorlint // the error itself: it is handed on, not wrapped
			t.Errorf("%v (%T) is handed on as %T", err, err, got)
		}
		if now := model.SameFailureText(got); now != was {
			t.Errorf("%v is recognised by %q, and was by %q", err, now, was)
		}
	}
	// One of the exporter's own that wraps a connection's error keeps both.
	both := sameFetchFailure(model.Errorf("after %d bytes: %w", model.Size(4711), resetFrom(53412)))
	if want := "after # bytes: read tcp #->10.0.0.2:80: read: connection reset by peer"; model.SameFailureText(both) != want {
		t.Errorf("%v is recognised by %q, want %q", both, model.SameFailureText(both), want)
	}
}

// Where a library keeps the value only in a text, the text is read for the
// one form that library writes, after the words that library leads up to it
// with, and anything else is left as it is: before `->` only an IP address
// with a port is taken for the address a connection was made from, and only
// after `read` or `write` and one of TCP's or UDP's networks; a certificate's
// detail is read only when it starts with `current time`, a time and `is
// after` or `is before`, and a status message only after all crypto/x509
// says before that detail; and a size only after all of what grpc-go's
// client says of a message over the limit, as digits, a `vs.` and the limit.
func TestOnlyALibrarysOwnFormIsReadForWhatMoves(t *testing.T) {
	for _, tc := range []struct {
		read      func(string) string
		text, out string
	}{
		{withoutSourceAddresses, "read udp 10.0.0.5:41234->10.0.0.53:53: i/o timeout", "read udp #->10.0.0.53:53: i/o timeout"},
		{withoutSourceAddresses, "write udp 10.0.0.5:41234->10.0.0.53:53: connection refused", "write udp #->10.0.0.53:53: connection refused"},
		{withoutSourceAddresses, "read tcp4 10.0.0.5:1->10.0.0.6:2: a: write tcp6 [::1]:3->[::1]:4: b: read udp4 10.0.0.5:5->10.0.0.6:6: c: write udp6 [fe80::1%eth0]:7->[fe80::2%eth0]:8: d",
			"read tcp4 #->10.0.0.6:2: a: write tcp6 #->[::1]:4: b: read udp4 #->10.0.0.6:6: c: write udp6 #->[fe80::2%eth0]:8: d"},
		{withoutSourceAddresses, `desc = "read tcp 10.0.0.5:1->10.0.0.6:2: EOF"`, `desc = "read tcp #->10.0.0.6:2: EOF"`},
		{withoutSourceAddresses, "10.0.0.5:41234->10.0.0.53:53", "10.0.0.5:41234->10.0.0.53:53"},
		{withoutSourceAddresses, "a 10.0.0.5:1->10.0.0.6:2: b [::1]:3->[::1]:4: c", "a 10.0.0.5:1->10.0.0.6:2: b [::1]:3->[::1]:4: c"},
		{withoutSourceAddresses, "replica 10.0.0.7:5432->10.0.0.9:5432 is lagging", "replica 10.0.0.7:5432->10.0.0.9:5432 is lagging"},
		{withoutSourceAddresses, "read 10.0.0.5:1->10.0.0.6:2", "read 10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "tcp 10.0.0.5:1->10.0.0.6:2", "tcp 10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "read  tcp 10.0.0.5:1->10.0.0.6:2", "read  tcp 10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "read tcp  10.0.0.5:1->10.0.0.6:2", "read tcp  10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "read sctp 10.0.0.5:1->10.0.0.6:2", "read sctp 10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "spread tcp 10.0.0.5:1->10.0.0.6:2", "spread tcp 10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "dial tcp 10.0.0.5:1->10.0.0.6:2: refused", "dial tcp 10.0.0.5:1->10.0.0.6:2: refused"},
		{withoutSourceAddresses, "route tcp 10.0.0.5:1->10.0.0.6:2", "route tcp 10.0.0.5:1->10.0.0.6:2"},
		{withoutSourceAddresses, "read ip 10.0.0.5->10.0.0.6: EOF", "read ip 10.0.0.5->10.0.0.6: EOF"},
		{withoutSourceAddresses, "socks connect tcp proxy.internal:1080->10.0.0.2:80: refused", "socks connect tcp proxy.internal:1080->10.0.0.2:80: refused"},
		{withoutSourceAddresses, "the value went 10->20 and 10.0.0.5->10.0.0.6", "the value went 10->20 and 10.0.0.5->10.0.0.6"},
		{withoutSourceAddresses, "->", "->"},
		{withoutSourceAddresses, "a -> b", "a -> b"},
		{withoutSourceAddresses, "read unix @->/run/x.sock: EOF", "read unix @->/run/x.sock: EOF"},
		{withoutSourceAddresses, "no arrow here 10.0.0.5:41234", "no arrow here 10.0.0.5:41234"},
		{withoutSourceAddresses, "", ""},
		{withoutCurrentTime, "current time 2026-10-04T08:06:25+03:00 is after 2026-10-03T05:06:25Z", "current time # is after 2026-10-03T05:06:25Z"},
		{withoutCurrentTime, "current time 2026-10-04T08:06:25Z is before 2026-10-03T05:06:25Z", "current time # is before 2026-10-03T05:06:25Z"},
		{withoutCurrentTime, "current time 2026-10-04T08:06:25Z is not 2026-10-03T05:06:25Z", "current time 2026-10-04T08:06:25Z is not 2026-10-03T05:06:25Z"},
		{withoutCurrentTime, "current time unknown is after 2026-10-03T05:06:25Z", "current time unknown is after 2026-10-03T05:06:25Z"},
		{withoutCurrentTime, "current time 2026-10-04 08:06:25 is after 2026-10-03T05:06:25Z", "current time 2026-10-04 08:06:25 is after 2026-10-03T05:06:25Z"},
		{withoutCurrentTime, "current time 2026-10-04T08:06:25Z", "current time 2026-10-04T08:06:25Z"},
		{withoutCurrentTime, "the time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z", "the time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		{withoutCurrentTime, "current time ", "current time "},
		{withoutCurrentTime, "", ""},
		{withoutCurrentTime, "lease: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z", "lease: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		{withoutExpiryTime, "tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z",
			"tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time # is after 2026-10-03T05:06:25Z"},
		{withoutExpiryTime, "x509: certificate has expired or is not yet valid: current time 2026-10-04T08:06:25Z is before 2027-01-01T00:00:00Z", "x509: certificate has expired or is not yet valid: current time # is before 2027-01-01T00:00:00Z"},
		{withoutExpiryTime, "current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z", "current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		{withoutExpiryTime, "lease: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z", "lease: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		{withoutExpiryTime, "certificate has expired or is not yet valid: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z", "certificate has expired or is not yet valid: current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		{withoutExpiryTime, "x509: certificate has expired or is not yet valid: the current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z", "x509: certificate has expired or is not yet valid: the current time 2026-10-04T08:06:25Z is after 2026-10-03T05:06:25Z"},
		{withoutExpiryTime, "x509: certificate has expired or is not yet valid: ", "x509: certificate has expired or is not yet valid: "},
		{withoutExpiryTime, "", ""},
		{withoutReceivedSize, "grpc: received message larger than max (5007 vs. 1000)", "grpc: received message larger than max (# vs. 1000)"},
		{withoutReceivedSize, "grpc: received message larger than max length allowed on current machine (5007 vs. 1000)", "grpc: received message larger than max length allowed on current machine (# vs. 1000)"},
		{withoutReceivedSize, "grpc: received message after decompression larger than max 1000", "grpc: received message after decompression larger than max 1000"},
		{withoutReceivedSize, "grpc: message after decompression larger than max (5007 vs. 1000)", "grpc: message after decompression larger than max (# vs. 1000)"},
		{withoutReceivedSize, "asking the reflection service: grpc: received message larger than max (5007 vs. 1000)", "asking the reflection service: grpc: received message larger than max (# vs. 1000)"},
		{withoutReceivedSize, "grpc: received message larger than max (many vs. 1000)", "grpc: received message larger than max (many vs. 1000)"},
		{withoutReceivedSize, "grpc: received message larger than max (5007 of 1000)", "grpc: received message larger than max (5007 of 1000)"},
		{withoutReceivedSize, "grpc: received message larger than max (", "grpc: received message larger than max ("},
		{withoutReceivedSize, "larger than max (5007 vs. 1000)", "larger than max (5007 vs. 1000)"},
		{withoutReceivedSize, "received message larger than max (5007 vs. 1000)", "received message larger than max (5007 vs. 1000)"},
		{withoutReceivedSize, "upstream: batch larger than max (5007 vs. 1000)", "upstream: batch larger than max (5007 vs. 1000)"},
		{withoutReceivedSize, "trying to send message larger than max (5007 vs. 1000)", "trying to send message larger than max (5007 vs. 1000)"},
		{withoutReceivedSize, "a larger than max; b larger than max (5007 vs. 1000)", "a larger than max; b larger than max (5007 vs. 1000)"},
		{withoutReceivedSize, "grpc: received message larger than max: batch (5007 vs. 1000)", "grpc: received message larger than max: batch (5007 vs. 1000)"},
		{withoutReceivedSize, "the queue holds (5007 vs. 1000) items", "the queue holds (5007 vs. 1000) items"},
		{withoutReceivedSize, "", ""},
	} {
		if got := tc.read(tc.text); got != tc.out {
			t.Errorf("%q is read as %q, want %q", tc.text, got, tc.out)
		}
	}
}

// The resolver's own error for an exchange with a name server that failed
// is of the form the text is read for: a resolver whose name server does
// not answer, asked twice, gives two errors that name the port each
// question was sent from, and both are recognised by one text.
func TestAResolversExchangeErrorIsRecognisedWhateverPortItAskedFrom(t *testing.T) {
	// A port nothing listens on: the question is refused at once.
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := listener.LocalAddr().String()
	_ = listener.Close()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", server)
	}}
	lookup := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := resolver.LookupHost(ctx, "target.test")
		var dns *net.DNSError
		if !errors.As(err, &dns) || !strings.Contains(dns.Err, "->"+server+": ") {
			t.Skipf("the resolver failed otherwise than in an exchange with its name server: %v", err)
		}
		return sameFetchFailure(err)
	}
	a, b := lookup(), lookup()
	if a.Error() == b.Error() {
		t.Skipf("both questions were sent from one port: %v", a)
	}
	same := model.SameFailureText(a)
	if same != model.SameFailureText(b) || !strings.Contains(same, " #->"+server+": ") || same == a.Error() {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant one text with the mark for the port", a, b, same, model.SameFailureText(b))
	}
}
