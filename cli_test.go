package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Build information, --version and sizes with units (exporter/buildinfo.go,
// model/bytesize.go).

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--version exited %d: %s", code, stderr.String())
	}
	b := exporter.BuildVersion()
	want := fmt.Sprintf("prometheus-universal-exporter version %s (revision %s, %s, request types %s)\n", b.Version, b.Revision, runtime.Version(), strings.Join(fetch.BuiltRequestTypes(), ","))
	if stdout.String() != want {
		t.Fatalf("--version printed %q, want %q", stdout.String(), want)
	}
	if b.Version == "" {
		t.Fatal("the version is empty")
	}
}

// -X main.version reaches the build information, where it wins over what Go
// stamped (exporter/buildinfo_test.go).
func TestVersionCanBeSetAtBuildTime(t *testing.T) {
	previous, previousVersion := version, exporter.Version
	version = "9.9.9"
	applyVersion()
	t.Cleanup(func() { version, exporter.Version = previous, previousVersion })
	if exporter.Version != "9.9.9" {
		t.Fatalf("version=%q, want the one set at build time", exporter.Version)
	}
}

// --dry-run answers "would this start?" without starting: it runs startup's
// validation, prints one JSON report on stdout, logs JSON lines on stderr, and
// exits 0 when everything would load and 1 when anything would not.

type checkRun struct {
	code   int
	report checkReport
	stdout string
	stderr string
	logs   []map[string]any
}

// runCLI drives the real command line, so these tests cover the flag, the
// streams and the exit status exactly as a shell would see them.
func runCLI(t *testing.T, args ...string) checkRun {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	out := checkRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	for _, line := range strings.Split(strings.TrimSpace(out.stderr), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("stderr line is not JSON: %q", line)
		}
		out.logs = append(out.logs, record)
	}
	return out
}

func runCheckCLI(t *testing.T, args ...string) checkRun {
	t.Helper()
	out := runCLI(t, append([]string{"--dry-run"}, args...)...)
	// stdout carries the report and nothing else: one JSON document.
	decoder := json.NewDecoder(strings.NewReader(out.stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out.report); err != nil {
		t.Fatalf("stdout is not a check report: %v\n%s", err, out.stdout)
	}
	if decoder.More() {
		t.Fatalf("stdout carries more than the report:\n%s", out.stdout)
	}
	return out
}

func (r checkRun) result(t *testing.T, check string) checkResult {
	t.Helper()
	for _, result := range r.report.Checks {
		if result.Check == check {
			return result
		}
	}
	t.Fatalf("no %q check in the report:\n%s", check, r.stdout)
	return checkResult{}
}

func (r checkRun) has(check string) bool {
	for _, result := range r.report.Checks {
		if result.Check == check {
			return true
		}
	}
	return false
}

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

	withTargets := runCheckCLI(t, "--config.file=configs/config.otlp.example.yaml", "--otlp.targets-file=configs/targets.example.yaml")
	if withTargets.code != 0 || withTargets.result(t, "targets").Status != checkOK {
		t.Fatalf("exit=%d\n%s", withTargets.code, withTargets.stdout)
	}
	if targets, _ := withTargets.result(t, "targets").Details["targets"].([]any); !reflect.DeepEqual(targets, []any{"legacy_eu", "legacy_us", "nightly_backup"}) {
		t.Fatalf("targets details=%v, want every example target", withTargets.result(t, "targets").Details)
	}
}

// Only the steps that apply are reported: no targets check without a targets
// file, and no watch check without --config.watch.
func TestCheckReportsOnlyTheStepsThatApply(t *testing.T) {
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig))
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
	if out.has("targets") || out.has("config_watch") {
		t.Fatalf("unexpected steps:\n%s", out.stdout)
	}
	if scripts := out.result(t, "python_scripts").Details["scripts"]; scripts != float64(0) {
		t.Fatalf("scripts=%v, want 0 for a configuration without Python", scripts)
	}
}

