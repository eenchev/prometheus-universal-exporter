package exporter

import (
	"html/template"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
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

// The theme is chosen in the bar of every page, as on the allocator's
// (pagekit): themeHead sets the choice kept before the page is first drawn,
// and themePicker, in the bar, changes it; the status page's refresh swaps
// in its content alone and keeps it.
const (
	themeHead   = pagekit.ThemeHead
	themePicker = pagekit.ThemePicker
)

// pageIcon is the pages' browser-tab icon, the project's logo as a data:
// URL (pagekit.Icon), which no request of the browser needs the exporter's
// Basic Auth for.
var pageIcon = pagekit.Icon

// pageTemplate parses a page's template, text, with the bar it places and
// the template for a cell's long text (longtext.go).
func pageTemplate(name, text string) *template.Template {
	t := template.Must(template.New(name).Funcs(pageFuncs).Parse(chromeTemplate))
	return template.Must(template.Must(t.Parse(longTemplate)).Parse(text))
}
