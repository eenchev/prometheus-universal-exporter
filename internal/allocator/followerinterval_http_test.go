//go:build !select_request_types || request_type_http

package allocator

import (
	"os"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sourceInterval is the refresh_interval_seconds the status of a gives the
// source named name, and where what it holds came from.
func sourceInterval(a *Allocator, name string) (float64, string) {
	for _, s := range a.Status().Sources {
		if s.Name == name {
			return s.RefreshIntervalSeconds, s.From
		}
	}
	return -1, ""
}

// A follower shows, for a source it takes from the leader's snapshot, how
// often the leader asks it, not its own setting, which nothing asks at:
// during a rolling update that changes the source's refresh_interval from 1h
// to 30m, the new follower of an old leader shows 1h. The interval is part
// of the snapshot's digest, so the follower still agrees with the leader,
// and when the leader's changes, the next fetch takes the new one.
func TestAFollowerShowsTheLeadersRefreshInterval(t *testing.T) {
	srv := stableSD(t)
	old := twoJobs(srv.URL, false)
	changed := strings.Replace(old, "refresh_interval: 1h", "refresh_interval: 30m", 1)
	pa, pb := writeInputs(t, testConfig, "", old), writeInputs(t, testConfig, "", old)
	ca, cb := newPeerInstance(t, pa, nil), newPeerInstance(t, pb, nil)
	paths := map[*peerInstance]Paths{ca: pa, cb: pb}
	leader, follower := sortedPair(ca, cb)
	reload := func(c *peerInstance, text string) {
		t.Helper()
		if err := os.WriteFile(paths[c].ProbeTargets, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if !c.a.Reload(paths[c]) {
			t.Fatal("the reload was refused")
		}
	}
	reload(follower, changed)
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool { return len(jobTargets(leader.a, "websites")) == 1 })
	follower.round()
	follower.round()
	if got, from := sourceInterval(follower.a, "cmdb"); got != 3600 || from != "snapshot" {
		t.Errorf("the follower shows the source asked every %vs, from %q; want 3600, the leader's, from the snapshot", got, from)
	}
	if got, _ := sourceInterval(leader.a, "cmdb"); got != 3600 {
		t.Errorf("the leader shows the source asked every %vs, want 3600", got)
	}
	_, held := follower.a.snapshotOf()
	_, leaders := leader.a.snapshotOf()
	if held != leaders {
		t.Error("the follower holding the leader's targets and interval has another digest than the leader's snapshot")
	}
	reload(leader, strings.Replace(old, "refresh_interval: 1h", "refresh_interval: 20m", 1))
	follower.round()
	if got, _ := sourceInterval(follower.a, "cmdb"); got != 1200 {
		t.Errorf("after the leader's interval changed to 20m the follower shows %vs, want 1200", got)
	}
}
