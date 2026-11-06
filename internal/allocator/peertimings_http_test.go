//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// usePeersWith is usePeers with the timings of o.
func usePeersWith(o PeerOptions, instances ...*peerInstance) {
	var list []string
	for _, c := range instances {
		list = append(list, c.addr.String())
	}
	for _, c := range instances {
		o.Peers, o.Self, o.Token = list, c.addr, func() (string, error) { return peerToken, nil }
		c.a.UsePeers(o)
	}
}

// A follower whose leader stops answering leads after FailureThreshold
// failed checks, CheckInterval apart: with checks every 100ms and a
// threshold of 5, from 400ms after the leader's listener closes, four
// intervals after the first failed check, and within five intervals and
// 2s of slack, sooner than the defaults' three 2s rounds could.
func TestAFollowerLeadsAfterTheConfiguredFailedChecks(t *testing.T) {
	const interval, threshold = 100 * time.Millisecond, 5
	srv := newSDServer(t)
	p := peerInputs(t, srv)
	leader, follower := sortedPair(newPeerInstance(t, p, nil), newPeerInstance(t, p, nil))
	usePeersWith(PeerOptions{CheckInterval: interval, CheckTimeout: 50 * time.Millisecond, FailureThreshold: threshold}, leader, follower)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderCtx, stopLeader := context.WithCancel(ctx)
	go leader.a.RunPeers(leaderCtx)
	go follower.a.RunPeers(ctx)
	testutil.WaitFor(t, "the follower to take the leader's targets", func() bool {
		return leader.discovering() && !follower.discovering() && serves(follower, t, "first")
	})
	stopLeader()
	leader.server.Close()
	closed := time.Now()
	testutil.WaitFor(t, "the follower to lead", follower.discovering)
	took := time.Since(closed)
	if least := (threshold - 1) * interval; took < least {
		t.Errorf("the follower led %s after the leader went, want no sooner than %s, %d intervals after the first failed check", took, least, threshold-1)
	}
	if most := threshold*interval + 2*time.Second; took > most {
		t.Errorf("the follower led %s after the leader went, want within %s", took, most)
	}
}

