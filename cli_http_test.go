//go:build !select_request_types || request_type_http

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func (r checkRun) has(check string) bool {
	for _, result := range r.report.Checks {
		if result.Check == check {
			return true
		}
	}
	return false
}

// Only the steps that apply are reported: no targets check without a targets
// file, and no watch check without --config.watch.
func TestCheckReportsOnlyTheStepsThatApply(t *testing.T) {
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig))
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
	if out.has("static_targets") || out.has("config_watch") {
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

// --dry-run lists every mistake of a configuration as an error of its own.
func TestCheckListsEveryConfigurationMistake(t *testing.T) {
	broken := strings.Replace(testutil.MinimalConfig, "expression: 'value=(\\d+)'", "expression: 'value=\\d+'\n      - name: bad-name\n        expression: 'x=(\\d+)'", 1)
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", broken))
	conf := out.result(t, "config")
	if out.code != 1 || len(conf.Errors) != 2 || !strings.Contains(conf.Errors[0], "has no capture group") || !strings.Contains(conf.Errors[1], `metric "bad-name"`) {
		t.Fatalf("exit=%d config=%+v", out.code, conf)
	}
}

// --dry-run refuses a rule whose literal name is longer than
// limits.max_metric_name_length, as startup does, where it passed and every
// scrape failed; the same rule at the limit passes.
func TestCheckRefusesARuleNameOverTheMetricNameLimit(t *testing.T) {
	for length, code := range map[int]int{201: 1, 200: 0} {
		name := strings.Repeat("m", length)
		conf := strings.Replace(testutil.MinimalConfig, "name: demo_value", "name: "+name, 1)
		if conf == testutil.MinimalConfig {
			t.Fatal("the minimal configuration has no rule named demo_value")
		}
		out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", conf))
		if out.code != code {
			t.Fatalf("a %d-byte name: exit=%d, want %d\n%s", length, out.code, code, out.stdout)
		}
		if code == 0 {
			continue
		}
		if errs := out.result(t, "config").Errors; len(errs) != 1 || !strings.Contains(errs[0], fmt.Sprintf("metric %q is 201 bytes, longer than limits.max_metric_name_length 200", name)) {
			t.Fatalf("a %d-byte name: errors %q", length, errs)
		}
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

// A Python fault of a collector that a collector file defines names that
// file, in the check's error and in what startup logs before it exits: it
// named the collector alone, under the configuration that only lists the file.
func TestStartupAndDryRunNameTheCollectorFileOfAPythonFault(t *testing.T) {
	dir := t.TempDir()
	file := testutil.WriteIn(t, dir, "collectors.d/scripted.yaml", `collectors:
  - name: scripted
    request:
      type: http
    transform:
      type: jq
      pre_script: |
        result = 1
    metrics:
      - name: scripted_value
        expression: .value
`)
	conf := testutil.WriteIn(t, dir, "config.yaml", "collector_files: ['collectors.d/*.yaml']\n"+testutil.MinimalConfig)
	want := "collector file " + file + ": collector scripted pre_script must produce its result in a variable named 'data'"
	check := runCheckCLI(t, "--config.file="+conf)
	python := check.result(t, "python_scripts")
	if check.code != 1 || python.Status != checkFailed || len(python.Errors) != 1 || !strings.HasPrefix(python.Errors[0], want) {
		t.Fatalf("--dry-run exit=%d python=%+v, want the error %q", check.code, python, want)
	}
	start := runCLI(t, "--config.file="+conf)
	if start.code != 1 || len(start.logs) == 0 {
		t.Fatalf("startup exit=%d stderr=%s", start.code, start.stderr)
	}
	if last := start.logs[len(start.logs)-1]; last["msg"] != "invalid startup configuration; exiting" || !strings.HasPrefix(fmt.Sprint(last["error"]), want) {
		t.Fatalf("startup logged %v, want the error %q", last, want)
	}
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

// --config.expand-env changes what is loaded, so it changes the verdict: a
// check run where the variables are not set fails, as startup would.
func TestCheckHonoursEnvironmentExpansion(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", strings.Replace(testutil.MinimalConfig, "path: /status", "path: ${CHECK_DEMO_PATH}", 1))
	if literal := runCheckCLI(t, conf); literal.code != 0 {
		t.Fatalf("without the flag the reference is literal text: exit=%d\n%s", literal.code, literal.stdout)
	}
	os.Unsetenv("CHECK_DEMO_PATH")
	missing := runCheckCLI(t, conf, "--config.expand-env")
	if missing.code != 1 || !strings.Contains(missing.result(t, "config").Errors[0], "CHECK_DEMO_PATH") {
		t.Fatalf("exit=%d\n%s", missing.code, missing.stdout)
	}
	t.Setenv("CHECK_DEMO_PATH", "/status")
	if set := runCheckCLI(t, conf, "--config.expand-env"); set.code != 0 || set.result(t, "config").Details["config_expand_env"] != true {
		t.Fatalf("exit=%d\n%s", set.code, set.stdout)
	}
}

// The static target file has its own flag, --static-targets.expand-env, and
// --config.expand-env does not reach it: each file is expanded only when its
// own flag says so.
func TestCheckExpandsTheStaticTargetFileByItsOwnFlag(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", strings.Replace(testutil.MinimalConfig, "path: /status", "path: ${CHECK_DEMO_PATH}", 1))
	targets := "--static-targets-file=" + testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: one\n    collector: demo\n    target: ${CHECK_DEMO_TARGET}\n")
	t.Setenv("CHECK_DEMO_PATH", "/status")
	t.Setenv("CHECK_DEMO_TARGET", "http://api.example:8080")

	// The configuration's flag leaves the target a literal reference, which
	// is not an absolute URL.
	configOnly := runCheckCLI(t, conf, targets, "--config.expand-env")
	if configOnly.code != 1 || configOnly.result(t, "config").Details["config_expand_env"] != true || configOnly.result(t, "static_targets").Status != checkFailed {
		t.Fatalf("--config.expand-env alone: exit=%d\n%s", configOnly.code, configOnly.stdout)
	}
	// The file's own flag expands it, and leaves the configuration literal.
	targetsOnly := runCheckCLI(t, conf, targets, "--static-targets.expand-env")
	if targetsOnly.code != 0 || targetsOnly.result(t, "static_targets").Details["static_targets_expand_env"] != true || targetsOnly.result(t, "config").Details["config_expand_env"] != false {
		t.Fatalf("--static-targets.expand-env alone: exit=%d\n%s", targetsOnly.code, targetsOnly.stdout)
	}
	if both := runCheckCLI(t, conf, targets, "--config.expand-env", "--static-targets.expand-env"); both.code != 0 {
		t.Fatalf("both flags: exit=%d\n%s", both.code, both.stdout)
	}
	// A variable the file needs and the environment lacks is named, with
	// the file's own flag.
	os.Unsetenv("CHECK_DEMO_TARGET")
	missing := runCheckCLI(t, conf, targets, "--static-targets.expand-env")
	if missing.code != 1 || !strings.Contains(missing.result(t, "static_targets").Errors[0], `"CHECK_DEMO_TARGET" not set; --static-targets.expand-env requires`) {
		t.Fatalf("exit=%d\n%s", missing.code, missing.stdout)
	}
}

// An argument that is not a flag is a command-line error, at a start and at a
// --dry-run alike. The flag package stops reading at one and leaves the flags
// after it unread, so "--config.watch true --static-targets-file=..." would
// otherwise start an exporter without its static targets, and without a word.
func TestAnArgumentThatIsNotAFlagIsACommandLineError(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig)
	// A start that got past its command line would end on this file, with
	// status 1, rather than serve.
	missing := "--config.file=/nonexistent/config.yaml"
	for argument, args := range map[string][]string{
		"true":         {missing, "--config.watch", "true", "--static-targets-file=/nonexistent/targets.yaml"},
		"false":        {"--dry-run", conf, "--config.expand-env", "false"},
		"targets.yaml": {"--dry-run", conf, "targets.yaml"},
		"":             {missing, ""},
	} {
		out := runCLI(t, args...)
		want := fmt.Sprintf("unexpected argument %q; flags take --name=value", argument)
		if out.code != 2 || out.stdout != "" || len(out.logs) != 1 || out.logs[0]["error"] != want || out.logs[0]["msg"] != "invalid command line; exiting" {
			t.Errorf("%q: exit=%d stdout=%q logs=%v, want status 2 and the one error %q", args, out.code, out.stdout, out.logs, want)
		}
	}
	// A flag that takes a value still takes it from the next argument.
	if out := runCheckCLI(t, "--config.file", testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig), "--log.level", "debug"); out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stderr)
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

// --dry-run takes a size written as a whole number with an exponent, as
// startup does and as the schema an editor checks the file against does:
// max_response_bytes: 1e6 is a megabyte, where the check once failed a file
// the editor showed as valid. A number that is no whole number of bytes
// still fails it, at its line.
func TestDryRunTakesASizeWrittenWithAnExponent(t *testing.T) {
	sized := func(size string) string {
		return testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: sized\n    request:\n      type: http\n      max_response_bytes: "+size+"\n    transform: {type: regex}\n    limits: {max_output_bytes: "+size+"}\n    metrics:\n      - name: v\n        expression: 'v=(\\d+)'\n")
	}
	for _, size := range []string{"1e6", "1000000.0", "2.5e6"} {
		out := runCheckCLI(t, "--config.file="+sized(size))
		if result := out.result(t, "config"); out.code != 0 || out.report.Status != checkOK || result.Status != checkOK || len(result.Errors) != 0 {
			t.Errorf("sizes written %s: exit=%d config=%+v", size, out.code, result)
		}
	}
	out := runCheckCLI(t, "--config.file="+sized("1e-1"))
	result := out.result(t, "config")
	if out.code != 1 || result.Status != checkFailed || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], `line 5: size "1e-1" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB; line 7: size "1e-1" is not`) {
		t.Fatalf("sizes written 1e-1: exit=%d config=%+v", out.code, result)
	}
}

