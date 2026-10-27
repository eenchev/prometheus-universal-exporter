//go:build !select_request_types || request_type_ntp

package fetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// ntpReply is what a stand-in server answers a request with.
type ntpReply struct {
	leap, version, mode, stratum int
	poll, precision              int8
	rootDelay, rootDispersion    uint32
	referenceID                  [4]byte
	reference, receive, transmit uint64
	// origin, when set, is the origin timestamp in place of the request's
	// transmit timestamp; short cuts the packet to that many bytes.
	origin *uint64
	short  int
}

// bytes is the reply to request as a datagram.
func (r ntpReply) bytes(request []byte) []byte {
	b := make([]byte, ntpPacketSize)
	b[0] = byte(r.leap<<6 | r.version<<3 | r.mode)
	b[1], b[2], b[3] = byte(r.stratum), byte(r.poll), byte(r.precision)
	binary.BigEndian.PutUint32(b[4:], r.rootDelay)
	binary.BigEndian.PutUint32(b[8:], r.rootDispersion)
	copy(b[12:], r.referenceID[:])
	binary.BigEndian.PutUint64(b[16:], r.reference)
	copy(b[24:32], request[40:48])
	if r.origin != nil {
		binary.BigEndian.PutUint64(b[24:], *r.origin)
	}
	binary.BigEndian.PutUint64(b[32:], r.receive)
	binary.BigEndian.PutUint64(b[40:], r.transmit)
	if r.short > 0 {
		return b[:r.short]
	}
	return b
}

// goodNTPReply is a synchronised stratum 2 server's reply whose clock is
// ahead of this one by ahead: it received and answered the request at once.
func goodNTPReply(ahead time.Duration) ntpReply { return ntpReplyAt(time.Now().Add(ahead)) }

// ntpReplyAt is a synchronised stratum 2 server's reply of a server whose
// clock reads now when the request comes, received and answered at once.
func ntpReplyAt(at time.Time) ntpReply {
	now := ntpTimestamp(at)
	return ntpReply{
		version: 4, mode: ntpModeServer, stratum: 2, poll: 6, precision: -20,
		rootDelay: 0x00008000, rootDispersion: 0x00004000,
		referenceID: [4]byte{192, 0, 2, 7},
		reference:   now - 64<<32, receive: now, transmit: now,
	}
}

// ntpStub is a stand-in NTP server on 127.0.0.1: for each request it reads
// it sends the datagrams answer gives, and it keeps the requests.
type ntpStub struct {
	addr     string
	mu       sync.Mutex
	requests [][]byte
}

func newNTPStub(t *testing.T, answer func(request []byte) [][]byte) *ntpStub {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &ntpStub{addr: conn.LocalAddr().String()}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			request := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.requests = append(s.requests, request)
			s.mu.Unlock()
			for _, d := range answer(request) {
				_, _ = conn.WriteTo(d, from)
			}
		}
	}()
	return s
}

// received is the requests the stub has read.
func (s *ntpStub) received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.requests...)
}

// ntpAnswering is a stub's answer of replies, made when a request comes.
func ntpAnswering(replies ...func() ntpReply) func([]byte) [][]byte {
	return func(request []byte) [][]byte {
		var out [][]byte
		for _, r := range replies {
			out = append(out, r().bytes(request))
		}
		return out
	}
}

// ntpCollector is a validated ntp collector.
func ntpCollector(t *testing.T, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "clock", Request: model.RequestConfig{Type: RequestTypeNTP}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// ntpDoc is a scrape of target through c within timeout, and its document.
func ntpDoc(t *testing.T, c *model.Collector, target string, timeout time.Duration) (ntpDocument, error) {
	t.Helper()
	resp, err := FetchCollector(context.Background(), target, c, RequestOverrides{Timeout: timeout}, nil)
	if err != nil {
		return ntpDocument{}, err
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	var doc ntpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, resp.Body)
	}
	return doc, nil
}

// ntpNear says whether got is within tolerance of want.
func ntpNear(got, want, tolerance float64) bool { return math.Abs(got-want) <= tolerance }

