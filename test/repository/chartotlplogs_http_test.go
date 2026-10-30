//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The chart README's OTLP export example renders, and the exporter loads the
// configuration it renders as the pod reads it, with --config.expand-env and
// the token from the Secret in the environment: the export of the
// exporter's own log is on, to the metrics' collector at /v1/logs, with the
// header both exports send.
func TestChartReadmeOTLPLogExportExampleRendersAndLoads(t *testing.T) {
	helm := requireHelm(t)
	_, section, found := strings.Cut(readChartFile(t, "README.md"), "### OTLP export\n")
	if !found {
		t.Fatal("the chart README has no OTLP export section")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	var values string
	for _, block := range yamlBlocksOf(section) {
		if strings.Contains(block, "logs:") {
			values = block
		}
	}
	if values == "" {
		t.Fatal("the OTLP export section shows no values with otlp.logs")
	}
	var configYAML string
	var expandEnv, tokenFromSecret bool
	for _, doc := range renderedDocuments(t, helm, "-f", valuesFile(t, values)) {
		switch child(doc, "kind").Value {
		case "ConfigMap":
			if data := path(doc, "data", "config.yaml"); data != nil {
				configYAML = data.Value
			}
		case "Deployment":
			container := path(doc, "spec", "template", "spec", "containers", "0")
			for _, arg := range path(container, "args").Content {
				expandEnv = expandEnv || arg.Value == "--config.expand-env"
			}
			for _, env := range path(container, "env").Content {
				if child(env, "name").Value == "OTLP_TOKEN" && path(env, "valueFrom", "secretKeyRef", "key") != nil {
					tokenFromSecret = true
				}
			}
		}
	}
	if !expandEnv || !tokenFromSecret {
		t.Errorf("the Deployment expands the environment: %v; has the token from the Secret: %v", expandEnv, tokenFromSecret)
	}
	t.Setenv("OTLP_TOKEN", "s3cret")
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", configYAML), config.WithEnvExpansion())
	if err != nil {
		t.Fatalf("the exporter does not load the rendered configuration: %v\n%s", err, configYAML)
	}
	if !cfg.OTLP.Enabled || !cfg.OTLP.Logs.Enabled || cfg.OTLP.Logs.Endpoint != "http://otel-collector.observability:4318/v1/logs" || cfg.OTLP.Headers["Authorization"] != "Bearer s3cret" {
		t.Errorf("the rendered configuration exports %+v", cfg.OTLP)
	}
}
