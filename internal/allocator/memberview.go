package allocator

import (
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A member's view, at /?member=<id> and as JSON at StatusPath?member=<id>,
// is what one member has: the static targets it owns, those it keeps
// draining and the member each moved to, and the probe targets service
// discovery routes to it, by job. It is worked out as the status's spread
// is, so its counts are the spread's for the member. A target is shown as
// the exporter shows it (fetch.DisplayTarget), credentials withheld, since
// a static target may carry them in its URL; its params and request block,
// which may hold them too, are not shown at all.

// MemberViewLimit bounds each list of a member's view: a member of a large
// share can own tens of thousands of targets, more than a page that
// refreshes itself can draw or a person can read. Each list says how many
// it holds and how many match the filter, so what is left out is counted;
// the filter, applied before the bound, finds any of them.
const MemberViewLimit = 1000

// MemberView is the answer at StatusPath?member=<id>.
type MemberView struct {
	// GeneratedAt is when the view was taken, RFC 3339 in UTC.
	GeneratedAt string `json:"generated_at"`
	Member      string `json:"member"`
	// Partial says the copy follows under --peers.leader-only and leaves
	// the members to the leader, at Leader; Status, Static, Draining and
	// Probes are nil.
	Partial bool   `json:"partial"`
	Leader  string `json:"leader,omitempty"`
	// Filter is the filter asked for, which every list is cut to: a
	// target is in when its name, target, collector, a label as
	// name=value, its job or the member it moved to holds the text, in
	// any case.
	Filter string `json:"filter,omitempty"`
	// Limit is MemberViewLimit, the most entries of each list.
	Limit int `json:"limit"`
	// Status is the member as the status's members have it.
	Status   *MemberStatus     `json:"status"`
	Static   *MemberStaticList `json:"static"`
	Draining *MemberStaticList `json:"draining"`
	Probes   *MemberProbeList  `json:"probes"`
}

// MemberStaticList is the static targets a member owns, by name, or keeps
// draining. Total counts them all, Matching those the filter matches, and
// List holds the first Limit of those.
type MemberStaticList struct {
	Total    int                  `json:"total"`
	Matching int                  `json:"matching"`
	List     []MemberStaticTarget `json:"list"`
}

// MemberStaticTarget is one static target of a member.
type MemberStaticTarget struct {
	Name      string `json:"name"`
	Collector string `json:"collector"`
	// Target is the target as the exporter shows it, credentials
	// withheld; "" for a collector that takes none.
	Target string `json:"target"`
	// Labels are the target's own labels, a label written "" left out, as
	// the exporter leaves it out.
	Labels   map[string]string `json:"labels,omitempty"`
	Interval string            `json:"interval"`
	// MovedTo is, for a target draining, the member that owns it now, and
	// DrainsForSeconds how much longer the member keeps it.
	MovedTo          string  `json:"moved_to,omitempty"`
	DrainsForSeconds float64 `json:"drains_for_seconds,omitempty"`
}

// MemberProbeList is the probe targets routed to a member: Jobs counts
// them by probe job, in file order, every job listed; Total, Matching and
// List are as a MemberStaticList's, by job and target.
type MemberProbeList struct {
	Jobs     []MemberProbeJob    `json:"jobs"`
	Total    int                 `json:"total"`
	Matching int                 `json:"matching"`
	List     []MemberProbeTarget `json:"list"`
}

// MemberProbeJob is one probe job's count.
type MemberProbeJob struct {
	Job       string `json:"job"`
	Collector string `json:"collector"`
	Targets   int    `json:"targets"`
}

// MemberProbeTarget is one probe target routed to a member, shown as the
// exporter shows it.
type MemberProbeTarget struct {
	Job       string `json:"job"`
	Collector string `json:"collector"`
	Target    string `json:"target"`
}

// MemberView is member's view, cut to filter; known is false when the
// allocator knows no such member, and it is not a follower leaving the
// members to its leader.
func (a *Allocator) MemberView(member, filter string) (view MemberView, known bool) {
	// A filter of spaces alone is no filter: it matches every target, and
	// the view does not say it cut the lists to it.
	filter = strings.TrimSpace(filter)
	now := a.clock.Now()
	view = MemberView{GeneratedAt: now.UTC().Format(time.RFC3339), Member: member, Filter: filter, Limit: MemberViewLimit}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := a.peersStatusLocked(now); p != nil && p.LeaderOnly && p.Role != "leader" {
		view.Partial, view.Leader = true, p.Leader
		return view, true
	}
	for _, m := range a.membersStatusLocked(now).List {
		if m.ID == member {
			view.Status = &m
			break
		}
	}
	if view.Status == nil {
		return view, false
	}
	match := matcher(filter)
	view.Static = &MemberStaticList{List: []MemberStaticTarget{}}
	view.Draining = &MemberStaticList{List: []MemberStaticTarget{}}
	view.Probes = &MemberProbeList{Jobs: []MemberProbeJob{}, List: []MemberProbeTarget{}}
	in := a.inputs
	if in == nil {
		return view, true
	}
	collectors := map[string]*model.Collector{}
	for i := range in.Config.Collectors {
		collectors[in.Config.Collectors[i].Name] = &in.Config.Collectors[i]
	}
	shown := func(collector, target string) string {
		if target == "" {
			return ""
		}
		c := collectors[collector]
		if c == nil {
			c = &model.Collector{}
		}
		return fetch.DisplayTarget(c, target)
	}
	static := func(t *model.StaticTarget) MemberStaticTarget {
		st := MemberStaticTarget{Name: t.Name, Collector: t.Collector, Target: shown(t.Collector, t.Target), Interval: time.Duration(t.Interval).String()}
		for name, value := range t.Labels {
			if value != "" {
				if st.Labels == nil {
					st.Labels = map[string]string{}
				}
				st.Labels[name] = value
			}
		}
		return st
	}
	if in.Targets != nil && len(a.effective) > 0 {
		var owned []MemberStaticTarget
		for i := range in.Targets.Targets {
			if owner, _ := allocapi.Owner(a.effective, in.shardKeys[i]); owner == member {
				owned = append(owned, static(&in.Targets.Targets[i]))
			}
		}
		sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
		view.Static.add(owned, match)
		var draining []MemberStaticTarget
		for key, until := range a.draining {
			i, ok := in.targetIndex[key.target]
			if key.member != member || !ok {
				continue
			}
			st := static(&in.Targets.Targets[i])
			st.MovedTo = a.ownerOf(key.target)
			st.DrainsForSeconds = ageSeconds(until.Sub(now).Seconds())
			draining = append(draining, st)
		}
		sort.Slice(draining, func(i, j int) bool { return draining[i].Name < draining[j].Name })
		view.Draining.add(draining, match)
	}
	if in.Probes != nil {
		var routed []MemberProbeTarget
		for i := range in.Probes.Jobs {
			j := &in.Probes.Jobs[i]
			job := MemberProbeJob{Job: j.Name, Collector: j.Collector}
			pool := a.routing(j)
			var targets []MemberProbeTarget
			for _, target := range j.Targets {
				if owner, err := allocapi.Owner(pool, shardKeyOf(j, target)); err == nil && owner == member {
					targets = append(targets, MemberProbeTarget{Job: j.Name, Collector: j.Collector, Target: shown(j.Collector, target)})
				}
			}
			sort.Slice(targets, func(i, j int) bool { return targets[i].Target < targets[j].Target })
			job.Targets = len(targets)
			view.Probes.Jobs = append(view.Probes.Jobs, job)
			routed = append(routed, targets...)
		}
		view.Probes.Total = len(routed)
		for _, t := range routed {
			if match(t.matchTexts()...) {
				view.Probes.Matching++
				if len(view.Probes.List) < MemberViewLimit {
					view.Probes.List = append(view.Probes.List, t)
				}
			}
		}
	}
	return view, true
}

// add puts targets in l, counted, cut to those match matches and to
// MemberViewLimit of them.
func (l *MemberStaticList) add(targets []MemberStaticTarget, match func(...string) bool) {
	l.Total = len(targets)
	for _, t := range targets {
		if match(t.matchTexts()...) {
			l.Matching++
			if len(l.List) < MemberViewLimit {
				l.List = append(l.List, t)
			}
		}
	}
}

// matchTexts are the texts of a static target a filter is matched against:
// its name, target, collector, the member it moved to and each label as
// name=value, in name order.
func (t *MemberStaticTarget) matchTexts() []string {
	texts := []string{t.Name, t.Target, t.Collector, t.MovedTo}
	for _, name := range slices.Sorted(maps.Keys(t.Labels)) {
		texts = append(texts, name+"="+t.Labels[name])
	}
	return texts
}

// matchTexts are the texts of a probe target a filter is matched against:
// its target, job and collector.
func (t *MemberProbeTarget) matchTexts() []string { return []string{t.Target, t.Job, t.Collector} }

// matchText is a row's data-match, which the filter box matches as it is
// typed: the texts the allocator's filter matches of the row's target,
// joined by line feeds. A search input drops line breaks from what is
// typed, so the box matches no text running across two of them, and it
// matches what the allocator matches and nothing else, an interval or
// a drain time not among them.
func matchText(target any) string {
	switch t := target.(type) {
	case MemberStaticTarget:
		return strings.Join(t.matchTexts(), "\n")
	case MemberProbeTarget:
		return strings.Join(t.matchTexts(), "\n")
	}
	return ""
}

// matcher is whether any of a target's texts holds filter, in any case;
// every target matches an empty filter.
func matcher(filter string) func(texts ...string) bool {
	needle := strings.ToLower(strings.TrimSpace(filter))
	return func(texts ...string) bool {
		return needle == "" || slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(strings.ToLower(s), needle) })
	}
}