// slowPeer is an instance that answers its check after delay, discovering
// and knowing its own address.
func slowPeer(t *testing.T, delay time.Duration) netip.AddrPort {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"id":"slow","discovering":true,"address_known":true}`)
	}))
	t.Cleanup(srv.Close)
	return netip.MustParseAddrPort(srv.Listener.Addr().String())
}

// A check is bounded by CheckTimeout: a peer of a lower address that
// answers after 300ms is not live with a timeout of 100ms, so the instance
// leads, and with the default of 1s it is, so the instance follows it.
func TestACheckIsBoundedByTheConfiguredTimeout(t *testing.T) {
	p := writeInputs(t, testConfig, "", twoJobs(stableSD(t).URL, false))
	peer := slowPeer(t, 300*time.Millisecond)
	for timeout, leads := range map[time.Duration]bool{100 * time.Millisecond: true, 0: false} {
		c := newPeerInstance(t, p, nil)
		c.a.UsePeers(PeerOptions{Peers: []string{peer.String()}, Self: netip.MustParseAddrPort("127.255.255.254:1"), CheckTimeout: timeout, FailureThreshold: 1,
			Token: func() (string, error) { return peerToken, nil }})
		c.a.peers.round(context.Background())
		if c.discovering() != leads {
			t.Errorf("with a check timeout of %s the instance leads %v, want %v", timeout, c.discovering(), leads)
		}
		c.a.peers.stopFetch()
	}
}

// A snapshot fetch is bounded by SnapshotTimeout: a leader that writes its
// snapshot after 600ms is not taken with a timeout of 200ms, the failure
// counted, and a follower of FailureThreshold 2 logs that it cannot fetch
// the snapshot after its second failed fetch, not its first.
func TestASnapshotFetchIsBoundedByTheConfiguredTimeoutAndThreshold(t *testing.T) {
	srv := stableSD(t)
	p := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	c := newPeerInstance(t, p, nil)
	id, _ := json.Marshal(mustLoad(t, p).sources[0].identity())
	body := `{"id":"leader","discovering":true,"sources":{` + string(id) + `:{"answered":true,"groups":[{"targets":["slow.example:80"],"labels":null}]}}}`
	leader := fakeLeader(t, body, 600*time.Millisecond)
	c.a.UsePeers(PeerOptions{Peers: []string{leader.String()}, Self: netip.MustParseAddrPort("127.255.255.254:1"),
		CheckTimeout: 100 * time.Millisecond, SnapshotTimeout: 200 * time.Millisecond, FailureThreshold: 2,
		Token: func() (string, error) { return peerToken, nil }})
	const stale = "the leader's discovered targets cannot be fetched"
	for i := 1; i <= 2; i++ {
		c.round()
		if got := jobTargets(c.a, "websites"); slices.Contains(got, "https://slow.example:80/health") {
			t.Fatalf("fetch %d: the follower took the slow snapshot", i)
		}
		if logged := strings.Contains(c.logs.String(), stale); logged != (i == 2) {
			t.Errorf("after %d failed fetches the follower logged %q: %v", i, stale, logged)
		}
	}
	hasLine(t, c, `pue_allocator_peer_failures_total{request="snapshot"} 2`+"\n")
}

// The peers' part of the status gives the timings, and the page says them
// in its Peers section, a failure threshold of 1 in the singular.
func TestTheStatusGivesThePeerTimings(t *testing.T) {
	p := writeInputs(t, testConfig, "", twoJobs(stableSD(t).URL, false))
	for _, c := range []struct {
		o    PeerOptions
		want PeersStatus
		text string
	}{
		{PeerOptions{}, PeersStatus{CheckIntervalSeconds: 2, CheckTimeoutSeconds: 1, FailureThreshold: 3, SnapshotTimeoutSeconds: 30},
			"The instances check each other every 2s, each check within 1s, and an instance stops being live after 3 failed checks in a row; a follower fetches the leader's snapshot with each round of checks, a fetch within 30s."},
		{PeerOptions{CheckInterval: 750 * time.Millisecond, CheckTimeout: 250 * time.Millisecond, FailureThreshold: 1, SnapshotTimeout: 90 * time.Second},
			PeersStatus{CheckIntervalSeconds: 0.75, CheckTimeoutSeconds: 0.25, FailureThreshold: 1, SnapshotTimeoutSeconds: 90},
			"The instances check each other every 750ms, each check within 250ms, and an instance stops being live after 1 failed check in a row; a follower fetches the leader's snapshot with each round of checks, a fetch within 1m30s."},
	} {
		inst := newPeerInstance(t, p, nil)
		c.o.Peers, c.o.Self = []string{inst.addr.String()}, inst.addr
		inst.a.UsePeers(c.o)
		s := inst.a.Status().Peers
		if s.CheckIntervalSeconds != c.want.CheckIntervalSeconds || s.CheckTimeoutSeconds != c.want.CheckTimeoutSeconds || s.FailureThreshold != c.want.FailureThreshold || s.SnapshotTimeoutSeconds != c.want.SnapshotTimeoutSeconds {
			t.Errorf("%+v: the status gives %+v", c.o, s)
		}
		raw, _ := json.Marshal(s)
		for _, key := range []string{`"check_interval_seconds":`, `"check_timeout_seconds":`, `"failure_threshold":`, `"snapshot_timeout_seconds":`} {
			if !strings.Contains(string(raw), key) {
				t.Errorf("the status's peers have no %s: %s", key, raw)
			}
		}
		if page := ask(inst.a.Handler(""), "/", "").Body.String(); !strings.Contains(page, c.text) {
			t.Errorf("%+v: the page does not say %q", c.o, c.text)
		}
	}
}

// The shutdown delay is a check interval and a check timeout, the defaults'
// 3s when they are left zero.
func TestTheShutdownDelayIsAnIntervalAndATimeout(t *testing.T) {
	for _, c := range []struct {
		o    PeerOptions
		want time.Duration
	}{
		{PeerOptions{}, 3 * time.Second},
		{PeerOptions{CheckInterval: time.Minute, CheckTimeout: 59 * time.Second}, 119 * time.Second},
		{PeerOptions{CheckInterval: 500 * time.Millisecond, CheckTimeout: 100 * time.Millisecond}, 600 * time.Millisecond},
	} {
		o, want := c.o, c.want
		if got := o.ShutdownDelay(); got != want {
			t.Errorf("%+v: the shutdown delay is %s, want %s", o, got, want)
		}
	}
}
