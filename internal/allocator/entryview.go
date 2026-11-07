package allocator

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A discovered_targets entry's view, at /?entry=<name> and as JSON at
// StatusPath?entry=<name>, is what a probe job's view is for a job
// (jobview.go), for the static targets an entry of the static target file
// makes of what its discovery sources find: its sources, how many of its
// targets each member owns, and each target with the member that owns it.
// Its counts are the status's static spread's for the entry, its list is
// bounded by MemberViewLimit and cut to a filter, and a target is shown as
// a member's view shows a static target: its target as the exporter shows
// it, credentials withheld, its params and request block not at all.

// EntryView is the answer at StatusPath?entry=<name>.
type EntryView struct {
	// GeneratedAt is when the view was taken, RFC 3339 in UTC.
	GeneratedAt string `json:"generated_at"`
	Entry       string `json:"entry"`
	// Partial says the instance follows under --peers.leader-only and leaves
	// the members to the leader, at Leader; Spread, Labels, Sources and
	// Targets are nil.
	Partial bool   `json:"partial"`
	Leader  string `json:"leader,omitempty"`
	// Filter is the filter asked for, which the list of targets is cut
	// to: a target is in when its name, target, collector, a label as
	// name=value or the member that owns it holds the text, in any case.
	Filter string `json:"filter,omitempty"`
	// Limit is MemberViewLimit, the most entries of the list.
	Limit int `json:"limit"`
	// Spread is the entry as the status's static spread has it, but its
	// members are in the order of their targets, the most first.
	Spread *StaticSpread `json:"spread"`
	// ShardKey is what a target is allocated by, target or address.
	ShardKey string `json:"shard_key,omitempty"`
	// Labels are the entry's labels, which win over a discovered target's,
	// a label written "" left out.
	Labels map[string]string `json:"labels"`
	// Sources are the entry's discovery sources, as the status has them.
	Sources []SourceStatus   `json:"sources"`
	Targets *EntryTargetList `json:"targets"`
}

// EntryTargetList is an entry's targets: Total counts them all, Matching
// those the filter matches, and List holds the first Limit of those, by
// name.
type EntryTargetList struct {
	Total    int           `json:"total"`
	Matching int           `json:"matching"`
	List     []EntryTarget `json:"list"`
}

// EntryTarget is one static target of an entry: as a member's view shows
// it, and the member that owns it, "" while there is none.
type EntryTarget struct {
	MemberStaticTarget
	Member string `json:"member"`
}

// matchTexts are the texts of an entry's target a filter is matched
// against: a static target's, and the member that owns it.
func (t *EntryTarget) matchTexts() []string {
	return append(t.MemberStaticTarget.matchTexts(), t.Member)
}

// EntryView is the view of the discovered_targets entry named entry, its
// targets cut to filter; known is false when the static target file has
// no such entry, and the instance is not a follower leaving the members to
// its leader.
func (a *Allocator) EntryView(entry, filter string) (view EntryView, known bool) {
	filter = strings.TrimSpace(filter)
	now := a.clock.Now()
	view = EntryView{GeneratedAt: now.UTC().Format(time.RFC3339), Entry: entry, Filter: filter, Limit: MemberViewLimit}
	a.mu.Lock()
	if p := a.peersStatusLocked(now); p != nil && p.LeaderOnly && p.Role != "leader" {
		a.mu.Unlock()
		view.Partial, view.Leader = true, p.Leader
		return view, true
	}
	in := a.inputs
	var e *discoveredEntry
	if in != nil {
		for _, candidate := range in.entries {
			if candidate.name == entry {
				e = candidate
			}
		}
	}
	if e == nil {
		a.mu.Unlock()
		return view, false
	}
	spread := StaticSpread{Entry: entry, Members: []JobMember{}, Sources: []JobSource{}}
	for _, s := range a.staticSpreadLocked() {
		if s.Entry == entry {
			spread = s
		}
	}
	var targets []EntryTarget
	if in.Targets != nil {
		plan := a.currentPlan()
		for i := range in.Targets.Targets {
			if i >= len(in.origins) || in.origins[i] != entry {
				continue
			}
			t := &in.Targets.Targets[i]
			et := EntryTarget{MemberStaticTarget: MemberStaticTarget{Name: t.Name, Collector: t.Collector, Interval: time.Duration(t.Interval).String(),
				Weight: float64(in.staticWeight(i)) / milli}, Member: plan.owner[in.shardKeys[i]]}
			if t.Target != "" {
				c := model.CollectorByName(in.Config, t.Collector)
				if c == nil {
					c = &model.Collector{}
				}
				et.Target = fetch.DisplayTarget(c, t.Target)
			}
			for name, value := range t.Labels {
				if value != "" {
					if et.Labels == nil {
						et.Labels = map[string]string{}
					}
					et.Labels[name] = value
				}
			}
			targets = append(targets, et)
		}
	}
	view.ShardKey = ShardKeyTarget
	if e.byAddress {
		view.ShardKey = ShardKeyAddress
	}
	view.Labels = map[string]string{}
	for name, value := range e.labels {
		if value != "" {
			view.Labels[name] = value
		}
	}
	a.mu.Unlock()

	view.Sources = []SourceStatus{}
	spread.Sources = []JobSource{}
	for _, s := range a.sourcesStatus(now) {
		if s.Kind == "static" && s.Owner == entry {
			view.Sources = append(view.Sources, s)
			spread.Sources = append(spread.Sources, JobSource{Name: s.Name, Type: s.Type, Result: s.Result})
		}
	}
	sort.SliceStable(spread.Members, func(i, j int) bool { return spread.Members[i].Targets > spread.Members[j].Targets })
	view.Spread = &spread
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	match := matcher(filter)
	view.Targets = &EntryTargetList{Total: len(targets), List: []EntryTarget{}}
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

// serveEntryView answers StatusPath?entry=<name> with the entry's view as
// JSON: 400 without a name, 404 for an entry the static target file does
// not have.
func (a *Allocator) serveEntryView(w http.ResponseWriter, r *http.Request) {
	entry := r.URL.Query().Get("entry")
	if entry == "" {
		http.Error(w, "the entry parameter names a discovered_targets entry; "+StatusPath+" without it serves the whole status", http.StatusBadRequest)
		return
	}
	view, known := a.EntryView(entry, r.URL.Query().Get("filter"))
	if !known {
		http.Error(w, "the static target file has no discovered_targets entry "+strconv.Quote(entry), http.StatusNotFound)
		return
	}
	writeStatusJSON(w, view)
}

// entryPage is what an entry's view renders: the view, whether the static
// target file has the entry, and its members' bars.
type entryPage struct {
	EntryView
	Known      bool
	Bars       []jobBar
	AveragePct float64
}

func entryPageOf(view EntryView, known bool) *entryPage {
	page := &entryPage{EntryView: view, Known: known}
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
			Text: m.Member + ": " + strconv.Itoa(m.Targets) + " static targets of the entry owned"})
	}
	page.AveragePct = pct(view.Spread.Average)
	return page
}

