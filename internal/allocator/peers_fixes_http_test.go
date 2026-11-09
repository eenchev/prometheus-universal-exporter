//go:build !select_request_types || request_type_http

package allocator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
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

// sortedPair is two instances, the lower address first.
func sortedPair(a, b *peerInstance) (low, high *peerInstance) {
	instances := []*peerInstance{a, b}
	sort.Slice(instances, func(i, j int) bool { return comparePeers(instances[i].addr, instances[j].addr) < 0 })
	return instances[0], instances[1]
}

func hasLine(t *testing.T, c *peerInstance, line string) {
	t.Helper()
	if metrics := c.metrics(t); !strings.Contains(metrics, line) {
		t.Errorf("the metrics of %s have no %q:\n%s", c.addr, line, metrics)
	}
}

// Two instances, each reading input files of its own, as two pods of one
// ConfigMap do, reload a new probe job at different moments, the leader
// first. The follower's fetch in between finds a source it does not have
// yet, holds other targets than the leader's snapshot, and its snapshot
// age grows; once it reloads too, its next fetch, which names the digest of
// what it holds rather than the snapshot it took last, takes the new job's
// targets, and the two serve the same targets and bundles again. The
// difference is logged once after DefaultPeerFailureThreshold fetches, and when it ends.
func TestAFollowerTakesTheLeadersTargetsOfASourceItReloadsLater(t *testing.T) {
	srv := stableSD(t)
	pa := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	pb := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	ca, cb := newPeerInstance(t, pa, nil), newPeerInstance(t, pb, nil)
	paths := map[*peerInstance]Paths{ca: pa, cb: pb}
	leader, follower := sortedPair(ca, cb)
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool { return len(jobTargets(leader.a, "websites")) == 1 })
	follower.round()

	reload := func(c *peerInstance) {
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
	for range DefaultPeerFailureThreshold + 1 {
		follower.clock.Advance(DefaultPeerCheckInterval)
		follower.round()
	}
	hasLine(t, follower, "pue_allocator_snapshot_age_seconds 8\n")
	const differ = "differ from the leader's snapshot"
	if n := strings.Count(follower.logs.String(), differ); n != 1 {
		t.Errorf("logged %q %d times, want once:\n%s", differ, n, follower.logs.String())
	}

	reload(follower)
	follower.clock.Advance(DefaultPeerCheckInterval)
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
	for range DefaultPeerFailureThreshold {
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

// ownLoops counts the sources the instance runs a refresh loop of.
func ownLoops(c *peerInstance) int {
	c.a.dmu.Lock()
	defer c.a.dmu.Unlock()
	n := 0
	for _, s := range c.a.sources {
		if s.cancel != nil {
			n++
		}
	}
	return n
}

// During a rolling update that changes a source's settings, a new instance
// following a leader of the old input files takes the leader's targets of
// that source, matched by its kind, owner and name, rather than nothing,
// and asks no source itself.
func TestAFollowerTakesASourceWhoseSettingsDifferOnTheLeader(t *testing.T) {
	srv := stableSD(t)
	old := twoJobs(srv.URL, false)
	changed := strings.Replace(old, "refresh_interval: 1h", "refresh_interval: 30m", 1)
	pa, pb := writeInputs(t, testConfig, "", old), writeInputs(t, testConfig, "", old)
	ca, cb := newPeerInstance(t, pa, nil), newPeerInstance(t, pb, nil)
	paths := map[*peerInstance]Paths{ca: pa, cb: pb}
	leader, follower := sortedPair(ca, cb)
	if err := os.WriteFile(paths[follower].ProbeTargets, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if !follower.a.Reload(paths[follower]) {
		t.Fatal("the reload was refused")
	}
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool { return len(jobTargets(leader.a, "websites")) == 1 })
	follower.round()
	if l, f := jobTargets(leader.a, "websites"), jobTargets(follower.a, "websites"); !slices.Equal(l, f) {
		t.Errorf("the follower serves %v, the leader %v", f, l)
	}
	if n := ownLoops(follower); n != 0 {
		t.Errorf("the follower runs %d sources itself, want none", n)
	}
}

// A follower whose input files have a source the leader's lack, as a new
// copy of a changed ConfigMap does before the leader's pod has it, asks that
// source itself and serves its targets, its snapshot age growing since it
// does not hold what the leader's snapshot holds; once the leader has the
// source too, the follower stops asking it and takes the leader's targets.
func TestAFollowerAsksASourceTheLeaderLacksUntilTheLeaderHasIt(t *testing.T) {
	srv := stableSD(t)
	pa := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	pb := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	ca, cb := newPeerInstance(t, pa, nil), newPeerInstance(t, pb, nil)
	paths := map[*peerInstance]Paths{ca: pa, cb: pb}
	leader, follower := sortedPair(ca, cb)
	reload := func(c *peerInstance) {
		t.Helper()
		if err := os.WriteFile(paths[c].ProbeTargets, []byte(twoJobs(srv.URL, true)), 0o600); err != nil {
			t.Fatal(err)
		}
		if !c.a.Reload(paths[c]) {
			t.Fatal("the reload was refused")
		}
	}
	reload(follower)
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool { return len(jobTargets(leader.a, "websites")) == 1 })
	follower.clock.Advance(DefaultPeerCheckInterval)
	follower.round()
	testutil.WaitFor(t, "the follower to ask the source the leader lacks", func() bool {
		return slices.Equal(jobTargets(follower.a, "extra"), []string{"https://extra.example:9100/health"})
	})
	if n := ownLoops(follower); n != 1 {
		t.Errorf("the follower runs %d sources itself, want the one the leader lacks", n)
	}
	if l, f := jobTargets(leader.a, "websites"), jobTargets(follower.a, "websites"); !slices.Equal(l, f) {
		t.Errorf("the follower serves %v of the job both have, the leader %v", f, l)
	}
	follower.clock.Advance(DefaultPeerCheckInterval)
	follower.round()
	if strings.Contains(follower.metrics(t), "pue_allocator_snapshot_age_seconds 0\n") {
		t.Error("the follower holding other targets than the leader's snapshot shows a snapshot age of 0")
	}

	reload(leader)
	testutil.WaitFor(t, "the leader to discover the new job", func() bool { return len(jobTargets(leader.a, "extra")) == 1 })
	follower.round()
	if n := ownLoops(follower); n != 0 {
		t.Errorf("the follower still runs %d sources itself", n)
	}
	if !strings.Contains(follower.logs.String(), "takes the leader's targets of it again") {
		t.Errorf("the follower does not log that it stopped asking the source:\n%s", follower.logs.String())
	}
	hasLine(t, follower, "pue_allocator_snapshot_age_seconds 0\n")
}

// A file_sd file of one group of 10000 targets with 7 KB of labels, 236 KB
// on disk, is a snapshot of about its own size, the group's labels written
// once rather than once for each target (70 MB); the follower takes it and
// serves every target. A snapshot may be up to 16 MiB for each of the
// follower's sources.
func TestASnapshotWritesAGroupsLabelsOnce(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(`[{"targets": [`)
	for i := range 10000 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, `"h-%d.example:%d"`, i, 1000+i)
	}
	fmt.Fprintf(&b, `], "labels": {"__meta_note": %q}}]`, strings.Repeat("y", 7000))
	file := testutil.WriteIn(t, dir, "one.json", b.String())
	probes := `probe_jobs:
  - name: websites
    collector: demo
    target_template: https://{{address}}/
    discovery:
      - name: f
        file_sd: {files: ["` + file + `"], refresh_interval: 1h}
`
	p := writeInputs(t, testConfig, "", probes)
	leader, follower := sortedPair(newPeerInstance(t, p, nil), newPeerInstance(t, p, nil))
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool { return len(jobTargets(leader.a, "websites")) == 10000 })

	req, _ := http.NewRequest(http.MethodGet, leader.server.URL+SnapshotPath, nil)
	req.Header.Set("Authorization", "Bearer "+peerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 2*b.Len() {
		t.Errorf("the snapshot of a %d byte file_sd file is %d bytes", b.Len(), len(body))
	}
	follower.round()
	if n := len(jobTargets(follower.a, "websites")); n != 10000 {
		t.Errorf("the follower serves %d targets, want 10000:\n%s", n, follower.logs.String())
	}
	hasLine(t, follower, "pue_allocator_snapshot_age_seconds 0\n")
	if got, want := follower.a.maxSnapshotBytes(), 16<<20; got != want {
		t.Errorf("a snapshot of an instance of one source may be %d bytes, want %d", got, want)
	}
	if got, want := New(nil, Options{Logger: testutil.QuietLogger(t)}).maxSnapshotBytes(), 16<<20; got != want {
		t.Errorf("a snapshot of an instance of no source may be %d bytes, want %d", got, want)
	}
	two := New(mustLoad(t, peerInputs(t, newSDServer(t))), Options{Logger: testutil.QuietLogger(t)})
	if got, want := two.maxSnapshotBytes(), 32<<20; got != want {
		t.Errorf("a snapshot of an instance of two sources may be %d bytes, want %d", got, want)
	}
}

