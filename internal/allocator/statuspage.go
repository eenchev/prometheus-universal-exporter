package allocator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
)

// The status page, at /, renders Status for a person, framed and styled as
// the exporter's pages are (pagekit): the same bar with its theme picker,
// the same icon, and the same refresh controls, which fetch the page's
// content again, ?format=fragment, and swap it in. Every value is escaped by
// html/template, and the script writes text with textContent alone. With
// ?member=<id> it is that member's view instead (memberview.go), and with
// ?job=<name> that probe job's (jobview.go), in the same frame.

// StatusPath is where the status is served as JSON.
const StatusPath = "/v1/status"

// statusDocs is where the page points for documentation.
const statusDocs = "https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/ALLOCATOR.md"

func (a *Allocator) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("member") {
		a.serveMemberView(w, r)
		return
	}
	if r.URL.Query().Has("job") {
		a.serveJobView(w, r)
		return
	}
	if r.URL.Query().Has("entry") {
		a.serveEntryView(w, r)
		return
	}
	writeStatusJSON(w, a.Status())
}

// writeStatusJSON answers v as JSON, which no cache keeps.
func writeStatusJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(append(body, '\n'))
}

// serveStatusPage answers the status page, or with ?member=<id> the
// member's view and with ?job=<name> the probe job's; with ?format=fragment its content alone, which the page's
// refresh swaps in.
func (a *Allocator) serveStatusPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	format := query.Get("format")
	if format != "" && format != "html" && format != "fragment" {
		http.Error(w, "the format parameter is html or fragment; "+StatusPath+" serves the status as JSON", http.StatusBadRequest)
		return
	}
	s := a.Status()
	frame := pageFrame{Version: s.Instance.Version, Revision: s.Instance.Revision, Docs: statusDocs, JSON: StatusPath[1:]}
	page, content := statusPageTemplate, statusContentTemplate
	switch {
	case query.Has("member"):
		member := query.Get("member")
		if member == "" {
			http.Error(w, "the member parameter names a member; / without it is the whole status", http.StatusBadRequest)
			return
		}
		// A member the allocator does not know is a page that says so,
		// answered 200 as its fragment is: a view left open, refreshing
		// itself, of a member that has gone then shows that it has, where
		// a 404 would be a failed refresh over the gone member's targets.
		// The JSON answers 404.
		view, known := a.MemberView(member, query.Get("filter"))
		frame.Member = &memberPage{MemberView: view, Known: known}
		frame.Content = frame.Member
		frame.JSON += "?" + url.Values{"member": {member}}.Encode()
		if !view.Partial {
			frame.Filter = &filterForm{Param: "member", Value: member, Text: view.Filter, Hidden: !known, Limit: view.Limit}
		}
		page, content = memberPageTemplate, memberContentTemplate
	case query.Has("job"):
		job := query.Get("job")
		if job == "" {
			http.Error(w, "the job parameter names a probe job; / without it is the whole status", http.StatusBadRequest)
			return
		}
		// A job the probe targets file does not have is a page that says
		// so, answered 200, as a gone member's is: a view left open over a
		// reload that renamed the job then shows it. The JSON answers 404.
		// ?meta=1 shows the targets' source labels, which the page leaves
		// out otherwise (jobview.go); the URL keeps it, so a refresh, which
		// fetches the URL's fragment, keeps it too.
		sourceLabels := query.Get("meta") == "1"
		view, known := a.JobView(job, query.Get("filter"), sourceLabels)
		frame.Job = jobPageOf(view, known)
		frame.Content = frame.Job
		frame.JSON += "?" + url.Values{"job": {job}}.Encode()
		if !view.Partial {
			// Only a discovered target has source labels: a job whose
			// file lists every target has no box for them.
			frame.Filter = &filterForm{Param: "job", Value: job, Text: view.Filter, Hidden: !known, Limit: view.Limit,
				SourceLabelsBox: len(view.Sources) > 0, SourceLabels: sourceLabels}
		}
		page, content = jobPageTemplate, jobContentTemplate
	case query.Has("entry"):
		entry := query.Get("entry")
		if entry == "" {
			http.Error(w, "the entry parameter names a discovered_targets entry; / without it is the whole status", http.StatusBadRequest)
			return
		}
		// An entry the static target file does not have is a page that
		// says so, answered 200, as a gone job's is. The JSON answers 404.
		view, known := a.EntryView(entry, query.Get("filter"))
		frame.Entry = entryPageOf(view, known)
		frame.Content = frame.Entry
		frame.JSON += "?" + url.Values{"entry": {entry}}.Encode()
		if !view.Partial {
			frame.Filter = &filterForm{Param: "entry", Value: entry, Text: view.Filter, Hidden: !known, Limit: view.Limit}
		}
		page, content = entryPageTemplate, entryContentTemplate
	default:
		frame.Content = statusPageOf(s)
	}
	t := page
	if format == "fragment" {
		t = content
	} else {
		frame.Refresh = pagekit.NewRefreshControl(0, query.Get("refresh"))
	}
	var data any = frame
	if format == "fragment" {
		data = frame.Content
	}
	var body bytes.Buffer
	if err := t.Execute(&body, data); err != nil {
		a.logger.Error("rendering the status page failed", "error", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", pagekit.Policy)
	_, _ = w.Write(body.Bytes())
}

// pageFrame is what frames a page's content: the bar, the heading, the
// refresh controls and, on a member's or a probe job's view, its way back
// and its filter.
type pageFrame struct {
	Version, Revision string
	Refresh           pagekit.RefreshControl
	Docs              string
	// JSON is where the bar's JSON link points, relative to the page.
	JSON string
	// Member is the member's view, Job the probe job's and Entry the
	// discovered_targets entry's, nil elsewhere.
	Member *memberPage
	Job    *jobPage
	Entry  *entryPage
	// Filter is a view's filter, nil on the status page and on a view a
	// follower leaves to its leader.
	Filter *filterForm
	// Content is what the content template renders: a statusPage, Member
	// or Job.
	Content any
}

// filterForm is a view's filter: outside the content, so a refresh keeps
// what is typed, and sent with the view's own parameter, Param=Value, member
// or job. Text is the filter in force, Limit the bound of a list, and
// Hidden hides the form while the allocator knows no such member or job.
// SourceLabelsBox puts a Show source labels box in the form, on the view
// of a job with discovery sources alone, and SourceLabels ticks it
// (?meta=1).
type filterForm struct {
	Param, Value, Text            string
	Limit                         int
	Hidden                        bool
	SourceLabelsBox, SourceLabels bool
}

// statusPage is what the page's template renders: the status, with the
// spread laid out for its table and bars.
type statusPage struct {
	Status
	View spreadView
}

// spreadView is the spread as the table shows it: a row of each member,
// with its bar, and rows of the total, the average and the imbalance. The
// probe jobs have a table of their own, a row each. FairShareMost is the
// fair share of the member of the largest capacity, which a heavy unit
// weighs more than.
type spreadView struct {
	Members                   []spreadRow
	Total, Average, Imbalance spreadRow
	FairShareMost             float64
	Shown                     bool
}

// spreadRow is a member's row, or the total's, the average's or the
// imbalance's: its targets, and their weight. Weight is what it owns and
// has routed, the weight it is placed by, and DrainingWeight what it keeps
// draining, which weighs on the targets' new owners. The bar runs on the
// weight, StaticPct, DrainingPct and ProbePct its segments' widths and
// FairPct where the member's fair share falls, from 0 to the most a
// member weighs or a fair share comes to.
type spreadRow struct {
	Member                              string
	Static, Draining, Probe, Total      float64
	Capacity, Weight, DrainingWeight    float64
	FairShare                           float64
	Overloaded                          bool
	StaticPct, DrainingPct, ProbePct    float64
	FairPct                             float64
	StaticText, DrainingText, ProbeText string
}

func statusPageOf(s Status) statusPage {
	page := statusPage{Status: s}
	sp := s.Spread
	if sp == nil {
		return page
	}
	v := spreadView{Shown: len(sp.Members) > 0}
	most := 0.0
	for _, m := range sp.Members {
		most = max(most, m.Weight.Total, m.FairShare)
		v.FairShareMost = max(v.FairShareMost, m.FairShare)
	}
	pct := func(n float64) float64 {
		if most == 0 {
			return 0
		}
		return math.Round(n/most*1000) / 10
	}
	row := func(name string, c, w SpreadCounts) spreadRow {
		return spreadRow{Member: name, Static: c.Static, Draining: c.Draining, Probe: c.Probe, Total: c.Total,
			Weight: w.Static + w.Probe, DrainingWeight: w.Draining}
	}
	var drainingMost float64
	for _, m := range sp.Members {
		r := row(m.Member, m.SpreadCounts, m.Weight)
		r.Capacity, r.FairShare, r.Overloaded = m.Capacity, m.FairShare, m.Overloaded
		r.StaticPct, r.DrainingPct, r.ProbePct, r.FairPct = pct(m.Weight.Static), pct(m.Weight.Draining), pct(m.Weight.Probe), pct(m.FairShare)
		r.StaticText = fmt.Sprintf("%s: %s static targets owned, weighing %s", m.Member, count(m.Static), weight(m.Weight.Static))
		r.DrainingText = fmt.Sprintf("%s: %s static targets draining, weighing %s", m.Member, count(m.Draining), weight(m.Weight.Draining))
		r.ProbeText = fmt.Sprintf("%s: %s probe targets routed, weighing %s", m.Member, count(m.Probe), weight(m.Weight.Probe))
		drainingMost = max(drainingMost, m.Weight.Draining)
		v.Members = append(v.Members, r)
	}
	n := float64(len(sp.Members))
	v.Total = row("Total", sp.Total, sp.TotalWeight)
	v.Total.Capacity, v.Total.FairShare = sp.Capacity, v.Total.Weight
	v.Average = row("Average per member", sp.Average, SpreadCounts{})
	v.Imbalance = row("Imbalance, most ÷ average", sp.Imbalance, SpreadCounts{})
	if n > 0 {
		v.Average.Capacity, v.Average.Weight, v.Average.DrainingWeight, v.Average.FairShare = sp.Capacity/n, v.Total.Weight/n, v.Total.DrainingWeight/n, v.Total.Weight/n
	}
	// A member's weight is weighed against its fair share, which is the
	// average for members of one capacity: the most a member holds of its
	// fair share is the weight's imbalance.
	v.Imbalance.Weight = sp.MaxLoadRatio
	if v.Average.DrainingWeight > 0 {
		v.Imbalance.DrainingWeight = drainingMost / v.Average.DrainingWeight
	}
	page.View = v
	return page
}

// count writes a count, average writes an average to a tenth, and
// imbalance a ratio to a hundredth with ×, "–" for none.
func count(n float64) string   { return strconv.FormatFloat(n, 'f', 0, 64) }
func average(n float64) string { return strconv.FormatFloat(n, 'f', 1, 64) }
func imbalance(n float64) string {
	if n == 0 {
		return "–"
	}
	return strconv.FormatFloat(n, 'f', 2, 64) + "×"
}

// weight writes a weight or a capacity as the files write it, to the
// thousandth at most: 2, 0.5, 1.25.
func weight(n float64) string {
	return strconv.FormatFloat(math.Round(n*milli)/milli, 'f', -1, 64)
}

// age writes seconds of an age, how long ago something was, in whole
// seconds, minutes and hours, as the pages write a duration
// (pagekit.Duration): 1m, not 1m0s. The fraction of an age that has run on
// since is no news; an interval configured is written by interval.
func age(s float64) string {
	d := time.Duration(s * float64(time.Second)).Truncate(time.Second)
	if d < time.Second {
		return "0s"
	}
	return pagekit.Duration(d)
}

// interval writes seconds of an interval configured, as the heartbeat
// interval or a source's refresh interval, as it was given, its fraction
// kept (pagekit.Duration): a watch interval of 500ms is 500ms, and a
// settle time of 1500ms is 1.5s, where age would write 0s and 1s. The
// seconds are rounded to the nanosecond, a duration's own unit, which is
// what the float of the JSON came from.
func interval(s float64) string {
	return pagekit.Duration(time.Duration(math.Round(s * float64(time.Second))))
}

// shortGeneration is a generation's first 8 characters, which tell
// generations apart on the page; its title has the whole.
func shortGeneration(g string) string {
	if len(g) > 8 {
		return g[:8]
	}
	return g
}

// sourceNames are a probe job's sources' names, a line each, as the probe
// jobs table's filter matches them.
func sourceNames(sources []JobSource) string {
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		names = append(names, s.Name)
	}
	return strings.Join(names, "\n")
}

