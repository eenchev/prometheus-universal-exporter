//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// targetsAcceptingStatuses is a static target file of one target of collector a whose
// request.accept_status is the list written.
func targetsAcceptingStatuses(list string) string {
	return "interval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {accept_status: [" + list + "]}\n"
}

// statusLoad is a list of request.accept_status as a load leaves it to be
// compared with a status, or the load's error.
type statusLoad struct {
	statuses []string
	err      error
}

// acceptStatusLoads loads the list written at request.accept_status of a
// collector in the configuration, in a collector file and of a static
// target, checked as the exporter checks each.
func acceptStatusLoads(t *testing.T, list string, opts ...LoadOption) map[string]statusLoad {
	t.Helper()
	loads := map[string]statusLoad{}
	document := checkedCollector("      accept_status: ["+list+"]\n", "", "")
	if cfg, err := Load(testutil.WriteFile(t, "config.yaml", document), opts...); err == nil {
		loads["the configuration"] = statusLoad{statuses: cfg.Collectors[0].Request.AcceptStatus}
	} else {
		loads["the configuration"] = statusLoad{err: err}
	}
	collectors := testutil.WriteIn(t, t.TempDir(), "collectors.yaml", document)
	if cfg, err := Load(testutil.WriteFile(t, "config.yaml", "collector_files: ["+collectors+"]\n"), opts...); err == nil {
		loads["a collector file"] = statusLoad{statuses: cfg.Collectors[0].Request.AcceptStatus}
	} else {
		loads["a collector file"] = statusLoad{err: err}
	}
	plain, err := Load(testutil.WriteFile(t, "config.yaml", checkedCollector("", "", "")))
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targetsAcceptingStatuses(list)), opts...)
	if err == nil {
		if err = ValidateStaticTargets(file); err == nil {
			err = ValidateStaticTargetsAgainst(file, plain)
		}
	}
	if err == nil {
		loads["the static target file"] = statusLoad{statuses: file.Targets[0].Request.AcceptStatus}
	} else {
		loads["the static target file"] = statusLoad{err: err}
	}
	return loads
}

// An entry of request.accept_status written as a number YAML reads as one is
// the status it is, however it is written — with a point, an exponent, in
// hex or octal, with an underscore, a sign or leading zeros — as it is to the
// schemas, which are handed the number: in the configuration, in a collector
// file and of a static target. The exporter was handed the number as it is
// written, and refused 200.0, 2e2 and 0xC8 as no status, and took +200 and
// 0200 at load as statuses that then matched none.
func TestAStatusWrittenAsANumberIsTheStatusItIsHoweverItIsWritten(t *testing.T) {
	for written, want := range map[string]string{
		"503": "503", "503.0": "503", "503.": "503", "5.03e2": "503", "5030e-1": "503", "0x1F7": "503", "0x1f7": "503", "0o767": "503",
		"5_03": "503", "+503": "503", "0503": "503", "+5.03e2": "503", "!!float 503": "503", "100.0": "100", "5.99e2": "599", "0b111110111": "503",
	} {
		for file, load := range acceptStatusLoads(t, written+", 2xx") {
			if load.err != nil || !slices.Equal(load.statuses, []string{want, "2xx"}) {
				t.Errorf("%s: accept_status: [%s, 2xx] is %q, %v; want [%s 2xx]", file, written, load.statuses, load.err, want)
			}
		}
	}
	// Text is held to what it was held to: a status's digits, with blanks
	// around them, or a class. Digits with a sign or leading zeros, which
	// the load took as a status, are the status by its digits alone, as
	// AcceptedStatus compares it, where they passed the load and matched no
	// status.
	for written, want := range map[string]string{`"503"`: "503", `'2XX'`: "2xx", `" 503 "`: "503", `'+503'`: "503", `"0503"`: "503", `" +0503 "`: "503"} {
		for file, load := range acceptStatusLoads(t, written) {
			if load.err != nil || !slices.Equal(load.statuses, []string{want}) {
				t.Errorf("%s: accept_status: [%s] is %q, %v; want [%s]", file, written, load.statuses, load.err, want)
			}
		}
	}
}

