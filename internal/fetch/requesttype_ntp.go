//go:build !select_request_types || request_type_ntp

package fetch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The ntp request type asks an NTP server for the time, as an SNTP client
// does (RFC 4330, with the packet of NTPv4, RFC 5905), and hands what the
// server answered to the transforms as a JSON document: the clock's offset
// from the server and the round trip's delay, worked out as RFC 5905 works
// them out, and the server's stratum, leap indicator, precision, poll,
// root delay and dispersion, reference id and reference time.
//
//	request:
//	  type: ntp
//
// The probe's target is host:port, port 123 when it is left out. A scrape
// sends one request in one UDP datagram and waits, until the scrape's
// deadline, for the reply to it: nothing is sent again, since a server that
// limits its clients' rate answers a client that asks twice with a
// Kiss-o'-Death rather than the time.
//
// The request's transmit timestamp is a random number, not the clock: a
// reply is the reply to this request when its origin timestamp is that
// number, which a third party that does not see the request cannot guess,
// and the client's clock is not told to the server. The offset and delay
// need the time the request left, which the exporter keeps itself: its
// wall clock then, and how long the round trip took by the monotonic
// clock, so that a step of the wall clock during the round trip does not
// make up a delay.
//
// A datagram that is not the reply is ignored, and the scrape goes on
// waiting for the reply: one shorter than an NTP packet, one whose origin
// timestamp is not the request's, one that is not a server's (mode 4, or 5
// as RFC 5905 lists), one of a version other than 3 or 4, and one with no
// transmit timestamp. Such a datagram may be forged, and a forged one must
// not end the scrape; when the deadline comes, the failure says how many
// were ignored and why the last one was. The socket is connected, so only
// datagrams from the address the request went to are read at all.
//
// The reply then fails the scrape when the server says it does not keep the
// time: a Kiss-o'-Death (stratum 0), named by its code; leap indicator 3,
// the alarm of a server that is not synchronised, or stratum 16, which says
// the same; or a stratum past 16, which no server has. Its offset would be
// to a clock nobody vouches for, and a rule over it would alert on that
// clock rather than say why.
//
// This file imports nothing outside the standard library.

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeNTP,
		Fields:       []string{"allowed_targets", "denied_targets"},
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    ntpRequester{},
		JSONAnswer:   true,
		Article:      "an",
		TargetHint:   func(*model.Collector) string { return "host or host:port" },
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "ntp",
	})
}

// ntpRequester is the ntp request type's Requester.
type ntpRequester struct{}

// Validate checks an ntp collector's request: it has no keys of its own, and
// those of the common part it does not take are refused before it is asked.
func (ntpRequester) Validate(*model.Collector) error { return nil }