// serveMemberView answers StatusPath?member=<id> with the member's view
// as JSON: 400 without an id, 404 for a member the allocator does not
// know.
func (a *Allocator) serveMemberView(w http.ResponseWriter, r *http.Request) {
	member := r.URL.Query().Get("member")
	if member == "" {
		http.Error(w, "the member parameter names a member; "+StatusPath+" without it serves the whole status", http.StatusBadRequest)
		return
	}
	view, known := a.MemberView(member, r.URL.Query().Get("filter"))
	if !known {
		http.Error(w, "the allocator knows no member "+strconv.Quote(member), http.StatusNotFound)
		return
	}
	writeStatusJSON(w, view)
}

// memberPage is what a member's view renders: the view, and whether the
// allocator knows the member.
type memberPage struct {
	MemberView
	Known bool
}

// memberStyle is the member view's own style, after statusStyle: its way
// back and its filter, and the target tables, which on a phone stack each
// row's cells under one another, each named, rather than scroll sideways,
// so a long target wraps in the width there is. A named cell is then two
// columns, its name and its value (span.v), so the value starts on the
// name's line however long its first unbreakable run, and its lines wrap
// in its own column; a hanging indent would move a run longer than the
// line's rest to the line below.
const memberStyle = `
p.back { margin: 0 0 4px; font-size: 14px; }
.member-id { overflow-wrap: anywhere; }
form.filter { display: flex; flex-direction: column; gap: 4px; margin: 16px 0 0; max-width: 640px; }
form.filter label { font-size: 14px; font-weight: 600; }
form.filter .filter-row { display: flex; gap: 8px; }
form.filter input[type="search"] { flex: 1; min-width: 0; height: 34px; padding: 0 10px; font: inherit; color: var(--fg); background: var(--bg); border: 1px solid var(--line); border-radius: 6px; }
form.filter button { flex: none; height: 34px; padding: 0 12px; font: inherit; color: var(--fg); background: var(--panel); border: 1px solid var(--line); border-radius: 6px; }
form.filter .muted { font-size: 13px; }
form.filter[hidden] { display: none; }
h2 .count { color: var(--muted); font-weight: 400; font-variant-numeric: tabular-nums; }
p.note { margin: 0 0 8px; font-size: 14px; }
p.note:empty { display: none; }
td.target { overflow-wrap: anywhere; min-width: 14em; }
/* The target takes no more than its share, so a name keeps its line and
   the labels theirs, and the target wraps. */
@media (min-width: 641px) { table#owned td.target, table#draining td.target { width: 40%; } }
table.targets td.id { min-width: 13em; }
td.labels { min-width: 10em; }
td.moved { overflow-wrap: anywhere; min-width: 9em; }
span.label { display: inline-block; margin: 0 4px 2px 0; padding: 0 6px; border: 1px solid var(--line); border-radius: 4px; font-size: 12px; line-height: 18px; overflow-wrap: anywhere; text-indent: 0; }
table.jobs { width: auto; min-width: 50%; margin-bottom: 12px; }
@media (max-width: 640px) {
  table.targets, table.targets tbody, table.targets tr, table.targets td { display: block; width: 100%; }
  table.targets[hidden], table.targets tr[hidden] { display: none; }
  table.targets thead { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
  table.targets tr { padding: 8px 0; border-bottom: 1px solid var(--line); }
  table.targets td { border: 0; padding: 1px 0; min-width: 0; text-align: left; white-space: normal; }
  table.targets td[data-label] { display: grid; grid-template-columns: 92px minmax(0, 1fr); align-items: baseline; }
  table.targets td[data-label]::before { content: attr(data-label); color: var(--muted); font-size: 13px; }
  table.targets td > .v { overflow-wrap: anywhere; }
  table.jobs { width: 100%; }
}
`

