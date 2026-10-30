//go:build !select_request_types || request_type_http

package exporter

import (
	"html"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The bar at the top of the pages (pagechrome.go).

// chromeOpening is how the bar starts.
const chromeOpening = `<header class="site"><nav aria-label="The exporter's pages">`

// Every HTML page the exporter serves carries the bar, once: the way back to
// the landing page, the other pages, the one shown marked, and the
// documentation. The pages are found by following every link from the
// landing page, as a visitor would, so a page added later and linked from
// any page is held to it too; each level of --web.debug and a static targets
// path several segments deep, which puts the status page further down, are
// crawled behind a proxy's path prefix. The status page's content section,
// which its refresh swaps in, carries no bar of its own.
func TestEveryPageCarriesTheBar(t *testing.T) {
	const base = "https://proxy.example/exporter"
	href := regexp.MustCompile(`href="([^"]*)"`)
	bar := regexp.MustCompile(`(?s)<header class="site">.*?</header>`)
	back := regexp.MustCompile(`<a class="home" href="[^"]*">← `)
	barLink := regexp.MustCompile(`<a(?: class="home")? href="([^"]*)">([^<]*)</a>|<span(?: class="home")? aria-current="page">([^<]*)</span>`)
	for _, debug := range []DebugLevel{DebugOff, DebugEdit, DebugFull} {
		for _, staticPath := range []string{"/static-targets", "/static/targets/deep"} {
			server := landingServer(t, weatherConfig())
			server.SetDebug(debug)
			server.SetStaticTargetsPath(staticPath)
			status := staticPath + "/status"
			want := map[string]string{"/": "Prometheus Universal Exporter", "/collectors": "Collectors", status: "Status"}
			if debug != DebugOff {
				want["/debug"] = "Debug"
			}
			where := "--web.debug=" + debug.String() + ", " + status
			pages := map[string]bool{}
			seen := map[string]bool{"/": true}
			queue := []string{"/"}
			for len(queue) > 0 {
				local := queue[0]
				queue = queue[1:]
				response := getPath(server, http.MethodGet, local)
				if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
					continue
				}
				body := response.Body.String()
				path, _, _ := strings.Cut(local, "?")
				pages[path] = true
				from, err := url.Parse(base + local)
				if err != nil {
					t.Fatal(err)
				}
				for _, match := range href.FindAllStringSubmatch(body, -1) {
					target, err := from.Parse(html.UnescapeString(match[1]))
					if err != nil || target.Host != "proxy.example" || !strings.HasPrefix(target.Path, "/exporter") {
						continue
					}
					next := strings.TrimPrefix(target.Path, "/exporter")
					if next == "" {
						next = "/"
					}
					if target.RawQuery != "" {
						next += "?" + target.RawQuery
					}
					if !seen[next] {
						seen[next] = true
						queue = append(queue, next)
					}
				}
				if n := strings.Count(body, chromeOpening); n != 1 {
					t.Errorf("%s: %s has the bar %d times", where, local, n)
					continue
				}
				if !strings.Contains(body, "<main>\n"+chromeOpening) {
					t.Errorf("%s: %s does not start its main element with the bar", where, local)
				}
				// The bar: home, then every page, each reached from here.
				var current []string
				reached := map[string]bool{}
				for _, link := range barLink.FindAllStringSubmatch(bar.FindString(body), -1) {
					if link[3] != "" {
						current = append(current, link[3])
						continue
					}
					target, err := from.Parse(html.UnescapeString(link[1]))
					if err != nil {
						t.Fatal(err)
					}
					if target.Host != "proxy.example" {
						continue // the documentation
					}
					if !strings.HasPrefix(target.Path+"/", base[len("https://proxy.example"):]+"/") {
						t.Errorf("%s: %s links %q, outside the prefix: %s", where, local, link[1], target)
						continue
					}
					reached[strings.TrimPrefix(target.Path, "/exporter")] = true
				}
				if len(current) != 1 || current[0] != want[path] {
					t.Errorf("%s: %s marks %q as the page shown, want %q", where, local, current, want[path])
				}
				for page := range want {
					if page != path && !reached[page] {
						t.Errorf("%s: the bar of %s does not lead to %s", where, local, page)
					}
				}
				if path != "/" && !back.MatchString(body) {
					t.Errorf("%s: %s has no way back to the landing page", where, local)
				}
			}
			if got, wanted := slices.Sorted(maps.Keys(pages)), slices.Sorted(maps.Keys(want)); !slices.Equal(got, wanted) {
				t.Errorf("%s: the HTML pages reached are %q, want %q", where, got, wanted)
			}
			fragment := getPath(server, http.MethodGet, status+"?format=fragment").Body.String()
			for _, part := range []string{"<header", "<nav", "<html", "<main"} {
				if strings.Contains(fragment, part) {
					t.Errorf("%s: the status page's fragment has %s", where, part)
				}
			}
			if !strings.Contains(fragment, "<h2>Static targets</h2>") {
				t.Errorf("%s: the status page's fragment is not its content:\n%.300s", where, fragment)
			}
		}
	}
}
