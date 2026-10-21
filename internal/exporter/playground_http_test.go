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
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The playground (playground.go): /playground and /playground/run.

// roomsCollector is playgroundExample as a configured collector, named name.
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

// roomsYAML is playgroundExample, named name.
func roomsYAML(name string) string {
	return strings.Replace(playgroundExample, "name: playground", "name: "+name, 1)
}

func playgroundServer(t *testing.T, cfg *model.Config) *Server {
	t.Helper()
	server := landingServer(t, cfg)
	server.SetPlayground(true)
	return server
}

// runRequest is a run as the page sends it.
func runRequest(t *testing.T, run any) *http.Request {
	t.Helper()
	body, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/playground/run", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Playground", "run")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return request
}

func serve(server *Server, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// postRun sends run and reads the answer, which must be JSON with code.
func postRun(t *testing.T, server *Server, run any, code int) playgroundAnswer {
	t.Helper()
	recorder := serve(server, runRequest(t, run))
	return readAnswer(t, recorder, code)
}

func readAnswer(t *testing.T, recorder *httptest.ResponseRecorder, code int) playgroundAnswer {
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
	var answer playgroundAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("%v: %s", err, recorder.Body)
	}
	return answer
}

func pasted(collector, body string) map[string]any {
	return map[string]any{"collector": collector, "body": body, "content_type": "application/json"}
}

// A pasted response is decoded, transformed and validated as a probe's
// would be, and the series are what /probe answers for the same collector
// and response, to the byte.
func TestThePlaygroundAnswersWhatAProbeWouldOnAPastedResponse(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(playgroundExampleBody))
	}))
	defer target.Close()
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	probe := getPath(server, http.MethodGet, "/probe?collector=rooms&target="+target.URL)
	if probe.Code != http.StatusOK {
		t.Fatalf("the probe answered %d: %s", probe.Code, probe.Body)
	}
	answer := postRun(t, server, pasted(roomsYAML("rooms"), playgroundExampleBody), http.StatusOK)
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
	for _, want := range []string{"Playground run of collector \"rooms\", on a pasted response\n", "A playground run puts nothing in force", "Requests\n  none sent\n", `"kitchen"`} {
		if !strings.Contains(answer.Report, want) {
			t.Errorf("the report lacks %q:\n%s", want, answer.Report)
		}
	}
}

// A pasted response is held to what a fetched one is: its status to
// accept_status, its body to the collector's response limit.
func TestThePlaygroundHoldsAPastedResponseToTheCollectorsRules(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	run := pasted(roomsYAML("rooms"), playgroundExampleBody)
	run["status"] = 503
	answer := postRun(t, server, run, http.StatusOK)
	if answer.OK || !strings.HasPrefix(answer.Verdict, "502: collector rooms http_status failed: received HTTP status 503") || answer.Series != "" {
		t.Errorf("a 503 the collector does not accept: %+v", answer)
	}
	limited := pasted(roomsYAML("rooms")+"limits:\n  max_response_bytes: 1KiB\n", playgroundExampleBody+strings.Repeat(" ", 1024))
	answer = postRun(t, server, limited, http.StatusOK)
	if answer.OK || !strings.Contains(answer.Verdict, "exceeds limit 1024") {
		t.Errorf("a body over limits.max_response_bytes: %+v", answer)
	}
}

