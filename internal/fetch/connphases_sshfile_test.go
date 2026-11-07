//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"os"
	"path/filepath"
	"testing"
)

// An sshfile read on a connection made for it has the connection's phases,
// its lookup, connection and SSH handshake, the SFTP session not among
// them; the next scrape's, on the connection kept, has none.
func TestAnSSHFileConnectionsPhasesAreTheScrapesThatMadeIt(t *testing.T) {
	f := newSSHFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "metrics.prom"), []byte("up 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := f.collector(t, "files", "metrics.prom")
	first := spanTracedRequests(t, c, f.server.Addr)
	if len(first) != 1 || phaseNames(t, first[0]) != "connection.connect,connection.handshake" {
		t.Errorf("the first scrape: %+v", first)
	}
	again := spanTracedRequests(t, c, f.server.Addr)
	if len(again) != 1 || len(again[0].Phases) != 0 {
		t.Errorf("the next scrape: %+v", again)
	}
}
