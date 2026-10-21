package allocator

import (
	"errors"
	"slices"
	"sort"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// SDGroup is one target group of Prometheus's HTTP service discovery.
type SDGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// The labels of a probe target besides the job's own and its parameters.
const (
	// ProbePath is the exporter's probe endpoint.
	ProbePath = "/probe"
	// MemberMetaLabel names the member a probe is routed to. Prometheus
	// drops a __meta_ label after relabeling, so a probe that moves keeps
	// its series; a relabeling rule may keep it.
	MemberMetaLabel = "__meta_pue_allocator_member"
)

var (
	// ErrUnknownJob is the answer for a probe job the probe targets file
	// does not have.
	ErrUnknownJob = errors.New("no such probe job in the probe targets file")
	// ErrNotSettled is the answer while the member list has not been
	// formed, or no member is live to route a probe to: Prometheus keeps
	// its last good targets when service discovery fails, rather than
	// dropping them all.
	ErrNotSettled = errors.New("no member to route probes to yet: no replica has been heard from long enough for the member list to form")
)

// SD is the service discovery answer for the probe job named job: a group
// per target, at the replica it is routed to.
//
// A probe is routed among the effective members that are live, or, when
// none is, among all live members; and among those, to the members whose
// heartbeat reports a bundle whose configuration has the job's collector,
// when any does, so a collector just added draws no "unknown collector"
// answers while its bundle spreads. The member is the one allocapi.Owner
// chooses for the target's shard key among them.
func (a *Allocator) SD(job string) ([]SDGroup, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.probeJob(job)
	if j == nil {
		return nil, ErrUnknownJob
	}
	pool := a.routing(j)
	if len(pool) == 0 {
		return nil, ErrNotSettled
	}
	groups := make([]SDGroup, 0, len(j.Targets))
	for _, target := range j.Targets {
		owner, _ := allocapi.Owner(pool, shardKeyOf(j, target))
		found := j.labelsOf[target]
		labels := make(map[string]string, len(found)+len(j.Labels)+len(j.Params)+5)
		// A discovered target's labels give way to the job's, and a job
		// label written "" takes one away; the labels set below, which
		// make the probe, give way to none.
		for name, value := range found {
			labels[name] = value
		}
		for name, value := range j.Labels {
			if value != "" {
				labels[name] = value
			} else {
				delete(labels, name)
			}
		}
		for name, value := range j.Params {
			labels["__param_"+name] = value
		}
		labels["__metrics_path__"] = ProbePath
		labels["__param_collector"] = j.Collector
		labels["__param_target"] = target
		labels["instance"] = target
		labels[MemberMetaLabel] = owner
		groups = append(groups, SDGroup{Targets: []string{a.members[owner].advertise}, Labels: labels})
	}
	return groups, nil
}

// probeJob is the probe job named name, nil when there is none.
func (a *Allocator) probeJob(name string) *ProbeJob {
	if a.inputs == nil || a.inputs.Probes == nil {
		return nil
	}
	for i := range a.inputs.Probes.Jobs {
		if a.inputs.Probes.Jobs[i].Name == name {
			return &a.inputs.Probes.Jobs[i]
		}
	}
	return nil
}

// routing are the members j's probes are routed among, sorted; none while
// the member list has not been formed or no member is live.
func (a *Allocator) routing(j *ProbeJob) []string {
	if !a.settled {
		return nil
	}
	now := a.clock.Now()
	var base, all []string
	for name, m := range a.members {
		if !live(m, now) {
			continue
		}
		all = append(all, name)
		if slices.Contains(a.effective, name) {
			base = append(base, name)
		}
	}
	if len(base) == 0 {
		base = all
	}
	var qualified []string
	for _, name := range base {
		if a.collectorsOf[a.members[name].generation][j.Collector] {
			qualified = append(qualified, name)
		}
	}
	if len(qualified) > 0 {
		base = qualified
	}
	sort.Strings(base)
	return base
}
