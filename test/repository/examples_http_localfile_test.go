//go:build !select_request_types || (request_type_http && request_type_localfile)

package repository

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"gopkg.in/yaml.v3"
)

// The shipped configurations document the key; the reference example uses it.
func TestTheExampleConfigurationShowsMetricsPrefix(t *testing.T) {
	cfg, err := config.Load("configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.Collectors {
		if c.MetricsPrefix != "" {
			return
		}
	}
	raw, _ := yaml.Marshal(cfg.Collectors[0])
	t.Fatalf("configs/config.example.yaml has no collector with metrics_prefix; first collector:\n%s", raw)
}

// The shipped examples must stay loadable and consistent with each other.
func TestShippedExampleFilesLoadTogether(t *testing.T) {
	cfg, err := config.Load("configs/config.otlp.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OTLP.Enabled {
		t.Fatal("configs/config.otlp.example.yaml must enable OTLP export")
	}
	file, err := config.LoadStaticTargets("configs/static-targets.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("configs/static-targets.example.yaml does not match configs/config.otlp.example.yaml: %v", err)
	}

	plain, err := config.Load("configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargetsAgainst(file, plain); err == nil {
		t.Fatal("configs/config.example.yaml leaves OTLP disabled, so the target file must be rejected against it")
	}
}

// The configurations the repository ships must satisfy the contract they
// demonstrate — the examples an operator copies from, and the fixture the
// different file format demos runs against, which is the only one of them carrying a
// pre-script.
func TestShippedExampleScriptsSatisfyTheContract(t *testing.T) {
	for _, path := range append([]string{
		"configs/config.example.yaml",
		"configs/config.otlp.example.yaml",
	}, shippedExamples(t).configs...) {
		t.Run(path, func(t *testing.T) {
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := transform.ValidatePythonScripts("python3", cfg); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		})
	}
}
