//go:build !select_request_types || request_type_http

package config

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A mistake validation finds in a collector is reported against the file
// that defines it: a collector file's own, not the configuration that only
// lists it.

const payCollector = "collectors:\n  - name: pay\n    request: {type: http}\n    decoder: {type: json}\n    transform: {type: jq}\n    metrics:\n      - name: depth\n        expression: .queue.depth\n"

// rejectedReload reloads and returns the error and the one line the refusal
// logged.
func rejectedReload(t *testing.T, m *Manager, logs *bytes.Buffer) (map[string]any, error) {
	t.Helper()
	logs.Reset()
	err := m.Reload(ReloadTriggerSignal)
	if err == nil {
		t.Fatal("the reload was accepted")
	}
	return testutil.AssertJSONLines(t, logs, 1)[0], err
}

// A validation error of a collector in a collector file names the file in
// its text, at startup and on reload, and the reload's log line carries it
// as file; a YAML error of a collector file does the same.
func TestAValidationErrorNamesTheCollectorFile(t *testing.T) {
	dir := t.TempDir()
	main := testutil.WriteIn(t, dir, "config.yaml", "collector_files: ['collectors.d/*.yaml']\n")
	file := testutil.WriteIn(t, dir, "collectors.d/payments.yaml", payCollector)
	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := NewManager(cfg, main, slog.New(slog.NewJSONHandler(&logs, nil)))

	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", strings.Replace(payCollector, ".queue.depth", ".queue.depth[", 1))
	wantText := "collector file " + file + `: collector "pay" metric "depth" expression ".queue.depth["`
	if _, err := Load(main); err == nil || !strings.HasPrefix(err.Error(), wantText) {
		t.Fatalf("startup: %v, want %q", err, wantText)
	}
	line, err := rejectedReload(t, m, &logs)
	if !strings.Contains(err.Error(), wantText) || line["file"] != file || !strings.Contains(line["error"].(string), wantText) {
		t.Fatalf("reload: error %v, logged file %v error %v; want the collector file %s", err, line["file"], line["error"], file)
	}

	// Several mistakes in the one file are each named, and still logged
	// against it.
	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", strings.Replace(payCollector, ".queue.depth\n", ".queue.depth[\n      - name: age\n        expression: .queue.age[\n", 1))
	line, err = rejectedReload(t, m, &logs)
	if strings.Count(err.Error(), "collector file "+file+": ") != 2 || line["file"] != file {
		t.Fatalf("two mistakes: error %v, logged file %v", err, line["file"])
	}

	// A file that is not YAML the exporter can read names itself too.
	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", strings.Replace(payCollector, "    request:", "    requst:", 1))
	line, err = rejectedReload(t, m, &logs)
	if !strings.Contains(err.Error(), "collector file "+file+`: line 3: unknown key "requst"`) || line["file"] != file {
		t.Fatalf("a YAML error: error %v, logged file %v", err, line["file"])
	}
}

// Mistakes in more than one file are logged against the configuration, with
// each collector file named in the text of its own; a mistake in the
// configuration's own collector names no collector file.
func TestMistakesInSeveralFilesAreLoggedAgainstTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	own := strings.Replace(strings.Replace(payCollector, "name: pay", "name: own", 1), "collectors:\n", "collector_files: ['collectors.d/*.yaml']\ncollectors:\n", 1)
	main := testutil.WriteIn(t, dir, "config.yaml", own)
	file := testutil.WriteIn(t, dir, "collectors.d/payments.yaml", payCollector)
	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := NewManager(cfg, main, slog.New(slog.NewJSONHandler(&logs, nil)))

	testutil.WriteIn(t, dir, "config.yaml", strings.Replace(own, ".queue.depth", ".queue.depth[", 1))
	line, err := rejectedReload(t, m, &logs)
	if strings.Contains(err.Error(), "collector file") || !strings.Contains(err.Error(), `collector "own" metric "depth"`) || line["file"] != main {
		t.Fatalf("the configuration's own collector: error %v, logged file %v", err, line["file"])
	}

	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", strings.Replace(payCollector, ".queue.depth", ".queue.depth[", 1))
	line, err = rejectedReload(t, m, &logs)
	if strings.Count(err.Error(), "collector file") != 1 || !strings.Contains(err.Error(), "collector file "+file+`: collector "pay"`) || line["file"] != main {
		t.Fatalf("both files: error %v, logged file %v", err, line["file"])
	}

	// Two collector files, each with a mistake: neither alone is the file.
	testutil.WriteIn(t, dir, "config.yaml", own)
	other := testutil.WriteIn(t, dir, "collectors.d/billing.yaml", strings.Replace(strings.Replace(payCollector, "name: pay", "name: bill", 1), ".queue.depth", ".queue.depth[", 1))
	line, err = rejectedReload(t, m, &logs)
	if !strings.Contains(err.Error(), "collector file "+file+": ") || !strings.Contains(err.Error(), "collector file "+other+": ") || line["file"] != main {
		t.Fatalf("two collector files: error %v, logged file %v", err, line["file"])
	}
	if filepath.Dir(other) != filepath.Dir(file) {
		t.Fatal("the test's files are not side by side")
	}
}

// A collector with a pre_script that reads data and never produces it, which
// the interpreter's check refuses.
const scriptedCollector = "  - name: %s\n    request: {type: http}\n    decoder: {type: json}\n    transform:\n      type: jq\n      pre_script: |\n        %s\n    metrics:\n      - name: depth\n        expression: .queue.depth\n"

func scripted(name, script string) string {
	return strings.Replace(strings.Replace(scriptedCollector, "%s", name, 1), "%s", script, 1)
}

// A problem the interpreter finds with the Python of a collector in a
// collector file names that file, as a validation error does, at startup and
// on reload, and the reload's log line carries it as file. It was reported
// against the configuration that only lists the file, with nothing saying
// where the collector was.
func TestAPythonProblemNamesTheCollectorFile(t *testing.T) {
	dir := t.TempDir()
	main := testutil.WriteIn(t, dir, "config.yaml", "collector_files: ['collectors.d/*.yaml']\n")
	file := testutil.WriteIn(t, dir, "collectors.d/payments.yaml", "collectors:\n"+scripted("pay", "data = data"))
	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePythonScripts("python3", cfg); err != nil {
		t.Fatalf("a sound script: %v", err)
	}
	var logs bytes.Buffer
	m := NewManager(cfg, main, slog.New(slog.NewJSONHandler(&logs, nil)))
	m.SetPythonPath("python3")

	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", "collectors:\n"+scripted("pay", "print(data)"))
	wantText := "collector file " + file + ": collector pay pre_script must produce its result in a variable named 'data'"
	cfg, err = Load(main)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePythonScripts("python3", cfg); err == nil || !strings.HasPrefix(err.Error(), wantText) {
		t.Fatalf("startup: %v, want %q", err, wantText)
	}
	line, err := rejectedReload(t, m, &logs)
	if !strings.Contains(err.Error(), wantText) || line["file"] != file || !strings.HasPrefix(line["error"].(string), wantText) {
		t.Fatalf("reload: error %v, logged file %v error %v; want the collector file %s", err, line["file"], line["error"], file)
	}

	// Two scripts of the one file are each named, and logged against it; a
	// syntax error is named the same way.
	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", "collectors:\n"+scripted("pay", "print(data)")+scripted("refunds", "data = ("))
	line, err = rejectedReload(t, m, &logs)
	if strings.Count(err.Error(), "collector file "+file+": ") != 2 || !strings.Contains(err.Error(), "collector file "+file+": collector refunds pre_script has a Python syntax error on line 1") || line["file"] != file {
		t.Fatalf("two scripts: error %v, logged file %v", err, line["file"])
	}
}

// Python problems in more than one file are logged against the
// configuration, each naming its collector file in its text; one in the
// configuration's own collector names no collector file. The problems come
// in the order of the collectors.
func TestPythonProblemsInSeveralFilesAreLoggedAgainstTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	own := func(script string) string {
		return "collector_files: ['collectors.d/*.yaml']\ncollectors:\n" + scripted("own", script)
	}
	main := testutil.WriteIn(t, dir, "config.yaml", own("data = data"))
	billing := testutil.WriteIn(t, dir, "collectors.d/billing.yaml", "collectors:\n"+scripted("bill", "data = data"))
	payments := testutil.WriteIn(t, dir, "collectors.d/payments.yaml", "collectors:\n"+scripted("pay", "data = data"))
	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := NewManager(cfg, main, slog.New(slog.NewJSONHandler(&logs, nil)))
	m.SetPythonPath("python3")

	testutil.WriteIn(t, dir, "config.yaml", own("print(data)"))
	line, err := rejectedReload(t, m, &logs)
	if strings.Contains(err.Error(), "collector file") || !strings.Contains(err.Error(), "collector own pre_script must produce") || line["file"] != main {
		t.Fatalf("the configuration's own collector: error %v, logged file %v", err, line["file"])
	}

	testutil.WriteIn(t, dir, "collectors.d/payments.yaml", "collectors:\n"+scripted("pay", "print(data)"))
	line, err = rejectedReload(t, m, &logs)
	if strings.Count(err.Error(), "collector file") != 1 || !strings.Contains(err.Error(), "collector file "+payments+": collector pay pre_script") || line["file"] != main {
		t.Fatalf("the configuration and a collector file: error %v, logged file %v", err, line["file"])
	}

	testutil.WriteIn(t, dir, "config.yaml", own("data = data"))
	testutil.WriteIn(t, dir, "collectors.d/billing.yaml", "collectors:\n"+scripted("bill", "data = ("))
	line, err = rejectedReload(t, m, &logs)
	text := line["error"].(string)
	first, second := strings.Index(text, "collector file "+billing+": collector bill pre_script has a Python syntax error"), strings.Index(text, "collector file "+payments+": collector pay pre_script must produce")
	if first != 0 || second < first || strings.Count(err.Error(), "collector file") != 2 || line["file"] != main {
		t.Fatalf("two collector files: error %v, logged file %v", err, line["file"])
	}

	// An interpreter that is not there is no problem of any file.
	problems, err := CheckPythonScripts("/nonexistent/python3", cfg)
	if err == nil || problems != nil || !strings.Contains(err.Error(), "needs a working interpreter") {
		t.Fatalf("no interpreter: problems %v, error %v", problems, err)
	}
}
