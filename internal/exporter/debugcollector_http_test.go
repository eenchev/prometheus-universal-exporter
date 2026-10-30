//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A configured collector chosen on the debug page is shown as YAML in place
// of the collector written there (debugcollector.go).

// debugCollectorConfig is a configuration file whose collectors hold
// credentials of each kind, take keys from the file's defaults and share a
// transform through an anchor; secrets are what the page may not show of
// it: the credentials, and the paths of the files that hold more.
func debugCollectorConfig(t *testing.T) (cfg *model.Config, secrets []string) {
	t.Helper()
	token := testutil.WriteFile(t, "token", "s3cret-in-the-file\n")
	password := testutil.WriteFile(t, "password", "s3cret-in-the-file\n")
	text := `# the file's own comment
x-shared:
  transform: &jq
    type: jq
defaults:
  limits:
    max_metrics: 50
  request:
    max_response_bytes: 2MiB
collectors:
  - name: rooms
    # the collector's comment
    request:
      type: http
      path: /rooms
      bearer_token: s3cret-bearer
      headers:
        X-Api-Key: s3cret-header
        Accept: application/json
      query:
        api_key: s3cret-query
        view: full
    decoder:
      type: json
    transform: *jq
    limits:
      max_label_value_length: 200
    metrics:
      - name: room_temperature_celsius
        description: Temperature of each room
        type: gauge
        items: .rooms[]
        expression: .celsius
        labels:
          - name: room
            expression: .name
  - name: filed
    request:
      type: http
      bearer_token_file: ` + token + `
      login:
        type: json
        url: https://login.example/session?api_key=s3cret-in-the-url&lang=en
        fields:
          username: operator
          password: s3cret-field
        field_files:
          otp: ` + password + `
        token: .access_token
        header: X-Session
    decoder: {type: json}
    transform: *jq
    metrics:
      - name: up
        type: gauge
        expression: .up
`
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", text))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, []string{"s3cret", token, password}
}

