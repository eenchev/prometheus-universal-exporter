//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The debug page (debugpage.go): /debug and /debug/run, at --web.debug edit
// and full.

// roomsCollector is debugExample as a configured collector, named name.
func roomsCollector(name string) model.Collector {
	return model.Collector{
		Name:      name,
		Request:   model.RequestConfig{Type: "http"},
		Decoder:   model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{
			Name: "room_temperature_celsius", Description: "Temperature of each room", Type: model.GaugeMetricType,
			Items: ".rooms[]", Expression: ".celsius",
			Labels: []model.LabelRule{{Name: "room", Expression: ".name"}},
		}},
	}
}

// roomsYAML is debugExample, named name.
func roomsYAML(name string) string {
	return strings.Replace(debugExample, "name: example", "name: "+name, 1)
}

// debugPageServer serves cfg with debugging at edit.
func debugPageServer(t *testing.T, cfg *model.Config) *Server {
	t.Helper()
	server := landingServer(t, cfg)
	server.SetDebug(DebugEdit)
	return server
}

// runRequest is a run as the page sends it.
func runRequest(t *testing.T, run any) *http.Request {
	t.Helper()
	body, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Debug-Run", "run")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return request
}

func serve(server *Server, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// postRun sends run and reads the answer, which must be JSON with code.
func postRun(t *testing.T, server *Server, run any, code int) debugAnswer {
	t.Helper()
	recorder := serve(server, runRequest(t, run))
	return readAnswer(t, recorder, code)
}

func readAnswer(t *testing.T, recorder *httptest.ResponseRecorder, code int) debugAnswer {
	t.Helper()
	if recorder.Code != code {
		t.Fatalf("answered %d, want %d: %s", recorder.Code, code, recorder.Body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	var answer debugAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("%v: %s", err, recorder.Body)
	}
	return answer
}

func pasted(collector, body string) map[string]any {
	return map[string]any{"collector": collector, "body": body, "content_type": "application/json"}
}

// pastedTo is pasted for a configured collector, run by its name.
func pastedTo(configured, body string) map[string]any {
	return map[string]any{"configured": configured, "body": body, "content_type": "application/json"}
}

// A pasted response is decoded, transformed and validated as a probe's
// would be, and the series are what /probe answers for the same collector
// and response, to the byte.
func TestTheDebugPageAnswersWhatAProbeWouldOnAPastedResponse(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	probe := getPath(server, http.MethodGet, "/probe?collector=rooms&target="+target.URL)
	if probe.Code != http.StatusOK {
		t.Fatalf("the probe answered %d: %s", probe.Code, probe.Body)
	}
	answer := postRun(t, server, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusOK)
	if !answer.OK || answer.Error != "" {
		t.Fatalf("the run failed: %+v", answer)
	}
	if answer.Series != probe.Body.String() {
		t.Errorf("the run's series differ from the probe's answer:\nrun:   %q\nprobe: %q", answer.Series, probe.Body.String())
	}
	if !strings.Contains(answer.Series, `room_temperature_celsius{room="kitchen"} 21.5`) {
		t.Errorf("series: %s", answer.Series)
	}
	if answer.Verdict != "200 with 2 series" {
		t.Errorf("verdict %q", answer.Verdict)
	}
	stages := map[string]string{}
	for _, stage := range answer.Stages {
		stages[stage.Stage] = stage.Outcome + " " + stage.Note
	}
	for stage, want := range map[string]string{"http": "ok pasted, status 200", "decode": "ok json", "transform": "ok 2 series", "validation": "ok"} {
		if !strings.HasPrefix(stages[stage], want) {
			t.Errorf("stage %s is %q, want it to start %q", stage, stages[stage], want)
		}
	}
	for _, want := range []string{"Debug run of edited collector \"rooms\", on a pasted response\n", "A debug run puts nothing in force", "Requests\n  none sent\n", `"kitchen"`} {
		if !strings.Contains(answer.Report, want) {
			t.Errorf("the report lacks %q:\n%s", want, answer.Report)
		}
	}
	// The JSON report has what the text one has: no request, and the
	// response as it was pasted.
	if len(answer.Requests) != 0 || answer.Response == nil || answer.Response.Status != "200 OK" ||
		answer.Response.Body != debugExampleBody || answer.Response.BodyBytes != len(debugExampleBody) ||
		!slices.Contains(answer.Response.Headers, "Content-Type: application/json") {
		t.Errorf("requests %+v, response %+v", answer.Requests, answer.Response)
	}
}

// A pasted response is held to what a fetched one is: its status to
// accept_status, its body to the collector's response limit.
func TestTheDebugPageHoldsAPastedResponseToTheCollectorsRules(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	run := pasted(roomsYAML("rooms"), debugExampleBody)
	run["status"] = 503
	answer := postRun(t, server, run, http.StatusOK)
	if answer.OK || !strings.HasPrefix(answer.Verdict, "502: collector rooms http_status failed: received HTTP status 503") || answer.Series != "" {
		t.Errorf("a 503 the collector does not accept: %+v", answer)
	}
	limited := pasted(roomsYAML("rooms")+"limits:\n  max_response_bytes: 1KiB\n", debugExampleBody+strings.Repeat(" ", 1024))
	answer = postRun(t, server, limited, http.StatusOK)
	if answer.OK || !strings.Contains(answer.Verdict, "exceeds limit 1024") {
		t.Errorf("a body over limits.max_response_bytes: %+v", answer)
	}
}

// The collector is read as the loader reads one, its mistakes said in the
// loader's words and at their lines.
func TestTheDebugPageRefusesAnInvalidCollectorInTheLoadersWords(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for yaml, want := range map[string]string{
		"name: x\nrequst:\n  type: http\n":   `line 2: unknown key "requst" in a collector`,
		roomsYAML("x") + "coalesce: maybe\n": "line 17:",
		"name: x\nrequest:\n  type: http\ntransform:\n  type: jq\nmetrics:\n  - name: up\n    type: gauge\n    expression: '.['\n": `collector "x"`,
		"- name: x\n":                "without the dash",
		"collectors:\n  - name: x\n": `"collectors" is a key of the configuration`,
		"":                           "the collector is empty",
		"name: x\n---\nname: y\n":    "a second document starts here",
	} {
		answer := postRun(t, server, pasted(yaml, "{}"), http.StatusUnprocessableEntity)
		if !strings.Contains(answer.Error, want) {
			t.Errorf("%q answered %q, want it to say %q", yaml, answer.Error, want)
		}
		if answer.Series != "" || answer.Report != "" {
			t.Errorf("%q ran: %+v", yaml, answer)
		}
	}
}

// ${NAME} is not expanded: the answer would show the exporter's environment.
func TestTheDebugPageLeavesEnvironmentReferencesAsWritten(t *testing.T) {
	t.Setenv("DEBUG_PAGE_SECRET", "s3cr3t")
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	yaml := strings.Replace(roomsYAML("rooms"), "Temperature of each room", "${DEBUG_PAGE_SECRET}", 1)
	answer := postRun(t, server, pasted(yaml, debugExampleBody), http.StatusOK)
	if strings.Contains(answer.Series+answer.Report, "s3cr3t") || !strings.Contains(answer.Series, "# HELP room_temperature_celsius ${DEBUG_PAGE_SECRET}") {
		t.Errorf("the reference was expanded:\n%s", answer.Series)
	}
}

// A collector written on the page never runs Python, at full as at edit, a
// request type other than http is refused, and so is an http collector
// naming a file of the server's, a login's among them: nothing the file
// holds reaches the answer. The keys naming files are listed in the order
// the collector has them.
func TestTheDebugPageRefusesPythonAndServerFilesInAWrittenCollector(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("file-secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	// The interpreter would fail any script; the refusal comes first.
	server.pythonPath = "/nonexistent/python"
	server.SetDebug(DebugFull)
	for name, c := range map[string]struct{ yaml, want string }{
		"python transform":          {"name: x\nrequest:\n  type: http\ntransform:\n  type: python\n  script: |\n    metric('up', 1)\nmetrics:\n  - name: up\n", "never runs Python"},
		"pre_script":                {strings.Replace(roomsYAML("x"), "  type: jq\n", "  type: jq\n  pre_script: |\n    data['x'] = 1\n", 1), "never runs Python"},
		"localfile":                 {"name: x\nrequest:\n  type: localfile\n  root: /etc\ntransform:\n  type: regex\nmetrics:\n  - name: up\n    type: gauge\n    expression: 'root:(x)'\n", "request.type localfile; a collector written on the debug page is an http collector"},
		"bearer_token_file":         {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  bearer_token_file: "+secret+"\n", 1), "request.bearer_token_file, which names a file on the server"},
		"basic_auth_file":           {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  basic_auth_file:\n    username: "+secret+"\n    password: "+secret+"\n", 1), "request.basic_auth_file"},
		"tls files":                 {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  tls:\n    ca_file: "+secret+"\n    key_file: "+secret+"\n", 1), "request.tls.ca_file, request.tls.key_file"},
		"files in the file's order": {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  tls:\n    key_file: "+secret+"\n    ca_file: "+secret+"\n  bearer_token_file: "+secret+"\n", 1), "request.tls.key_file, request.tls.ca_file, request.bearer_token_file, which"},
		"login":                     {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  login:\n    type: json\n    url: /login\n    token: .token\n    field_files: {password: "+secret+"}\n", 1), "request.login, which names a file on the server"},
	} {
		answer := postRun(t, server, pasted(c.yaml, debugExampleBody), http.StatusUnprocessableEntity)
		if !strings.Contains(answer.Error, c.want) {
			t.Errorf("%s answered %q, want %q", name, answer.Error, c.want)
		}
		if strings.Contains(answer.Error+answer.Report+answer.Series, "file-secret-value") {
			t.Errorf("%s shows what the file holds: %+v", name, answer)
		}
	}
}

// A run that is too large is refused before anything is read of it.
func TestTheDebugPageBoundsWhatARunSends(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for name, run := range map[string]map[string]any{
		"body":      pasted(roomsYAML("rooms"), strings.Repeat(" ", debugMaxBodyBytes+1)),
		"collector": pasted(roomsYAML("rooms")+"#"+strings.Repeat("x", debugMaxCollectorBytes), "{}"),
		"request":   pasted(roomsYAML("rooms"), strings.Repeat(`"`, debugMaxBodyBytes)),
	} {
		answer := postRun(t, server, run, http.StatusRequestEntityTooLarge)
		if !strings.Contains(answer.Error, "at most") {
			t.Errorf("an oversized %s: %q", name, answer.Error)
		}
	}
	for name, c := range map[string]struct {
		run  map[string]any
		want string
	}{
		"both":              {map[string]any{"collector": "name: x", "body": "", "request_from": "rooms"}, "not several"},
		"static and body":   {map[string]any{"collector": "name: x", "body": "", "static_target": "eu"}, "not several"},
		"neither":           {map[string]any{"collector": "name: x"}, "a run needs a response"},
		"two collectors":    {map[string]any{"collector": "name: x", "configured": "rooms", "body": ""}, "not both"},
		"no collector":      {map[string]any{"body": ""}, "a run needs a collector"},
		"status":            {map[string]any{"collector": "name: x", "body": "", "status": 42}, "no HTTP status"},
		"header":            {map[string]any{"collector": "name: x", "body": "", "headers": map[string]string{"Bad Name": "v"}}, "is not one HTTP allows"},
		"unknown key":       {map[string]any{"collector": "name: x", "body": "", "bodyy": ""}, "not the JSON the debug page takes"},
		"target param":      {map[string]any{"collector": "name: x", "body": "", "params": map[string]string{"target": "x"}}, "the debug page sets itself"},
		"static params":     {map[string]any{"configured": "rooms", "static_target": "eu", "params": map[string]string{"param_x": "1"}}, "not taken with static_target"},
		"unknown collector": {map[string]any{"configured": "nobody", "body": ""}, `unknown collector "nobody" in configured`},
	} {
		answer := postRun(t, server, c.run, http.StatusBadRequest)
		if !strings.Contains(answer.Error, c.want) {
			t.Errorf("%s answered %q, want %q", name, answer.Error, c.want)
		}
	}
}

// With --web.debug off neither path is served, and no page links them; at
// edit and at full both are, and both pages link the debug page.
func TestTheDebugPageIsOffUnlessEnabled(t *testing.T) {
	server := landingServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for _, path := range []string{"/debug", "/debug/run"} {
		if code := getPath(server, http.MethodGet, path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s answered %d with debugging off, want 404", path, code)
		}
	}
	if code := serve(server, runRequest(t, pasted(roomsYAML("rooms"), "{}"))).Code; code != http.StatusNotFound {
		t.Errorf("a run answered %d with debugging off, want 404", code)
	}
	for _, path := range []string{"/", "/collectors"} {
		if page := getPage(t, server, path); strings.Contains(page, `href="debug`) {
			t.Errorf("%s links the debug page with debugging off", path)
		}
	}
	for _, level := range []DebugLevel{DebugEdit, DebugFull} {
		server.SetDebug(level)
		for _, path := range []string{"/", "/collectors"} {
			requireContains(t, getPage(t, server, path), `href="debug"`)
		}
		if answer := postRun(t, server, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusOK); !answer.OK {
			t.Errorf("at %s, a run: %+v", level, answer)
		}
	}
}

// The debug page is behind the exporter's Basic Auth like every page.
func TestTheDebugPageIsProtectedByBasicAuth(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}}}
	server := debugPageServer(t, cfg)
	if code := getPath(server, http.MethodGet, "/debug").Code; code != http.StatusUnauthorized {
		t.Errorf("the page answered %d without credentials, want 401", code)
	}
	if code := serve(server, runRequest(t, pasted(roomsYAML("rooms"), debugExampleBody))).Code; code != http.StatusUnauthorized {
		t.Errorf("a run answered %d without credentials, want 401", code)
	}
	request := runRequest(t, pasted(roomsYAML("rooms"), debugExampleBody))
	request.SetBasicAuth("u", "p")
	if answer := readAnswer(t, serve(server, request), http.StatusOK); !answer.OK {
		t.Errorf("an authenticated run: %+v", answer)
	}
}

// A run another site's page could send — without the X-Debug-Run header,
// with a body that is not JSON, or one the browser says comes from another
// site — is refused before the collector is read.
func TestTheDebugPageRefusesRunsAnotherSiteCouldSend(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for name, c := range map[string]struct {
		change func(*http.Request)
		code   int
	}{
		"no header":  {func(r *http.Request) { r.Header.Del("X-Debug-Run") }, http.StatusForbidden},
		"cross-site": {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		"same-site":  {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, http.StatusForbidden},
		"form":       {func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType},
		"text":       {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		"get":        {func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
	} {
		request := runRequest(t, pasted(roomsYAML("rooms"), debugExampleBody))
		c.change(request)
		recorder := serve(server, request)
		if recorder.Code != c.code {
			t.Errorf("%s answered %d, want %d", name, recorder.Code, c.code)
		}
	}
	// A browser that says nothing of where a request comes from, curl
	// included, is let through with the header.
	request := runRequest(t, pasted(roomsYAML("rooms"), debugExampleBody))
	request.Header.Del("Sec-Fetch-Site")
	if answer := readAnswer(t, serve(server, request), http.StatusOK); !answer.OK {
		t.Errorf("a run without Sec-Fetch-Site: %+v", answer)
	}
}

// Fetching a target is refused at edit, before the target is contacted, and
// at full goes through a configured collector exactly as a probe does: its
// target policy, its credentials, and nothing of the browser's request. A
// written collector's own request block makes no request, so it names the
// collector to fetch through; a configured one fetches through itself.
func TestTheDebugPageFetchesATargetThroughAConfiguredCollectorAtFullOnly(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	open := roomsCollector("open")
	open.Request.BearerToken = "configured-token"
	open.Request.ForwardHeaders = []string{"X-Tenant"}
	guarded := roomsCollector("guarded")
	guarded.Request.AllowedTargets = []string{"example.invalid"}
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{open, guarded}})
	fetchRun := func(through string) map[string]any {
		// The written collector's own request would send another token.
		yaml := strings.Replace(roomsYAML("mine"), "  type: http\n", "  type: http\n  bearer_token: page-token\n", 1)
		return map[string]any{"collector": yaml, "request_from": through, "target": target.URL, "params": map[string]string{"header_X-Tenant": "a"}}
	}
	answer := postRun(t, server, fetchRun("open"), http.StatusForbidden)
	if !strings.Contains(answer.Error, "--web.debug=full") || got != nil {
		t.Errorf("a fetch at edit: %q, the target got %v", answer.Error, got)
	}
	if answer := postRun(t, server, map[string]any{"configured": "open", "target": target.URL}, http.StatusForbidden); got != nil || answer.Error == "" {
		t.Errorf("a configured collector's fetch at edit: %+v, the target got %v", answer, got)
	}
	server.SetDebug(DebugFull)
	request := runRequest(t, fetchRun("open"))
	request.Header.Set("Cookie", "session=browser")
	answer = readAnswer(t, serve(server, request), http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, `room_temperature_celsius{room="attic"} 17`) {
		t.Fatalf("a fetch through open: %+v", answer)
	}
	if auth := got.Get("Authorization"); auth != "Bearer configured-token" {
		t.Errorf("the target was sent Authorization %q, want the configured collector's", auth)
	}
	if got.Get("Cookie") != "" || got.Get("X-Debug-Run") != "" || got.Get("X-Tenant") != "a" {
		t.Errorf("the target was sent %v", got)
	}
	if !strings.Contains(answer.Report, `fetched through collector "open"`) || !strings.Contains(answer.Report, "1. GET "+target.URL) {
		t.Errorf("the report:\n%s", answer.Report)
	}
	if len(answer.Requests) != 1 || answer.Requests[0].URL != target.URL || !slices.Contains(answer.Requests[0].Headers, "Authorization: <redacted>") ||
		answer.Response == nil || !strings.Contains(answer.Response.Body, "kitchen") {
		t.Errorf("requests %+v, response %+v", answer.Requests, answer.Response)
	}
	got = nil
	answer = postRun(t, server, fetchRun("guarded"), http.StatusOK)
	if answer.OK || !strings.HasPrefix(answer.Verdict, "403: ") || got != nil {
		t.Errorf("guarded's allowed_targets did not refuse the target: %+v", answer)
	}
	if answer := postRun(t, server, fetchRun("nobody"), http.StatusBadRequest); !strings.Contains(answer.Error, `unknown collector "nobody"`) {
		t.Errorf("request_from nobody: %q", answer.Error)
	}
	missing := fetchRun("open")
	missing["target"] = ""
	if answer := postRun(t, server, missing, http.StatusBadRequest); !strings.Contains(answer.Error, "a target is required") {
		t.Errorf("no target: %q", answer.Error)
	}
	own := fetchRun("open")
	delete(own, "request_from")
	if answer := postRun(t, server, own, http.StatusBadRequest); !strings.Contains(answer.Error, "makes no request of its own") {
		t.Errorf("a written collector without request_from: %q", answer.Error)
	}
	// A configured collector fetches through itself, as a debug probe of it.
	got = nil
	answer = postRun(t, server, map[string]any{"configured": "open", "target": target.URL}, http.StatusOK)
	if !answer.OK || got.Get("Authorization") != "Bearer configured-token" || !strings.HasPrefix(answer.Report, `Debug run of collector "open", on target `+target.URL+`, fetched through collector "open"`) {
		t.Errorf("open through itself: %+v", answer)
	}
}

// A configured collector is run by its name on a pasted body at edit: as it
// is configured, its request block making no request, and answering what a
// probe of it would answer for the same response.
func TestTheDebugPageRunsAConfiguredCollectorOnAPastedBody(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	rooms := roomsCollector("rooms")
	rooms.Request.BearerToken = "configured-token"
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{rooms}})
	answer := postRun(t, server, pastedTo("rooms", debugExampleBody), http.StatusOK)
	if !answer.OK || hits != 0 {
		t.Fatalf("the run: %+v, %d requests", answer, hits)
	}
	if !strings.HasPrefix(answer.Report, "Debug run of collector \"rooms\", on a pasted response\n") || !strings.Contains(answer.Report, "Requests\n  none sent\n") {
		t.Errorf("the report:\n%s", answer.Report)
	}
	server.SetDebug(DebugFull)
	probe := getPath(server, http.MethodGet, "/probe?collector=rooms&target="+target.URL)
	if answer.Series != probe.Body.String() {
		t.Errorf("the run's series differ from the probe's answer:\nrun:   %q\nprobe: %q", answer.Series, probe.Body.String())
	}
}

// A configured collector's Python runs on a pasted body, at edit too: it is
// the operator's script, which reads whatever a target sends on every probe.
// It runs in the collector's workers and counts in its worker series.
func TestTheDebugPageRunsAConfiguredCollectorsPython(t *testing.T) {
	requirePython(t)
	scripted := pythonCollector("scripted", "metric(name=\"length\", value=len(str(data)))\n")
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{scripted}})
	answer := postRun(t, server, pastedTo("scripted", "four"), http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, "length 4") {
		t.Errorf("the run: %+v", answer)
	}
	// The script ran in the collector's own workers, and counts in its
	// worker series as a debug probe's does.
	if runs := transform.PythonWorkers().Snapshot("scripted").Runs["ok"]; runs != 1 {
		t.Errorf("the collector's workers counted %d runs, want 1", runs)
	}
}

// At full a static target is scraped once as its schedule would scrape it,
// with its own credential, and read by its collector or by one written on
// the page; at edit it is refused before the target is contacted. Nothing is
// published.
func TestTheDebugPageScrapesAStaticTargetAtFull(t *testing.T) {
	var authorization atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "rooms", Target: target.URL, Labels: map[string]string{"region": "eu"},
			Request: model.TargetRequestConfig{BearerToken: "s3cret-target-token"}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugEdit)
	answer := postRun(t, server, map[string]any{"configured": "rooms", "static_target": "eu"}, http.StatusForbidden)
	if !strings.Contains(answer.Error, "--web.debug=full") || authorization.Load() != nil {
		t.Fatalf("at edit: %+v", answer)
	}
	server.SetDebug(DebugFull)
	answer = postRun(t, server, map[string]any{"configured": "rooms", "static_target": "eu"}, http.StatusOK)
	if !answer.OK || authorization.Load() != "Bearer s3cret-target-token" {
		t.Fatalf("the scrape: %+v, the target got %v", answer, authorization.Load())
	}
	if answer.Verdict != "target up 1 with 2 series" || !strings.Contains(answer.Series, `room_temperature_celsius{region="eu",room="kitchen",static_target="eu"} 21.5`) {
		t.Errorf("verdict %q, series:\n%s", answer.Verdict, answer.Series)
	}
	if strings.Contains(answer.Report, "s3cret") || !strings.HasPrefix(answer.Report, `Debug run of collector "rooms", on static target "eu", scraped through collector "rooms"`) {
		t.Errorf("the report:\n%s", answer.Report)
	}
	written := strings.Replace(roomsYAML("mine"), "expression: .celsius", "expression: .celsius * 2", 1)
	answer = postRun(t, server, map[string]any{"collector": written, "static_target": "eu"}, http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, `room_temperature_celsius{region="eu",room="kitchen",static_target="eu"} 43`) {
		t.Errorf("a written collector on the scrape: %+v", answer)
	}
	if answer := postRun(t, server, map[string]any{"configured": "rooms", "static_target": "nobody"}, http.StatusNotFound); !strings.Contains(answer.Error, `no static target is named "nobody"`) {
		t.Errorf("an unknown target: %+v", answer)
	}
	if published := getStaticTargets(t, server, "/static-targets"); published != "" {
		t.Fatalf("a debug run published:\n%s", published)
	}
}

// Every run of the debug engine counts in http_exporter_debug_runs_total
// alone, by kind and result, and in no collector's statistics. The family
// is absent with debugging off, has the page alone at edit, and every kind
// at full.
func TestDebugRunsCountInTheirOwnSelfMetricAlone(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "eu", Collector: "rooms", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	if metrics := getPath(server, http.MethodGet, "/self-metrics").Body.String(); strings.Contains(metrics, "debug_runs") {
		t.Errorf("the self-metrics have the debug family with debugging off")
	}
	server.SetDebug(DebugEdit)
	postRun(t, server, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusOK)
	postRun(t, server, pastedTo("rooms", debugExampleBody), http.StatusOK)
	postRun(t, server, pasted(roomsYAML("rooms"), "not json"), http.StatusOK)
	postRun(t, server, pasted("name: rooms\n", "{}"), http.StatusUnprocessableEntity)
	postRun(t, server, map[string]any{"collector": "x"}, http.StatusBadRequest)
	postRun(t, server, map[string]any{"configured": "rooms", "target": target.URL}, http.StatusForbidden)
	metrics := getPath(server, http.MethodGet, "/self-metrics").Body.String()
	if strings.Contains(metrics, `kind="probe"`) || strings.Contains(metrics, `kind="static"`) {
		t.Errorf("at edit the family has the kinds edit does not serve:\n%s", metrics)
	}
	for series, want := range map[string]float64{
		`http_exporter_debug_runs_total{kind="page",result="success"}`: 2,
		`http_exporter_debug_runs_total{kind="page",result="failure"}`: 1,
		`http_exporter_debug_runs_total{kind="page",result="invalid"}`: 3,
	} {
		if got := seriesValue(t, metrics, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	server.SetDebug(DebugFull)
	probe := "/probe?collector=rooms&debug=true&target=" + target.URL
	getPath(server, http.MethodGet, probe)
	getPath(server, http.MethodGet, probe+"&param_nothing=1")
	getPath(server, http.MethodGet, "/probe?collector=nobody&debug=true&target="+target.URL)
	getPath(server, http.MethodGet, "/static-targets?debug=eu")
	getPath(server, http.MethodGet, "/static-targets?debug=nobody")
	postRun(t, server, map[string]any{"configured": "rooms", "target": target.URL}, http.StatusOK)
	metrics = getPath(server, http.MethodGet, "/self-metrics").Body.String()
	for series, want := range map[string]float64{
		`http_exporter_debug_runs_total{kind="probe",result="success"}`:  1,
		`http_exporter_debug_runs_total{kind="probe",result="failure"}`:  0,
		`http_exporter_debug_runs_total{kind="probe",result="invalid"}`:  2,
		`http_exporter_debug_runs_total{kind="static",result="success"}`: 1,
		`http_exporter_debug_runs_total{kind="static",result="invalid"}`: 1,
		`http_exporter_debug_runs_total{kind="page",result="success"}`:   3,
		`http_exporter_scrapes_total{collector="rooms"}`:                 0,
		`http_exporter_metrics_emitted_total{collector="rooms"}`:         0,
		`http_exporter_parse_errors_total{collector="rooms"}`:            0,
		`http_exporter_probes_seconds_total`:                             0,
	} {
		if got := seriesValue(t, metrics, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

// The page's runs in progress are bounded; one past the bound is answered
// 503, and counted a failure: it was asked for rightly, and not run.
func TestTheDebugPageBoundsTheRunsInProgress(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for range debugMaxConcurrent {
		server.debugSlots <- struct{}{}
	}
	answer := postRun(t, server, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusServiceUnavailable)
	if !strings.Contains(answer.Error, "runs in progress") {
		t.Errorf("a run past the bound: %q", answer.Error)
	}
	if got := seriesValue(t, getPath(server, http.MethodGet, "/self-metrics").Body.String(), `http_exporter_debug_runs_total{kind="page",result="failure"}`); got != 1 {
		t.Errorf("the run refused for the bound counted %v failures", got)
	}
	<-server.debugSlots
	if answer := postRun(t, server, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusOK); !answer.OK {
		t.Errorf("a run with a slot free: %+v", answer)
	}
}

// The page is one document with its style and script inline: it loads
// nothing from anywhere, links only relatively but for the documentation,
// and posts its runs relatively, with the header that tells them apart. It
// offers the configured collectors at every level, and fetching a target
// or a static target's scrape at full alone.
func TestTheDebugPageIsSelfContained(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	// The icon is the logo, as a data: URL (TestEveryPageHasTheLogoAsItsIcon).
	page := strings.Replace(getPage(t, server, "/debug"), pageIcon, "", 1)
	requireContains(t, page, "<title>Debug · Prometheus Universal Exporter</title>", `new URL("debug/run", document.baseURI)`, `"X-Debug-Run": "run"`, "room_temperature_celsius", "never runs Python", `<option value="rooms">rooms (http)</option>`, `asked.get("collector")`)
	for _, pattern := range []string{`<link`, `src=`, `@import`, `url\(`, `(?:href|action)="/`, `fetch\("/`} {
		if found := regexp.MustCompile(pattern).FindString(page); found != "" {
			t.Errorf("the page has %q", found)
		}
	}
	for _, found := range regexp.MustCompile(`https?://[^"\s<]*`).FindAllString(page, -1) {
		if found != landingDocs && found != "http://host:port" {
			t.Errorf("the page names %s", found)
		}
	}
	if strings.Contains(page, `value="fetch"`) || strings.Contains(page, `value="static"`) {
		t.Error("the page offers fetching at edit")
	}
	server.SetDebug(DebugFull)
	requireContains(t, getPage(t, server, "/debug"), `value="fetch"`, `value="static"`, `id="fetched-through"`)
}

// The debug page's paths are the exporter's own, which the self-metrics and
// static targets paths cannot take.
func TestTheDebugPagesPathsAreReserved(t *testing.T) {
	for _, path := range []string{"/debug", "/debug/run"} {
		if _, err := SelfMetricsPath(path); err == nil {
			t.Errorf("--web.self-metrics-path %s was accepted", path)
		}
	}
}

// The warnings a load would give for the collector come first in the notes,
// before what the run logged: the decoder left to each response here, and
// the rule that carried on without its series.
func TestTheDebugPageNotesTheLoadsWarningsAndTheRunsLogs(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	yaml := strings.Replace(roomsYAML("rooms"), "decoder:\n  type: json\n", "", 1)
	yaml = strings.Replace(yaml, "expression: .celsius", "expression: .fahrenheit\n    error_mode: log", 1)
	answer := postRun(t, server, pasted(yaml, debugExampleBody), http.StatusOK)
	if len(answer.Notes) < 2 || !strings.Contains(answer.Notes[0], "sets no decoder.type") || !strings.Contains(strings.Join(answer.Notes[1:], "\n"), "metric extraction failed") {
		t.Errorf("notes: %q", answer.Notes)
	}
	if !answer.OK || answer.Series != "" {
		t.Errorf("a run whose rule carried on without its series: %+v", answer)
	}
}

// The page takes its collector and target from its URL, which a link on any
// page can set, so it runs nothing when it opens: run is called when Run is
// pressed, and nowhere else: not on opening, nor after an edit.
func TestTheDebugPageNeverRunsOnOpening(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	server.SetDebug(DebugFull)
	page := getPage(t, server, "/debug?collector=rooms&target=https://attacker.example/")
	script := page[strings.Index(page, "<script>"):strings.LastIndex(page, "</script>")]
	requireContains(t, script, `asked.get("target")`, `$("target").value = asked.get("target")`)
	// The script's code alone: its strings and comments taken out.
	code := regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|//[^\n]*`).ReplaceAllString(script, "")
	var uses []string
	for _, use := range regexp.MustCompile(`(?:^|[^\w.$])run\b.{0,6}`).FindAllString(code, -1) {
		uses = append(uses, strings.TrimSpace(use))
	}
	want := []string{"run() {", "run);"}
	if !slices.Equal(uses, want) {
		t.Errorf("the page's script uses run at %q, want only its definition and the click handler: %q", uses, want)
	}
}