// Fetch performs one request of an ntp collector.
func (ntpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchNTP(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks an ntp collector's target.
func (ntpRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseNTPTarget(target)
	return err
}

// Label is the url label of an ntp request.
func (ntpRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseNTPTarget(target)
	if err != nil {
		return "", err
	}
	return ntpURL(address), nil
}

// Method is the http_method label of an ntp request.
func (ntpRequester) Method(*model.Collector, RequestOverrides) string { return ntpMethod }

// Display renders an ntp target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func (ntpRequester) Display(target string) string {
	if _, err := parseNTPTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// ntpMethod is the http_method label of a request for the time.
const ntpMethod = "QUERY"

// ntpDefaultPort is the port of a target that names none.
const ntpDefaultPort = "123"

// ntpTimeout bounds a request with no deadline of its own.
const ntpTimeout = 30 * time.Second

// ntpPacketSize is the size of an NTP packet without extension fields or a
// MAC: the request sent, and the least a reply holds.
const ntpPacketSize = 48

// ntpVersion is the version the request is sent as.
const ntpVersion = 4

// The modes of an NTP packet: what a client sends, and what a server and a
// broadcast server answer with.
const (
	ntpModeClient    = 3
	ntpModeServer    = 4
	ntpModeBroadcast = 5
)

// ntpUnsynchronised is the leap indicator of a server whose clock is not
// synchronised (the alarm condition), and ntpStratumUnsynchronised the
// stratum that says the same.
const (
	ntpUnsynchronised        = 3
	ntpStratumUnsynchronised = 16
)

// ntpEraOffset is the seconds from the NTP epoch, 1900-01-01, to the Unix
// epoch.
const ntpEraOffset = 2208988800

// ntpAddress is a target as a request reaches it: host as the policy matches
// it, port, and hostPort what is dialed.
type ntpAddress struct {
	host, hostPort string
	port           int
}

// ntpURL is the url label of a request to address.
func ntpURL(address ntpAddress) string { return "ntp://" + address.hostPort }

// parseNTPTarget reads a target: host, host:port or [v6]:port, port 123
// when it names none. Nothing else may come with it.
func parseNTPTarget(raw string) (ntpAddress, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return ntpAddress{}, ErrMissingTarget
	}
	if strings.Contains(target, "://") {
		return ntpAddress{}, fmt.Errorf("target %q has a scheme; an ntp target is host or host:port, such as time.example.com:123", raw)
	}
	if strings.ContainsAny(target, "/?#@") {
		return ntpAddress{}, fmt.Errorf("target %q has a path, query or user in it; an ntp target is only the host and its port", raw)
	}
	host, port := target, ntpDefaultPort
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Count(target, ":") == 1 {
		return ntpAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:123", raw)
	}
	if host == "" {
		return ntpAddress{}, fmt.Errorf("target %q names no host; an ntp target is host or host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return ntpAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return ntpAddress{}, err
	}
	return ntpAddress{host: canonical, port: n, hostPort: net.JoinHostPort(canonical, port)}, nil
}

// ntpDocument is a server's reply as the transforms read it.
type ntpDocument struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Address is the address the request was sent to, host:port.
	Address string `json:"address"`
	// OffsetSeconds is how far the server's clock is ahead of the
	// exporter's, negative when it is behind; DelaySeconds the round trip
	// without the time the server held the request.
	OffsetSeconds float64 `json:"offset_seconds"`
	DelaySeconds  float64 `json:"delay_seconds"`
	Version       int     `json:"version"`
	LeapIndicator int     `json:"leap_indicator"`
	Stratum       int     `json:"stratum"`
	// Precision and Poll are the exponents the server sent, log2 seconds;
	// PrecisionSeconds and PollSeconds the seconds they stand for.
	Precision             int     `json:"precision"`
	PrecisionSeconds      float64 `json:"precision_seconds"`
	Poll                  int     `json:"poll"`
	PollSeconds           float64 `json:"poll_seconds"`
	RootDelaySeconds      float64 `json:"root_delay_seconds"`
	RootDispersionSeconds float64 `json:"root_dispersion_seconds"`
	ReferenceID           string  `json:"reference_id"`
	ReferenceTime         float64 `json:"reference_time"`
	ReferenceTimeRFC3339  string  `json:"reference_time_rfc3339"`
	TransmitTime          float64 `json:"transmit_time"`
	TransmitTimeRFC3339   string  `json:"transmit_time_rfc3339"`
	DurationSeconds       float64 `json:"duration_seconds"`
}

// ntpNow is the wall clock the request's time is read from; a test sets it.
// The round trip is timed by the monotonic clock whatever it is.
var ntpNow = time.Now

// fetchNTP asks a scrape's server for the time and answers its reply as
// JSON.
func fetchNTP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseNTPTarget(target)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ntpTimeout)
		defer cancel()
	}
	// The host's name now; the address the datagrams go to when the socket
	// is connected, as an http request's connections are checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "ntp", Host: address.hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	traceRequest(ctx, ntpMethod, ntpURL(address), http.Header{"Ntp-Version": {strconv.Itoa(ntpVersion)}}, "", false)
	doc, err := ntpExchange(ctx, address)
	if err != nil {
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	doc.DurationSeconds = time.Since(start).Seconds()
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	traceOutcome(ctx, fmt.Sprintf("stratum %d, offset %gs, delay %gs", doc.Stratum, doc.OffsetSeconds, doc.DelaySeconds))
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// ntpExchange sends the request to address and reads datagrams until the
// reply to it comes or ctx ends.
func ntpExchange(ctx context.Context, address ntpAddress) (ntpDocument, error) {
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "udp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = fmt.Errorf("connecting to %s: %w", address.hostPort, err)
		}
		return ntpDocument{}, err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return ntpDocument{}, err
	}
	// A scrape cancelled before its deadline, as when every probe waiting
	// for it went away, ends the read too.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	request := make([]byte, ntpPacketSize)
	request[0] = ntpVersion<<3 | ntpModeClient
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	copy(request[40:], nonce[:])
	origin := binary.BigEndian.Uint64(nonce[:])

	wall, sent := ntpNow(), time.Now()
	if _, err := conn.Write(request); err != nil {
		return ntpDocument{}, fmt.Errorf("sending the request to %s: %w", address.hostPort, err)
	}
	reply := make([]byte, 1024)
	ignored, lastIgnored := 0, ""
	for {
		n, err := conn.Read(reply)
		elapsed := time.Since(sent)
		if err != nil {
			return ntpDocument{}, ntpReadFailure(ctx, address, err, ignored, lastIgnored)
		}
		packet, why := ntpCheckReply(reply[:n], origin)
		if why != "" {
			ignored, lastIgnored = ignored+1, why
			continue
		}
		if err := ntpServerFailure(address, packet); err != nil {
			return ntpDocument{}, err
		}
		doc := ntpDescribe(packet, wall, elapsed)
		doc.Host, doc.Port, doc.Address = address.host, address.port, conn.RemoteAddr().String()
		return doc, nil
	}
}

