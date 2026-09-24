package exporter

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The collectors page at /collectors lists each collector with a form that
// probes a target through it, taking what the collector takes from a probe:
// the target, its request parameters, the headers it forwards and, when it
// forwards the probe's Authorization, a credential for the target.
//
// The form is sent by the page's script, which calls /probe and shows the
// answer in place. That is how a credential reaches the target without being
// written into a URL: it travels as the probe's Authorization header, as
// Prometheus sends a monitor's. Its fields have no name, so without the script
// the form still probes, as a plain GET of /probe, but leaves the credential
// out rather than put it in the address bar and the history. Forwarded headers
// are probe parameters, header_<name>, which is how Prometheus sends them too,
// and so are not for secrets. A credential the collector's own configuration
// holds is used by the exporter itself: the page says it is there, never
// what it is.

type collectorEntry struct {
	Name, RequestType, Transform string
	// TargetRequired is whether a probe must name a target; a collector whose
	// request type reads a fixed source may leave it out.
	TargetRequired bool
	TargetHint     string
	Params         []fetch.RequestParam
	// Credentials names the credentials the collector's configuration sends
	// the target, without their values.
	Credentials          []string
	ForwardAuthorization bool
	ForwardHeaders       []string
}

type collectorsPage struct {
	Collectors []collectorEntry
	Docs       string
}

// configuredCredentials names the credentials c's configuration sends its
// target.
func configuredCredentials(c *model.Collector) []string {
	var out []string
	r := c.Request
	if r.BasicAuth != nil {
		out = append(out, "basic auth from the configuration")
	}
	if r.BasicAuthFile != nil {
		out = append(out, "basic auth read from files")
	}
	if r.BearerToken != "" {
		out = append(out, "a bearer token from the configuration")
	}
	if r.BearerTokenFile != "" {
		out = append(out, "a bearer token read from a file")
	}
	if r.TLS.CertFile != "" {
		out = append(out, "a TLS client certificate")
	}
	return out
}

func targetHint(c *model.Collector, required bool) string {
	hint := "target"
	switch c.Request.Type {
	case fetch.RequestTypeHTTP:
		hint = "http://host:port"
	case "localfile":
		hint = "a file under its root"
	}
	if !required {
		hint += " (optional)"
	}
	return hint
}

func (s *Server) collectorsHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := s.manager.Get()
	page := collectorsPage{Docs: landingDocs}
	for i := range cfg.Collectors {
		c := &cfg.Collectors[i]
		required := errors.Is(fetch.CheckTarget(c, "", false), fetch.ErrMissingTarget)
		page.Collectors = append(page.Collectors, collectorEntry{
			Name: c.Name, RequestType: c.Request.Type, Transform: c.Transform.Type,
			TargetRequired:       required,
			TargetHint:           targetHint(c, required),
			Params:               fetch.RequestParams(c),
			Credentials:          configuredCredentials(c),
			ForwardAuthorization: c.Request.ForwardAuthorization,
			ForwardHeaders:       forwardableHeaders(c.Request),
		})
	}
	s.renderPage(w, collectorsTemplate, page)
}