// A scrape sends one NTPv4 client request, of 48 bytes, whose only
// timestamp is a random transmit timestamp, and answers the server's reply
// as the document: the offset of a server 5 seconds ahead, the round trip's
// delay, and the server's fields, each in the unit its name says.
func TestAnNTPCollectorAnswersTheServersReplyAsADocument(t *testing.T) {
	s := newNTPStub(t, ntpAnswering(func() ntpReply { return goodNTPReply(5 * time.Second) }))
	doc, err := ntpDoc(t, ntpCollector(t), s.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ntpNear(doc.OffsetSeconds, 5, 0.5) || doc.DelaySeconds < 0 || doc.DelaySeconds > 1 {
		t.Errorf("offset %g, delay %g", doc.OffsetSeconds, doc.DelaySeconds)
	}
	host, port, _ := net.SplitHostPort(s.addr)
	if doc.Host != host || doc.Address != s.addr || doc.Port == 0 || port == "" || doc.Version != 4 || doc.LeapIndicator != 0 || doc.Stratum != 2 ||
		doc.Precision != -20 || doc.PrecisionSeconds != math.Ldexp(1, -20) || doc.Poll != 6 || doc.PollSeconds != 64 ||
		doc.RootDelaySeconds != 0.5 || doc.RootDispersionSeconds != 0.25 || doc.ReferenceID != "192.0.2.7" || doc.DurationSeconds <= 0 {
		t.Errorf("%+v", doc)
	}
	if age := doc.TransmitTime - doc.ReferenceTime; !ntpNear(age, 64, 0.001) || !ntpNear(doc.TransmitTime, float64(time.Now().Add(5*time.Second).Unix()), 5) || doc.TransmitTimeRFC3339 == "" || doc.ReferenceTimeRFC3339 == "" {
		t.Errorf("reference %g, transmit %g (%s)", doc.ReferenceTime, doc.TransmitTime, doc.TransmitTimeRFC3339)
	}
	if _, err := ntpDoc(t, ntpCollector(t), s.addr, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	requests := s.received()
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	for _, r := range requests {
		if len(r) != ntpPacketSize || r[0] != 0x23 {
			t.Errorf("request % x", r)
		}
		for i, b := range r[1:40] {
			if b != 0 {
				t.Errorf("request byte %d is %d; nothing but the transmit timestamp is set", i+1, b)
			}
		}
	}
	first, second := binary.BigEndian.Uint64(requests[0][40:]), binary.BigEndian.Uint64(requests[1][40:])
	clock := ntpTimestamp(time.Now())
	if first == second || math.Abs(ntpDifference(first, clock)) < 3600 || math.Abs(ntpDifference(second, clock)) < 3600 {
		t.Errorf("transmit timestamps %x and %x near the clock %x; want random ones", first, second, clock)
	}
}

// The offset and the delay are RFC 5905's of the four timestamps, the
// round trip timed by the duration given: a request sent at t1 and
// received at t1+10.03s, answered at t1+10.05s and received back 100ms
// after t1 is an offset of 9.99s and a delay of 80ms. A stratum 1 server's
// reference id is its clock's code, or its bytes in hexadecimal when they
// are no printable text.
func TestNTPOffsetAndDelayAreWorkedOutFromTheFourTimestamps(t *testing.T) {
	wall := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	t1 := ntpTimestamp(wall)
	seconds := func(s float64) uint64 { return uint64(s * (1 << 32)) }
	p := ntpPacket{version: 4, mode: ntpModeServer, stratum: 1, referenceID: [4]byte{'G', 'P', 'S', 0}, reference: t1 - seconds(16), receive: t1 + seconds(10.03), transmit: t1 + seconds(10.05)}
	doc := ntpDescribe(p, wall, 100*time.Millisecond)
	if !ntpNear(doc.OffsetSeconds, 9.99, 1e-6) || !ntpNear(doc.DelaySeconds, 0.08, 1e-6) || doc.ReferenceID != "GPS" {
		t.Errorf("offset %g, delay %g, reference id %q", doc.OffsetSeconds, doc.DelaySeconds, doc.ReferenceID)
	}
	if !ntpNear(doc.TransmitTime, float64(wall.Unix())+10.05, 1e-6) || !ntpNear(doc.ReferenceTime, float64(wall.Unix())-16, 1e-6) || doc.ReferenceTimeRFC3339 != "2026-10-27T11:59:44Z" {
		t.Errorf("transmit %f, reference %f %s", doc.TransmitTime, doc.ReferenceTime, doc.ReferenceTimeRFC3339)
	}
	behind := ntpDescribe(ntpPacket{stratum: 3, receive: t1 - seconds(2), transmit: t1 - seconds(2)}, wall, 20*time.Millisecond)
	if !ntpNear(behind.OffsetSeconds, -2.01, 1e-6) || !ntpNear(behind.DelaySeconds, 0.02, 1e-6) {
		t.Errorf("behind: offset %g, delay %g", behind.OffsetSeconds, behind.DelaySeconds)
	}
	if id := ntpReferenceID(1, [4]byte{0x01, 'P', 'S', 0}); id != "0x01505300" {
		t.Errorf("reference id %q", id)
	}
	if doc := ntpDescribe(ntpPacket{stratum: 2, receive: t1, transmit: t1}, wall, 0); doc.ReferenceTime != 0 || doc.ReferenceTimeRFC3339 != "" {
		t.Errorf("a reference timestamp of 0 is %g %q", doc.ReferenceTime, doc.ReferenceTimeRFC3339)
	}
}

// Across the end of NTP era 0, on 2036-02-07 at 06:28:16 UTC, timestamps
// count from 0 again: a server 10 seconds ahead of an exporter 6 seconds
// before the end is in era 1, and its offset is still 10 seconds, its
// transmit time 10 seconds after the exporter's, not 136 years before.
func TestNTPTimestampsAreReadAcrossTheEndOfAnEra(t *testing.T) {
	end := time.Date(2036, 2, 7, 6, 28, 16, 0, time.UTC)
	if ntpTimestamp(end) != 0 || ntpTimestamp(end.Add(-time.Second)) != 0xffffffff<<32 {
		t.Fatalf("the end of era 0 is %x", ntpTimestamp(end))
	}
	exporter := end.Add(-6 * time.Second)
	if got := ntpTime(ntpTimestamp(end.Add(4*time.Second)), exporter); !got.Equal(end.Add(4 * time.Second)) {
		t.Errorf("4s into era 1 read as %s", got)
	}
	if got := ntpTime(ntpTimestamp(exporter.Add(-time.Hour)), end.Add(time.Hour)); !got.Equal(exporter.Add(-time.Hour)) {
		t.Errorf("an hour before the end of era 0 read from era 1 as %s", got)
	}
	defer func(saved func() time.Time) { ntpNow = saved }(ntpNow)
	ntpNow = func() time.Time { return exporter }
	// The stub answers from a clock of its own, which reads 10 seconds
	// ahead of the exporter's as the request comes; it reads nothing the
	// test restores afterwards.
	s := newNTPStub(t, ntpAnswering(func() ntpReply { return ntpReplyAt(exporter.Add(10 * time.Second)) }))
	doc, err := ntpDoc(t, ntpCollector(t), s.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ntpNear(doc.OffsetSeconds, 10, 0.5) || doc.DelaySeconds < 0 || doc.DelaySeconds > 1 || !ntpNear(doc.TransmitTime, float64(end.Unix())+4, 1e-6) || doc.TransmitTimeRFC3339 != "2036-02-07T06:28:20Z" {
		t.Errorf("offset %g, delay %g, transmit %f %s", doc.OffsetSeconds, doc.DelaySeconds, doc.TransmitTime, doc.TransmitTimeRFC3339)
	}
}

// A datagram that is not the reply to the request is ignored and the reply
// waited for: one whose origin timestamp is another, which would otherwise
// say the server is 1000 seconds ahead, is passed over for the reply after
// it. When only such datagrams come, the scrape fails at its deadline,
// saying how many were ignored and why the last was.
func TestAnNTPReplyThatIsNotTheRequestsIsIgnored(t *testing.T) {
	forged := uint64(0x0123456789abcdef)
	wrongOrigin := func() ntpReply { r := goodNTPReply(1000 * time.Second); r.origin = &forged; return r }
	s := newNTPStub(t, ntpAnswering(wrongOrigin, func() ntpReply { return goodNTPReply(0) }))
	doc, err := ntpDoc(t, ntpCollector(t), s.addr, 5*time.Second)
	if err != nil || !ntpNear(doc.OffsetSeconds, 0, 0.5) {
		t.Fatalf("offset %g, %v", doc.OffsetSeconds, err)
	}
	for why, bad := range map[string]func() ntpReply{
		"its origin timestamp is not the request's": wrongOrigin,
		"it is not a server's reply (mode 4 or 5)":  func() ntpReply { r := goodNTPReply(0); r.mode = ntpModeClient; return r },
		"it is of an NTP version other than 3 or 4": func() ntpReply { r := goodNTPReply(0); r.version = 2; return r },
		"it has no transmit timestamp":              func() ntpReply { r := goodNTPReply(0); r.transmit = 0; return r },
		"it is shorter than an NTP packet":          func() ntpReply { r := goodNTPReply(0); r.short = 47; return r },
	} {
		s := newNTPStub(t, ntpAnswering(bad, bad))
		_, err := ntpDoc(t, ntpCollector(t), s.addr, 300*time.Millisecond)
		if want := "no reply from " + s.addr + " before the deadline; 2 datagrams were ignored, the last as " + why; err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: %v", why, err)
		}
	}
	version3 := newNTPStub(t, ntpAnswering(func() ntpReply { r := goodNTPReply(0); r.version = 3; return r }))
	if doc, err := ntpDoc(t, ntpCollector(t), version3.addr, 5*time.Second); err != nil || doc.Version != 3 {
		t.Errorf("version 3: %+v, %v", doc, err)
	}
}

// A server that says it does not keep the time fails the scrape: a
// Kiss-o'-Death, named by its code, which a probe's caller is told when
// RFC 5905 lists it and is withheld from otherwise; leap indicator 3 and
// stratum 16, an unsynchronised server; a stratum past 16.
func TestAnNTPServerThatDoesNotKeepTheTimeFailsTheScrape(t *testing.T) {
	for name, tc := range map[string]struct {
		edit         func(*ntpReply)
		want, caller string
	}{
		"RATE":       {func(r *ntpReply) { r.stratum, r.referenceID = 0, [4]byte{'R', 'A', 'T', 'E'} }, "answered a Kiss-o'-Death, RATE: rate exceeded", "answered a Kiss-o'-Death, RATE: rate exceeded"},
		"DENY":       {func(r *ntpReply) { r.stratum, r.referenceID = 0, [4]byte{'D', 'E', 'N', 'Y'} }, "answered a Kiss-o'-Death, DENY: access denied", "Kiss-o'-Death, DENY"},
		"unknown":    {func(r *ntpReply) { r.stratum, r.referenceID = 0, [4]byte{'Q', 'Z', 'X', 0} }, `answered a Kiss-o'-Death, "QZX", a code RFC 5905 does not list`, "Kiss-o'-Death, " + model.WithheldMark},
		"leap 3":     {func(r *ntpReply) { r.leap = 3 }, "is not synchronised: its leap indicator is 3", "is not synchronised: its leap indicator is 3"},
		"stratum 16": {func(r *ntpReply) { r.stratum = 16 }, "is not synchronised: its stratum is 16", "its stratum is 16"},
		"stratum 99": {func(r *ntpReply) { r.stratum = 99 }, "answered stratum 99, which no server has", "answered stratum " + model.WithheldMark},
	} {
		s := newNTPStub(t, ntpAnswering(func() ntpReply { r := goodNTPReply(0); tc.edit(&r); return r }))
		start := time.Now()
		_, err := ntpDoc(t, ntpCollector(t), s.addr, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), s.addr+" "+tc.want) || !strings.Contains(model.CallerText(err), tc.caller) {
			t.Errorf("%s: %v (caller: %v)", name, err, model.CallerText(err))
		}
		if waited := time.Since(start); waited > 3*time.Second {
			t.Errorf("%s: failed after %s, not at the reply", name, waited)
		}
	}
}

// A server that never answers fails the scrape at its deadline, which
// bounds the whole request, after the one request: nothing is sent again.
// A port nothing listens on fails it at once, as the ICMP refusal comes.
func TestAnNTPServerThatDoesNotAnswerFailsAtTheDeadline(t *testing.T) {
	s := newNTPStub(t, func([]byte) [][]byte { return nil })
	start := time.Now()
	_, err := ntpDoc(t, ntpCollector(t), s.addr, 300*time.Millisecond)
	if waited := time.Since(start); err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no reply from "+s.addr+" before the deadline") || waited > 3*time.Second {
		t.Errorf("after %s: %v", waited, err)
	}
	if got := len(s.received()); got != 1 {
		t.Errorf("%d requests sent; want one", got)
	}
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.LocalAddr().String()
	_ = closed.Close()
	start = time.Now()
	_, err = ntpDoc(t, ntpCollector(t), addr, 5*time.Second)
	if waited := time.Since(start); err == nil || !strings.Contains(err.Error(), "reading the reply from "+addr) || waited > 3*time.Second {
		t.Errorf("a closed port: after %s: %v", waited, err)
	}
}

