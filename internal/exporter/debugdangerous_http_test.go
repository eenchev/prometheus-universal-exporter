//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// --web.debug.dangerous (debuglevel.go): the playground may run Python,
// name the server's files and make its own request, at full alone.

// dangerousServer serves cfg at full, with --web.debug.dangerous as given.
func dangerousServer(t *testing.T, cfg *model.Config, dangerous bool) *Server {
	t.Helper()
	server := debugPageServer(t, cfg)
	server.SetDebug(DebugFull)
	server.SetDebugDangerous(dangerous)
	return server
}

// The flag is refused below full, and taken at full or left off at any
// level.
func TestTheDangerousDebugModeNeedsFull(t *testing.T) {
	for _, level := range []DebugLevel{DebugOff, DebugEdit} {
		err := ValidateDebugDangerous(level, true)
		if err == nil || !strings.Contains(err.Error(), "--web.debug.dangerous needs --web.debug=full, not "+level.String()) {
			t.Errorf("at %s: %v", level, err)
		}
	}
	for _, level := range []DebugLevel{DebugOff, DebugEdit, DebugFull} {
		if err := ValidateDebugDangerous(level, false); err != nil {
			t.Errorf("off at %s: %v", level, err)
		}
	}
	if err := ValidateDebugDangerous(DebugFull, true); err != nil {
		t.Errorf("at full: %v", err)
	}
}

// The playground's Python is refused without the flag, and with it runs
// as a configured collector's does, a python transform and a pre_script, in
// the pool's workers; it counts for the pool alone, under no collector's
// name, a configured one of the same name included, and leaves no worker
// idle.
func TestTheDangerousDebugModeRunsThePlaygroundsPython(t *testing.T) {
	requirePython(t)
	transformYAML := "name: scripted\nrequest:\n  type: http\ndecoder:\n  type: text\ntransform:\n  type: python\n  script: |\n    metric(name=\"length\", value=len(str(data)))\n"
	preYAML := strings.Replace(roomsYAML("rooms"), "  type: jq\n", "  type: jq\n  pre_script: |\n    data['rooms'] = data['rooms'][:1]\n", 1)
	scripted := pythonCollector("scripted", "metric(name=\"other\", value=1)\n")
	off := dangerousServer(t, &model.Config{Collectors: []model.Collector{scripted}}, false)
	for _, yaml := range []string{transformYAML, preYAML} {
		if answer := postRun(t, off, pasted(yaml, debugExampleBody), http.StatusUnprocessableEntity); !strings.Contains(answer.Error, "never runs Python") {
			t.Errorf("without the flag: %q", answer.Error)
		}
	}
	server := dangerousServer(t, &model.Config{Collectors: []model.Collector{scripted}}, true)
	answer := postRun(t, server, map[string]any{"collector": transformYAML, "body": "four"}, http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, "length 4") || !answer.Dangerous {
		t.Fatalf("the python transform: %+v", answer)
	}
	answer = postRun(t, server, pasted(preYAML, debugExampleBody), http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, `room="kitchen"`) || strings.Contains(answer.Series, `room="attic"`) {
		t.Fatalf("the pre_script: %+v", answer)
	}
	pool := transform.PythonWorkers()
	if runs := pool.Snapshot("scripted").Runs["ok"]; runs != 0 {
		t.Errorf("the configured collector of the playground's name counted %d runs", runs)
	}
	whole := pool.PoolSnapshot()
	if whole.Runs["ok"] != 2 || whole.Idle != 0 {
		t.Errorf("the pool counted %d runs and has %d workers idle, want 2 and none", whole.Runs["ok"], whole.Idle)
	}
}

