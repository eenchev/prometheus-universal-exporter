package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Every byte-count setting takes a unit, in the configuration and the schema.
func TestByteSizeSettings(t *testing.T) {
	root := t.TempDir()
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", `collectors:
  - name: web
    request:
      type: http
      max_response_bytes: 1MiB
    transform:
      type: regex
    metrics:
      - name: v
        type: gauge
        expression: 'v=(\d+)'
    limits:
      max_response_bytes: 2MB
      max_output_bytes: 64KiB
      max_script_memory: 256MiB
  - name: files
    request:
      type: localfile
      root: `+root+`
      files: ["*.prom"]
      max_total_bytes: 1.5 MiB
    transform:
      type: prometheus
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	web, files := cfg.Collectors[0], cfg.Collectors[1]
	if web.Request.MaxResponseBytes != 1<<20 || web.Limits.MaxResponseBytes != 2_000_000 || web.Limits.MaxOutputBytes != 64<<10 || files.Request.MaxTotalBytes != 3<<19 {
		t.Fatalf("sizes: %d %d %d %d", web.Request.MaxResponseBytes, web.Limits.MaxResponseBytes, web.Limits.MaxOutputBytes, files.Request.MaxTotalBytes)
	}
	if web.Limits.MaxScriptMemory != 256<<20 {
		t.Fatalf("max_script_memory %d", web.Limits.MaxScriptMemory)
	}
	small := testutil.WriteIn(t, t.TempDir(), "config.yaml", "collectors:\n  - name: py\n    request:\n      type: http\n    transform:\n      type: python\n      script: metric(name='v', value=1)\n    limits:\n      max_script_memory: 1MiB\n")
	if _, err := Load(small); err == nil || !strings.Contains(err.Error(), "limits.max_script_memory must be 0, for no limit, or at least 32MiB") {
		t.Fatalf("a memory limit too small for the interpreter: %v", err)
	}
	bad := testutil.WriteIn(t, t.TempDir(), "config.yaml", "collectors:\n  - name: web\n    request:\n      type: http\n      max_response_bytes: lots\n")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), `size "lots"`) {
		t.Fatalf("a malformed size: %v", err)
	}
	schema, err := SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(model.ByteSizePattern)
	if !strings.Contains(string(schema), string(quoted)) {
		t.Fatal("the schema does not describe sizes with units")
	}
}

// request.accept_status takes numbers and classes, as YAML numbers or
// strings.
func TestAcceptStatusFromYAML(t *testing.T) {
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", `collectors:
  - name: web
    request:
      type: http
      accept_status: [200, "5xx", 404]
    transform:
      type: jq
    metrics:
      - name: status
        type: gauge
        expression: $status
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Collectors[0].Request.AcceptStatus, ","); got != "200,5xx,404" {
		t.Fatalf("accept_status %q", got)
	}
}

// A top-level x- key is the file's own: it holds YAML anchors the rest of the
// file reuses, in the configuration, a collector file and the static target
// file alike. Anywhere else, and bare x-, it is an unknown key.
func TestExtensionKeysHoldAnchors(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "more.yaml", `x-shared: &shared
  request:
    type: http
  transform:
    type: regex
collectors:
  - <<: *shared
    name: three
    metrics:
      - name: three_value
        expression: 'v=(\d+)'
`)
	path := testutil.WriteIn(t, dir, "config.yaml", `x-base: &base
  request:
    type: http
    path: /status
    headers:
      Accept: text/plain
  transform:
    type: regex
x-rule: &rule
  type: gauge
  error_mode: fail
collector_files: [more.yaml]
collectors:
  - <<: *base
    name: one
    metrics:
      - <<: *rule
        name: one_value
        expression: 'value=(\d+)'
  - <<: *base
    name: two
    request:
      type: http
      path: /other
    metrics:
      - <<: *rule
        name: two_value
        expression: 'v=(\d+)'
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]model.Collector{}
	for _, c := range cfg.Collectors {
		byName[c.Name] = c
	}
	if one := byName["one"]; one.Request.Path != "/status" || one.Request.Headers["Accept"] != "text/plain" || one.Metrics[0].ErrorMode != "fail" {
		t.Fatalf("one: %+v", one)
	}
	// A key set beside the merge replaces the anchor's whole value.
	if two := byName["two"]; two.Request.Path != "/other" || len(two.Request.Headers) != 0 {
		t.Fatalf("two: %+v", two.Request)
	}
	if _, ok := byName["three"]; !ok {
		t.Fatal("the collector file's anchor was not reused")
	}

	targets := testutil.WriteIn(t, dir, "static.yaml", `x-eu: &eu
  region: eu
interval: 1m
targets:
  - {name: a, collector: one, target: "http://a", labels: *eu}
`)
	file, err := LoadStaticTargets(targets)
	if err != nil || file.Targets[0].Labels["region"] != "eu" {
		t.Fatalf("%v %+v", err, file)
	}

	for name, document := range map[string]string{
		"x- inside a collector": strings.Replace(testutil.CollectorsDocument("c"), "    request:", "    x-note: here\n    request:", 1),
		"a bare x-":             "x-: {}\n" + testutil.CollectorsDocument("c"),
		"another unknown key":   "extras: {}\n" + testutil.CollectorsDocument("c"),
	} {
		if _, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", document)); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Past the x- key the document is still checked.
	bad := "x-base: {}\ncollectors:\n  - name: c\n    requst: {}\n"
	if _, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", bad)); err == nil || !strings.Contains(err.Error(), `unknown key "requst"`) || strings.Contains(err.Error(), "x-base") {
		t.Fatalf("%v", err)
	}
}