// --dry-run refuses an output limit under the least, which no answer of a
// transform's script fits, as startup does, with the collector, the key, the
// least and the default in its report, where the check once passed a
// configuration whose every scrape then failed; the least itself, 38 bytes,
// passes.
func TestDryRunReportsAnOutputLimitUnderTheLeast(t *testing.T) {
	limited := func(size string) string {
		return testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: scripted\n    request: {type: http}\n    transform: {type: python, script: \"metric('m', 1)\"}\n    limits: {max_output_bytes: "+size+"}\n")
	}
	for _, size := range []string{"26", "37", "20B"} {
		out := runCheckCLI(t, "--config.file="+limited(size))
		result := out.result(t, "config")
		if want := `collector "scripted" limits.max_output_bytes is ` + strings.TrimSuffix(size, "B") + `, and a Python script that emits no metric answers in 38 bytes, so no transform's script could answer within it; set at least 38, or leave it out, or 0, for the default, 1MiB`; out.code != 1 || out.report.Status != checkFailed || result.Status != checkFailed || len(result.Errors) != 1 || result.Errors[0] != want {
			t.Errorf("max_output_bytes: %s: exit=%d config=%+v, want %s", size, out.code, result, want)
		}
	}
	out := runCheckCLI(t, "--config.file="+limited("38"))
	if result := out.result(t, "config"); out.code != 0 || out.report.Status != checkOK || result.Status != checkOK || len(result.Errors) != 0 {
		t.Errorf("max_output_bytes: 38: exit=%d config=%+v", out.code, result)
	}
}

