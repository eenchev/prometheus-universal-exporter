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
	// Member is the member's view and Job the probe job's, nil elsewhere.
	Member *memberPage
	Job    *jobPage
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
// probe jobs have a table of their own, a row each.
type spreadView struct {
	Members                   []spreadRow
	Total, Average, Imbalance spreadRow
	// AveragePct is where the average total falls on the bars, which run
	// from 0 to the largest total of a member.
	AveragePct float64
	Shown      bool
}

type spreadRow struct {
	Member                              string
	Static, Draining, Probe, Total      float64
	StaticPct, DrainingPct, ProbePct    float64
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
		most = max(most, m.Total)
	}
	pct := func(n float64) float64 {
		if most == 0 {
			return 0
		}
		return math.Round(n/most*1000) / 10
	}
	row := func(name string, c SpreadCounts) spreadRow {
		return spreadRow{Member: name, Static: c.Static, Draining: c.Draining, Probe: c.Probe, Total: c.Total}
	}
	for _, m := range sp.Members {
		r := row(m.Member, m.SpreadCounts)
		r.StaticPct, r.DrainingPct, r.ProbePct = pct(m.Static), pct(m.Draining), pct(m.Probe)
		r.StaticText = fmt.Sprintf("%s: %s static targets owned", m.Member, count(m.Static))
		r.DrainingText = fmt.Sprintf("%s: %s static targets draining", m.Member, count(m.Draining))
		r.ProbeText = fmt.Sprintf("%s: %s probe targets routed", m.Member, count(m.Probe))
		v.Members = append(v.Members, r)
	}
	v.Total = row("Total", sp.Total)
	v.Average = row("Average per member", sp.Average)
	v.Imbalance = row("Imbalance, most ÷ average", sp.Imbalance)
	v.AveragePct = pct(sp.Average.Total)
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

// age writes seconds, an age or an interval, in whole seconds, minutes and
// hours, as the pages write a duration (pagekit.Duration): 1m, not 1m0s.
func age(s float64) string {
	d := time.Duration(s * float64(time.Second)).Truncate(time.Second)
	if d < time.Second {
		return "0s"
	}
	return pagekit.Duration(d)
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

var statusFuncs = template.FuncMap{"count": count, "average": average, "imbalance": imbalance, "age": age, "duration": pagekit.DurationText, "gen": shortGeneration,
	"listNote": listNote, "match": matchText, "jobMatch": jobMatch, "sourceNames": sourceNames}

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
/* A source's name and kind, its owner and where its targets came from are
   short phrases that read badly broken over lines; the result, with its
   error, takes the width that is left. */
table.sources td:first-child, table.sources td:nth-child(2), table.sources td:last-child { white-space: nowrap; }
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
@media (max-width: 640px) { td.bar { min-width: 90px; } }
/* A sortable table's headings are buttons, marked as the exporter's debug
   page marks its series table's: ↕ for a column not sorted by, ↑ or ↓ for
   the one sorted by. A number's column has the mark before its name, so
   the name ends where the column's numbers end. */
table.sortable th button { padding: 0; border: 0; background: transparent; color: var(--fg); font: inherit; font-weight: 600; cursor: pointer; white-space: nowrap; }
table.sortable th:not(.num) button::after { content: " \2195"; color: var(--muted); }
table.sortable th.num button::before { content: "\2195 "; color: var(--muted); }
table.sortable th[aria-sort="ascending"]:not(.num) button::after { content: " \2191"; color: var(--accent); }
table.sortable th[aria-sort="descending"]:not(.num) button::after { content: " \2193"; color: var(--accent); }
table.sortable th.num[aria-sort="ascending"] button::before { content: "\2191 "; color: var(--accent); }
table.sortable th.num[aria-sort="descending"] button::before { content: "\2193 "; color: var(--accent); }
td.sources { min-width: 10em; }
td.sources span.source { white-space: nowrap; }
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
{{if or .Member .Job}}<a class="home" href="./">Prometheus Universal Exporter · Target Allocator</a>{{else}}<span class="home" aria-current="page">Prometheus Universal Exporter · Target Allocator</span>{{end}}
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
)

