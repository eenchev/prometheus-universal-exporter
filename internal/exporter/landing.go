package exporter

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// The landing page at /, as Prometheus exporters usually have one: what is
// running, the endpoints it serves, and the collectors loaded, each with a form
// that probes a target through it. It reads the configuration in force, so a
// reload shows on the next visit. It is protected like /probe when the
// exporter's Basic Auth is on, since it lists the collectors.

// landingDocs is where the page points for documentation.
const landingDocs = "https://github.com/eenchev/prometheus-universal-exporter"

type landingCollector struct {
	Name, RequestType, Transform string
	// TargetRequired is whether a probe must name a target; a collector whose
	// request type reads a fixed source may leave it out.
	TargetRequired bool
}

type landingPage struct {
	Version, Revision, GoVersion string
	SelfMetricsPath              string
	Lifecycle                    bool
	Collectors                   []landingCollector
	ScheduledTargets             int
	OTLPEnabled                  bool
	Docs                         string
}

var landingTemplate = template.Must(template.New("landing").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Prometheus Universal Exporter</title>
<style>
:root { color-scheme: light dark; --fg: #1d1d1f; --muted: #5f6368; --bg: #ffffff; --line: #d9dce1; --accent: #c2410c; --on-accent: #ffffff; --code: #f3f4f6; }
@media (prefers-color-scheme: dark) { :root { --fg: #e8eaed; --muted: #9aa0a6; --bg: #16181b; --line: #34373c; --accent: #ff8a5c; --on-accent: #16181b; --code: #23262b; } }
* { box-sizing: border-box; }
body { margin: 0; padding: 32px 16px; background: var(--bg); color: var(--fg); font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; overflow-wrap: anywhere; }
main { max-width: 860px; margin: 0 auto; }
h1 { font-size: 26px; margin: 0 0 4px; }
h2 { font-size: 17px; margin: 32px 0 8px; }
p.meta { color: var(--muted); margin: 0; }
a { color: var(--accent); }
code { background: var(--code); padding: 1px 5px; border-radius: 4px; font-size: 13px; }
ul.links { padding-left: 20px; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 8px 6px; border-bottom: 1px solid var(--line); vertical-align: middle; }
th { font-size: 13px; color: var(--muted); font-weight: 600; }
form { display: flex; gap: 6px; flex-wrap: wrap; margin: 0; }
input[type=text] { flex: 1 1 220px; min-width: 0; padding: 5px 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--fg); font: inherit; font-size: 14px; }
button { padding: 5px 12px; border: 1px solid var(--accent); border-radius: 6px; background: var(--accent); color: var(--on-accent); font: inherit; font-size: 14px; cursor: pointer; }
@media (max-width: 600px) {
  thead { display: none; }
  table, tbody, tr, td { display: block; }
  tr { padding: 10px 0; border-bottom: 1px solid var(--line); }
  td { border: 0; padding: 3px 0; }
  td:nth-child(2), td:nth-child(3) { display: inline; color: var(--muted); font-size: 13px; }
  td:nth-child(2)::after { content: " · "; }
}
</style>
</head>
<body>
<main>
<h1>Prometheus Universal Exporter</h1>
<p class="meta">Version {{.Version}}, revision {{.Revision}}, {{.GoVersion}}</p>

<h2>Endpoints</h2>
<ul class="links">
<li><a href="/metrics">/metrics</a> — the exporter's own metrics</li>
{{- if ne .SelfMetricsPath "/metrics"}}
<li><a href="{{.SelfMetricsPath}}">{{.SelfMetricsPath}}</a> — the same self-metrics on their dedicated path</li>
{{- end}}
<li><code>/probe?collector=&lt;name&gt;&amp;target=&lt;target&gt;</code> — scrape a target through a collector</li>
<li><a href="/health">/health</a> and <a href="/ready">/ready</a> — liveness and readiness</li>
{{- if .Lifecycle}}
<li><code>POST /-/reload</code> — reload the configuration</li>
{{- end}}
</ul>

<h2>Collectors</h2>
<table>
<thead><tr><th>Name</th><th>Request</th><th>Transform</th><th>Probe</th></tr></thead>
<tbody>
{{- range .Collectors}}
<tr>
<td><code>{{.Name}}</code></td>
<td>{{.RequestType}}</td>
<td>{{.Transform}}</td>
<td><form action="/probe" method="get"><input type="hidden" name="collector" value="{{.Name}}"><input type="text" name="target" aria-label="Target for {{.Name}}" placeholder="{{if .TargetRequired}}target{{else}}target (optional){{end}}"{{if .TargetRequired}} required{{end}}><button type="submit">Probe</button></form></td>
</tr>
{{- end}}
</tbody>
</table>
{{- if .OTLPEnabled}}
<p class="meta">{{.ScheduledTargets}} scheduled target{{if ne .ScheduledTargets 1}}s{{end}} exported over OTLP.</p>
{{- end}}

<h2>Documentation</h2>
<p><a href="{{.Docs}}">{{.Docs}}</a></p>
</main>
</body>
</html>
`))

func (s *Server) landingHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := s.manager.Get()
	build := BuildVersion()
	page := landingPage{
		Version: build.Version, Revision: build.Revision, GoVersion: build.GoVersion,
		SelfMetricsPath:  s.selfMetricsPath,
		Lifecycle:        s.lifecycle,
		ScheduledTargets: len(s.manager.Targets()),
		OTLPEnabled:      cfg.OTLP.Enabled,
		Docs:             landingDocs,
	}
	if page.SelfMetricsPath == "" {
		page.SelfMetricsPath = "/self-metrics"
	}
	for i := range cfg.Collectors {
		c := &cfg.Collectors[i]
		page.Collectors = append(page.Collectors, landingCollector{
			Name: c.Name, RequestType: c.Request.Type, Transform: c.Transform.Type,
			TargetRequired: errors.Is(fetch.CheckTarget(c, "", false), fetch.ErrMissingTarget),
		})
	}
	// Rendered whole before anything is written, so a failure is a clean 500
	// rather than half a page.
	var body bytes.Buffer
	if err := landingTemplate.Execute(&body, page); err != nil {
		s.logger.Error("rendering the landing page failed", "error", err)
		http.Error(w, "the landing page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}