// ntpReadFailure words a read that ended without the reply: the deadline,
// which wraps context.DeadlineExceeded as a lookup's does, saying how many
// datagrams were ignored and why the last was; or an error of the socket,
// such as the refusal an ICMP port unreachable brings.
func ntpReadFailure(ctx context.Context, address ntpAddress, err error, ignored int, lastIgnored string) error {
	if ctx.Err() == nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("reading the reply from %s: %w", address.hostPort, err)
	}
	cause := ctx.Err()
	if cause == nil || errors.Is(cause, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	switch ignored {
	case 0:
		return fmt.Errorf("no reply from %s before the deadline: %w", address.hostPort, cause)
	case 1:
		return model.Errorf("no reply from %s before the deadline; %d datagram was ignored, as %s: %w", address.hostPort, model.Size(ignored), lastIgnored, cause)
	}
	return model.Errorf("no reply from %s before the deadline; %d datagrams were ignored, the last as %s: %w", address.hostPort, model.Size(ignored), lastIgnored, cause)
}

// ntpPacket is the fields of a reply the document and the checks read.
type ntpPacket struct {
	leap, version, mode, stratum int
	poll, precision              int8
	rootDelay, rootDispersion    uint32
	referenceID                  [4]byte
	reference, receive, transmit uint64
}

