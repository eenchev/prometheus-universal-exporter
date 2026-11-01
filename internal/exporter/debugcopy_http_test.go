//go:build !select_request_types || request_type_http

package exporter

import (
	"html"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Edit a copy leaves out of the playground what it would refuse: every key
// of the request ending in _file or _files and a login, each line replaced
// by a comment that says what was dropped and why, and a mapping left with
// nothing, tls: with its ca_file alone, with it; the copy loads as a
// playground, and the page says above the editor what it left out. With
// --web.debug.dangerous a login is kept, but the paths, which the page
// withholds, are dropped all the same, the comment saying to write each.
// Python is left in, and the note says the copy will be refused for it.
func TestEditACopyLeavesOutWhatThePlaygroundRefuses(t *testing.T) {
	dir := t.TempDir()
	file := func(name string) string { return testutil.WriteIn(t, dir, name, "x\n") }
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: files
    request:
      type: http
      path: /status
      bearer_token_file: `+file("token")+`
      tls:
        ca_file: `+file("ca.pem")+`
      login:
        type: json
        url: /api/login
        fields:
          tenant: acme
        field_files:
          password: `+file("password")+`
        token: .access_token
        header: X-Session
    transform: {type: jq}
    metrics: [{name: up, expression: .up}]
  - name: scripted
    request: {type: http}
    transform:
      type: jq
      pre_script: |
        def transform(doc):
            return doc
    metrics: [{name: up, expression: .up}]
`))
	if err != nil {
		t.Fatal(err)
	}
	server := debugPageServer(t, cfg)
	server.SetDebug(DebugFull)

	answer := getCollectorYAML(t, server, "files", 200)
	want := `name: files
request:
  type: http
  path: /status
  # bearer_token_file dropped: the playground reads no file of the server's (--web.debug.dangerous lets it)
  # tls.ca_file dropped: the playground reads no file of the server's (--web.debug.dangerous lets it)
  # login dropped: a login reads its secrets from the server's files, which the playground reads none of (--web.debug.dangerous lets it)
transform: {type: jq}
`
	if !strings.HasPrefix(answer.Copy, want) {
		t.Errorf("the copy is\n%s\nwant it to start\n%s", answer.Copy, want)
	}
	if !strings.Contains(answer.CopyNote, "The copy leaves out request.bearer_token_file, request.tls.ca_file, request.login: the playground reads no file of the server's") {
		t.Errorf("the note: %q", answer.CopyNote)
	}
	if !strings.Contains(answer.YAML, "ca_file: <redacted>") {
		t.Errorf("the YAML shown lost what it shows:\n%s", answer.YAML)
	}
	if _, err := config.ParseCollector([]byte(answer.Copy), refusedOnDebugPage); err != nil {
		t.Errorf("the copy does not load as a playground: %v", err)
	}
	// The page opened on it holds the copy and its note for Edit a copy,
	// which puts them in place of what it shows.
	page := getPage(t, server, "/debug?collector=files")
	attributes, _ := textareaOf(t, page)
	if !strings.Contains(attributes, ` data-copy="`+html.EscapeString(answer.Copy)+`"`) || !strings.Contains(attributes, ` data-copy-note="`+html.EscapeString(answer.CopyNote)+`"`) {
		t.Errorf("the editor's attributes hold no copy: %s", attributes)
	}
	script := debugPageScript(page)
	requireContains(t, script,
		`copy: $("collector").dataset.copy, note: $("collector").dataset.copyNote || ""`,
		`copy: answer.copy || answer.yaml, note: answer.copy_note || ""`,
		`if (!shown) return;`,
		`written = shown.copy || shown.yaml;`,
		// Edit a copy waits for the collector's YAML: the editor holds what
		// the page says of it while it is read, or when it could not be.
		`$("edit-copy").disabled = name !== "" && !configuredYAML.has(name);`,
		"if ($(\"configured\").value !== name) return;\n  setEditor(text);\n  $(\"edit-copy\").disabled = !configuredYAML.has(name);",
		`copyNote = shown.note;`,
		`$("copy-note").hidden = name !== "" || copiedFrom === "" || copyNote === "";`)
	requireContains(t, page, `<p class="note" id="copy-note" role="status" hidden></p>`)

	if answer := getCollectorYAML(t, server, "scripted", 200); !strings.Contains(answer.CopyNote, "It runs Python, which the playground never runs") || !strings.Contains(answer.Copy, "pre_script: |") {
		t.Errorf("a copy of a collector with Python: %q\n%s", answer.CopyNote, answer.Copy)
	}

	server.SetDebugDangerous(true)
	answer = getCollectorYAML(t, server, "files", 200)
	for _, line := range []string{
		"  # bearer_token_file dropped: the page does not show the path it names; write it here\n",
		"  # tls.ca_file dropped: the page does not show the path it names; write it here\n",
		"  login:\n    type: json\n    url: /api/login\n",
		"    token: .access_token\n    header: X-Session\n",
		"    # field_files dropped: the page does not show the path it names; write it here\n",
	} {
		if !strings.Contains(answer.Copy, line) {
			t.Errorf("dangerous: the copy has no %q:\n%s", line, answer.Copy)
		}
	}
	if !strings.Contains(answer.CopyNote, "request.login.field_files") || !strings.Contains(answer.CopyNote, "write each in place of its comment") || strings.Contains(answer.Copy, "<redacted>") {
		t.Errorf("dangerous: %q\n%s", answer.CopyNote, answer.Copy)
	}
	if answer := getCollectorYAML(t, server, "scripted", 200); answer.CopyNote != "" {
		t.Errorf("dangerous: a copy with Python is noted %q", answer.CopyNote)
	}
}

// A key dropped from a mapping written in flow style, in a request written
// in flow style too, leaves its comment on a line of its own, the mappings
// that held it then written in block style, and the copy loads as a
// playground.
func TestEditACopyOfAFlowMappingWritesItsComments(t *testing.T) {
	ca := testutil.WriteFile(t, "ca.pem", "x\n")
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: kept
    request:
      type: http
      tls: {ca_file: `+ca+`, server_name: api.example}
    transform: {type: jq}
    metrics: [{name: up, expression: .up}]
  - name: emptied
    request: {type: http, tls: {ca_file: `+ca+`}}
    transform: {type: jq}
    metrics: [{name: up, expression: .up}]
`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"request:\n  type: http\n  tls:\n    # ca_file dropped: the playground reads no file of the server's (--web.debug.dangerous lets it)\n    server_name: api.example\ntransform: {type: jq}\n",
		"request:\n  type: http\n  # tls.ca_file dropped: the playground reads no file of the server's (--web.debug.dangerous lets it)\ntransform: {type: jq}\n",
	} {
		copied, _, err := copiedCollectorYAML(&cfg.Collectors[i], false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(copied, want) || strings.Contains(copied, droppedKey) {
			t.Errorf("the copy is\n%s\nwant it to hold\n%s", copied, want)
		}
		if _, err := config.ParseCollector([]byte(copied), refusedOnDebugPage); err != nil {
			t.Errorf("the copy does not load as a playground: %v\n%s", err, copied)
		}
	}
}
