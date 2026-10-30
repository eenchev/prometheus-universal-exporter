package exporter

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// The exporter's HTML pages: the landing page at /, as Prometheus exporters
// usually have one, saying what is running and linking the endpoints, and the
// collectors page (collectorspage.go), where a target can be probed through
// each collector by hand. Both read the configuration in force, so a reload
// shows on the next visit, and both are protected like /probe when the
// exporter's Basic Auth is on, since they list the collectors.

// landingDocs is where the pages point for documentation.
const landingDocs = "https://github.com/eenchev/prometheus-universal-exporter"

// pageStyle is the pages' shared stylesheet. The colours follow the browser's
// light or dark preference, and the layout works at phone width.
const pageStyle = `
:root { color-scheme: light dark; --fg: #1d1d1f; --muted: #5f6368; --bg: #ffffff; --panel: #f7f7f8; --line: #d9dce1; --accent: #c2410c; --on-accent: #ffffff; --code: #f3f4f6; --bad: #b91c1c; --good: #15803d; }
@media (prefers-color-scheme: dark) { :root { --fg: #e8eaed; --muted: #9aa0a6; --bg: #16181b; --panel: #1d2024; --line: #34373c; --accent: #ff8a5c; --on-accent: #16181b; --code: #23262b; --bad: #f87171; --good: #4ade80; } }
* { box-sizing: border-box; }
body { margin: 0; padding: 32px 16px; background: var(--bg); color: var(--fg); font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; overflow-wrap: anywhere; }
main { max-width: 860px; margin: 0 auto; }
h1 { font-size: 26px; margin: 0 0 4px; }
h2 { font-size: 17px; margin: 32px 0 8px; }
p.meta { color: var(--muted); margin: 0; }
a { color: var(--accent); }
code { background: var(--code); padding: 1px 5px; border-radius: 4px; font-size: 13px; }
ul.links { padding-left: 20px; }
` + chromeStyle + longStyle

// pagePolicy is the pages' Content-Security-Policy. Its img-src is for the
// icon (pageIcon), which is a data: URL.
const pagePolicy = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// renderPage executes t with data, a page with its bar (pagechrome.go), and
// writes it as HTML. The page is rendered whole before anything is written,
// so a failure is a clean 500 rather than half a page.
func (s *Server) renderPage(w http.ResponseWriter, t *template.Template, data framedPage) {
	s.renderHTML(w, t, data)
}

// renderFragment is renderPage for a part of a page, which a page's script
// swaps in, and which has no bar of its own.
func (s *Server) renderFragment(w http.ResponseWriter, t *template.Template, data any) {
	s.renderHTML(w, t, data)
}

