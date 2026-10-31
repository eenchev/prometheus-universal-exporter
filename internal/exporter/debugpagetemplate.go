package exporter

// The debug page (debugpage.go): a collector, the playground, one written in
// an editor, or one chosen from the configuration in force, the response it
// is run on — pasted, or at --web.debug=full a target fetched through a
// configured collector or a static target's scrape — and what the run gave:
// the series a probe would answer, the requests, the response, the stages,
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
}

// debugForms are the forms of the playground and of each configured
// collector by name (debugForm), which the page's script reads.
type debugForms struct {
	Playground debugForm            `json:"playground"`
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
`+pageIcon+`
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
</style>
</head>
<body>
<main>
{{template "chrome" .Chrome}}
<h1>Debug</h1>
<p class="meta">Run one collector — one of the configuration, or the playground, a collector written on the page as it is written under <code>collectors:</code> — and see the series a probe would answer. Nothing is put in force, cached or counted against a collector.</p>
<p class="note">The playground never runs Python, and one that names a file of the server's is refused; a configured collector runs as it is configured.{{if not .Full}} Debugging is at <code>edit</code>: collectors run on a pasted response only, and nothing is fetched. Fetching a target needs <code>--web.debug=full</code>.{{end}}</p>
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
</div>
<p class="note" id="collector-note"{{if not .Chosen}} hidden{{end}}>As its file writes it: credentials and file paths withheld, the keys it takes from its file's defaults marked. <em>Edit a copy</em> makes the playground a copy of it, which runs in its place and fetches a target through it.</p>
<textarea id="collector" spellcheck="false" autocomplete="off" wrap="off" data-example="{{.Example}}"{{if .Chosen}} readonly data-shown="{{.Chosen}}"{{end}}>{{if .Chosen}}{{.ChosenYAML}}{{else}}{{.Example}}{{end}}</textarea>
</section>
<section class="panel">
<h2>Response</h2>
{{- if .Full}}
<div class="modes" role="radiogroup" aria-label="Response">
<label><input type="radio" name="mode" value="paste" checked> Paste a response</label>
<label><input type="radio" name="mode" value="fetch" id="mode-fetch"{{if not .Chosen}} disabled{{end}}> Fetch a target</label>
<label><input type="radio" name="mode" value="static" id="mode-static"{{if not .StaticTargets}} disabled{{end}}> Scrape a static target</label>
</div>
<p class="note" id="fetch-unavailable"{{if .Chosen}} hidden{{end}}>Choose a configured collector, or <em>Edit a copy</em> of one, to fetch a target or scrape a static target.</p>
<p class="note" id="static-unavailable"{{if not .StaticNote}} hidden{{end}}>{{.StaticNote}}</p>
{{- end}}
<div data-mode="paste">
<div class="row">
<div id="content-type-part"{{if not .Form.ContentType}} hidden{{end}}><label for="content-type">Content-Type</label>
<input id="content-type" type="text" value="application/json" autocomplete="off"></div>
<div class="narrow" id="status-part"{{if not .Form.HTTPAnswer}} hidden{{end}}><label for="status">Status</label>
<input id="status" type="number" min="100" max="599" value="200"></div>
</div>
<div id="headers-part"{{if not .Form.HTTPAnswer}} hidden{{end}}>
<label for="headers">Other headers, one <code>Name: value</code> a line</label>
<textarea id="headers" rows="2" spellcheck="false" autocomplete="off"></textarea>
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
<div id="params-part"{{if not .Form.Params}} hidden{{end}}>
<label for="params">Probe parameters, one <code>name=value</code> a line</label>
<textarea id="params" rows="3" spellcheck="false" autocomplete="off" placeholder="{{.Form.Params}}"></textarea>
</div>
<p class="note" id="no-params"{{if .Form.Params}} hidden{{end}}>This collector takes no probe parameters.</p>
</div>
<div data-mode="static" hidden>
<p class="note">The static target is scraped once as its schedule scrapes it, with its own settings and credentials, through the collector chosen, or the one the playground was copied from, and read by the collector chosen. Nothing is published.</p>
<label for="static-target">Static target</label>
<select id="static-target" data-for="{{.Chosen}}"{{if not .StaticTargets}} disabled{{end}}>
{{- range .StaticTargets}}
<option value="{{.Name}}"{{if eq .Name $.StaticTarget}} selected{{end}}>{{.Name}}{{if .Target}} — {{.Target}}{{end}}</option>
{{- end}}
</select>
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
<h3>Series a probe would answer</h3>
<pre id="series"></pre>
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
// The playground and the body are kept in this browser between visits, and
// so is the configured collector the playground is a copy of, if it is one:
// written is the playground, which the editor shows while no configured
// collector is chosen, and copiedFrom the collector Edit a copy copied it
// from, which a target is fetched through while the playground runs.
let written = $("collector").dataset.example, copiedFrom = "";
for (const id of ["collector", "body", "copied"]) {
  try {
    const kept = store && store.getItem("debug." + id);
    if (kept === null || kept === undefined) continue;
    if (id === "collector") written = kept;
    else if (id === "copied") copiedFrom = kept;
    else $(id).value = kept;
  } catch (error) { /* storage refused: start from the example */ }
}
// A link from the collectors page names the collector, and at full the
// target or the static target. The server has chosen the collector the
// link names, or the one of the static target it names, and the browser's
// own memory of the form does not choose another.
const asked = new URLSearchParams(location.search);
const pick = (select, value) => {
  for (const option of select.options) if (option.value === value) { select.value = value; return true; }
  return false;
};
// A copy of a collector a reload has since taken away fetches through
// nothing.
if (copiedFrom !== "" && ![...$("configured").options].some((option) => option.value !== "" && option.value === copiedFrom)) copiedFrom = "";
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
// (debugForm): the one run reads what is pasted, the playground being an
// http collector, and a target is probed through fetcher().
const forms = JSON.parse($("forms").textContent);
const formOf = (name) => (name !== "" && forms.collectors[name]) || forms.playground;
const readerForm = () => formOf($("configured").value);
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
  $("params-part").hidden = !through.params;
  $("no-params").hidden = !!through.params;
  $("params").placeholder = through.params || "";
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
}
// The static targets offered are those scraped through fetcher(), read from
// /debug/collector whenever it changes, so they follow a reload of the
// static target file; the page opens with those of the collector chosen.
// A response comes through that collector, a fetched target's as a static
// target's, so the example playground, copied from none, scrapes none.
let listedFor = $("static-target") ? $("static-target").dataset.for : "";
async function showStaticTargets() {
  const select = $("static-target"), through = fetcher();
  if (!select || through === listedFor) return;
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
function keep() {
  try {
    if (store) {
      store.setItem("debug.collector", written);
      store.setItem("debug.copied", copiedFrom);
      store.setItem("debug.body", $("body").value);
    }
  } catch (error) { /* not kept */ }
}
$("collector").addEventListener("input", () => { if (!$("collector").readOnly) { written = $("collector").value; keep(); } });
$("body").addEventListener("input", keep);
$("edit-copy").addEventListener("click", () => {
  written = $("collector").value;
  copiedFrom = $("configured").value;
  $("configured").value = "";
  showCollector();
  keep();
  $("collector").focus();
});
for (const radio of document.querySelectorAll('input[name="mode"]')) radio.addEventListener("change", showMode);
$("configured").addEventListener("change", showCollector);
showMode();
showCollector();
// "Name: value" and "name=value" lines, a blank one skipped.
const lines = (text, separator) => {
  const out = {};
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    const at = line.indexOf(separator);
    if (at < 0) throw new Error("the line " + JSON.stringify(line) + " has no " + separator);
    out[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return out;
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
      request.params = through.params ? lines($("params").value, "=") : {};
    } else if (mode() === "static") {
      request.static_target = $("static-target").value;
    } else {
      const reads = readerForm();
      request.body = $("body").value;
      request.content_type = reads.content_type ? $("content-type").value : "";
      request.status = reads.http_answer ? Number($("status").value) || 0 : 0;
      request.headers = reads.http_answer ? lines($("headers").value, ":") : {};
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
function show(answer, response, took) {
  const statusLine = $("status-line");
  const series = $("series");
  const stages = $("stages");
  stages.replaceChildren();
  if (answer.error) {
    statusLine.className = "status failed";
    statusLine.textContent = response.status + ": refused in " + took + " ms";
    series.className = "error";
    series.textContent = answer.error;
    for (const part of ["lint-part", "notes-part", "requests-part", "response-part"]) $(part).hidden = true;
    $("report").textContent = "";
    return;
  }
  statusLine.className = "status " + (answer.ok ? "ok" : "failed");
  statusLine.textContent = "Ran in " + took + " ms: " + answer.verdict;
  series.className = "";
  series.textContent = answer.series === "" ? "(no series)" : answer.series;
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
