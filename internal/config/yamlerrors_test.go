package config

import (
	"path/filepath"
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
		"string for a number":  {collector + "    max_concurrent_probes: lots\n", `line 6: expected a whole number, not a string`},
		"string for a boolean": {collector + "    coalesce: maybe\n", `line 6: expected true or false, not a string`},
		"number for a list":    {"collectors: 5\n", "line 1: expected a list of collectors, not the number 5"},
		"mapping for a string": {collector + "    name_escaping: {a: 1}\n", "line 6: expected a single value, not a mapping"},
		"list for a type":      {strings.Replace(collector, "{name: x,", "{name: x, type: [gauge],", 1), "line 5: expected a metric type: gauge, counter, histogram, summary or untyped, not a list"},
		"bad duration":         {collector + "    limits: {script_timeout: soon}\n", `line 6: "soon" is not a duration; write one such as 500ms, 30s or 1m30s`},
		"list for a duration":  {collector + "    limits:\n      script_timeout: [1s]\n", "line 7: expected a duration such as 30s, not a list"},
		"bad size":             {collector + "    limits:\n      max_response_bytes: 10 parsecs\n", `line 7: size "10 parsecs" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`},
		"errors of all kinds":  {collector + "    limits: {script_timeout: soon}\n    coalesce: maybe\n", `line 6: "soon" is not a duration; write one such as 500ms, 30s or 1m30s; line 7: expected true or false, not a string`},
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

// A static target's request block refuses keys it does not know, at any
// depth, as the rest of the file does, instead of ignoring them.
func TestTargetRequestRefusesUnknownKeys(t *testing.T) {
	for name, test := range map[string]struct {
		request string
		want    string
	}{
		"misspelt key":     {"{pth: /status}", `line 6: unknown key "pth" in a static target's request`},
		"nested key":       {"{retry: {atempts: 2}}", `line 6: unknown key "atempts" in retry`},
		"basic_auth key":   {"{basic_auth: {user: a}}", `line 6: unknown key "user" in basic_auth`},
		"list for a value": {"{method: [GET]}", "line 6: expected a single value, not a list"},
	} {
		t.Run(name, func(t *testing.T) {
			document := "interval: 1m\ntargets:\n  - name: a\n    collector: c\n    target: http://x\n    request: " + test.request + "\n"
			_, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: a\n    collector: c\n    target: http://x\n    request: {path: /s, retry: {attempts: 2, backoff: 1s}, headers: {X-A: b}}\n"))
	if err != nil {
		t.Fatalf("a valid request block was refused: %v", err)
	}
	if r := file.Targets[0].Request; !r.PathSet || r.Path != "/s" || r.Retry == nil || r.Retry.Attempts == nil || *r.Retry.Attempts != 2 || r.Headers["X-A"] != "b" {
		t.Fatalf("request decoded as %+v", r)
	}
}

// A label's kind follows from its keys, so a leftover type says what to write
// instead.
func TestALabelTypeSaysWhatReplacedIt(t *testing.T) {
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", testutil.MinimalConfig+"        labels:\n          - name: env\n            type: string\n            value: prod\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `unknown key "type" in a label; a label has no type: set value for a static label, or expression to read it from the response`) {
		t.Fatalf("err=%v", err)
	}
}

// response.format is gone; decoder.type chooses the decoder.
func TestAResponseFormatSaysWhatReplacedIt(t *testing.T) {
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.Replace(testutil.MinimalConfig, "    transform:\n", "    response:\n      format: text\n    transform:\n", 1))
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `unknown key "format" in response; the decoder is chosen by decoder.type`) {
		t.Fatalf("err=%v", err)
	}
}

// A file holds one document: one that goes on after --- would have the rest
// ignored without a word, so it is refused naming the line. A leading ---, a
// final --- with nothing after it, and ... ending the document are fine.
func TestAFileHoldsOneDocument(t *testing.T) {
	collector := testutil.CollectorsDocument("demo")
	targets := "interval: 1m\ntargets:\n  - name: one\n    collector: demo\n    target: http://a.invalid\n"
	for name, tc := range map[string]struct {
		body string
		load func(string) error
		want string
	}{
		"configuration":  {collector + "---\notlp:\n  enabled: true\n", func(p string) error { _, err := Load(p); return err }, "line 12: a second document starts here"},
		"static targets": {targets + "---\ninterval: 5m\n", func(p string) error { _, err := LoadStaticTargets(p); return err }, "line 7: a second document starts here"},
		"collector file": {"collector_files: [more.yaml]\n", func(p string) error {
			testutil.WriteIn(t, filepath.Dir(p), "more.yaml", collector+"---\ncollectors: []\n")
			_, err := Load(p)
			return err
		}, "collector file"},
	} {
		t.Run(name, func(t *testing.T) {
			path := testutil.WriteIn(t, t.TempDir(), "file.yaml", tc.body)
			if err := tc.load(path); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "a second document starts here") {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	for name, body := range map[string]string{
		"a leading ---":           "---\n" + collector,
		"a final ---":             collector + "---\n",
		"an end marker":           collector + "...\n",
		"a final --- and comment": collector + "---\n# nothing more\n",
	} {
		path := testutil.WriteIn(t, t.TempDir(), "config.yaml", body)
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
