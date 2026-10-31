//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The debug page's Copy as: Probe a target's form as a /probe URL, a curl
// command, a Prometheus job and a debug probe's URL, and a static target as
// a /static-targets URL and its curl.

// Probe a target and Scrape a static target each have a Copy as row, its
// buttons, a note and the box that shows what is copied; the script makes
// the URLs from the page's own address, quotes for the shell and YAML, and
// writes the multi-target job the docs recommend.
func TestTheDebugPageCopiesAProbeAsAURLCurlAndAScrapeConfig(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	if page := getPage(t, server, "/debug?collector=rooms"); strings.Contains(page, `id="probe-copy"`) || strings.Contains(page, `id="static-copy"`) {
		t.Error("at edit, which probes nothing, the page offers Copy as")
	}
	server.SetDebug(DebugFull)
	page := getPage(t, server, "/debug?collector=rooms")
	requireContains(t, page,
		// The markup, the buttons in one row with the text Copy as.
		`<div class="copy-as" id="probe-copy" role="group" aria-labelledby="probe-copy-title">`,
		`<div class="copy-row"><span class="kv-title" id="probe-copy-title">Copy as</span>`,
		`<button type="button" class="secondary" data-copy="url" data-what="probe URL" aria-controls="probe-copy-text" aria-pressed="false">URL</button>`,
		`<button type="button" class="secondary" data-copy="curl" data-what="curl command" aria-controls="probe-copy-text" aria-pressed="false">curl</button>`,
		`<button type="button" class="secondary" data-copy="scrape" data-what="scrape_config" aria-controls="probe-copy-text" aria-pressed="false">scrape_config</button>`,
		`<button type="button" class="secondary" data-copy="debug" data-what="debug report URL" aria-controls="probe-copy-text" aria-pressed="false">Debug report URL</button>`,
		`<pre class="copy-text" id="probe-copy-text" tabindex="0" aria-label="The text copied" hidden></pre>`,
		`<div class="copy-as" id="static-copy" role="group" aria-labelledby="static-copy-title">`,
		`<button type="button" class="secondary" data-copy="url" data-what="static targets URL" aria-controls="static-copy-text" aria-pressed="false">URL</button>`,
		`<span class="copy-done" data-copy-done role="status"></span>`,
		`.copy-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }`, `.copy-row button { margin: 0; }`,
		`pre.copy-text { margin-top: 8px; max-height: 260px; white-space: pre; overflow-wrap: normal; }`,
		// Quoting: the shell's single quotes, YAML's single-quoted scalars.
		`const shellQuoted = (text) => "'" + text.replaceAll("'", "'\\''") + "'";`,
		`const yamlQuoted = (text) => "'" + text.replaceAll("'", "''") + "'";`,
		`const curlOf = (url) => "curl -s " + (basicAuth ? "-u USER " : "") + shellQuoted(url);`,
		// The URL: the exporter's /probe as the page's own requests resolve
		// it, so a path prefix is kept, the collector, the target and every
		// parameter, a repeated header_<name> repeated, never debug=true but
		// in the debug report's URL.
		`const url = new URL("probe", document.baseURI);`,
		`url.searchParams.append("collector", through);`,
		`if (target !== null) url.searchParams.append("target", target);`,
		`const given = own ? Object.entries(own.values((name) => name)).flatMap(([name, values]) => [values].flat().map((value) => [name, value])) : [];`,
		`for (const [name, value] of given) url.searchParams.append(name, value);`,
		`debug.searchParams.append("debug", "true");`,
		// What stops it, and the playground copied from a collector.
		`if (through === "") return { blocked: true, note: "Choose a configured collector, or Edit a copy of one, to copy a probe." };`,
		`const target = form.no_target ? null : $("target").value;`,
		`if (target !== null && target.trim() === "") return { blocked: true, note: "Give the target to copy the probe." };`,
		`if (own && own.wrong()) return { blocked: true, note: "A probe parameter row is wrong; put it right to copy the probe." };`,
		`wrong: () => rows.some((row, i) => messageOf(i) !== ""),`,
		`"These probe through " + through + ", the collector this playground was copied from, as configured: /probe runs only the configured collectors, so the playground's edits are not in them."`,
		// The job: docs/LOCALFILE.md's multi-target pattern.
		`const lines = ["scrape_configs:", "  - job_name: " + yamlQuoted(collector)];`,
		`if (url.protocol === "https:") lines.push("    scheme: https");`,
		`lines.push("    metrics_path: " + yamlQuoted(url.pathname), "    params:");`,
		`const listed = new Map([["collector", [collector]]]);`,
		`lines.push("      " + yamlKey(name) + ": [" + values.map(yamlQuoted).join(", ") + "]");`,
		`"      - source_labels: [__address__]", "        target_label: __param_target",`,
		`"      - source_labels: [__param_target]", "        target_label: instance",`,
		`"      " + exporter, "      - target_label: __address__", "        replacement: " + yamlQuoted(url.host));`,
		`"      - targets: [" + yamlQuoted(url.host) + "]"`,
		// The static target's.
		`const url = new URL("static-targets", document.baseURI);`, `url.searchParams.set("targets", name);`,
		// Copied, or shown to be selected.
		`await navigator.clipboard.writeText(text);`, `say("Copied the " + button.dataset.what + ".", true);`,
		`say("Select the text below to copy it.");`, `say("The browser did not let the page copy it; select the text below.");`,
		`pre.textContent = text;`, `note.textContent = made.note;`,
		`$("target").addEventListener("input", probeCopy);`,
		`for (const event of ["input", "click"]) $("params-part").addEventListener(event, probeCopy);`,
	)
	for _, never := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "<main data-basic-auth"} {
		if strings.Contains(page, never) {
			t.Errorf("the page has %q", never)
		}
	}
}

// With web.basic_auth on, the page says so to Copy as, which gives curl
// -u USER and the job a basic_auth to fill in, and never the credential.
func TestTheDebugPageCopiesNoCredential(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "copy-user", Password: "copy-secret"}}}
	server := debugPageServer(t, cfg)
	server.SetDebug(DebugFull)
	request := httptest.NewRequest(http.MethodGet, "/debug?collector=rooms", nil)
	request.SetBasicAuth("copy-user", "copy-secret")
	response := serve(server, request)
	if response.Code != http.StatusOK {
		t.Fatalf("the page answered %d", response.Code)
	}
	page := response.Body.String()
	requireContains(t, page, `<main data-basic-auth>`,
		`const basicAuth = document.querySelector("main").hasAttribute("data-basic-auth");`,
		`const authNote = basicAuth ? " The exporter asks for web.basic_auth: curl -u USER asks for USER's password, and the job's basic_auth is left for you to fill in; no credential is put in these."`,
		`lines.push("    # The exporter asks for web.basic_auth:", "    # basic_auth:", "    #   username: USER", "    #   password_file: /path/to/the/password");`,
	)
	for _, never := range []string{"copy-user", "copy-secret"} {
		if strings.Contains(page, never) {
			t.Errorf("the page has the credential's %q", never)
		}
	}
}
