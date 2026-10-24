package exporter

import "html/template"

// The debug page (debugpage.go): a collector, written in an editor or chosen
// from the configuration in force, the response it is run on — pasted, or at
// --web.debug=full a target fetched through a configured collector or a
// static target's scrape — and what the run gave: the series a probe would
// answer, the requests, the response, the stages, the metric lint's advice,
// what was logged and the whole debug report. It runs on demand and, while
// Run as you type is on, a moment after each edit; a run cancels the one
// before it. The collectors page links each collector here as
// ?collector=<name>, with &target=<target> at full, and the page starts from
// what the link names.

type debugPage struct {
	// Full offers fetching a target and scraping a static target,
	// --web.debug=full.
	Full bool
	// Collectors are the configured collectors, which can be run by name;
	// HTTPCollectors those a written collector can fetch a target through.
	Collectors     []debugPageCollector
	HTTPCollectors []string
	// ProbeTimeoutSeconds is the timeout a run starts with, sent as the
	// scrape timeout Prometheus would send.
	ProbeTimeoutSeconds  int
	Example, ExampleBody string
	Docs                 string
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

var debugPageTemplate = template.Must(template.New("debug").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Debug · Prometheus Universal Exporter</title>
<style>` + pageStyle + `
main { max-width: 1200px; }
nav { margin-bottom: 16px; font-size: 14px; }
.note { color: var(--muted); font-size: 13px; margin: 4px 0 10px; }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 16px; margin-top: 16px; }
section.panel { border: 1px solid var(--line); border-radius: 10px; padding: 16px; background: var(--panel); min-width: 0; }
section.panel h2 { margin: 0 0 8px; font-size: 17px; }
label { display: block; font-size: 13px; margin: 8px 0 2px; }
input, select, textarea { width: 100%; padding: 6px 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--fg); font: inherit; font-size: 14px; }
textarea { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; resize: vertical; tab-size: 2; }
textarea#collector { min-height: 360px; }
textarea:disabled { opacity: .55; }
.row { display: flex; gap: 8px; flex-wrap: wrap; }
.row > div { flex: 1 1 160px; min-width: 0; }
.row > div.narrow { flex: 0 1 120px; }
.modes { display: flex; gap: 16px; flex-wrap: wrap; font-size: 14px; margin: 4px 0 8px; }
.modes label { display: inline-flex; align-items: center; gap: 6px; margin: 0; font-size: 14px; }
.modes input { width: auto; }
.actions { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; margin-top: 14px; }
button { padding: 7px 16px; border: 1px solid var(--accent); border-radius: 6px; background: var(--accent); color: var(--on-accent); font: inherit; font-size: 14px; cursor: pointer; }
[hidden] { display: none !important; }
.switch { display: inline-flex; align-items: center; gap: 8px; margin: 0; font-size: 14px; cursor: pointer; }
.switch input { appearance: none; -webkit-appearance: none; position: relative; width: 36px; height: 20px; flex: none; margin: 0; padding: 0; border-radius: 10px; border: 1px solid var(--muted); background: transparent; cursor: pointer; transition: background .15s; }
.switch input::before { content: ""; position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--muted); transition: transform .15s; }
.switch input:checked { background: var(--accent); border-color: var(--accent); }
.switch input:checked::before { transform: translateX(16px); background: var(--on-accent); }
.switch input:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
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
<nav><a href="./">← Prometheus Universal Exporter</a> · <a href="collectors">Collectors</a></nav>
<h1>Debug</h1>
<p class="meta">Run one collector — written here, as it is written under <code>collectors:</code>, or one of the configuration — and see the series a probe would answer. Nothing is put in force, cached or counted against a collector.</p>
<p class="note">A collector written here never runs Python, and one that names a file of the server's is refused; a configured collector runs as it is configured.{{if not .Full}} Debugging is at <code>edit</code>: collectors run on a pasted response only, and nothing is fetched. Fetching a target needs <code>--web.debug=full</code>.{{end}}</p>
<noscript><p class="note">The debug page needs JavaScript.</p></noscript>
<div class="grid">
<section class="panel">
<h2>Collector</h2>
<label for="configured">Collector to run</label>
<select id="configured">
<option value="">The one written below</option>
{{- if .Collectors}}
<optgroup label="Configured">
{{- range .Collectors}}
<option value="{{.Name}}">{{.Name}} ({{.RequestType}})</option>
{{- end}}
</optgroup>
{{- end}}
</select>
<label for="collector">YAML of one collector</label>
<textarea id="collector" spellcheck="false" autocomplete="off">{{.Example}}</textarea>
</section>
<section class="panel">
<h2>Response</h2>
{{- if .Full}}
<div class="modes" role="radiogroup" aria-label="Response">
<label><input type="radio" name="mode" value="paste" checked> Paste a response</label>
<label><input type="radio" name="mode" value="fetch"> Fetch a target</label>
<label><input type="radio" name="mode" value="static"> Scrape a static target</label>
</div>
{{- end}}
<div data-mode="paste">
<div class="row">
<div><label for="content-type">Content-Type</label>
<input id="content-type" type="text" value="application/json" autocomplete="off"></div>
<div class="narrow"><label for="status">Status</label>
<input id="status" type="number" min="100" max="599" value="200"></div>
</div>
<label for="headers">Other headers, one <code>Name: value</code> a line</label>
<textarea id="headers" rows="2" spellcheck="false" autocomplete="off"></textarea>
<label for="body">Body</label>
<textarea id="body" rows="14" spellcheck="false" autocomplete="off">{{.ExampleBody}}</textarea>
</div>
{{- if .Full}}
<div data-mode="fetch" hidden>
<p class="note">The target is fetched as <code>/probe?debug=true</code> fetches it: through a configured collector's request, target policy and credentials. A collector written here makes no request of its own; it reads what the collector chosen here fetched.</p>
<label for="request-from">Fetch through</label>
<select id="request-from">
<option value="">The configured collector run</option>
{{- range .HTTPCollectors}}
<option value="{{.}}">{{.}}</option>
{{- end}}
</select>
<label for="target">Target</label>
<input id="target" type="text" placeholder="http://host:port" autocomplete="off">
<label for="params">Probe parameters, one <code>name=value</code> a line</label>
<textarea id="params" rows="3" spellcheck="false" autocomplete="off" placeholder="param_tenant=a"></textarea>
</div>
<div data-mode="static" hidden>
<p class="note">The static target is scraped once as its schedule scrapes it, with its own settings and credentials, and read by the collector run. Nothing is published.</p>
<label for="static-target">Static target name</label>
<input id="static-target" type="text" autocomplete="off">
</div>
{{- end}}
<div class="actions">
<button type="button" id="run">Run</button>
<label class="switch"><input type="checkbox" id="live" role="switch" checked> Run as you type</label>
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
<p class="meta"><a href="{{.Docs}}">Documentation</a></p>
</main>
<script>
"use strict";
const $ = (id) => document.getElementById(id);
const store = (() => { try { return window.localStorage; } catch (error) { return null; } })();
// The collector and the body are kept in this browser between visits.
for (const id of ["collector", "body"]) {
  try {
    const kept = store && store.getItem("debug." + id);
    if (kept !== null && kept !== undefined) $(id).value = kept;
  } catch (error) { /* storage refused: start from the example */ }
}
// A link from the collectors page names the collector, and at full the
// target or the static target.
const asked = new URLSearchParams(location.search);
const pick = (select, value) => {
  for (const option of select.options) if (option.value === value) { select.value = value; return true; }
  return false;
};
if (asked.get("collector")) pick($("configured"), asked.get("collector"));
const choose = (value) => {
  const radio = document.querySelector('input[name="mode"][value="' + value + '"]');
  if (radio) radio.checked = true;
};
if (asked.get("target") !== null && $("target")) { choose("fetch"); $("target").value = asked.get("target"); }
if (asked.get("static_target") !== null && $("static-target")) { choose("static"); $("static-target").value = asked.get("static_target"); }
const mode = () => {
  const checked = document.querySelector('input[name="mode"]:checked');
  return checked ? checked.value : "paste";
};
const showMode = () => {
  for (const part of document.querySelectorAll("[data-mode]")) part.hidden = part.dataset.mode !== mode();
  // A configured collector is run as configured: the editor is not read.
  $("collector").disabled = $("configured").value !== "";
};
for (const radio of document.querySelectorAll('input[name="mode"]')) radio.addEventListener("change", () => { showMode(); schedule(); });
$("configured").addEventListener("change", showMode);
showMode();
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
      if ($("request-from").value !== "") request.request_from = $("request-from").value;
      request.target = $("target").value;
      request.params = lines($("params").value, "=");
    } else if (mode() === "static") {
      request.static_target = $("static-target").value;
    } else {
      request.body = $("body").value;
      request.content_type = $("content-type").value;
      request.status = Number($("status").value) || 0;
      request.headers = lines($("headers").value, ":");
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
let timer = null;
function schedule() {
  for (const id of ["collector", "body"]) {
    try { if (store) store.setItem("debug." + id, $(id).value); } catch (error) { /* not kept */ }
  }
  if (!$("live").checked) return;
  clearTimeout(timer);
  timer = setTimeout(run, 600);
}
for (const field of document.querySelectorAll("textarea, input, select")) {
  if (field.id === "live" || field.name === "mode") continue;
  field.addEventListener(field.tagName === "SELECT" ? "change" : "input", schedule);
}
$("run").addEventListener("click", run);
// Nothing runs until the visitor presses Run or edits a field: the page
// takes its collector and target from its URL, which a link on any page
// can set, and running on opening would make that link a run the visitor
// never asked for.
</script>
</body>
</html>
`))