// allowed_targets and denied_targets hold the request to them: a denied
// address, one allowed_targets does not list, and a name whose address the
// socket is connected to is denied, are refused, and nothing is sent.
func TestAnNTPCollectorKeepsToItsTargetLists(t *testing.T) {
	s := newNTPStub(t, ntpAnswering(func() ntpReply { return goodNTPReply(0) }))
	_, port, _ := net.SplitHostPort(s.addr)
	loopback := []string{"127.0.0.0/8", "::1/128"}
	for name, tc := range map[string]struct {
		target string
		edit   func(*model.Collector)
	}{
		"denied":           {s.addr, func(c *model.Collector) { c.Request.DeniedTargets = loopback }},
		"not allowed":      {s.addr, func(c *model.Collector) { c.Request.AllowedTargets = []string{"10.0.0.0/8"} }},
		"denied by a name": {net.JoinHostPort("localhost", port), func(c *model.Collector) { c.Request.DeniedTargets = loopback }},
	} {
		if _, err := ntpDoc(t, ntpCollector(t, tc.edit), tc.target, 2*time.Second); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := len(s.received()); got != 0 {
		t.Errorf("a refused target was sent %d requests", got)
	}
	if _, err := ntpDoc(t, ntpCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = []string{"127.0.0.0/8"} }), s.addr, 5*time.Second); err != nil {
		t.Errorf("allowed: %v", err)
	}
}

