//go:build !select_request_types || (request_type_http && request_type_localfile)

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestCheckPassesTheShippedExamples(t *testing.T) {
	plain := runCheckCLI(t, "--config.file=configs/config.example.yaml")
	if plain.code != 0 || plain.report.Status != checkOK {
		t.Fatalf("exit=%d status=%s\n%s", plain.code, plain.report.Status, plain.stdout)
	}
	if collectors, _ := plain.result(t, "config").Details["collectors"].([]any); len(collectors) == 0 {
		t.Fatalf("a passing config check should list its collectors: %+v", plain.result(t, "config"))
	}
	if plain.result(t, "python_scripts").Status != checkOK {
		t.Fatal("the example's Python scripts should pass")
	}

	withTargets := runCheckCLI(t, "--config.file=configs/config.otlp.example.yaml", "--static-targets-file=configs/static-targets.example.yaml")
	if withTargets.code != 0 || withTargets.result(t, "static_targets").Status != checkOK {
		t.Fatalf("exit=%d\n%s", withTargets.code, withTargets.stdout)
	}
	if targets, _ := withTargets.result(t, "static_targets").Details["targets"].([]any); !reflect.DeepEqual(targets, []any{"legacy_eu", "legacy_us", "nightly_backup"}) {
		t.Fatalf("targets details=%v, want every example target", withTargets.result(t, "static_targets").Details)
	}
}

// Without an interpreter the scripts cannot be checked, which is a failure —
// the exporter would not start either — but a configuration with no scripts
// never needs one.
func TestCheckNeedsAnInterpreterOnlyForScripts(t *testing.T) {
	missing := "--python.path=/nonexistent/python"
	withScripts := runCheckCLI(t, "--config.file=configs/config.example.yaml", missing)
	if withScripts.code != 1 || !strings.Contains(withScripts.result(t, "python_scripts").Errors[0], "interpreter") {
		t.Fatalf("exit=%d\n%s", withScripts.code, withScripts.stdout)
	}
	without := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig), missing)
	if without.code != 0 {
		t.Fatalf("exit=%d\n%s", without.code, without.stdout)
	}
}

func TestCheckValidatesTheStaticTargetsFile(t *testing.T) {
	t.Run("invalid on its own", func(t *testing.T) {
		targets := testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: bad-name\n    collector: legacy_text\n    target: http://a.example\n")
		out := runCheckCLI(t, "--config.file=configs/config.otlp.example.yaml", "--static-targets-file="+targets)
		if out.code != 1 || out.result(t, "static_targets").Status != checkFailed || !strings.Contains(out.result(t, "static_targets").Errors[0], "invalid name") {
			t.Fatalf("exit=%d\n%s", out.code, out.stdout)
		}
	})
	t.Run("valid, but not against this configuration", func(t *testing.T) {
		// configs/config.example.yaml leaves OTLP disabled, and static targets
		// need it.
		out := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--static-targets-file=configs/static-targets.example.yaml")
		targets := out.result(t, "static_targets")
		if out.code != 1 || targets.Status != checkFailed || !strings.Contains(targets.Errors[0], "otlp.enabled") {
			t.Fatalf("exit=%d targets=%+v", out.code, targets)
		}
		if out.result(t, "config").Status != checkOK {
			t.Fatal("the configuration itself is fine; only the pairing is not")
		}
	})
	t.Run("valid, with the configuration broken", func(t *testing.T) {
		out := runCheckCLI(t, "--config.file="+t.TempDir()+"/absent.yaml", "--static-targets-file=configs/static-targets.example.yaml")
		targets := out.result(t, "static_targets")
		if out.code != 1 || targets.Status != checkSkipped || targets.Details["targets"] == nil {
			t.Fatalf("targets=%+v, want skipped, still listing what it loaded", targets)
		}
	})
}

// Every stderr line is JSON (runCLI fails otherwise), each step is logged, and
// a failure is logged at ERROR with its errors.
func TestCheckLogsEachStepAsJSON(t *testing.T) {
	broken := strings.Replace(testutil.MinimalConfig, "expression:", "error_mode: panic\n        expression:", 1)
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", broken))
	var failed, skipped, complete bool
	for _, record := range out.logs {
		switch record["msg"] {
		case "configuration check failed":
			failed = record["level"] == "ERROR" && record["check"] == "config" && record["errors"] != nil
		case "configuration check skipped":
			skipped = record["level"] == "WARN" && record["reason"] != nil
		case "configuration check complete":
			complete = record["status"] == checkFailed
		}
	}
	if !failed || !skipped || !complete {
		t.Fatalf("failed=%v skipped=%v complete=%v in:\n%s", failed, skipped, complete, out.stderr)
	}

	// --log.level quietens the log, never the report.
	quiet := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--log.level=error")
	if quiet.code != 0 || len(quiet.logs) != 0 || quiet.report.Status != checkOK {
		t.Fatalf("exit=%d logs=%d status=%s", quiet.code, len(quiet.logs), quiet.report.Status)
	}
}

// The check never serves: a well-formed listen address that could not be
// bound here, one of another machine, does not matter to it. One that is not
// host:port at all is a command-line error (TestOutOfRangeLimits…).
func TestCheckDoesNotStartTheServer(t *testing.T) {
	out := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--web.listen-address=192.0.2.1:9115")
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// Static targets need no OTLP export unless one sets export_via_otlp, so a
// document without it passes against the plain example configuration, and the
// shipped one, which exports a target over OTLP, does not.
func TestCheckStaticTargetsWithoutOTLP(t *testing.T) {
	endpointOnly := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--static-targets-file=testdata/chart/static-targets-endpoint-only.yaml")
	if endpointOnly.code != 0 || endpointOnly.result(t, "static_targets").Status != checkOK {
		t.Fatalf("exit=%d\n%s", endpointOnly.code, endpointOnly.stdout)
	}
	exported := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--static-targets-file=configs/static-targets.example.yaml")
	result := exported.result(t, "static_targets")
	if exported.code != 1 || result.Status != checkFailed || !strings.Contains(result.Errors[0], "export_via_otlp") {
		t.Fatalf("exit=%d\n%s", exported.code, exported.stdout)
	}
}