// statusFrame frames a page's content (pageFrame).
var statusFrame = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{with .Member}}Member {{.Member}}{{else}}{{with .Job}}Probe job {{.Job}}{{else}}Status{{end}}{{end}} · pue-allocator</title>
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
{{- else}}
<label for="target-filter">Filter: target, member or label</label>
{{- end}}
<span class="filter-row"><input id="target-filter" name="filter" type="search" value="{{.Text}}" autocomplete="off" spellcheck="false"><button type="submit">Search all</button></span>
{{- if .SourceLabelsBox}}
<label class="check" for="source-labels"><input id="source-labels" name="meta" value="1" type="checkbox"{{if .SourceLabels}} checked{{end}}>Show source labels <span class="muted">(names starting with <code>__</code>)</span></label>
{{- end}}
<span class="muted" id="filter-hint">Typing filters the rows shown; Search all asks the allocator, which looks through every target of the {{.Param}}{{if gt .Limit 0}}, beyond the {{.Limit}} a list shows{{end}}. {{if eq .Param "member"}}Both match a target's name, target, collector, labels, probe job and the member it moved to.{{else}}Both match a target, the member it is routed to and the labels shown.{{end}}</span>
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
// hidden tab is not refreshed, and a refresh still waiting is not
// overlapped. Without JavaScript the button reloads the page, the form puts
// the interval in the URL, and the meta refresh reloads it.
(() => {
  const $ = (id) => document.getElementById(id);
  const content = $("status-content"), every = $("refresh-every"), state = $("refresh-state");
  let timer = 0, inFlight = false, refreshedAt = Date.now(), dataAt = "";
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
  // The status page's probe jobs table sorts by the column whose heading
  // is clicked, the other way at a second click, numbers by their value
  // (data-value) and text as it reads, a tie in file order (data-order);
  // and it filters its rows as its filter is typed, matching in any case
  // the texts each row carries in data-match. Both are in the content,
  // which a refresh replaces, so the order chosen and the text typed are
  // kept here and put back on the new table, the caret where it was.
  let jobSort = -1, jobDown = false, jobFilter = "";
  function arrangeJobs() {
    const table = content.querySelector("table#probe-jobs"), box = $("jobs-filter");
    if (!table) return;
    if (box) {
      box.value = jobFilter;
      box.form.hidden = false;
    }
    const heads = table.tHead.rows[0].cells, body = table.tBodies[0];
    const numeric = jobSort >= 0 && heads[jobSort].dataset.sort === "num";
    const key = (row) => {
      const cell = row.cells[jobSort];
      return numeric ? Number(cell.dataset.value) : cell.textContent.trim();
    };
    const rows = Array.from(body.rows).sort((x, y) => {
      let c = 0;
      if (jobSort >= 0) c = numeric ? key(x) - key(y) : key(x).localeCompare(key(y));
      if (jobDown) c = -c;
      return c || Number(x.dataset.order) - Number(y.dataset.order);
    });
    for (const row of rows) body.appendChild(row);
    for (let i = 0; i < heads.length; i++) heads[i].setAttribute("aria-sort", i === jobSort ? (jobDown ? "descending" : "ascending") : "none");
    const needle = jobFilter.trim().toLowerCase();
    let shown = 0;
    for (const row of rows) {
      row.hidden = needle !== "" && !(row.dataset.match || "").toLowerCase().includes(needle);
      if (!row.hidden) shown++;
    }
    const count = content.querySelector('[data-rows-of="probe-jobs"]');
    if (count) count.textContent = needle === "" ? "" : shown === 0 ? "No probe job holds what is typed in the filter." : shown + " of the " + rows.length + " probe jobs match.";
  }
  content.addEventListener("click", (event) => {
    const button = event.target.closest("table#probe-jobs th button");
    if (!button) return;
    const column = button.parentElement.cellIndex;
    jobDown = column === jobSort ? !jobDown : false;
    jobSort = column;
    arrangeJobs();
  });
  content.addEventListener("input", (event) => {
    if (event.target.id !== "jobs-filter") return;
    jobFilter = event.target.value;
    arrangeJobs();
  });
  content.addEventListener("submit", (event) => {
    if (event.target.id === "jobs") event.preventDefault();
  });
  arrangeJobs();
  async function refresh() {
    if (inFlight) return;
    inFlight = true;
    const url = new URL(location.href);
    url.searchParams.set("format", "fragment");
    url.hash = "";
    try {
      const response = await fetch(url, {credentials: "same-origin", cache: "no-store"});
      if (!response.ok) throw new Error("HTTP " + response.status);
      const typing = document.activeElement && document.activeElement.id === "jobs-filter" ? document.activeElement : null;
      const caret = typing && [typing.selectionStart, typing.selectionEnd];
      content.innerHTML = await response.text();
      refreshedAt = Date.now();
      asOf();
      filterRows();
      arrangeJobs();
      if (typing && $("jobs-filter")) {
        $("jobs-filter").focus();
        $("jobs-filter").setSelectionRange(caret[0], caret[1]);
      }
    } catch (error) {
      state.className = "bad";
      state.textContent = "Refresh failed at " + clock(Date.now()) + " (" + error.message + "); showing the data as of " + clock(Date.parse(dataAt) || refreshedAt);
    } finally {
      inFlight = false;
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
{{- define "heartbeat"}}{{if .Late}}<span class="bad" title="a heartbeat is overdue">{{age .LastHeartbeatAgeSeconds}} ago</span>{{else}}{{age .LastHeartbeatAgeSeconds}} ago{{end}}{{end}}
{{- define "bundle"}}{{if .UpToDate}}<code class="gen" title="{{.Generation}}">{{gen .Generation}}</code> <span class="good">current</span>{{else}}{{if .Generation}}<code class="gen" title="{{.Generation}}">{{gen .Generation}}</code>{{else}}<span class="muted">none</span>{{end}}{{if .Current}} <span class="bad">→</span> <code class="gen" title="{{.Current}}">{{gen .Current}}</code>{{end}}{{end}}{{if .Refused}}<br><span class="bad">refused</span> <code class="gen" title="{{.Refused}}">{{gen .Refused}}</code>{{end}}{{end}}
{{- define "state"}}{{if .ShuttingDown}}<span class="bad">shutting down</span>{{else if and .Live .InList}}<span class="good">in the list</span>{{else if .Live}}joining{{else if .InList}}<span class="bad">silent</span>, leaving{{else}}<span class="muted">gone</span>{{end}}{{end}}
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
<p class="note muted" data-rows-of="{{.Table}}"></p>
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
<tr><th>Started</th><td>{{.Instance.StartedAt}} <span class="muted">(up {{age .Instance.UptimeSeconds}})</span></td></tr>
<tr><th>Input files</th><td>{{if .Instance.WatchIntervalSeconds}}read again every {{age .Instance.WatchIntervalSeconds}} for changes{{else}}<span class="muted">read once, at the start</span> (<code class="flag">--config.watch-interval=0</code>){{end}}</td></tr>
<tr><th>Ready</th><td>{{if .Instance.Ready}}<span class="good">ready</span>{{else}}<span class="bad">shutting down</span>{{end}}</td></tr>
<tr><th>Peers</th><td>{{with .Peers}}{{if .LeaderOnly}}<span class="muted">(leader only)</span> {{end}}{{if eq .Role "leader"}}<span class="good">leader</span>{{if gt .LiveInstances 1}} of {{.LiveInstances}} live instances{{else}}, the only live instance{{end}}{{else if eq .Role "follower"}}follower of {{.Leader}}{{else}}<span class="muted">choosing the leader</span>{{end}}{{else}}<span class="muted">off: this instance asks its discovery sources itself (no <code class="flag">--peers</code>)</span>{{end}}</td></tr>
</table>
{{- with .Peers}}

<h2>Peers</h2>
<p>{{if eq .Role "leader"}}This instance leads: it asks the discovery sources, and the other instances take what it finds.{{else if eq .Role "follower"}}This instance follows {{.Leader}}: it takes the leader's discovered targets from its snapshot.{{else}}This instance has not yet checked its peers.{{end}} {{.LiveInstances}} live instance{{if ne .LiveInstances 1}}s{{end}}, {{.LeaderChanges}} leader change{{if ne .LeaderChanges 1}}s{{end}}.{{if not .AddressKnown}} <span class="bad">This instance does not know its own address: list every instance in <code class="flag">--peers</code>, or give <code class="flag">--peers.self</code>.</span>{{end}}</p>
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
<td class="num">{{if .Self}}<span class="muted">–</span>{{else if .LastCheck}}{{age .LastCheckAgeSeconds}} ago{{else}}<span class="muted">never</span>{{end}}</td>
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
<p>{{if .Formed}}{{.InList}} member{{if ne .InList 1}}s{{end}} in the member list, {{.Live}} live.{{else}}<span class="bad">The member list has not formed yet</span>: no bundle or service discovery answer is served until it does. {{.Live}} live.{{end}} The replicas heartbeat every {{age .HeartbeatIntervalSeconds}}; a member not heard from for {{age .MemberTTLSeconds}} stops being live. A changed member list holds for {{age .SettleSeconds}} before targets move.</p>
{{- if .List}}
<div class="scroll">
<table>
<thead><tr><th>Member</th><th>Address</th><th class="num">Last heartbeat</th><th>Bundle in force</th><th>State</th><th class="num">Draining</th></tr></thead>
<tbody>
{{- range .List}}
<tr>
<td class="id"><a href="?member={{.ID}}" title="the targets of {{.ID}}"><strong>{{.ID}}</strong></a></td>
<td>{{.Address}}</td>
<td class="num">{{template "heartbeat" .}}</td>
<td>{{template "bundle" .}}</td>
<td>{{template "state" .}}</td>
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
<p>{{count .Total.Static}} static target{{if ne .Total.Static 1.0}}s{{end}}{{if .Total.Draining}}, {{count .Total.Draining}} draining,{{end}} and {{count .Total.Probe}} probe target{{if ne .Total.Probe 1.0}}s{{end}} over {{len .Members}} member{{if ne (len .Members) 1}}s{{end}}: {{average .Average.Total}} per member on average, the most {{imbalance .Imbalance.Total}} the average.</p>
<div class="scroll">
<table class="spread">
<thead><tr><th>Member</th><th class="num">Static</th><th class="num">Draining</th><th class="num">Probe</th><th class="num">Total</th><th>Share</th></tr></thead>
<tbody>
{{- $avg := .AveragePct}}
{{- range .Members}}
<tr>
<td class="id"><a href="?member={{.Member}}" title="the targets of {{.Member}}"><strong>{{.Member}}</strong></a></td>
<td class="num">{{count .Static}}</td>
<td class="num">{{count .Draining}}</td>
<td class="num">{{count .Probe}}</td>
<td class="num"><strong>{{count .Total}}</strong></td>
<td class="bar"><div class="bar-track" role="img" aria-label="{{.StaticText}}; {{.DrainingText}}; {{.ProbeText}}">{{if .Static}}<span class="seg-static" style="width: {{.StaticPct}}%" title="{{.StaticText}}"></span>{{end}}{{if .Draining}}<span class="seg-draining" style="width: {{.DrainingPct}}%" title="{{.DrainingText}}"></span>{{end}}{{if .Probe}}<span class="seg-probe" style="width: {{.ProbePct}}%" title="{{.ProbeText}}"></span>{{end}}<i class="avg" style="left: {{$avg}}%" title="the average per member"></i></div></td>
</tr>
{{- end}}
</tbody>
<tfoot>
{{- with .Total}}
<tr><td>{{.Member}}</td><td class="num">{{count .Static}}</td><td class="num">{{count .Draining}}</td><td class="num">{{count .Probe}}</td><td class="num">{{count .Total}}</td><td></td></tr>
{{- end}}
{{- with .Average}}
<tr><td>{{.Member}}</td><td class="num">{{average .Static}}</td><td class="num">{{average .Draining}}</td><td class="num">{{average .Probe}}</td><td class="num">{{average .Total}}</td><td></td></tr>
{{- end}}
{{- with .Imbalance}}
<tr><td>{{.Member}}</td><td class="num">{{imbalance .Static}}</td><td class="num">{{imbalance .Draining}}</td><td class="num">{{imbalance .Probe}}</td><td class="num">{{imbalance .Total}}</td><td></td></tr>
{{- end}}
</tfoot>
</table>
</div>
<p class="legend"><span><b class="seg-static"></b>static, owned</span><span><b class="seg-draining"></b>static, draining</span><span><b class="seg-probe"></b>probe, routed</span><span><b class="avg"></b>average per member</span><span>Bars run to the largest total.</span></p>
{{- end}}{{end}}
{{- with .Spread}}{{if .Jobs}}

<h2>Probe jobs</h2>
<p>{{len .Jobs}} probe job{{if ne (len .Jobs) 1}}s{{end}}. A job's targets are spread over the members its probes are routed among: Min, Avg and Max count the targets of the job a member has, and Imbalance is Max ÷ Avg, <span class="bad">marked</span> where Max is further above Avg than hashing puts a member's targets by chance: by 3 targets or more, and by 3 standard deviations of a member's count or more, √(Targets × (1 − 1/Members) ÷ Members).</p>
<form class="filter jobs" id="jobs" role="search" hidden>
<label for="jobs-filter">Filter: job, collector or source</label>
<input id="jobs-filter" type="search" autocomplete="off" spellcheck="false">
<span class="muted">Click a column's heading to sort by it, again for the other way.</span>
</form>
<p class="note muted" data-rows-of="probe-jobs"></p>
<div class="scroll">
<table class="spread sortable" id="probe-jobs">
<thead><tr><th aria-sort="none" data-sort="text"><button type="button">Job</button></th><th aria-sort="none" data-sort="text"><button type="button">Collector</button></th><th class="num" aria-sort="none" data-sort="num"><button type="button">Targets</button></th><th class="num" aria-sort="none" data-sort="num"><button type="button">Members</button></th><th class="num" aria-sort="none" data-sort="num"><button type="button">Min</button></th><th class="num" aria-sort="none" data-sort="num"><button type="button">Avg</button></th><th class="num" aria-sort="none" data-sort="num"><button type="button">Max</button></th><th class="num" aria-sort="none" data-sort="num"><button type="button">Imbalance</button></th><th aria-sort="none" data-sort="text"><button type="button">Sources</button></th></tr></thead>
<tbody>
{{- range $i, $j := .Jobs}}
<tr data-order="{{$i}}" data-match="{{jobMatch $j}}">
<td class="id"><a href="?job={{.Job}}" title="the targets of the probe job {{.Job}}"><strong>{{.Job}}</strong></a></td>
<td>{{.Collector}}</td>
<td class="num" data-value="{{.Targets}}">{{.Targets}}</td>
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
<thead><tr><th>Source</th><th>Owner</th><th>Result</th><th class="num">Targets</th><th class="num">Refreshed every</th><th class="num">Last refresh</th><th>From</th></tr></thead>
<tbody>
{{- range .Sources}}
<tr>
<td class="id"><strong>{{if .Name}}{{.Name}}{{else}}<span class="muted">unnamed</span>{{end}}</strong><br><span class="muted">{{.Type}}</span></td>
<td>{{.Owner}}<br><span class="muted">{{if eq .Kind "probe"}}probe job{{else}}discovered_targets{{end}}</span></td>
<td>{{template "result" .Result}}{{if eq .Result "failing"}}{{if .Targets}}, keeping its last answer{{end}}<br><span class="muted">{{.Error}}</span>{{end}}</td>
<td class="num">{{.Targets}}</td>
<td class="num">{{age .RefreshIntervalSeconds}}</td>
<td class="num">{{if .LastRefresh}}{{age .LastRefreshAgeSeconds}} ago{{else}}<span class="muted">never</span>{{end}}</td>
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
