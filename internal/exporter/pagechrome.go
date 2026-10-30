package exporter

import "html/template"

// Every HTML page of the exporter — the landing page, the collectors page,
// the debug page and the status page — is framed alike: the bar at the top
// of each (pageChrome), the stylesheet (pageStyle), light or dark as the
// browser prefers, and the headers renderPage sets. The bar leads back to the
// landing page and to every other page from wherever a page is served, so no
// page is a dead end, and a page added later gets it by being rendered at
// all: renderPage takes only data that carries a bar (framedPage).

// pageChrome is the bar at the top of a page: Home, the landing page as a
// link relative to the page, empty on the landing page itself; Links, the
// other pages, the one shown marked; and Docs, the documentation.
type pageChrome struct {
	Home  string
	Links []chromeLink
	Docs  string
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
	chrome := pageChrome{Docs: landingDocs}
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
{{- range .Links}}{{if .Current}}<span aria-current="page">{{.Label}}</span>{{else}}<a href="{{.Href}}">{{.Label}}</a>{{end}}{{end}}<a href="{{.Docs}}">Documentation</a></span>
</nav></header>{{end}}`

// chromeStyle is the bar's part of pageStyle.
const chromeStyle = `
header.site { margin: -16px 0 20px; padding-bottom: 10px; border-bottom: 1px solid var(--line); font-size: 14px; }
header.site nav { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: baseline; gap: 6px 20px; }
header.site .pages { display: flex; flex-wrap: wrap; gap: 4px 16px; }
header.site a { text-decoration: none; }
header.site a:hover { text-decoration: underline; }
header.site [aria-current="page"] { color: var(--fg); font-weight: 600; }
`

// pageTemplate parses a page's template, text, with the bar it places.
func pageTemplate(name, text string) *template.Template {
	return template.Must(template.Must(template.New(name).Parse(chromeTemplate)).Parse(text))
}
