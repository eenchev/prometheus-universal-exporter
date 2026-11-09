package allocator

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WriteMetrics writes the allocator's own metrics in the Prometheus text
// format, every name prefixed pue_allocator_.
//
// A follower under --peers.leader-only writes the peers' families alone
// (writePeerMetrics): is_leader 0, the leader, the peers and their
// liveness, the snapshot's age and the failures, so it stays observable and
// a cluster without a leader shows, while the allocation, the members and
// the discovery sources are in the leader's metrics only, and a Prometheus
// scraping every instance keeps one series of each.
func (a *Allocator) WriteMetrics(w io.Writer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	family := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	if st := a.peersStatusLocked(a.clock.Now()); st != nil && st.LeaderOnly && st.Role != "leader" {
		writePeerFamilies(&b, family, st)
		_, err := io.WriteString(w, b.String())
		return err
	}
	family("pue_allocator_members", "gauge", "Members in the member list allocation uses: the replicas heard from within three heartbeat intervals (--heartbeat.interval), once the list has held for the settle time.")
	fmt.Fprintf(&b, "pue_allocator_members %d\n", len(a.effective))

	static, probes := 0, 0
	if in := a.inputs; in != nil {
		if in.Targets != nil {
			static = len(in.Targets.Targets)
		}
		if in.Probes != nil {
			for i := range in.Probes.Jobs {
				probes += len(in.Probes.Jobs[i].Targets)
			}
		}
	}
	family("pue_allocator_targets", "gauge", "Targets of the input files in force, by kind: static, from the static target file, or probe, from the probe targets file.")
	fmt.Fprintf(&b, "pue_allocator_targets{kind=\"probe\"} %d\npue_allocator_targets{kind=\"static\"} %d\n", probes, static)

	family("pue_allocator_assigned", "gauge", "Targets allocated to each member of the member list, by kind: static targets it owns, without those it keeps draining, and probe targets service discovery routes to it.")
	loads := a.loadsLocked()
	names := loads.names
	for _, name := range names {
		fmt.Fprintf(&b, "pue_allocator_assigned{kind=\"probe\",member=\"%s\"} %d\n", escape(name), loads.rows[name].probe)
		fmt.Fprintf(&b, "pue_allocator_assigned{kind=\"static\",member=\"%s\"} %d\n", escape(name), loads.rows[name].static)
	}
	family("pue_allocator_assigned_weight", "gauge", "The weight of the targets allocated to each member of the member list, by kind, as pue_allocator_assigned counts them: a static target that moved weighs on its new owner, not on the member that keeps it draining.")
	for _, name := range names {
		fmt.Fprintf(&b, "pue_allocator_assigned_weight{kind=\"probe\",member=\"%s\"} %s\n", escape(name), formatMilli(loads.rows[name].probeWeight))
		fmt.Fprintf(&b, "pue_allocator_assigned_weight{kind=\"static\",member=\"%s\"} %s\n", escape(name), formatMilli(loads.rows[name].staticWeight))
	}
	family("pue_allocator_member_capacity", "gauge", "The capacity each member is placed by: its --allocator.capacity in the member list, or, for a live member outside it that a probe is routed to, its last heartbeat's.")
	for _, name := range names {
		fmt.Fprintf(&b, "pue_allocator_member_capacity{member=\"%s\"} %s\n", escape(name), formatMilli(loads.rows[name].capacity))
	}
	family("pue_allocator_member_load_ratio", "gauge", "Each member's static and probe weight over its fair share, the weight of every member times its capacity over theirs: 1 for weight in proportion to capacity; 0 for a member of no fair share.")
	overloaded := 0
	for _, name := range names {
		r := loads.rows[name]
		ratio := 0.0
		if r.fair > 0 {
			ratio = float64(r.staticWeight+r.probeWeight) / float64(r.fair)
		}
		if r.overloaded {
			overloaded++
		}
		fmt.Fprintf(&b, "pue_allocator_member_load_ratio{member=\"%s\"} %s\n", escape(name), strconv.FormatFloat(ratio, 'g', -1, 64))
	}
	family("pue_allocator_overloaded_members", "gauge", "Members whose static and probe weight is above --allocation.balance-factor times their fair share because a target, or the targets of one shard key, weigh too much for any member's room: still above it without their lightest unit, or filled by one unit above it alone.")
	fmt.Fprintf(&b, "pue_allocator_overloaded_members %d\n", overloaded)

	// The average is what one more member would take off each: the figure to
	// scale the exporter's replicas by when it is to keep so many targets
	// each. A member list that is empty has no average, and shows 0, as it
	// shows no member assigned anything.
	family("pue_allocator_targets_per_member", "gauge", "Targets allocated to the members of the member list, by kind, divided by the number of members: static targets and probe targets per member on average; 0 with no member.")
	var allStatic, allProbe int
	for _, name := range names {
		allStatic += loads.rows[name].static
		allProbe += loads.rows[name].probe
	}
	perMember := func(total int) string {
		if len(a.effective) == 0 {
			return "0"
		}
		return strconv.FormatFloat(float64(total)/float64(len(a.effective)), 'g', -1, 64)
	}
	fmt.Fprintf(&b, "pue_allocator_targets_per_member{kind=\"probe\"} %s\npue_allocator_targets_per_member{kind=\"static\"} %s\n", perMember(allProbe), perMember(allStatic))

	family("pue_allocator_generation_info", "gauge", "The generation of the bundle each member of the member list is to have; a replica reports the one it has in force in its heartbeats.")
	members := make([]string, 0, len(a.bundles))
	for name := range a.bundles {
		members = append(members, name)
	}
	sort.Strings(members)
	for _, name := range members {
		fmt.Fprintf(&b, "pue_allocator_generation_info{generation=\"%s\",member=\"%s\"} 1\n", a.bundles[name].Generation, escape(name))
	}

	family("pue_allocator_reloads_total", "counter", "Reloads of the input files that changed them, by result: success when the changed files were put in force, failure when they were refused and the last good ones stayed; one refusal is counted once while it stays the same.")
	fmt.Fprintf(&b, "pue_allocator_reloads_total{result=\"failure\"} %d\npue_allocator_reloads_total{result=\"success\"} %d\n", a.reloadsFailed, a.reloadsOK)

	family("pue_allocator_member_changes_total", "counter", "Changes of the member list allocation uses, after the one it was first formed with.")
	fmt.Fprintf(&b, "pue_allocator_member_changes_total %d\n", a.memberChanges)

	family("pue_allocator_members_refused_total", "counter", "Heartbeats of new members refused because as many members as --members.max allows were live.")
	fmt.Fprintf(&b, "pue_allocator_members_refused_total %d\n", a.membersRefused)

	sameName, sameNameNow := 0, a.clock.Now()
	for _, m := range a.members {
		if a.twice(m, sameNameNow) {
			sameName++
		}
	}
	family("pue_allocator_members_same_name", "gauge", "Members under whose name two exporters heartbeat, each scraping the member's share of the static targets; 0 unless two exporters were given one --allocator.member.")
	fmt.Fprintf(&b, "pue_allocator_members_same_name %d\n", sameName)

	family("pue_allocator_weight_label_fallbacks_total", "counter", "Discovered targets whose owner's weight_label gave no weight, which took the owner's weight instead, by kind (probe or static), owner (the probe job or the discovered_targets entry) and reason: missing, not_a_number, out_of_range (not above 0 and at most 1000) or too_many_decimals; a target is counted when it begins to fall back.")
	keys := make([]fallbackKey, 0, len(a.fallbacksTotal))
	for key := range a.fallbacksTotal {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		x, y := keys[i], keys[j]
		if x.kind != y.kind {
			return x.kind < y.kind
		}
		if x.owner != y.owner {
			return x.owner < y.owner
		}
		return x.reason < y.reason
	})
	for _, key := range keys {
		fmt.Fprintf(&b, "pue_allocator_weight_label_fallbacks_total{kind=\"%s\",owner=\"%s\",reason=\"%s\"} %d\n", key.kind, escape(key.owner), key.reason, a.fallbacksTotal[key])
	}

	sources := a.sourceMetrics()
	labels := func(m sourceMetric) string {
		return fmt.Sprintf("kind=\"%s\",owner=\"%s\",source=\"%s\"", m.kind, escape(m.owner), escape(m.name))
	}
	family("pue_allocator_discovered_targets", "gauge", "Targets in the last good answer of each discovery source, by kind (probe or static), owner (the probe job or the discovered_targets entry) and source; 0 for a source that has never answered.")
	for _, m := range sources {
		fmt.Fprintf(&b, "pue_allocator_discovered_targets{%s} %d\n", labels(m), m.found)
	}
	family("pue_allocator_discovery_refreshes_total", "counter", "Refreshes of each discovery source, by result: success for every answer taken, failure once for a failure while its text stays the same.")
	for _, m := range sources {
		fmt.Fprintf(&b, "pue_allocator_discovery_refreshes_total{%s,result=\"failure\"} %d\n", labels(m), m.failures)
		fmt.Fprintf(&b, "pue_allocator_discovery_refreshes_total{%s,result=\"success\"} %d\n", labels(m), m.successes)
	}
	family("pue_allocator_discovery_last_success_timestamp_seconds", "gauge", "When each discovery source last answered, in seconds since the Unix epoch; 0 for a source that has never answered.")
	for _, m := range sources {
		at := 0.0
		if !m.lastGood.IsZero() {
			at = float64(m.lastGood.UnixNano()) / 1e9
		}
		fmt.Fprintf(&b, "pue_allocator_discovery_last_success_timestamp_seconds{%s} %s\n", labels(m), strconv.FormatFloat(at, 'f', -1, 64))
	}
	a.writePeerMetrics(&b, family)
	_, err := io.WriteString(w, b.String())
	return err
}

// escape writes a label value as the text format quotes it.
func escape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}