// ntpCheckReply reads a datagram as the reply to the request whose
// transmit timestamp was origin, or says why it is not that reply.
func ntpCheckReply(b []byte, origin uint64) (ntpPacket, string) {
	if len(b) < ntpPacketSize {
		return ntpPacket{}, "it is shorter than an NTP packet"
	}
	p := ntpPacket{
		leap: int(b[0] >> 6), version: int(b[0]>>3) & 7, mode: int(b[0]) & 7,
		stratum: int(b[1]), poll: int8(b[2]), precision: int8(b[3]), //nolint:gosec // G115: signed bytes, as the packet writes them
		rootDelay:      binary.BigEndian.Uint32(b[4:8]),
		rootDispersion: binary.BigEndian.Uint32(b[8:12]),
		reference:      binary.BigEndian.Uint64(b[16:24]),
		receive:        binary.BigEndian.Uint64(b[32:40]),
		transmit:       binary.BigEndian.Uint64(b[40:48]),
	}
	copy(p.referenceID[:], b[12:16])
	switch {
	case binary.BigEndian.Uint64(b[24:32]) != origin:
		return p, "its origin timestamp is not the request's"
	case p.mode != ntpModeServer && p.mode != ntpModeBroadcast:
		return p, "it is not a server's reply (mode 4 or 5)"
	case p.version != 3 && p.version != 4:
		return p, "it is of an NTP version other than 3 or 4"
	case p.transmit == 0:
		return p, "it has no transmit timestamp"
	}
	return p, ""
}

// ntpKissCodes are the Kiss-o'-Death codes of RFC 5905 (7.4), with what
// each says. A code among them is shown to a probe's caller; any other is a
// value of the answer, and withheld.
var ntpKissCodes = map[string]string{
	"ACST": "the association belongs to an anycast server",
	"AUTH": "server authentication failed",
	"AUTO": "autokey sequence failed",
	"BCST": "the association belongs to a broadcast server",
	"CRYP": "cryptographic authentication or identification failed",
	"DENY": "access denied by remote server",
	"DROP": "lost peer in symmetric mode",
	"RSTR": "access denied due to local policy",
	"INIT": "the association has not yet synchronized for the first time",
	"MCST": "the association belongs to a dynamically discovered server",
	"NKEY": "no key found",
	"RATE": "rate exceeded: the server asks to be asked less often",
	"RMOT": "alteration of association from a remote host running ntpdc",
	"STEP": "a step change in system time has occurred, but the association has not yet resynchronized",
}

// ntpServerFailure is the failure of a reply whose server does not keep the
// time: a Kiss-o'-Death, an unsynchronised server, a stratum no server has.
func ntpServerFailure(address ntpAddress, p ntpPacket) error {
	switch {
	case p.stratum == 0:
		code := strings.TrimRight(string(p.referenceID[:]), "\x00")
		if meaning, known := ntpKissCodes[code]; known {
			return model.Errorf("%s answered a Kiss-o'-Death, %s: %s", address.hostPort, model.Bare(code).ForCaller(code), meaning)
		}
		return model.Errorf("%s answered a Kiss-o'-Death, %s, a code RFC 5905 does not list", address.hostPort, model.Quoted(code))
	case p.leap == ntpUnsynchronised:
		return fmt.Errorf("%s is not synchronised: its leap indicator is 3, the alarm condition", address.hostPort)
	case p.stratum == ntpStratumUnsynchronised:
		return fmt.Errorf("%s is not synchronised: its stratum is 16", address.hostPort)
	case p.stratum > ntpStratumUnsynchronised:
		return model.Errorf("%s answered stratum %s, which no server has; a stratum is from 1 to 16", address.hostPort, model.Bare(strconv.Itoa(p.stratum)))
	}
	return nil
}

