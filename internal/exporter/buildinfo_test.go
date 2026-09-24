package exporter

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestBuildInfoMetric(t *testing.T) {
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	exposition := selfMetrics(t, server)
	b := BuildVersion()
	want := fmt.Sprintf(`http_exporter_build_info{goversion=%q,request_types=%q,revision=%q,version=%q} 1`, runtime.Version(), strings.Join(fetch.BuiltRequestTypes(), ","), b.Revision, b.Version)
	if !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
	if !strings.Contains(exposition, "# TYPE http_exporter_build_info gauge") {
		t.Fatal("http_exporter_build_info is not a gauge")
	}
}

func TestByteSizes(t *testing.T) {
	for in, want := range map[string]model.ByteSize{
		"0": 0, "1024": 1024, "10B": 10, "1kB": 1000, "1KB": 1000, "1k": 1000, "2KiB": 2048, "2kib": 2048,
		"10MB": 10_000_000, "64MiB": 64 << 20, "64 MiB": 64 << 20, "1.5GiB": 3 << 29, "1GB": 1e9, "1TiB": 1 << 40, "0.5KiB": 512, "1.0001B": 1,
	} {
		got, err := model.ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-1", "10 XB", "1e3", "10MiBs", "ten", "99999999999TiB"} {
		if _, err := model.ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) was accepted", in)
		}
	}
	var doc struct {
		A model.ByteSize `yaml:"a"`
		B model.ByteSize `yaml:"b"`
		C model.ByteSize `yaml:"c"`
	}
	if err := yaml.Unmarshal([]byte("a: 1048576\nb: 64MiB\nc: \"512\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.A != 1<<20 || doc.B != 64<<20 || doc.C != 512 {
		t.Fatalf("decoded %+v", doc)
	}
	if err := yaml.Unmarshal([]byte("a: [1]\n"), &doc); err == nil {
		t.Fatal("a list was accepted as a size")
	}
}

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
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	web, files := cfg.Collectors[0], cfg.Collectors[1]
	if web.Request.MaxResponseBytes != 1<<20 || web.Limits.MaxResponseBytes != 2_000_000 || web.Limits.MaxOutputBytes != 64<<10 || files.Request.MaxTotalBytes != 3<<19 {
		t.Fatalf("sizes: %d %d %d %d", web.Request.MaxResponseBytes, web.Limits.MaxResponseBytes, web.Limits.MaxOutputBytes, files.Request.MaxTotalBytes)
	}
	if fetch.ResponseLimit(&web) != 1<<20 {
		t.Fatalf("response limit %d", fetch.ResponseLimit(&web))
	}
	bad := testutil.WriteIn(t, t.TempDir(), "config.yaml", "collectors:\n  - name: web\n    request:\n      type: http\n      max_response_bytes: lots\n")
	if _, err := config.Load(bad); err == nil || !strings.Contains(err.Error(), `size "lots"`) {
		t.Fatalf("a malformed size: %v", err)
	}
	schema, err := config.SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(model.ByteSizePattern)
	if !strings.Contains(string(schema), string(quoted)) {
		t.Fatal("the schema does not describe sizes with units")
	}
}