var collectorsTemplate = template.Must(template.New("collectors").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Collectors · Prometheus Universal Exporter</title>
<style>` + pageStyle + `
nav { margin-bottom: 16px; font-size: 14px; }
section.collector { border: 1px solid var(--line); border-radius: 10px; padding: 16px; margin: 16px 0; background: var(--panel); }
section.collector h2 { margin: 0; font-size: 17px; }
section.collector h2 code { font-size: 15px; }
.kind { color: var(--muted); font-size: 13px; margin: 2px 0 12px; }
.note { color: var(--muted); font-size: 13px; margin: 4px 0 10px; }
fieldset { border: 0; padding: 0; margin: 14px 0 0; min-width: 0; }
legend { font-size: 13px; font-weight: 600; color: var(--muted); padding: 0; margin-bottom: 4px; }
label { display: block; font-size: 13px; margin: 6px 0 2px; }
input, select { width: 100%; padding: 6px 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--fg); font: inherit; font-size: 14px; }
.row { display: flex; gap: 8px; flex-wrap: wrap; }
.row > div { flex: 1 1 200px; min-width: 0; }
button { margin-top: 14px; padding: 7px 16px; border: 1px solid var(--accent); border-radius: 6px; background: var(--accent); color: var(--on-accent); font: inherit; font-size: 14px; cursor: pointer; }
button:disabled { opacity: .6; cursor: wait; }
[hidden] { display: none !important; }
.result { margin-top: 12px; }
.status { font-size: 13px; font-weight: 600; }
.status.ok { color: var(--good); }
.status.failed { color: var(--bad); }
.sent { font-size: 12px; color: var(--muted); margin: 2px 0 6px; }
pre { margin: 0; padding: 10px; max-height: 360px; overflow: auto; background: var(--bg); border: 1px solid var(--line); border-radius: 6px; font-size: 12px; white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
</head>
<body>
<main>
<nav><a href="/">← Prometheus Universal Exporter</a></nav>
<h1>Collectors</h1>
<p class="meta">Probe a target through a collector, as Prometheus would. The answer is what Prometheus would scrape.</p>
<noscript><p class="note">Without JavaScript the forms open <code>/probe</code> directly, and a target credential entered here is not sent.</p></noscript>
{{- range .Collectors}}
<section class="collector" id="collector-{{.Name}}">
<h2><code>{{.Name}}</code></h2>
<p class="kind">{{.RequestType}} · {{.Transform}}</p>
<form class="probe" action="/probe" method="get" autocomplete="off">
<input type="hidden" name="collector" value="{{.Name}}">
<label for="{{.Name}}-target">Target</label>
<input id="{{.Name}}-target" type="text" name="target" placeholder="{{.TargetHint}}"{{if .TargetRequired}} required{{end}}>
{{- if .Params}}
<fieldset>
<legend>Request parameters</legend>
<div class="row">
{{- $name := .Name}}
{{- range .Params}}
<div><label for="{{$name}}-{{.Name}}"><code>{{.Name}}</code>{{if .Required}} (required){{end}}</label>
<input id="{{$name}}-{{.Name}}" type="text" name="{{.Name}}"{{if .Required}} required{{else}} placeholder="default: {{.Default}}"{{end}}></div>
{{- end}}
</div>
</fieldset>
{{- end}}
{{- if .ForwardHeaders}}
<fieldset>
<legend>Forwarded headers</legend>
<p class="note">Sent as probe parameters, as Prometheus sends a monitor's headers, so they appear in the URL: not for secrets.</p>
<div class="row">
{{- $name := .Name}}
{{- range .ForwardHeaders}}
<div><label for="{{$name}}-header-{{.}}"><code>{{.}}</code></label>
<input id="{{$name}}-header-{{.}}" type="text" name="header_{{.}}"></div>
{{- end}}
</div>
</fieldset>
{{- end}}
{{- if .ForwardAuthorization}}
<fieldset>
<legend>Target credential</legend>
<p class="note">Forwarded to the target as the probe's <code>Authorization</code> header, as Prometheus forwards a monitor's{{if .Credentials}}, in place of the credential the configuration holds{{end}}. It is never put in the URL.</p>
<label for="{{.Name}}-scheme">Scheme</label>
<select id="{{.Name}}-scheme" data-auth="scheme">
<option value="">None</option>
<option value="bearer">Bearer token</option>
<option value="basic">Basic auth</option>
</select>
<div data-auth-for="bearer" hidden><label for="{{.Name}}-token">Token</label>
<input id="{{.Name}}-token" type="password" data-auth="token" autocomplete="off"></div>
<div class="row" data-auth-for="basic" hidden>
<div><label for="{{.Name}}-username">Username</label><input id="{{.Name}}-username" type="text" data-auth="username" autocomplete="off"></div>
<div><label for="{{.Name}}-password">Password</label><input id="{{.Name}}-password" type="password" data-auth="password" autocomplete="off"></div>
</div>
</fieldset>
{{- end}}
{{- if .Credentials}}
<p class="note">The exporter sends the target {{range $i, $c := .Credentials}}{{if $i}}, {{end}}{{$c}}{{end}}{{if not .ForwardAuthorization}}; nothing to enter here{{end}}.</p>
{{- end}}
<button type="submit">Probe</button>
</form>
<div class="result" hidden aria-live="polite">
<div class="status"></div>
<div class="sent"></div>
<pre></pre>
</div>
</section>
{{- end}}
<p class="meta"><a href="{{.Docs}}">Documentation</a></p>
</main>
<script>
"use strict";
// Each form calls /probe itself, so a target credential travels as the
// Authorization header rather than in the URL, and shows the answer in place.
// The exporter's own Basic Auth, when on, is the browser's to send.
for (const form of document.querySelectorAll("form.probe")) {
  const scheme = form.querySelector('[data-auth="scheme"]');
  const field = (name) => form.querySelector('[data-auth="' + name + '"]');
  if (scheme) {
    const show = () => {
      for (const part of form.querySelectorAll("[data-auth-for]")) {
        part.hidden = part.dataset.authFor !== scheme.value;
      }
    };
    scheme.addEventListener("change", show);
    show();
  }
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const params = new URLSearchParams();
    for (const [key, value] of new FormData(form)) {
      if (value !== "") params.append(key, value);
    }
    const headers = {};
    if (scheme && scheme.value === "bearer" && field("token").value !== "") {
      headers.Authorization = "Bearer " + field("token").value;
    } else if (scheme && scheme.value === "basic") {
      const pair = new TextEncoder().encode(field("username").value + ":" + field("password").value);
      headers.Authorization = "Basic " + btoa(String.fromCharCode(...pair));
    }
    const result = form.nextElementSibling;
    const status = result.querySelector(".status");
    const sent = result.querySelector(".sent");
    const body = result.querySelector("pre");
    const button = form.querySelector("button");
    const url = "/probe?" + params.toString();
    button.disabled = true;
    result.hidden = false;
    status.className = "status";
    status.textContent = "Probing…";
    sent.textContent = "GET " + url + (headers.Authorization ? ", with an Authorization header" : "");
    body.textContent = "";
    const started = performance.now();
    try {
      const response = await fetch(url, { headers, cache: "no-store", credentials: "same-origin" });
      const text = await response.text();
      const took = Math.round(performance.now() - started);
      status.className = "status " + (response.ok ? "ok" : "failed");
      status.textContent = response.status + " " + response.statusText + " in " + took + " ms";
      body.textContent = text === "" ? "(an empty answer: nothing was extracted)" : text;
    } catch (error) {
      status.className = "status failed";
      status.textContent = "The probe could not be sent: " + error.message;
    } finally {
      button.disabled = false;
    }
  });
}
</script>
</body>
</html>
`))