// An ntp collector takes no key of its own and of the common part only
// the target lists; a target is host or host:port, port 123 when left out,
// and anything else is refused before anything is sent. The url label is
// ntp://host:port, the http_method label QUERY, and a debug probe's request
// section shows the request and how it ended.
func TestAnNTPCollectorsKeysAndTargetsAreChecked(t *testing.T) {
	for name, request := range map[string]model.RequestConfig{
		"path": {Type: RequestTypeNTP, Path: "/x"},
		"tls":  {Type: RequestTypeNTP, TLS: model.TLSConfig{CAFile: "/ca.pem"}},
	} {
		if err := ValidateRequest(&model.Collector{Name: "clock", Request: request}); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	c := ntpCollector(t)
	for _, target := range []string{"", "ntp://time.example.test", "time.example.test/x", "time.example.test:0", "time.example.test:ntp", "user@time.example.test", "2001:db8::1:123:x:", ":123", "a b:123"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("%q: taken", target)
		}
	}
	for target, label := range map[string]string{"time.example.test": "ntp://time.example.test:123", "Time.Example.Test:1123": "ntp://time.example.test:1123", "[2001:db8::1]:123": "ntp://[2001:db8::1]:123", "192.0.2.1": "ntp://192.0.2.1:123"} {
		if err := CheckTarget(c, target, false); err != nil {
			t.Errorf("%q: %v", target, err)
		}
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
			t.Errorf("%q: label %q, %v", target, got, err)
		}
	}
	if method, stage := RequestMethodFor(c, RequestOverrides{}), FetchStage(c); method != "QUERY" || stage != "ntp" {
		t.Errorf("method %q, stage %q", method, stage)
	}
	if shown := DisplayTarget(c, "ntp://user:secret@time.example.test"); shown != "<invalid target>" {
		t.Errorf("shown %q", shown)
	}
	s := newNTPStub(t, ntpAnswering(func() ntpReply { return goodNTPReply(0) }))
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, s.addr, c, RequestOverrides{Timeout: 5 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	requests := trace.Requests()
	if len(requests) != 1 {
		t.Fatalf("traced %+v", requests)
	}
	if r := requests[0]; r.Method != "QUERY" || r.URL != "ntp://"+s.addr || r.Header.Get("Ntp-Version") != "4" || !strings.HasPrefix(r.Outcome, "stratum 2, offset ") {
		t.Errorf("traced %+v", r)
	}
}
