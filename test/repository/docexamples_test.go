//go:build !select_request_types || request_type_http || (request_type_radius && request_type_whois)

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The helpers of the tests that load the configuration examples a page
// shows, in every build that has one of those tests.

// yamlBlocks returns the fenced YAML blocks of text.
func yamlBlocks(text string) []string {
	var blocks []string
	for _, part := range strings.Split(text, "```yaml\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	return blocks
}

// loadExample loads conf as the exporter's configuration file, credential
// files included, and returns why it does not load.
func loadExample(t *testing.T, conf string) error {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", conf))
	if err != nil {
		return err
	}
	if cfg.Web.BasicAuth != nil && cfg.Web.BasicAuth.Enabled {
		_, _, err = config.WebAuthCredentials(cfg.Web.BasicAuth)
	}
	return err
}
