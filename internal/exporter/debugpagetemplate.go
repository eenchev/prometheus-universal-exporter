package exporter

// The debug page (debugpage.go): a collector, the playground, one written in
// an editor, or one chosen from the configuration in force, the response it
// is run on — pasted, or at --web.debug=full a target fetched through a
// configured collector or a static target's scrape — and what the run gave:
// the series a probe would answer, as a table and as text, the requests, the response, the stages,
// the metric lint's advice, what was logged and the whole debug report. It
// runs when Run is pressed, and only then; a run cancels the one before it.
// The collectors page links each collector here as ?collector=<name>, with
// &target=<target> at full, and the page starts from what the link names;
// at full ?static_target=<name> opens it on that static target, its
// collector chosen.

type debugPage struct {
	// Chrome is the bar at the top of the page (pagechrome.go).
	Chrome pageChrome
	// Full offers fetching a target and scraping a static target,
	// --web.debug=full.
	Full bool
	// Collectors are the configured collectors, which can be run by name.
	Collectors []debugPageCollector
	// ProbeTimeoutSeconds is the timeout a run starts with, sent as the
	// scrape timeout Prometheus would send.
	ProbeTimeoutSeconds  int
	Example, ExampleBody string
	// Chosen is the configured collector the page's address names, which
	// it opens with, and ChosenYAML that collector as the page shows it
	// (debugcollector.go).
	Chosen, ChosenYAML string
	// StaticTargets are the static targets scraped through Chosen, which
	// the page offers to scrape, StaticTarget the one the page's address
	// names, and StaticNote why none is offered, at full.
	StaticTargets []debugStaticTarget
	StaticTarget  string
	StaticNote    string
	// Forms are what the page asks a run of each collector to fill, and
	// Form that of the collector it opens with: Chosen, or the playground.
	Forms debugForms
	Form  debugForm
	// BasicAuth is whether the exporter asks for web.basic_auth, which the
	// probe's Copy as says to give.
	BasicAuth bool
}

// debugForms are the forms of the playground, of a playground of each
// request type the build has, by its name, and of each configured
// collector by name (debugForm), which the page's script reads.
type debugForms struct {
	Playground debugForm            `json:"playground"`
	Types      map[string]debugForm `json:"types"`
	Collectors map[string]debugForm `json:"collectors"`
}

// debugExample and debugExampleBody are what the page starts with: a
// collector and a response it reads.
const debugExample = `name: example
request:
  type: http
decoder:
  type: json
transform:
  type: jq
metrics:
  - name: room_temperature_celsius
    description: Temperature of each room
    type: gauge
    items: .rooms[]
    expression: .celsius
    labels:
      - name: room
        expression: .name
`

const debugExampleBody = `{"rooms": [{"name": "kitchen", "celsius": 21.5}, {"name": "attic", "celsius": 17}]}
`

