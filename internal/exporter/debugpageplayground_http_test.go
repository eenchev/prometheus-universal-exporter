//go:build !select_request_types || request_type_http

package exporter

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// debugPageScript is the debug page's script, its strings and comments in.
func debugPageScript(page string) string {
	return page[strings.Index(page, "<script>"):strings.LastIndex(page, "</script>")]
}

// The collector written on the page is the playground, by that name, in the
// list, the editor's label and the notes; the page names it nothing else.
func TestTheDebugPageCallsItsWrittenCollectorThePlayground(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	server.SetDebug(DebugFull)
	for _, path := range []string{"/debug", "/debug?collector=rooms"} {
		page := getPage(t, server, path)
		requireContains(t, page, `<option value="">Playground</option>`, "The playground never runs Python",
			`"Playground: the YAML of a collector, which runs"`, `"Playground, a copy of " + copiedFrom + ": the YAML of a collector, which runs"`,
			"<em>Edit a copy</em> makes the playground a copy of it")
		for _, gone := range []string{"written on this page", "written here", "The one written"} {
			if strings.Contains(page, gone) {
				t.Errorf("%s still says %q", path, gone)
			}
		}
	}
	requireContains(t, getPage(t, server, "/debug"), `<label for="collector" id="collector-label">Playground: the YAML of a collector, which runs</label>`)
}

// Runs happen when Run is pressed and at no other time: there is no switch
// to run as one types, and no timer that would.
func TestTheDebugPageHasNoRunAsYouType(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	page := getPage(t, server, "/debug")
	for _, gone := range []string{"Run as you type", `id="live"`, `role="switch"`, "setTimeout", "clearTimeout", ".switch"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %q", gone)
		}
	}
	requireContains(t, page, `<button type="button" id="run">Run</button>`, `$("run").addEventListener("click", run);`)
}

// A target is fetched through the collector chosen on the left, or through
// the one the playground was copied from, and through no collector picked
// apart from it: there is no Fetch through list. A playground copied from
// nothing makes no request, so Fetch a target is offered disabled, with a
// note saying how to make it available.
func TestTheDebugPageFetchesThroughTheCollectorChosenOrCopied(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	server.SetDebug(DebugFull)
	for _, path := range []string{"/debug", "/debug?collector=rooms"} {
		page := getPage(t, server, path)
		for _, gone := range []string{"request-from", "Fetch through", "The configured collector run"} {
			if strings.Contains(page, gone) {
				t.Errorf("%s still has %q", path, gone)
			}
		}
	}
	playground := getPage(t, server, "/debug")
	requireContains(t, playground, `<input type="radio" name="mode" value="fetch" id="mode-fetch" disabled> Fetch a target`,
		`<p class="note" id="fetch-unavailable">Choose a configured collector, or <em>Edit a copy</em> of one, to fetch a target.</p>`,
		`<p class="note" id="fetched-through"></p>`)
	chosen := getPage(t, server, "/debug?collector=rooms")
	requireContains(t, chosen, `<input type="radio" name="mode" value="fetch" id="mode-fetch"> Fetch a target`,
		`<p class="note" id="fetch-unavailable" hidden>`, `<p class="note" id="fetched-through">Fetched through rooms, the collector chosen.</p>`)
	script := debugPageScript(chosen)
	// The playground's fetch names the collector it was copied from, which
	// Edit a copy remembers, kept with the playground between visits; the
	// fetch radio is disabled while there is neither it nor a configured
	// collector chosen.
	requireContains(t, script,
		`if (request.collector !== undefined) request.request_from = copiedFrom;`,
		`copiedFrom = $("configured").value;`,
		`store.setItem("debug.copied", copiedFrom);`,
		`const fetcher = () => $("configured").value || copiedFrom;`,
		`radio.disabled = through === "";`,
		`"Fetched through " + through + ", the collector this playground was copied from."`)
	if n := strings.Count(script, "request_from"); n != 1 {
		t.Errorf("the script sets request_from %d times, want once, from copiedFrom", n)
	}
}

// The example body fits the example collector only: with a configured
// collector chosen the body starts empty, with a placeholder, and the
// script empties or restores it on a change of collector only while it is
// empty or the example, never what the visitor wrote.
func TestTheDebugPagesBodyFitsTheCollectorChosen(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	body := regexp.MustCompile(`<textarea id="body"[^>]*>([^<]*)</textarea>`)
	attributes := `<textarea id="body" rows="14" spellcheck="false" autocomplete="off" placeholder="Paste what the target answers" data-example="` + html.EscapeString(debugExampleBody) + `">`
	playground := getPage(t, server, "/debug")
	requireContains(t, playground, attributes)
	if got := body.FindStringSubmatch(playground); got == nil || html.UnescapeString(got[1]) != debugExampleBody {
		t.Errorf("the playground's body is %q, want the example", got)
	}
	chosen := getPage(t, server, "/debug?collector=rooms")
	requireContains(t, chosen, attributes)
	if got := body.FindStringSubmatch(chosen); got == nil || got[1] != "" {
		t.Errorf("a configured collector's body is %q, want it empty", got)
	}
	requireContains(t, debugPageScript(chosen),
		`if (body.value !== example && body.value !== "") return;`,
		`body.value = $("configured").value === "" && copiedFrom === "" ? example : "";`)
}