// entryContent is an entry view's content, which a refresh swaps in.
const entryContent = `{{define "content"}}
<p class="meta" id="as-of">As of <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.</p>
{{- if .Partial}}
<p class="notice">This instance {{if .Leader}}follows the leader, <strong>{{.Leader}}</strong>{{else}}has not yet chosen its leader{{end}}, and runs with <code class="flag">--peers.leader-only</code>: the members and the static targets are on the leader's page, this entry's at <code>/?entry=…</code> there.</p>
{{- else if not .Known}}
<p class="notice" id="unknown-entry">The static target file has no <code>discovered_targets</code> entry <strong>{{.Entry}}</strong>: it was renamed or taken out, or the allocator has no static target file.</p>
{{- else}}
{{- $sp := .Spread}}

<h2>This entry</h2>
<table class="facts">
<tr><th>Targets</th><td>{{$sp.Targets}} <span class="muted">(what its sources find, made static targets)</span></td></tr>
<tr><th>Weight</th><td>{{weight $sp.Weight}}</td></tr>
<tr><th>Shard key</th><td>{{.ShardKey}}</td></tr>
<tr><th>Labels</th><td>{{template "labels" .Labels}}</td></tr>
<tr><th>Discovery</th><td>{{range .Sources}}<span class="source"><strong>{{.Name}}</strong> <span class="muted">{{.Type}}</span> {{template "result" .Result}}, refreshed every {{interval .RefreshIntervalSeconds}}</span><br>{{if eq .Result "failing"}}<span class="muted">{{.Error}}</span><br>{{end}}{{else}}<span class="muted">none</span>{{end}}</td></tr>
</table>

<h2>Members <span class="count">{{len $sp.Members}}</span></h2>
{{- if $sp.Members}}
<p>The entry's targets are spread over {{len $sp.Members}} member{{if ne (len $sp.Members) 1}}s{{end}}: {{$sp.Min}} to {{$sp.Max}} owned by each, {{average $sp.Average}} on average, the most {{if $sp.Uneven}}<span class="bad" title="the most is further above the average than hashing puts a member's targets by chance">{{imbalance $sp.Imbalance}}</span>{{else}}{{imbalance $sp.Imbalance}}{{end}} the average.</p>
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
<td class="bar"><div class="bar-track" role="img" aria-label="{{.Text}}">{{if .Targets}}<span class="seg-static" style="width: {{.Pct}}%" title="{{.Text}}"></span>{{end}}<i class="avg" style="left: {{$avg}}%" title="the average per member"></i></div></td>
</tr>
{{- end}}
</tbody>
</table>
</div>
<p class="legend"><span><b class="seg-static"></b>static targets of the entry, owned</span><span><b class="avg"></b>average per member</span><span>Bars run to the most of a member.</span></p>
{{- else}}
<p class="muted">No member owns static targets yet: the member list has not formed.</p>
{{- end}}

<h2><b class="swatch seg-static"></b>Static targets <span class="count">{{.Targets.Total}}</span></h2>
{{- with .Targets}}{{template "list-note" (listNote .Total .Matching (len .List) $.Filter "entry-targets")}}{{end}}
{{- if .Targets.List}}
<div class="scroll">
<table class="targets" id="entry-targets">
<thead><tr><th>Name</th><th>Target</th><th>Collector</th><th>Member</th><th>Labels</th><th class="num">Weight</th></tr></thead>
<tbody>
{{- range .Targets.List}}
<tr data-match="{{match .}}"><td class="id"><strong>{{.Name}}</strong></td><td class="target" data-label="Target"><span class="v">{{template "target" .Target}}</span></td><td data-label="Collector"><span class="v">{{.Collector}}</span></td><td class="moved" data-label="Member"><span class="v">{{if .Member}}<a href="?member={{.Member}}">{{.Member}}</a>{{else}}<span class="muted">none yet</span>{{end}}</span></td><td class="labels" data-label="Labels"><span class="v">{{template "labels" .Labels}}</span></td><td class="num" data-label="Weight"><span class="v">{{weight .Weight}}</span></td></tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}
{{- end}}
{{end}}`
