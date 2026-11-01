package exporter

import (
	_ "embed"
	"encoding/base64"
	"html/template"
)

// Every HTML page of the exporter — the landing page, the collectors page,
// the debug page and the status page — is framed alike: the bar at the top
// of each (pageChrome), the stylesheet (pageStyle), in the theme chosen in
// the bar (themeHead), and the headers renderPage sets. The bar leads back to the
// landing page and to every other page from wherever a page is served, so no
// page is a dead end, and a page added later gets it by being rendered at
// all: renderPage takes only data that carries a bar (framedPage).

// pageChrome is the bar at the top of a page: Home, the landing page as a
// link relative to the page, empty on the landing page itself; Links, the
// other pages, the one shown marked; Docs, the documentation; and
// Dangerous, --web.debug.dangerous, which every page's bar warns of, as the
// debug page's playground then runs Python and reads the server's files.
type pageChrome struct {
	Home      string
	Links     []chromeLink
	Docs      string
	Dangerous bool
}

// chromeLink is a page the bar links, Current on the page itself.
type chromeLink struct {
	Label, Href string
	Current     bool
}

// framedPage is what renderPage renders: a page's data, which carries its
// bar as its Chrome field, where the template places it.
type framedPage interface{ frame() pageChrome }

func (p landingPage) frame() pageChrome      { return p.Chrome }
func (p collectorsPage) frame() pageChrome   { return p.Chrome }
func (p debugPage) frame() pageChrome        { return p.Chrome }
func (p staticStatusPage) frame() pageChrome { return p.Chrome }

// The pages a bar names, by the path each is served at.
const (
	pageHome       = "/"
	pageCollectors = "/collectors"
)

// chromeFor is the bar of the page served at path. Its links are relative to
// that path, as every link of the pages is, so they hold behind a reverse
// proxy that serves the exporter under a path of its own; the status page is
// under the static targets path, one or more segments down.
func (s *Server) chromeFor(path string) pageChrome {
	up := rootLink(path)
	prefix := up
	if prefix == "./" {
		prefix = ""
	}
	link := func(label, endpoint string) chromeLink {
		return chromeLink{Label: label, Href: prefix + relativeLink(endpoint), Current: endpoint == path}
	}
	chrome := pageChrome{Docs: landingDocs, Dangerous: s.playgroundUnbound()}
	if path != pageHome {
		chrome.Home = up
	}
	chrome.Links = append(chrome.Links, link("Collectors", pageCollectors))
	if s.debug != DebugOff {
		chrome.Links = append(chrome.Links, link("Debug", debugPagePath))
	}
	chrome.Links = append(chrome.Links, link("Status", s.staticTargetsStatusEndpoint()))
	return chrome
}

// chromeTemplate is the bar, which each page's template places first in its
// main element as {{template "chrome" .Chrome}}.
const chromeTemplate = `{{define "chrome"}}<header class="site"><nav aria-label="The exporter's pages">
{{- if .Home}}<a class="home" href="{{.Home}}">← Prometheus Universal Exporter</a>{{else}}<span class="home" aria-current="page">Prometheus Universal Exporter</span>{{end}}
<span class="pages">
{{- if .Dangerous}}<span class="danger-badge" title="--web.debug.dangerous: the debug page's playground runs Python and reads the server's files">Dangerous debug mode</span>{{end}}
{{- range .Links}}{{if .Current}}<span aria-current="page">{{.Label}}</span>{{else}}<a href="{{.Href}}">{{.Label}}</a>{{end}}{{end}}<a href="{{.Docs}}">Documentation</a>` + themePicker + `</span>
</nav></header>{{end}}`

// The theme is chosen in the bar of every page: Auto, the default, follows
// the browser's light or dark preference, and Light and Dark hold whatever
// it prefers. The choice is kept in the browser's local storage, so it holds
// on every page and every visit; where the browser keeps none, as in some
// private windows, it holds until the page is left. It is the data-theme
// attribute of the html element (pageStyle), which themeHead sets from what
// is kept before the page is first drawn, so a page never shows in the other
// theme first; the status page's refresh swaps in its content alone, under
// that element, and keeps it. Without JavaScript nothing could be kept or
// applied, so the picker stays hidden and the pages follow the browser.

// themeStorageKey is where the choice is kept: light or dark, and nothing
// for Auto.
const themeStorageKey = "theme"

// themeHead is the script every page's head runs first.
const themeHead = `<script id="theme-head">try { const theme = localStorage.getItem("` + themeStorageKey + `"); if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme; } catch (error) { /* Auto, then */ }</script>`

// themePicker is the picker in the bar, shown by its script, which runs in
// a function of its own so that nothing it declares meets a page's own
// script.
const themePicker = `<label class="theme" hidden>Theme <select id="theme">
<option value="auto">Auto</option><option value="light">Light</option><option value="dark">Dark</option>
</select></label><script id="theme-script">
(() => {
  const picker = document.getElementById("theme"), root = document.documentElement;
  picker.value = root.dataset.theme || "auto";
  picker.addEventListener("change", () => {
    if (picker.value === "auto") delete root.dataset.theme;
    else root.dataset.theme = picker.value;
    try {
      if (picker.value === "auto") localStorage.removeItem("` + themeStorageKey + `");
      else localStorage.setItem("` + themeStorageKey + `", picker.value);
    } catch (error) { /* chosen for this page alone */ }
  });
  picker.parentElement.hidden = false;
})();
</script>`

// chromeStyle is the bar's part of pageStyle.
const chromeStyle = `
header.site { margin: -16px 0 20px; padding-bottom: 10px; border-bottom: 1px solid var(--line); font-size: 14px; }
header.site nav { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: baseline; gap: 6px 20px; }
header.site .pages { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 16px; }
header.site a { text-decoration: none; }
header.site a:hover { text-decoration: underline; }
header.site [aria-current="page"] { color: var(--fg); font-weight: 600; }
header.site label.theme { color: var(--muted); }
header.site .danger-badge { padding: 1px 8px; border: 1px solid var(--bad); border-radius: 999px; background: var(--danger-bg); color: var(--bad); font-size: 12px; font-weight: 600; }
header.site label.theme[hidden] { display: none; }
header.site label.theme select { width: auto; height: 26px; margin: 0 0 0 4px; padding: 0 30px 0 8px; border: 1px solid var(--line); border-radius: 6px; background-color: var(--bg); color: var(--fg); font: inherit; font-size: 13px; background-position: calc(100% - 17px) 50%, calc(100% - 12px) 50%; }
`

// logoSVG is the project's logo, docs/logo.svg, which the go:embed pattern
// cannot reach from here: logo.svg is a copy of it, and
// TestThePagesIconIsTheProjectsLogo fails the moment the two differ.
//
//go:embed logo.svg
var logoSVG []byte

// pageIcon is the pages' browser-tab icon, the logo as a data: URL in the
// page itself, so it needs no request: none that the exporter's Basic Auth
// would ask a credential for, nor one a reverse proxy's path prefix would
// have to be reckoned with. pagePolicy's img-src allows data: for it. It is
// written into each page's template text rather than given to it as data,
// since html/template would refuse a data: URL put in an href.
var pageIcon = `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString(logoSVG) + `">`

// pageTemplate parses a page's template, text, with the bar it places and
// the template for a cell's long text (longtext.go).
func pageTemplate(name, text string) *template.Template {
	t := template.Must(template.New(name).Funcs(pageFuncs).Parse(chromeTemplate))
	return template.Must(template.Must(t.Parse(longTemplate)).Parse(text))
}
