package allocator

import (
	"bufio"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// The status of an allocator instance, for people who have no Prometheus to
// ask or want it all on one page: the instance itself, its peers and the
// leader, the members, how the targets are spread over them, and the
// discovery sources. StatusPath serves it as JSON, and the status page
// (statuspage.go) renders the same. It is built from what the allocator
// already keeps for its work and its metrics, so the page and /metrics never
// disagree.

// Status is the answer at StatusPath.
type Status struct {
	// GeneratedAt is when the status was taken, RFC 3339 in UTC.
	GeneratedAt string         `json:"generated_at"`
	Instance    InstanceStatus `json:"instance"`
	// Peers is nil without --peers.
	Peers *PeersStatus `json:"peers"`
	// Partial says the instance follows under --peers.leader-only: it leaves
	// the members, the spread and the discovery sources to the leader, and
	// Members, Spread and Sources are nil.
	Partial bool           `json:"partial"`
	Members *MembersStatus `json:"members"`
	Spread  *SpreadStatus  `json:"spread"`
	Sources []SourceStatus `json:"sources"`
}

// InstanceStatus is the instance answering.
type InstanceStatus struct {
	// Address is the instance's address among its peers when it is known, and
	// else its --web.listen-address. ID is its process's id (newInstanceID).
	Address  string `json:"address"`
	ID       string `json:"id"`
	Version  string `json:"version"`
	Revision string `json:"revision"`
	// StartedAt is when the instance started, RFC 3339 in UTC, and
	// UptimeSeconds how long ago.
	StartedAt     string  `json:"started_at"`
	UptimeSeconds float64 `json:"uptime_seconds"`
	// Ready is what /ready answers: false once the instance shuts down.
	Ready bool `json:"ready"`
	// WatchIntervalSeconds is --config.watch-interval, how often the input
	// files are read again for changes; 0 for files read once, at the
	// start.
	WatchIntervalSeconds float64 `json:"watch_interval_seconds"`
}

// PeersStatus is the instance's part among its peers.
type PeersStatus struct {
	// Role is leader or follower, or deciding before the first round.
	Role string `json:"role"`
	// LeaderOnly is --peers.leader-only.
	LeaderOnly bool `json:"leader_only"`
	// Leader is the leader's address, "self" while this instance leads
	// without knowing its own, "" before the first round.
	Leader string `json:"leader"`
	// Discovering says this instance asks its discovery sources, and
	// AddressKnown that it knows its own address.
	Discovering  bool `json:"discovering"`
	AddressKnown bool `json:"address_known"`
	// LiveInstances are the live instances, this one included.
	LiveInstances int    `json:"live_instances"`
	LeaderChanges uint64 `json:"leader_changes"`
	// SnapshotAgeSeconds is pue_allocator_snapshot_age_seconds;
	// SnapshotTaken says the instance has fetched a snapshot, and
	// HoldsLeaderState that it leads, or held what the leader's snapshot
	// holds after its last fetch.
	SnapshotAgeSeconds float64 `json:"snapshot_age_seconds"`
	SnapshotTaken      bool    `json:"snapshot_taken"`
	HoldsLeaderState   bool    `json:"holds_leader_state"`
	// SnapshotFailuresInARow are the fetches of the leader's snapshot that
	// failed since the last that did not.
	SnapshotFailuresInARow int    `json:"snapshot_failures_in_a_row"`
	CheckFailures          uint64 `json:"check_failures"`
	SnapshotFailures       uint64 `json:"snapshot_failures"`
	// Instances are every address of the peers, this instance's included,
	// in the order the leader is chosen by.
	Instances []PeerStatus `json:"instances"`
	// Metrics are the series of the peers' families in /metrics, as
	// /metrics has them.
	Metrics []MetricSample `json:"metrics"`
}

// PeerStatus is one address of the peers.
type PeerStatus struct {
	Address string `json:"address"`
	// Self says it is this instance, and Leader that it is the leader.
	Self   bool `json:"self"`
	Leader bool `json:"leader"`
	// Live says the instance answered and has not failed PeerFailures
	// checks in a row since; this instance is always live.
	Live bool `json:"live"`
	// ID, Discovering and AddressKnown are the instance's last answer.
	ID           string `json:"id,omitempty"`
	Discovering  bool   `json:"discovering"`
	AddressKnown bool   `json:"address_known"`
	// LastCheck is when it was last checked, RFC 3339 in UTC, and
	// LastCheckAgeSeconds how long ago; "" and 0 for this instance.
	LastCheck           string  `json:"last_check,omitempty"`
	LastCheckAgeSeconds float64 `json:"last_check_age_seconds,omitempty"`
	// Failures are the checks in a row it failed.
	Failures int `json:"failures"`
}

// MetricSample is one series of /metrics and its value, as written there.
type MetricSample struct {
	Series string `json:"series"`
	Value  string `json:"value"`
}

// MembersStatus are the members the allocator knows.
type MembersStatus struct {
	// Formed says the member list has formed; before, no bundle and no
	// service discovery answer is served.
	Formed bool `json:"formed"`
	// InList are the members of the member list allocation uses, and Live
	// the live members.
	InList int `json:"in_list"`
	Live   int `json:"live"`
	// HeartbeatIntervalSeconds is --heartbeat.interval, and
	// MemberTTLSeconds how long a member is live after its last heartbeat,
	// three intervals.
	HeartbeatIntervalSeconds float64 `json:"heartbeat_interval_seconds"`
	MemberTTLSeconds         float64 `json:"member_ttl_seconds"`
	// SettleSeconds is --settle, how long a changed member list must hold
	// before targets move.
	SettleSeconds float64 `json:"settle_seconds"`
	// List is every member known, by id.
	List []MemberStatus `json:"list"`
}

// MemberStatus is one member.
type MemberStatus struct {
	ID string `json:"id"`
	// Address is the host:port its heartbeats advertise.
	Address string `json:"address"`
	// LastHeartbeat is when its last heartbeat came, RFC 3339 in UTC, and
	// LastHeartbeatAgeSeconds how long ago.
	LastHeartbeat           string  `json:"last_heartbeat"`
	LastHeartbeatAgeSeconds float64 `json:"last_heartbeat_age_seconds"`
	// Late says it is live but its last heartbeat is more than one and a
	// half heartbeat intervals old: one is overdue, and it stops being
	// live when its TTL runs out.
	Late bool `json:"late"`
	// Live says it is heard from within the member TTL and not shutting
	// down; InList that it is in the member list.
	Live         bool `json:"live"`
	InList       bool `json:"in_list"`
	ShuttingDown bool `json:"shutting_down"`
	// Generation is the generation of the bundle it reports in force,
	// Current the one it is to have; UpToDate says they are the same.
	Generation string `json:"generation"`
	Current    string `json:"current"`
	UpToDate   bool   `json:"up_to_date"`
	// Refused is the generation of the last bundle it reported refusing,
	// while it reports it.
	Refused string `json:"refused,omitempty"`
	// Draining are the static targets it keeps for one of their intervals
	// after they moved to another member.
	Draining int `json:"draining"`
}

// SpreadStatus is how the targets are spread over the members.
type SpreadStatus struct {
	// Members are the members of the member list, and any other live
	// member a probe is routed to, by id.
	Members []SpreadMember `json:"members"`
	// Total is the sum over the members, Average the same divided by their
	// number, and Imbalance the most a member has divided by Average: 1
	// for targets spread evenly, 0 when there are none.
	Total     SpreadCounts `json:"total"`
	Average   SpreadCounts `json:"average"`
	Imbalance SpreadCounts `json:"imbalance"`
	// Jobs are the probe jobs, in file order, each with how its targets
	// are spread: one column of the members' table each would not fit a
	// page beside a few jobs.
	Jobs []JobSpread `json:"jobs"`
}

// SpreadMember is one member's targets.
type SpreadMember struct {
	Member string `json:"member"`
	SpreadCounts
}

// SpreadCounts count a member's targets, or a sum, an average or an
// imbalance of them: Static, the static targets it owns, Draining those it
// keeps draining, Probe the probe targets service discovery routes to it,
// and Total all of them.
type SpreadCounts struct {
	Static   float64 `json:"static"`
	Draining float64 `json:"draining"`
	Probe    float64 `json:"probe"`
	Total    float64 `json:"total"`
}

// JobSpread is how one probe job's targets are spread over the members
// its probes are routed among (routing): those are the members a target
// of the job can go to, so one of them given none of its targets counts,
// with 0, where a member outside them would not.
type JobSpread struct {
	Job       string `json:"job"`
	Collector string `json:"collector"`
	// Targets are the job's targets, listed and discovered.
	Targets int `json:"targets"`
	// Members are the members it is routed among, by id, each with the
	// targets routed to it; none while no member is live or the member
	// list has not formed.
	Members []JobMember `json:"members"`
	// Min, Average and Max are the fewest, the mean and the most targets
	// of a member of Members, and Imbalance is Max divided by Average: 1
	// for targets spread evenly, 0 when there are none.
	Min       int     `json:"min"`
	Average   float64 `json:"average"`
	Max       int     `json:"max"`
	Imbalance float64 `json:"imbalance"`
	// Uneven says Max is at least half again as many as the evenest split
	// would give the member of the most, Targets divided by the members
	// and rounded up (unevenFactor). Against Average alone a job of few
	// targets would always read uneven: 2 targets over 3 members cannot
	// do better than 1.5.
	Uneven bool `json:"uneven"`
	// Sources are the job's discovery sources, in the order of the
	// status's; none for a job whose file lists every target.
	Sources []JobSource `json:"sources"`
}

// JobMember is the targets of a probe job routed to one member.
type JobMember struct {
	Member  string `json:"member"`
	Targets int    `json:"targets"`
}

// JobSource is one discovery source of a probe job, as SourceStatus has
// it.
type JobSource struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Result string `json:"result"`
}