// jobMatch is a probe job row's data-match: the job, its collector and
// its sources' names, a line each.
func jobMatch(j JobSpread) string {
	return strings.Join([]string{j.Job, j.Collector, sourceNames(j.Sources)}, "\n")
}

var statusFuncs = template.FuncMap{"count": count, "weight": weight, "average": average, "imbalance": imbalance, "age": age, "interval": interval, "duration": pagekit.DurationText, "gen": shortGeneration,
	"listNote": listNote, "match": matchText, "jobMatch": jobMatch, "sourceNames": sourceNames, "plus": plus, "andList": andList,
	"exporterStatus": exporterStatus, "exporterSeries": exporterSeries}

// plus is a + b, for a total of two counts.
func plus(a, b int) int { return a + b }

// andList writes names as prose: "a", "a and b", "a, b and c".
func andList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// statusStyle is the page's own style, after pagekit.Style: its tables, as
// the exporter's status page draws them, numbers right-aligned in figures
// of one width so a column's digits line up, and the bars.
const statusStyle = `
main { max-width: 1200px; }
.scroll { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; font-size: 14px; }
th, td { text-align: left; vertical-align: top; padding: 6px 8px; border-bottom: 1px solid var(--line); }
th { background: var(--panel); white-space: nowrap; }
td { overflow-wrap: normal; }
th.num, td.num { text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
td.id { overflow-wrap: anywhere; min-width: 9em; }
/* A source's kind, under its name, its owner's, and where its targets
   came from are short phrases that read badly broken over lines. The
   names of a source and its owner, a probe job's, are as long as they were
   written, so they wrap, at a hyphen where they have one, rather than push
   the columns past the page's edge; the result, with its error, takes the
   width that is left, a path in the error broken where it must be. */
table.sources span.kind, table.sources td:last-child { white-space: nowrap; }
table.sources td.owner { overflow-wrap: anywhere; min-width: 9em; }
table.sources td.result { overflow-wrap: anywhere; min-width: 12em; }
tfoot td { background: var(--panel); }
/* Scrolled sideways, as on a phone, the spread keeps each row's member in
   view beside its counts and bar. */
table.spread tr > :first-child { position: sticky; left: 0; z-index: 1; background: var(--bg); }
table.spread thead th:first-child, table.spread tfoot td:first-child { background: var(--panel); }
tfoot tr:first-child td { border-top: 2px solid var(--line); }
h3 { font-size: 15px; margin: 24px 0 8px; }
.scroll + table.facts { margin-top: 12px; }
td.series { overflow-wrap: anywhere; }
td.series code { overflow-wrap: anywhere; }
table.facts { width: auto; }
table.facts th { background: none; font-weight: 600; white-space: nowrap; }
table.facts td { overflow-wrap: anywhere; }
.bad { color: var(--bad); font-weight: 600; }
.good { color: var(--good); }
.muted { color: var(--muted); }
code.gen, code.flag, p.notice code { white-space: nowrap; }
p.notice { margin: 16px 0 0; padding: 10px 14px; border: 1px solid var(--line); border-left: 4px solid var(--accent); border-radius: 6px; background: var(--panel); }
/* The heavy units' warning: a notice marked as a fault is, its units a
   list, each shard key, a target's address or URL, broken where it must
   be rather than push the box past the page's edge. */
div.notice.warning { margin: 0 0 12px; padding: 10px 14px; border: 1px solid var(--line); border-left: 4px solid var(--bad); border-radius: 6px; background: var(--panel); }
div.notice.warning p { margin: 0; }
div.notice.warning ul { margin: 6px 0 0; padding-left: 20px; }
code.shard-key { overflow-wrap: anywhere; }
:root { --bar-static: #c2410c; --bar-probe: #2563eb; }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { --bar-static: #e26a35; --bar-probe: #4d8fea; } }
:root[data-theme="dark"] { --bar-static: #e26a35; --bar-probe: #4d8fea; }
td.bar { width: 32%; min-width: 120px; }
/* On the first line of the row, beside the counts, however the member's
   id wraps: 12px centred in a 21px line. A segment's width is its share
   of the track exactly, so the bars of two rows compare at every edge: the
   2px between two segments is a border of the later one, in the page's
   colour, inside its width rather than a gap beside it. */
.bar-track { position: relative; display: flex; height: 12px; margin-top: 5px; }
.bar-track span { display: block; box-sizing: border-box; height: 100%; min-width: 0; }
.bar-track span + span { border-left: 2px solid var(--bg); }
.bar-track span:first-child { border-radius: 2px 0 0 2px; }
.bar-track span:last-of-type { border-radius: 0 4px 4px 0; }
.seg-static { background: var(--bar-static); }
.seg-draining { background: repeating-linear-gradient(135deg, var(--bar-static) 0 2px, transparent 2px 5px); box-shadow: inset 0 0 0 1px var(--bar-static); }
.seg-probe { background: var(--bar-probe); }
.bar-track i.avg { position: absolute; top: -3px; bottom: -3px; width: 0; border-left: 2px solid var(--fg); }
.legend { display: flex; flex-wrap: wrap; gap: 4px 16px; margin: 8px 0 0; font-size: 13px; color: var(--muted); }
.legend span { display: inline-flex; align-items: center; gap: 6px; }
.legend b { display: inline-block; width: 12px; height: 12px; border-radius: 2px; }
.legend b.avg { width: 0; height: 14px; border-radius: 0; border-left: 2px solid var(--fg); }
/* A kind of target's colour, the bars', beside its name wherever the page
   names it: the definitions, the members table's headings, the sections'
   headings and what a discovery source feeds. */
b.swatch { display: inline-block; width: 12px; height: 12px; border-radius: 2px; margin-right: 6px; vertical-align: -1px; }
th.group { text-align: center; border-bottom: 1px solid var(--line); }
@media (max-width: 640px) { td.bar { min-width: 90px; } }
` + pagekit.SortableStyle + `/* A job's sources' names wrap, as the sources table's do, so a long one
   does not push the table past the page's edge. */
td.sources { min-width: 10em; }
td.sources span.source { overflow-wrap: anywhere; }
form.filter.jobs { margin: 0 0 8px; }
form.filter.jobs input[type="search"] { flex: none; width: 100%; box-sizing: border-box; }
/* On a phone the label is "Instance" over "ID", so the column is no wider
   than its other labels and the instance ID keeps the room it has beside
   them, rather than its last character wrapping alone onto a line. */
@media (max-width: 400px) { table.facts th.wraps { white-space: normal; overflow-wrap: normal; width: 1px; } }
`

