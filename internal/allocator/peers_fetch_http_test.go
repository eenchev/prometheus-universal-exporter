//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A leader that hangs while it writes its snapshot delays none of the
// follower's checks: the fetch runs beside the rounds, one at a time, so
// the follower sees the leader shutting down at its next check and leads at
// once, cancelling the fetch, rather than after SnapshotTimeout.
func TestAHangingSnapshotDoesNotDelayFailover(t *testing.T) {
	var shuttingDown atomic.Bool
	var fetches atomic.Int32
	cancelled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PeerPath {
			if shuttingDown.Load() {
				http.Error(w, "the allocator is shutting down", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"instance":"leader","discovering":true,"address_known":true}`)
			return
		}
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"instance":"leader",`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	leader := netip.MustParseAddrPort(srv.Listener.Addr().String())

	c := newPeerCopy(t, writeInputs(t, testConfig, "", twoJobs(stableSD(t).URL, false)), nil)
	followFake(c, leader)
	round := func(when string) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			c.a.peers.round(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s, a round has not ended after 5s: it waits for the hanging snapshot", when)
		}
	}
	round("following the leader")
	hasLine(t, c, fmt.Sprintf("pue_allocator_leader{peer=%q} 1\n", leader.String()))
	testutil.WaitFor(t, "the fetch to reach the leader", func() bool { return fetches.Load() == 1 })
	round("with the snapshot fetch still running")
	if n := fetches.Load(); n != 1 {
		t.Errorf("the leader was asked for its snapshot %d times while the first fetch ran, want once", n)
	}
	shuttingDown.Store(true)
	round("the leader shutting down")
	hasLine(t, c, "pue_allocator_is_leader 1\n")
	if !c.discovering() {
		t.Error("the copy leads but does not ask its sources")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the fetch of the old leader's snapshot was not cancelled")
	}
	if strings.Contains(c.metrics(t), `pue_allocator_peer_failures_total{request="snapshot"} 1`) {
		t.Error("a fetch cancelled because the leader changed is counted as a failure")
	}
}
