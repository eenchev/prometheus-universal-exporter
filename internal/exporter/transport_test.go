package exporter

import (
	"os"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
)

func TestStaticTargetCarriesTransportSettings(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	document := "interval: 1m\ntargets:\n  - export_via_otlp: true\n    name: legacy_eu\n    collector: text\n    target: http://legacy.example:8080\n" +
		"    request:\n      follow_redirects: true\n      enable_http2: true\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadStaticTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	target := file.Targets[0]
	query := targetCacheQuery(&target)
	if query.Get("follow_redirects") != "true" || query.Get("enable_http2") != "true" {
		t.Fatalf("the settings must reach the cache key: %v", query)
	}
}
