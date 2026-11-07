package fetch

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
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
	// ProtoMajor and ProtoMinor are the version of HTTP the answer came
	// in, 1 and 1 for HTTP/1.1, 2 and 0 for HTTP/2; 0 and 0 for none.
	ProtoMajor, ProtoMinor int
	// RemoteIP is the address the request's connection was made to, the
	// zero Addr when it is not known: a request that was not traced (fetch
	// says when), one through a proxy, whose connection is to the proxy and
	// not to the target, and one over a Unix socket, which has no address.
	RemoteIP netip.Addr
}

// JQHTTPVersion is the version of HTTP the answer came in, as jq's
// $http_version and Python's response.http_version have it: "1.0", "1.1"
// or "2", as blackbox_exporter's probe_http_version writes it; null for no
// trip.
func (t *HTTPTrip) JQHTTPVersion() any {
	if t == nil || t.ProtoMajor == 0 {
		return nil
	}
	switch {
	case t.ProtoMajor == 1 && t.ProtoMinor == 1:
		return "1.1"
	case t.ProtoMajor == 1 && t.ProtoMinor == 0:
		return "1.0"
	case t.ProtoMajor >= 2:
		// HTTP/2 has no minor version: Go writes it HTTP/2.0.
		return strconv.Itoa(t.ProtoMajor)
	}
	return strconv.Itoa(t.ProtoMajor) + "." + strconv.Itoa(t.ProtoMinor)
}

// JQRemoteIP is the address the request's connection was made to, as jq's
// $remote_ip and Python's response.remote_ip have it: "203.0.113.7" or
// "2001:db8::7", an IPv4 address that came as one mapped into IPv6 written
// as IPv4; null when it is not known (RemoteIP) and for no trip.
func (t *HTTPTrip) JQRemoteIP() any {
	if t == nil || !t.RemoteIP.IsValid() {
		return nil
	}
	return t.RemoteIP.String()
}

// remoteIP is the IP address of addr, a connection's remote address: the
// zero Addr for one that is no TCP address, as a Unix socket's.
func remoteIP(addr net.Addr) netip.Addr {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp == nil {
		return netip.Addr{}
	}
	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return netip.Addr{}
	}
	return ip.Unmap()
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
// net/http/httptrace, and keeps the address of the connection each was sent
// on. A redirect is a request of its own, which asks for a connection again
// (GetConn), so each GetConn starts the timings anew and what is left at the
// end is the last request's, the one answered. The hooks may be called from
// the transport's own goroutines, a dial that tries several addresses among
// them, hence the lock.
type tripTrace struct {
	mu                  sync.Mutex
	start               time.Time
	dnsStart, dialStart time.Time
	handshakeStart      time.Time
	dns, connect, tls   *time.Duration
	firstByte           *time.Duration
	connected           bool
	// remote is the remote address of the connection the request got,
	// kept or new (GotConn).
	remote net.Addr
	// ctx is the context of the trip's requests, whose request trace, an
	// exported trip's spans or a debug run's, is given each request's
	// phases (tracePhases) once they are known: at the answer's first
	// byte, or when the request failed before it (recordPhases).
	ctx context.Context
	// given says the phases held are given already, to the request that
	// was answered: a redirect the trip then refused, recorded but never
	// sent, asked for no connection (GetConn), and has none of them.
	given bool
}

func newTripTrace(ctx context.Context, start time.Time) *tripTrace {
	return &tripTrace{ctx: ctx, start: start}
}

// phasesLocked are the phases timed of the request being sent, each with
// its start and end, in the order they began; under t.mu.
func (t *tripTrace) phasesLocked() []TracedPhase {
	var phases []TracedPhase
	add := func(name string, start time.Time, d *time.Duration) {
		if d != nil && !start.IsZero() {
			phases = append(phases, TracedPhase{Name: name, Start: start, End: start.Add(*d)})
		}
	}
	add("dns", t.dnsStart, t.dns)
	add("connect", t.dialStart, t.connect)
	add("tls", t.handshakeStart, t.tls)
	add("first_byte", t.start, t.firstByte)
	return phases
}

// recordPhases gives the request being sent its phases timed so far, as
// one that failed before its first byte has them: a lookup or a
// connection that went through before what failed. The phases of a
// request answered (given) are its own, not those of a redirect of it the
// trip refused.
func (t *tripTrace) recordPhases() {
	if t == nil {
		return
	}
	t.mu.Lock()
	phases := t.phasesLocked()
	if t.given {
		phases = nil
	}
	t.mu.Unlock()
	tracePhases(t.ctx, phases)
}

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
			t.dns, t.connect, t.tls, t.firstByte, t.connected, t.remote, t.given = nil, nil, nil, nil, false, nil, false
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
			if info.Conn != nil {
				t.remote = info.Conn.RemoteAddr()
			}
			if info.Reused {
				// Whatever was timed was not of this connection.
				t.dns, t.connect, t.tls = nil, nil, nil
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			if t.firstByte == nil {
				t.firstByte = since(t.start)
			}
			phases := t.phasesLocked()
			t.given = true
			t.mu.Unlock()
			// The request's own, before a redirect's GetConn starts
			// another's.
			tracePhases(t.ctx, phases)
		},
	}
}

// trip is what was timed of the answered request, which ended at end, when
// its body had been read, and the connection resp came on. A nil t is an
// attempt that was not timed (fetch says when), begun at start: its trip
// has no phases and no remote address, and its total is the whole
// attempt's, its redirects included.
func (t *tripTrace) trip(resp *http.Response, start, end time.Time) *HTTPTrip {
	trip := &HTTPTrip{Total: end.Sub(start), ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor}
	if t != nil {
		t.mu.Lock()
		trip.DNS, trip.Connect, trip.TLS, trip.FirstByte, trip.Total = t.dns, t.connect, t.tls, t.firstByte, end.Sub(t.start)
		trip.RemoteIP = remoteIP(t.remote)
		t.mu.Unlock()
	}
	trip.TLSConnection = tlsConnectionOf(resp.TLS)
	if resp.Request != nil {
		trip.URL, trip.Redirects = resp.Request.URL, redirectsTo(resp.Request)
	}
	return trip
}