// --dry-run refuses a label value with a {{param_...}} placeholder that the
// "*" entry of a value_map of its rule's name maps to a text longer than
// limits.max_label_value_length, as startup does, where the check once
// passed a configuration whose every probe giving a value the map does not
// list then failed its scrape; with truncate: true on the label it passes.
func TestDryRunRefusesATemplatedLabelMappedTooLongByAnyValue(t *testing.T) {
	mapped := func(label string) string {
		return testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: tenants\n    request: {type: http}\n    limits: {max_label_value_length: 8}\n    transform: {type: jq}\n    metrics:\n      - name: m\n        expression: .x\n        labels:\n          - {name: tenant, value: \"{{param_tenant}}\""+label+"}\n      - name: m\n        expression: .y\n        labels:\n          - {name: tenant, expression: .t, value_map: {\"*\": overlongvalue}}\n")
	}
	out := runCheckCLI(t, "--config.file="+mapped(""))
	result := out.result(t, "config")
	if want := `collector "tenants" metric "m" label "tenant" value holds {{param_...}} placeholders, and the value_map of the metric's name maps every value it does not list, by its "*" entry, to 13 bytes, longer than limits.max_label_value_length 8, so every series of a probe that gives a value not listed would fail validation; shorten the "*" entry or take it out, set truncate: true on the label, or raise the limit`; out.code != 1 || result.Status != checkFailed || len(result.Errors) != 1 || !strings.HasSuffix(result.Errors[0], want) {
		t.Errorf("exit=%d config=%+v, want the error %q", out.code, result, want)
	}
	out = runCheckCLI(t, "--config.file="+mapped(", truncate: true"))
	if result := out.result(t, "config"); out.code != 0 || out.report.Status != checkOK || result.Status != checkOK || len(result.Errors) != 0 {
		t.Errorf("with truncate: true: exit=%d config=%+v", out.code, result)
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

// Startup reads the static target file with its own flag, as --dry-run does:
// the configuration's flag leaves a reference in it literal, and the file's
// own flag expands it, refusing to start without the variable.
func TestStartupExpandsTheStaticTargetFileByItsOwnFlag(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig)
	targets := "--static-targets-file=" + testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: one\n    collector: demo\n    target: ${STARTUP_DEMO_TARGET}\n")
	os.Unsetenv("STARTUP_DEMO_TARGET")
	for flag, want := range map[string]string{
		"--config.expand-env":         "must have an absolute target URL",
		"--static-targets.expand-env": `STARTUP_DEMO_TARGET\" not set; --static-targets.expand-env requires`,
	} {
		out := runCLI(t, conf, targets, flag, "--web.listen-address=127.0.0.1:0")
		if out.code != 1 || !strings.Contains(out.stderr, "invalid static target configuration") || !strings.Contains(out.stderr, want) {
			t.Errorf("%s: exit=%d, want 1 with %q:\n%s", flag, out.code, want, out.stderr)
		}
	}
}

// --log.level takes debug, info, warn or error, in any case. Anything else is
// a malformed command line, refused before --dry-run or startup, rather than
// quietly logging at info.
func TestTheLogLevelIsChecked(t *testing.T) {
	conf := "--config.file=" + testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig)
	for _, level := range []string{"debgu", "warning", "", "trace"} {
		for _, args := range [][]string{{conf, "--log.level=" + level}, {"--dry-run", conf, "--log.level=" + level}} {
			out := runCLI(t, args...)
			if out.code != 2 || !strings.Contains(out.stderr, `is not a level; use debug, info, warn or error`) {
				t.Errorf("%v: exit=%d, want 2 naming the levels:\n%s", args, out.code, out.stderr)
			}
		}
	}
	for _, level := range []string{"debug", "INFO", "Warn", "error"} {
		if out := runCheckCLI(t, conf, "--log.level="+level); out.code != 0 {
			t.Errorf("--log.level=%s: exit=%d\n%s", level, out.code, out.stderr)
		}
	}
	// WARN is warn: the check's info lines are not logged, its warnings are.
	quiet := runCheckCLI(t, conf, "--log.level=WARN")
	if strings.Contains(quiet.stderr, `"level":"INFO"`) {
		t.Errorf("--log.level=WARN logged at info:\n%s", quiet.stderr)
	}
}