// memberContent is a member view's content, which a refresh swaps in.
const memberContent = `{{define "content"}}
<p class="meta" id="as-of">As of <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.</p>
{{- if .Partial}}
<p class="notice">This copy {{if .Leader}}follows the leader, <strong>{{.Leader}}</strong>{{else}}has not yet chosen its leader{{end}}, and runs with <code class="flag">--peers.leader-only</code>: the members and their targets are on the leader's page, this member's at <code>/?member=…</code> there.</p>
{{- else if not .Known}}
<p class="notice" id="unknown-member">The allocator knows no member <strong>{{.Member}}</strong>: no replica of that id has heartbeated, or it stopped long enough ago to be forgotten.</p>
{{- else}}
{{- with .Status}}

<h2>This member</h2>
<table class="facts">
<tr><th>Address</th><td>{{.Address}}</td></tr>
<tr><th>State</th><td>{{template "state" .}}</td></tr>
<tr><th>Last heartbeat</th><td>{{template "heartbeat" .}}</td></tr>
<tr><th>Bundle in force</th><td>{{template "bundle" .}}</td></tr>
</table>
{{- end}}
<p>{{.Static.Total}} static target{{if ne .Static.Total 1}}s{{end}} owned, {{.Draining.Total}} draining and {{.Probes.Total}} probe target{{if ne .Probes.Total 1}}s{{end}} routed to this member.{{if .Filter}} Only those holding “{{.Filter}}” are listed.{{end}}</p>

<h2>Static targets owned <span class="count">{{.Static.Total}}</span></h2>
{{- with .Static}}{{template "list-note" (listNote .Total .Matching (len .List) $.Filter "owned")}}{{end}}
{{- if .Static.List}}
<div class="scroll">
<table class="targets" id="owned">
<thead><tr><th>Name</th><th>Target</th><th>Collector</th><th>Labels</th><th class="num">Interval</th></tr></thead>
<tbody>
{{- range .Static.List}}
<tr data-match="{{match .}}"><td class="id"><strong>{{.Name}}</strong></td><td class="target" data-label="Target"><span class="v">{{template "target" .Target}}</span></td><td data-label="Collector"><span class="v">{{.Collector}}</span></td><td class="labels" data-label="Labels"><span class="v">{{template "labels" .Labels}}</span></td><td class="num" data-label="Interval"><span class="v">{{.Interval}}</span></td></tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}

<h2>Static targets draining <span class="count">{{.Draining.Total}}</span></h2>
<p class="muted">Targets that moved to another member, which this one keeps scraping for one of their intervals, so their series have no gap.</p>
{{- with .Draining}}{{template "list-note" (listNote .Total .Matching (len .List) $.Filter "draining")}}{{end}}
{{- if .Draining.List}}
<div class="scroll">
<table class="targets" id="draining">
<thead><tr><th>Name</th><th>Target</th><th>Collector</th><th>Moved to</th><th class="num">Drains for</th></tr></thead>
<tbody>
{{- range .Draining.List}}
<tr data-match="{{match .}}"><td class="id"><strong>{{.Name}}</strong></td><td class="target" data-label="Target"><span class="v">{{template "target" .Target}}</span></td><td data-label="Collector"><span class="v">{{.Collector}}</span></td><td class="moved" data-label="Moved to"><span class="v">{{if .MovedTo}}<a href="?member={{.MovedTo}}">{{.MovedTo}}</a>{{else}}<span class="muted">–</span>{{end}}</span></td><td class="num" data-label="Drains for"><span class="v">{{age .DrainsForSeconds}}</span></td></tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}

<h2>Probe targets routed <span class="count">{{.Probes.Total}}</span></h2>
{{- if .Probes.Jobs}}
<table class="jobs">
<thead><tr><th>Probe job</th><th>Collector</th><th class="num">Targets</th></tr></thead>
<tbody>
{{- range .Probes.Jobs}}
<tr><td class="id">{{.Job}}</td><td>{{.Collector}}</td><td class="num">{{.Targets}}</td></tr>
{{- end}}
</tbody>
</table>
{{- end}}
{{- with .Probes}}{{template "list-note" (listNote .Total .Matching (len .List) $.Filter "probes")}}{{end}}
{{- if .Probes.List}}
<div class="scroll">
<table class="targets" id="probes">
<thead><tr><th>Target</th><th>Probe job</th><th>Collector</th></tr></thead>
<tbody>
{{- range .Probes.List}}
<tr data-match="{{match .}}"><td class="target id">{{template "target" .Target}}</td><td data-label="Probe job"><span class="v">{{.Job}}</span></td><td data-label="Collector"><span class="v">{{.Collector}}</span></td></tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}
{{- end}}
{{end}}
{{- define "target"}}{{if .}}{{.}}{{else}}<span class="muted">–</span>{{end}}{{end}}
{{- define "labels"}}{{range $name, $value := .}}<span class="label">{{$name}}={{$value}}</span> {{else}}<span class="muted">–</span>{{end}}{{end}}
{{- define "list-note"}}
{{- if eq .Total 0}}
<p class="muted">None.</p>
{{- else if eq .Matching 0}}
<p class="muted">None of the {{.Total}} holds “{{.Filter}}”.</p>
{{- else}}
{{- if lt .Matching .Total}}
<p class="note">{{.Matching}} of the {{.Total}} hold “{{.Filter}}”.</p>
{{- end}}
{{- if lt .Shown .Matching}}
<p class="note"><span class="bad">Only the first {{.Shown}} are listed</span>; {{.Hidden}} more are left out. Filter to narrow the list.</p>
{{- end}}
<p class="note muted" data-rows-of="{{.Table}}"></p>
<p class="muted" data-none-of="{{.Table}}" hidden>No row below holds what is typed in the filter; Search all looks through every target.</p>
{{- end}}
{{- end}}
`

// listNoteData is what a list's note says: how many targets it has, how
// many the filter matches, how many it shows, and its table's id.
type listNoteData struct {
	Total, Matching, Shown, Hidden int
	Filter, Table                  string
}

func listNote(total, matching, shown int, filter, table string) listNoteData {
	return listNoteData{Total: total, Matching: matching, Shown: shown, Hidden: matching - shown, Filter: filter, Table: table}
}
