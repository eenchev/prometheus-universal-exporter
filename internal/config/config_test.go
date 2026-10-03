package config

import (
	"os"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestExporterBasicAuthRejectsAuthorizationBridge(t *testing.T) {
	c := testutil.Collector("text", "text")
	c.Request.ForwardAuthorization = true
	cfg := &model.Config{Collectors: []model.Collector{c}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}}}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "forward_authorization") {
		t.Fatalf("expected auth bridge conflict, got %v", err)
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("collectors:\n  - name: app\n    unknown: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Load() error=%v, want unknown-field error", err)
	}
}
