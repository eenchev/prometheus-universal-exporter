//go:build !select_request_types || request_type_http

package repository

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// In the chart, every key of config.data is a file beside config.yaml, which is
// how collector files are supplied. The template renders every key and the
// whole ConfigMap is mounted, and the example the chart README documents loads
// as the exporter would load it, beside the static target file the chart
// renders into the same directory.
func TestChartSuppliesCollectorFilesAsConfigMapKeys(t *testing.T) {
	configmap := readChartFile(t, "templates/configmap.yaml")
	if !strings.Contains(configmap, "range $name, $content := .Values.config.data") {
		t.Fatal("the ConfigMap template must render every key of config.data")
	}
	deployment := readChartFile(t, "templates/deployment.yaml")
	const configVolume = "      volumes:\n        - name: config\n"
	_, volume, found := strings.Cut(deployment, configVolume)
	if !found {
		t.Fatal("the deployment has no configuration volume")
	}
	volume, _, _ = strings.Cut(volume, "        - ")
	if !strings.Contains(volume, "configMap:") || strings.Contains(volume, "items:") {
		t.Fatalf("the configuration volume must mount the whole ConfigMap:\n%s", volume)
	}

	readme := readChartFile(t, "README.md")
	start := strings.Index(readme, "### Collector files")
	if start < 0 {
		t.Fatal("the chart README does not document collector files")
	}
	_, block, _ := strings.Cut(readme[start:], "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal([]byte(block), &values); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range values.Config.Data {
		testutil.WriteIn(t, dir, name, content)
	}
	testutil.WriteIn(t, dir, "targets.yaml", "targets: []\n")
	c, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectorNames(c); !reflect.DeepEqual(got, []string{"payments_api", "search_api"}) {
		t.Fatalf("collectors=%v", got)
	}
}

// The configuration the chart ships by default has to start, so it declares
// its request type like any other.
func TestTheChartsDefaultConfigurationIsValid(t *testing.T) {
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	raw, err := os.ReadFile("charts/prometheus-universal-exporter/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteFile(t, "config.yaml", values.Config.Data["config.yaml"])
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the chart's default configuration does not load: %v", err)
	}
}
