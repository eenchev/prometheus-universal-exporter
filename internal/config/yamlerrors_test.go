package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Decoding errors name keys and places in the file, not the Go types behind
// them, and every problem in the file is reported at once.
func TestDecodingErrorsUseTheFilesTerms(t *testing.T) {
	collector := "collectors:\n  - name: a\n    request: {type: http}\n    transform: {type: regex}\n    metrics: [{name: x, expression: 'v=(\\d+)'}]\n"
	for name, test := range map[string]struct {
		document string
		want     string
	}{
		"unknown key":          {strings.Replace(collector, "    request:", "    requst: {}\n    request:", 1), `line 3: unknown key "requst" in a collector`},
		"two unknown keys":     {strings.Replace(collector, "    request:", "    requst: {}\n    respnse: {}\n    request:", 1), `line 3: unknown key "requst" in a collector; line 4: unknown key "respnse" in a collector`},
		"unknown label key":    {strings.Replace(collector, "expression: 'v=(\\d+)'}", "expression: 'v=(\\d+)', labels: [{nme: y}]}", 1), `line 5: unknown key "nme" in a label`},
		"unknown top key":      {collector + "otlpp: {}\n", `line 6: unknown key "otlpp" in the configuration`},
		"list for a mapping":   {strings.Replace(collector, "request: {type: http}", "request: [http]", 1), "line 3: request must be a mapping, not a list"},
		"string for a number":  {collector + "    max_concurrent_probes: lots\n", `line 6: expected a whole number, not the string "lots"`},
		"string for a boolean": {collector + "    coalesce: maybe\n", `line 6: expected true or false, not the string "maybe"`},
		"number for a list":    {"collectors: 5\n", "line 1: expected a list of collectors, not the number 5"},
		"mapping for a string": {collector + "    name_escaping: {a: 1}\n", "line 6: expected a single value, not a mapping"},
		"list for a type":      {strings.Replace(collector, "{name: x,", "{name: x, type: [gauge],", 1), "line 5: expected a metric type: gauge, counter, histogram, summary or untyped, not a list"},
		"bad duration":         {collector + "    limits: {script_timeout: soon}\n", `line 6: "soon" is not a duration; write one such as 500ms, 30s or 1m30s`},
		"list for a duration":  {collector + "    limits:\n      script_timeout: [1s]\n", "line 7: expected a duration such as 30s, not a list"},
		"bad size":             {collector + "    limits:\n      max_response_bytes: 10 parsecs\n", `line 7: size "10 parsecs" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`},
		"errors of all kinds":  {collector + "    limits: {script_timeout: soon}\n    coalesce: maybe\n", `line 6: "soon" is not a duration; write one such as 500ms, 30s or 1m30s; line 7: expected true or false, not the string "maybe"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(testutil.WriteFile(t, "config.yaml", test.document))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "model.") || strings.Contains(err.Error(), "!!") {
				t.Fatalf("the error names Go or YAML internals: %v", err)
			}
		})
	}
}

// A syntax error is not a decoding error and is reported as the parser gives it.
func TestYAMLSyntaxErrorsAreUnchanged(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", "collectors: [\n"))
	if err == nil || !strings.HasPrefix(err.Error(), "yaml: line") {
		t.Fatalf("error %v, want the parser's", err)
	}
}

// A scheduled target's request block refuses keys it does not know, at any
// depth, as the rest of the file does, instead of ignoring them.
func TestTargetRequestRefusesUnknownKeys(t *testing.T) {
	for name, test := range map[string]struct {
		request string
		want    string
	}{
		"misspelt key":     {"{pth: /status}", `line 5: unknown key "pth" in a scheduled target's request`},
		"nested key":       {"{retry: {atempts: 2}}", `line 5: unknown key "atempts" in retry`},
		"basic_auth key":   {"{basic_auth: {user: a}}", `line 5: unknown key "user" in basic_auth`},
		"list for a value": {"{method: [GET]}", "line 5: expected a single value, not a list"},
	} {
		t.Run(name, func(t *testing.T) {
			document := "targets:\n  - name: a\n    collector: c\n    target: http://x\n    request: " + test.request + "\n"
			_, err := LoadTargets(testutil.WriteFile(t, "targets.yaml", document))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
	file, err := LoadTargets(testutil.WriteFile(t, "targets.yaml", "targets:\n  - name: a\n    collector: c\n    target: http://x\n    request: {path: /s, retry: {attempts: 2, backoff: 1s}, headers: {X-A: b}}\n"))
	if err != nil {
		t.Fatalf("a valid request block was refused: %v", err)
	}
	if r := file.Targets[0].Request; !r.PathSet || r.Path != "/s" || r.Retry == nil || r.Retry.Attempts != 2 || r.Headers["X-A"] != "b" {
		t.Fatalf("request decoded as %+v", r)
	}
}