// statusChrome is the page's bar, the exporter's in its shape (pagekit),
// with the allocator's own links.
const statusChrome = `<header class="site"><nav aria-label="The allocator's pages">
{{if or .Member .Job .Entry}}<a class="home" href="./">Prometheus Universal Exporter · Target Allocator</a>{{else}}<span class="home" aria-current="page">Prometheus Universal Exporter · Target Allocator</span>{{end}}
<span class="pages"><a href="{{.JSON}}">JSON</a><a href="metrics">Metrics</a><a href="{{.Docs}}">Documentation</a>` + pagekit.ThemePicker + `</span>
</nav></header>`

// statusTemplate is a page of the frame and the content content
// defines, and contentTemplate its content alone, which a refresh fetches.
func statusTemplate(content string) (page, contentTemplate *template.Template) {
	contentTemplate = template.Must(template.New("content").Funcs(statusFuncs).Parse(statusParts + content))
	page = template.Must(template.Must(template.New("page").Funcs(statusFuncs).Parse(statusParts + content)).Parse(statusFrame))
	return page, contentTemplate
}

var (
	statusPageTemplate, statusContentTemplate = statusTemplate(statusContent)
	memberPageTemplate, memberContentTemplate = statusTemplate(memberContent)
	jobPageTemplate, jobContentTemplate       = statusTemplate(jobContent)
	entryPageTemplate, entryContentTemplate   = statusTemplate(entryContent)
)

