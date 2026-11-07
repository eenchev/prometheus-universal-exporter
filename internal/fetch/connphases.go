//go:build !select_request_types || request_type_tcp || request_type_ldap || request_type_redis || request_type_tlscert || request_type_sshexec || request_type_sshfile || request_type_grpc

package fetch

import (
	"context"
	"net/http/httptrace"
	"sync"
	"time"
)

// connTiming times the phases of a connection a request type other than
// http makes itself, as an http request's are timed (tripTrace): the
// name's lookup and the connection, from the net package's hooks, which a
// dial with the context of withConnTiming calls, and the TLS or SSH
// handshake, which the type times itself (handshake). The phases are its
// request's span's children: of the request sent when the connection was
// made (tracePhases), or of the first request sent on it once it is
// (traceNextPhases). A nil connTiming, of a trip whose requests are not
// recorded, times nothing.
type connTiming struct {
	mu                           sync.Mutex
	dnsStart, dnsEnd             time.Time
	dialStart, dialEnd           time.Time
	handshakeStart, handshakeEnd time.Time
	handshakeName                string
}

// connTimingKey holds a context's connTiming, which its handshakes are
// timed by (connTimingOf).
type connTimingKey struct{}

// connTimingOf is the connTiming ctx's dials are timed by, nil when none.
func connTimingOf(ctx context.Context) *connTiming {
	t, _ := ctx.Value(connTimingKey{}).(*connTiming)
	return t
}

// withConnTiming returns a context whose dials are timed by the returned
// connTiming, which connTimingOf finds in it; nil and ctx itself when ctx
// records no request.
func withConnTiming(ctx context.Context) (context.Context, *connTiming) {
	if !tracing(ctx) {
		return ctx, nil
	}
	return timeConns(ctx)
}

// timeConns returns a context whose dials are timed by the returned
// connTiming, whether ctx records requests or not: a connection kept for
// other scrapes is timed for whichever of them is waiting for it.
func timeConns(ctx context.Context) (context.Context, *connTiming) {
	t := &connTiming{}
	ctx = context.WithValue(ctx, connTimingKey{}, t)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.dnsStart.IsZero() {
				t.dnsStart = time.Now()
			}
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.dnsEnd = time.Now()
		},
		// A dial that tries several addresses starts each: the phase is
		// from the first's start to the one that connected.
		ConnectStart: func(string, string) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.dialStart.IsZero() {
				t.dialStart = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			if err != nil {
				return
			}
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.dialEnd.IsZero() {
				t.dialEnd = time.Now()
			}
		},
	}), t
}

// handshake runs a handshake, tls or ssh as name says, timing it.
func (t *connTiming) handshake(name string, run func() error) error {
	if t == nil {
		return run()
	}
	start := time.Now()
	err := run()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.handshakeName, t.handshakeStart, t.handshakeEnd = name, start, time.Now()
	}
	return err
}

// phases are the phases timed that ended, in the order they began, each
// named connection.<phase>, the name of its span.
func (t *connTiming) phases() []TracedPhase {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var phases []TracedPhase
	add := func(name string, start, end time.Time) {
		if !start.IsZero() && !end.IsZero() {
			phases = append(phases, TracedPhase{Name: name, Start: start, End: end})
		}
	}
	add("connection.dns", t.dnsStart, t.dnsEnd)
	add("connection.connect", t.dialStart, t.dialEnd)
	add("connection."+t.handshakeName, t.handshakeStart, t.handshakeEnd)
	return phases
}

// traceNextPhases holds the phases of a connection just made for the next
// request recorded, the first sent on it, whose span then begins with them.
func traceNextPhases(ctx context.Context, phases []TracedPhase) {
	if len(phases) == 0 {
		return
	}
	if t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace); ok {
		t.mu.Lock()
		t.pending = phases
		t.mu.Unlock()
	}
}

// traceAddPhases adds phases to the last request recorded, as a TLS
// handshake that STARTTLS began on the connection the request was sent
// on.
func traceAddPhases(ctx context.Context, phases []TracedPhase) {
	if len(phases) == 0 {
		return
	}
	if t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace); ok {
		t.mu.Lock()
		defer t.mu.Unlock()
		if n := len(t.requests); n > 0 {
			r := &t.requests[n-1]
			r.Phases = append(r.Phases, phases...)
			r.started = earlier(r.started, phases[0].Start)
		}
	}
}

// tracing reports whether ctx records its requests, for a debug run or an
// exported trip's spans.
func tracing(ctx context.Context) bool {
	_, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	return ok
}
