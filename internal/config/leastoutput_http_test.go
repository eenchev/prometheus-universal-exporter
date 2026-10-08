//go:build !select_request_types || request_type_http

package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// outputTooSmall is what a limits.max_output_bytes of size bytes that no
// answer of a Python worker fits is refused with, in the collector named.
func outputTooSmall(collector string, size int) string {
	return fmt.Sprintf(`collector %q limits.max_output_bytes is %d, and a Python script that emits no metric answers in 38 bytes, so no transform's script could answer within it; set at least 38, or leave it out, or 0, for the default, 1MiB`, collector, size)
}

// A limits.max_output_bytes under the 38 bytes a Python script that emits no
// metric answers in is refused when the configuration loads, where it once
// loaded and failed every scrape of a collector whose transform is a script
// as output over the limit, or, under 27 bytes, as an interpreter that did
// not start. The refusal names the
// collector, the key, the bytes the value comes to, the least and the
// default, however the size is written: as a number, with an exponent or a
// fraction of zero, in quotes, and with a unit. 38 bytes and more load as
// what they are, and 0, however it is written, and the key left out are the
// default, 1 MiB, as they were.
func TestAnOutputLimitUnderTheLeastIsRefusedWhenTheConfigurationLoads(t *testing.T) {
	const least = transform.MinPythonOutputBytes
	if _, err := loadChecked(t, "", fmt.Sprintf("    limits: {max_output_bytes: %d}\n", least-1), ""); err == nil || err.Error() != outputTooSmall("a", least-1) {
		t.Errorf("a byte under the least: %v, want %s", err, outputTooSmall("a", least-1))
	}
	for written, size := range map[string]int{
		"1": 1, "26": 26, "2.6e1": 26, "26.0": 26, "+26": 26, "0x1a": 26, "'26'": 26, "26B": 26, "26 b": 26, "0.02KiB": 20, "0.000026MB": 26,
		"27": 27, "37": 37, "3.7e1": 37, "37.0": 37, `"37"`: 37, "37B": 37, "37.9B": 37,
	} {
		if _, err := loadChecked(t, "", "    limits: {max_output_bytes: "+written+"}\n", ""); err == nil || err.Error() != outputTooSmall("a", size) {
			t.Errorf("max_output_bytes: %s: %v, want %s", written, err, outputTooSmall("a", size))
		}
	}
	for written, want := range map[string]model.ByteSize{
		"38": 38, "3.8e1": 38, "38.0": 38, "'38'": 38, "38B": 38, "38.9B": 38, "0.04KiB": 40, "39": 39, "1KiB": 1 << 10, "64MiB": 64 << 20,
		"0": 1 << 20, "0.0": 1 << 20, "0e0": 1 << 20, "'0'": 1 << 20, "0B": 1 << 20, "0.0001KiB": 1 << 20,
	} {
		cfg, err := loadChecked(t, "", "    limits: {max_output_bytes: "+written+"}\n", "")
		if err != nil || cfg.Collectors[0].Limits.MaxOutputBytes != want {
			t.Errorf("max_output_bytes: %s: %v, want it loaded as %d bytes", written, err, want)
		}
	}
	if cfg, err := loadChecked(t, "", "", ""); err != nil || cfg.Collectors[0].Limits.MaxOutputBytes != 1<<20 {
		t.Errorf("max_output_bytes left out: %v, want the default", err)
	}
}