// The collector is read as the loader reads one, its mistakes said in the
// loader's words and at their lines.
func TestThePlaygroundRefusesAnInvalidCollectorInTheLoadersWords(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
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
func TestThePlaygroundLeavesEnvironmentReferencesAsWritten(t *testing.T) {
	t.Setenv("PLAYGROUND_SECRET", "s3cr3t")
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	yaml := strings.Replace(roomsYAML("rooms"), "Temperature of each room", "${PLAYGROUND_SECRET}", 1)
	answer := postRun(t, server, pasted(yaml, playgroundExampleBody), http.StatusOK)
	if strings.Contains(answer.Series+answer.Report, "s3cr3t") || !strings.Contains(answer.Series, "# HELP room_temperature_celsius ${PLAYGROUND_SECRET}") {
		t.Errorf("the reference was expanded:\n%s", answer.Series)
	}
}

// Python is never run, a request type other than http is refused, and so is
// an http collector naming a file of the server's: nothing the file holds
// reaches the answer.
func TestThePlaygroundRefusesPythonAndServerFiles(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("file-secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	// The interpreter would fail any script; the refusal comes first.
	server.pythonPath = "/nonexistent/python"
	for name, c := range map[string]struct{ yaml, want string }{
		"python transform":  {"name: x\nrequest:\n  type: http\ntransform:\n  type: python\n  script: |\n    metric('up', 1)\nmetrics:\n  - name: up\n", "never runs Python"},
		"pre_script":        {strings.Replace(roomsYAML("x"), "  type: jq\n", "  type: jq\n  pre_script: |\n    data['x'] = 1\n", 1), "never runs Python"},
		"localfile":         {"name: x\nrequest:\n  type: localfile\n  root: /etc\ntransform:\n  type: regex\nmetrics:\n  - name: up\n    type: gauge\n    expression: 'root:(x)'\n", "request.type localfile; the playground runs http collectors only"},
		"bearer_token_file": {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  bearer_token_file: "+secret+"\n", 1), "request.bearer_token_file, which names a file on the server"},
		"basic_auth_file":   {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  basic_auth_file:\n    username: "+secret+"\n    password: "+secret+"\n", 1), "request.basic_auth_file"},
		"tls files":         {strings.Replace(roomsYAML("x"), "  type: http\n", "  type: http\n  tls:\n    ca_file: "+secret+"\n    key_file: "+secret+"\n", 1), "request.tls.ca_file, request.tls.key_file"},
	} {
		answer := postRun(t, server, pasted(c.yaml, playgroundExampleBody), http.StatusUnprocessableEntity)
		if !strings.Contains(answer.Error, c.want) {
			t.Errorf("%s answered %q, want %q", name, answer.Error, c.want)
		}
		if strings.Contains(answer.Error+answer.Report+answer.Series, "file-secret-value") {
			t.Errorf("%s shows what the file holds: %+v", name, answer)
		}
	}
}

// A run that is too large is refused before anything is read of it.
func TestThePlaygroundBoundsWhatARunSends(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for name, run := range map[string]map[string]any{
		"body":      pasted(roomsYAML("rooms"), strings.Repeat(" ", playgroundMaxBodyBytes+1)),
		"collector": pasted(roomsYAML("rooms")+"#"+strings.Repeat("x", playgroundMaxCollectorBytes), "{}"),
		"request":   pasted(roomsYAML("rooms"), strings.Repeat(`"`, playgroundMaxBodyBytes)),
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
		"both":         {map[string]any{"collector": "name: x", "body": "", "request_from": "rooms"}, "not both"},
		"neither":      {map[string]any{"collector": "name: x"}, "a run needs a response"},
		"status":       {map[string]any{"collector": "name: x", "body": "", "status": 42}, "no HTTP status"},
		"header":       {map[string]any{"collector": "name: x", "body": "", "headers": map[string]string{"Bad Name": "v"}}, "is not one HTTP allows"},
		"unknown key":  {map[string]any{"collector": "name: x", "body": "", "bodyy": ""}, "not the JSON the playground takes"},
		"target param": {map[string]any{"collector": "name: x", "body": "", "params": map[string]string{"target": "x"}}, "the playground sets itself"},
	} {
		answer := postRun(t, server, c.run, http.StatusBadRequest)
		if !strings.Contains(answer.Error, c.want) {
			t.Errorf("%s answered %q, want %q", name, answer.Error, c.want)
		}
	}
}

// Without --web.enable-playground neither path is served, and no page links
// it; with it, both pages do.
func TestThePlaygroundIsOffUnlessEnabled(t *testing.T) {
	server := landingServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for _, path := range []string{"/playground", "/playground/run"} {
		if code := getPath(server, http.MethodGet, path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s answered %d without the flag, want 404", path, code)
		}
	}
	if code := serve(server, runRequest(t, pasted(roomsYAML("rooms"), "{}"))).Code; code != http.StatusNotFound {
		t.Errorf("a run answered %d without the flag, want 404", code)
	}
	for _, path := range []string{"/", "/collectors"} {
		if page := getPage(t, server, path); strings.Contains(page, "playground") {
			t.Errorf("%s links the playground without the flag", path)
		}
	}
	server.SetPlayground(true)
	for _, path := range []string{"/", "/collectors"} {
		requireContains(t, getPage(t, server, path), `href="playground"`)
	}
}

// The playground is behind the exporter's Basic Auth like every page.
func TestThePlaygroundIsProtectedByBasicAuth(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}}}
	server := playgroundServer(t, cfg)
	if code := getPath(server, http.MethodGet, "/playground").Code; code != http.StatusUnauthorized {
		t.Errorf("the page answered %d without credentials, want 401", code)
	}
	if code := serve(server, runRequest(t, pasted(roomsYAML("rooms"), playgroundExampleBody))).Code; code != http.StatusUnauthorized {
		t.Errorf("a run answered %d without credentials, want 401", code)
	}
	request := runRequest(t, pasted(roomsYAML("rooms"), playgroundExampleBody))
	request.SetBasicAuth("u", "p")
	if answer := readAnswer(t, serve(server, request), http.StatusOK); !answer.OK {
		t.Errorf("an authenticated run: %+v", answer)
	}
}

// A run another site's page could send — without the X-Playground header,
// with a body that is not JSON, or one the browser says comes from another
// site — is refused before the collector is read.
func TestThePlaygroundRefusesRunsAnotherSiteCouldSend(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for name, c := range map[string]struct {
		change func(*http.Request)
		code   int
	}{
		"no header":  {func(r *http.Request) { r.Header.Del("X-Playground") }, http.StatusForbidden},
		"cross-site": {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		"same-site":  {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, http.StatusForbidden},
		"form":       {func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType},
		"text":       {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		"get":        {func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
	} {
		request := runRequest(t, pasted(roomsYAML("rooms"), playgroundExampleBody))
		c.change(request)
		recorder := serve(server, request)
		if recorder.Code != c.code {
			t.Errorf("%s answered %d, want %d", name, recorder.Code, c.code)
		}
	}
	// A browser that says nothing of where a request comes from, curl
	// included, is let through with the header.
	request := runRequest(t, pasted(roomsYAML("rooms"), playgroundExampleBody))
	request.Header.Del("Sec-Fetch-Site")
	if answer := readAnswer(t, serve(server, request), http.StatusOK); !answer.OK {
		t.Errorf("a run without Sec-Fetch-Site: %+v", answer)
	}
}

// Fetching from a target needs --web.enable-probe-debug, and goes through a
// configured collector exactly as a probe does: its target policy, its
// credentials, and nothing of the browser's request; the playground
// collector's own request block makes no request.
func TestThePlaygroundFetchesATargetThroughAConfiguredCollector(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(playgroundExampleBody))
	}))
	defer target.Close()
	open := roomsCollector("open")
	open.Request.BearerToken = "configured-token"
	open.Request.ForwardHeaders = []string{"X-Tenant"}
	guarded := roomsCollector("guarded")
	guarded.Request.AllowedTargets = []string{"example.invalid"}
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{open, guarded}})
	fetchRun := func(through string) map[string]any {
		// The playground collector's own request would send another token.
		yaml := strings.Replace(roomsYAML("mine"), "  type: http\n", "  type: http\n  bearer_token: playground-token\n", 1)
		return map[string]any{"collector": yaml, "request_from": through, "target": target.URL, "params": map[string]string{"header_X-Tenant": "a"}}
	}
	answer := postRun(t, server, fetchRun("open"), http.StatusForbidden)
	if !strings.Contains(answer.Error, "--web.enable-probe-debug") {
		t.Errorf("a fetch without probe debug: %q", answer.Error)
	}
	server.SetProbeDebug(true)
	request := runRequest(t, fetchRun("open"))
	request.Header.Set("Cookie", "session=browser")
	answer = readAnswer(t, serve(server, request), http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, `room_temperature_celsius{room="attic"} 17`) {
		t.Fatalf("a fetch through open: %+v", answer)
	}
	if auth := got.Get("Authorization"); auth != "Bearer configured-token" {
		t.Errorf("the target was sent Authorization %q, want the configured collector's", auth)
	}
	if got.Get("Cookie") != "" || got.Get("X-Playground") != "" || got.Get("X-Tenant") != "a" {
		t.Errorf("the target was sent %v", got)
	}
	if !strings.Contains(answer.Report, `fetched through collector "open"`) || !strings.Contains(answer.Report, "1. GET "+target.URL) {
		t.Errorf("the report:\n%s", answer.Report)
	}
	got = nil
	answer = postRun(t, server, fetchRun("guarded"), http.StatusOK)
	if answer.OK || !strings.HasPrefix(answer.Verdict, "403: ") || got != nil {
		t.Errorf("guarded's allowed_targets did not refuse the target: %+v", answer)
	}
	for through, want := range map[string]string{"nobody": `unknown collector "nobody"`} {
		if answer := postRun(t, server, fetchRun(through), http.StatusBadRequest); !strings.Contains(answer.Error, want) {
			t.Errorf("request_from %s: %q", through, answer.Error)
		}
	}
	missing := fetchRun("open")
	missing["target"] = ""
	if answer := postRun(t, server, missing, http.StatusBadRequest); !strings.Contains(answer.Error, "a target is required") {
		t.Errorf("no target: %q", answer.Error)
	}
}