// ntpDescribe is the document of a reply to a request sent at wall by the
// exporter's clock, which came elapsed later by the monotonic clock.
//
// The four timestamps of RFC 5905 are t1, the request sent, and t4, the
// reply received, of the exporter's clock, and t2, the request received,
// and t3, the reply sent, of the server's. The offset is ((t2-t1)+(t3-t4))/2
// and the delay (t4-t1)-(t3-t2), each difference worked out in the 64-bit
// timestamps as a signed number (ntpDifference), so that one across the
// end of an NTP era, as in 2036, is the small number it is.
func ntpDescribe(p ntpPacket, wall time.Time, elapsed time.Duration) ntpDocument {
	t1 := ntpTimestamp(wall)
	t4 := t1 + ntpDuration(elapsed)
	doc := ntpDocument{
		OffsetSeconds: (ntpDifference(p.receive, t1) + ntpDifference(p.transmit, t4)) / 2,
		DelaySeconds:  ntpDifference(t4, t1) - ntpDifference(p.transmit, p.receive),
		Version:       p.version, LeapIndicator: p.leap, Stratum: p.stratum,
		Precision: int(p.precision), PrecisionSeconds: math.Ldexp(1, int(p.precision)),
		Poll: int(p.poll), PollSeconds: math.Ldexp(1, int(p.poll)),
		RootDelaySeconds:      float64(p.rootDelay) / (1 << 16),
		RootDispersionSeconds: float64(p.rootDispersion) / (1 << 16),
		ReferenceID:           ntpReferenceID(p.stratum, p.referenceID),
	}
	transmit := ntpTime(p.transmit, wall)
	doc.TransmitTime, doc.TransmitTimeRFC3339 = ntpUnixSeconds(transmit), transmit.UTC().Format(time.RFC3339Nano)
	// A reference timestamp of 0 is one the server has not set.
	if p.reference != 0 {
		reference := ntpTime(p.reference, wall)
		doc.ReferenceTime, doc.ReferenceTimeRFC3339 = ntpUnixSeconds(reference), reference.UTC().Format(time.RFC3339Nano)
	}
	return doc
}

// ntpReferenceID is the reference id as text: of stratum 1, the code of the
// server's reference clock, such as GPS, when it is printable ASCII, and
// its bytes in hexadecimal otherwise; of a higher stratum, the IPv4 address
// of the server's own server, or for one over IPv6 the first four bytes of
// a hash of its address, which reads the same way.
func ntpReferenceID(stratum int, id [4]byte) string {
	if stratum != 1 {
		return net.IPv4(id[0], id[1], id[2], id[3]).String()
	}
	code := strings.TrimRight(string(id[:]), "\x00")
	for i := 0; i < len(code); i++ {
		if code[i] < 0x20 || code[i] > 0x7e {
			return fmt.Sprintf("0x%x", id[:])
		}
	}
	return code
}

// ntpTimestamp is t as a 64-bit NTP timestamp: seconds since 1900 in its
// era, which is all the timestamp holds, and the fraction of a second.
func ntpTimestamp(t time.Time) uint64 {
	seconds := uint64(t.Unix() + ntpEraOffset)                       //nolint:gosec // G115: wraps to the era, as the timestamp does
	fraction := (uint64(t.Nanosecond()) << 32) / uint64(time.Second) //nolint:gosec // G115: 0 to 999999999
	return seconds<<32 | fraction
}

// ntpDuration is d as an NTP timestamp counts it.
func ntpDuration(d time.Duration) uint64 {
	return uint64((d.Nanoseconds() / int64(time.Second) << 32) + ((d.Nanoseconds() % int64(time.Second)) << 32 / int64(time.Second))) //nolint:gosec // G115: a round trip, never negative
}

// ntpDifference is a-b in seconds, the timestamps' difference read as a
// signed number: right across the end of an era whenever the two are
// within 68 years of each other.
func ntpDifference(a, b uint64) float64 {
	return float64(int64(a-b)) / (1 << 32) //nolint:gosec // G115: read as signed on purpose (RFC 5905)
}

// ntpTime is the time of an NTP timestamp, of whichever era puts it within
// 68 years of pivot.
func ntpTime(ts uint64, pivot time.Time) time.Time {
	d := int64(ts - ntpTimestamp(pivot)) //nolint:gosec // G115: read as signed on purpose (RFC 5905)
	seconds, fraction := d>>32, d&(1<<32-1)
	return pivot.Add(time.Duration(seconds)*time.Second + time.Duration((fraction*int64(time.Second))>>32))
}

// ntpUnixSeconds is t as Unix seconds with their fraction.
func ntpUnixSeconds(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/float64(time.Second)
}