// The least is of the key, whatever the collector is: one whose transform is
// a script, one with a pre-script and one that runs no Python at all are
// refused alike, each by its name, in one load; and a collector built
// without a file is held to it by Validate, which takes the least itself.
func TestAnOutputLimitUnderTheLeastIsRefusedWithAScriptAndWithoutOne(t *testing.T) {
	const limits = "    limits: {max_output_bytes: 30}\n"
	document := "collectors:\n" +
		"  - name: scripted\n    request: {type: http}\n    transform: {type: python, script: \"metric('m', 1)\"}\n" + limits +
		"  - name: prepared\n    request: {type: http}\n    decoder: {type: json}\n    transform: {type: jq, pre_script: \"data = {}\"}\n" + limits + "    metrics:\n      - name: m\n        expression: .x\n" +
		"  - name: plain\n    request: {type: http}\n    transform: {type: regex}\n" + limits + "    metrics:\n      - name: m\n        expression: 'v=(\\d+)'\n"
	_, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if want := outputTooSmall("scripted", 30) + "\n" + outputTooSmall("prepared", 30) + "\n" + outputTooSmall("plain", 30); err == nil || err.Error() != want {
		t.Fatalf("three collectors with an output limit of 30 bytes: %v, want\n%s", err, want)
	}
	if _, err := Load(testutil.WriteFile(t, "config.yaml", strings.ReplaceAll(document, "max_output_bytes: 30", "max_output_bytes: 38"))); err != nil {
		t.Fatalf("the same collectors with an output limit of 38 bytes: %v", err)
	}
	built := func(size model.ByteSize) *model.Config {
		cfg, err := loadChecked(t, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Collectors[0].Limits.MaxOutputBytes = size
		return cfg
	}
	if err := Validate(built(37)); err == nil || err.Error() != outputTooSmall("a", 37) {
		t.Errorf("a collector built with an output limit of 37 bytes: %v", err)
	}
	if cfg := built(38); Validate(cfg) != nil || cfg.Collectors[0].Limits.MaxOutputBytes != 38 {
		t.Errorf("a collector built with an output limit of 38 bytes is not taken as it is: %+v", cfg.Collectors[0].Limits)
	}
}

// A collector file and a YAML anchor are nothing special: the limit of a
// collector in a collector file is refused naming that file, and one a
// collector takes from an anchor is refused of the collector that takes it.
func TestAnOutputLimitUnderTheLeastIsRefusedInACollectorFileAndFromAnAnchor(t *testing.T) {
	dir := t.TempDir()
	main := testutil.WriteIn(t, dir, "config.yaml", "collector_files: ['collectors.d/*.yaml']\n")
	file := testutil.WriteIn(t, dir, "collectors.d/payments.yaml", strings.Replace(payCollector, "    decoder:", "    limits: {max_output_bytes: 20B}\n    decoder:", 1))
	if _, err := Load(main); err == nil || err.Error() != "collector file "+file+": "+outputTooSmall("pay", 20) {
		t.Errorf("in a collector file: %v", err)
	}
	anchored := "x-limits: &limits {max_output_bytes: 26, max_metrics: 5}\n" + strings.Replace(payCollector, "    decoder:", "    limits: *limits\n    decoder:", 1)
	if _, err := Load(testutil.WriteFile(t, "config.yaml", anchored)); err == nil || err.Error() != outputTooSmall("pay", 26) {
		t.Errorf("from an anchor: %v", err)
	}
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(anchored, "max_output_bytes: 26", "max_output_bytes: 38", 1)))
	if err != nil || cfg.Collectors[0].Limits.MaxOutputBytes != 38 || cfg.Collectors[0].Limits.MaxMetrics != 5 {
		t.Errorf("38 bytes from an anchor: %v", err)
	}
}

// A reload that sets an output limit under the least is rejected like any
// other: it says why, its one log line names the file, and the
// configuration that was in force stays in force, with the limit it had.
func TestAReloadThatSetsAnOutputLimitUnderTheLeastIsRejected(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", payCollector)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := NewManager(cfg, path, slog.New(slog.NewJSONHandler(&logs, nil)))
	testutil.WriteIn(t, strings.TrimSuffix(path, "config.yaml"), "config.yaml", strings.Replace(payCollector, "    decoder:", "    limits: {max_output_bytes: 37}\n    decoder:", 1))
	line, err := rejectedReload(t, m, &logs)
	if !strings.Contains(err.Error(), outputTooSmall("pay", 37)) || line["file"] != path || !strings.Contains(line["error"].(string), outputTooSmall("pay", 37)) {
		t.Fatalf("the reload: error %v, logged file %v error %v", err, line["file"], line["error"])
	}
	if m.Get() != cfg || m.Get().Collectors[0].Limits.MaxOutputBytes != 1<<20 {
		t.Fatalf("after a rejected reload the configuration in force is not the one that was: %+v", m.Get().Collectors[0].Limits)
	}
	testutil.WriteIn(t, strings.TrimSuffix(path, "config.yaml"), "config.yaml", strings.Replace(payCollector, "    decoder:", "    limits: {max_output_bytes: 38}\n    decoder:", 1))
	if err := m.Reload(ReloadTriggerSignal); err != nil || m.Get().Collectors[0].Limits.MaxOutputBytes != 38 {
		t.Fatalf("a reload that sets 38 bytes: %v, in force %+v", err, m.Get().Collectors[0].Limits)
	}
}