// statusFrame frames a page's content (pageFrame).
var statusFrame = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{with .Member}}Member {{.Member}}{{else}}{{with .Job}}Probe job {{.Job}}{{else}}{{with .Entry}}Entry {{.Entry}}{{else}}Status{{end}}{{end}}{{end}} · pue-allocator</title>
` + pagekit.Icon + "\n" + pagekit.ThemeHead + `
<style>` + pagekit.Style + pagekit.RefreshStyle + statusStyle + memberStyle + `</style>
{{- if .Refresh.Seconds}}
<noscript><meta http-equiv="refresh" content="{{.Refresh.Seconds}}"></noscript>
{{- end}}
</head>
<body>
<main>
` + statusChrome + `
{{- if .Member}}
<p class="back"><a href="./">← Allocator status</a></p>
<h1>Member <span class="member-id">{{.Member.Member}}</span></h1>
{{- else if .Job}}
<p class="back"><a href="./">← Allocator status</a></p>
<h1>Probe job <span class="member-id">{{.Job.Job}}</span></h1>
{{- else if .Entry}}
<p class="back"><a href="./">← Allocator status</a></p>
<h1>Entry <span class="member-id">{{.Entry.Entry}}</span> <span class="muted kind">discovered_targets</span></h1>
{{- else}}
<h1>Allocator status</h1>
{{- end}}
<p class="meta">Version {{.Version}}, revision {{.Revision}}</p>
<form class="refresh" id="refresh" method="get">
<a class="button" id="refresh-now" href="">Refresh</a>
<label>Auto-refresh <select name="refresh" id="refresh-every">
{{- range .Refresh.Choices}}
<option value="{{.Value}}" data-seconds="{{.Seconds}}"{{if .Selected}} selected{{end}}>{{if eq .Value "off"}}Off{{else}}{{.Value}}{{end}}</option>
{{- end}}
</select></label>
<noscript><button type="submit">Apply</button></noscript>
<span id="refresh-state" class="muted" role="status" aria-live="polite"></span>
</form>
{{- with .Filter}}
<form class="filter" id="filter" method="get" role="search"{{if .Hidden}} hidden{{end}}>
<input type="hidden" name="{{.Param}}" value="{{.Value}}">
{{- if $.Refresh.Seconds}}
<input type="hidden" name="refresh" value="{{$.Refresh.Interval}}">
{{- end}}
{{- if eq .Param "member"}}
<label for="target-filter">Filter: name, target, collector, label or job</label>
{{- else if eq .Param "entry"}}
<label for="target-filter">Filter: name, target, collector, label or member</label>
{{- else}}
<label for="target-filter">Filter: target, member or label</label>
{{- end}}
<span class="filter-row"><input id="target-filter" name="filter" type="search" value="{{.Text}}" autocomplete="off" spellcheck="false"><button type="submit">Search all</button></span>
{{- if .SourceLabelsBox}}
<label class="check" for="source-labels"><input id="source-labels" name="meta" value="1" type="checkbox"{{if .SourceLabels}} checked{{end}}>Show source labels <span class="muted">(names starting with <code>__</code>)</span></label>
{{- end}}
<span class="muted" id="filter-hint">Typing filters the rows shown; Search all asks the allocator, which looks through every target of the {{.Param}}{{if gt .Limit 0}}, beyond the {{.Limit}} a list shows{{end}}. {{if eq .Param "member"}}Both match a target's name, target, collector, labels, probe job and the member it moved to.{{else if eq .Param "entry"}}Both match a target's name, target, collector, labels and the member that owns it.{{else}}Both match a target, the member it is routed to and the labels shown.{{end}}</span>
</form>
{{- end}}
<div id="status-content">
{{- template "content" .Content}}
</div>
<script>
"use strict";
// Refresh and the auto-refresh fetch the content again, ?format=fragment,
// and swap it in, as the exporter's status page does: the fetch is
// same-origin, so it carries the credentials the page was opened with. A
// hidden tab is not refreshed, and a refresh still waiting for the URL as
// it is is not overlapped (refresh). Without JavaScript the button reloads
// the page, the form puts the interval in the URL, and the meta refresh
// reloads it.
(() => {
  const $ = (id) => document.getElementById(id);
  const content = $("status-content"), every = $("refresh-every"), state = $("refresh-state");
  let timer = 0, inFlight = null, refreshedAt = Date.now(), dataAt = "";
  const clock = (ms) => new Date(ms).toLocaleTimeString();
  const seconds = () => Number(every.selectedOptions[0].dataset.seconds) || 0;
  function asOf() {
    const line = content.querySelector("#as-of"), time = line && line.querySelector("time");
    if (!time) return;
    line.hidden = true;
    dataAt = time.getAttribute("datetime");
    state.title = dataAt;
    state.className = "muted";
    state.textContent = "As of " + clock(Date.parse(dataAt) || refreshedAt);
  }
  asOf();
  // A member's or a probe job's view filters its tables' rows as the
  // filter is typed,
  // matching in any case the texts the allocator's filter matches, which
  // each row carries in data-match, one a line, and counts the rows left;
  // the filter is outside the content, so a refresh keeps what was typed
  // and filters the new rows again. Search all asks the allocator, which
  // filters every target, those beyond a list's bound included. The
  // filter is hidden while the allocator knows no such member or job.
  const filter = $("target-filter");
  function filterRows() {
    if (!filter) return;
    const needle = filter.value.trim().toLowerCase();
    for (const table of content.querySelectorAll("table.targets")) {
      let shown = 0;
      const rows = table.tBodies[0].rows;
      for (const row of rows) {
        const hit = needle === "" || (row.dataset.match || "").toLowerCase().includes(needle);
        row.hidden = !hit;
        if (hit) shown++;
      }
      const count = content.querySelector('[data-rows-of="' + table.id + '"]');
      if (count) count.textContent = shown === rows.length || shown === 0 ? "" : shown + " of the " + rows.length + " rows below match.";
      const none = content.querySelector('[data-none-of="' + table.id + '"]');
      if (none) none.hidden = shown > 0;
      table.hidden = shown === 0;
    }
    $("filter").hidden = content.querySelector("#unknown-member, #unknown-job") !== null;
  }
  // A job's view's Show source labels box puts ?meta=1 in the URL, or
  // takes it out, and refreshes the content at once: the allocator writes
  // the rows with or without the targets' source labels, and their
  // data-match with them, so the filter matches what is shown. A refresh
  // fetches the URL's fragment, so it keeps the choice, as a bookmark does.
  // Without JavaScript the box goes with Search all.
  const sourceLabels = $("source-labels");
  if (sourceLabels) {
    sourceLabels.addEventListener("change", () => {
      const url = new URL(location.href);
      if (sourceLabels.checked) url.searchParams.set("meta", "1");
      else url.searchParams.delete("meta");
      history.replaceState(null, "", url);
      refresh();
    });
  }
  if (filter) {
    filter.addEventListener("input", filterRows);
    $("filter").addEventListener("submit", (event) => {
      event.preventDefault();
      const url = new URL(location.href);
      if (filter.value.trim() === "") url.searchParams.delete("filter");
      else url.searchParams.set("filter", filter.value.trim());
      location.assign(url);
    });
    filterRows();
  }
  // The status page's probe jobs table sorts and filters as
  // pagekit.SortableScript says.
` + pagekit.SortableScript + pagekit.LocalTimesScript + `
  const arrangeJobs = sortable(content, "probe-jobs", "jobs-filter", "probe jobs");
  localTimes(content);
  // A refresh asked while another waits for the URL as it is now is that
  // one: the answer coming is the one asked for, and a slow allocator is
  // not asked again at every tick of the auto-refresh. One asked after the
  // URL changed, as the Show source labels box changes it, aborts the one
  // waiting, whose answer is to the URL before, and asks again, so the
  // latest wins and an answer to a URL left is never swapped in.
  async function refresh() {
    const url = new URL(location.href);
    url.searchParams.set("format", "fragment");
    url.hash = "";
    if (inFlight && inFlight.url === url.href) return;
    if (inFlight) inFlight.abort();
    const mine = new AbortController();
    mine.url = url.href;
    inFlight = mine;
    try {
      const response = await fetch(url, {credentials: "same-origin", cache: "no-store", signal: mine.signal});
      if (!response.ok) throw new Error("HTTP " + response.status);
      const text = await response.text();
      if (inFlight !== mine) return;
      arrangeJobs(() => {
        content.innerHTML = text;
        refreshedAt = Date.now();
        asOf();
        filterRows();
        localTimes(content);
      });
    } catch (error) {
      // A refresh aborted for a later one says nothing: the later one says
      // how it went.
      if (inFlight !== mine) return;
      state.className = "bad";
      state.textContent = "Refresh failed at " + clock(Date.now()) + " (" + error.message + "); showing the data as of " + clock(Date.parse(dataAt) || refreshedAt);
    } finally {
      if (inFlight === mine) inFlight = null;
    }
  }
  function schedule() {
    clearInterval(timer);
    timer = 0;
    const s = seconds();
    if (!s || document.visibilityState === "hidden") return;
    if (Date.now() - refreshedAt >= s * 1000) refresh();
    timer = setInterval(refresh, s * 1000);
  }
  $("refresh-now").addEventListener("click", (event) => {
    event.preventDefault();
    refresh();
  });
  every.addEventListener("change", () => {
    const url = new URL(location.href);
    url.searchParams.set("refresh", every.value);
    history.replaceState(null, "", url);
    schedule();
  });
  $("refresh").addEventListener("submit", (event) => event.preventDefault());
  document.addEventListener("visibilitychange", schedule);
  schedule();
})();
</script>
</main>
</body>
</html>
`

// statusParts are what the status page and the views share: of a member
// (MemberStatus), how long ago its last heartbeat came, marked when one is
// overdue, the bundle it has in force beside the one it is to have, and
// its state; a discovery source's result; and a view's targets, labels
// and the note over a list.
const statusParts = `
{{- define "heartbeat"}}{{if .Late}}<time class="ago bad" datetime="{{.LastHeartbeat}}" data-note="a heartbeat is overdue" title="a heartbeat is overdue; {{.LastHeartbeat}}">{{age .LastHeartbeatAgeSeconds}} ago</time>{{else}}<time class="ago" datetime="{{.LastHeartbeat}}" title="{{.LastHeartbeat}}">{{age .LastHeartbeatAgeSeconds}} ago</time>{{end}}{{end}}
{{- define "bundle"}}{{if .UpToDate}}<code class="gen" title="{{.Generation}}">{{gen .Generation}}</code> <span class="good">current</span>{{else}}{{if .Generation}}<code class="gen" title="{{.Generation}}">{{gen .Generation}}</code>{{else}}<span class="muted">none</span>{{end}}{{if .Current}} <span class="bad">→</span> <code class="gen" title="{{.Current}}">{{gen .Current}}</code>{{end}}{{end}}{{if .Refused}}<br><span class="bad">refused</span> <code class="gen" title="{{.Refused}}">{{gen .Refused}}</code>{{end}}{{end}}
{{- define "state"}}{{if .ShuttingDown}}<span class="bad">shutting down</span>{{else if and .Live .InList}}<span class="good">in the list</span>{{else if .Live}}joining{{else if .InList}}<span class="bad">silent</span>, leaving{{else}}<span class="muted">gone</span>{{end}}{{if .SameName}}, <span class="bad" title="Two exporters heartbeat under this member name, and each scrapes its share: give each a name of its own with --allocator.member">two exporters under this name</span>{{end}}{{end}}
{{- define "capacity"}}{{if and .InList (ne .CapacityInList .Capacity)}}<span title="the member list places it by {{weight .CapacityInList}} until its heartbeats' {{weight .Capacity}} has held for the settle time">{{weight .CapacityInList}} <span class="bad">→</span> {{weight .Capacity}}</span>{{else}}{{weight .Capacity}}{{end}}{{end}}
{{- define "result"}}{{if eq . "ok"}}<span class="good">ok</span>{{else if eq . "failing"}}<span class="bad">failing</span>{{else}}<span class="muted">not answered yet</span>{{end}}{{end}}
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
<p class="note muted" data-rows-of="{{.Table}}" role="status" aria-live="polite"></p>
<p class="muted" data-none-of="{{.Table}}" hidden>No row below holds what is typed in the filter; Search all looks through every target.</p>
{{- end}}
{{- end}}
`

// statusContent is the page's content, which a refresh swaps in.
const statusContent = `{{define "content"}}
<p class="meta" id="as-of">As of <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.</p>
{{- if .Partial}}{{with .Peers}}
<p class="notice">This instance {{if eq .Role "follower"}}follows the leader, <strong>{{.Leader}}</strong>{{else}}has not yet chosen its leader{{end}}, and runs with <code class="flag">--peers.leader-only</code>: the members, the spread of the targets and the discovery sources are on the leader's page, and in the leader's metrics. Below are this instance and its peers.</p>
{{- end}}{{end}}

