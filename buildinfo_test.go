package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Build information, --version and sizes with units (buildinfo.go,
// bytesize.go).

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--version exited %d: %s", code, stderr.String())
	}
	b := buildVersion()
	want := fmt.Sprintf("prometheus-universal-exporter version %s (revision %s, %s, request types %s)\n", b.Version, b.Revision, runtime.Version(), strings.Join(builtRequestTypes(), ","))
	if stdout.String() != want {
		t.Fatalf("--version printed %q, want %q", stdout.String(), want)
	}
	if b.Version == "" {
		t.Fatal("the version is empty")
	}
}

// -X main.version wins over what Go stamped.
func TestVersionCanBeSetAtBuildTime(t *testing.T) {
	previous := version
	version = "9.9.9"
	t.Cleanup(func() { version = previous })
	if info := computeBuildVersion(); info.Version != "9.9.9" {
		t.Fatalf("version=%q, want the one set at build time", info.Version)
	}
}

func TestBuildInfoMetric(t *testing.T) {
	server := verboseServer(t, false, testCollector("text", "text"))
	exposition := selfMetrics(t, server)
	b := buildVersion()
	want := fmt.Sprintf(`http_exporter_build_info{goversion=%q,request_types=%q,revision=%q,version=%q} 1`, runtime.Version(), strings.Join(builtRequestTypes(), ","), b.Revision, b.Version)
	if !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
	if !strings.Contains(exposition, "# TYPE http_exporter_build_info gauge") {
		t.Fatal("http_exporter_build_info is not a gauge")
	}
}

func TestByteSizes(t *testing.T) {
	for in, want := range map[string]ByteSize{
		"0": 0, "1024": 1024, "10B": 10, "1kB": 1000, "1KB": 1000, "1k": 1000, "2KiB": 2048, "2kib": 2048,
		"10MB": 10_000_000, "64MiB": 64 << 20, "64 MiB": 64 << 20, "1.5GiB": 3 << 29, "1GB": 1e9, "1TiB": 1 << 40, "0.5KiB": 512, "1.0001B": 1,
	} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-1", "10 XB", "1e3", "10MiBs", "ten", "99999999999TiB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) was accepted", in)
		}
	}
	var doc struct {
		A ByteSize `yaml:"a"`
		B ByteSize `yaml:"b"`
		C ByteSize `yaml:"c"`
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
	path := writeIn(t, t.TempDir(), "config.yaml", `collectors:
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
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	web, files := cfg.Collectors[0], cfg.Collectors[1]
	if web.Request.MaxResponseBytes != 1<<20 || web.Limits.MaxResponseBytes != 2_000_000 || web.Limits.MaxOutputBytes != 64<<10 || files.Request.MaxTotalBytes != 3<<19 {
		t.Fatalf("sizes: %d %d %d %d", web.Request.MaxResponseBytes, web.Limits.MaxResponseBytes, web.Limits.MaxOutputBytes, files.Request.MaxTotalBytes)
	}
	if responseLimit(&web) != 1<<20 {
		t.Fatalf("response limit %d", responseLimit(&web))
	}
	bad := writeIn(t, t.TempDir(), "config.yaml", "collectors:\n  - name: web\n    request:\n      type: http\n      max_response_bytes: lots\n")
	if _, err := LoadConfig(bad); err == nil || !strings.Contains(err.Error(), `size "lots"`) {
		t.Fatalf("a malformed size: %v", err)
	}
	schema, err := configSchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(byteSizePattern)
	if !strings.Contains(string(schema), string(quoted)) {
		t.Fatal("the schema does not describe sizes with units")
	}
}

// The released image carries its version.
func TestDockerfileSetsTheVersion(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG VERSION\n", "-X main.version=${VERSION}"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
}
