package config

import (
	"log/slog"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Deprecations and warnings are logged on every start and reload. None is
// deprecated at present, but a deprecation Validate records is logged.
func TestDeprecationsAndWarningsAreLogged(t *testing.T) {
	out := testutil.CaptureLogs(t)
	LogNotices(slog.Default(), "config.yaml", &model.Config{
		Deprecations: []string{"collector \"x\" old_key is deprecated"},
		Warnings:     []string{"collector \"x\" sets no decoder.type"},
	})
	records := testutil.AssertJSONLines(t, out, 2)
	if records[0]["msg"] != "deprecated configuration" || records[0]["level"] != "WARN" || records[0]["deprecation"] != "collector \"x\" old_key is deprecated" || records[0]["file"] != "config.yaml" {
		t.Fatalf("deprecation record=%v", records[0])
	}
	if records[1]["msg"] != "configuration warning" || records[1]["level"] != "WARN" || records[1]["warning"] != "collector \"x\" sets no decoder.type" || records[1]["file"] != "config.yaml" {
		t.Fatalf("warning record=%v", records[1])
	}
}
