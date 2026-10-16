//go:build !select_request_types || request_type_http

package allocator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/allocclient"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The end-to-end test runs the allocator's HTTP API and exporter replicas
// in allocator mode in one process: each replica is an exporter server with
// its configuration manager, served over HTTP, and a real allocator client
// writing into a cache directory of its own. The allocator runs on a clock
// the test moves; the replicas heartbeat every few milliseconds, so after
// each move the test waits for every live replica's next heartbeat before
// it moves the clock again. Every wait is for an event: a heartbeat the
// allocator handled, or something a client did.

// pulse is fired at every event the test may be waiting for.
type pulse struct {
	mu sync.Mutex
	ch chan struct{}
}

func newPulse() *pulse { return &pulse{ch: make(chan struct{})} }

func (p *pulse) fire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.ch)
	p.ch = make(chan struct{})
}

// until waits for cond to hold, checking it at every event.
func (p *pulse) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		p.mu.Lock()
		ch := p.ch
		p.mu.Unlock()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

// heard is what the allocator last heard from a member, and when on its
// clock the heartbeat arrived.
type heard struct {
	at         time.Time
	generation string
}

// heartbeatLog notes every heartbeat the allocator's API handled.
type heartbeatLog struct {
	clock *fakeClock
	pulse *pulse
	mu    sync.Mutex
	heard map[string]heard
}

func (l *heartbeatLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != allocapi.HeartbeatPath {
			next.ServeHTTP(w, r)
			return
		}
		at := l.clock.Now()
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
		var hb allocapi.Heartbeat
		if json.Unmarshal(body, &hb) != nil {
			return
		}
		l.mu.Lock()
		l.heard[hb.Member] = heard{at: at, generation: hb.Generation}
		l.mu.Unlock()
		l.pulse.fire()
	})
}

func (l *heartbeatLog) last(member string) (heard, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.heard[member]
	return h, ok
}

// replica is an exporter in allocator mode.
type replica struct {
	name    string
	manager *config.Manager
	http    *httptest.Server
	client  *allocclient.Client
}

func (r *replica) advertise() string { return r.http.Listener.Addr().String() }

