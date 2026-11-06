package allocator

import (
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A probe job's view, at /?job=<name> and as JSON at StatusPath?job=<name>,
// is what one probe job is and where its targets go: its collector, its
// labels and params, its discovery sources, how many of its targets each
// member it is routed among has, and each target with the member it is
// routed to. It mirrors a member's view (memberview.go): it is worked out
// as the status's spread is, so its counts are the spread's for the job,
// its list is bounded by MemberViewLimit and cut to a filter, and a target
// is shown as the exporter shows it (fetch.DisplayTarget). A param whose
// name reads as a credential's is withheld, and any other has what reads
// as one in its value masked (fetch.RedactText), as the exporter's status
// page shows a probe's params: a probe targets file may carry an API key
// as a param.
//
// A discovered target carries its source's labels whose names start with
// __, as __meta_kubernetes_namespace, for the job's relabeling: a dozen or
// more a target, they bury the labels the target is shown with. The page
// leaves them out unless asked for them, ?meta=1, and its filter then
// matches only the labels shown, as a row's data-match does; the JSON
// keeps them all.

// JobView is the answer at StatusPath?job=<name>.
type JobView struct {
	// GeneratedAt is when the view was taken, RFC 3339 in UTC.
	GeneratedAt string `json:"generated_at"`
	Job         string `json:"job"`
	// Partial says the instance follows under --peers.leader-only and leaves
	// the members to the leader, at Leader; Spread, Labels, Params, Sources
	// and Targets are nil.
	Partial bool   `json:"partial"`
	Leader  string `json:"leader,omitempty"`
	// Filter is the filter asked for, which the list of targets is cut
	// to: a target is in when the target, the member it is routed to or a
	// label as name=value holds the text, in any case.
	Filter string `json:"filter,omitempty"`
	// Limit is MemberViewLimit, the most entries of the list.
	Limit int `json:"limit"`
	// Spread is the job as the status's spread has it, but its members
	// are in the order of their targets, the most first.
	Spread *JobSpread `json:"spread"`
	// ShardKey is what a target is allocated by, target or address.
	ShardKey string `json:"shard_key,omitempty"`
	// JobWeight is the job's weight, a target's but for one weighed by
	// its WeightLabel, the job's weight_label.
	JobWeight   float64 `json:"job_weight,omitempty"`
	WeightLabel string  `json:"weight_label,omitempty"`
	// Labels are the job's labels, a label written "" left out, and
	// Params its params, shown as described above.
	Labels map[string]string `json:"labels"`
	Params map[string]string `json:"params"`
	// Sources are the job's discovery sources, as the status has them.
	Sources []SourceStatus `json:"sources"`
	Targets *JobTargetList `json:"targets"`
}

// JobTargetList is a probe job's targets: Total counts them all, Matching
// those the filter matches, and List holds the first Limit of those, by
// target.
type JobTargetList struct {
	Total    int         `json:"total"`
	Matching int         `json:"matching"`
	List     []JobTarget `json:"list"`
}

// JobTarget is one target of a probe job.
type JobTarget struct {
	// Target is the target as the exporter shows it, credentials withheld.
	Target string `json:"target"`
	// Member is the member it is routed to, "" while there is none.
	Member string `json:"member"`
	// Labels are those its discovery source gave it, but those the job
	// sets or takes away: the job's own are the view's Labels. Those whose
	// names start with __, its source labels, are left out of the page's
	// view unless asked for (JobView).
	Labels map[string]string `json:"labels,omitempty"`
	// Weight is the target's weight: its own from the job's weight_label,
	// or the job's.
	Weight float64 `json:"weight"`
}

// JobView is the view of the probe job named job, its targets cut to
// filter, with their source labels, those whose names start with __, when
// sourceLabels is set and else without them, the filter then not matching
// them either; known is false when the probe targets file has no such job,
// and the instance is not a follower leaving the members to its leader.
func (a *Allocator) JobView(job, filter string, sourceLabels bool) (view JobView, known bool) {
	filter = strings.TrimSpace(filter)
	now := a.clock.Now()
	view = JobView{GeneratedAt: now.UTC().Format(time.RFC3339), Job: job, Filter: filter, Limit: MemberViewLimit}
	a.mu.Lock()
	if p := a.peersStatusLocked(now); p != nil && p.LeaderOnly && p.Role != "leader" {
		a.mu.Unlock()
		view.Partial, view.Leader = true, p.Leader
		return view, true
	}
	j := a.probeJob(job)
	if j == nil {
		a.mu.Unlock()
		return view, false
	}
	spread, owners, _ := a.jobSpreadLocked(j)
	collector := model.CollectorByName(a.inputs.Config, j.Collector)
	if collector == nil {
		collector = &model.Collector{}
	}
	var targets []JobTarget
	for _, target := range j.Targets {
		t := JobTarget{Target: fetch.DisplayTarget(collector, target), Member: owners[target], Weight: float64(j.weightOfTarget(target)) / milli}
		for name, value := range j.labelsOf[target] {
			if _, set := j.Labels[name]; !set && (sourceLabels || !isSourceLabel(name)) {
				if t.Labels == nil {
					t.Labels = map[string]string{}
				}
				t.Labels[name] = value
			}
		}
		targets = append(targets, t)
	}
	view.JobWeight = float64(j.weightOfTarget("")) / milli
	view.WeightLabel = j.WeightLabel
	view.ShardKey = j.ShardKey
	if view.ShardKey == "" {
		view.ShardKey = ShardKeyTarget
	}
	view.Labels, view.Params = map[string]string{}, map[string]string{}
	for name, value := range j.Labels {
		if value != "" {
			view.Labels[name] = value
		}
	}
	for name, value := range j.Params {
		view.Params[name] = shownParam(name, value)
	}
	a.mu.Unlock()

	view.Sources = []SourceStatus{}
	for _, s := range a.sourcesStatus(now) {
		if s.Kind == "probe" && s.Owner == job {
			view.Sources = append(view.Sources, s)
			spread.Sources = append(spread.Sources, JobSource{Name: s.Name, Type: s.Type, Result: s.Result})
		}
	}
	sort.SliceStable(spread.Members, func(i, j int) bool { return spread.Members[i].Targets > spread.Members[j].Targets })
	view.Spread = &spread
	sort.Slice(targets, func(i, j int) bool { return targets[i].Target < targets[j].Target })
	match := matcher(filter)
	view.Targets = &JobTargetList{Total: len(targets), List: []JobTarget{}}
	for _, t := range targets {
		if match(t.matchTexts()...) {
			view.Targets.Matching++
			if len(view.Targets.List) < MemberViewLimit {
				view.Targets.List = append(view.Targets.List, t)
			}
		}
	}
	return view, true
}

// isSourceLabel is whether a discovered target's label is one of its
// source's, its name starting with __, as __meta_kubernetes_namespace.
func isSourceLabel(name string) bool { return strings.HasPrefix(name, "__") }

// shownParam is a probe job's param as its view shows it: withheld when
// its name reads as a credential's, and else with what reads as one in it
// masked.
func shownParam(name, value string) string {
	if fetch.CredentialName(name) {
		return fetch.Redacted
	}
	return fetch.RedactText(value)
}

// matchTexts are the texts of a probe job's target a filter is matched
// against: the target, the member it is routed to and each label as
// name=value, in name order.
func (t *JobTarget) matchTexts() []string {
	texts := []string{t.Target, t.Member}
	for _, name := range slices.Sorted(maps.Keys(t.Labels)) {
		texts = append(texts, name+"="+t.Labels[name])
	}
	return texts
}

// serveJobView answers StatusPath?job=<name> with the job's view as JSON,
// every label of a target kept: 400 without a name, 404 for a job the probe
// targets file does not have.
func (a *Allocator) serveJobView(w http.ResponseWriter, r *http.Request) {
	job := r.URL.Query().Get("job")
	if job == "" {
		http.Error(w, "the job parameter names a probe job; "+StatusPath+" without it serves the whole status", http.StatusBadRequest)
		return
	}
	view, known := a.JobView(job, r.URL.Query().Get("filter"), true)
	if !known {
		http.Error(w, "the probe targets file has no probe job "+strconv.Quote(job), http.StatusNotFound)
		return
	}
	writeStatusJSON(w, view)
}

// jobPage is what a probe job's view renders: the view, whether the
// probe targets file has the job, and its members' bars.
type jobPage struct {
	JobView
	Known bool
	// Bars are the members' bars, each as a share of the track, which
	// runs from 0 to the most targets of a member, and AveragePct where
	// the average falls on it.
	Bars       []jobBar
	AveragePct float64
}

type jobBar struct {
	JobMember
	Pct  float64
	Text string
}

func jobPageOf(view JobView, known bool) *jobPage {
	page := &jobPage{JobView: view, Known: known}
	if view.Spread == nil {
		return page
	}
	most := float64(view.Spread.Max)
	pct := func(n float64) float64 {
		if most == 0 {
			return 0
		}
		return float64(int(n/most*1000+0.5)) / 10
	}
	for _, m := range view.Spread.Members {
		page.Bars = append(page.Bars, jobBar{JobMember: m, Pct: pct(float64(m.Targets)),
			Text: m.Member + ": " + strconv.Itoa(m.Targets) + " targets of the job routed"})
	}
	page.AveragePct = pct(view.Spread.Average)
	return page
}

// jobContent is a probe job view's content, which a refresh swaps in.
const jobContent = `{{define "content"}}
<p class="meta" id="as-of">As of <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.</p>
{{- if .Partial}}
<p class="notice">This instance {{if .Leader}}follows the leader, <strong>{{.Leader}}</strong>{{else}}has not yet chosen its leader{{end}}, and runs with <code class="flag">--peers.leader-only</code>: the members and the probe jobs' targets are on the leader's page, this job's at <code>/?job=…</code> there.</p>
{{- else if not .Known}}
<p class="notice" id="unknown-job">The probe targets file has no probe job <strong>{{.Job}}</strong>: it was renamed or taken out, or the allocator has no probe targets file.</p>
{{- else}}
{{- $sp := .Spread}}

<h2>This probe job</h2>
<table class="facts">
<tr><th>Collector</th><td>{{$sp.Collector}}</td></tr>
<tr><th>Targets</th><td>{{$sp.Targets}}{{if .Sources}} <span class="muted">(those discovered included)</span>{{end}}</td></tr>
<tr><th>Weight</th><td>{{weight $sp.Weight}}{{if .WeightLabel}} <span class="muted">(each target's from its label <code>{{.WeightLabel}}</code>, else the job's {{weight .JobWeight}})</span>{{else}} <span class="muted">({{weight .JobWeight}} a target)</span>{{end}}</td></tr>
<tr><th>Shard key</th><td>{{.ShardKey}}</td></tr>
<tr><th>Labels</th><td>{{template "labels" .Labels}}</td></tr>
<tr><th>Params</th><td>{{template "labels" .Params}}</td></tr>
<tr><th>Discovery</th><td>{{range .Sources}}<span class="source"><strong>{{.Name}}</strong> <span class="muted">{{.Type}}</span> {{template "result" .Result}}, refreshed every {{interval .RefreshIntervalSeconds}}</span><br>{{if eq .Result "failing"}}<span class="muted">{{.Error}}</span><br>{{end}}{{else}}<span class="muted">none: the probe targets file lists every target</span>{{end}}</td></tr>
</table>

<h2>Members <span class="count">{{len $sp.Members}}</span></h2>
{{- if $sp.Members}}
<p>The job's probes are routed among {{len $sp.Members}} member{{if ne (len $sp.Members) 1}}s{{end}}: {{$sp.Min}} to {{$sp.Max}} targets each, {{average $sp.Average}} on average, the most {{if $sp.Uneven}}<span class="bad" title="the most is further above the average than hashing puts a member's targets by chance">{{imbalance $sp.Imbalance}}</span>{{else}}{{imbalance $sp.Imbalance}}{{end}} the average.</p>
<div class="scroll">
<table class="spread job-members">
<thead><tr><th>Member</th><th class="num">Targets</th><th class="num">Weight</th><th>Share</th></tr></thead>
<tbody>
{{- $avg := .AveragePct}}
{{- range .Bars}}
<tr>
<td class="id"><a href="?member={{.Member}}" title="the targets of {{.Member}}"><strong>{{.Member}}</strong></a></td>
<td class="num">{{.Targets}}</td>
<td class="num">{{weight .Weight}}</td>
<td class="bar"><div class="bar-track" role="img" aria-label="{{.Text}}">{{if .Targets}}<span class="seg-probe" style="width: {{.Pct}}%" title="{{.Text}}"></span>{{end}}<i class="avg" style="left: {{$avg}}%" title="the average per member"></i></div></td>
</tr>
{{- end}}
</tbody>
</table>
</div>
<p class="legend"><span><b class="seg-probe"></b>probe targets of the job, routed</span><span><b class="avg"></b>average per member</span><span>Bars run to the most of a member.</span></p>
{{- else}}
<p class="muted">No member to route the job's probes to yet: the member list has not formed, or no member is live.</p>
{{- end}}

<h2>Targets <span class="count">{{.Targets.Total}}</span></h2>
{{- with .Targets}}{{template "list-note" (listNote .Total .Matching (len .List) $.Filter "job-targets")}}{{end}}
{{- if .Targets.List}}
<div class="scroll">
<table class="targets" id="job-targets">
<thead><tr><th>Target</th><th>Member</th><th>Labels</th><th class="num">Weight</th></tr></thead>
<tbody>
{{- range .Targets.List}}
<tr data-match="{{match .}}"><td class="target id">{{template "target" .Target}}</td><td class="moved" data-label="Member"><span class="v">{{if .Member}}<a href="?member={{.Member}}">{{.Member}}</a>{{else}}<span class="muted">none yet</span>{{end}}</span></td><td class="labels" data-label="Labels"><span class="v">{{template "labels" .Labels}}</span></td><td class="num" data-label="Weight"><span class="v">{{weight .Weight}}</span></td></tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}
{{- end}}
{{end}}`
