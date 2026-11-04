package allocator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
)

// The status page, at /, renders Status for a person, framed and styled as
// the exporter's pages are (pagekit): the same bar with its theme picker,
// the same icon, and the same refresh controls, which fetch the page's
// content again, ?format=fragment, and swap it in. Every value is escaped by
// html/template, and the script writes text with textContent alone.

// StatusPath is where the status is served as JSON.
const StatusPath = "/v1/status"

// statusDocs is where the page points for documentation.
const statusDocs = "https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/ALLOCATOR.md"

func (a *Allocator) serveStatus(w http.ResponseWriter, _ *http.Request) {
	body, err := json.Marshal(a.Status())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(append(body, '\n'))
}

func (a *Allocator) serveStatusPage(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "" && format != "html" && format != "fragment" {
		http.Error(w, "the format parameter is html or fragment; "+StatusPath+" serves the status as JSON", http.StatusBadRequest)
		return
	}
	page := statusPageOf(a.Status())
	t := statusPageTemplate
	if format == "fragment" {
		t = statusContentTemplate
	} else {
		page.Refresh = pagekit.NewRefreshControl(0, r.URL.Query().Get("refresh"))
	}
	var body bytes.Buffer
	if err := t.Execute(&body, page); err != nil {
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

// statusPage is what the page's template renders: the status, with the
// spread laid out for its table and bars.
type statusPage struct {
	Status
	Refresh pagekit.RefreshControl
	Docs    string
	View    spreadView
}

// spreadView is the spread as the table shows it: a row of each member,
// with its bar, and rows of the total, the average and the imbalance, each
// with a cell of each probe job.
type spreadView struct {
	Jobs                      []string
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
	Jobs                                []float64
	StaticPct, DrainingPct, ProbePct    float64
	StaticText, DrainingText, ProbeText string
}

func statusPageOf(s Status) statusPage {
	page := statusPage{Status: s, Docs: statusDocs}
	sp := s.Spread
	if sp == nil {
		return page
	}
	v := spreadView{Jobs: sp.Jobs, Shown: len(sp.Members) > 0}
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
		r := spreadRow{Member: name, Static: c.Static, Draining: c.Draining, Probe: c.Probe, Total: c.Total}
		for _, job := range sp.Jobs {
			r.Jobs = append(r.Jobs, c.ProbeJobs[job])
		}
		return r
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

// age writes seconds as an age reads, in whole seconds, minutes and hours.
func age(s float64) string {
	d := time.Duration(s * float64(time.Second)).Truncate(time.Second)
	if d < time.Second {
		return "0s"
	}
	return d.String()
}

// shortGeneration is a generation's first 8 characters, which tell
// generations apart on the page; its title has the whole.
func shortGeneration(g string) string {
	if len(g) > 8 {
		return g[:8]
	}
	return g
}

var statusFuncs = template.FuncMap{"count": count, "average": average, "imbalance": imbalance, "age": age, "gen": shortGeneration}

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
code.gen, code.flag { white-space: nowrap; }
p.notice { margin: 16px 0 0; padding: 10px 14px; border: 1px solid var(--line); border-left: 4px solid var(--accent); border-radius: 6px; background: var(--panel); }
:root { --bar-static: #c2410c; --bar-probe: #2563eb; }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { --bar-static: #e26a35; --bar-probe: #4d8fea; } }
:root[data-theme="dark"] { --bar-static: #e26a35; --bar-probe: #4d8fea; }
td.bar { width: 32%; min-width: 120px; }
/* On the first line of the row, beside the counts, however the member's
   id wraps: 12px centred in a 21px line. */
.bar-track { position: relative; display: flex; gap: 2px; height: 12px; margin-top: 5px; }
.bar-track span { display: block; height: 100%; min-width: 0; }
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
`

// statusChrome is the page's bar, the exporter's in its shape (pagekit),
// with the allocator's own links.
const statusChrome = `<header class="site"><nav aria-label="The allocator's pages">
<span class="home" aria-current="page">Prometheus Universal Exporter · allocator</span>
<span class="pages"><a href="v1/status">JSON</a><a href="metrics">Metrics</a><a href="{{.Docs}}">Documentation</a>` + pagekit.ThemePicker + `</span>
</nav></header>`

var statusContentTemplate = template.Must(template.New("content").Funcs(statusFuncs).Parse(statusContent))

var statusPageTemplate = template.Must(template.Must(template.New("status").Funcs(statusFuncs).Parse(statusContent)).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Status · pue-allocator</title>
` + pagekit.Icon + "\n" + pagekit.ThemeHead + `
<style>` + pagekit.Style + pagekit.RefreshStyle + statusStyle + `</style>
{{- if .Refresh.Seconds}}
<noscript><meta http-equiv="refresh" content="{{.Refresh.Seconds}}"></noscript>
{{- end}}
</head>
<body>
<main>
` + statusChrome + `
<h1>Allocator status</h1>
<p class="meta">Version {{.Copy.Version}}, revision {{.Copy.Revision}}</p>
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
<div id="status-content">
{{- template "content" .}}
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
  async function refresh() {
    if (inFlight) return;
    inFlight = true;
    const url = new URL(location.href);
    url.searchParams.set("format", "fragment");
    url.hash = "";
    try {
      const response = await fetch(url, {credentials: "same-origin", cache: "no-store"});
      if (!response.ok) throw new Error("HTTP " + response.status);
      content.innerHTML = await response.text();
      refreshedAt = Date.now();
      asOf();
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
`))

// statusContent is the page's content, which a refresh swaps in.
const statusContent = `{{define "content"}}
<p class="meta" id="as-of">As of <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.</p>
{{- if .Partial}}{{with .Peers}}
<p class="notice">This copy {{if eq .Role "follower"}}follows the leader, <strong>{{.Leader}}</strong>{{else}}has not yet chosen its leader{{end}}, and runs with <code class="flag">--peers.leader-only</code>: the members, the spread of the targets and the discovery sources are on the leader's page, and in the leader's metrics. Below are this copy and its peers.</p>
{{- end}}{{end}}

<h2>This copy</h2>
<table class="facts">
<tr><th>Address</th><td>{{.Copy.Address}}</td></tr>
<tr><th>Instance</th><td><code>{{.Copy.Instance}}</code></td></tr>
<tr><th>Started</th><td>{{.Copy.StartedAt}} <span class="muted">(up {{age .Copy.UptimeSeconds}})</span></td></tr>
<tr><th>Ready</th><td>{{if .Copy.Ready}}<span class="good">ready</span>{{else}}<span class="bad">shutting down</span>{{end}}</td></tr>
<tr><th>Peers</th><td>{{with .Peers}}{{if .LeaderOnly}}<span class="muted">(leader only)</span> {{end}}{{if eq .Role "leader"}}<span class="good">leader</span>{{if gt .LiveCopies 1}} of {{.LiveCopies}} live copies{{else}}, the only live copy{{end}}{{else if eq .Role "follower"}}follower of {{.Leader}}{{else}}<span class="muted">choosing the leader</span>{{end}}{{else}}<span class="muted">off: this copy asks its discovery sources itself (no <code class="flag">--peers</code>)</span>{{end}}</td></tr>
</table>
{{- with .Peers}}

<h2>Peers</h2>
<p>{{if eq .Role "leader"}}This copy leads: it asks the discovery sources, and the other copies take what it finds.{{else if eq .Role "follower"}}This copy follows {{.Leader}}: it takes the leader's discovered targets from its snapshot.{{else}}This copy has not yet checked its peers.{{end}} {{.LiveCopies}} live cop{{if eq .LiveCopies 1}}y{{else}}ies{{end}}, {{.LeaderChanges}} leader change{{if ne .LeaderChanges 1}}s{{end}}.{{if not .AddressKnown}} <span class="bad">This copy does not know its own address: list every copy in <code class="flag">--peers</code>, or give <code class="flag">--peers.self</code>.</span>{{end}}</p>
<div class="scroll">
<table>
<thead><tr><th>Copy</th><th>Role</th><th>Live</th><th>Discovering</th><th>Address known</th><th class="num">Last check</th><th class="num">Failures</th></tr></thead>
<tbody>
{{- range .Copies}}
<tr>
<td class="id">{{.Address}}{{if .Self}} <span class="muted">(this copy)</span>{{end}}{{if .Instance}}<br><span class="muted">instance</span> <code>{{.Instance}}</code>{{end}}</td>
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
<tr><th>Leader's targets</th><td>{{if eq .Role "leader"}}this copy's own discovery{{else if .HoldsLeaderState}}<span class="good">held</span>, the snapshot fetched {{age .SnapshotAgeSeconds}} ago{{else if .SnapshotTaken}}<span class="bad">not held</span>: the input files differ from the leader's in their discovery sources, last agreed {{age .SnapshotAgeSeconds}} ago{{else}}<span class="bad">not yet fetched</span> ({{age .SnapshotAgeSeconds}} since the start){{end}}</td></tr>
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
<p>{{if .Formed}}{{.InList}} member{{if ne .InList 1}}s{{end}} in the member list, {{.Live}} live.{{else}}<span class="bad">The member list has not formed yet</span>: no bundle or service discovery answer is served until it does. {{.Live}} live.{{end}}</p>
{{- if .List}}
<div class="scroll">
<table>
<thead><tr><th>Member</th><th>Address</th><th class="num">Last heartbeat</th><th>Bundle in force</th><th>State</th><th class="num">Draining</th></tr></thead>
<tbody>
{{- range .List}}
<tr>
<td class="id"><strong>{{.ID}}</strong></td>
<td>{{.Address}}</td>
<td class="num">{{age .LastHeartbeatAgeSeconds}} ago</td>
<td>{{if .UpToDate}}<code class="gen" title="{{.Generation}}">{{gen .Generation}}</code> <span class="good">current</span>{{else}}{{if .Generation}}<code class="gen" title="{{.Generation}}">{{gen .Generation}}</code>{{else}}<span class="muted">none</span>{{end}}{{if .Current}} <span class="bad">→</span> <code class="gen" title="{{.Current}}">{{gen .Current}}</code>{{end}}{{end}}{{if .Refused}}<br><span class="bad">refused</span> <code class="gen" title="{{.Refused}}">{{gen .Refused}}</code>{{end}}</td>
<td>{{if .ShuttingDown}}<span class="bad">shutting down</span>{{else if and .Live .InList}}<span class="good">in the list</span>{{else if .Live}}joining{{else if .InList}}<span class="bad">silent</span>, leaving{{else}}<span class="muted">gone</span>{{end}}</td>
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
<thead><tr><th>Member</th><th class="num">Static</th><th class="num">Draining</th>{{range .Jobs}}<th class="num" title="probe job {{.}}">{{.}}</th>{{end}}<th class="num">Probe</th><th class="num">Total</th><th>Share</th></tr></thead>
<tbody>
{{- $avg := .AveragePct}}
{{- range .Members}}
<tr>
<td class="id"><strong>{{.Member}}</strong></td>
<td class="num">{{count .Static}}</td>
<td class="num">{{count .Draining}}</td>
{{- range .Jobs}}<td class="num">{{count .}}</td>{{end}}
<td class="num">{{count .Probe}}</td>
<td class="num"><strong>{{count .Total}}</strong></td>
<td class="bar"><div class="bar-track" role="img" aria-label="{{.StaticText}}; {{.DrainingText}}; {{.ProbeText}}">{{if .Static}}<span class="seg-static" style="width: {{.StaticPct}}%" title="{{.StaticText}}"></span>{{end}}{{if .Draining}}<span class="seg-draining" style="width: {{.DrainingPct}}%" title="{{.DrainingText}}"></span>{{end}}{{if .Probe}}<span class="seg-probe" style="width: {{.ProbePct}}%" title="{{.ProbeText}}"></span>{{end}}<i class="avg" style="left: {{$avg}}%" title="the average per member"></i></div></td>
</tr>
{{- end}}
</tbody>
<tfoot>
{{- with .Total}}
<tr><td>{{.Member}}</td><td class="num">{{count .Static}}</td><td class="num">{{count .Draining}}</td>{{range .Jobs}}<td class="num">{{count .}}</td>{{end}}<td class="num">{{count .Probe}}</td><td class="num">{{count .Total}}</td><td></td></tr>
{{- end}}
{{- with .Average}}
<tr><td>{{.Member}}</td><td class="num">{{average .Static}}</td><td class="num">{{average .Draining}}</td>{{range .Jobs}}<td class="num">{{average .}}</td>{{end}}<td class="num">{{average .Probe}}</td><td class="num">{{average .Total}}</td><td></td></tr>
{{- end}}
{{- with .Imbalance}}
<tr><td>{{.Member}}</td><td class="num">{{imbalance .Static}}</td><td class="num">{{imbalance .Draining}}</td>{{range .Jobs}}<td class="num">{{imbalance .}}</td>{{end}}<td class="num">{{imbalance .Probe}}</td><td class="num">{{imbalance .Total}}</td><td></td></tr>
{{- end}}
</tfoot>
</table>
</div>
<p class="legend"><span><b class="seg-static"></b>static, owned</span><span><b class="seg-draining"></b>static, draining</span><span><b class="seg-probe"></b>probe, routed</span><span><b class="avg"></b>average per member</span><span>Bars run to the largest total.</span></p>
{{- end}}{{end}}
{{- if .Members}}

<h2>Discovery sources</h2>
{{- if .Sources}}
<div class="scroll">
<table>
<thead><tr><th>Source</th><th>Owner</th><th>Result</th><th class="num">Targets</th><th class="num">Last refresh</th><th>From</th></tr></thead>
<tbody>
{{- range .Sources}}
<tr>
<td class="id"><strong>{{if .Name}}{{.Name}}{{else}}<span class="muted">unnamed</span>{{end}}</strong><br><span class="muted">{{.Type}}</span></td>
<td>{{.Owner}}<br><span class="muted">{{if eq .Kind "probe"}}probe job{{else}}discovered_targets{{end}}</span></td>
<td>{{if eq .Result "ok"}}<span class="good">ok</span>{{else if eq .Result "failing"}}<span class="bad">failing</span>{{if .Targets}}, keeping its last answer{{end}}<br><span class="muted">{{.Error}}</span>{{else}}<span class="muted">not answered yet</span>{{end}}</td>
<td class="num">{{.Targets}}</td>
<td class="num">{{if .LastRefresh}}{{age .LastRefreshAgeSeconds}} ago{{else}}<span class="muted">never</span>{{end}}</td>
<td>{{if eq .From "own"}}this copy's discovery{{else if eq .From "snapshot"}}the leader's snapshot{{else}}<span class="muted">–</span>{{end}}</td>
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