// --dry-run lists what each collector's request lets through, as it will be
// applied: its target lists and the statuses it decodes, normalised.
func TestDryRunReportsRequestPolicies(t *testing.T) {
	config := testutil.WriteIn(t, t.TempDir(), "config.yaml", `collectors:
  - name: guarded
    request:
      type: http
      allowed_targets: ["*.example.com"]
      denied_targets: [169.254.169.254]
      accept_status: ["2XX", 503]
    transform: {type: regex}
    metrics: [{name: v, expression: 'v=(\d+)'}]
  - name: open
    request: {type: http}
    transform: {type: regex}
    metrics: [{name: w, expression: 'w=(\d+)'}]
`)
	check := runCheckCLI(t, "--config.file="+config)
	policies, _ := check.result(t, "config").Details["request_policies"].(map[string]any)
	guarded, _ := policies["guarded"].(map[string]any)
	if check.code != 0 || len(policies) != 1 || !reflect.DeepEqual(guarded["accept_status"], []any{"2xx", "503"}) || !reflect.DeepEqual(guarded["denied_targets"], []any{"169.254.169.254"}) {
		t.Fatalf("exit=%d\n%s", check.code, check.stdout)
	}
}

// --dry-run takes a status of request.accept_status written as a number with
// a point, an exponent or in hex, as startup does and as the schema an editor
// checks the file against does, and lists it as the status it is; a number
// that is no status still fails it, naming the collector and the entry.
func TestDryRunTakesAStatusWrittenAsANumberOfAnySpelling(t *testing.T) {
	accepting := func(list string) string {
		return testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: tolerant\n    request:\n      type: http\n      accept_status: ["+list+"]\n    transform: {type: regex}\n    metrics: [{name: v, expression: 'v=(\\d+)'}]\n")
	}
	check := runCheckCLI(t, "--config.file="+accepting("503.0, 4.04e2, 0x1F4, 2xx"))
	policies, _ := check.result(t, "config").Details["request_policies"].(map[string]any)
	tolerant, _ := policies["tolerant"].(map[string]any)
	if check.code != 0 || !reflect.DeepEqual(tolerant["accept_status"], []any{"503", "404", "500", "2xx"}) {
		t.Fatalf("exit=%d\n%s", check.code, check.stdout)
	}
	out := runCheckCLI(t, "--config.file="+accepting("503.5"))
	if result := out.result(t, "config"); out.code != 1 || result.Status != checkFailed || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], `collector "tolerant" request.accept_status entry "503.5" is not an HTTP status from 100 to 599 or a class such as 2xx`) {
		t.Fatalf("accept_status: [503.5]: exit=%d config=%+v", out.code, result)
	}
}

