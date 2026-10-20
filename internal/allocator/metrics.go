package allocator

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// WriteMetrics writes the allocator's own metrics in the Prometheus text
// format, every name prefixed pue_allocator_.
func (a *Allocator) WriteMetrics(w io.Writer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	family := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	family("pue_allocator_members", "gauge", "Members in the member list allocation uses: the replicas heard from within the member TTL, once the list has held for the settle time.")
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
	type count struct{ static, probe int }
	assigned := map[string]*count{}
	for _, name := range a.effective {
		assigned[name] = &count{}
	}
	if in := a.inputs; in != nil && len(a.effective) > 0 {
		for i := range in.shardKeys {
			owner, _ := allocapi.Owner(a.effective, in.shardKeys[i])
			assigned[owner].static++
		}
		if in.Probes != nil {
			for i := range in.Probes.Jobs {
				j := &in.Probes.Jobs[i]
				pool := a.routing(j)
				for _, target := range j.Targets {
					if owner, err := allocapi.Owner(pool, shardKeyOf(j, target)); err == nil {
						if assigned[owner] == nil {
							assigned[owner] = &count{}
						}
						assigned[owner].probe++
					}
				}
			}
		}
	}
	names := make([]string, 0, len(assigned))
	for name := range assigned {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "pue_allocator_assigned{kind=\"probe\",member=\"%s\"} %d\n", escape(name), assigned[name].probe)
		fmt.Fprintf(&b, "pue_allocator_assigned{kind=\"static\",member=\"%s\"} %d\n", escape(name), assigned[name].static)
	}

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
	_, err := io.WriteString(w, b.String())
	return err
}

// escape writes a label value as the text format quotes it.
func escape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}
