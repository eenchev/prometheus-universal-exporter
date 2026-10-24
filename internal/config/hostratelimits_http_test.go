//go:build !select_request_types || request_type_http

package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// rate_limits is read from the top of the configuration and checked with
// it: its mistakes are reported with the collectors', and a good entry has
// its burst filled in.
func TestRateLimitsLoadWithTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	testutil.WriteIn(t, dir, "config.yaml", testutil.CollectorsDocument("app")+`rate_limits:
  - name: api
    hosts: [api.example, "*.api.example"]
    rate_limit:
      requests_per_second: 5
  - name: twice
    hosts: [API.example]
    rate_limit:
      requests_per_second: 0
`)
	_, err := Load(path)
	for _, want := range []string{
		`rate_limits entry "twice" lists "API.example", which rate_limits entry "api" lists already`,
		`rate_limits entry "twice" rate_limit.requests_per_second is 0`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v; want %q", err, want)
		}
	}
	testutil.WriteIn(t, dir, "config.yaml", testutil.CollectorsDocument("app")+`rate_limits:
  - name: api
    hosts: [api.example]
    rate_limit:
      requests_per_second: 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RateLimits) != 1 || cfg.RateLimits[0].RateLimit.Burst == nil || *cfg.RateLimits[0].RateLimit.Burst != 1 {
		t.Errorf("rate_limits %+v; want the entry with its burst filled in as 1", cfg.RateLimits)
	}
}