<h2>This instance</h2>
<table class="facts">
<tr><th>Address</th><td>{{.Instance.Address}}</td></tr>
<tr><th class="wraps">Instance ID</th><td><code>{{.Instance.ID}}</code></td></tr>
<tr><th>Started</th><td><time datetime="{{.Instance.StartedAt}}">{{.Instance.StartedAt}}</time> <span class="muted">(up {{age .Instance.UptimeSeconds}})</span></td></tr>
<tr><th>Input files</th><td>{{if .Instance.WatchIntervalSeconds}}read again every {{interval .Instance.WatchIntervalSeconds}} for changes{{else}}<span class="muted">read once, at the start</span> (<code class="flag">--config.watch-interval=0</code>){{end}}</td></tr>
<tr><th>Ready</th><td>{{if .Instance.Ready}}<span class="good">ready</span>{{else}}<span class="bad">shutting down</span>{{end}}</td></tr>
<tr><th>Peers</th><td>{{with .Peers}}{{if .LeaderOnly}}<span class="muted">(leader only)</span> {{end}}{{if eq .Role "leader"}}<span class="good">leader</span>{{if gt .LiveInstances 1}} of {{.LiveInstances}} live instances{{else}}, the only live instance{{end}}{{else if eq .Role "follower"}}follower of {{.Leader}}{{else}}<span class="muted">choosing the leader</span>{{end}}{{else}}<span class="muted">off: this instance asks its discovery sources itself (no <code class="flag">--peers</code>)</span>{{end}}</td></tr>
</table>
{{- with .Peers}}

