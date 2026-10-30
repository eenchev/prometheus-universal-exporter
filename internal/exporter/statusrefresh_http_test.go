//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The status page's Refresh button and auto-refresh (statusrefresh.go).

// refreshChoices are the status page's auto-refresh choices, in order, each
// as value=text, the selected one marked with a *.
func refreshChoices(t *testing.T, page string) []string {
	t.Helper()
	start := strings.Index(page, `<select name="refresh" id="refresh-every">`)
	end := strings.Index(page, "</select>")
	if start < 0 || end < start {
		t.Fatalf("the page has no auto-refresh selector:\n%s", page)
	}
	var choices []string
	for _, m := range regexp.MustCompile(`<option value="([^"]*)" data-seconds="([0-9]+)"( selected)?>([^<]*)</option>`).FindAllStringSubmatch(page[start:end], -1) {
		choice := m[1] + "=" + m[4] + "/" + m[2]
		if m[3] != "" {
			choice += "*"
		}
		choices = append(choices, choice)
	}
	return choices
}

// grafanaChoices are the choices with off selected.
var grafanaChoices = []string{"off=Off/0*", "5s=5s/5", "10s=10s/10", "30s=30s/30", "1m=1m/60", "5m=5m/300", "15m=15m/900", "30m=30m/1800", "1h=1h/3600"}

// The page has a Refresh button, a link that reloads it without JavaScript,
// and Grafana's auto-refresh choices, off by default, with a form that puts
// the one chosen in the URL without JavaScript; its script fetches the
// content section, same-origin, one fetch at a time, keeps the interval in
// the URL, and stops while the tab is hidden. Off, no meta refresh reloads
// the page.
func TestTheStatusPageHasRefreshControls(t *testing.T) {
	server := newStaticServer(t, weatherConfig(), nil)
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page,
		`<form class="refresh" id="refresh" method="get">`,
		`<a class="button" id="refresh-now" href="">Refresh</a>`,
		`<noscript><button type="submit">Apply</button></noscript>`,
		`<span id="refresh-state" class="muted" role="status" aria-live="polite"></span>`,
		`<div id="status-content">`,
	)
	if got := refreshChoices(t, page); strings.Join(got, " ") != strings.Join(grafanaChoices, " ") {
		t.Errorf("the choices are %v, want %v", got, grafanaChoices)
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("with auto-refresh off the page reloads itself without JavaScript")
	}
	script := page[strings.Index(page, "<script>"):strings.LastIndex(page, "</script>")]
	requireContains(t, script,
		`url.searchParams.set("format", "fragment")`,
		`credentials: "same-origin"`,
		`cache: "no-store"`,
		`if (inFlight) return;`,
		`document.visibilityState === "hidden"`,
		`document.addEventListener("visibilitychange", schedule)`,
		`url.searchParams.set("refresh", every.value)`,
		`history.replaceState(null, "", url)`,
		`content.innerHTML = await response.text()`,
		`"Last refreshed at "`,
		`"Refresh failed at "`,
	)
}

// --web.status-refresh is the interval a URL without ?refresh= has, chosen
// in the selector and reloading the page through a meta refresh when
// JavaScript is off.
func TestTheStatusPageAutoRefreshDefaultsToTheFlag(t *testing.T) {
	server := newStaticServer(t, weatherConfig(), nil)
	server.SetStatusRefresh(30 * time.Second)
	page := getPage(t, server, "/static-targets/status")
	want := []string{"off=Off/0", "5s=5s/5", "10s=10s/10", "30s=30s/30*", "1m=1m/60", "5m=5m/300", "15m=15m/900", "30m=30m/1800", "1h=1h/3600"}
	if got := refreshChoices(t, page); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the choices are %v, want %v", got, want)
	}
	requireContains(t, page, `<noscript><meta http-equiv="refresh" content="30"></noscript>`)
}