// getCollectorYAML reads a configured collector as the page shows it.
func getCollectorYAML(t *testing.T, server *Server, name string, code int) debugCollectorAnswer {
	t.Helper()
	response := getPath(server, http.MethodGet, "/debug/collector?name="+name)
	if response.Code != code {
		t.Fatalf("/debug/collector?name=%s answered %d, want %d: %s", name, response.Code, code, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
	var answer debugCollectorAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// textareaOf is the collector's editor of a debug page, unescaped.
func textareaOf(t *testing.T, page string) (attributes, text string) {
	t.Helper()
	m := regexp.MustCompile(`(?s)<textarea id="collector"([^>]*)>(.*?)</textarea>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the page has no collector editor")
	}
	return m[1], html.UnescapeString(m[2])
}

// The page opened on a configured collector, as the collectors page links
// it, shows that collector's YAML, read-only, with Edit a copy, and says
// what runs; opened on none, it shows the example to write over, which
// /debug/collector answers no part of. The YAML is the collector as its
// file writes it: its keys in its order, the anchor it uses resolved, its
// comments left out, and the keys it takes from the file's defaults added
// and marked, while one it writes itself stays its own.
func TestTheDebugPageShowsTheConfiguredCollectorChosen(t *testing.T) {
	cfg, _ := debugCollectorConfig(t)
	server := debugPageServer(t, cfg)
	page := getPage(t, server, "/debug?collector=rooms")
	attributes, text := textareaOf(t, page)
	want := `name: rooms
request:
  type: http
  path: /rooms
  bearer_token: <redacted>
  headers:
    X-Api-Key: <redacted>
    Accept: application/json
  query:
    api_key: <redacted>
    view: full
  max_response_bytes: 2MiB # from the file's defaults
decoder:
  type: json
transform:
  type: jq
limits:
  max_label_value_length: 200
  max_metrics: 50 # from the file's defaults
metrics:
  - name: room_temperature_celsius
    description: Temperature of each room
    type: gauge
    items: .rooms[]
    expression: .celsius
    labels:
      - name: room
        expression: .name
`
	if text != want {
		t.Errorf("the editor shows:\n%s\nwant:\n%s", text, want)
	}
	if !strings.Contains(attributes, " readonly") || !strings.Contains(attributes, `data-shown="rooms"`) {
		t.Errorf("the editor of a configured collector is not read-only: %s", attributes)
	}
	requireContains(t, page, `<option value="rooms" selected>rooms (http)</option>`,
		`<button type="button" class="secondary" id="edit-copy">Edit a copy</button>`,
		"The configuration of rooms, which runs as configured (read-only)")
	if answer := getCollectorYAML(t, server, "rooms", http.StatusOK); answer.YAML != want || answer.Name != "rooms" || strings.Join(answer.FromDefaults, ",") != "limits.max_metrics,request.max_response_bytes" {
		t.Errorf("/debug/collector answered %+v", answer)
	}

	page = getPage(t, server, "/debug")
	attributes, text = textareaOf(t, page)
	if text != debugExample || strings.Contains(attributes, "readonly") || !strings.Contains(attributes, `data-example="`+html.EscapeString(debugExample)) {
		t.Errorf("the page opened on no collector shows %q (%s)", text, attributes)
	}
	requireContains(t, page, `id="edit-copy" hidden`, "Playground: the YAML of a collector, which runs", `new URL("debug/collector", document.baseURI)`)
	if answer := getCollectorYAML(t, server, "nothing", http.StatusNotFound); !strings.Contains(answer.Error, `unknown collector "nothing"`) {
		t.Errorf("an unknown collector: %+v", answer)
	}
	// An unknown name in the page's address opens it on the example.
	if _, text = textareaOf(t, getPage(t, server, "/debug?collector=nothing")); text != debugExample {
		t.Errorf("the page opened on an unknown collector shows %q", text)
	}
	server.SetDebug(DebugOff)
	if code := getPath(server, http.MethodGet, "/debug/collector?name=rooms").Code; code != http.StatusNotFound {
		t.Errorf("/debug/collector with debugging off answered %d", code)
	}
}

// What the page shows of a configured collector holds none of its
// credentials, at edit as at full: a value whose key reads as a
// credential's, a header's, a query parameter's and a login field's among
// them, a URL's query parameter named like one, and every path a *_file key names. What holds no
// credential is shown as written: the login's username and its token
// expression.
func TestTheDebugPageWithholdsAConfiguredCollectorsCredentials(t *testing.T) {
	cfg, secrets := debugCollectorConfig(t)
	server := debugPageServer(t, cfg)
	for _, level := range []DebugLevel{DebugEdit, DebugFull} {
		server.SetDebug(level)
		for _, name := range []string{"rooms", "filed"} {
			page := getPage(t, server, "/debug?collector="+name)
			answer := getCollectorYAML(t, server, name, http.StatusOK)
			_, text := textareaOf(t, page)
			for _, secret := range secrets {
				if strings.Contains(page, secret) || strings.Contains(answer.YAML, secret) {
					t.Errorf("--web.debug=%s: %s is shown with %q:\n%s", level, name, secret, answer.YAML)
				}
			}
			if text != answer.YAML {
				t.Errorf("the page and /debug/collector show %s differently", name)
			}
		}
	}
	yaml := getCollectorYAML(t, server, "filed", http.StatusOK).YAML
	requireContains(t, yaml, "bearer_token_file: <redacted>\n", "url: https://login.example/session?api_key=<redacted>&lang=en\n",
		"username: operator\n", "password: <redacted>\n", "otp: <redacted>\n", "token: .access_token\n")
}

// A Python script is shown as written, but for what reads as a credential
// in it; and a collector the exporter holds without a file is shown as it
// holds it, its empty keys left out.
func TestTheDebugPageShowsAScriptWithItsCredentialsMasked(t *testing.T) {
	x := roomsCollector("scripted")
	x.Transform.PreScript = "headers = {\"Authorization\": \"Bearer s3cret-in-a-script\"}\npassword = \"s3cret-literal\"\nresult = data\n"
	shown, err := shownCollectorYAML(&x)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret-in-a-script", "s3cret-literal"} {
		if strings.Contains(shown, secret) {
			t.Errorf("the script is shown with %q:\n%s", secret, shown)
		}
	}
	requireContains(t, shown, "name: scripted\n", "  pre_script: |\n", "result = data\n", `"Authorization": "<redacted>`)
	for _, empty := range []string{"metrics_prefix", "forward_authorization", `""`, "null", "max_concurrent_probes"} {
		if strings.Contains(shown, empty) {
			t.Errorf("the collector held without a file is shown with %s:\n%s", empty, shown)
		}
	}
}

// The YAML shown, pasted back as a collector written on the page with what
// was withheld left out, is read as a collector and runs to the series the
// configured collector gives.
func TestTheShownYAMLOfAConfiguredCollectorRunsWrittenOnThePage(t *testing.T) {
	cfg, _ := debugCollectorConfig(t)
	server := debugPageServer(t, cfg)
	shown := getCollectorYAML(t, server, "rooms", http.StatusOK).YAML
	var kept []string
	for _, line := range strings.Split(shown, "\n") {
		if !strings.Contains(line, "<redacted>") {
			kept = append(kept, line)
		}
	}
	written := postRun(t, server, pasted(strings.Join(kept, "\n"), debugExampleBody), http.StatusOK)
	configured := postRun(t, server, pastedTo("rooms", debugExampleBody), http.StatusOK)
	if !written.OK || written.Series == "" || written.Series != configured.Series {
		t.Errorf("the copy gave %+v\nthe configured collector %q", written, configured.Series)
	}
}

// /debug/collector is the debug page's, which the static targets and
// self-metrics paths cannot take.
func TestTheDebugCollectorPathIsReserved(t *testing.T) {
	if _, err := SelfMetricsPath(debugCollectorPath); err == nil {
		t.Errorf("--web.self-metrics-path %s was accepted", debugCollectorPath)
	}
}