// --dry-run and startup refuse alike a label whose expression is nothing but
// blanks, beside a value and without one: a csv collector with such a label
// passed the dry run and started, and read the column of that name where the
// label was meant to be the constant. The report names the collector, the
// metric and the label of each, as an error of its own.
func TestDryRunReportsALabelExpressionOfBlanks(t *testing.T) {
	config := testutil.WriteFile(t, "config.yaml", `collectors:
  - name: racks
    request:
      type: http
    transform:
      type: csv
    metrics:
      - name: cpu
        expression: cpu
        labels:
          - name: site
            value: x
            expression: "  "
      - name: memory
        expression: memory
        labels:
          - name: zone
            expression: " "
`)
	check := runCheckCLI(t, "--config.file="+config)
	result := check.result(t, "config")
	if check.code != 1 || result.Status != checkFailed || len(result.Errors) != 2 ||
		!strings.HasSuffix(result.Errors[0], `collector "racks" metric "cpu" label "site" expression "  " is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant`) ||
		!strings.HasSuffix(result.Errors[1], `collector "racks" metric "memory" label "zone" expression " " is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant`) {
		t.Fatalf("exit=%d config=%+v", check.code, result)
	}
	if start := runCLI(t, "--config.file="+config); start.code != 1 || !strings.Contains(start.stderr, `label \"site\" expression \"  \" is nothing but blanks`) {
		t.Fatalf("startup exit=%d\n%s", start.code, start.stderr)
	}
}
