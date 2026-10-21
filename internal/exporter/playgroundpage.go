package exporter

import "html/template"

// The playground's page (playground.go): an editor for one collector's YAML,
// the response it is run against — pasted, or with --web.enable-probe-debug
// fetched from a target through a configured collector — and what the run
// gave: the series a probe would answer, the stages, what was logged and the
// whole debug report. It runs on demand and, while Run as you type is on, a
// moment after each edit; a run cancels the one before it.

type playgroundPage struct {
	// Fetch offers fetching from a target, which needs
	// --web.enable-probe-debug; Collectors are the http collectors a target
	// can be fetched through.
	Fetch      bool
	Collectors []string
	// ProbeTimeoutSeconds is the timeout a run starts with, sent as the
	// scrape timeout Prometheus would send.
	ProbeTimeoutSeconds  int
	Example, ExampleBody string
	Docs                 string
}

// playgroundExample and playgroundExampleBody are what the page starts with:
// a collector and a response it reads.
const playgroundExample = `name: playground
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

const playgroundExampleBody = `{"rooms": [{"name": "kitchen", "celsius": 21.5}, {"name": "attic", "celsius": 17}]}
`

var playgroundTemplate = template.Must(template.New("playground").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Playground · Prometheus Universal Exporter</title>
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
<h1>Playground</h1>
<p class="meta">Write one collector, as it is written under <code>collectors:</code>, and see the series a probe would answer. Nothing is put in force, cached or counted against a collector.</p>
<p class="note">Python is never run here, and a collector that names a file of the server's is refused.{{if not .Fetch}} Fetching from a target needs <code>--web.enable-probe-debug</code> as well; paste a response instead.{{end}}</p>
<noscript><p class="note">The playground needs JavaScript.</p></noscript>
<div class="grid">
<section class="panel">
<h2>Collector</h2>
<label for="collector">YAML of one collector</label>
<textarea id="collector" spellcheck="false" autocomplete="off">{{.Example}}</textarea>
</section>
<section class="panel">
<h2>Response</h2>
{{- if .Fetch}}
<div class="modes" role="radiogroup" aria-label="Response">
<label><input type="radio" name="mode" value="paste" checked> Paste a response</label>
<label><input type="radio" name="mode" value="fetch"> Fetch from a target</label>
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
{{- if .Fetch}}
<div data-mode="fetch" hidden>
<p class="note">The target is fetched as <code>/probe?debug=true</code> would fetch it through the collector chosen here: its request, target policy and credentials, never the request block above. The response is then read by the collector above.</p>
<label for="request-from">Fetch through collector</label>
<select id="request-from">
{{- range .Collectors}}
<option value="{{.}}">{{.}}</option>
{{- else}}
<option value="">no http collector is configured</option>
{{- end}}
</select>
<label for="target">Target</label>
<input id="target" type="text" placeholder="http://host:port" autocomplete="off">
<label for="params">Probe parameters, one <code>name=value</code> a line</label>
<textarea id="params" rows="3" spellcheck="false" autocomplete="off" placeholder="param_tenant=a"></textarea>
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
<div id="notes-part" hidden>
<h3>Logged</h3>
<pre id="notes"></pre>
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
    const kept = store && store.getItem("playground." + id);
    if (kept !== null && kept !== undefined) $(id).value = kept;
  } catch (error) { /* storage refused: start from the example */ }
}
const mode = () => {
  const checked = document.querySelector('input[name="mode"]:checked');
  return checked ? checked.value : "paste";
};
const showMode = () => {
  for (const part of document.querySelectorAll("[data-mode]")) part.hidden = part.dataset.mode !== mode();
};
for (const radio of document.querySelectorAll('input[name="mode"]')) radio.addEventListener("change", () => { showMode(); schedule(); });
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
  const request = { collector: $("collector").value };
  try {
    if (mode() === "fetch") {
      request.request_from = $("request-from").value;
      request.target = $("target").value;
      request.params = lines($("params").value, "=");
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
    const response = await fetch(new URL("playground/run", document.baseURI), {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Playground": "run", "X-Prometheus-Scrape-Timeout-Seconds": String(timeout) },
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
    $("notes-part").hidden = true;
    $("report").textContent = "";
    return;
  }
  statusLine.className = "status " + (answer.ok ? "ok" : "failed");
  statusLine.textContent = "Ran in " + took + " ms: a probe would have answered " + answer.verdict;
  series.className = "";
  series.textContent = answer.series === "" ? "(no series)" : answer.series;
  for (const stage of answer.stages || []) {
    const item = document.createElement("li");
    item.className = stage.outcome === "ok" ? "" : "failed";
    item.textContent = stage.stage + " " + stage.outcome + " in " + stage.took + (stage.note ? ": " + stage.note : "");
    stages.append(item);
  }
  const notes = answer.notes || [];
  $("notes-part").hidden = notes.length === 0;
  $("notes").textContent = notes.join("\n");
  $("report").textContent = answer.report || "";
}
let timer = null;
function schedule() {
  for (const id of ["collector", "body"]) {
    try { if (store) store.setItem("playground." + id, $(id).value); } catch (error) { /* not kept */ }
  }
  if (!$("live").checked) return;
  clearTimeout(timer);
  timer = setTimeout(run, 600);
}
for (const field of document.querySelectorAll("textarea, input, select")) {
  if (field.id === "live" || field.name === "mode") continue;
  field.addEventListener("input", schedule);
}
$("run").addEventListener("click", run);
run();
</script>
</body>
</html>
`))
