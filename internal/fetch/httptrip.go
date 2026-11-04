package fetch

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// HTTPTrip is what an http or graphite collector's answer says of the
// request that brought it, beside its status, headers and body: how long
// each phase of that request took, the TLS connection it was sent on, its
// URL and the redirects that led to it. It is of the request whose status the
// answer has: the last attempt of a retried trip, the request a redirect
// led to, a 304 that revalidated a cached response, and of a paginated
// trip the first page, or the page whose status failed it.
type HTTPTrip struct {
	// DNS, Connect and TLS are how long the name's lookup, the connection
	// and the TLS handshake took, nil for a phase the request did not
	// have: on a connection it reused, none of them; over plain HTTP, no
	// handshake; to an IP address or a Unix socket, no lookup. Through a
	// proxy, the lookup and the connection are the proxy's, and the
	// handshake the target's through the tunnel, never the one with an
	// https:// proxy (proxyTLSDialer).
	DNS, Connect, TLS *time.Duration
	// FirstByte is the time from the request's start, when it asked for a
	// connection, to the first byte of the answer; nil for none.
	FirstByte *time.Duration
	// Total is the request's whole time, from its start to the end of
	// reading the body.
	Total time.Duration
	// TLSConnection is the TLS connection the request was sent on, nil
	// over plain HTTP.
	TLSConnection *TLSConnection
	// URL is the request's URL, where the redirects led, as it was sent:
	// ShownURL is what a rule reads of it. It is kept as it is, and shown
	// only when it is read, since most collectors' rules never read it.
	URL *url.URL
	// Redirects is how many redirects were followed to it, 0 for none.
	Redirects int
}

// JQURL is the URL as jq's $url and Python's response.url have it, null for
// no trip.
func (t *HTTPTrip) JQURL() any {
	if t == nil {
		return nil
	}
	return t.ShownURL()
}

// ShownURL is the request's URL with its userinfo and fragment left out
// and the values of the query parameters whose names read as credentials
// masked, as a target is shown in a log line (MaskCredentialQueryValues);
// "" for none.
func (t *HTTPTrip) ShownURL() string {
	if t == nil || t.URL == nil {
		return ""
	}
	return shownURL(t.URL)
}

// JQRedirects is the number of redirects as jq's $redirects and Python's
// response.redirects have it, null for no trip.
func (t *HTTPTrip) JQRedirects() any {
	if t == nil {
		return nil
	}
	return t.Redirects
}

// shownURL is u as $url has it: without userinfo, which a label would keep,
// rather than the redacted:redacted a log line shows in its place, and
// otherwise as a target is shown in one.
func shownURL(u *url.URL) string {
	shown := *u
	shown.User = nil
	return RedactURL(&shown, MaskCredentialQueryValues)
}

// redirectsTo counts the redirects that led to req: each request a redirect
// made carries the answer that asked for it, which carries the request
// before.
func redirectsTo(req *http.Request) int {
	n := 0
	for ; req.Response != nil && req.Response.Request != nil; req = req.Response.Request {
		n++
	}
	return n
}

// TLSConnection is what the exporter knows of a TLS connection to a target
// and of the certificate the target presented on it, nothing secret: the
// names follow the tlscert type's document (tls_version, cipher_suite,
// server_name, verified, not_before, not_after, subject_cn, issuer_cn), so
// a rule reads the two alike.
type TLSConnection struct {
	// TLSVersion is the protocol's version as the tlscert document writes
	// it, "TLS 1.3".
	TLSVersion string
	// CipherSuite is the cipher suite's IANA name.
	CipherSuite string
	// ServerName is the name sent as SNI, "" when none was, as for an
	// IP address.
	ServerName string
	// Verified says the exporter verified the chain: false under
	// insecure_skip_verify.
	Verified bool
	// NotBefore and NotAfter are the leaf's validity, in Unix seconds.
	NotBefore, NotAfter int64
	// SubjectCN and IssuerCN are the common names of the leaf's subject
	// and issuer.
	SubjectCN, IssuerCN string
}

// tlsConnectionOf is what state says of the connection a request was
// answered on; nil for a request over plain HTTP. Through an https://
// proxy, the state is the target's handshake through the tunnel, and
// there is none for a plain HTTP target: the proxy's own connection is
// handed to the transport as no TLS connection (proxyTLSDialer).
func tlsConnectionOf(state *tls.ConnectionState) *TLSConnection {
	if state == nil {
		return nil
	}
	c := &TLSConnection{TLSVersion: tls.VersionName(state.Version), CipherSuite: tls.CipherSuiteName(state.CipherSuite), ServerName: state.ServerName, Verified: len(state.VerifiedChains) > 0}
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		c.NotBefore, c.NotAfter = leaf.NotBefore.Unix(), leaf.NotAfter.Unix()
		c.SubjectCN, c.IssuerCN = leaf.Subject.CommonName, leaf.Issuer.CommonName
	}
	return c
}

// JQTLS is the TLS connection as jq's $tls and Python's response.tls have
// it, null over plain HTTP and for no trip. The times are numbers, as
// $timings' are.
func (t *HTTPTrip) JQTLS() any {
	if t == nil || t.TLSConnection == nil {
		return nil
	}
	c := t.TLSConnection
	return map[string]any{
		"tls_version":  c.TLSVersion,
		"cipher_suite": c.CipherSuite,
		"server_name":  c.ServerName,
		"verified":     c.Verified,
		"not_before":   float64(c.NotBefore),
		"not_after":    float64(c.NotAfter),
		"subject_cn":   c.SubjectCN,
		"issuer_cn":    c.IssuerCN,
	}
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
// its body had been read, and the connection resp came on. A nil t is an
// attempt that was not timed (fetch says when), begun at start: its trip
// has no phases, and its total is the whole attempt's, its redirects
// included.
func (t *tripTrace) trip(resp *http.Response, start, end time.Time) *HTTPTrip {
	trip := &HTTPTrip{Total: end.Sub(start)}
	if t != nil {
		t.mu.Lock()
		trip.DNS, trip.Connect, trip.TLS, trip.FirstByte, trip.Total = t.dns, t.connect, t.tls, t.firstByte, end.Sub(t.start)
		t.mu.Unlock()
	}
	trip.TLSConnection = tlsConnectionOf(resp.TLS)
	if resp.Request != nil {
		trip.URL, trip.Redirects = resp.Request.URL, redirectsTo(resp.Request)
	}
	return trip
}