<h2>Peers</h2>
<p>{{if eq .Role "leader"}}This instance leads: it asks the discovery sources, and the other instances take what it finds.{{else if eq .Role "follower"}}This instance follows {{.Leader}}: it takes the leader's discovered targets from its snapshot.{{else}}This instance has not yet checked its peers.{{end}} {{.LiveInstances}} live instance{{if ne .LiveInstances 1}}s{{end}}, {{.LeaderChanges}} leader change{{if ne .LeaderChanges 1}}s{{end}}. The instances check each other every {{interval .CheckIntervalSeconds}}, each check within {{interval .CheckTimeoutSeconds}}, and an instance stops being live after {{.FailureThreshold}} failed check{{if ne .FailureThreshold 1}}s{{end}} in a row; a follower fetches the leader's snapshot with each round of checks, a fetch within {{interval .SnapshotTimeoutSeconds}}.{{if not .AddressKnown}} <span class="bad">This instance does not know its own address: list every instance in <code class="flag">--peers</code>, or give <code class="flag">--peers.self</code>.</span>{{end}}</p>
<div class="scroll">
<table>
<thead><tr><th>Instance</th><th>Role</th><th>Live</th><th>Discovering</th><th>Address known</th><th class="num">Last check</th><th class="num">Failures</th></tr></thead>
<tbody>
{{- range .Instances}}
<tr>
<td class="id">{{.Address}}{{if .Self}} <span class="muted">(this instance)</span>{{end}}{{if .ID}}<br><span class="muted">ID</span> <code>{{.ID}}</code>{{end}}</td>
<td>{{if .Leader}}<strong>leader</strong>{{else}}follower{{end}}</td>
<td>{{if .Live}}<span class="good">live</span>{{else}}<span class="bad">down</span>{{end}}</td>
<td>{{if .Discovering}}yes{{else}}<span class="muted">no</span>{{end}}</td>
<td>{{if .AddressKnown}}yes{{else}}<span class="bad">no</span>{{end}}</td>
<td class="num">{{if .Self}}<span class="muted">–</span>{{else if .LastCheck}}<time class="ago" datetime="{{.LastCheck}}" title="{{.LastCheck}}">{{age .LastCheckAgeSeconds}} ago</time>{{else}}<span class="muted">never</span>{{end}}</td>
<td class="num">{{if .Failures}}<span class="bad">{{.Failures}}</span>{{else}}0{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
<table class="facts">
<tr><th>Leader's targets</th><td>{{if eq .Role "leader"}}this instance's own discovery{{else if .HoldsLeaderState}}<span class="good">held</span>, the snapshot fetched {{age .SnapshotAgeSeconds}} ago{{else if .SnapshotTaken}}<span class="bad">not held</span>: the input files differ from the leader's in their discovery sources, last agreed {{age .SnapshotAgeSeconds}} ago{{else}}<span class="bad">not yet fetched</span> ({{age .SnapshotAgeSeconds}} since the start){{end}}</td></tr>
<tr><th>Snapshot fetches</th><td>{{if .SnapshotFailuresInARow}}<span class="bad">{{.SnapshotFailuresInARow}} failed in a row</span>; {{end}}{{.SnapshotFailures}} failed in all, and {{.CheckFailures}} checks</td></tr>
</table>
<h3>Leader election metrics</h3>
<div class="scroll">
<table>
<thead><tr><th>Series</th><th class="num">Value</th></tr></thead>
<tbody>
{{- range .Metrics}}
<tr><td class="series"><code>{{.Series}}</code></td><td class="num">{{.Value}}</td></tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}
{{- with .Members}}

<h2>Members</h2>
<p>{{if .Formed}}{{.InList}} member{{if ne .InList 1}}s{{end}} in the member list, {{.Live}} live.{{else}}<span class="bad">The member list has not formed yet</span>: no bundle or service discovery answer is served until it does. {{.Live}} live.{{end}} The replicas heartbeat every {{interval .HeartbeatIntervalSeconds}}; a member not heard from for {{interval .MemberTTLSeconds}} stops being live. A changed member list holds for {{interval .SettleSeconds}} before targets move.</p>
{{- if .List}}
<div class="scroll">
<table>
<thead><tr><th>Member</th><th>Address</th><th class="num">Last heartbeat</th><th>Bundle in force</th><th>State</th><th class="num">Capacity</th><th class="num">Draining</th></tr></thead>
<tbody>
{{- range .List}}
<tr>
<td class="id"><a href="?member={{.ID}}" title="the targets of {{.ID}}"><strong>{{.ID}}</strong></a></td>
<td>{{.Address}}</td>
<td class="num">{{template "heartbeat" .}}</td>
<td>{{template "bundle" .}}</td>
<td>{{template "state" .}}</td>
<td class="num">{{template "capacity" .}}</td>
<td class="num">{{.Draining}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}
{{- end}}
{{- with .View}}{{if .Shown}}

