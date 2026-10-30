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
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// longErrorServer is a server with two scraped static targets: one whose
// error is long, since it names a long metric and quotes the target's
// answer, markup and all, and
// one whose error is short.
func longErrorServer(t *testing.T) (*Server, string) {
	t.Helper()
	long := textTarget(t, "value=<script>alert(1)</script>"+strings.Repeat(" and more of the page", 20)+"\n")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	strict := testutil.Collector("strict", "text")
	strict.Metrics[0].ErrorMode = "fail"
	strict.Metrics[0].Expression = `value=(.+)`
	strict.Metrics[0].Name = "demo_value" + strings.Repeat("_of_a_rather_long_name", 4)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text"), strict}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "long", Collector: "strict", Target: long.URL},
		{Name: "short", Collector: "text", Target: down.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	for _, target := range server.manager.StaticTargets() {
		server.scrapeTarget(t.Context(), server.manager.Get(), target)
	}
	var longError string
	for _, row := range statusJSON(t, server, "/static-targets/status").Rows {
		if row.Name == "long" {
			longError = row.LastError
		}
	}
	if len([]rune(longError)) <= longPreviewRunes || !strings.Contains(longError, "<script>") {
		t.Fatalf("the long target's error is %q; the test needs one past %d characters quoting markup", longError, longPreviewRunes)
	}
	return server, longError
}

// A long error is shown as its start, in a <details> whose summary is a
// control labelled for what it does and whose body is the whole error, so
// the whole of it is on the page without JavaScript; both are escaped. A
// short error is shown whole, with no control.
func TestTheStatusPageShowsALongErrorsStartAndTheWholeOne(t *testing.T) {
	server, longError := longErrorServer(t)
	page := getPage(t, server, "/static-targets/status")
	if strings.Contains(page, "<script>alert") {
		t.Fatalf("the page has the error as markup:\n%s", page)
	}
	preview := clipLong("error", longError).Preview
	requireContains(t, page,
		`<td class="err"><details class="long"><summary aria-label="Show the whole error" data-kind="error"><span class="preview">`+templateEscaped(preview)+`</span></summary><pre class="whole">`+templateEscaped(longError)+`</pre></details><br>`,
		`<td class="err">received HTTP status 500<br>`)
	if strings.Count(page, `<details class="long">`) != 1 {
		t.Errorf("the page has a control for other than the one long error:\n%s", page)
	}
}

// The dialog that shows a long text whole is outside the content a refresh
// swaps, so a refresh cannot take it away or empty it; the fragment has the
// long texts' markup, which the page's listener opens, and no dialog.
func TestTheStatusPagesDialogIsOutsideTheSwappedContent(t *testing.T) {
	server, _ := longErrorServer(t)
	page := getPage(t, server, "/static-targets/status")
	content := strings.Index(page, `<div id="status-content">`)
	end := strings.Index(page, "\n</div>\n<dialog id=\"whole\"")
	script := strings.Index(page, "<script>")
	if content < 0 || end < content || script < end || strings.Count(page, "<dialog") != 1 {
		t.Fatalf("the dialog does not follow the content element, before the script:\n%s", page)
	}
	requireContains(t, page[end:script], `<dialog id="whole" aria-labelledby="whole-title">`, `<pre id="whole-text"></pre>`, `<button type="button" id="whole-close">Close</button>`)
	fragment := getPage(t, server, "/static-targets/status?format=fragment")
	if strings.Contains(fragment, "<dialog") || !strings.Contains(fragment, `<details class="long">`) {
		t.Errorf("the fragment has a dialog, or no long text's markup:\n%s", fragment)
	}
}

// The page's script opens a long text by a listener on the content element,
// which a refresh keeps, copies the text with textContent, never parsing it,
// skips a refresh while the dialog is open, resumes when it closes, and
// gives focus back to the control that opened it.
func TestTheStatusPagesScriptOpensLongTextsAndHoldsTheRefresh(t *testing.T) {
	server, _ := longErrorServer(t)
	page := getPage(t, server, "/static-targets/status")
	_, script, found := strings.Cut(page, "<script>")
	if !found {
		t.Fatalf("the page has no script:\n%s", page)
	}
	requireContains(t, script,
		`content.addEventListener("click", (event) => {`,
		`event.target.closest("details.long > summary")`,
		`$("whole-text").textContent = summary.parentElement.querySelector("pre.whole").textContent;`,
		`whole.showModal();`,
		`if (inFlight || whole.open) return;`,
		`whole.addEventListener("click", (event) => { if (event.target === whole) whole.close(); });`,
		`if (opener && opener.isConnected) opener.focus();`)
	_, closing, _ := strings.Cut(script, `whole.addEventListener("close"`)
	closing, _, _ = strings.Cut(closing, "});")
	if !strings.Contains(closing, "schedule();") {
		t.Errorf("closing the dialog does not resume the refresh:\n%s", closing)
	}
	// The one innerHTML is the swap of the fragment the exporter rendered.
	if n := regexp.MustCompile(`innerHTML`).FindAllStringIndex(script, -1); len(n) != 1 || !strings.Contains(script, "content.innerHTML = await response.text();") {
		t.Errorf("the script sets innerHTML other than to swap the fragment:\n%s", script)
	}
}

// Controls on one line line up: the debug page's Run button and its timeout
// box have one height and their bottoms aligned under the box's label, the
// fields of a row on the debug and collectors pages align at their bottoms
// when a label wraps, and the status page's Refresh button, auto-refresh
// choice and its label are centred on one line at one height.
func TestThePagesControlsOnOneLineLineUp(t *testing.T) {
	server := newStaticServer(t, weatherConfig(), nil)
	server.debug = DebugFull
	debug := getPage(t, server, "/debug")
	requireContains(t, debug,
		".actions { display: flex; align-items: flex-end;",
		".actions button, .actions input { height: 36px; }",
		".row { display: flex; align-items: flex-end;")
	requireContains(t, getPage(t, server, "/collectors"), ".row { display: flex; align-items: flex-end;")
	requireContains(t, getPage(t, server, "/static-targets/status"),
		"form.refresh label { display: inline-flex; align-items: center; gap: 6px; }",
		"form.refresh a.button, form.refresh button, form.refresh select { display: inline-flex; align-items: center; height: 30px;")
}