// unevenFactor is how many times the evenest split's most a job's most
// must be for the job to be uneven (JobSpread.Uneven).
const unevenFactor = 1.5

// SourceStatus is one discovery source.
type SourceStatus struct {
	Name string `json:"name"`
	// Kind is probe or static, Owner the probe job or discovered_targets
	// entry, and Type the source's kind, file_sd, http_sd, kubernetes or
	// dns_sd.
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
	Type  string `json:"type"`
	// Result is ok, failing (it keeps the targets of its last good answer)
	// or pending (it has not answered yet); Error is the last failure's
	// text while it fails.
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	// Targets are those of its last good answer.
	Targets int `json:"targets"`
	// RefreshIntervalSeconds is how often it is asked: its
	// refresh_interval, or --discovery.refresh-interval for a source that
	// sets none, or its kind's default; for what a follower took from the
	// leader's snapshot (From snapshot), the leader's, who asks it.
	RefreshIntervalSeconds float64 `json:"refresh_interval_seconds"`
	// LastRefresh is when it was last asked or taken from the leader's
	// snapshot, LastSuccess when that last succeeded, RFC 3339 in UTC, ""
	// for never.
	LastRefresh           string  `json:"last_refresh,omitempty"`
	LastRefreshAgeSeconds float64 `json:"last_refresh_age_seconds,omitempty"`
	LastSuccess           string  `json:"last_success,omitempty"`
	// From is own when what it holds is this instance's own discovery, snapshot
	// when it came from the leader's snapshot, "" before either.
	From string `json:"from,omitempty"`
}