// targets are the names of the static targets in force, sorted.
func (r *replica) targets() []string {
	var names []string
	for _, t := range r.manager.StaticTargets() {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

func (r *replica) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(r.http.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// gate holds a replica's next bundle back, once it is written into the
// cache directory and before it is checked, while its member is closed.
type gate struct {
	mu     sync.Mutex
	closed map[string]chan struct{}
}

func (g *gate) step(member, change string) {
	g.mu.Lock()
	ch := g.closed[member]
	g.mu.Unlock()
	if ch != nil && strings.HasPrefix(change, "wrote ") {
		<-ch
	}
}

func (g *gate) close(members ...string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan struct{})
	for _, m := range members {
		g.closed[m] = ch
	}
	return ch
}

// endToEnd is the allocator and its replicas.
type endToEnd struct {
	t        *testing.T
	a        *Allocator
	clock    *fakeClock
	log      *heartbeatLog
	pulse    *pulse
	gate     *gate
	api      *httptest.Server
	logger   *slog.Logger
	replicas map[string]*replica
}

func (e *endToEnd) startReplica(name string) *replica {
	t := e.t
	t.Helper()
	dir := t.TempDir()
	manager := config.NewAwaitingManager(filepath.Join(dir, allocapi.ConfigFile), e.logger)
	manager.SetTargets(filepath.Join(dir, allocapi.StaticTargetsFile), nil)
	manager.AllowAbsentTargets()
	server := exporter.NewServer(manager, "python3", e.logger)
	server.SetAwaitingReason("no configuration yet: waiting for the allocator at " + e.api.URL)
	hs := httptest.NewServer(server.Handler())
	r := &replica{name: name, manager: manager, http: hs}
	client, err := allocclient.New(allocclient.Options{
		URL: e.api.URL, CacheDir: dir, Member: name, Advertise: r.advertise(), Manager: manager, Logger: e.logger,
		HeartbeatInterval: 20 * time.Millisecond, MinBackoff: time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		Event: func(allocclient.Event) { e.pulse.fire() },
		Step:  func(change string) { e.gate.step(name, change) },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.client = client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		hs.Close()
	})
	e.replicas[name] = r
	return r
}

// advance moves the allocator's clock on by d and waits for a heartbeat of
// every live replica after it, so none outlives its TTL however far the
// clock goes, ten seconds at a time.
func (e *endToEnd) advance(d time.Duration, live ...string) {
	e.t.Helper()
	for step := time.Duration(0); step < d; step += allocapi.HeartbeatInterval {
		e.clock.Advance(min(allocapi.HeartbeatInterval, d-step))
		now := e.clock.Now()
		e.pulse.until(e.t, fmt.Sprintf("%v heartbeat after the clock moved", live), func() bool {
			for _, name := range live {
				if h, ok := e.log.last(name); !ok || h.at.Before(now) {
					return false
				}
			}
			return true
		})
	}
}

// inForce waits until each replica named has its bundle in force and has
// said so in a heartbeat.
func (e *endToEnd) inForce(names ...string) {
	e.t.Helper()
	e.pulse.until(e.t, fmt.Sprintf("%v have their bundles in force", names), func() bool {
		for _, name := range names {
			b := e.a.Bundle(name)
			h, _ := e.log.last(name)
			if b == nil || e.replicas[name].client.Status().Generation != b.Generation || h.generation != b.Generation {
				return false
			}
		}
		return true
	})
}

// sd is the allocator's service discovery answer for job, over HTTP.
func (e *endToEnd) sd(job string) []SDGroup {
	e.t.Helper()
	resp, err := http.Get(e.api.URL + allocapi.SDPathPrefix + job)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var groups []SDGroup
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&groups) != nil {
		e.t.Fatalf("service discovery of %s answered %s", job, resp.Status)
	}
	return groups
}

// byAdvertise is the replica Prometheus would send a group's probes to.
func (e *endToEnd) byAdvertise(address string) *replica {
	for _, r := range e.replicas {
		if r.advertise() == address {
			return r
		}
	}
	e.t.Fatalf("no replica advertises %s", address)
	return nil
}

// probe sends the probe of an SD group to r, as Prometheus would send it,
// and checks it is answered with the collector's series.
func probe(t *testing.T, r *replica, g SDGroup, series string) {
	t.Helper()
	q := url.Values{"collector": {g.Labels["__param_collector"]}, "target": {g.Labels["__param_target"]}}
	if code, body := r.get(t, g.Labels["__metrics_path__"]+"?"+q.Encode()); code != http.StatusOK || !strings.Contains(body, series) {
		t.Errorf("the probe of %s at %s answered %d: %s", g.Labels["__param_target"], r.name, code, body)
	}
}

// owners maps each static target to the replicas holding it.
func owners(replicas ...*replica) map[string][]string {
	out := map[string][]string{}
	for _, r := range replicas {
		for _, name := range r.targets() {
			out[name] = append(out[name], r.name)
		}
	}
	return out
}

// Three replicas in allocator mode and the allocator, end to end. The
// replicas are unready until the member list forms, then all ready, their
// static targets together the file's, each on one replica, and every probe
// target of the SD answer at a replica that answers the probe, as does any
// other replica. A fourth replica joins with the configuration alone and,
// once the list takes it, its share, while each old owner keeps the targets
// it lost until their interval has passed, then gives them up. A replica
// shutting down leaves at once: once the settle time has passed its targets
// are the others', and nothing else moved. A collector added to the
// configuration reaches every replica, and the SD answer of a probe job of
// it points only at the replica that has it in force until the others do.
func TestTheAllocatorAndItsReplicasEndToEnd(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value=7\n")) }))
	defer target.Close()
	var static strings.Builder
	static.WriteString("interval: 30s\ntargets:\n")
	var all []string
	for i := range 24 {
		fmt.Fprintf(&static, "  - name: t%02d\n    collector: demo\n    target: http://t%02d.example\n", i, i)
		all = append(all, fmt.Sprintf("t%02d", i))
	}
	var probeTargets strings.Builder
	for i := range 6 {
		fmt.Fprintf(&probeTargets, "      - %s/site%d\n", target.URL, i)
	}
	probes := "probe_jobs:\n  - name: web\n    collector: demo\n    targets:\n" + probeTargets.String()
	p := writeInputs(t, testConfig, static.String(), probes)

	clock := newFakeClock()
	logger := testutil.QuietLogger(t)
	a := New(mustLoad(t, p), Options{Settle: DefaultSettle, Clock: clock, Logger: logger})
	e := &endToEnd{t: t, a: a, clock: clock, pulse: newPulse(), gate: &gate{closed: map[string]chan struct{}{}}, logger: logger, replicas: map[string]*replica{}}
	e.log = &heartbeatLog{clock: clock, pulse: e.pulse, heard: map[string]heard{}}
	e.api = httptest.NewServer(e.log.wrap(a.Handler("")))
	// Run last: the long polls end first, then the API stops.
	t.Cleanup(func() {
		a.BeginShutdown()
		e.api.Close()
	})
	r1, r2, r3 := e.startReplica("r1"), e.startReplica("r2"), e.startReplica("r3")
	e.pulse.until(t, "the allocator heard every replica", func() bool {
		_, ok1 := e.log.last("r1")
		_, ok2 := e.log.last("r2")
		_, ok3 := e.log.last("r3")
		return ok1 && ok2 && ok3
	})
	for _, r := range []*replica{r1, r2, r3} {
		if code, body := r.get(t, "/ready"); code != http.StatusServiceUnavailable {
			t.Errorf("%s was ready before the member list formed: %d %s", r.name, code, body)
		}
	}

	// The member list forms; every replica is ready with its share.
	e.advance(20*time.Second, "r1", "r2", "r3")
	e.inForce("r1", "r2", "r3")
	for _, r := range []*replica{r1, r2, r3} {
		if code, body := r.get(t, "/ready"); code != http.StatusOK {
			t.Errorf("%s is not ready: %d %s", r.name, code, body)
		}
	}
	checkOnce(t, owners(r1, r2, r3), all)
	for _, g := range e.sd("web") {
		owner := e.byAdvertise(g.Targets[0])
		if owner.name != g.Labels[MemberMetaLabel] {
			t.Errorf("a group at %s names the member %s", owner.name, g.Labels[MemberMetaLabel])
		}
		probe(t, owner, g, "demo_value 7")
		for _, other := range []*replica{r1, r2, r3} {
			if other != owner {
				probe(t, other, g, "demo_value 7")
				break
			}
		}
	}

	// A fourth replica joins: the configuration alone at first, then its
	// share, which the old owners drain for an interval.
	before := map[string][]string{"r1": r1.targets(), "r2": r2.targets(), "r3": r3.targets()}
	r4 := e.startReplica("r4")
	e.inForce("r4")
	if code, _ := r4.get(t, "/ready"); code != http.StatusOK || len(r4.targets()) != 0 {
		t.Errorf("the joining replica is ready %d with targets %v, want ready with none", code, r4.targets())
	}
	e.advance(20*time.Second, "r1", "r2", "r3", "r4")
	e.inForce("r1", "r2", "r3", "r4")
	four := []string{"r1", "r2", "r3", "r4"}
	var moved []string
	for _, name := range all {
		if owner, _ := allocapi.Owner(four, name); owner == "r4" {
			moved = append(moved, name)
		}
	}
	if len(moved) == 0 || !slices.Equal(r4.targets(), moved) {
		t.Fatalf("the new replica holds %v, want the targets it owns, %v", r4.targets(), moved)
	}
	for _, r := range []*replica{r1, r2, r3} {
		if !slices.Equal(r.targets(), before[r.name]) {
			t.Errorf("while draining %s holds %v, want what it held, %v", r.name, r.targets(), before[r.name])
		}
	}
	e.advance(30*time.Second, "r1", "r2", "r3", "r4")
	e.pulse.until(t, "the old owners gave up the moved targets", func() bool { return len(owners(r1, r2, r3, r4)) == len(all) && onceEach(owners(r1, r2, r3, r4)) })
	e.inForce("r1", "r2", "r3", "r4")
	checkOnce(t, owners(r1, r2, r3, r4), all)

	// r2 shuts down: once the settle time has passed its targets are the
	// others', who keep their own.
	before = map[string][]string{"r1": r1.targets(), "r3": r3.targets(), "r4": r4.targets()}
	lost := r2.targets()
	r2.client.ShuttingDown(context.Background())
	e.advance(20*time.Second, "r1", "r3", "r4")
	e.inForce("r1", "r3", "r4")
	checkOnce(t, owners(r1, r3, r4), all)
	for _, r := range []*replica{r1, r3, r4} {
		for _, name := range before[r.name] {
			if !slices.Contains(r.targets(), name) {
				t.Errorf("%s lost %s, which r2 leaving should not have moved", r.name, name)
			}
		}
		for _, name := range r.targets() {
			if !slices.Contains(before[r.name], name) && !slices.Contains(lost, name) {
				t.Errorf("%s took %s, which was not r2's", r.name, name)
			}
		}
	}
	for _, g := range e.sd("web") {
		if owner := e.byAdvertise(g.Targets[0]); owner == r2 {
			t.Errorf("a probe of %s is still sent to the replica that left", g.Labels["__param_target"])
		}
	}

	// A collector is added, with a probe job of it. Until r3 and r4 have
	// the new bundle in force, its probes go to r1 alone.
	release := e.gate.close("r3", "r4")
	testutil.WriteIn(t, filepath.Dir(p.Config), "teams/b.yaml", strings.ReplaceAll(testCollectorFile, "other", "fresh"))
	testutil.WriteIn(t, filepath.Dir(p.Config), "probes.yaml", probes+"  - name: fresh_web\n    collector: fresh\n    targets:\n"+probeTargets.String())
	if !a.Reload(p) {
		t.Fatal("the allocator did not take the added collector")
	}
	e.inForce("r1")
	for _, g := range e.sd("fresh_web") {
		if owner := e.byAdvertise(g.Targets[0]); owner != r1 {
			t.Errorf("a probe of the new collector goes to %s, which does not have it yet", owner.name)
		} else {
			probe(t, owner, g, "fresh_value 7")
		}
	}
	close(release)
	e.inForce("r1", "r3", "r4")
	live := []string{"r1", "r3", "r4"}
	for _, g := range e.sd("fresh_web") {
		owner := e.byAdvertise(g.Targets[0])
		if want, _ := allocapi.Owner(live, g.Labels["__param_target"]); owner.name != want {
			t.Errorf("a probe of %s goes to %s, want its owner %s", g.Labels["__param_target"], owner.name, want)
		}
		probe(t, owner, g, "fresh_value 7")
	}
}

// onceEach reports whether every target is held by one replica.
func onceEach(holders map[string][]string) bool {
	for _, h := range holders {
		if len(h) != 1 {
			return false
		}
	}
	return true
}

// checkOnce fails the test unless the targets held are all, each by one
// replica.
func checkOnce(t *testing.T, holders map[string][]string, all []string) {
	t.Helper()
	held := make([]string, 0, len(holders))
	for name, h := range holders {
		held = append(held, name)
		if len(h) != 1 {
			t.Errorf("%s is held by %v, want one replica", name, h)
		}
	}
	sort.Strings(held)
	if !slices.Equal(held, all) {
		t.Errorf("the replicas hold %v, want %v", held, all)
	}
}