<h2>Targets</h2>
<p class="kinds"><b class="swatch seg-static"></b><strong>Static targets</strong> are scraped by the member that owns them, on their own interval, and served on its <code>/static-targets</code>. <b class="swatch seg-probe"></b><strong>Probe targets</strong> are what Prometheus asks this allocator for, <code>/v1/sd/&lt;job&gt;</code>, each pointed at the member that probes it, and Prometheus probes them through that member.</p>
{{- with $.Spread}}
<p id="kinds-summary">{{plus .StaticWritten .StaticDiscovered}} static target{{if ne (plus .StaticWritten .StaticDiscovered) 1}}s{{end}}: {{.StaticWritten}} written in the static target file{{if .StaticDiscovered}} and {{.StaticDiscovered}} discovered by {{andList .StaticSources}}{{end}}. {{plus .ProbeListed .ProbeDiscovered}} probe target{{if ne (plus .ProbeListed .ProbeDiscovered) 1}}s{{end}}: {{.ProbeListed}} listed in the probe jobs{{if .ProbeDiscovered}} and {{.ProbeDiscovered}} discovered by {{andList .ProbeSources}}{{end}}.</p>
{{- end}}
<p>{{count .Total.Static}} static target{{if ne .Total.Static 1.0}}s{{end}}{{if .Total.Draining}}, {{count .Total.Draining}} draining,{{end}} and {{count .Total.Probe}} probe target{{if ne .Total.Probe 1.0}}s{{end}} over {{len .Members}} member{{if ne (len .Members) 1}}s{{end}}: {{average .Average.Total}} per member on average, the most {{imbalance .Imbalance.Total}} the average. They weigh {{weight .Total.Weight}}{{if .Total.DrainingWeight}}, and those draining {{weight .Total.DrainingWeight}} more on the members that keep them,{{end}} over a capacity of {{weight .Total.Capacity}}: a member's fair share is the weight times its capacity over the capacity of all, and the most loaded holds {{imbalance .Imbalance.Weight}} its fair share. A target goes to the member its hash prefers while that member's weight stays within {{weight $.Spread.BalanceFactor}} times its fair share (<code class="flag">--allocation.balance-factor</code>), and to the next otherwise.</p>
{{- if $.Spread.Overloaded}}
<p><span class="bad">{{$.Spread.Overloaded}} member{{if ne $.Spread.Overloaded 1}}s are{{else}} is{{end}} overloaded</span>, {{if ne $.Spread.Overloaded 1}}their weights{{else}}its weight{{end}} marked: above {{weight $.Spread.BalanceFactor}} times {{if ne $.Spread.Overloaded 1}}their fair shares{{else}}its fair share{{end}}, which only a target, or the targets of one shard key, too heavy for the room any member has left puts a member.</p>
{{- end}}
{{- with $.Spread.Heavy}}
<div class="notice warning" id="heavy-units">
<p><strong>{{len .}} unit{{if ne (len .) 1}}s weigh{{else}} weighs{{end}} more than a member's fair share</strong>, {{average $.View.FairShareMost}}, that of the member of the largest capacity: such a unit, a target or the targets of one shard key, fills the member it is on, which then holds little else, and no member list can spread it. Split it, by a <code>shard_key</code> of its own for each target, or give it a weight nearer the others'.</p>
<ul>
{{- range .}}
<li><code class="shard-key">{{.ShardKey}}</code> weighs <strong>{{weight .Weight}}</strong>, on <a href="?member={{.Member}}" title="the targets of {{.Member}}">{{.Member}}</a></li>
{{- end}}
</ul>
</div>
{{- end}}
<div class="scroll">
<table class="spread">
<thead>
<tr><th rowspan="2">Member</th><th class="group" colspan="2"><b class="swatch seg-static"></b>Static targets</th><th class="group"><b class="swatch seg-probe"></b>Probe targets</th><th class="num" rowspan="2">Total</th><th class="num" rowspan="2">Weight</th><th class="num" rowspan="2">Draining weight</th><th class="num" rowspan="2">Capacity</th><th class="num" rowspan="2">Fair share</th><th rowspan="2">Share</th></tr>
<tr><th class="num">Owned</th><th class="num">Draining</th><th class="num">Routed</th></tr>
</thead>
<tbody>
{{- range .Members}}
<tr>
<td class="id"><a href="?member={{.Member}}" title="the targets of {{.Member}}"><strong>{{.Member}}</strong></a></td>
<td class="num">{{count .Static}}</td>
<td class="num">{{count .Draining}}</td>
<td class="num">{{count .Probe}}</td>
<td class="num"><strong>{{count .Total}}</strong></td>
<td class="num">{{if .Overloaded}}<strong class="bad" title="overloaded: above {{weight $.Spread.BalanceFactor}} times its fair share">{{weight .Weight}}</strong>{{else}}<strong>{{weight .Weight}}</strong>{{end}}</td>
<td class="num">{{weight .DrainingWeight}}</td>
<td class="num">{{weight .Capacity}}</td>
<td class="num">{{average .FairShare}}</td>
<td class="bar"><div class="bar-track" role="img" aria-label="{{.StaticText}}; {{.DrainingText}}; {{.ProbeText}}; its fair share {{average .FairShare}}">{{if .StaticPct}}<span class="seg-static" style="width: {{.StaticPct}}%" title="{{.StaticText}}"></span>{{end}}{{if .DrainingPct}}<span class="seg-draining" style="width: {{.DrainingPct}}%" title="{{.DrainingText}}"></span>{{end}}{{if .ProbePct}}<span class="seg-probe" style="width: {{.ProbePct}}%" title="{{.ProbeText}}"></span>{{end}}<i class="avg" style="left: {{.FairPct}}%" title="its fair share, {{average .FairShare}}"></i></div></td>
</tr>
{{- end}}
</tbody>
<tfoot>
{{- with .Total}}
<tr><td>{{.Member}}</td><td class="num">{{count .Static}}</td><td class="num">{{count .Draining}}</td><td class="num">{{count .Probe}}</td><td class="num">{{count .Total}}</td><td class="num">{{weight .Weight}}</td><td class="num">{{weight .DrainingWeight}}</td><td class="num">{{weight .Capacity}}</td><td class="num">{{weight .FairShare}}</td><td></td></tr>
{{- end}}
{{- with .Average}}
<tr><td>{{.Member}}</td><td class="num">{{average .Static}}</td><td class="num">{{average .Draining}}</td><td class="num">{{average .Probe}}</td><td class="num">{{average .Total}}</td><td class="num">{{average .Weight}}</td><td class="num">{{average .DrainingWeight}}</td><td class="num">{{average .Capacity}}</td><td class="num">{{average .FairShare}}</td><td></td></tr>
{{- end}}
{{- with .Imbalance}}
<tr><td>{{.Member}}</td><td class="num">{{imbalance .Static}}</td><td class="num">{{imbalance .Draining}}</td><td class="num">{{imbalance .Probe}}</td><td class="num">{{imbalance .Total}}</td><td class="num" title="the most a member holds of its fair share">{{imbalance .Weight}}</td><td class="num">{{imbalance .DrainingWeight}}</td><td></td><td></td><td></td></tr>
{{- end}}
</tfoot>
</table>
</div>
<p class="legend"><span><b class="seg-static"></b>static targets, owned</span><span><b class="seg-draining"></b>static targets, draining</span><span><b class="seg-probe"></b>probe targets, routed</span><span><b class="avg"></b>the member's fair share</span><span>Bars are weight, and run to the most a member weighs or a fair share comes to.</span></p>
{{- end}}{{end}}
{{- with .Spread}}{{if .Static}}

