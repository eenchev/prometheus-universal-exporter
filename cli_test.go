package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
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

func TestCheckFailsAMissingConfigurationFile(t *testing.T) {
	out := runCheckCLI(t, "--config.file="+t.TempDir()+"/absent.yaml")
	if out.code != 1 || !strings.Contains(out.result(t, "config").Errors[0], "no such file") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// The check calls startup's own validation, so anything it fails, startup
// refuses too. This pins that for every failing case above.
func TestCheckAgreesWithStartup(t *testing.T) {
	broken := strings.Replace(testutil.MinimalConfig, "expression:", "error_mode: panic\n        expression:", 1)
	badTargets := testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: bad-name\n    collector: legacy_text\n    target: http://a.example\n")
	for name, args := range map[string][]string{
		"invalid configuration":     {"--config.file=" + testutil.WriteFile(t, "config.yaml", broken)},
		"missing configuration":     {"--config.file=" + t.TempDir() + "/absent.yaml"},
		"no interpreter":            {"--config.file=configs/config.example.yaml", "--python.path=/nonexistent/python"},
		"invalid targets file":      {"--config.file=configs/config.otlp.example.yaml", "--static-targets-file=" + badTargets},
		"targets without OTLP":      {"--config.file=configs/config.example.yaml", "--static-targets-file=configs/static-targets.example.yaml"},
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

func TestANegativeDefaultProbeTimeoutIsACommandLineError(t *testing.T) {
	for _, args := range [][]string{
		{"--probe.default-timeout=-1s"},
		{"--dry-run", "--config.file=configs/config.example.yaml", "--probe.default-timeout=-1s"},
	} {
		out := runCLI(t, args...)
		if out.code != 2 || !strings.Contains(out.stderr, "--probe.default-timeout must not be negative") || out.stdout != "" {
			t.Fatalf("%v: exit=%d stderr=%s stdout=%s", args, out.code, out.stderr, out.stdout)
		}
	}
}

func TestOutOfRangeLimitsAreCommandLineErrors(t *testing.T) {
	for flag, message := range map[string]string{
		"--probe.max-concurrent=-1":        "--probe.max-concurrent must not be negative",
		"--python.max-workers=-1":          "--python.max-workers must not be negative",
		"--runtime.memory-limit-ratio=1.5": "--runtime.memory-limit-ratio must be from 0",
		// A soft limit of a few kilobytes would have the Go runtime collect
		// garbage all the time.
		"--runtime.memory-limit-ratio=0.00001":   "or at least 0.1, got 1e-05: below it the Go memory limit leaves the heap almost nothing",
		"--runtime.memory-limit-ratio=1e-300":    "or at least 0.1, got 1e-300",
		"--runtime.memory-limit-ratio=0.0999999": "or at least 0.1, got 0.0999999",
		// A bare port would stop the start with "missing port in address";
		// --dry-run says so, rather than ok.
		"--web.listen-address=9115":    "must be host:port, such as :8080",
		"--web.listen-address=:999999": "which is not a TCP port",
	} {
		for _, args := range [][]string{{flag}, {"--dry-run", "--config.file=configs/config.example.yaml", flag}} {
			out := runCLI(t, args...)
			if out.code != 2 || !strings.Contains(out.stderr, message) || out.stdout != "" {
				t.Fatalf("%v: exit=%d stderr=%s stdout=%s", args, out.code, out.stderr, out.stdout)
			}
		}
	}
}

// The floor of --runtime.memory-limit-ratio leaves 0, for off, and 0.1 to 1
// as they were: neither startup nor --dry-run takes them for a command-line
// error.
func TestAMemoryLimitRatioOfZeroOrFromATenthIsNoCommandLineError(t *testing.T) {
	for _, ratio := range []string{"0", "0.1", ".1", "0.10000001", "0.5", "1"} {
		out := runCLI(t, "--dry-run", "--config.file=configs/config.example.yaml", "--runtime.memory-limit-ratio="+ratio)
		if out.code == 2 || strings.Contains(out.stderr, "memory-limit-ratio") {
			t.Errorf("ratio %s: exit=%d stderr=%s", ratio, out.code, out.stderr)
		}
	}
}

func TestASelfMetricsPathOfAnotherEndpointIsACommandLineError(t *testing.T) {
	for _, args := range [][]string{
		{"--web.self-metrics-path=/probe"},
		{"--dry-run", "--config.file=configs/config.example.yaml", "--web.self-metrics-path=/"},
		{"--web.self-metrics-path=/stats/"},
	} {
		out := runCLI(t, args...)
		if out.code != 2 || !strings.Contains(out.stderr, "--web.self-metrics-path") || out.stdout != "" {
			t.Fatalf("%v: exit=%d stderr=%s stdout=%s", args, out.code, out.stderr, out.stdout)
		}
	}
}

func TestAStaticTargetsPathOfAnotherEndpointIsACommandLineError(t *testing.T) {
	for _, args := range [][]string{
		{"--web.static-targets-path=/probe"},
		{"--web.static-targets-path=/self-metrics"},
		{"--web.self-metrics-path=/metrics", "--web.static-targets-path=/metrics"},
		{"--dry-run", "--config.file=configs/config.example.yaml", "--web.static-targets-path=/x/"},
	} {
		out := runCLI(t, args...)
		if out.code != 2 || !strings.Contains(out.stderr, "--web.static-targets-path") || out.stdout != "" {
			t.Fatalf("%v: exit=%d stderr=%s stdout=%s", args, out.code, out.stderr, out.stdout)
		}
	}
}

func TestStaticTargetsFileSchemaFlagPrintsTheSchema(t *testing.T) {
	out := runCLI(t, "--static-targets-file-schema")
	generated, _ := config.StaticTargetsSchemaJSON()
	if out.code != 0 || out.stdout != string(generated) || out.stderr != "" {
		t.Fatalf("exit=%d stderr=%q stdout starts %q", out.code, out.stderr, testutil.FirstLines(out.stdout, 3))
	}
}
