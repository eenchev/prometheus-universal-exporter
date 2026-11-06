//go:build !select_request_types || request_type_http

package allocator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sdServer is an http_sd server whose answers differ from one request to
// the next, as a source's do when it changes between two instances' refreshes:
// the first request of each path is answered with the "first" target, and
// every later one with the "later" target, once release is closed.
type sdServer struct {
	*httptest.Server
	mu      sync.Mutex
	asked   map[string]int
	release chan struct{}
	once    sync.Once
}

func newSDServer(t *testing.T) *sdServer {
	s := &sdServer{asked: map[string]int{}, release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked[r.URL.Path]++
		n := s.asked[r.URL.Path]
		s.mu.Unlock()
		target := "first"
		if n > 1 {
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			}
			target = "later"
		}
		fmt.Fprintf(w, `[{"targets": ["%s-%s.example:9100"]}]`, target, strings.Trim(r.URL.Path, "/"))
	}))
	t.Cleanup(s.Close)
	t.Cleanup(s.free)
	return s
}

// free lets every held request be answered.
func (s *sdServer) free() { s.once.Do(func() { close(s.release) }) }

func (s *sdServer) requests(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[path]
}

// syncBuffer is a log written by several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// peerInstance is one allocator instance served over HTTP, with its token, on a
// clock of the test's own.
type peerInstance struct {
	a      *Allocator
	clock  *fakeClock
	server *httptest.Server
	addr   netip.AddrPort
	logs   *syncBuffer
	// failSnapshot makes the snapshot endpoint answer 500, and failPeer
	// makes the check endpoint answer 504.
	failSnapshot, failPeer atomic.Bool
}

const peerToken = "s3cret"

// peerInputs are input files whose probe job and discovered_targets entry
// each have an http_sd source of srv.
func peerInputs(t *testing.T, srv *sdServer) Paths {
	probes := `probe_jobs:
  - name: websites
    collector: demo
    target_template: https://{{address}}/health
    discovery:
      - name: cmdb
        http_sd: {url: "` + srv.URL + `/probes", refresh_interval: 1h}
`
	static := `interval: 30s
discovered_targets:
  - name: node
    collector: demo
    target_template: http://{{address}}/metrics
    discovery:
      - http_sd: {url: "` + srv.URL + `/nodes", refresh_interval: 1h}
`
	return writeInputs(t, testConfig, static, probes)
}

// newPeerInstance starts an allocator instance of p, its member list formed
// of r1 and r2, serving its API on ln, or on a port of its own when ln is
// nil.
func newPeerInstance(t *testing.T, p Paths, ln net.Listener) *peerInstance {
	t.Helper()
	c := &peerInstance{clock: newFakeClock(), logs: &syncBuffer{}}
	c.a = New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: c.clock, Logger: slog.New(slog.NewJSONHandler(c.logs, nil))})
	beat(t, c.a, "r1", "r2")
	keepBeating(t, c.a, c.clock, 20*time.Second, "r1", "r2")
	api := c.a.Handler(peerToken)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == SnapshotPath && c.failSnapshot.Load() {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == PeerPath && c.failPeer.Load() {
			http.Error(w, "slow", http.StatusGatewayTimeout)
			return
		}
		api.ServeHTTP(w, r)
	})
	if ln == nil {
		c.server = httptest.NewServer(handler)
	} else {
		c.server = httptest.NewUnstartedServer(handler)
		_ = c.server.Listener.Close()
		c.server.Listener = ln
		c.server.Start()
	}
	t.Cleanup(c.server.Close)
	t.Cleanup(c.a.BeginShutdown)
	c.addr = netip.MustParseAddrPort(c.server.Listener.Addr().String())
	return c
}

// usePeers makes each instance a peer of every instance, each knowing its own
// address.
func usePeers(instances ...*peerInstance) {
	var list []string
	for _, c := range instances {
		list = append(list, c.addr.String())
	}
	for _, c := range instances {
		c.a.UsePeers(PeerOptions{Peers: list, Self: c.addr, Token: func() (string, error) { return peerToken, nil }})
	}
}