// What is no status stays refused, naming the collector or the target, the
// entry as it is written and what an entry is: a number with a fraction, out
// of the range of a status however it is written, a boolean, and a number
// with a point or an exponent in quotes, which is text that is no status, and
// quoted digits with a sign or leading zeros out of the range of a status.
func TestANumberThatIsNoStatusIsRefusedAtRequestAcceptStatus(t *testing.T) {
	for _, written := range []string{"503.5", "5.035e2", "6e2", "600.0", "0x258", "9.9e1", "-5.03e2", "1e19", "-200", "0700", "true", "'503.0'", `"5e2"`, "'0xc8'", ".inf", "'-503'", "'0700'"} {
		entry := strings.Trim(written, `'"`)
		for file, load := range acceptStatusLoads(t, written) {
			who := `collector "a"`
			if file == "the static target file" {
				who = `target "t"`
			}
			want := who + ` request.accept_status entry "` + entry + `" is not an HTTP status from 100 to 599 or a class such as 2xx`
			if load.err == nil || !strings.Contains(load.err.Error(), want) {
				t.Errorf("%s: accept_status: [%s] is %q, %v; want %q", file, written, load.statuses, load.err, want)
			}
		}
	}
}

// Under --config.expand-env and --static-targets.expand-env a status that
// comes from the environment is what it would be written there by hand: a
// reference without quotes is the number its value spells, a point and all,
// and one in quotes is text.
func TestAStatusFromTheEnvironmentIsReadAsItWouldBeWritten(t *testing.T) {
	for value, want := range map[string]string{"503.0": "503", "0x1F7": "503", "503": "503", "5xx": "5xx"} {
		t.Setenv("PUE_TEST_STATUS", value)
		for file, load := range acceptStatusLoads(t, "${PUE_TEST_STATUS}", WithEnvExpansion(), WithStaticTargetsEnvExpansion()) {
			if load.err != nil || !slices.Equal(load.statuses, []string{want}) {
				t.Errorf("%s: accept_status: [${PUE_TEST_STATUS}] of %s is %q, %v; want [%s]", file, value, load.statuses, load.err, want)
			}
		}
	}
	t.Setenv("PUE_TEST_STATUS", "503.0")
	for file, load := range acceptStatusLoads(t, `"${PUE_TEST_STATUS}"`, WithEnvExpansion(), WithStaticTargetsEnvExpansion()) {
		if load.err == nil || !strings.Contains(load.err.Error(), `request.accept_status entry "503.0" is not an HTTP status`) {
			t.Errorf("%s: accept_status: [\"${PUE_TEST_STATUS}\"] of 503.0 is %q, %v; want it refused as text that is no status", file, load.statuses, load.err)
		}
	}
}

// A static target's request.accept_status merged in from an anchor is read
// as one written out, a number as the status it is, and quoted digits with
// leading zeros beside a number in hex are the status both are, once the
// file is checked as the exporter checks it.
func TestAStaticTargetsStatusesMergedInAreTheStatusesTheyAre(t *testing.T) {
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\nx-r: &r {accept_status: [5.03e2, +404]}\ntargets:\n  - {name: t, collector: a, target: 'http://x', request: {<<: *r}}\n  - {name: u, collector: a, target: 'http://x', request: {accept_status: [\"0503\", 0x1F7]}}\n"))
	if err == nil {
		err = ValidateStaticTargets(file)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Targets[0].Request.AcceptStatus; !slices.Equal(got, []string{"503", "404"}) {
		t.Errorf("merged in: %q, want [503 404]", got)
	}
	if got := file.Targets[1].Request.AcceptStatus; !slices.Equal(got, []string{"503", "503"}) {
		t.Errorf("written out: %q, want [503 503]", got)
	}
}
