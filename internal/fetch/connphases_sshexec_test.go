//go:build !select_request_types || request_type_sshexec

package fetch

import (
	"context"
	"testing"
	"time"
)

// An sshexec command's request on a connection made for it has the
// connection's phases, its lookup, connection and SSH handshake; the next
// scrape's, on the connection kept, has none.
func TestAnSSHExecConnectionsPhasesAreTheScrapesThatMadeIt(t *testing.T) {
	f := newExecFixture(t, scripted("ok\n", "", 0))
	c := f.collector(t, "uptime", []string{"uptime"})
	first := spanTracedRequests(t, c, f.server.Addr)
	if len(first) != 1 || phaseNames(t, first[0]) != "connection.connect,connection.handshake" {
		t.Errorf("the first scrape: %+v", first)
	}
	again := spanTracedRequests(t, c, f.server.Addr)
	if len(again) != 1 || len(again[0].Phases) != 0 {
		t.Errorf("the next scrape: %+v", again)
	}
}

// A connection's phases are the request's of the scrape whose trace began
// making it alone: another scrape that came to wait for it, not reused yet
// when it asked, did not wait for all of it.
func TestAnSSHConnectionsPhasesAreOnlyItsMakersScrapes(t *testing.T) {
	maker, makerTrace := WithSpanTrace(context.Background())
	other, otherTrace := WithSpanTrace(context.Background())
	began := time.Now()
	entry := &sshConn{phases: []TracedPhase{{Name: "connection.connect", Start: began, End: began.Add(time.Millisecond)}}}
	entry.trace, _ = maker.Value(requestTraceKey{}).(*RequestTrace)
	traceSSH(maker, sshExecMethod, sshConnKey{}, "ssh://h", nil, entry, false, nil)
	traceSSH(other, sshExecMethod, sshConnKey{}, "ssh://h", nil, entry, false, nil)
	if got := makerTrace.Requests()[0].Phases; len(got) != 1 {
		t.Errorf("the maker's request: %+v", got)
	}
	if got := otherTrace.Requests()[0].Phases; len(got) != 0 {
		t.Errorf("a waiter's request: %+v", got)
	}
}