// ?refresh= chooses the interval over the flag's, off among them; a value
// that is no auto-refresh — below 5s, above a day, not whole seconds, not a
// duration — is ignored and the flag's applies; one of none of Grafana's
// choices is put among them in its place.
func TestTheStatusPageReadsTheRefreshParameter(t *testing.T) {
	server := newStaticServer(t, weatherConfig(), nil)
	server.SetStatusRefresh(time.Minute)
	for query, selected := range map[string]string{
		"":                         "1m=1m/60*",
		"?refresh=5m":              "5m=5m/300*",
		"?refresh=300s":            "5m=5m/300*",
		"?refresh=off":             "off=Off/0*",
		"?refresh=1s":              "1m=1m/60*",
		"?refresh=4s":              "1m=1m/60*",
		"?refresh=0s":              "1m=1m/60*",
		"?refresh=-30s":            "1m=1m/60*",
		"?refresh=1500ms":          "1m=1m/60*",
		"?refresh=25h":             "1m=1m/60*",
		"?refresh=often":           "1m=1m/60*",
		"?refresh=%3Cb%3E":         "1m=1m/60*",
		"?refresh=Off":             "1m=1m/60*",
		"?refresh=20s":             "20s=20s/20*",
		"?refresh=90s":             "1m30s=1m30s/90*",
		"?refresh=2h":              "2h=2h/7200*",
		"?refresh=24h":             "24h=24h/86400*",
		"?format=html&refresh=10s": "10s=10s/10*",
	} {
		page := getPage(t, server, "/static-targets/status"+query)
		got := refreshChoices(t, page)
		var chosen []string
		for _, choice := range got {
			if strings.HasSuffix(choice, "*") {
				chosen = append(chosen, choice)
			}
		}
		if len(chosen) != 1 || chosen[0] != selected {
			t.Errorf("%q: the choices are %v, want %s selected alone", query, got, selected)
		}
	}
	// A choice of none of Grafana's is put in its place among them.
	page := getPage(t, server, "/static-targets/status?refresh=20s")
	want := []string{"off=Off/0", "5s=5s/5", "10s=10s/10", "20s=20s/20*", "30s=30s/30", "1m=1m/60", "5m=5m/300", "15m=15m/900", "30m=30m/1800", "1h=1h/3600"}
	if got := refreshChoices(t, page); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("?refresh=20s: the choices are %v, want %v", got, want)
	}
	requireContains(t, page, `<noscript><meta http-equiv="refresh" content="20"></noscript>`)
	if page := getPage(t, server, "/static-targets/status?refresh=off"); strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("?refresh=off still reloads the page without JavaScript")
	}
}

// ?format=fragment is the page's content section alone, as the page has it
// inside its content element, with the page's headers: HTML, never cached,
// under the pages' policy. It has no controls and no script of its own.
func TestTheStatusFragmentIsThePageContentAlone(t *testing.T) {
	server := newStaticServer(t, weatherConfig(), nil)
	server.SetStatusRefresh(time.Minute)
	fragment := getPage(t, server, "/static-targets/status?format=fragment&refresh=5s")
	for _, unwanted := range []string{"<!DOCTYPE", "<html", "<head", "<body", "<main", "<script", "<form", "<style", "<h1>", `id="status-content"`, `http-equiv`} {
		if strings.Contains(fragment, unwanted) {
			t.Errorf("the fragment has %q:\n%s", unwanted, fragment)
		}
	}
	requireContains(t, fragment, `<p class="meta">As of `, "<h2>Static targets</h2>", "<h2>Recent probes</h2>", `<h2 id="otlp">OTLP export</h2>`)
	page := getPage(t, server, "/static-targets/status")
	// The two are rendered a moment apart, so the time each is as of is
	// taken out before they are compared.
	asOf := regexp.MustCompile(`As of [^.<]*\.`)
	start := strings.Index(page, `<div id="status-content">`) + len(`<div id="status-content">`)
	end := strings.Index(page, "\n</div>\n<script>")
	if start < len(`<div id="status-content">`) || end < start {
		t.Fatalf("the page has no content element:\n%s", page)
	}
	if got, want := asOf.ReplaceAllString(page[start:end], ""), asOf.ReplaceAllString(fragment, ""); got != want {
		t.Errorf("the page's content is not the fragment:\npage:\n%s\nfragment:\n%s", got, want)
	}
	r := getPath(server, http.MethodGet, "/static-targets/status?format=fragments")
	if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "html, json or fragment") {
		t.Errorf("an unknown format answered %d: %s", r.Code, r.Body)
	}
}

// The fragment is behind the exporter's Basic Auth and its cross-site
// refusal as the page is, and a same-origin fetch with the credential, as
// the page's script makes, is answered.
func TestTheStatusFragmentIsGuardedAsThePageIs(t *testing.T) {
	server, err := webAuthServer(t, &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	send := func(auth bool, site string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/static-targets/status?format=fragment", nil)
		if auth {
			request.SetBasicAuth("exporter", "secret")
		}
		if site != "" {
			request.Header.Set("Sec-Fetch-Site", site)
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	if r := send(false, "same-origin"); r.Code != http.StatusUnauthorized {
		t.Errorf("without the credential the fragment answered %d", r.Code)
	}
	if r := send(true, "cross-site"); r.Code != http.StatusForbidden {
		t.Errorf("cross-site the fragment answered %d", r.Code)
	}
	r := send(true, "same-origin")
	if r.Code != http.StatusOK || r.Header().Get("Cache-Control") != "no-store" || !strings.Contains(r.Body.String(), "<h2>Static targets</h2>") || strings.Contains(r.Body.String(), "<html") {
		t.Errorf("same-origin with the credential the fragment answered %d, Cache-Control %q: %s", r.Code, r.Header().Get("Cache-Control"), r.Body)
	}
}
