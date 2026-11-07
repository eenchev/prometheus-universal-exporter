//go:build request_type_sshexec

package fetch

import (
	"testing"
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
