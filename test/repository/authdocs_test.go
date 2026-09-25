//go:build !select_request_types || request_type_http

package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The configuration examples about authentication are ones a reader can use
// as written: every exporter configuration docs/AUTHENTICATION.md shows with
// its collectors loads as the exporter loads it, one it shows without them
// says it is part of one, and the chart README's Exporter authentication
// values render and give the exporter a configuration that loads.

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

func TestAuthenticationPageExamplesLoad(t *testing.T) {
	loaded := 0
	for _, block := range yamlBlocks(read(t, "docs/AUTHENTICATION.md")) {
		switch {
		case strings.HasPrefix(block, "# exporter config\n") || strings.HasPrefix(block, "collectors:\n") || strings.Contains(block, "\ncollectors:\n"):
			if err := loadExample(t, block); err != nil {
				t.Errorf("%v\n%s", err, block)
			}
			loaded++
		case strings.HasPrefix(block, "web:\n"):
			t.Errorf("an exporter configuration without collectors does not load; mark it as part of one with a first line of # part of the exporter config:\n%s", block)
		}
	}
	if loaded < 2 {
		t.Fatalf("%d complete exporter configurations found, want the two the page shows", loaded)
	}
}

// The web-auth files the chart mounts are made in a directory of the test's
// own, since the configuration names them by their path in the pod.
func TestChartReadmeExporterAuthenticationExamplesRender(t *testing.T) {
	helm := requireHelm(t)
	readme := readChartFile(t, "README.md")
	_, section, found := strings.Cut(readme, "### Exporter authentication\n")
	if !found {
		t.Fatal("the chart README has no Exporter authentication section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	blocks := yamlBlocks(section)
	if len(blocks) != 2 {
		t.Fatalf("%d examples in the section, want 2", len(blocks))
	}
	mounted := t.TempDir()
	testutil.WriteIn(t, mounted, "username", "exporter\n")
	testutil.WriteIn(t, mounted, "password", "change-me\n")
	for _, block := range blocks {
		values := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(values, []byte(block), 0o600); err != nil {
			t.Fatal(err)
		}
		var conf string
		for _, doc := range renderedDocuments(t, helm, "-f", values) {
			if child(doc, "kind").Value == "ConfigMap" {
				if data := path(doc, "data", "config.yaml"); data != nil {
					conf = data.Value
				}
			}
		}
		if conf == "" {
			t.Fatalf("no config.yaml rendered from\n%s", block)
		}
		conf = strings.ReplaceAll(conf, "/var/run/prometheus-universal-exporter/web-auth", mounted)
		if err := loadExample(t, conf); err != nil {
			t.Errorf("%v\n%s", err, block)
		}
	}
}