// Without the flag a playground that names a file is refused, and one
// fetching a target with no request_from is told it makes no request of
// its own. With it, its own request is made, as a configured collector's
// probe makes one: with the credential its file holds, and with the target
// policy it writes; the files a request needs are required of it then.
func TestTheDangerousDebugModeMakesThePlaygroundsOwnRequestWithItsFiles(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(roomsYAML("mine"), "  type: http\n", "  type: http\n  bearer_token_file: "+token+"\n", 1)
	own := map[string]any{"collector": yaml, "target": target.URL}
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}
	off := dangerousServer(t, cfg, false)
	if answer := postRun(t, off, pasted(yaml, debugExampleBody), http.StatusUnprocessableEntity); !strings.Contains(answer.Error, "request.bearer_token_file, which names a file on the server") {
		t.Errorf("a file without the flag: %q", answer.Error)
	}
	if answer := postRun(t, off, map[string]any{"collector": roomsYAML("mine"), "target": target.URL}, http.StatusBadRequest); !strings.Contains(answer.Error, "makes no request of its own") || got != nil {
		t.Errorf("its own request without the flag: %q, the target got %v", answer.Error, got)
	}
	server := dangerousServer(t, cfg, true)
	answer := postRun(t, server, own, http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, `room_temperature_celsius{room="attic"} 17`) || !answer.Dangerous {
		t.Fatalf("its own request: %+v", answer)
	}
	if auth := got.Get("Authorization"); auth != "Bearer file-token" {
		t.Errorf("the target was sent Authorization %q, want the file's token", auth)
	}
	if !strings.Contains(answer.Report, `fetched through collector "mine"`) || strings.Contains(answer.Report, "file-token") {
		t.Errorf("the report:\n%s", answer.Report)
	}
	// A pasted response still makes no request.
	got = nil
	if answer := postRun(t, server, pasted(yaml, debugExampleBody), http.StatusOK); !answer.OK || got != nil {
		t.Errorf("a pasted run with the flag: %+v, the target got %v", answer, got)
	}
	// Its own target policy holds.
	guarded := strings.Replace(yaml, "  type: http\n", "  type: http\n  allowed_targets: [example.invalid]\n", 1)
	answer = postRun(t, server, map[string]any{"collector": guarded, "target": target.URL}, http.StatusOK)
	if answer.OK || !strings.HasPrefix(answer.Verdict, "403: ") || got != nil {
		t.Errorf("its allowed_targets did not refuse the target: %+v", answer)
	}
	// A file a request reads must be there, as a load requires.
	missing := strings.Replace(yaml, token, token+".missing", 1)
	if answer := postRun(t, server, map[string]any{"collector": missing, "target": target.URL}, http.StatusOK); answer.OK || got != nil {
		t.Errorf("a missing token file: %+v", answer)
	}
	// request_from still fetches through the configured collector.
	answer = postRun(t, server, map[string]any{"collector": yaml, "request_from": "rooms", "target": target.URL}, http.StatusOK)
	if !answer.OK || got.Get("Authorization") != "" {
		t.Errorf("through rooms: %+v, Authorization %q", answer, got.Get("Authorization"))
	}
}

// ${NAME} references stay as written with the flag too.
func TestTheDangerousDebugModeLeavesEnvironmentReferencesAsWritten(t *testing.T) {
	t.Setenv("DEBUG_PAGE_SECRET", "s3cr3t")
	server := dangerousServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}, true)
	yaml := strings.Replace(roomsYAML("rooms"), "Temperature of each room", "${DEBUG_PAGE_SECRET}", 1)
	answer := postRun(t, server, pasted(yaml, debugExampleBody), http.StatusOK)
	if strings.Contains(answer.Series+answer.Report, "s3cr3t") || !strings.Contains(answer.Series, "# HELP room_temperature_celsius ${DEBUG_PAGE_SECRET}") {
		t.Errorf("the reference was expanded:\n%s", answer.Series)
	}
}

// The debug page says what the playground may do in each mode: without the
// flag, that it never runs Python and refuses the server's files; with it,
// a warning that it runs Python and reads the server's files, the playground
// offered Probe a target through its own request. Every page's bar carries
// the badge with the flag and not without it.
func TestTheDebugPageSaysWhatThePlaygroundMayDoInEachMode(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}
	const quiet = "The playground never runs Python, and one that names a file of the server's is refused; a configured collector runs as it is configured."
	const warning = `<p class="danger" id="danger" role="alert"><strong>Dangerous debug mode</strong> (<code>--web.debug.dangerous</code>): the playground runs Python and reads the server's files with the exporter's permissions, and probing a target with it makes its own request, to any target it names.`
	const badge = `<span class="danger-badge" title="--web.debug.dangerous: the debug page's playground runs Python and reads the server's files">Dangerous debug mode</span>`
	off := dangerousServer(t, cfg, false)
	page := getPage(t, off, "/debug")
	requireContains(t, page, quiet, `id="mode-fetch" disabled>`, "The playground makes no request of its own")
	if strings.Contains(page, "Dangerous debug mode") || strings.Contains(page, "<main data-dangerous") {
		t.Error("the page warns of dangerous mode without the flag")
	}
	on := dangerousServer(t, cfg, true)
	page = getPage(t, on, "/debug")
	requireContains(t, page, warning, "<main data-dangerous>", `<input type="radio" name="mode" value="fetch" id="mode-fetch"> Probe a target`,
		`<p class="note" id="fetched-through">Probed through the playground's own request.</p>`,
		`to scrape a static target.</p>`,
		`radio.disabled = through === "" && !dangerous;`,
		`const throughForm = () => fetcher() === "" ? playgroundForm() : formOf(fetcher());`)
	if strings.Contains(page, quiet) || strings.Contains(page, "The playground makes no request of its own") {
		t.Error("the page still says the playground runs no Python and makes no request")
	}
	for _, path := range []string{"/", "/collectors", "/debug", on.staticTargetsStatusEndpoint()} {
		if !strings.Contains(getPage(t, on, path), badge) {
			t.Errorf("%s has no dangerous debug mode badge in its bar", path)
		}
		if strings.Contains(getPage(t, off, path), `class="danger-badge"`) {
			t.Errorf("%s has the badge without the flag", path)
		}
	}
	// Below full the flag does nothing, though main refuses it there.
	edit := dangerousServer(t, cfg, true)
	edit.SetDebug(DebugEdit)
	if page := getPage(t, edit, "/debug"); strings.Contains(page, "Dangerous debug mode") {
		t.Error("the page warns at edit")
	}
}