<h2 id="static-targets"><b class="swatch seg-static"></b>Static targets</h2>
<p>The static targets by where they come from: those the static target file writes, and each <code>discovered_targets</code> entry's. Each row's targets are spread over the member list: Min, Avg and Max count the row's targets a member owns, and Imbalance is Max ÷ Avg, <span class="bad">marked</span> as a probe job's is below.</p>
<div class="scroll">
<table class="spread" id="static-origins">
<thead><tr><th>From</th><th class="num">Targets</th><th class="num">Weight</th><th class="num">Members</th><th class="num">Min</th><th class="num">Avg</th><th class="num">Max</th><th class="num">Imbalance</th><th>Sources</th></tr></thead>
<tbody>
{{- range .Static}}
<tr{{if .Entry}} id="static-entry-{{.Entry}}"{{end}}>
<td class="id">{{if .Entry}}<a href="?entry={{.Entry}}" title="the static targets of the discovered_targets entry {{.Entry}}"><strong>{{.Entry}}</strong></a><br><span class="muted kind">discovered_targets entry</span>{{else}}<strong>written</strong><br><span class="muted kind">in the static target file</span>{{end}}</td>
<td class="num">{{.Targets}}</td>
<td class="num">{{weight .Weight}}</td>
<td class="num">{{len .Members}}</td>
{{- if .Members}}
<td class="num">{{.Min}}</td>
<td class="num">{{average .Average}}</td>
<td class="num">{{.Max}}</td>
<td class="num">{{if .Uneven}}<span class="bad" title="Max is further above Avg than hashing puts a member's targets by chance">{{imbalance .Imbalance}}</span>{{else}}{{imbalance .Imbalance}}{{end}}</td>
{{- else}}
<td class="num"><span class="muted">–</span></td><td class="num"><span class="muted">–</span></td><td class="num"><span class="muted">–</span></td><td class="num"><span class="muted">–</span></td>
{{- end}}
<td class="sources">{{range $k, $s := .Sources}}{{if $k}}<br>{{end}}<span class="source">{{.Name}} {{template "result" .Result}}</span>{{else}}<span class="muted">written</span>{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}{{end}}
{{- with .Spread}}{{if .Jobs}}

<h2 id="probe-jobs-section"><b class="swatch seg-probe"></b>Probe jobs</h2>
<p>{{len .Jobs}} probe job{{if ne (len .Jobs) 1}}s{{end}}, their probe targets by job. A job's Weight is what its targets weigh together, and its targets are spread over the members its probes are routed among: Min, Avg and Max count the targets of the job a member has, and Imbalance is Max ÷ Avg, <span class="bad">marked</span> where Max is further above Avg than hashing puts a member's targets by chance: by 3 targets or more, and by 3 standard deviations of a member's count or more, √(Targets × (1 − 1/Members) ÷ Members).</p>
<form class="filter jobs" id="jobs" role="search" hidden>
<label for="jobs-filter">Filter: job, collector or source</label>
<input id="jobs-filter" type="search" autocomplete="off" spellcheck="false">
<span class="muted">Click a column's heading to sort by it, again for the other way.</span>
</form>
<p class="note muted" data-rows-of="probe-jobs" role="status" aria-live="polite"></p>
<div class="scroll">
<table class="spread" id="probe-jobs">
<thead><tr><th data-sort="text">Job</th><th data-sort="text">Collector</th><th class="num" data-sort="num">Targets</th><th class="num" data-sort="num">Weight</th><th class="num" data-sort="num">Members</th><th class="num" data-sort="num">Min</th><th class="num" data-sort="num">Avg</th><th class="num" data-sort="num">Max</th><th class="num" data-sort="num">Imbalance</th><th data-sort="text">Sources</th></tr></thead>
<tbody>
{{- range $i, $j := .Jobs}}
<tr data-order="{{$i}}" data-match="{{jobMatch $j}}">
<td class="id"><a href="?job={{.Job}}" title="the targets of the probe job {{.Job}}"><strong>{{.Job}}</strong></a></td>
<td>{{.Collector}}</td>
<td class="num" data-value="{{.Targets}}">{{.Targets}}</td>
<td class="num" data-value="{{.Weight}}">{{weight .Weight}}</td>
<td class="num" data-value="{{len .Members}}">{{len .Members}}</td>
{{- if .Members}}
<td class="num" data-value="{{.Min}}">{{.Min}}</td>
<td class="num" data-value="{{.Average}}">{{average .Average}}</td>
<td class="num" data-value="{{.Max}}">{{.Max}}</td>
<td class="num" data-value="{{.Imbalance}}">{{if .Uneven}}<span class="bad" title="Max is further above Avg than hashing puts a member's targets by chance">{{imbalance .Imbalance}}</span>{{else}}{{imbalance .Imbalance}}{{end}}</td>
{{- else}}
<td class="num" data-value="0"><span class="muted">–</span></td><td class="num" data-value="0"><span class="muted">–</span></td><td class="num" data-value="0"><span class="muted">–</span></td><td class="num" data-value="0"><span class="muted">–</span></td>
{{- end}}
<td class="sources">{{range $k, $s := .Sources}}{{if $k}}<br>{{end}}<span class="source">{{.Name}} {{template "result" .Result}}</span>{{else}}<span class="muted">listed</span>{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
{{- end}}{{end}}
{{- if .Members}}

<h2>Discovery sources</h2>
{{- if .Sources}}
<div class="scroll">
<table class="sources">
<thead><tr><th>Source</th><th>Feeds</th><th>Result</th><th class="num">Targets</th><th class="num">Refreshed every</th><th class="num">Last refresh</th><th>From</th></tr></thead>
<tbody>
{{- range .Sources}}
<tr>
<td class="id"><strong>{{if .Name}}{{.Name}}{{else}}<span class="muted">unnamed</span>{{end}}</strong><br><span class="muted kind">{{.Type}}</span></td>
<td class="owner">{{if eq .Kind "probe"}}<b class="swatch seg-probe"></b>probe targets<br><span class="muted kind">of the job</span> <a href="?job={{.Owner}}" title="the targets of the probe job {{.Owner}}">{{.Owner}}</a>{{else}}<b class="swatch seg-static"></b>static targets<br><span class="muted kind">of the entry</span> <a href="?entry={{.Owner}}" title="the static targets of the discovered_targets entry {{.Owner}}">{{.Owner}}</a>{{end}}</td>
<td class="result">{{template "result" .Result}}{{if eq .Result "failing"}}{{if .Targets}}, keeping its last answer{{end}}<br><span class="muted">{{.Error}}</span>{{end}}{{if .WeightFallbacks}}<br><span class="bad">{{.WeightFallbacks}} weight{{if ne .WeightFallbacks 1}}s{{end}} fell back</span> <span class="muted">on the owner's: their weight label gave no weight</span>{{end}}</td>
<td class="num">{{.Targets}}</td>
<td class="num">{{interval .RefreshIntervalSeconds}}</td>
<td class="num">{{if .LastRefresh}}<time class="ago" datetime="{{.LastRefresh}}" title="{{.LastRefresh}}">{{age .LastRefreshAgeSeconds}} ago</time>{{else}}<span class="muted">never</span>{{end}}</td>
<td>{{if eq .From "own"}}this instance's discovery{{else if eq .From "snapshot"}}the leader's snapshot{{else}}<span class="muted">–</span>{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
{{- else}}
<p class="muted">No discovery sources: the input files list every target.</p>
{{- end}}
{{- end}}
{{end}}`
