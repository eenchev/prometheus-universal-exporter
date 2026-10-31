//go:build !select_request_types || request_type_dns || request_type_http || request_type_kubernetes || request_type_localfile || request_type_sql || request_type_sshfile

// The helpers of the debug page's per-type form tests, built with any of
// the types those tests are for: a build with none of them, graphite's
// alone say, has none of the tests and would find these unused.

package exporter

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The debug page asks a run of a collector for what its request type takes
// (debugForm): these are the helpers of the tests of each type's form,
// debugform_<type>_test.go.

// debugFormServer serves the collectors YAML lists, as a file writes them
// under collectors:, with debugging at full.
func debugFormServer(t *testing.T, collectors string) *Server {
	t.Helper()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n"+collectors))
	if err != nil {
		t.Fatal(err)
	}
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	return server
}

// debugFormPage is the debug page opened on the collector named name, and
// the form /debug/collector answers with it, which must be the one the
// page holds for it; for no name, the page opened on the playground.
func debugFormPage(t *testing.T, server *Server, name string) (string, debugForm) {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug?collector="+name, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/debug answered %d: %s", recorder.Code, recorder.Body)
	}
	page := recorder.Body.String()
	if name == "" {
		// The playground, which /debug/collector does not answer.
		return page, debugForm{}
	}
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/collector?name="+name, nil))
	var answer debugCollectorAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Form == nil {
		t.Fatalf("/debug/collector answered %d without a form (%v): %s", recorder.Code, err, recorder.Body)
	}
	start := strings.Index(page, `<script type="application/json" id="forms">`)
	end := strings.Index(page[max(start, 0):], "</script>")
	if start < 0 || end < 0 {
		t.Fatalf("the page holds no forms:\n%s", page)
	}
	var held debugForms
	if err := json.Unmarshal([]byte(page[start+len(`<script type="application/json" id="forms">`):start+end]), &held); err != nil {
		t.Fatal(err)
	}
	if held.Collectors[name] != *answer.Form {
		t.Errorf("the page holds the form %+v of %s, /debug/collector answers %+v", held.Collectors[name], name, *answer.Form)
	}
	return page, *answer.Form
}

// requireDebugForm checks that page, opened on a collector, asks for what
// form says, as the page's fields first show it: the target's label and
// placeholder, or the note that the collector names its own; the probe
// parameters' placeholder, or the note that it takes none; the body's
// placeholder; and the Content-Type, status and headers shown or hidden.
func requireDebugForm(t *testing.T, page string, form debugForm) {
	t.Helper()
	hidden := func(on bool) string {
		if on {
			return " hidden"
		}
		return ""
	}
	if form.NoTarget {
		requireInPage(t, page, `<div id="target-part" hidden>`, `<p class="note" id="no-target">This collector names its own target.</p>`)
	} else {
		requireInPage(t, page, `<div id="target-part">`, `<label for="target" id="target-label">`+html.EscapeString(form.TargetLabel)+`</label>`,
			`<input id="target" type="text" placeholder="`+html.EscapeString(form.TargetHint)+`" autocomplete="off">`, `<p class="note" id="no-target" hidden>`)
	}
	if form.Params == "" {
		requireInPage(t, page, `<div id="params-part" hidden>`, `<p class="note" id="no-params">This collector takes no probe parameters.</p>`)
	} else {
		requireInPage(t, page, `<div id="params-part">`, `placeholder="`+html.EscapeString(form.Params)+`"></textarea>`, `<p class="note" id="no-params" hidden>`)
	}
	requireInPage(t, page, `<div id="content-type-part"`+hidden(!form.ContentType)+`>`, `<div class="narrow" id="status-part"`+hidden(!form.HTTPAnswer)+`>`,
		`<div id="headers-part"`+hidden(!form.HTTPAnswer)+`>`, `placeholder="`+html.EscapeString(form.Answer)+`" data-example=`)
}

// requireInPage is requireContains, which builds without http and localfile.
func requireInPage(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}
