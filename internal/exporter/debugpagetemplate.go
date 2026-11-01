package exporter

// The debug page (debugpage.go): a collector, the playground, one written in
// an editor, or one chosen from the configuration in force, the response it
// is run on — pasted, or at --web.debug=full a target fetched through a
// configured collector or a static target's scrape — and what the run gave:
// the series a probe would answer, as a table and as text, and with OTLP on
// what the next export would send of them, the requests, the response, the stages,
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
	// (debugcollector.go); ChosenCopy is what Edit a copy puts in the
	// playground in its place, and ChosenCopyNote what the page says of it
	// (debugcopy.go).
	Chosen, ChosenYAML         string
	ChosenCopy, ChosenCopyNote string
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
	// Dangerous is --web.debug.dangerous: the playground runs Python, names
	// the server's files and probes a target with its own request.
	Dangerous bool
	// OTLP offers the OTLP tab, with otlp.enabled, and OTLPLogs its log
	// records, with otlp.logs on too (debugotlp.go).
	OTLP, OTLPLogs bool
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
textarea[readonly] { background: var(--code); }
/* The collector's editor: a gutter of line numbers beside the textarea, and
   behind it a band for each line a run's error names. The textarea does not
   wrap, so each of its lines is one line box of --editor-line, which the
   gutter and the bands are laid out by, and its scroll is followed. */
.editor { --editor-line: 20px; --editor-pad: 6px; display: flex; margin-top: 2px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); overflow: hidden; }
.editor.readonly { background: var(--code); }
.editor:focus-within { outline: 2px solid var(--accent); outline-offset: -1px; }
.editor .gutter { flex: 0 0 auto; position: relative; overflow: hidden; width: 4ch; min-width: 3.2em; border-right: 1px solid var(--line); background: var(--code); color: var(--muted); font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; line-height: var(--editor-line); text-align: right; user-select: none; }
/* Laid out of the flow, so the numbers of a long YAML never make the editor
   taller than its textarea. */
.editor .gutter-lines { position: absolute; top: 0; left: 4px; right: 6px; padding-top: var(--editor-pad); will-change: transform; }
.editor .band-layer { position: relative; will-change: transform; }
.editor .gutter-lines div { height: var(--editor-line); white-space: nowrap; }
.editor .gutter-lines div.marked { color: var(--on-accent); background: var(--bad); border-radius: 3px; font-weight: 600; }
.editor .text { flex: 1 1 auto; position: relative; min-width: 0; }
.editor .bands { position: absolute; inset: 0; overflow: hidden; pointer-events: none; }
.editor .bands .band { position: absolute; left: 0; right: 0; height: var(--editor-line); background: var(--danger-bg); box-shadow: inset 3px 0 0 var(--bad); }
.editor textarea { position: relative; display: block; min-height: 360px; padding: var(--editor-pad) 8px; border: 0; border-radius: 0; background: transparent; line-height: var(--editor-line); white-space: pre; overflow-wrap: normal; overflow: auto; }
.editor textarea:focus { outline: none; }
.editor-hint { margin: 4px 0 0; }
/* Level with Run and the timeout's box, at the foot of the row, and not
   with the row's middle, which the timeout's label above its box lowers. */