// Status is the status of the allocator now. An instance that does not lead
// under --peers.leader-only gives its own part and its peers' alone
// (Partial).
func (a *Allocator) Status() Status {
	now := a.clock.Now()
	s := Status{GeneratedAt: now.UTC().Format(time.RFC3339)}
	a.mu.Lock()
	ready := a.inputs != nil && !a.stopped
	s.Peers = a.peersStatusLocked(now)
	s.Partial = s.Peers != nil && s.Peers.LeaderOnly && s.Peers.Role != "leader"
	if !s.Partial {
		s.Members = a.membersStatusLocked(now)
		s.Spread = a.spreadLocked()
	}
	a.mu.Unlock()

	s.Instance = InstanceStatus{Address: a.address, ID: a.id, Version: a.version, Revision: a.revision,
		StartedAt: a.started.UTC().Format(time.RFC3339), UptimeSeconds: ageSeconds(now.Sub(a.started).Seconds()), Ready: ready,
		WatchIntervalSeconds: a.watchInterval.Seconds()}
	if s.Peers != nil {
		for _, c := range s.Peers.Instances {
			if c.Self {
				s.Instance.Address = c.Address
			}
		}
	}
	if !s.Partial {
		s.Sources = a.sourcesStatus(now)
		sourcesOfJobs(s.Spread, s.Sources)
	}
	return s
}

// ageSeconds is d, in seconds, to the millisecond, never below 0.
func ageSeconds(d float64) float64 {
	return max(float64(int64(d*1000))/1000, 0)
}