func TestCheckFailsAnInvalidConfiguration(t *testing.T) {
	broken := strings.Replace(testutil.MinimalConfig, "expression:", "error_mode: panic\n        expression:", 1)
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", broken))
	if out.code != 1 || out.report.Status != checkFailed {
		t.Fatalf("exit=%d status=%s", out.code, out.report.Status)
	}
	conf := out.result(t, "config")
	if conf.Status != checkFailed || len(conf.Errors) != 1 || !strings.Contains(conf.Errors[0], "error_mode") {
		t.Fatalf("config=%+v", conf)
	}
	// The scripts cannot be read from a configuration that did not load, and
	// the report says so rather than leaving the step out.
	if python := out.result(t, "python_scripts"); python.Status != checkSkipped || python.Reason == "" {
		t.Fatalf("python_scripts=%+v, want skipped with a reason", python)
	}
}

func TestCheckFailsAMissingConfigurationFile(t *testing.T) {
	out := runCheckCLI(t, "--config.file="+t.TempDir()+"/absent.yaml")
	if out.code != 1 || !strings.Contains(out.result(t, "config").Errors[0], "no such file") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// The Python check reports each faulty script on its own line, not one blob.
func TestCheckListsEveryPythonFault(t *testing.T) {
	conf := testutil.MinimalConfig + `  - name: first
    request:
      type: http
    transform:
      type: jq
      pre_script: |
        result = 1
    metrics:
      - name: first_value
        expression: .value
  - name: second
    request:
      type: http
    transform:
      type: jq
      pre_script: |
        result = 2
    metrics:
      - name: second_value
        expression: .value
`
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", conf))
	python := out.result(t, "python_scripts")
	if out.code != 1 || python.Status != checkFailed || len(python.Errors) != 2 {
		t.Fatalf("exit=%d python=%+v", out.code, python)
	}
	for i, collector := range []string{"first", "second"} {
		if !strings.Contains(python.Errors[i], collector) {
			t.Errorf("error %d %q should name collector %q", i, python.Errors[i], collector)
		}
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

func TestCheckValidatesTheTargetsFile(t *testing.T) {
	t.Run("invalid on its own", func(t *testing.T) {
		targets := testutil.WriteFile(t, "targets.yaml", "targets:\n  - name: bad-name\n    collector: legacy_text\n    target: http://a.example\n")
		out := runCheckCLI(t, "--config.file=configs/config.otlp.example.yaml", "--otlp.targets-file="+targets)
		if out.code != 1 || out.result(t, "targets").Status != checkFailed || !strings.Contains(out.result(t, "targets").Errors[0], "invalid name") {
			t.Fatalf("exit=%d\n%s", out.code, out.stdout)
		}
	})
	t.Run("valid, but not against this configuration", func(t *testing.T) {
		// configs/config.example.yaml leaves OTLP disabled, and scheduled targets
		// need it.
		out := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--otlp.targets-file=configs/targets.example.yaml")
		targets := out.result(t, "targets")
		if out.code != 1 || targets.Status != checkFailed || !strings.Contains(targets.Errors[0], "otlp.enabled") {
			t.Fatalf("exit=%d targets=%+v", out.code, targets)
		}
		if out.result(t, "config").Status != checkOK {
			t.Fatal("the configuration itself is fine; only the pairing is not")
		}
	})
	t.Run("valid, with the configuration broken", func(t *testing.T) {
		out := runCheckCLI(t, "--config.file="+t.TempDir()+"/absent.yaml", "--otlp.targets-file=configs/targets.example.yaml")
		targets := out.result(t, "targets")
		if out.code != 1 || targets.Status != checkSkipped || targets.Details["targets"] == nil {
			t.Fatalf("targets=%+v, want skipped, still listing what it loaded", targets)
		}
	})
}

func TestCheckValidatesTheWatchFlags(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig)
	bad := runCheckCLI(t, conf, "--config.watch", "--config.watch-interval=0s")
	if bad.code != 1 || bad.result(t, "config_watch").Status != checkFailed {
		t.Fatalf("exit=%d\n%s", bad.code, bad.stdout)
	}
	good := runCheckCLI(t, conf, "--config.watch", "--config.watch-interval=90s")
	if good.code != 0 || good.result(t, "config_watch").Details["interval"] != "1m30s" {
		t.Fatalf("exit=%d\n%s", good.code, good.stdout)
	}
	// Without the watch the interval is never read, at startup or here.
	if unused := runCheckCLI(t, conf, "--config.watch-interval=0s"); unused.code != 0 || unused.has("config_watch") {
		t.Fatalf("exit=%d\n%s", unused.code, unused.stdout)
	}
}

// --config.export-env changes what is loaded, so it changes the verdict: a
// check run where the variables are not set fails, as startup would.
func TestCheckHonoursEnvironmentExpansion(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", strings.Replace(testutil.MinimalConfig, "path: /status", "path: ${CHECK_DEMO_PATH}", 1))
	if literal := runCheckCLI(t, conf); literal.code != 0 {
		t.Fatalf("without the flag the reference is literal text: exit=%d\n%s", literal.code, literal.stdout)
	}
	os.Unsetenv("CHECK_DEMO_PATH")
	missing := runCheckCLI(t, conf, "--config.export-env")
	if missing.code != 1 || !strings.Contains(missing.result(t, "config").Errors[0], "CHECK_DEMO_PATH") {
		t.Fatalf("exit=%d\n%s", missing.code, missing.stdout)
	}
	t.Setenv("CHECK_DEMO_PATH", "/status")
	if set := runCheckCLI(t, conf, "--config.export-env"); set.code != 0 || set.result(t, "config").Details["config_export_env"] != true {
		t.Fatalf("exit=%d\n%s", set.code, set.stdout)
	}
}

// The check calls startup's own validation, so anything it fails, startup
// refuses too. This pins that for every failing case above.
func TestCheckAgreesWithStartup(t *testing.T) {
	broken := strings.Replace(testutil.MinimalConfig, "expression:", "error_mode: panic\n        expression:", 1)
	badTargets := testutil.WriteFile(t, "targets.yaml", "targets:\n  - name: bad-name\n    collector: legacy_text\n    target: http://a.example\n")
	for name, args := range map[string][]string{
		"invalid configuration":     {"--config.file=" + testutil.WriteFile(t, "config.yaml", broken)},
		"missing configuration":     {"--config.file=" + t.TempDir() + "/absent.yaml"},
		"no interpreter":            {"--config.file=configs/config.example.yaml", "--python.path=/nonexistent/python"},
		"invalid targets file":      {"--config.file=configs/config.otlp.example.yaml", "--otlp.targets-file=" + badTargets},
		"targets without OTLP":      {"--config.file=configs/config.example.yaml", "--otlp.targets-file=configs/targets.example.yaml"},
		"non-positive watch period": {"--config.file=configs/config.example.yaml", "--config.watch", "--config.watch-interval=0s"},
	} {
		t.Run(name, func(t *testing.T) {
			if check := runCheckCLI(t, args...); check.code != 1 {
				t.Fatalf("--dry-run exit=%d, want 1", check.code)
			}
			if start := runCLI(t, args...); start.code != 1 {
				t.Fatalf("startup exit=%d, want 1: --dry-run and startup disagree", start.code)
			}
		})
	}
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

// The check never serves: a listen address that could not be bound does not
// matter to it.
func TestCheckDoesNotStartTheServer(t *testing.T) {
	out := runCheckCLI(t, "--config.file=configs/config.example.yaml", "--web.listen-address=256.0.0.1:99999")
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// A command line that cannot be parsed is not a verdict on the configuration,
// so it keeps the conventional status 2, and -h is not an error.
func TestCommandLineErrorsAreNotCheckResults(t *testing.T) {
	bad := runCLI(t, "--dry-run", "--no-such-flag")
	if bad.code != 2 || bad.stdout != "" {
		t.Fatalf("exit=%d stdout=%q", bad.code, bad.stdout)
	}
	// Even the complaint about the command line is a JSON log line.
	if len(bad.logs) != 1 || !strings.Contains(bad.logs[0]["error"].(string), "no-such-flag") {
		t.Fatalf("logs=%v", bad.logs)
	}
	help := runCLI(t, "-h")
	if help.code != 0 || !strings.Contains(help.stdout, "-dry-run") || help.stderr != "" {
		t.Fatalf("-h exit=%d stdout=%q stderr=%q", help.code, help.stdout, help.stderr)
	}
}

// Startup and --dry-run both refuse a duplicate across files, and the dry run
// reports which collector files it read.
func TestStartupAndDryRunRefuseDuplicateCollectors(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "a.yaml", testutil.CollectorsDocument("demo"))
	duplicate := testutil.WriteIn(t, dir, "duplicate.yaml", "collector_files: [a.yaml]\n"+testutil.CollectorsDocument("demo"))
	check := runCheckCLI(t, "--config.file="+duplicate)
	if check.code != 1 || !strings.Contains(strings.Join(check.result(t, "config").Errors, "\n"), `duplicate collector "demo"`) {
		t.Fatalf("exit=%d\n%s", check.code, check.stdout)
	}
	if start := runCLI(t, "--config.file="+duplicate); start.code != 1 || !strings.Contains(start.stderr, `duplicate collector \"demo\"`) {
		t.Fatalf("startup exit=%d stderr=%s", start.code, start.stderr)
	}

	fine := testutil.WriteIn(t, dir, "fine.yaml", "collector_files: [a.yaml]\n"+testutil.CollectorsDocument("own"))
	check = runCheckCLI(t, "--config.file="+fine)
	details := check.result(t, "config").Details
	if check.code != 0 || !reflect.DeepEqual(details["collectors"], []any{"own", "demo"}) || !reflect.DeepEqual(details["collector_files"], []any{filepath.Join(dir, "a.yaml")}) {
		t.Fatalf("exit=%d\n%s", check.code, check.stdout)
	}
}

func TestConfigSchemaFlagPrintsTheSchema(t *testing.T) {
	out := runCLI(t, "--config.schema")
	generated, _ := config.SchemaJSON()
	if out.code != 0 || out.stdout != string(generated) || out.stderr != "" {
		t.Fatalf("exit=%d stderr=%q stdout starts %q", out.code, out.stderr, testutil.FirstLines(out.stdout, 3))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out.stdout), &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorFileSchemaFlagPrintsTheSchema(t *testing.T) {
	out := runCLI(t, "--config.collector-file-schema")
	generated, _ := config.CollectorFileSchemaJSON()
	if out.code != 0 || out.stdout != string(generated) || out.stderr != "" {
		t.Fatalf("exit=%d stderr=%q stdout starts %q", out.code, out.stderr, testutil.FirstLines(out.stdout, 3))
	}
}

func TestLogLevelIsHonoured(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var out bytes.Buffer
	logger := newLogger("error", &out)
	logger.Info("suppressed")
	slog.Default().Warn("also suppressed")
	logger.Error("kept")
	records := testutil.AssertJSONLines(t, &out, 1)
	if records[0]["msg"] != "kept" {
		t.Fatalf("msg=%v, want only the error line", records[0]["msg"])
	}
}

// --dry-run reports an invalid prefix as a failed configuration, as startup
// would refuse it.
func TestDryRunReportsAnInvalidMetricsPrefix(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", `collectors:
  - name: prefixed
    metrics_prefix: grafana_
    request:
      type: http
    transform:
      type: regex
    metrics:
      - name: demo_value
        description: A value
        type: gauge
        error_mode: log
        expression: 'value=(\d+)'
`)
	out := runCheckCLI(t, "--config.file="+path)
	if out.code != 1 || out.report.Status != "failed" {
		t.Fatalf("exit=%d status=%s", out.code, out.report.Status)
	}
	result := out.result(t, "config")
	if result.Status != checkFailed || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], `invalid metrics_prefix "grafana_"`) {
		t.Fatalf("config check=%+v", result)
	}
}

// --dry-run reports a missing type like any other invalid configuration.
func TestDryRunReportsAMissingRequestType(t *testing.T) {
	untyped := strings.Replace(testutil.MinimalConfig, "      type: http\n", "", 1)
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", untyped))
	if out.code != 1 || !strings.Contains(out.result(t, "config").Errors[0], "request.type") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// The dry-run report says which types the binary has, so a configuration can
// be checked against the build that will run it.
func TestDryRunReportListsTheBuiltRequestTypes(t *testing.T) {
	report := checkStartup(checkInputs{ConfigFile: filepath.Join(t.TempDir(), "missing.yaml")})
	if !reflect.DeepEqual(report.RequestTypes, fetch.BuiltRequestTypes()) {
		t.Fatalf("report lists %v, want %v", report.RequestTypes, fetch.BuiltRequestTypes())
	}
}

// --dry-run reports an expression that does not compile, because it loads the
// configuration the way startup does.
func TestDryRunReportsAnExpressionThatDoesNotCompile(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", `collectors:
  - name: html
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: value
        description: A value
        type: gauge
        expression: 'td:nth-child('
`)
	out := runCheckCLI(t, "--config.file="+path)
	result := out.result(t, "config")
	if out.code != 1 || result.Status != checkFailed || !strings.Contains(strings.Join(result.Errors, " "), `CSS selector "td:nth-child("`) {
		t.Fatalf("exit=%d config=%+v", out.code, result)
	}
}

// The dry-run report and the log carry the deprecations, and the check still
// passes: a deprecated spelling works until it is removed.
// A collector leaving its decoder to each response still passes, and is
// reported and logged so the operator can pin it.
func TestDryRunWarnsOfAnUnsetDecoder(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", `collectors:
  - name: undecided
    request:
      type: http
    transform:
      type: jq
    metrics:
      - name: value
        expression: .value
`)
	out := runCheckCLI(t, "--config.file="+path)
	result := out.result(t, "config")
	warnings, _ := result.Details["warnings"].([]any)
	if out.code != 0 || result.Status != checkOK || len(warnings) != 1 || !strings.Contains(warnings[0].(string), `collector "undecided" sets no decoder.type`) {
		t.Fatalf("exit=%d config=%+v", out.code, result)
	}
	logged := false
	for _, record := range out.logs {
		if record["msg"] == "configuration warning" && strings.Contains(record["warning"].(string), "undecided") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no warning in the log:\n%s", out.stderr)
	}
}

// None is accepted at present, but the plumbing stays: a deprecation Validate
// records is listed in the report, and logged as startup logs it, and the
// check still passes.
func TestDryRunReportsDeprecations(t *testing.T) {
	conf := &model.Config{Collectors: []model.Collector{{Name: "legacy"}}, Deprecations: []string{`collector "legacy" old_key: "x" is deprecated; use "y", which means the same`}}
	details := configDetails(conf, false)
	if deprecations, _ := details["deprecations"].([]string); len(deprecations) != 1 || deprecations[0] != conf.Deprecations[0] {
		t.Fatalf("details=%v", details)
	}
	if _, listed := configDetails(&model.Config{}, false)["deprecations"]; listed {
		t.Fatal("a configuration without deprecations lists them")
	}
	var out bytes.Buffer
	logNotices(newLogger("info", &out), checkResult{Check: "config", File: "config.yaml", Status: checkOK, Details: details})
	records := testutil.AssertJSONLines(t, &out, 1)
	if records[0]["msg"] != "deprecated configuration" || records[0]["level"] != "WARN" || records[0]["deprecation"] != conf.Deprecations[0] || records[0]["file"] != "config.yaml" {
		t.Fatalf("record=%v", records[0])
	}
}

func TestANegativeTimeoutOffsetIsACommandLineError(t *testing.T) {
	for _, args := range [][]string{
		{"--probe.timeout-offset=-1s"},
		{"--dry-run", "--config.file=configs/config.example.yaml", "--probe.timeout-offset=-1s"},
	} {
		out := runCLI(t, args...)
		if out.code != 2 || !strings.Contains(out.stderr, "--probe.timeout-offset must not be negative") || out.stdout != "" {
			t.Fatalf("%v: exit=%d stderr=%s stdout=%s", args, out.code, out.stderr, out.stdout)
		}
	}
}

func TestTargetsFileSchemaFlagPrintsTheSchema(t *testing.T) {
	out := runCLI(t, "--otlp.targets-file-schema")
	generated, _ := config.TargetsSchemaJSON()
	if out.code != 0 || out.stdout != string(generated) || out.stderr != "" {
		t.Fatalf("exit=%d stderr=%q stdout starts %q", out.code, out.stderr, testutil.FirstLines(out.stdout, 3))
	}
}
