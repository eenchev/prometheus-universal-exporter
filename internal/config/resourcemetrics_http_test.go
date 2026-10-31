//go:build !select_request_types || request_type_http

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The go_ and process_ series are on unless resource_metrics_enabled says
// false: reading them no longer stops the world, so nothing argues for making
// the operator ask.
func TestResourceMetricsAreOnUnlessSwitchedOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		document string
		want     bool
	}{
		{"unset", testutil.MinimalConfig, true},
		{"an empty block", "web: {self_metrics: {}}\n" + testutil.MinimalConfig, true},
		{"true", "web: {self_metrics: {resource_metrics_enabled: true}}\n" + testutil.MinimalConfig, true},
		{"false", "web: {self_metrics: {resource_metrics_enabled: false}}\n" + testutil.MinimalConfig, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.document), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Web.SelfMetrics.ResourceMetricsEnabled(); got != tc.want {
				t.Fatalf("resource metrics enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