// fakeLeader is a leader of the given snapshot body, held for delay before
// it is written, the headers sent at once.
func fakeLeader(t *testing.T, body string, delay time.Duration) netip.AddrPort {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PeerPath {
			fmt.Fprint(w, `{"id":"leader","discovering":true,"address_known":true}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return netip.MustParseAddrPort(srv.Listener.Addr().String())
}

// followFake makes c follow the leader at addr, c's own address being
// above any the test listens on.
func followFake(c *peerInstance, addr netip.AddrPort) {
	c.a.UsePeers(PeerOptions{Peers: []string{addr.String()}, Self: netip.MustParseAddrPort("127.255.255.254:1"),
		Token: func() (string, error) { return peerToken, nil }})
}

// A snapshot whose body takes longer than a check may, as a large one over
// a busy network does, is taken: a fetch has SnapshotTimeout, not
// DefaultPeerCheckTimeout.
func TestASnapshotMayTakeLongerThanACheck(t *testing.T) {
	tier.Slow(t)
	srv := stableSD(t)
	p := writeInputs(t, testConfig, "", twoJobs(srv.URL, false))
	c := newPeerInstance(t, p, nil)
	id, _ := json.Marshal(mustLoad(t, p).sources[0].identity())
	body := `{"id":"leader","discovering":true,"sources":{` + string(id) + `:{"answered":true,"groups":[{"targets":["slow.example:80"],"labels":null}]}}}`
	followFake(c, fakeLeader(t, body, DefaultPeerCheckTimeout+500*time.Millisecond))
	c.round()
	if got := jobTargets(c.a, "websites"); !slices.Equal(got, []string{"https://slow.example:80/health"}) {
		t.Errorf("the follower serves %v, want the slow snapshot's target:\n%s", got, c.logs.String())
	}
}

// A leader's snapshot of a source is taken as the source's own answer
// would be: one with more targets than the follower's max_targets, or
// with an empty target or a label of no label name, is refused, logged
// and counted as the source's failure, and the source keeps what it held.
func TestASnapshotIsHeldToTheSourcesMaxTargetsAndChecks(t *testing.T) {
	srv := stableSD(t)
	probes := strings.Replace(twoJobs(srv.URL, false), "refresh_interval: 1h}", "refresh_interval: 1h}\n        max_targets: 10", 1)
	p := writeInputs(t, testConfig, "", probes)
	id, _ := json.Marshal(mustLoad(t, p).sources[0].identity())
	var many []string
	for i := range 11 {
		many = append(many, fmt.Sprintf(`"h%d.example:80"`, i))
	}
	for what, groups := range map[string]string{
		"more than max_targets": `[{"targets":[` + strings.Join(many, ",") + `]}]`,
		"an empty target":       `[{"targets":["ok.example:80",""]}]`,
		"no label name":         `[{"targets":["ok.example:80"],"labels":{"bad name":"x"}}]`,
	} {
		c := newPeerInstance(t, p, nil)
		body := `{"id":"leader","discovering":true,"sources":{` + string(id) + `:{"answered":true,"groups":` + groups + `}}}`
		followFake(c, fakeLeader(t, body, 0))
		c.round()
		if got := jobTargets(c.a, "websites"); len(got) != 0 {
			t.Errorf("%s: the follower serves %d targets of the snapshot", what, len(got))
		}
		hasLine(t, c, `pue_allocator_discovery_refreshes_total{kind="probe",owner="websites",source="cmdb",result="failure"} 1`+"\n")
		if !strings.Contains(c.logs.String(), "the leader's snapshot of it is refused") {
			t.Errorf("%s: the refusal is not logged:\n%s", what, c.logs.String())
		}
	}
}

// Two instances whose --peers name only the other, neither given --peers.self,
// cannot place themselves among their peers. Each follows only an instance that
// discovers, so they settle on one leader, even when their rounds run
// together and both may begin to lead in the same round, rather than each
// following the other with neither discovering; each logs once that its own address is missing.
func TestInstancesThatDoNotKnowTheirAddressSettleOnOneLeader(t *testing.T) {
	for _, together := range []bool{false, true} {
		p := writeInputs(t, testConfig, "", twoJobs(stableSD(t).URL, false))
		a, b := newPeerInstance(t, p, nil), newPeerInstance(t, p, nil)
		token := func() (string, error) { return peerToken, nil }
		a.a.UsePeers(PeerOptions{Peers: []string{b.addr.String()}, Token: token})
		b.a.UsePeers(PeerOptions{Peers: []string{a.addr.String()}, Token: token})
		for range DefaultPeerFailureThreshold + 2 {
			if together {
				var wg sync.WaitGroup
				wg.Go(a.round)
				wg.Go(b.round)
				wg.Wait()
			} else {
				a.round()
				b.round()
			}
		}
		if a.discovering() == b.discovering() {
			t.Fatalf("rounds together %v: the instances discover %v and %v, want one leader", together, a.discovering(), b.discovering())
		}
		leader, follower := a, b
		if b.discovering() {
			leader, follower = b, a
		}
		want := []string{"https://probes.example:9100/health"}
		testutil.WaitFor(t, "the leader to discover", func() bool { return slices.Equal(jobTargets(leader.a, "websites"), want) })
		follower.round()
		if got := jobTargets(follower.a, "websites"); !slices.Equal(got, want) {
			t.Errorf("rounds together %v: the follower serves %v", together, got)
		}
		for _, c := range []*peerInstance{a, b} {
			const missing = "no address of --peers answers as this instance"
			if n := strings.Count(c.logs.String(), missing); n != 1 {
				t.Errorf("rounds together %v: logged %q %d times, want once:\n%s", together, missing, n, c.logs.String())
			}
		}
	}
}
