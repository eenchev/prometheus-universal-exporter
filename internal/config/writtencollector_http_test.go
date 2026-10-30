//go:build !select_request_types || request_type_http

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Each collector keeps the mapping its file writes it as (writtencollector.go).

// A collector keeps its file's text: its ${NAME} references as written,
// though they were expanded to load it; the keys a merge key brings in,
// after the mapping's own as the decoder reads them, and what an alias names
// in its place; none of its comments; and the keys it takes from its own
// file's defaults — the configuration's or a collector file's, never the
// other's — added and marked, a block the collector writes taking the
// block's keys it does not write.
func TestACollectorKeepsTheMappingItsFileWritesItAs(t *testing.T) {
	t.Setenv("WRITTEN_TOKEN", "s3cret-from-the-environment")
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("team.yaml", `defaults:
  cache:
    ttl: 30s
collectors:
  - name: team
    request: {type: http, path: /team}
    transform: {type: jq}
    metrics: [{name: up, type: gauge, expression: .up}]
`)
	path := write("config.yaml", `collector_files: [team.yaml]
x-base: &base
  type: http
  headers:
    Accept: application/json
defaults:
  limits:
    max_metrics: 50
  request:
    max_response_bytes: 2MiB
collectors:
  - name: own
    # a comment of the collector's
    request:
      <<: *base
      bearer_token: ${WRITTEN_TOKEN}  # a comment after a value
    transform: {type: jq}
    limits:
      max_label_value_length: 100
    metrics: [{name: up, type: gauge, expression: .up}]
`)
	c, err := Load(path, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	collector := func(name string) *model.Collector {
		t.Helper()
		for i := range c.Collectors {
			if c.Collectors[i].Name == name {
				return &c.Collectors[i]
			}
		}
		t.Fatalf("no collector %s", name)
		return nil
	}
	if got := collector("own").Request.BearerToken; got != "s3cret-from-the-environment" {
		t.Fatalf("the token was not expanded: %q", got)
	}
	encode := func(name string) string {
		t.Helper()
		n := collector(name).Written
		if n == nil {
			t.Fatalf("%s keeps no written mapping", name)
		}
		out, err := yaml.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	want := `name: own
request:
    bearer_token: ${WRITTEN_TOKEN}
    type: http
    headers:
        Accept: application/json
    max_response_bytes: 2MiB # from the file's defaults
transform: {type: jq}
limits:
    max_label_value_length: 100
    max_metrics: 50 # from the file's defaults
metrics: [{name: up, type: gauge, expression: .up}]
`
	if got := encode("own"); got != want {
		t.Errorf("own is kept as:\n%s\nwant:\n%s", got, want)
	}
	want = `name: team
request: {type: http, path: /team}
transform: {type: jq}
metrics: [{name: up, type: gauge, expression: .up}]
cache:
    ttl: 30s # from the file's defaults
`
	if got := encode("team"); got != want {
		t.Errorf("team is kept as:\n%s\nwant:\n%s", got, want)
	}
}