var debugPageTemplate = pageTemplate("debug", `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Debug · Prometheus Universal Exporter</title>
`+pageIcon+"\n"+themeHead+`
<style>`+pageStyle+`
main { max-width: 1200px; }
.note { color: var(--muted); font-size: 13px; margin: 4px 0 10px; }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 16px; margin-top: 16px; }
section.panel { border: 1px solid var(--line); border-radius: 10px; padding: 16px; background: var(--panel); min-width: 0; }
section.panel h2 { margin: 0 0 8px; font-size: 17px; }
label { display: block; font-size: 13px; margin: 8px 0 2px; }
input, select, textarea { width: 100%; padding: 6px 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--fg); font: inherit; font-size: 14px; }
textarea { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; resize: vertical; tab-size: 2; }
textarea#collector { min-height: 360px; }
textarea[readonly] { background: var(--code); }
.collector-head { display: flex; justify-content: space-between; align-items: flex-end; gap: 8px; flex-wrap: wrap; }
.collector-head label { flex: 1 1 200px; }
.row { display: flex; align-items: flex-end; gap: 8px; flex-wrap: wrap; }
.row > div { flex: 1 1 160px; min-width: 0; }
.row > div.narrow { flex: 0 1 120px; }
.modes { display: flex; gap: 16px; flex-wrap: wrap; font-size: 14px; margin: 4px 0 8px; }
.modes label { display: inline-flex; align-items: center; gap: 6px; margin: 0; font-size: 14px; }
.modes input { width: auto; }
.actions { display: flex; align-items: flex-end; gap: 16px; flex-wrap: wrap; margin-top: 14px; }
.actions > div { flex: 0 1 140px; }
.actions button, .actions input { height: 36px; }
button { padding: 7px 16px; border: 1px solid var(--accent); border-radius: 6px; background: var(--accent); color: var(--on-accent); font: inherit; font-size: 14px; cursor: pointer; }
button.secondary { padding: 3px 12px; margin-top: 6px; background: transparent; color: var(--accent); font-size: 13px; }
[hidden] { display: none !important; }
.status { font-size: 13px; font-weight: 600; margin-bottom: 8px; }
.status.ok { color: var(--good); }
.status.failed { color: var(--bad); }
h3 { font-size: 14px; margin: 14px 0 4px; }
pre { margin: 0; padding: 10px; max-height: 420px; overflow: auto; background: var(--bg); border: 1px solid var(--line); border-radius: 6px; font-size: 12px; white-space: pre-wrap; overflow-wrap: anywhere; }
pre.error { color: var(--bad); }
ul.stages { margin: 0; padding-left: 18px; font-size: 13px; }
ul.stages .failed { color: var(--bad); }
details { margin-top: 14px; }
summary { cursor: pointer; font-size: 14px; font-weight: 600; }
.series-head { display: flex; justify-content: space-between; align-items: flex-end; gap: 8px; flex-wrap: wrap; margin: 14px 0 4px; }
.series-head h3 { margin: 0; }
.views { display: inline-flex; border: 1px solid var(--accent); border-radius: 6px; overflow: hidden; }
.views button { padding: 3px 12px; border: 0; border-radius: 0; background: transparent; color: var(--accent); font-size: 13px; }
.views button[aria-selected="true"] { background: var(--accent); color: var(--on-accent); }
.series-filters { margin-bottom: 6px; }
.series-filters label { margin-top: 0; }
.series-filters input, .series-filters select { height: 34px; }
.table-box { max-height: 520px; overflow: auto; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); }
table.series { border-collapse: collapse; width: 100%; font-size: 12px; }
table.series th, table.series td { padding: 4px 8px; border-bottom: 1px solid var(--line); text-align: left; vertical-align: top; }
table.series th { position: sticky; top: 0; background: var(--code); white-space: nowrap; z-index: 1; }
table.series th button { padding: 0; border: 0; background: transparent; color: var(--fg); font: inherit; font-weight: 600; cursor: pointer; }
table.series th button::after { content: " \2195"; color: var(--muted); }
table.series th[aria-sort="ascending"] button::after { content: " \2191"; color: var(--accent); }
table.series th[aria-sort="descending"] button::after { content: " \2193"; color: var(--accent); }
table.series td { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; overflow-wrap: break-word; max-width: 32em; }
table.series td.value { text-align: right; white-space: nowrap; }
table.series td.type { color: var(--muted); font-family: inherit; }
table.series td.lint { font-family: inherit; min-width: 9em; }
table.series details { margin: 0; }
table.series summary, .lint-unplaced summary { font-size: 12px; font-weight: 600; color: var(--accent); }
.lint-advice { margin: 4px 0 0; font-size: 12px; white-space: normal; }
.lint-unplaced { margin: 0 0 8px; padding-left: 18px; font-size: 13px; }
.kv-title { font-size: 13px; margin: 8px 0 2px; }
.kv-item + .kv-item { margin-top: 6px; }
.kv-row { display: flex; align-items: flex-start; gap: 6px; }
.kv-fields { display: flex; flex-wrap: wrap; gap: 6px; flex: 1 1 auto; min-width: 0; }
.kv-fields input { flex: 1 1 8em; width: auto; min-width: 0; height: 34px; }
.kv-fields input.kv-value { flex-grow: 2; }
.kv-row button.kv-remove { flex: 0 0 34px; width: 34px; height: 34px; padding: 0; margin: 0; border-color: var(--line); background: transparent; color: var(--muted); font-size: 18px; line-height: 1; }
.kv-row button.kv-remove:hover { color: var(--bad); border-color: var(--bad); }
.kv-fields input[aria-invalid="true"] { border-color: var(--bad); }
.kv-error { margin: 2px 0 0; color: var(--bad); font-size: 12px; }
.copy-as { margin-top: 14px; }
.copy-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.copy-row .kv-title { margin: 0; }
.copy-row button { margin: 0; }
.copy-row button[aria-pressed="true"] { background: var(--accent); color: var(--on-accent); }
.copy-row button:disabled { opacity: 0.5; cursor: not-allowed; }
.copy-as .note { margin: 6px 0 0; }
pre.copy-text { margin-top: 8px; max-height: 260px; white-space: pre; overflow-wrap: normal; }
.copy-done { color: var(--muted); font-size: 13px; }
.copy-done.copied { color: var(--good); }
</style>
</head>
<body>
<main{{if .BasicAuth}} data-basic-auth{{end}}>
{{template "chrome" .Chrome}}
<h1>Debug</h1>
<p class="meta">Run one collector — one of the configuration, or the playground, a collector written on the page as it is written under <code>collectors:</code> — and see the series a probe would answer. Nothing is put in force, cached or counted against a collector.</p>
<p class="note">The playground never runs Python, and one that names a file of the server's is refused; a configured collector runs as it is configured.{{if not .Full}} Debugging is at <code>edit</code>: collectors run on a pasted response only, and nothing is fetched. Probing a target needs <code>--web.debug=full</code>.{{end}}</p>
<noscript><p class="note">The debug page needs JavaScript.</p></noscript>
<div class="grid">
<section class="panel">
<h2>Collector</h2>
<label for="configured">Collector to run</label>
<select id="configured" data-chosen="{{.Chosen}}">
<option value="">Playground</option>
{{- if .Collectors}}
<optgroup label="Configured, run as configured">
{{- range .Collectors}}
<option value="{{.Name}}"{{if eq .Name $.Chosen}} selected{{end}}>{{.Name}} ({{.RequestType}})</option>
{{- end}}
</optgroup>
{{- end}}
</select>
<div class="collector-head">
<label for="collector" id="collector-label">{{if .Chosen}}The configuration of {{.Chosen}}, which runs as configured (read-only){{else}}Playground: the YAML of a collector, which runs{{end}}</label>
<button type="button" class="secondary" id="edit-copy"{{if not .Chosen}} hidden{{end}}>Edit a copy</button>
<button type="button" class="secondary" id="back-to-example" hidden>Back to the example</button>
</div>
<p class="note" id="collector-note"{{if not .Chosen}} hidden{{end}}>As its file writes it: credentials and file paths withheld, the keys it takes from its file's defaults marked. <em>Edit a copy</em> makes the playground a copy of it, which runs in its place and fetches a target through it.</p>
<textarea id="collector" spellcheck="false" autocomplete="off" wrap="off" data-example="{{.Example}}"{{if .Chosen}} readonly data-shown="{{.Chosen}}"{{end}}>{{if .Chosen}}{{.ChosenYAML}}{{else}}{{.Example}}{{end}}</textarea>
</section>
<section class="panel">
<h2>Response</h2>
{{- if .Full}}
<div class="modes" role="radiogroup" aria-label="Response">
<label><input type="radio" name="mode" value="paste" checked> Paste a response</label>
<label><input type="radio" name="mode" value="fetch" id="mode-fetch"{{if not .Chosen}} disabled{{end}}> Probe a target</label>
<label><input type="radio" name="mode" value="static" id="mode-static"{{if not .StaticTargets}} disabled{{end}}> Scrape a static target</label>
</div>
<p class="note" id="fetch-unavailable"{{if .Chosen}} hidden{{end}}>Choose a configured collector, or <em>Edit a copy</em> of one, to probe a target or scrape a static target.</p>
<p class="note" id="static-unavailable"{{if not .StaticNote}} hidden{{end}}>{{.StaticNote}}</p>
{{- end}}
<div data-mode="paste">
<div class="row">
<div id="content-type-part"{{if not .Form.ContentType}} hidden{{end}}><label for="content-type">Content-Type</label>
<input id="content-type" type="text" value="application/json" autocomplete="off"></div>
<div class="narrow" id="status-part"{{if not .Form.HTTPAnswer}} hidden{{end}}><label for="status">Status</label>
<input id="status" type="number" min="100" max="599" value="200"></div>
</div>
<div id="headers-part" role="group" aria-labelledby="headers-title"{{if not .Form.HTTPAnswer}} hidden{{end}}>
<div class="kv-title" id="headers-title">Other headers</div>
<div class="kv" id="headers-rows"></div>
<button type="button" class="secondary" id="headers-add">+ Add header</button>
</div>
<label for="body">Body</label>
<textarea id="body" rows="14" spellcheck="false" autocomplete="off" placeholder="{{.Form.Answer}}" data-example="{{.ExampleBody}}">{{if not .Chosen}}{{.ExampleBody}}{{end}}</textarea>
</div>
{{- if .Full}}
<div data-mode="fetch" hidden>
<p class="note">The target is probed as <code>/probe?debug=true</code> probes it: through a configured collector's request, target policy and credentials. The playground makes no request of its own; it reads what the collector it was copied from answered.</p>
<p class="note" id="fetched-through">{{if .Chosen}}Probed through {{.Chosen}}, the collector chosen.{{end}}</p>
<div id="target-part"{{if .Form.NoTarget}} hidden{{end}}>
<label for="target" id="target-label">{{or .Form.TargetLabel "Target"}}</label>
<input id="target" type="text" placeholder="{{.Form.TargetHint}}" autocomplete="off">
</div>
<p class="note" id="no-target"{{if not .Form.NoTarget}} hidden{{end}}>This collector names its own target.</p>
<div id="params-part" role="group" aria-labelledby="params-title"{{if not (or .Form.Params .Form.OtherParams)}} hidden{{end}}>
<div class="kv-title" id="params-title">Probe parameters</div>
<p class="note">A parameter left empty takes its default, shown greyed in its box; a name box suggests the parameters the collector takes.</p>
<datalist id="params-names"></datalist>
<div class="kv" id="params-rows"></div>
<button type="button" class="secondary" id="params-add">+ Add parameter</button>
</div>
<p class="note" id="no-params"{{if or .Form.Params .Form.OtherParams}} hidden{{end}}>This collector takes no probe parameters.</p>
<div class="copy-as" id="probe-copy" role="group" aria-labelledby="probe-copy-title">
<div class="copy-row"><span class="kv-title" id="probe-copy-title">Copy as</span>
<button type="button" class="secondary" data-copy="url" data-what="probe URL" aria-controls="probe-copy-text" aria-pressed="false">URL</button>
<button type="button" class="secondary" data-copy="curl" data-what="curl command" aria-controls="probe-copy-text" aria-pressed="false">curl</button>
<button type="button" class="secondary" data-copy="scrape" data-what="scrape_config" aria-controls="probe-copy-text" aria-pressed="false">scrape_config</button>
<button type="button" class="secondary" data-copy="debug" data-what="debug report URL" aria-controls="probe-copy-text" aria-pressed="false">Debug report URL</button>
<span class="copy-done" data-copy-done role="status"></span>
</div>
<p class="note" data-copy-note hidden></p>
<pre class="copy-text" id="probe-copy-text" tabindex="0" aria-label="The text copied" hidden></pre>
</div>
</div>
<div data-mode="static" hidden>
<p class="note">The static target is scraped once as its schedule scrapes it, with its own settings and credentials, through the collector chosen, or the one the playground was copied from, and read by the collector chosen. Nothing is published.</p>
<p class="note" id="static-through">{{if .Chosen}}Static targets of {{.Chosen}}, the collector chosen.{{end}}</p>
<label for="static-target">Static target</label>
<select id="static-target" data-for="{{.Chosen}}"{{if not .StaticTargets}} disabled{{end}}>
{{- range .StaticTargets}}
<option value="{{.Name}}"{{if eq .Name $.StaticTarget}} selected{{end}}>{{.Name}}{{if .Target}} — {{.Target}}{{end}}</option>
{{- end}}
</select>
<div class="copy-as" id="static-copy" role="group" aria-labelledby="static-copy-title">
<div class="copy-row"><span class="kv-title" id="static-copy-title">Copy as</span>
<button type="button" class="secondary" data-copy="url" data-what="static targets URL" aria-controls="static-copy-text" aria-pressed="false">URL</button>
<button type="button" class="secondary" data-copy="curl" data-what="curl command" aria-controls="static-copy-text" aria-pressed="false">curl</button>
<span class="copy-done" data-copy-done role="status"></span>
</div>
<p class="note" data-copy-note hidden></p>
<pre class="copy-text" id="static-copy-text" tabindex="0" aria-label="The text copied" hidden></pre>
</div>
</div>
{{- end}}
<div class="actions">
<button type="button" id="run">Run</button>
<div class="narrow"><label for="timeout">Timeout, seconds</label>
<input id="timeout" type="number" min="1" max="600" step="any" value="{{.ProbeTimeoutSeconds}}"></div>
</div>
</section>
</div>
<section class="panel" style="margin-top:16px" aria-live="polite">
<h2>Output</h2>
<div class="status" id="status-line">Not run yet.</div>
<div class="series-head">
<h3 id="series-heading">Series a probe would answer</h3>
<div class="views" id="views" role="tablist" aria-labelledby="series-heading">
<button type="button" role="tab" id="view-table" data-view="table" aria-controls="series-table-part" aria-selected="true">Table</button>
<button type="button" role="tab" id="view-text" data-view="text" aria-controls="series-text-part" aria-selected="false">Text</button>
</div>
</div>
<div id="series-table-part" role="tabpanel" aria-labelledby="view-table">
<div class="row series-filters">
<div><label for="series-filter">Filter: name, label or value</label>
<input id="series-filter" type="search" autocomplete="off" spellcheck="false"></div>
<div><label for="series-metric">Metric</label>
<select id="series-metric"><option value="">Every metric</option></select></div>
</div>
<ul class="lint-unplaced" id="lint-unplaced" hidden></ul>
<p class="note" id="series-count">Not run yet.</p>
<div class="table-box" id="series-box" hidden><table class="series" id="series-table"><thead><tr id="series-columns"></tr></thead><tbody id="series-rows"></tbody></table></div>
<p class="note" id="series-cap" hidden></p>
</div>
<div id="series-text-part" role="tabpanel" aria-labelledby="view-text" hidden>
<pre id="series"></pre>
</div>
<h3>Stages</h3>
<ul class="stages" id="stages"></ul>
<div id="lint-part" hidden>
<h3>Lint</h3>
<pre id="lint"></pre>
</div>
<div id="notes-part" hidden>
<h3>Logged</h3>
<pre id="notes"></pre>
</div>
<div id="requests-part" hidden>
<h3>Requests</h3>
<pre id="requests"></pre>
</div>
<div id="response-part" hidden>
<h3>Response</h3>
<pre id="response"></pre>
</div>
<details>
<summary>Debug report</summary>
<pre id="report"></pre>
</details>
</section>
</main>
<script type="application/json" id="forms">{{.Forms}}</script>
<script>
"use strict";
const $ = (id) => document.getElementById(id);
const store = (() => { try { return window.localStorage; } catch (error) { return null; } })();
// The page opens with the example playground, whatever was written on it
// before: written is the playground, which the editor shows while no
// configured collector is chosen, and copiedFrom the collector Edit a copy
// copied it from, which a target is fetched through while the playground
// runs. Both last only while the page is open; Back to the example puts
// the example back.
let written = $("collector").dataset.example, copiedFrom = "";
// A link from the collectors page names the collector, and at full the
// target or the static target. The server has chosen the collector the
// link names, or the one of the static target it names, and the browser's
// own memory of the form does not choose another.
const asked = new URLSearchParams(location.search);
const pick = (select, value) => {
  for (const option of select.options) if (option.value === value) { select.value = value; return true; }
  return false;
};
pick($("configured"), $("configured").dataset.chosen);
const choose = (value) => {
  const radio = document.querySelector('input[name="mode"][value="' + value + '"]');
  if (radio) radio.checked = true;
};
if (asked.get("target") !== null && $("target")) { choose("fetch"); $("target").value = asked.get("target"); }
if (asked.get("static_target") !== null && $("static-target") && pick($("static-target"), asked.get("static_target"))) choose("static");
const mode = () => {
  const checked = document.querySelector('input[name="mode"]:checked');
  return checked ? checked.value : "paste";
};
const showMode = () => {
  for (const part of document.querySelectorAll("[data-mode]")) part.hidden = part.dataset.mode !== mode();
};
// fetcher is the collector a target is fetched through: the configured one
// chosen, or the one the playground was copied from; the playground makes
// no request of its own, so without either nothing can be fetched.
const fetcher = () => $("configured").value || copiedFrom;
// What a run asks for follows the request type of the collector it is for
// (debugForm): the one run reads what is pasted, and a target is probed
// through fetcher(). The playground's is the request.type written in the
// editor, as it is written, with the form the page holds of each type the
// build has; one the page does not know, or none, is asked for as an http
// collector's answer, and the server says what is wrong with it.
const forms = JSON.parse($("forms").textContent);
const formOf = (name) => (name !== "" && forms.collectors[name]) || forms.playground;
// requestTypeOf is the request.type a collector's YAML writes, or "": the
// request key of its top mapping, written as a block (request: then type:
// indented under it) or in flow style (request: {type: sql}), its value
// quoted or not; comments are left out. It reads what the editor holds as
// it is typed, so it is tolerant, and the server reads the YAML itself.
function requestTypeOf(text) {
  const uncommented = (line) => {
    let quote = "";
    for (let i = 0; i < line.length; i++) {
      const ch = line[i];
      if (quote !== "") { if (ch === quote) quote = ""; continue; }
      if (ch === "'" || ch === '"') quote = ch;
      else if (ch === "#" && (i === 0 || /\s/.test(line[i - 1]))) return line.slice(0, i);
    }
    return line;
  };
  const lines = text.split(/\r?\n/).map(uncommented);
  const indentOf = (line) => line.length - line.trimStart().length;
  const scalar = (value) => {
    value = value.trim();
    const quoted = /^(["'])(.*)\1$/.exec(value);
    return quoted ? quoted[2] : value;
  };
  const key = /^\s*(?:-\s+)?(["']?)([A-Za-z_][\w-]*)\1\s*:(?:\s+(.*)|$)/;
  let top = -1;
  for (let i = 0; i < lines.length; i++) {
    if (lines[i].trim() === "" || lines[i].trim() === "---") continue;
    const at = key.exec(lines[i]);
    if (!at) continue;
    // The top mapping's keys are at the indentation of its first; a list's
    // dash, as a collector is written under collectors:, is passed over.
    const indent = indentOf(lines[i]) + (/^\s*-\s/.test(lines[i]) ? lines[i].trimStart().indexOf(at[1] + at[2]) : 0);
    if (top < 0) top = indent;
    if (indent !== top || at[2] !== "request") continue;
    const rest = (at[3] || "").trim();
    if (rest.startsWith("{")) {
      // Flow style, which may go on over the lines after it.
      let flow = rest, depth = 0;
      for (let j = i + 1; j < lines.length && (flow.match(/{/g) || []).length > (flow.match(/}/g) || []).length; j++) flow += " " + lines[j];
      for (let k = 0, start = 1; k < flow.length; k++) {
        const ch = flow[k];
        if (ch === "{" || ch === "[") depth++;
        else if (ch === "}" || ch === "]") depth--;
        if ((ch === "," && depth === 1) || depth === 0) {
          const pair = /^\s*(["']?)type\1\s*:\s*(.*)$/.exec(flow.slice(start, k));
          if (pair) return scalar(pair[2]);
          start = k + 1;
          if (depth === 0) break;
        }
      }
      return "";
    }
    if (rest !== "") return "";
    let inner = -1;
    for (let j = i + 1; j < lines.length; j++) {
      if (lines[j].trim() === "") continue;
      const indent = indentOf(lines[j]);
      if (indent <= top) break;
      if (inner < 0) inner = indent;
      const pair = key.exec(lines[j]);
      if (indent === inner && pair && pair[2] === "type") return scalar(pair[3] || "");
    }
    return "";
  }
  return "";
}
let writtenType = requestTypeOf(written);
const playgroundForm = () => forms.types[writtenType] || forms.playground;
const readerForm = () => $("configured").value !== "" ? formOf($("configured").value) : playgroundForm();
// The headers of a pasted response and the probe parameters are each a list
// of rows, a name and a value with × to remove the row, and a button to add
// one. A row left blank is not sent, and one with a value and no name, or a
// name the server would refuse, is marked on the row, with why, and the run
// not sent; the server checks again whatever it is sent. What is typed is
// put in with value and textContent alone, never as HTML.
let rowIds = 0;
function editor(rowsBox, addButton, noun, check, hint, names) {
  const rows = [];
  const title = noun[0].toUpperCase() + noun.slice(1);
  const relabel = () => rows.forEach((row, i) => {
    row.name.setAttribute("aria-label", title + " name " + (i + 1));
    row.value.setAttribute("aria-label", title + " value " + (i + 1));
    row.remove.setAttribute("aria-label", "Remove " + noun + " " + (i + 1));
  });
  let marked = false;
  // messageOf is why the row at i is wrong, or "".
  const messageOf = (i) => {
    const name = rows[i].name.value.trim(), value = rows[i].value.value;
    if (name === "" && value !== "") return "A " + noun + " needs a name: give one, or clear the value.";
    if (name === "") return "";
    return check(name, rows.slice(0, i).map((before) => before.name.value.trim()), rows.map((each) => each.name.value.trim()));
  };
  // validate marks every row that is wrong and returns the first input to
  // put right, or null.
  const validate = () => {
    let first = null;
    marked = false;
    rows.forEach((row, i) => {
      const message = messageOf(i);
      row.error.textContent = message;
      row.error.hidden = message === "";
      if (message === "") {
        row.name.removeAttribute("aria-invalid");
        row.name.removeAttribute("aria-describedby");
      } else {
        row.name.setAttribute("aria-invalid", "true");
        row.name.setAttribute("aria-describedby", row.error.id);
        marked = true;
        if (first === null) first = row.name;
      }
    });
    return first;
  };
  const add = (name = "", value = "", focus = false) => {
    const item = document.createElement("div"), line = document.createElement("div"), fields = document.createElement("div");
    item.className = "kv-item";
    line.className = "kv-row";
    fields.className = "kv-fields";
    const row = { item, name: document.createElement("input"), value: document.createElement("input"), remove: document.createElement("button"), error: document.createElement("p") };
    for (const input of [row.name, row.value]) {
      input.type = "text";
      input.autocomplete = "off";
      input.spellcheck = false;
      input.addEventListener("input", () => { if (marked) validate(); });
      input.addEventListener("change", validate);
    }
    row.name.className = "kv-name";
    row.name.placeholder = "Name";
    if (names) row.name.setAttribute("list", names);
    row.name.value = name;
    row.value.className = "kv-value";
    row.value.value = value;
    row.value.placeholder = hint(name.trim());
    row.name.addEventListener("input", () => { row.value.placeholder = hint(row.name.value.trim()); });
    // Enter in the last row's value adds a row.
    row.value.addEventListener("keydown", (event) => {
      if (event.key !== "Enter" || rows[rows.length - 1] !== row) return;
      event.preventDefault();
      add("", "", true);
    });
    row.remove.type = "button";
    row.remove.className = "secondary kv-remove";
    row.remove.textContent = "\u00d7";
    row.remove.addEventListener("click", () => {
      const at = rows.indexOf(row);
      rows.splice(at, 1);
      item.remove();
      relabel();
      validate();
      // Focus stays in the list: on the row that took this one's place,
      // the one before it, or the button that adds one.
      const next = rows[at] || rows[at - 1];
      (next ? next.name : addButton).focus();
    });
    row.error.className = "kv-error";
    row.error.id = "kv-error-" + ++rowIds;
    row.error.hidden = true;
    fields.append(row.name, row.value);
    line.append(fields, row.remove);
    item.append(line, row.error);
    rowsBox.append(item);
    rows.push(row);
    relabel();
    if (focus) row.name.focus();
    return row;
  };
  addButton.addEventListener("click", () => add("", "", true));
  return {
    rows, add, validate,
    // wrong is whether a row is wrong, marking none: what is copied is made
    // as the visitor types, and a row is marked as the run marks it.
    wrong: () => rows.some((row, i) => messageOf(i) !== ""),
    clear: () => { rows.length = 0; rowsBox.replaceChildren(); },
    // values are the rows filled in, name to its values in the order of the
    // rows; same (a name) is the name a later row's is given as, so that two
    // that differ only as same says are one name repeated.
    values: (same) => {
      const out = {}, spelt = new Map();
      for (const row of rows) {
        const name = row.name.value.trim();
        if (name === "") continue;
        const key = spelt.get(same(name)) ?? name;
        spelt.set(same(name), key);
        (out[key] ||= []).push(row.value.value);
      }
      // One value is sent as a string, more as a list.
      for (const name of Object.keys(out)) if (out[name].length === 1) out[name] = out[name][0];
      return out;
    },
  };
}
// A header's name is an HTTP token, as the server reads it; a name may be
// given again, as HTTP lets a response repeat a header, and the names of a
// header are the same in any case.
const headerName = /^[!#$%&'*+.^_\x60|~0-9A-Za-z-]+$/;
const headers = editor($("headers-rows"), $("headers-add"), "header",
  (name) => headerName.test(name) ? "" : "Header name " + JSON.stringify(name) + " is not one HTTP allows.",
  () => "Value");
headers.add();
// A probe parameter is given once, as /probe takes it, but header_<name>,
// every value of which is forwarded; target, collector and debug are the
// page's own.
const headerParam = (name) => name.length > 7 && name.slice(0, 7).toLowerCase() === "header_";
let paramDefaults = new Map();
const params = $("params-rows") && editor($("params-rows"), $("params-add"), "parameter",
  (name, before, all) => {
    if (name === "target" || name === "collector" || name === "debug") return name + " is one the debug page sets itself.";
    if (headerParam(name) || !before.includes(name)) return "";
    return "Probe parameter " + name + " is given " + all.filter((other) => other === name).length + " times; give it once.";
  },
  (name) => paramDefaults.get(name) || "Value", "params-names");
// The rows of the probe parameters are those of the collector a target is
// probed through: a row of each of its own, with its default shown, and the
// others it takes offered as names. A change of collector makes them again,
// keeping what was typed for a name the new one takes too.
let paramsOf = null;
function showParams(through) {
  const own = through.params || [], others = through.other_params || [];
  $("params-part").hidden = own.length + others.length === 0;
  $("no-params").hidden = own.length + others.length > 0;
  const of = JSON.stringify([own, others]);
  if (of === paramsOf) return;
  paramsOf = of;
  paramDefaults = new Map(own.map((param) => [param.name, param.default]));
  const takes = new Set([...own.map((param) => param.name), ...others]);
  $("params-names").replaceChildren(...[...takes].map((name) => { const option = document.createElement("option"); option.value = name; return option; }));
  const typed = params.rows.map((row) => ({ name: row.name.value.trim(), value: row.value.value })).filter((row) => takes.has(row.name));
  params.clear();
  for (const param of own) {
    const at = typed.findIndex((row) => row.name === param.name);
    params.add(param.name, at < 0 ? "" : typed.splice(at, 1)[0].value);
  }
  for (const row of typed) params.add(row.name, row.value);
  if (params.rows.length === 0 && takes.size > 0) params.add();
  params.validate();
}
function showForm() {
  const reads = readerForm();
  $("content-type-part").hidden = !reads.content_type;
  $("status-part").hidden = !reads.http_answer;
  $("headers-part").hidden = !reads.http_answer;
  $("body").placeholder = reads.answer;
  if (!$("target")) return;
  const through = formOf(fetcher());
  $("target-part").hidden = !!through.no_target;
  $("no-target").hidden = !through.no_target;
  $("target-label").textContent = through.target_label || "Target";
  $("target").placeholder = through.target_hint || "";
  showParams(through);
  refreshCopies();
}
// A collector read again from /debug/collector brings its form with it,
// which a reload may have changed.
const learnForm = (name, answer) => {
  if (answer && answer.form) { forms.collectors[name] = answer.form; showForm(); }
};
function showFetch() {
  const radio = $("mode-fetch");
  if (!radio) return;
  const through = fetcher();
  radio.disabled = through === "";
  $("fetch-unavailable").hidden = through !== "";
  if (radio.disabled && radio.checked) choose("paste");
  $("fetched-through").textContent = through === "" ? "" : $("configured").value !== ""
    ? "Probed through " + through + ", the collector chosen."
    : "Probed through " + through + ", the collector this playground was copied from.";
  showMode();
  refreshCopies();
}
// The static targets offered are those scraped through fetcher(), read from
// /debug/collector whenever it changes, so they follow a reload of the
// static target file; the page opens with those of the collector chosen.
// A response comes through that collector, a fetched target's as a static
// target's, so the example playground, copied from none, scrapes none.
let listedFor = $("static-target") ? $("static-target").dataset.for : "";
async function showStaticTargets() {
  const select = $("static-target"), through = fetcher();
  if (!select) return;
  // Whose they are is said whenever the collector chosen changes, the list
  // being the same when a copy is made of it.
  $("static-through").textContent = through === "" ? "" : $("configured").value !== ""
    ? "Static targets of " + through + ", the collector chosen."
    : "Static targets of " + through + ", the collector this playground was copied from.";
  if (through === listedFor) return;
  listedFor = through;
  let targets = [], note = "";
  if (through !== "") {
    try {
      const url = new URL("debug/collector", document.baseURI);
      url.searchParams.set("name", through);
      const response = await fetch(url, { cache: "no-store", credentials: "same-origin" });
      const answer = await response.json();
      if (answer.error) note = answer.error;
      else { targets = answer.static_targets || []; learnForm(through, answer); }
    } catch (error) {
      note = "The static targets could not be read: " + error.message;
    }
    if (note === "" && targets.length === 0) note = "No static target uses " + through + ".";
  }
  if (listedFor !== through) return;
  const kept = select.value;
  select.replaceChildren(...targets.map((target) => new Option(target.target ? target.name + " — " + target.target : target.name, target.name)));
  pick(select, kept);
  select.disabled = targets.length === 0;
  $("mode-static").disabled = targets.length === 0;
  $("static-unavailable").textContent = note;
  $("static-unavailable").hidden = note === "";
  if ($("mode-static").disabled && $("mode-static").checked) choose("paste");
  showMode();
  refreshCopies();
}
// The example body fits the example collector alone: with a configured
// collector, or a copy of one, the body starts empty, waiting for what its
// target answers. A body the visitor wrote is left as it is.
function fitBody() {
  const body = $("body"), example = body.dataset.example;
  if (body.value !== example && body.value !== "") return;
  body.value = $("configured").value === "" && copiedFrom === "" ? example : "";
}
// The editor shows what runs: the playground, or the configured collector
// chosen, as /debug/collector shows it, read-only, since it runs as
// configured. Edit a copy makes the playground a copy of that one.
const configuredYAML = new Map();
if ($("collector").dataset.shown) configuredYAML.set($("collector").dataset.shown, $("collector").value);
async function showCollector() {
  const area = $("collector"), name = $("configured").value;
  area.readOnly = name !== "";
  $("edit-copy").hidden = name === "";
  $("back-to-example").hidden = name !== "" || copiedFrom === "";
  $("collector-note").hidden = name === "";
  $("collector-label").textContent = name !== "" ? "The configuration of " + name + ", which runs as configured (read-only)"
    : copiedFrom !== "" ? "Playground, a copy of " + copiedFrom + ": the YAML of a collector, which runs"
    : "Playground: the YAML of a collector, which runs";
  showFetch();
  showForm();
  showStaticTargets();
  fitBody();
  if (name === "") { area.value = written; return; }
  if (configuredYAML.has(name)) { area.value = configuredYAML.get(name); return; }
  area.value = "# Reading the configuration of " + name + "…";
  let text;
  try {
    const url = new URL("debug/collector", document.baseURI);
    url.searchParams.set("name", name);
    const response = await fetch(url, { cache: "no-store", credentials: "same-origin" });
    const answer = await response.json();
    if (answer.error) text = "# " + answer.error;
    else { configuredYAML.set(name, text = answer.yaml); learnForm(name, answer); }
  } catch (error) {
    text = "# The configuration could not be read: " + error.message;
  }
  if ($("configured").value === name) area.value = text;
}
// Copy as gives what the page's form asks for as it is asked of the exporter
// outside the page: the /probe URL, a curl command for it, a Prometheus job
// that scrapes it, and the debug probe's URL; for a static target, the
// /static-targets URL and its curl. Each is made from the form as it now
// stands and shown in a box, so it can be selected where the browser does
// not let the page copy. The URLs are absolute, resolved as the page's own
// requests are, so a proxy's path prefix is kept. A credential is never put
// in: with web.basic_auth on, curl is given -u USER, which asks for the
// password, and the job a basic_auth to fill in.
const basicAuth = document.querySelector("main").hasAttribute("data-basic-auth");
const shellQuoted = (text) => "'" + text.replaceAll("'", "'\\''") + "'";
const yamlQuoted = (text) => "'" + text.replaceAll("'", "''") + "'";
// A mapping key is written plain only where YAML reads it as that string.
const yamlKey = (text) => /^[A-Za-z_][A-Za-z0-9_-]*$/.test(text) && !/^(true|false|yes|no|on|off|y|n|null)$/i.test(text) ? text : yamlQuoted(text);
const curlOf = (url) => "curl -s " + (basicAuth ? "-u USER " : "") + shellQuoted(url);
const authNote = basicAuth ? " The exporter asks for web.basic_auth: curl -u USER asks for USER's password, and the job's basic_auth is left for you to fill in; no credential is put in these." : "";
function copier(box, texts) {
  const pre = box.querySelector(".copy-text"), note = box.querySelector("[data-copy-note]"), done = box.querySelector("[data-copy-done]");
  const buttons = [...box.querySelectorAll("button[data-copy]")];
  let shown = "";
  const say = (message, copied = false) => {
    done.textContent = message;
    done.classList.toggle("copied", copied);
  };
  const refresh = () => {
    const made = texts();
    note.textContent = made.note;
    note.hidden = made.note === "";
    for (const button of buttons) button.disabled = !!made.blocked;
    if (made.blocked) shown = "";
    for (const button of buttons) button.setAttribute("aria-pressed", String(button.dataset.copy === shown));
    pre.hidden = shown === "";
    const text = shown === "" ? "" : made[shown];
    if (pre.textContent !== text) { pre.textContent = text; say(""); }
    return made;
  };
  for (const button of buttons) {
    button.addEventListener("click", async () => {
      shown = button.dataset.copy;
      const text = refresh()[shown];
      if (!navigator.clipboard || !navigator.clipboard.writeText) { say("Select the text below to copy it."); return; }
      try {
        await navigator.clipboard.writeText(text);
        say("Copied the " + button.dataset.what + ".", true);
      } catch (error) {
        say("The browser did not let the page copy it; select the text below.");
      }
    });
  }
  return refresh;
}
// The Prometheus job is the multi-target pattern of docs/LOCALFILE.md: the
// target is the job's, moved into the target parameter and instance, and
// the exporter, as this page reached it, is what is scraped.
function scrapeConfigOf(collector, target, given, url) {
  const lines = ["scrape_configs:", "  - job_name: " + yamlQuoted(collector)];
  if (url.protocol === "https:") lines.push("    scheme: https");
  lines.push("    metrics_path: " + yamlQuoted(url.pathname), "    params:");
  const listed = new Map([["collector", [collector]]]);
  for (const [name, value] of given) {
    if (listed.has(name)) listed.get(name).push(value);
    else listed.set(name, [value]);
  }
  for (const [name, values] of listed) lines.push("      " + yamlKey(name) + ": [" + values.map(yamlQuoted).join(", ") + "]");
  if (basicAuth) lines.push("    # The exporter asks for web.basic_auth:", "    # basic_auth:", "    #   username: USER", "    #   password_file: /path/to/the/password");
  const exporter = "# The exporter as this page reached it; inside a cluster, its service's address.";
  if (target === null) {
    lines.push("    " + exporter, "    static_configs:", "      - targets: [" + yamlQuoted(url.host) + "]");
  } else {
    lines.push("    static_configs:", "      - targets: [" + yamlQuoted(target) + "]",
      "    relabel_configs:",
      "      - source_labels: [__address__]", "        target_label: __param_target",
      "      - source_labels: [__param_target]", "        target_label: instance",
      "      " + exporter, "      - target_label: __address__", "        replacement: " + yamlQuoted(url.host));
  }
  return lines.join("\n") + "\n";
}
// /probe runs only the configured collectors, so a playground copied from
// one is copied as a probe through that one, as configured.
const probeCopy = $("probe-copy") ? copier($("probe-copy"), () => {
  const through = fetcher();
  if (through === "") return { blocked: true, note: "Choose a configured collector, or Edit a copy of one, to copy a probe." };
  const form = formOf(through);
  const target = form.no_target ? null : $("target").value;
  if (target !== null && target.trim() === "") return { blocked: true, note: "Give the target to copy the probe." };
  const own = $("params-part").hidden ? null : params;
  if (own && own.wrong()) return { blocked: true, note: "A probe parameter row is wrong; put it right to copy the probe." };
  const given = own ? Object.entries(own.values((name) => name)).flatMap(([name, values]) => [values].flat().map((value) => [name, value])) : [];
  const url = new URL("probe", document.baseURI);
  url.searchParams.append("collector", through);
  if (target !== null) url.searchParams.append("target", target);
  for (const [name, value] of given) url.searchParams.append(name, value);
  const debug = new URL(url);
  debug.searchParams.append("debug", "true");
  const copied = $("configured").value === "" ? "These probe through " + through + ", the collector this playground was copied from, as configured: /probe runs only the configured collectors, so the playground's edits are not in them." : "";
  return { note: (copied + authNote).trim(), url: url.href, curl: curlOf(url.href), scrape: scrapeConfigOf(through, target, given, url), debug: debug.href };
}) : null;
const staticCopy = $("static-copy") ? copier($("static-copy"), () => {
  const name = $("static-target").disabled ? "" : $("static-target").value;
  if (name === "") return { blocked: true, note: "Choose a static target to copy its scrape." };
  const url = new URL("static-targets", document.baseURI);
  url.searchParams.set("targets", name);
  return { note: authNote.trim(), url: url.href, curl: curlOf(url.href) };
}) : null;
function refreshCopies() {
  if (probeCopy) probeCopy();
  if (staticCopy) staticCopy();
}
if (probeCopy) {
  $("target").addEventListener("input", probeCopy);
  // A row's input, its × and + Add parameter.
  for (const event of ["input", "click"]) $("params-part").addEventListener(event, probeCopy);
}
if (staticCopy) $("static-target").addEventListener("change", staticCopy);
// The panel follows the request type written in the playground as it is
// typed, once a frame however fast the keys come: the fields it hides keep
// what was typed in them, for when the type comes back, and are not sent.
let typeFrame = 0;
$("collector").addEventListener("input", () => {
  if ($("collector").readOnly) return;
  written = $("collector").value;
  if (typeFrame) return;
  typeFrame = requestAnimationFrame(() => {
    typeFrame = 0;
    const now = requestTypeOf(written);
    if (now !== writtenType) { writtenType = now; showForm(); }
  });
});
$("edit-copy").addEventListener("click", () => {
  written = $("collector").value;
  writtenType = requestTypeOf(written);
  copiedFrom = $("configured").value;
  $("configured").value = "";
  showCollector();
  $("collector").focus();
});
// Back to the example puts the example playground back in place of the
// copy, with the example's body unless one was written in.
$("back-to-example").addEventListener("click", () => {
  written = $("collector").dataset.example;
  writtenType = requestTypeOf(written);
  copiedFrom = "";
  showCollector();
  $("collector").focus();
});
for (const radio of document.querySelectorAll('input[name="mode"]')) radio.addEventListener("change", showMode);
$("configured").addEventListener("change", showCollector);
showMode();
showCollector();
// A list whose rows are wrong is not sent: the first wrong row is focused,
// and the status says where to look.
const filled = (list, what, same) => {
  const wrong = list.validate();
  if (wrong) { wrong.focus(); throw new Error("A " + what + " row is wrong; it says why below it."); }
  return list.values(same);
};
let running = null;
async function run() {
  if (running) running.abort();
  const controller = new AbortController();
  running = controller;
  const statusLine = $("status-line");
  const request = {};
  if ($("configured").value !== "") request.configured = $("configured").value;
  else request.collector = $("collector").value;
  try {
    if (mode() === "fetch") {
      // A field the collector's form leaves out is sent empty: a collector
      // that names its own target is probed with none.
      const through = formOf(fetcher());
      if (request.collector !== undefined) request.request_from = copiedFrom;
      request.target = through.no_target ? "" : $("target").value;
      // A parameter's name is the same in one case only, as /probe reads
      // it.
      request.params = $("params-part").hidden ? {} : filled(params, "probe parameter", (name) => name);
    } else if (mode() === "static") {
      request.static_target = $("static-target").value;
    } else {
      const reads = readerForm();
      request.body = $("body").value;
      request.content_type = reads.content_type ? $("content-type").value : "";
      request.status = reads.http_answer ? Number($("status").value) || 0 : 0;
      request.headers = reads.http_answer ? filled(headers, "header", (name) => name.toLowerCase()) : {};
    }
  } catch (error) {
    statusLine.className = "status failed";
    statusLine.textContent = error.message;
    return;
  }
  const timeout = Number($("timeout").value) || {{.ProbeTimeoutSeconds}};
  statusLine.className = "status";
  statusLine.textContent = "Running…";
  const started = performance.now();
  try {
    // Relative, so the page works under a proxy's path prefix. The header
    // and the JSON body are what another site's page cannot send.
    const response = await fetch(new URL("debug/run", document.baseURI), {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Debug-Run": "run", "X-Prometheus-Scrape-Timeout-Seconds": String(timeout) },
      body: JSON.stringify(request),
      cache: "no-store", credentials: "same-origin",
      signal: AbortSignal.any([controller.signal, AbortSignal.timeout((timeout + 5) * 1000)]),
    });
    const answer = await response.json();
    show(answer, response, Math.round(performance.now() - started));
  } catch (error) {
    if (controller.signal.aborted) return;
    statusLine.className = "status failed";
    statusLine.textContent = error.name === "TimeoutError" ? "No answer within " + (timeout + 5) + " s; the page stopped waiting" : "The run could not be sent: " + error.message;
  } finally {
    if (running === controller) running = null;
  }
}
// The series are shown as a table, by default, or as the text a probe
// answers, which stays whole whatever the table shows; the choice is kept
// in this browser. A refused run's error is text, and shown as text.
let view = "table", refused = false;
try { if (store && store.getItem("debug.view") === "text") view = "text"; } catch (error) { /* the table, then */ }
function showView() {
  const shown = refused ? "text" : view;
  $("views").hidden = refused;
  for (const tab of document.querySelectorAll("#views [data-view]")) tab.setAttribute("aria-selected", String(tab.dataset.view === shown));
  $("series-table-part").hidden = shown !== "table";
  $("series-text-part").hidden = shown !== "text";
}
for (const tab of document.querySelectorAll("#views [data-view]")) {
  tab.addEventListener("click", () => {
    view = tab.dataset.view;
    try { if (store) store.setItem("debug.view", view); } catch (error) { /* not kept */ }
    showView();
  });
}
showView();
// The table has a row for each sample of answer.samples, which the server
// makes from the series themselves: the name, a column for each label name
// any sample has, alphabetically, the value as the text writes it and the
// type. A lint finding is shown on the rows of the metric it names, and one
// about a metric no row is of above the table. What a target sent is put
// in with textContent alone, never as HTML. At most tableRows rows are
// drawn, so a large answer keeps the page quick; the text has them all.
const tableRows = 2000;
let samples = [], labelNames = [], findingsOf = new Map(), sortBy = null, sortDown = false;
// A number as the text writes it: +Inf and -Inf, which Number does not read.
const numberOf = (text) => text === "+Inf" ? Infinity : text === "-Inf" ? -Infinity : Number(text);
const numericLabels = new Set(["le", "quantile"]);
function cellOf(sample, column) {
  if (column === "name") return sample.name;
  if (column === "value") return sample.value;
  if (column === "type") return sample.type;
  return (sample.labels && sample.labels[column.slice(6)]) || "";
}
// Ascending or descending by the column, a value and le or quantile as
// numbers, NaN after every number; a label a sample lacks sorts last
// either way.
function compareBy(column, down) {
  const numeric = column === "value" || (column.startsWith("label:") && numericLabels.has(column.slice(6)));
  return (a, b) => {
    const x = cellOf(a, column), y = cellOf(b, column);
    if (x === "" || y === "") return (x === "") - (y === "");
    let order;
    if (numeric) {
      const p = numberOf(x), q = numberOf(y);
      order = Number.isNaN(p) || Number.isNaN(q) ? Number.isNaN(p) - Number.isNaN(q) : p < q ? -1 : p > q ? 1 : 0;
    } else {
      order = x < y ? -1 : x > y ? 1 : 0;
    }
    return down ? -order : order;
  };
}
const haystackOf = (sample) => [sample.name, ...Object.entries(sample.labels || {}).map(([k, v]) => k + "=" + v), sample.value].join("\n").toLowerCase();
function adviceOf(findings) {
  const details = document.createElement("details");
  const summary = document.createElement("summary");
  summary.textContent = "\u26a0 " + findings.map((f) => f.rule).join(", ");
  summary.title = findings.map((f) => "[" + f.rule + "] " + f.message).join("\n");
  details.append(summary);
  for (const f of findings) {
    const advice = document.createElement("p");
    advice.className = "lint-advice";
    advice.textContent = "[" + f.rule + "] " + f.message;
    details.append(advice);
  }
  return details;
}
function fillTable(answer) {
  samples = (answer.samples || []).map((sample) => ({ ...sample, haystack: haystackOf(sample) }));
  const names = new Set();
  for (const sample of samples) for (const name of Object.keys(sample.labels || {})) names.add(name);
  labelNames = [...names].sort();
  findingsOf = new Map();
  const metrics = new Set(samples.map((sample) => sample.metric));
  const unplaced = [];
  for (const f of answer.lint || []) {
    if (!metrics.has(f.metric)) { unplaced.push(f); continue; }
    if (!findingsOf.has(f.metric)) findingsOf.set(f.metric, []);
    findingsOf.get(f.metric).push(f);
  }
  const list = $("lint-unplaced");
  list.replaceChildren(...unplaced.map((f) => {
    const item = document.createElement("li");
    item.textContent = "\u26a0 [" + f.rule + "] metric " + JSON.stringify(f.metric) + ", which no series is of: " + f.message;
    return item;
  }));
  list.hidden = unplaced.length === 0;
  const select = $("series-metric"), kept = select.value;
  select.replaceChildren(new Option("Every metric", ""), ...[...metrics].map((metric) => new Option(metric, metric)));
  if (!pick(select, kept)) select.value = "";
  const columns = ["name", ...labelNames.map((name) => "label:" + name), "value", "type"];
  if (sortBy !== null && !columns.includes(sortBy)) sortBy = null;
  const head = $("series-columns");
  head.replaceChildren(...columns.map((column) => {
    const th = document.createElement("th");
    th.scope = "col";
    th.dataset.column = column;
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = column.startsWith("label:") ? column.slice(6) : column;
    button.addEventListener("click", () => {
      sortDown = sortBy === column ? !sortDown : false;
      sortBy = column;
      drawRows();
    });
    th.append(button);
    return th;
  }));
  if (findingsOf.size > 0) {
    const th = document.createElement("th");
    th.scope = "col";
    th.textContent = "lint";
    head.append(th);
  }
  drawRows();
}
function drawRows() {
  const needle = $("series-filter").value.trim().toLowerCase(), metric = $("series-metric").value;
  let rows = samples.filter((sample) => (metric === "" || sample.metric === metric) && (needle === "" || sample.haystack.includes(needle)));
  if (sortBy !== null) rows = rows.slice().sort(compareBy(sortBy, sortDown));
  for (const th of $("series-columns").children) {
    if (th.dataset.column) th.setAttribute("aria-sort", th.dataset.column === sortBy ? (sortDown ? "descending" : "ascending") : "none");
  }
  const drawn = rows.slice(0, tableRows);
  $("series-rows").replaceChildren(...drawn.map((sample) => {
    const tr = document.createElement("tr");
    const cell = (text, className) => {
      const td = document.createElement("td");
      if (className) td.className = className;
      td.textContent = text;
      tr.append(td);
      return td;
    };
    cell(sample.name);
    for (const name of labelNames) cell((sample.labels && sample.labels[name]) || "");
    cell(sample.value, "value");
    cell(sample.type, "type");
    if (findingsOf.size > 0) {
      const td = cell("", "lint");
      const findings = findingsOf.get(sample.metric);
      if (findings) td.append(adviceOf(findings));
    }
    return tr;
  }));
  $("series-box").hidden = samples.length === 0;
  $("series-count").textContent = samples.length === 0 ? "(no series)" : rows.length + " of " + samples.length + " series";
  $("series-cap").hidden = rows.length <= tableRows;
  $("series-cap").textContent = "Showing the first " + tableRows + " of " + rows.length + "; filter to narrow. The text view has them all.";
}
$("series-filter").addEventListener("input", drawRows);
$("series-metric").addEventListener("change", drawRows);
function show(answer, response, took) {
  const statusLine = $("status-line");
  const series = $("series");
  const stages = $("stages");
  stages.replaceChildren();
  refused = !!answer.error;
  showView();
  if (answer.error) {
    statusLine.className = "status failed";
    statusLine.textContent = response.status + ": refused in " + took + " ms";
    series.className = "error";
    series.textContent = answer.error;
    fillTable({});
    for (const part of ["lint-part", "notes-part", "requests-part", "response-part"]) $(part).hidden = true;
    $("report").textContent = "";
    return;
  }
  statusLine.className = "status " + (answer.ok ? "ok" : "failed");
  statusLine.textContent = "Ran in " + took + " ms: " + answer.verdict;
  series.className = "";
  series.textContent = answer.series === "" ? "(no series)" : answer.series;
  fillTable(answer);
  for (const stage of answer.stages || []) {
    const item = document.createElement("li");
    item.className = stage.outcome === "ok" ? "" : "failed";
    item.textContent = stage.stage + " " + stage.outcome + " in " + stage.took + (stage.note ? ": " + stage.note : "");
    stages.append(item);
  }
  const lint = answer.lint || [];
  $("lint-part").hidden = lint.length === 0;
  $("lint").textContent = lint.map((f) => "[" + f.rule + "] metric " + JSON.stringify(f.metric) + ": " + f.message).join("\n");
  const notes = answer.notes || [];
  $("notes-part").hidden = notes.length === 0;
  $("notes").textContent = notes.join("\n");
  const requests = answer.requests || [];
  $("requests-part").hidden = requests.length === 0;
  $("requests").textContent = requests.map((r) =>
    r.method + " " + r.url + (r.page ? " (page " + r.page + ")" : "") + (r.redirect ? " (redirect)" : "") + (r.login ? " (login)" : "") +
    (r.outcome ? " -> " + r.outcome + " in " + r.took : "") +
    (r.headers || []).map((h) => "\n  " + h).join("") +
    (r.withheld ? "\n  not sent: " + r.withheld.join(", ") + " — " + r.withheld_why : "") +
    (r.wait ? "\n  " + r.wait : "")).join("\n");
  const shown = answer.response;
  $("response-part").hidden = !shown;
  if (shown) {
    const parts = [];
    if (shown.status) parts.push(shown.status);
    for (const h of shown.headers || []) parts.push(h);
    for (const f of shown.files || []) parts.push(f.trim());
    if (!shown.files) {
      parts.push("");
      parts.push(shown.body_not_text ? "(" + shown.body_bytes + " bytes, not text)" : shown.body + (shown.body_cut ? "\n... cut" : ""));
    }
    $("response").textContent = parts.join("\n");
  }
  $("report").textContent = answer.report || "";
}
$("run").addEventListener("click", run);
// Nothing runs until the visitor presses Run: the page takes its collector
// and target from its URL, which a link on any page can set, and running on
// opening would make that link a run the visitor never asked for.
</script>
</body>
</html>
`)
