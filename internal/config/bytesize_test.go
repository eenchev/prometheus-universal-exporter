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