// Runs count in http_exporter_playground_runs_total alone, by result, and in
// no collector's statistics; without the playground the family is not there.
func TestPlaygroundRunsCountInTheirOwnSelfMetricAlone(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}
	off := landingServer(t, cfg)
	if metrics := getPath(off, http.MethodGet, "/self-metrics").Body.String(); strings.Contains(metrics, "playground") {
		t.Errorf("the self-metrics have the playground's family without the flag")
	}
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	postRun(t, server, pasted(roomsYAML("rooms"), playgroundExampleBody), http.StatusOK)
	postRun(t, server, pasted(roomsYAML("rooms"), playgroundExampleBody), http.StatusOK)
	postRun(t, server, pasted(roomsYAML("rooms"), "not json"), http.StatusOK)
	postRun(t, server, pasted("name: rooms\n", "{}"), http.StatusUnprocessableEntity)
	postRun(t, server, map[string]any{"collector": "x"}, http.StatusBadRequest)
	metrics := getPath(server, http.MethodGet, "/self-metrics").Body.String()
	for series, want := range map[string]float64{
		`http_exporter_playground_runs_total{result="success"}`:  2,
		`http_exporter_playground_runs_total{result="failure"}`:  1,
		`http_exporter_playground_runs_total{result="invalid"}`:  1,
		`http_exporter_scrapes_total{collector="rooms"}`:         0,
		`http_exporter_metrics_emitted_total{collector="rooms"}`: 0,
		`http_exporter_parse_errors_total{collector="rooms"}`:    0,
		`http_exporter_probes_seconds_total`:                     0,
	} {
		if got := seriesValue(t, metrics, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

// Runs in progress are bounded; one past the bound is answered 503.
func TestThePlaygroundBoundsTheRunsInProgress(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for range playgroundMaxConcurrent {
		server.playgroundSlots <- struct{}{}
	}
	answer := postRun(t, server, pasted(roomsYAML("rooms"), playgroundExampleBody), http.StatusServiceUnavailable)
	if !strings.Contains(answer.Error, "runs in progress") {
		t.Errorf("a run past the bound: %q", answer.Error)
	}
	<-server.playgroundSlots
	if answer := postRun(t, server, pasted(roomsYAML("rooms"), playgroundExampleBody), http.StatusOK); !answer.OK {
		t.Errorf("a run with a slot free: %+v", answer)
	}
}

// The page is one document with its style and script inline: it loads
// nothing from anywhere, links only relatively but for the documentation,
// and posts its runs relatively, with the header that tells them apart.
func TestThePlaygroundPageIsSelfContained(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	page := getPage(t, server, "/playground")
	requireContains(t, page, "<title>Playground · Prometheus Universal Exporter</title>", `new URL("playground/run", document.baseURI)`, `"X-Playground": "run"`, "room_temperature_celsius", "never run here")
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
	if strings.Contains(page, `value="fetch"`) {
		t.Error("the page offers fetching without --web.enable-probe-debug")
	}
	server.SetProbeDebug(true)
	requireContains(t, getPage(t, server, "/playground"), `value="fetch"`, `<option value="rooms">rooms</option>`)
}

// The playground's paths are the exporter's own, which the self-metrics and
// static targets paths cannot take.
func TestThePlaygroundsPathsAreReserved(t *testing.T) {
	for _, path := range []string{"/playground", "/playground/run"} {
		if _, err := SelfMetricsPath(path); err == nil {
			t.Errorf("--web.self-metrics-path %s was accepted", path)
		}
	}
}

// The warnings a load would give for the collector come first in the notes,
// before what the run logged: the decoder left to each response here, and
// the rule that carried on without its series.
func TestThePlaygroundNotesTheLoadsWarningsAndTheRunsLogs(t *testing.T) {
	server := playgroundServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	yaml := strings.Replace(roomsYAML("rooms"), "decoder:\n  type: json\n", "", 1)
	yaml = strings.Replace(yaml, "expression: .celsius", "expression: .fahrenheit\n    error_mode: log", 1)
	answer := postRun(t, server, pasted(yaml, playgroundExampleBody), http.StatusOK)
	if len(answer.Notes) < 2 || !strings.Contains(answer.Notes[0], "sets no decoder.type") || !strings.Contains(strings.Join(answer.Notes[1:], "\n"), "metric extraction failed") {
		t.Errorf("notes: %q", answer.Notes)
	}
	if !answer.OK || answer.Series != "" {
		t.Errorf("a run whose rule carried on without its series: %+v", answer)
	}
}
