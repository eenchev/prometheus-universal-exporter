//go:build !select_request_types || request_type_http

package allocator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// stableSD is an http_sd server answering every path with one target named
// after the path, the same at every request.
func stableSD(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"targets": ["%s.example:9100"]}]`, strings.Trim(r.URL.Path, "/"))
	}))
	t.Cleanup(s.Close)
	return s
}

// twoJobs is a probe targets file of the job websites, with an http_sd
// source of url, and with extra the job extra, with another.
func twoJobs(url string, extra bool) string {
	s := `probe_jobs:
  - name: websites
    collector: demo
    target_template: https://{{address}}/health
    discovery:
      - name: cmdb
        http_sd: {url: "` + url + `/probes", refresh_interval: 1h}
`
	if extra {
		s += `  - name: extra
    collector: demo
    target_template: https://{{address}}/health
    discovery:
      - name: more
        http_sd: {url: "` + url + `/extra", refresh_interval: 1h}
`
	}
	return s
}

// jobTargets are the targets the allocator serves for job, or the error.
func jobTargets(a *Allocator, job string) []string {
	groups, err := a.SD(job)
	if err != nil {
		return []string{"error: " + err.Error()}
	}
	var out []string
	for _, g := range groups {
		out = append(out, g.Labels["__param_target"])
	}
	return out
}

// sortedPair is two copies, the lower address first.
func sortedPair(a, b *peerCopy) (low, high *peerCopy) {
	copies := []*peerCopy{a, b}
	sort.Slice(copies, func(i, j int) bool { return comparePeers(copies[i].addr, copies[j].addr) < 0 })
	return copies[0], copies[1]
}

func hasLine(t *testing.T, c *peerCopy, line string) {
	t.Helper()
	if metrics := c.metrics(t); !strings.Contains(metrics, line) {
		t.Errorf("the metrics of %s have no %q:\n%s", c.addr, line, metrics)
	}
}

// Two copies, each reading input files of its own, as two pods of one
// ConfigMap do, reload a new probe job at different moments, the leader
// first. The follower's fetch in between finds a source it does not have
// yet, holds other targets than the leader's snapshot, and its snapshot
// age grows; once it reloads too, its next fetch, which names the digest of
// what it holds rather than the snapshot it took last, takes the new job's
// targets, and the two serve the same targets and bundles again. The
// difference is logged once after PeerFailures fetches, and when it ends.
func TestAFollowerTakesTheLeadersTargetsOfASourceItReloadsLater(t *testing.T) {
	srv := stableSD(t)
	pa := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	pb := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	ca, cb := newPeerCopy(t, pa, nil), newPeerCopy(t, pb, nil)
	paths := map[*peerCopy]Paths{ca: pa, cb: pb}
	leader, follower := sortedPair(ca, cb)
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool { return len(jobTargets(leader.a, "websites")) == 1 })
	follower.round()

	reload := func(c *peerCopy) {
		t.Helper()
		if err := os.WriteFile(paths[c].ProbeTargets, []byte(twoJobs(srv.URL, true)), 0o600); err != nil {
			t.Fatal(err)
		}
		if !c.a.Reload(paths[c]) {
			t.Fatal("the reload was refused")
		}
	}
	reload(leader)
	testutil.WaitFor(t, "the leader to discover the new job", func() bool { return len(jobTargets(leader.a, "extra")) == 1 })
	for range PeerFailures + 1 {
		follower.clock.Advance(PeerInterval)
		follower.round()
	}
	hasLine(t, follower, "pue_allocator_snapshot_age_seconds 8\n")
	const differ = "differ from the leader's snapshot"
	if n := strings.Count(follower.logs.String(), differ); n != 1 {
		t.Errorf("logged %q %d times, want once:\n%s", differ, n, follower.logs.String())
	}

	reload(follower)
	follower.clock.Advance(PeerInterval)
	follower.round()
	if l, f := jobTargets(leader.a, "extra"), jobTargets(follower.a, "extra"); !reflect.DeepEqual(l, f) {
		t.Errorf("the leader serves %v for the new job, the follower %v", l, f)
	}
	for _, member := range []string{"r1", "r2"} {
		if lb, fb := leader.a.Bundle(member), follower.a.Bundle(member); lb.Generation != fb.Generation {
			t.Errorf("the bundles of %s are of the generations %s and %s", member, lb.Generation, fb.Generation)
		}
	}
	hasLine(t, follower, "pue_allocator_snapshot_age_seconds 0\n")
	if !strings.Contains(follower.logs.String(), "holds the leader's discovered targets again") {
		t.Errorf("the agreement is not logged:\n%s", follower.logs.String())
	}
}

// A follower that led for a while, the leader's checks failing, and whose
// own sources then found other targets, takes the leader's again once it
// follows: its fetch names the digest of what it holds, which is not the
// leader's, so it is answered with the snapshot, not 304.
func TestAFollowerThatLedBrieflyTakesTheLeadersTargetsAgain(t *testing.T) {
	leader, follower, srv := leaderAndFollower(t)
	leader.failPeer.Store(true)
	for range PeerFailures {
		follower.round()
	}
	if !follower.discovering() {
		t.Fatal("the follower does not lead while the leader's checks fail")
	}
	srv.free()
	testutil.WaitFor(t, "the brief leader's sources to answer", func() bool { return serves(follower, t, "later") })
	leader.failPeer.Store(false)
	follower.round()
	if follower.discovering() {
		t.Fatal("the follower still leads")
	}
	if l, f := leader.probeTargets(t), follower.probeTargets(t); !slices.Equal(l, f) {
		t.Errorf("following again, the follower serves %v, the leader %v", f, l)
	}
}