// round runs a round of the instance's peers, and waits for the snapshot fetch
// it started, which runs beside it, to end.
func (c *peerInstance) round() {
	c.a.peers.round(context.Background())
	if f := c.a.peers.fetch; f != nil {
		<-f.done
	}
}

func (c *peerInstance) metrics(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := c.a.WriteMetrics(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func (c *peerInstance) discovering() bool {
	c.a.dmu.Lock()
	defer c.a.dmu.Unlock()
	return c.a.discovering
}

// probeTargets are the targets the instance serves for the probe job.
func (c *peerInstance) probeTargets(t *testing.T) []string {
	t.Helper()
	groups, err := c.a.SD("websites")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range groups {
		out = append(out, g.Labels["__param_target"])
	}
	return out
}

func serves(c *peerInstance, t *testing.T, target string) bool {
	return slices.Equal(c.probeTargets(t), []string{"https://" + target + "-probes.example:9100/health"})
}

// leaderAndFollower are two instances of the same input files, peers of each
// other, once the leader, the lower address, has discovered and the
// follower has taken its snapshot.
func leaderAndFollower(t *testing.T) (leader, follower *peerInstance, srv *sdServer) {
	t.Helper()
	srv = newSDServer(t)
	p := peerInputs(t, srv)
	instances := []*peerInstance{newPeerInstance(t, p, nil), newPeerInstance(t, p, nil)}
	sort.Slice(instances, func(i, j int) bool { return comparePeers(instances[i].addr, instances[j].addr) < 0 })
	leader, follower = instances[0], instances[1]
	usePeers(leader, follower)
	leader.round()
	follower.round()
	testutil.WaitFor(t, "the leader to discover", func() bool {
		return serves(leader, t, "first") && len(allTargets(t, leader.a, "r1", "r2")) == 1
	})
	follower.round()
	return leader, follower, srv
}

// Of two instances with the same sources, only the leader, the lower address,
// asks them; the follower takes what the leader found from its snapshot, so
// both serve the same service discovery answers and the same bundles, of
// the same generations, though the sources would have answered the
// follower otherwise.
func TestAFollowerServesTheLeadersDiscoveredTargets(t *testing.T) {
	leader, follower, srv := leaderAndFollower(t)
	if !serves(follower, t, "first") {
		t.Fatalf("the follower serves %v, want the leader's discovered target", follower.probeTargets(t))
	}
	ls, _ := leader.a.SD("websites")
	fs, _ := follower.a.SD("websites")
	if !reflect.DeepEqual(ls, fs) {
		t.Errorf("the service discovery answers differ:\nleader   %v\nfollower %v", ls, fs)
	}
	for _, member := range []string{"r1", "r2", "joining"} {
		lb, fb := leader.a.Bundle(member), follower.a.Bundle(member)
		if lb.Generation != fb.Generation || !reflect.DeepEqual(lb.Files, fb.Files) {
			t.Errorf("the bundles of %s differ: generation %s on the leader, %s on the follower", member, lb.Generation, fb.Generation)
		}
	}
	if got := allTargets(t, follower.a, "r1", "r2"); len(got) != 1 || got["node_first_2Dnodes_example_3A9100"].Target == "" {
		t.Errorf("the follower's shares hold %v, want the leader's discovered static target", got)
	}
	if n, m := srv.requests("/probes"), srv.requests("/nodes"); n != 1 || m != 1 {
		t.Errorf("the sources were asked %d and %d times, want once each, by the leader alone", n, m)
	}
	if follower.discovering() || !leader.discovering() {
		t.Errorf("the follower discovers %v and the leader %v", follower.discovering(), leader.discovering())
	}
	for c, want := range map[*peerInstance][]string{
		leader:   {"pue_allocator_is_leader 1\n", fmt.Sprintf("pue_allocator_leader{peer=%q} 1\n", leader.addr), "pue_allocator_peers 2\n", "pue_allocator_snapshot_age_seconds 0\n"},
		follower: {"pue_allocator_is_leader 0\n", fmt.Sprintf("pue_allocator_leader{peer=%q} 1\n", leader.addr), "pue_allocator_peers 2\n", "pue_allocator_leader_changes_total 0\n"},
	} {
		metrics := c.metrics(t)
		for _, line := range want {
			if !strings.Contains(metrics, line) {
				t.Errorf("the metrics of %s have no %q:\n%s", c.addr, line, metrics)
			}
		}
	}
	if strings.Contains(follower.metrics(t), "discovery_refreshes_total{kind=\"probe\",owner=\"websites\",source=\"cmdb\",result=\"success\"} 1") {
		t.Error("the follower counts a refresh it did not make")
	}
}

// When the leader shuts down, the follower leads at its next round, starts
// asking the sources, and serves the targets of the snapshot it took last
// until they answer: there is no moment without them.
func TestTheNextInstanceLeadsWhenTheLeaderShutsDown(t *testing.T) {
	leader, follower, srv := leaderAndFollower(t)
	leader.a.BeginShutdown()
	follower.round()
	if !follower.discovering() {
		t.Fatal("the follower does not lead once the leader shuts down")
	}
	// The sources' answer is held: the new leader still serves the
	// targets the old one found.
	if !serves(follower, t, "first") {
		t.Fatalf("the new leader serves %v before its sources answer, want the snapshot's target", follower.probeTargets(t))
	}
	srv.free()
	testutil.WaitFor(t, "the new leader to serve what its sources found", func() bool { return serves(follower, t, "later") })
	metrics := follower.metrics(t)
	for _, line := range []string{"pue_allocator_is_leader 1\n", "pue_allocator_leader_changes_total 1\n", "pue_allocator_peers 1\n"} {
		if !strings.Contains(metrics, line) {
			t.Errorf("the metrics have no %q:\n%s", line, metrics)
		}
	}
}

// A follower that cannot fetch the leader's snapshot keeps serving the one
// it took last, says so once in the log after DefaultPeerFailureThreshold failures in a
// row, and shows how old it is; a leader that stops answering altogether
// stays the leader until DefaultPeerFailureThreshold checks in a row have failed, and then
// the follower leads, still serving the snapshot's targets.
func TestAFollowerThatLosesTheLeaderKeepsItsLastSnapshot(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	leader.failSnapshot.Store(true)
	for range DefaultPeerFailureThreshold + 1 {
		follower.clock.Advance(DefaultPeerCheckInterval)
		follower.round()
		if !serves(follower, t, "first") || follower.discovering() {
			t.Fatalf("a follower that cannot fetch the snapshot serves %v, discovering %v", follower.probeTargets(t), follower.discovering())
		}
	}
	metrics := follower.metrics(t)
	for _, line := range []string{"pue_allocator_peer_failures_total{request=\"snapshot\"} 4\n", "pue_allocator_snapshot_age_seconds 8\n", "pue_allocator_is_leader 0\n"} {
		if !strings.Contains(metrics, line) {
			t.Errorf("the metrics have no %q:\n%s", line, metrics)
		}
	}
	const stale = "the leader's discovered targets cannot be fetched"
	if n := strings.Count(follower.logs.String(), stale); n != 1 {
		t.Errorf("logged %q %d times, want once:\n%s", stale, n, follower.logs.String())
	}
	leader.failSnapshot.Store(false)
	follower.round()
	if !strings.Contains(follower.metrics(t), "pue_allocator_snapshot_age_seconds 0\n") || !strings.Contains(follower.logs.String(), "fetched again") {
		t.Errorf("a snapshot fetched again is not taken as fresh:\n%s", follower.logs.String())
	}

	leader.server.Close()
	for i := 1; i <= DefaultPeerFailureThreshold; i++ {
		follower.round()
		if leading := follower.discovering(); leading != (i == DefaultPeerFailureThreshold) {
			t.Fatalf("after %d failed checks of the leader the follower leads: %v", i, leading)
		}
		if !serves(follower, t, "first") {
			t.Fatalf("after %d failed checks the follower serves %v, want the snapshot's target", i, follower.probeTargets(t))
		}
	}
	if !strings.Contains(follower.logs.String(), "an allocator instance stopped answering") {
		t.Errorf("the leader's loss is not logged:\n%s", follower.logs.String())
	}
}

// The peer endpoints are the API's: without its token they answer 401, and
// an instance of another token is never counted live, so it leads alone.
func TestThePeerEndpointsNeedTheToken(t *testing.T) {
	srv := newSDServer(t)
	p := peerInputs(t, srv)
	c := newPeerInstance(t, p, nil)
	for _, path := range []string{PeerPath, SnapshotPath} {
		for token, want := range map[string]int{"": http.StatusUnauthorized, "wrong": http.StatusUnauthorized, peerToken: http.StatusOK} {
			req, _ := http.NewRequest(http.MethodGet, c.server.URL+path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("%s with the token %q answered %d, want %d", path, token, resp.StatusCode, want)
			}
		}
	}

	other := newPeerInstance(t, p, nil)
	list := []string{c.addr.String(), other.addr.String()}
	c.a.UsePeers(PeerOptions{Peers: list, Self: c.addr, Token: func() (string, error) { return "wrong", nil }})
	other.a.UsePeers(PeerOptions{Peers: list, Self: other.addr, Token: func() (string, error) { return "wrong", nil }})
	c.round()
	other.round()
	if !c.discovering() || !other.discovering() {
		t.Error("an instance counted a peer live that refused its token")
	}
	if !strings.Contains(c.metrics(t), "pue_allocator_peer_failures_total{request=\"check\"} 1\n") {
		t.Errorf("the refused check is not counted:\n%s", c.metrics(t))
	}
}

// listenPair listens on 127.0.0.1 and 127.0.0.2 at one port, the addresses
// one name resolves to.
func listenPair(t *testing.T) (low, high net.Listener) {
	t.Helper()
	for range 20 {
		low, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		high, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", strconv.Itoa(low.Addr().(*net.TCPAddr).Port)))
		if err == nil {
			return low, high
		}
		_ = low.Close()
	}
	t.Skip("cannot listen on 127.0.0.2 beside 127.0.0.1 at one port")
	return nil, nil
}

// Instances given only a name, which resolves to all of them, find their own
// address by asking each, the one answering with their own instance being
// themselves; an instance that comes to lead without ever having followed, as
// a new instance of the lowest address does, starts from another instance's
// snapshot, so it serves the discovered targets before its own sources
// answer.
func TestInstancesFindThemselvesAmongTheAddressesOfTheirName(t *testing.T) {
	srv := newSDServer(t)
	p := peerInputs(t, srv)
	lowLn, highLn := listenPair(t)
	port := lowLn.Addr().(*net.TCPAddr).Port
	high := newPeerInstance(t, p, highLn)
	opts := PeerOptions{
		Peers: []string{fmt.Sprintf("allocators.test:%d", port)},
		Token: func() (string, error) { return peerToken, nil },
		Resolve: func(_ context.Context, host string) ([]string, error) {
			if host != "allocators.test" {
				return nil, fmt.Errorf("no such host %s", host)
			}
			return []string{"127.0.0.2", "127.0.0.1"}, nil
		},
	}
	high.a.UsePeers(opts)
	// Nothing listens on 127.0.0.1 yet: the instance there has not started.
	_ = lowLn.Close()
	high.round()
	if !high.discovering() || high.a.peers.self != high.addr {
		t.Fatalf("alone, the instance at %s discovers %v and takes itself for %v", high.addr, high.discovering(), high.a.peers.self)
	}
	testutil.WaitFor(t, "the lone instance to discover", func() bool { return serves(high, t, "first") })

	lowLn, err := net.Listen("tcp", lowLn.Addr().String())
	if err != nil {
		t.Skipf("cannot listen again on %s: %v", lowLn.Addr(), err)
	}
	low := newPeerInstance(t, p, lowLn)
	low.a.UsePeers(opts)
	low.round()
	if low.a.peers.self != low.addr || !low.discovering() {
		t.Fatalf("the instance at %s takes itself for %v and discovers %v, want itself, leading", low.addr, low.a.peers.self, low.discovering())
	}
	// Its own sources' answer is held, so what it serves is the snapshot.
	if !serves(low, t, "first") {
		t.Errorf("the new leader serves %v, want the targets of the other instance's snapshot", low.probeTargets(t))
	}
	high.round()
	if high.discovering() || !strings.Contains(high.metrics(t), fmt.Sprintf("pue_allocator_leader{peer=%q} 1\n", low.addr)) {
		t.Errorf("the instance at %s does not follow the lower one:\n%s", high.addr, high.metrics(t))
	}
}
