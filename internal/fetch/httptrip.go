package fetch

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// HTTPTrip is what an http or graphite collector's answer says of the
// request that brought it, beside its status, headers and body: how long
// each phase of that request took. It is of the request whose status the
// answer has: the last attempt of a retried trip, the request a redirect
// led to, a 304 that revalidated a cached response, and of a paginated
// trip the first page, or the page whose status failed it.
type HTTPTrip struct {
	// DNS, Connect and TLS are how long the name's lookup, the connection
	// and the TLS handshake took, nil for a phase the request did not
	// have: on a connection it reused, none of them; over plain HTTP, no
	// handshake; to an IP address or a Unix socket, no lookup. Through a
	// proxy, the lookup and the connection are the proxy's.
	DNS, Connect, TLS *time.Duration
	// FirstByte is the time from the request's start, when it asked for a
	// connection, to the first byte of the answer; nil for none.
	FirstByte *time.Duration
	// Total is the request's whole time, from its start to the end of
	// reading the body.
	Total time.Duration
}

// JQTimings is the trip's phases as jq's $timings and Python's
// response.timings have them: seconds as numbers, null for a phase the
// request did not have, and null as a whole for no trip.
func (t *HTTPTrip) JQTimings() any {
	if t == nil {
		return nil
	}
	return map[string]any{
		"dns":        tripSeconds(t.DNS),
		"connect":    tripSeconds(t.Connect),
		"tls":        tripSeconds(t.TLS),
		"first_byte": tripSeconds(t.FirstByte),
		"total":      t.Total.Seconds(),
	}
}

func tripSeconds(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.Seconds()
}

// tripTrace times the phases of one attempt's requests through
// net/http/httptrace. A redirect is a request of its own, which asks for a
// connection again (GetConn), so each GetConn starts the timings anew and
// what is left at the end is the last request's, the one answered. The
// hooks may be called from the transport's own goroutines, a dial that
// tries several addresses among them, hence the lock.
type tripTrace struct {
	mu                  sync.Mutex
	start               time.Time
	dnsStart, dialStart time.Time
	handshakeStart      time.Time
	dns, connect, tls   *time.Duration
	firstByte           *time.Duration
	connected           bool
}

func newTripTrace(start time.Time) *tripTrace { return &tripTrace{start: start} }

// hooks are the trace's hooks. A phase is taken only before the request
// has its connection (GotConn): a dial still going on for an earlier
// request, or one the transport began but did not use, says nothing of the
// connection this request was sent on.
func (t *tripTrace) hooks() *httptrace.ClientTrace {
	since := func(start time.Time) *time.Duration {
		d := time.Since(start)
		return &d
	}
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.start, t.dnsStart, t.dialStart, t.handshakeStart = time.Now(), time.Time{}, time.Time{}, time.Time{}
			t.dns, t.connect, t.tls, t.firstByte, t.connected = nil, nil, nil, nil, false
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connected && t.dnsStart.IsZero() {
				t.dnsStart = time.Now()
			}
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connected && !t.dnsStart.IsZero() && t.dns == nil && info.Err == nil {
				t.dns = since(t.dnsStart)
			}
		},
		// A dial that tries several addresses starts several connections:
		// the phase is from the first start to the first that succeeded.
		ConnectStart: func(string, string) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connected && t.dialStart.IsZero() {
				t.dialStart = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connected && !t.dialStart.IsZero() && t.connect == nil && err == nil {
				t.connect = since(t.dialStart)
			}
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connected && t.handshakeStart.IsZero() {
				t.handshakeStart = time.Now()
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connected && !t.handshakeStart.IsZero() && t.tls == nil && err == nil {
				t.tls = since(t.handshakeStart)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.connected = true
			if info.Reused {
				// Whatever was timed was not of this connection.
				t.dns, t.connect, t.tls = nil, nil, nil
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.firstByte == nil {
				t.firstByte = since(t.start)
			}
		},
	}
}

// trip is what was timed of the answered request, which ended at end, when
// its body had been read.
func (t *tripTrace) trip(_ *http.Response, end time.Time) *HTTPTrip {
	t.mu.Lock()
	defer t.mu.Unlock()
	return &HTTPTrip{DNS: t.dns, Connect: t.connect, TLS: t.tls, FirstByte: t.firstByte, Total: end.Sub(t.start)}
}