// membersStatusLocked is the members. Called under a.mu.
func (a *Allocator) membersStatusLocked(now time.Time) *MembersStatus {
	out := &MembersStatus{Formed: a.settled, List: []MemberStatus{},
		HeartbeatIntervalSeconds: a.heartbeatInterval.Seconds(), MemberTTLSeconds: a.memberTTL.Seconds(), SettleSeconds: a.settle.Seconds()}
	if a.settled {
		out.InList = len(a.effective)
	}
	draining := map[string]int{}
	for key := range a.draining {
		draining[key.member]++
	}
	for id, m := range a.members {
		ms := MemberStatus{ID: id, Address: m.advertise, LastHeartbeat: m.lastSeen.UTC().Format(time.RFC3339),
			LastHeartbeatAgeSeconds: ageSeconds(now.Sub(m.lastSeen).Seconds()), Live: a.live(m, now),
			InList: a.settled && contains(a.effective, id), ShuttingDown: m.shuttingDown, Generation: m.generation,
			Refused: a.refusals[id], Draining: draining[id]}
		if b := a.bundleLocked(id); b != nil {
			ms.Current = b.Generation
		}
		ms.UpToDate = ms.Current != "" && ms.Generation == ms.Current
		ms.Late = ms.Live && now.Sub(m.lastSeen) > a.heartbeatInterval*3/2
		if ms.Live {
			out.Live++
		}
		out.List = append(out.List, ms)
	}
	sort.Slice(out.List, func(i, j int) bool { return out.List[i].ID < out.List[j].ID })
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// spreadLocked is how the targets are spread over the members: a static
// target to its owner in the member list, and a probe target to the member
// service discovery routes it to, as /metrics counts them. Called under
// a.mu.
func (a *Allocator) spreadLocked() *SpreadStatus {
	out := &SpreadStatus{Members: []SpreadMember{}, Jobs: []JobSpread{}}
	rows := map[string]*SpreadMember{}
	row := func(name string) *SpreadMember {
		r := rows[name]
		if r == nil {
			r = &SpreadMember{Member: name}
			rows[name] = r
		}
		return r
	}
	for _, name := range a.effective {
		row(name)
	}
	in := a.inputs
	if in != nil && len(a.effective) > 0 {
		for i := range in.shardKeys {
			owner, _ := allocapi.Owner(a.effective, in.shardKeys[i])
			row(owner).Static++
		}
		for key := range a.draining {
			row(key.member).Draining++
		}
	}
	if in != nil && in.Probes != nil {
		for i := range in.Probes.Jobs {
			j := &in.Probes.Jobs[i]
			js, _ := a.jobSpreadLocked(j)
			// A job's targets count towards the members' rows once the
			// member list has formed, as the static targets do.
			if len(a.effective) > 0 {
				for _, m := range js.Members {
					row(m.Member).Probe += float64(m.Targets)
				}
			}
			out.Jobs = append(out.Jobs, js)
		}
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := rows[name]
		r.Total = r.Static + r.Draining + r.Probe
		out.Members = append(out.Members, *r)
	}

	var most SpreadCounts
	for _, r := range out.Members {
		add := func(sum, top *float64, v float64) {
			*sum += v
			*top = max(*top, v)
		}
		add(&out.Total.Static, &most.Static, r.Static)
		add(&out.Total.Draining, &most.Draining, r.Draining)
		add(&out.Total.Probe, &most.Probe, r.Probe)
		add(&out.Total.Total, &most.Total, r.Total)
	}
	n := float64(len(out.Members))
	average := func(v float64) float64 {
		if n == 0 {
			return 0
		}
		return v / n
	}
	imbalance := func(top, sum float64) float64 {
		if avg := average(sum); avg > 0 {
			return top / avg
		}
		return 0
	}
	out.Average = SpreadCounts{Static: average(out.Total.Static), Draining: average(out.Total.Draining),
		Probe: average(out.Total.Probe), Total: average(out.Total.Total)}
	out.Imbalance = SpreadCounts{Static: imbalance(most.Static, out.Total.Static), Draining: imbalance(most.Draining, out.Total.Draining),
		Probe: imbalance(most.Probe, out.Total.Probe), Total: imbalance(most.Total, out.Total.Total)}
	return out
}

// jobSpreadLocked is how j's targets are spread over the members it is
// routed among, and the member each target is routed to, "" for none.
// Called under a.mu.
func (a *Allocator) jobSpreadLocked(j *ProbeJob) (JobSpread, map[string]string) {
	out := JobSpread{Job: j.Name, Collector: j.Collector, Targets: len(j.Targets), Members: []JobMember{}, Sources: []JobSource{}}
	pool := a.routing(j)
	owners := make(map[string]string, len(j.Targets))
	counts := make(map[string]int, len(pool))
	for _, target := range j.Targets {
		if owner, err := allocapi.Owner(pool, shardKeyOf(j, target)); err == nil {
			owners[target] = owner
			counts[owner]++
		}
	}
	for _, name := range pool {
		out.Members = append(out.Members, JobMember{Member: name, Targets: counts[name]})
	}
	if len(out.Members) == 0 {
		return out, owners
	}
	routed := 0
	out.Min = out.Members[0].Targets
	for _, m := range out.Members {
		routed += m.Targets
		out.Min, out.Max = min(out.Min, m.Targets), max(out.Max, m.Targets)
	}
	out.Average = float64(routed) / float64(len(out.Members))
	if out.Average > 0 {
		out.Imbalance = float64(out.Max) / out.Average
		evenest := (routed + len(out.Members) - 1) / len(out.Members)
		out.Uneven = float64(out.Max) >= unevenFactor*float64(evenest)
	}
	return out, owners
}

// sourcesOfJobs puts each probe job's discovery sources of sources in its
// spread.
func sourcesOfJobs(sp *SpreadStatus, sources []SourceStatus) {
	if sp == nil {
		return
	}
	for i := range sp.Jobs {
		for _, s := range sources {
			if s.Kind == "probe" && s.Owner == sp.Jobs[i].Job {
				sp.Jobs[i].Sources = append(sp.Jobs[i].Sources, JobSource{Name: s.Name, Type: s.Type, Result: s.Result})
			}
		}
	}
}

// sourcesStatus is every discovery source, in the order of /metrics.
func (a *Allocator) sourcesStatus(now time.Time) []SourceStatus {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	out := make([]SourceStatus, 0, len(a.sources))
	for _, s := range a.sources {
		st := SourceStatus{Name: s.spec.cfg.Name, Kind: s.spec.kind, Owner: s.spec.owner, Type: s.spec.cfg.kind(),
			RefreshIntervalSeconds: s.interval().Seconds()}
		switch {
		case s.lastErr != "":
			st.Result, st.Error = "failing", s.lastErr
		case s.ok:
			st.Result = "ok"
		default:
			st.Result = "pending"
		}
		if s.ok {
			st.Targets = len(s.found)
			st.From = "own"
			if s.fromSnapshot {
				st.From = "snapshot"
			}
		}
		if !s.refreshed.IsZero() {
			st.LastRefresh = s.refreshed.UTC().Format(time.RFC3339)
			st.LastRefreshAgeSeconds = ageSeconds(now.Sub(s.refreshed).Seconds())
		}
		if !s.lastGood.IsZero() {
			st.LastSuccess = s.lastGood.UTC().Format(time.RFC3339)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// peersStatusLocked is the instance's part among its peers, nil without
// --peers. Called under a.mu, as writePeerMetrics, which writes the
// metrics from it.
func (a *Allocator) peersStatusLocked(now time.Time) *PeersStatus {
	p := a.peers
	if p == nil {
		return nil
	}
	a.dmu.Lock()
	discovering := a.discovering
	a.dmu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	st := &PeersStatus{Role: "deciding", LeaderOnly: p.o.LeaderOnly, Discovering: discovering, AddressKnown: p.self.IsValid(),
		LiveInstances: 1, LeaderChanges: p.leaderChanges, SnapshotTaken: p.took, SnapshotFailuresInARow: p.snapshotFails,
		CheckFailures: p.checkFailures, SnapshotFailures: p.snapshotFailures, Instances: []PeerStatus{}}
	if p.decided {
		st.Role = "follower"
		if p.leading {
			st.Role = "leader"
		}
		st.Leader = "self"
		if p.leader.IsValid() {
			st.Leader = p.leader.String()
		}
	}
	leading := p.decided && p.leading
	st.HoldsLeaderState = leading || (p.took && !p.snapshotAt.IsZero() && p.differ == 0)
	if !leading {
		since := p.snapshotAt
		if since.IsZero() {
			since = a.started
		}
		st.SnapshotAgeSeconds = max(now.Sub(since).Seconds(), 0)
	}
	selfListed := false
	for addr, ps := range p.known {
		c := PeerStatus{Address: addr.String(), Self: ps.self, Leader: p.decided && addr == p.leader, Failures: ps.fails}
		if ps.self {
			selfListed = true
			c.Live, c.ID, c.Discovering, c.AddressKnown = true, a.id, discovering, true
			c.Leader = leading
		} else {
			c.Live, c.ID, c.Discovering, c.AddressKnown = ps.live, ps.answer.ID, ps.answer.Discovering, ps.answer.AddressKnown
			if ps.live {
				st.LiveInstances++
			}
			if !ps.checked.IsZero() {
				c.LastCheck = ps.checked.UTC().Format(time.RFC3339)
				c.LastCheckAgeSeconds = ageSeconds(now.Sub(ps.checked).Seconds())
			}
		}
		st.Instances = append(st.Instances, c)
	}
	if !selfListed {
		// An instance that does not know its own address is listed too, so the
		// list always shows it.
		st.Instances = append(st.Instances, PeerStatus{Address: addrText(p.self), Self: true, Leader: leading, Live: true,
			ID: a.id, Discovering: discovering, AddressKnown: p.self.IsValid()})
	}
	sort.Slice(st.Instances, func(i, j int) bool {
		x, errX := netip.ParseAddrPort(st.Instances[i].Address)
		y, errY := netip.ParseAddrPort(st.Instances[j].Address)
		if errX != nil || errY != nil {
			return errX == nil
		}
		return comparePeers(x, y) < 0
	})
	var b strings.Builder
	writePeerFamilies(&b, func(string, string, string) {}, st)
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		series, value, _ := strings.Cut(sc.Text(), " ")
		st.Metrics = append(st.Metrics, MetricSample{Series: series, Value: value})
	}
	return st
}

// writePeerFamilies writes the metrics of the peers from st, family
// writing each family's HELP and TYPE.
func writePeerFamilies(b *strings.Builder, family func(name, kind, help string), st *PeersStatus) {
	family("pue_allocator_peers", "gauge", "Allocator instances live among the peers, this one included.")
	fmt.Fprintf(b, "pue_allocator_peers %d\n", st.LiveInstances)
	family("pue_allocator_peer_up", "gauge", "1 for each other allocator instance among the peers that is live, by its address, 0 for one that has stopped answering.")
	for _, c := range st.Instances {
		if c.Self {
			continue
		}
		up := 0
		if c.Live {
			up = 1
		}
		fmt.Fprintf(b, "pue_allocator_peer_up{peer=\"%s\"} %d\n", escape(c.Address), up)
	}
	family("pue_allocator_is_leader", "gauge", "1 while this allocator instance leads its peers and asks the discovery sources, 0 while it follows the leader.")
	family("pue_allocator_leader", "gauge", "1, with the address of the instance this one takes for the leader; self when it leads and its own address is not known.")
	isLeader := 0
	if st.Role == "leader" {
		isLeader = 1
	}
	fmt.Fprintf(b, "pue_allocator_is_leader %d\n", isLeader)
	if st.Leader != "" {
		fmt.Fprintf(b, "pue_allocator_leader{peer=\"%s\"} 1\n", escape(st.Leader))
	}
	family("pue_allocator_leader_changes_total", "counter", "Changes of the instance this one takes for the leader, after the first choice.")
	fmt.Fprintf(b, "pue_allocator_leader_changes_total %d\n", st.LeaderChanges)
	family("pue_allocator_snapshot_age_seconds", "gauge", "Seconds since this instance last held the discovered targets of the leader's snapshot, having fetched it, or since its start before the first time; 0 while it leads.")
	fmt.Fprintf(b, "pue_allocator_snapshot_age_seconds %s\n", strconv.FormatFloat(st.SnapshotAgeSeconds, 'f', -1, 64))
	family("pue_allocator_peer_failures_total", "counter", "Requests to the peers that failed, by request: check, a peer's liveness, or snapshot, a follower's fetch of the leader's discovered targets.")
	fmt.Fprintf(b, "pue_allocator_peer_failures_total{request=\"check\"} %d\npue_allocator_peer_failures_total{request=\"snapshot\"} %d\n", st.CheckFailures, st.SnapshotFailures)
}