// renderHTML is renderPage and renderFragment.
func (s *Server) renderHTML(w http.ResponseWriter, t *template.Template, data any) {
	var body bytes.Buffer
	if err := t.Execute(&body, data); err != nil {
		s.logger.Error("rendering a page failed", "page", t.Name(), "error", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The pages list the configuration in force; a cached copy would not.
	w.Header().Set("Cache-Control", "no-store")
	// The collectors page takes target credentials, so no other site may
	// frame the pages and steer what is typed into them. The policy also
	// keeps the pages to what they are: their own inline style and script,
	// requests to the exporter itself, and forms sent nowhere else.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", pagePolicy)
	_, _ = w.Write(body.Bytes())
}

// The pages link to each other and to the endpoints relatively — collectors,
// probe, not /collectors, /probe — so they keep working behind a reverse proxy
// that serves the exporter under a path of its own, such as /exporter/. The
// paths shown as text stay absolute: they are what the exporter answers.

type landingPage struct {
	// Chrome is the bar at the top of the page (pagechrome.go).
	Chrome                       pageChrome
	Version, Revision, GoVersion string
	SelfMetricsPath              string
	// SelfMetricsHref and StaticTargetsHref are the paths as relative links.
	SelfMetricsHref, StaticTargetsHref string
	Lifecycle                          bool
	// Debug links /debug, --web.debug at edit or full.
	Debug                bool
	Collectors           int
	StaticTargetsPath    string
	StaticTargets        int
	StaticTargetsViaOTLP int
	// StatusPath and StatusHref are the status page
	// (statictargetstatus.go), always linked: it has the recent probes.
	StatusPath, StatusHref string
	// Reloaded says a reload has put something in force since the exporter
	// started; ReloadedAt is when the last did, and ReloadChanges what it
	// changed (config.ReloadSummary).
	Reloaded                  bool
	ReloadedAt, ReloadChanges string
	Docs                      string
}

var landingTemplate = pageTemplate("landing", `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Prometheus Universal Exporter</title>
`+pageIcon+`
<style>`+pageStyle+`</style>
</head>
<body>
<main>
{{template "chrome" .Chrome}}
<h1>Prometheus Universal Exporter</h1>
<p class="meta">Version {{.Version}}, revision {{.Revision}}, {{.GoVersion}}</p>

<h2>Collectors</h2>
<p>{{.Collectors}} collector{{if ne .Collectors 1}}s{{end}} loaded. <a href="collectors">Probe a target through one</a>, with the parameters and credentials it takes.{{if .Debug}} Write or debug one on the <a href="debug">debug page</a>.{{end}} <a href="{{.StatusHref}}">See how the recent probes went</a>.</p>
{{- if .StaticTargets}}

<h2>Static targets</h2>
<p>{{.StaticTargets}} static target{{if ne .StaticTargets 1}}s{{end}}, scraped by the exporter on their own intervals and served at <a href="{{.StaticTargetsHref}}">{{.StaticTargetsPath}}</a>{{if .StaticTargetsViaOTLP}}; {{.StaticTargetsViaOTLP}} also exported over OTLP{{end}}. <a href="{{.StatusHref}}">See how each one's last scrape went</a>.</p>
{{- end}}

<h2>Configuration</h2>
{{- if .Reloaded}}
<p>Last reloaded at {{.ReloadedAt}}: {{.ReloadChanges}}.</p>
{{- else}}
<p>Not reloaded since the exporter started.</p>
{{- end}}

<h2>Endpoints</h2>
<ul class="links">
<li><a href="collectors">/collectors</a> — the collectors, and a form to probe through each</li>
{{- if .Debug}}
<li><a href="debug">/debug</a> — run a collector and see each stage of its trip and the series it gives</li>
{{- end}}
<li><code>/probe?collector=&lt;name&gt;&amp;target=&lt;target&gt;</code> — scrape a target through a collector</li>
<li><a href="{{.StaticTargetsHref}}">{{.StaticTargetsPath}}</a> — the static targets' latest results</li>
<li><a href="{{.StatusHref}}">{{.StatusPath}}</a> — the status page: each static target's last scrape, its result, its error and whether it is backing off, and the recent probes of each collector and target; <code>?format=json</code> as JSON</li>
<li><a href="{{.SelfMetricsHref}}">{{.SelfMetricsPath}}</a> — the exporter's own metrics</li>
<li><a href="health">/health</a> and <a href="ready">/ready</a> — liveness and readiness</li>
{{- if .Lifecycle}}
<li><code>POST /-/reload</code> or <code>PUT /-/reload</code> — reload the configuration</li>
{{- end}}
</ul>

<h2>Documentation</h2>
<p><a href="{{.Docs}}">{{.Docs}}</a></p>
</main>
</body>
</html>
`)

func (s *Server) landingHandler(w http.ResponseWriter, _ *http.Request) {
	cfg, file := s.manager.InForce()
	build := BuildVersion()
	page := landingPage{
		Chrome:  s.chromeFor(pageHome),
		Version: build.Version, Revision: build.Revision, GoVersion: build.GoVersion,
		SelfMetricsPath:   s.selfMetricsEndpoint(),
		SelfMetricsHref:   relativeLink(s.selfMetricsEndpoint()),
		StaticTargetsHref: relativeLink(s.staticTargetsEndpoint()),
		Lifecycle:         s.lifecycle,
		Debug:             s.debug != DebugOff,
		Collectors:        len(cfg.Collectors),
		StaticTargetsPath: s.staticTargetsEndpoint(),
		StatusPath:        s.staticTargetsStatusEndpoint(),
		StatusHref:        relativeLink(s.staticTargetsStatusEndpoint()),
		Docs:              landingDocs,
	}
	if last, ok := s.manager.LastReload(); ok {
		page.Reloaded = true
		page.ReloadedAt = last.At.UTC().Format(time.RFC3339)
		page.ReloadChanges = last.Changes
	}
	for _, target := range staticTargetsOf(file) {
		page.StaticTargets++
		if target.ExportViaOTLP {
			page.StaticTargetsViaOTLP++
		}
	}
	s.renderPage(w, landingTemplate, page)
}

// relativeLink is path, an endpoint's absolute path, as a link relative to the
// pages, which are served at the root.
func relativeLink(path string) string { return strings.TrimPrefix(path, "/") }