.actions .run-hint { flex: 0 1 auto; align-self: flex-end; display: flex; align-items: center; min-height: 36px; margin: 0; }
pre button.line-link { padding: 0; margin: 0; border: 0; background: transparent; color: var(--bad); font: inherit; text-decoration: underline; cursor: pointer; }
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
table.series td.otlp-notes { font-family: inherit; min-width: 12em; }
ul.otlp-notes { margin: 4px 0 8px; padding-left: 18px; font-size: 13px; color: var(--muted); }
#otlp-resource th { position: static; width: 1%; }
.otlp-test { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
.otlp-test button { margin: 0; }
.otlp-test button:disabled { opacity: 0.5; cursor: not-allowed; }
/* The JSON viewer (jsonView): a line for each key or item, indented by
   its node, with the node's fold button in the gutter before it, so keys
   and items line up whether they fold or not. A long string wraps within
   the panel, which scrolls past its height. */
.json-view { margin-top: 8px; padding: 10px 10px 10px 4px; max-height: 520px; overflow: auto; background: var(--bg); border: 1px solid var(--line); border-radius: 6px; font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; overflow-wrap: anywhere; }
.json-view .jv-line { position: relative; padding-left: 1.6em; }
.json-view .jv-kids { margin-left: 2ch; }
.json-view .jv-kids > .jv-node { content-visibility: auto; contain-intrinsic-size: auto 18px; }
.json-view button.jv-toggle { position: absolute; left: 0; top: 0; width: 1.5em; height: 1.5em; padding: 0; margin: 0; border: 0; border-radius: 3px; background: transparent; color: var(--muted); font: inherit; line-height: 1.5; cursor: pointer; }
.json-view button.jv-toggle:hover { color: var(--accent); }
.json-view button.jv-toggle:focus-visible { outline: 2px solid var(--accent); outline-offset: -1px; }
.json-view .jv-closed > .jv-kids, .json-view .jv-closed > .jv-tail { display: none; }
.json-view .jv-node:not(.jv-closed) > .jv-line > .jv-fold { display: none; }
.json-view .jv-fold { cursor: pointer; }
.json-view .jv-key { color: var(--json-key); }
.json-view .jv-string { color: var(--json-string); }
.json-view .jv-number { color: var(--json-number); }
.json-view .jv-boolean { color: var(--json-boolean); }
.json-view .jv-null { color: var(--json-null); }
.json-view .jv-punct, .json-view .jv-summary { color: var(--json-punct); }
.json-view .jv-hint { color: var(--fg); }
.json-view .jv-doc + .jv-doc { margin-top: 10px; padding-top: 10px; border-top: 1px dashed var(--line); }
.json-view pre.jv-plain { padding: 0 0 0 6px; max-height: none; border: 0; overflow: visible; }
.json-view p.note { margin: 0 0 6px 6px; }
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
p.danger { margin: 12px 0 4px; padding: 10px 14px; border: 1px solid var(--bad); border-left-width: 6px; border-radius: 6px; background: var(--danger-bg); color: var(--fg); font-size: 14px; }
p.danger strong { color: var(--bad); }
</style>
</head>
<body>
<main{{if .BasicAuth}} data-basic-auth{{end}}{{if .Dangerous}} data-dangerous{{end}}>
{{template "chrome" .Chrome}}
<h1>Debug</h1>
<p class="meta">Run one collector — one of the configuration, or the playground, a collector written on the page as it is written under <code>collectors:</code> — and see the series a probe would answer. Nothing is put in force, cached or counted against a collector.</p>
{{- if .Dangerous}}
<p class="danger" id="danger" role="alert"><strong>Dangerous debug mode</strong> (<code>--web.debug.dangerous</code>): the playground runs Python and reads the server's files with the exporter's permissions, and probing a target with it makes its own request, to any target it names. Anyone who can open this page can do the same. Turn the flag off once the collector is fixed.</p>
{{- else}}
<p class="note">The playground never runs Python, and one that names a file of the server's is refused; a configured collector runs as it is configured.{{if not .Full}} Debugging is at <code>edit</code>: collectors run on a pasted response only, and nothing is fetched. Probing a target needs <code>--web.debug=full</code>.{{end}}</p>
{{- end}}
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
<p class="note" id="copy-note" role="status" hidden></p>
<div class="editor{{if .Chosen}} readonly{{end}}" id="collector-editor">
<div class="gutter" aria-hidden="true"><div class="gutter-lines" id="collector-gutter"></div></div>
<div class="text"><div class="bands" id="collector-bands" aria-hidden="true"></div>
<textarea id="collector" spellcheck="false" autocomplete="off" wrap="off" data-example="{{.Example}}"{{if .Chosen}} readonly data-shown="{{.Chosen}}" data-copy="{{.ChosenCopy}}" data-copy-note="{{.ChosenCopyNote}}"{{end}}>{{if .Chosen}}{{.ChosenYAML}}{{else}}{{.Example}}{{end}}</textarea>
</div>
</div>
<p class="note editor-hint" id="editor-hint">In the playground Tab indents by two spaces and Shift+Tab outdents; Esc, then Tab, leaves the editor.</p>
</section>
<section class="panel">
<h2>Response</h2>
{{- if .Full}}
<div class="modes" role="radiogroup" aria-label="Response">
<label><input type="radio" name="mode" value="paste" checked> Paste a response</label>
<label><input type="radio" name="mode" value="fetch" id="mode-fetch"{{if not (or .Chosen .Dangerous)}} disabled{{end}}> Probe a target</label>
<label><input type="radio" name="mode" value="static" id="mode-static"{{if not .StaticTargets}} disabled{{end}}> Scrape a static target</label>
</div>
<p class="note" id="fetch-unavailable"{{if .Chosen}} hidden{{end}}>Choose a configured collector, or <em>Edit a copy</em> of one, to {{if not .Dangerous}}probe a target or {{end}}scrape a static target.</p>
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
{{- if .Dangerous}}
<p class="note">The target is probed as <code>/probe?debug=true</code> probes it: through a configured collector's request, target policy and credentials, or, for a playground copied from none, through the playground's own request, with the target policy and credentials it writes.</p>
{{- else}}
<p class="note">The target is probed as <code>/probe?debug=true</code> probes it: through a configured collector's request, target policy and credentials. The playground makes no request of its own; it reads what the collector it was copied from answered.</p>
{{- end}}
<p class="note" id="fetched-through">{{if .Chosen}}Probed through {{.Chosen}}, the collector chosen.{{else if .Dangerous}}Probed through the playground's own request.{{end}}</p>
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
<button type="button" id="run" title="Run (Ctrl+Enter, or ⌘+Enter on a Mac, from anywhere on the page)" aria-keyshortcuts="Control+Enter Meta+Enter">Run</button>
<p class="note run-hint" id="run-hint">Ctrl+Enter runs, ⌘+Enter on a Mac</p>
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
{{- if .OTLP}}
<button type="button" role="tab" id="view-otlp" data-view="otlp" aria-controls="series-otlp-part" aria-selected="false">OTLP</button>
{{- end}}
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
{{- if .OTLP}}
<div id="series-otlp-part" role="tabpanel" aria-labelledby="view-otlp" hidden>
<p class="note" id="otlp-none">Not run yet. Once a collector has run, this shows what the next OTLP export would send of its series, made by the export's own code; nothing of a run is queued or sent.</p>
<div id="otlp-shown" hidden>
<ul class="otlp-notes" id="otlp-notes"></ul>
<h3>Resource</h3>
<div class="table-box"><table class="series" id="otlp-resource"><tbody id="otlp-resource-rows"></tbody></table></div>
<ul class="otlp-notes" id="otlp-resource-notes"></ul>
<p class="note" id="otlp-scope"></p>
<h3>Data points</h3>
<div class="row series-filters">
<div><label for="otlp-filter">Filter: name, attribute, value or note</label>
<input id="otlp-filter" type="search" autocomplete="off" spellcheck="false"></div>
</div>
<p class="note" id="otlp-count"></p>
<div class="table-box" id="otlp-box" hidden><table class="series" id="otlp-table"><thead><tr id="otlp-columns"></tr></thead><tbody id="otlp-rows"></tbody></table></div>
<div id="otlp-left-part" hidden>
<h3>Left out</h3>
<ul class="lint-unplaced" id="otlp-left"></ul>
<pre id="otlp-clashes" hidden></pre>
</div>
<details id="otlp-body-part">
<summary>Request body (ExportMetricsServiceRequest)</summary>
<div class="copy-row"><button type="button" class="secondary" id="otlp-body-copy">Copy</button><button type="button" class="secondary" data-json-all="open" aria-controls="otlp-body">Expand all</button><button type="button" class="secondary" data-json-all="close" aria-controls="otlp-body">Collapse all</button><span class="copy-done" id="otlp-body-copied"></span></div>
<div class="json-view" id="otlp-body"></div>
</details>
{{- if .OTLPLogs}}
<div id="otlp-logs-part" hidden>
<h3>Log records</h3>
<ul class="otlp-notes" id="otlp-logs-notes"></ul>
<p class="note" id="otlp-logs-count"></p>
<div class="table-box" id="otlp-logs-box" hidden><table class="series" id="otlp-logs-table"><thead><tr><th scope="col">time</th><th scope="col">severity</th><th scope="col">body</th><th scope="col">attributes</th><th scope="col">exported</th></tr></thead><tbody id="otlp-logs-rows"></tbody></table></div>
<details id="otlp-logs-body-part">
<summary>Request body (ExportLogsServiceRequest)</summary>
<div class="copy-row"><button type="button" class="secondary" id="otlp-logs-copy">Copy</button><button type="button" class="secondary" data-json-all="open" aria-controls="otlp-logs-body">Expand all</button><button type="button" class="secondary" data-json-all="close" aria-controls="otlp-logs-body">Collapse all</button><span class="copy-done" id="otlp-logs-copied"></span></div>
<div class="json-view" id="otlp-logs-body"></div>
</details>
</div>
{{- end}}
</div>
<h3>Test the endpoint</h3>
<div class="otlp-test">
<button type="button" class="secondary" id="otlp-test-metrics" data-signal="metrics"{{if not .Full}} disabled{{end}}>Test the metrics endpoint</button>
{{- if .OTLPLogs}}
<button type="button" class="secondary" id="otlp-test-logs" data-signal="logs"{{if not .Full}} disabled{{end}}>Test the logs endpoint</button>
{{- end}}
</div>
{{- if .Full}}
<p class="note">Sends one empty export to the endpoint otlp configures, with its headers, TLS, compression and timeout, once, without retrying. It counts in none of the export's self-metrics and changes nothing the status page shows.</p>
{{- else}}
<p class="note" id="otlp-test-unavailable">Testing the endpoint sends it a request and shows what it answers, so it needs <code>--web.debug=full</code>.</p>
{{- end}}
<pre id="otlp-test-result" hidden></pre>
</div>
{{- end}}
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
// With --web.debug.dangerous a playground copied from none makes its own
// request, and is asked for a target as its request type asks for one.
const dangerous = document.querySelector("main").hasAttribute("data-dangerous");
const throughForm = () => fetcher() === "" ? playgroundForm() : formOf(fetcher());
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
      if (event.key !== "Enter" || event.ctrlKey || event.metaKey || rows[rows.length - 1] !== row) return;
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
  const through = throughForm();
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
  radio.disabled = through === "" && !dangerous;
  $("fetch-unavailable").hidden = through !== "";
  if (radio.disabled && radio.checked) choose("paste");
  $("fetched-through").textContent = through === "" ? (dangerous ? "Probed through the playground's own request." : "") : $("configured").value !== ""
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
// The collector's editor is a textarea, which does not wrap, with a gutter
// of its line numbers beside it and, behind it, a band on each line a run's
// error names, the line's number marked in the gutter too. Each line of the
// textarea is one line box of the height the gutter's lines and the bands
// are laid out by, so both follow its scroll, sideways scrolling leaving
// them where they are. The marks go with the next edit, which moves the
// lines they were of.
const editorArea = $("collector"), gutterLines = $("collector-gutter"), bandLayer = document.createElement("div");
bandLayer.className = "band-layer";
$("collector-bands").append(bandLayer);
editorArea.setAttribute("aria-describedby", "editor-hint");
let marked = [];
const editorLine = () => parseFloat(getComputedStyle(editorArea).lineHeight) || 20;
const editorPad = () => parseFloat(getComputedStyle(editorArea).paddingTop) || 0;
function followScroll() {
  const shift = "translateY(" + -editorArea.scrollTop + "px)";
  gutterLines.style.transform = shift;
  bandLayer.style.transform = shift;
}
function drawGutter() {
  const count = editorArea.value.split("\n").length;
  // Room for the widest number, and the gutter's padding.
  gutterLines.parentElement.style.width = "calc(" + String(count).length + "ch + 12px)";
  while (gutterLines.childElementCount > count) gutterLines.lastElementChild.remove();
  for (let n = gutterLines.childElementCount + 1; n <= count; n++) {
    const number = document.createElement("div");
    number.textContent = String(n);
    gutterLines.append(number);
  }
  followScroll();
}
// markLines marks the lines numbered in lines, those the editor has.
function markLines(lines) {
  for (const n of marked) if (gutterLines.children[n - 1]) gutterLines.children[n - 1].classList.remove("marked");
  marked = lines.filter((n) => n >= 1 && n <= gutterLines.childElementCount);
  const height = editorLine(), pad = editorPad();
  bandLayer.replaceChildren(...marked.map((n) => {
    gutterLines.children[n - 1].classList.add("marked");
    const band = document.createElement("div");
    band.className = "band";
    band.style.top = pad + (n - 1) * height + "px";
    return band;
  }));
  followScroll();
}
function setEditor(text) {
  editorArea.value = text;
  markLines([]);
  drawGutter();
}
editorArea.addEventListener("scroll", followScroll);
// jumpTo selects the line numbered n in the editor and scrolls it into the
// middle of the view.
function jumpTo(n) {
  const lines = editorArea.value.split("\n");
  if (n < 1 || n > lines.length) return;
  let start = 0;
  for (let i = 0; i < n - 1; i++) start += lines[i].length + 1;
  editorArea.focus();
  editorArea.setSelectionRange(start, start + lines[n - 1].length);
  editorArea.scrollTop = Math.max(0, editorPad() + (n - 1) * editorLine() - editorArea.clientHeight / 2);
  editorArea.scrollLeft = 0;
  followScroll();
}
// linesNamed are the lines an error of the loader names, "line 4: …" or
// "yaml: line 4: …", counted from the collector's first line, as the
// editor numbers them; each once, in order.
const lineNamed = /\bline (\d+)\b/g;
const linesNamed = (text) => [...new Set([...text.matchAll(lineNamed)].map((m) => Number(m[1])))].sort((a, b) => a - b);
// showError writes an error into pre, each "line N" a button that selects
// that line in the editor when linked; as text alone, never as HTML.
function showError(pre, text, linked) {
  pre.replaceChildren();
  let at = 0;
  for (const m of text.matchAll(lineNamed)) {
    pre.append(text.slice(at, m.index));
    if (linked) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "line-link";
      button.textContent = m[0];
      button.title = "Select line " + m[1] + " in the editor";
      button.addEventListener("click", () => jumpTo(Number(m[1])));
      pre.append(button);
    } else {
      pre.append(m[0]);
    }
    at = m.index + m[0].length;
  }
  pre.append(text.slice(at));
}
// In the playground Tab puts in two spaces, or indents the lines selected
// by two, and Shift+Tab takes up to two from the start of each line the
// selection is on. Esc, then Tab, leaves the editor, as Tab does anywhere
// else, so the keyboard is never caught in it.
let escaped = false;
editorArea.addEventListener("focus", () => { escaped = false; });
editorArea.addEventListener("keydown", (event) => {
  if (event.key === "Escape") { escaped = true; return; }
  if (event.key === "Shift") return;
  const leave = escaped;
  escaped = false;
  if (event.key !== "Tab" || leave || editorArea.readOnly || event.ctrlKey || event.altKey || event.metaKey) return;
  event.preventDefault();
  indent(event.shiftKey);
});
// edit replaces start to end with text, through the browser's own editing
// where it has it, which keeps its undo, and selects from to to after.
function edit(start, end, text, from, to) {
  editorArea.setSelectionRange(start, end);
  let done = false;
  try { done = document.execCommand("insertText", false, text); } catch (error) { done = false; }
  if (!done || editorArea.value.slice(start, start + text.length) !== text) {
    editorArea.setRangeText(text, start, end, "end");
    editorArea.dispatchEvent(new Event("input", { bubbles: true }));
  }
  editorArea.setSelectionRange(from, to);
}
function indent(out) {
  const value = editorArea.value, start = editorArea.selectionStart, end = editorArea.selectionEnd;
  const many = value.slice(start, end).includes("\n");
  if (!out && !many) { edit(start, end, "  ", start + 2, start + 2); return; }
  // The lines the selection is on, a line it ends at the start of left out.
  const from = value.lastIndexOf("\n", start - 1) + 1;
  let to = value.indexOf("\n", end > start && value[end - 1] === "\n" ? end - 1 : end);
  if (to < 0) to = value.length;
  const lines = value.slice(from, to).split("\n");
  const cuts = lines.map((line) => out ? (line.startsWith("  ") ? 2 : line.startsWith(" ") ? 1 : 0) : -2);
  const changed = lines.map((line, i) => cuts[i] < 0 ? "  " + line : line.slice(cuts[i])).join("\n");
  if (changed === value.slice(from, to)) return;
  if (many) { edit(from, to, changed, from, from + changed.length); return; }
  const at = Math.max(from, start - cuts[0]);
  edit(from, to, changed, at, Math.max(at, end - cuts[0]));
}
// The editor shows what runs: the playground, or the configured collector
// chosen, as /debug/collector shows it, read-only, since it runs as
// configured. Edit a copy makes the playground a copy of that one.
// Each is kept as /debug/collector answers it: the YAML shown, and the copy
// Edit a copy makes of it, which leaves out what the playground refuses,
// with the note that says what (debugcopy.go).
const configuredYAML = new Map();
if ($("collector").dataset.shown) {
  configuredYAML.set($("collector").dataset.shown, { yaml: $("collector").value, copy: $("collector").dataset.copy, note: $("collector").dataset.copyNote || "" });
}
let copyNote = "";
async function showCollector() {
  const area = $("collector"), name = $("configured").value;
  area.readOnly = name !== "";
  $("collector-editor").classList.toggle("readonly", name !== "");
  markLines([]);
  // A playground copied from none, the example, has nothing left out.
  if (copiedFrom === "") copyNote = "";
  $("copy-note").textContent = copyNote;
  $("copy-note").hidden = name !== "" || copiedFrom === "" || copyNote === "";
  $("edit-copy").hidden = name === "";
  // Edit a copy copies the collector's YAML, which a collector still being
  // read, or one that could not be, has none of: the editor's text is then
  // what the page says of it, no collector to copy.
  $("edit-copy").disabled = name !== "" && !configuredYAML.has(name);
  $("back-to-example").hidden = name !== "" || copiedFrom === "";
  $("collector-note").hidden = name === "";
  $("collector-label").textContent = name !== "" ? "The configuration of " + name + ", which runs as configured (read-only)"
    : copiedFrom !== "" ? "Playground, a copy of " + copiedFrom + ": the YAML of a collector, which runs"
    : "Playground: the YAML of a collector, which runs";
  showFetch();
  showForm();
  showStaticTargets();
  fitBody();
  if (name === "") { setEditor(written); return; }
  if (configuredYAML.has(name)) { setEditor(configuredYAML.get(name).yaml); return; }
  setEditor("# Reading the configuration of " + name + "…");
  let text;
  try {
    const url = new URL("debug/collector", document.baseURI);
    url.searchParams.set("name", name);
    const response = await fetch(url, { cache: "no-store", credentials: "same-origin" });
    const answer = await response.json();
    if (answer.error) text = "# " + answer.error;
    else {
      configuredYAML.set(name, { yaml: answer.yaml, copy: answer.copy || answer.yaml, note: answer.copy_note || "" });
      text = answer.yaml;
      learnForm(name, answer);
    }
  } catch (error) {
    text = "# The configuration could not be read: " + error.message;
  }
  if ($("configured").value !== name) return;
  setEditor(text);
  $("edit-copy").disabled = !configuredYAML.has(name);
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
// metrics_path is written as the path reads, not as the page's URL escapes
// it: Prometheus escapes the path itself, so a proxy's prefix /my%20pfx
// would be scraped as /my%2520pfx. A path the browser left with an escape
// that decodes to no text is written as it stands.
const decodedPath = (path) => {
  try { return decodeURIComponent(path); } catch (error) { return path; }
};
// The Prometheus job is the multi-target pattern of docs/LOCALFILE.md: the
// target is the job's, moved into the target parameter and instance, and
// the exporter, as this page reached it, is what is scraped.
function scrapeConfigOf(collector, target, given, url) {
  const lines = ["scrape_configs:", "  - job_name: " + yamlQuoted(collector)];
  if (url.protocol === "https:") lines.push("    scheme: https");
  lines.push("    metrics_path: " + yamlQuoted(decodedPath(url.pathname)), "    params:");
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
  if (through === "") return { blocked: true, note: dangerous ? "/probe runs only the configured collectors, so the playground's own request is run on this page alone; choose a configured collector, or Edit a copy of one, to copy a probe." : "Choose a configured collector, or Edit a copy of one, to copy a probe." };
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
  // An edit moves the lines an error named: their marks go.
  markLines([]);
  drawGutter();
  if ($("collector").readOnly) return;
  written = $("collector").value;
  if (typeFrame) return;
  typeFrame = requestAnimationFrame(() => {
    typeFrame = 0;
    const now = requestTypeOf(written);
    if (now !== writtenType) { writtenType = now; showForm(); }
  });
});
// The copy leaves out what the playground would refuse, the lines that name
// the server's files and a login, each replaced by a comment, and the note
// above the editor says what it left out (debugcopy.go).
$("edit-copy").addEventListener("click", () => {
  const shown = configuredYAML.get($("configured").value);
  if (!shown) return;
  written = shown.copy || shown.yaml;
  copyNote = shown.note;
  writtenType = requestTypeOf(written);
  copiedFrom = $("configured").value;
  $("configured").value = "";
  showCollector();
  // The copy is read from its first line.
  $("collector").setSelectionRange(0, 0);
  $("collector").focus();
  $("collector").scrollTop = 0;
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
drawGutter();
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
      const through = throughForm();
      // An empty request_from, with --web.debug.dangerous, is the
      // playground's own request.
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
    show(answer, response, Math.round(performance.now() - started), request.collector);
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
// The OTLP tab is there with otlp.enabled alone.
try {
  const kept = store && store.getItem("debug.view");
  if (kept === "text" || (kept === "otlp" && $("view-otlp"))) view = kept;
} catch (error) { /* the table, then */ }
function showView() {
  const shown = refused ? "text" : view;
  $("views").hidden = refused;
  for (const tab of document.querySelectorAll("#views [data-view]")) tab.setAttribute("aria-selected", String(tab.dataset.view === shown));
  $("series-table-part").hidden = shown !== "table";
  $("series-text-part").hidden = shown !== "text";
  if ($("series-otlp-part")) $("series-otlp-part").hidden = shown !== "otlp";
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
// jsonView shows text, which is JSON, in box as a tree: each object and
// array a node that folds behind a button in the gutter, which says whether
// it is open (aria-expanded), and which shows, folded, how many keys or
// items it has and the value of its "name" or "key", so a folded OTLP
// attribute still says which it is. Keys, strings, numbers, booleans, null
// and punctuation each have a class of their own, coloured by the theme.
// Every value is put in with textContent alone: they are the target's data,
// label values and log lines among them. A node's children are made when it
// is first opened, so a body of thousands of points costs what is shown:
// at first the nodes down to jsonOpenDepth below the top. A number is the
// text the body has of it, where the browser hands the reviver its source,
// so a large integer is not rounded to a double's. Several documents, one
// blank line between them, as the log export's requests are, are a tree
// each; text that is not JSON is shown as it is, and the box says so. Copy
// copies the text the box was given (jsonTexts), not what the tree shows.
const jsonOpenDepth = 4;
const jsonTexts = new WeakMap(), jsonNodes = new WeakMap();
class JSONNumber { constructor(text) { this.text = text; } }
const parseJSON = (text) => JSON.parse(text, (key, value, context) =>
  typeof value === "number" ? new JSONNumber(context && typeof context.source === "string" ? context.source : String(value)) : value);
function jsonDocuments(text) {
  try {
    return [parseJSON(text)];
  } catch (error) {
    const parts = text.split(/\n\n(?=[[{])/);
    if (parts.length < 2) throw error;
    return parts.map(parseJSON);
  }
}
function jsonSpan(className, text) {
  const span = document.createElement("span");
  span.className = className;
  span.textContent = text;
  return span;
}
function jsonLeaf(value) {
  if (value instanceof JSONNumber) return jsonSpan("jv-number", value.text);
  if (typeof value === "string") return jsonSpan("jv-string", JSON.stringify(value));
  if (typeof value === "boolean") return jsonSpan("jv-boolean", String(value));
  return jsonSpan("jv-null", "null");
}
function jsonNode(key, value, depth, last) {
  const head = document.createElement("div");
  head.className = "jv-line";
  if (key !== null) head.append(jsonSpan("jv-key", JSON.stringify(key)), jsonSpan("jv-punct", ": "));
  const comma = () => last ? [] : [jsonSpan("jv-punct", ",")];
  if (value === null || typeof value !== "object" || value instanceof JSONNumber) {
    head.append(jsonLeaf(value), ...comma());
    return head;
  }
  const array = Array.isArray(value);
  const entries = array ? value.map((item) => [null, item]) : Object.entries(value);
  const [open, close] = array ? ["[", "]"] : ["{", "}"];
  if (entries.length === 0) {
    head.append(jsonSpan("jv-punct", open + close), ...comma());
    return head;
  }
  const node = document.createElement("div");
  node.className = "jv-node";
  const toggle = document.createElement("button");
  toggle.type = "button";
  toggle.className = "jv-toggle";
  const named = array ? undefined : [value.name, value.key].find((v) => typeof v === "string");
  const what = (key !== null ? key : array ? "array" : "object") + (named !== undefined ? " " + named : "");
  toggle.setAttribute("aria-label", what);
  const fold = jsonSpan("jv-fold", "");
  fold.append(jsonSpan("jv-punct", "…" + close), ...comma(),
    jsonSpan("jv-summary", " " + entries.length + (array ? (entries.length === 1 ? " item" : " items") : (entries.length === 1 ? " key" : " keys")) + (named !== undefined ? " · " : "")));
  if (named !== undefined) fold.append(jsonSpan("jv-hint", named));
  head.prepend(toggle);
  head.append(jsonSpan("jv-punct", open), fold);
  const kids = document.createElement("div");
  kids.className = "jv-kids";
  const tail = document.createElement("div");
  tail.className = "jv-line jv-tail";
  tail.append(jsonSpan("jv-punct", close), ...comma());
  node.append(head, kids, tail);
  jsonNodes.set(node, { entries, depth, kids, toggle, built: false });
  jsonSet(node, depth <= jsonOpenDepth);
  return node;
}
function jsonSet(node, open) {
  const state = jsonNodes.get(node);
  if (open && !state.built) {
    state.built = true;
    state.kids.append(...state.entries.map(([key, value], i) => jsonNode(key, value, state.depth + 1, i === state.entries.length - 1)));
  }
  node.classList.toggle("jv-closed", !open);
  state.toggle.textContent = open ? "▾" : "▸";
  state.toggle.setAttribute("aria-expanded", String(open));
}
// jsonAll opens every node of box, making each, or folds every node but the
// top ones, which keep their keys in sight. Opening goes down a level at a
// time, the top first, in slices of 30 ms, one a frame, between which the
// page answers clicks and draws, as a body of thousands of points has tens
// of thousands of nodes; a later jsonAll, a click on a node, or a new body
// stops it (jsonRuns).
const jsonRuns = new WeakMap();
function jsonAll(box, open) {
  const pass = {};
  jsonRuns.set(box, pass);
  if (!open) {
    for (const node of box.querySelectorAll(".jv-node")) jsonSet(node, jsonNodes.get(node).depth === 0);
    return;
  }
  const todo = [...box.querySelectorAll(".jv-doc > .jv-node")];
  let next = 0;
  const slice = () => {
    if (jsonRuns.get(box) !== pass) return;
    const until = performance.now() + 30;
    while (next < todo.length && performance.now() < until) {
      const node = todo[next++];
      jsonSet(node, true);
      for (const child of jsonNodes.get(node).kids.children) if (child.classList.contains("jv-node")) todo.push(child);
    }
    if (next < todo.length) requestAnimationFrame(slice);
  };
  slice();
}
function jsonView(box, text) {
  if (!jsonTexts.has(box)) {
    box.addEventListener("click", (event) => {
      const at = event.target.closest(".jv-toggle, .jv-fold");
      if (!at) return;
      const node = at.closest(".jv-node");
      jsonRuns.delete(box);
      jsonSet(node, node.classList.contains("jv-closed"));
    });
  }
  jsonTexts.set(box, text);
  jsonRuns.delete(box);
  const buttons = document.querySelectorAll('[data-json-all][aria-controls="' + box.id + '"]');
  let documents;
  try {
    documents = jsonDocuments(text);
  } catch (error) {
    const note = document.createElement("p");
    note.className = "note";
    note.textContent = "This is not JSON (" + error.message + "), so it is shown as text.";
    const pre = document.createElement("pre");
    pre.className = "jv-plain";
    pre.textContent = text;
    box.replaceChildren(note, pre);
    for (const button of buttons) button.disabled = true;
    return;
  }
  for (const button of buttons) button.disabled = false;
  box.replaceChildren(...documents.map((value) => {
    const doc = document.createElement("div");
    doc.className = "jv-doc";
    doc.append(jsonNode(null, value, 0, true));
    return doc;
  }));
}
for (const button of document.querySelectorAll("[data-json-all]")) {
  button.addEventListener("click", () => jsonAll($(button.getAttribute("aria-controls")), button.dataset.jsonAll === "open"));
}
// The OTLP tab (debugotlp.go): what the next export would send of the
// run's series, as the server made it with the export's own code. Every
// value is put in with textContent alone. The table of data points sorts by
// a column's header, a value as a number where both are one, and keeps the
// rows whose cells hold the filter's text; a row with notes has a badge,
// which opens to them, as a lint finding's does.
const otlpColumns = ["prometheus", "name", "kind", "unit", "attributes", "value", "notes"];
let otlpPoints = [], otlpSort = null, otlpDown = false;
const attributeText = (attributes) => (attributes || []).map((a) => a.key + "=" + a.value).join(", ");
function otlpCell(point, column) {
  if (column === "attributes") return attributeText(point.attributes);
  if (column === "notes") return (point.notes || []).join("\n");
  return point[column] || "";
}
function otlpNotesOf(notes) {
  const details = document.createElement("details");
  const summary = document.createElement("summary");
  summary.textContent = "\u24d8 " + notes.length + (notes.length === 1 ? " note" : " notes");
  summary.title = notes.join("\n");
  details.append(summary);
  for (const note of notes) {
    const p = document.createElement("p");
    p.className = "lint-advice";
    p.textContent = note;
    details.append(p);
  }
  return details;
}
const listOf = (list, items) => {
  list.replaceChildren(...items.map((text) => { const li = document.createElement("li"); li.textContent = text; return li; }));
  list.hidden = items.length === 0;
};
function drawOTLPRows() {
  const needle = $("otlp-filter").value.trim().toLowerCase();
  let rows = otlpPoints.filter((point) => needle === "" || point.haystack.includes(needle));
  if (otlpSort !== null) {
    rows = rows.slice().sort((a, b) => {
      const x = otlpCell(a, otlpSort), y = otlpCell(b, otlpSort);
      const p = numberOf(x), q = numberOf(y);
      const order = x !== "" && y !== "" && !Number.isNaN(p) && !Number.isNaN(q) ? (p < q ? -1 : p > q ? 1 : 0) : (x < y ? -1 : x > y ? 1 : 0);
      return otlpDown ? -order : order;
    });
  }
  for (const th of $("otlp-columns").children) th.setAttribute("aria-sort", th.dataset.column === otlpSort ? (otlpDown ? "descending" : "ascending") : "none");
  $("otlp-rows").replaceChildren(...rows.slice(0, tableRows).map((point) => {
    const tr = document.createElement("tr");
    for (const column of otlpColumns) {
      const td = document.createElement("td");
      if (column === "value") td.className = "value";
      if (column === "kind") td.className = "type";
      if (column === "notes") {
        td.className = "otlp-notes";
        if ((point.notes || []).length > 0) td.append(otlpNotesOf(point.notes));
      } else {
        td.textContent = otlpCell(point, column);
      }
      tr.append(td);
    }
    return tr;
  }));
  $("otlp-box").hidden = otlpPoints.length === 0;
  $("otlp-count").textContent = otlpPoints.length === 0 ? "(no data points)" : rows.length + " of " + otlpPoints.length + " data points" + (rows.length > tableRows ? "; showing the first " + tableRows + ", filter to narrow" : "");
}
if ($("series-otlp-part")) {
  $("otlp-columns").replaceChildren(...otlpColumns.map((column) => {
    const th = document.createElement("th");
    th.scope = "col";
    th.dataset.column = column;
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = column === "prometheus" ? "Prometheus name" : column === "name" ? "OTLP name" : column;
    button.addEventListener("click", () => {
      otlpDown = otlpSort === column ? !otlpDown : false;
      otlpSort = column;
      drawOTLPRows();
    });
    th.append(button);
    return th;
  }));
  $("otlp-filter").addEventListener("input", drawOTLPRows);
  const copyButton = (button, source, done) => button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(jsonTexts.get($(source)) || "");
      $(done).textContent = "Copied.";
    } catch (error) {
      $(done).textContent = "The browser did not let the page copy it; select the text below.";
    }
  });
  copyButton($("otlp-body-copy"), "otlp-body", "otlp-body-copied");
  if ($("otlp-logs-copy")) copyButton($("otlp-logs-copy"), "otlp-logs-body", "otlp-logs-copied");
}
function showOTLP(otlp, ran) {
  if (!$("series-otlp-part")) return;
  $("otlp-shown").hidden = !otlp;
  $("otlp-none").hidden = !!otlp;
  if (!otlp) {
    $("otlp-none").textContent = ran ? "The OTLP export is off (otlp.enabled), so a run shows nothing here." : "The run was refused: nothing would be exported.";
    otlpPoints = [];
    return;
  }
  listOf($("otlp-notes"), otlp.notes || []);
  $("otlp-resource-rows").replaceChildren(...(otlp.resource || []).map((a) => {
    const tr = document.createElement("tr");
    const th = document.createElement("th");
    th.scope = "row";
    th.textContent = a.key;
    const td = document.createElement("td");
    td.textContent = a.value;
    tr.append(th, td);
    return tr;
  }));
  listOf($("otlp-resource-notes"), otlp.resource_notes || []);
  $("otlp-scope").textContent = "Scope: " + otlp.scope.name + (otlp.scope.version ? " " + otlp.scope.version : " (no version)") + ". Metric names: " + otlp.metric_names + ".";
  otlpPoints = (otlp.points || []).map((point) => ({ ...point, haystack: otlpColumns.map((column) => otlpCell(point, column)).join("\n").toLowerCase() }));
  drawOTLPRows();
  const left = otlp.left_out || [], clashes = otlp.clashes || [];
  $("otlp-left-part").hidden = left.length === 0 && clashes.length === 0;
  listOf($("otlp-left"), left.map((p) => "\u26a0 " + p.prometheus + " as " + p.name + " (" + p.kind + ")" + (p.attributes ? " {" + attributeText(p.attributes) + "}" : "") + ": " + p.reason));
  $("otlp-clashes").hidden = clashes.length === 0;
  $("otlp-clashes").textContent = clashes.join("\n");
  jsonView($("otlp-body"), otlp.body);
  if ($("otlp-logs-part")) {
    const logs = otlp.logs;
    $("otlp-logs-part").hidden = !logs;
    if (logs) {
      listOf($("otlp-logs-notes"), logs.notes || []);
      const records = logs.records || [];
      const exported = records.filter((r) => r.exported).length;
      $("otlp-logs-count").textContent = records.length === 0 ? "The run logged nothing." : records.length + (records.length === 1 ? " line, " : " lines, ") + exported + " exported at otlp.logs.level " + logs.level;
      $("otlp-logs-box").hidden = records.length === 0;
      $("otlp-logs-rows").replaceChildren(...records.map((r) => {
        const tr = document.createElement("tr");
        for (const text of [r.time || "", r.severity_text + " (" + r.severity_number + ")", r.body, attributeText(r.attributes), r.exported ? "yes" : "no: " + (r.note || "")]) {
          const td = document.createElement("td");
          td.textContent = text;
          tr.append(td);
        }
        return tr;
      }));
      jsonView($("otlp-logs-body"), logs.body);
    }
  }
}
// The endpoint test: one empty export to the endpoint otlp configures, at
// --web.debug=full. One test at a time: the buttons are disabled while one
// is in progress, and the server refuses a second.
for (const button of document.querySelectorAll(".otlp-test button")) {
  button.addEventListener("click", async () => {
    const buttons = [...document.querySelectorAll(".otlp-test button")];
    const result = $("otlp-test-result");
    for (const b of buttons) b.disabled = true;
    result.hidden = false;
    result.className = "";
    result.textContent = "Testing the " + button.dataset.signal + " endpoint…";
    try {
      const response = await fetch(new URL("debug/otlp-test", document.baseURI), {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Debug-Run": "otlp-test" },
        body: JSON.stringify({ signal: button.dataset.signal }),
        cache: "no-store", credentials: "same-origin",
      });
      const t = await response.json();
      const lines = [];
      if (t.error) {
        lines.push(response.status + ": " + t.error);
      } else {
        lines.push((t.ok ? "OK" : "Failed") + ": " + t.signal + " endpoint " + t.endpoint + (t.took ? " in " + t.took : ""));
        if (t.status) lines.push("Status: " + t.status);
        if (t.failure) lines.push("Error: " + t.failure);
        if (t.outcome) lines.push("An export: " + t.outcome);
        if (t.partial_success) lines.push("partialSuccess: " + t.partial_success.rejected + " rejected" + (t.partial_success.message ? ": " + t.partial_success.message : ""));
        lines.push("", "Sent: " + t.body, ...(t.sent || []).map((h) => "  " + h));
        if (t.headers) lines.push("", "Answered:", ...t.headers.map((h) => "  " + h));
        if (t.response) lines.push("", t.response);
        for (const note of t.notes || []) lines.push("", note);
      }
      result.className = t.ok ? "" : "error";
      result.textContent = lines.join("\n");
    } catch (error) {
      result.className = "error";
      result.textContent = "The test could not be sent: " + error.message;
    } finally {
      for (const b of buttons) b.disabled = false;
    }
  });
}
// sent is the playground's YAML the run sent, undefined for a configured
// collector's run: the lines a refusal of it names are marked in the editor,
// and link to it, while the editor still holds what was sent.
function show(answer, response, took, sent) {
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
    const ours = sent !== undefined && sent === editorArea.value && $("configured").value === "";
    showError(series, answer.error, ours);
    markLines(ours ? linesNamed(answer.error) : []);
    fillTable({});
    showOTLP(null, false);
    for (const part of ["lint-part", "notes-part", "requests-part", "response-part"]) $(part).hidden = true;
    $("report").textContent = "";
    return;
  }
  markLines([]);
  statusLine.className = "status " + (answer.ok ? "ok" : "failed");
  statusLine.textContent = "Ran in " + took + " ms: " + answer.verdict;
  series.className = "";
  series.textContent = answer.series === "" ? "(no series)" : answer.series;
  fillTable(answer);
  showOTLP(answer.otlp, true);
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
// Ctrl+Enter, or Cmd+Enter, runs as Run does, from anywhere on the page, the
// editors and the fields among it.
document.addEventListener("keydown", (event) => {
  if (event.key !== "Enter" || !(event.ctrlKey || event.metaKey) || event.altKey || event.shiftKey || event.isComposing) return;
  event.preventDefault();
  run();
});
// Nothing runs until the visitor presses Run: the page takes its collector
// and target from its URL, which a link on any page can set, and running on
// opening would make that link a run the visitor never asked for.
</script>
</body>
</html>
`)
