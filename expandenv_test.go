package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// envConfig writes a configuration document and returns its path.
func envConfig(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const envCollector = "collectors:\n  - name: example\n    request:\n      type: http\n      path: %s\n" +
	"    transform:\n      type: jq\n    metrics:\n      - name: demo_value\n        expression: .value\n"

func collectorWithPath(path string) string {
	return strings.Replace(envCollector, "%s", path, 1)
}

// Expansion is opt-in. Without the flag a reference is part of the value, which
// is what lets a configuration contain one without the exporter touching it.
func TestReferencesAreLiteralWithoutTheFlag(t *testing.T) {
	t.Setenv("DEMO_PATH", "/expanded")
	path := envConfig(t, collectorWithPath("${DEMO_PATH}"))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Path; got != "${DEMO_PATH}" {
		t.Fatalf("path=%q, want the reference left alone", got)
	}
}

func TestReferencesAreExpandedWithTheFlag(t *testing.T) {
	t.Setenv("DEMO_PATH", "/expanded")
	path := envConfig(t, collectorWithPath("${DEMO_PATH}"))

	cfg, err := LoadConfig(path, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Path; got != "/expanded" {
		t.Fatalf("path=%q, want the expanded value", got)
	}
}

// A configuration file is full of dollar signs that are not references: a
// regex, a jq expression, a Python pre-script. Only the braced form is a
// reference, and `$$` escapes a literal dollar.
func TestOnlyTheBracedFormIsAReference(t *testing.T) {
	t.Setenv("DEMO_PATH", "/expanded")
	t.Setenv("PRICE", "9")
	tests := []struct {
		name  string
		given string
		want  string
	}{
		{"braced", "${DEMO_PATH}", "/expanded"},
		{"bare dollar name", "/$DEMO_PATH", "/$DEMO_PATH"},
		{"regex anchor", "/a$", "/a$"},
		{"escaped brace", "/$${DEMO_PATH}", "/${DEMO_PATH}"},
		{"embedded", "/before${DEMO_PATH}/after", "/before/expanded/after"},
		{"twice", "${DEMO_PATH}${DEMO_PATH}", "/expanded/expanded"},
		{"adjacent text", "/x${PRICE}y", "/x9y"},
		{"lowercase name is still a name", "${DEMO_PATH}", "/expanded"},
		{"not a name", "${1BAD}", "${1BAD}"},
		{"unclosed", "${DEMO_PATH", "${DEMO_PATH"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadConfig(envConfig(t, collectorWithPath(test.given)), WithEnvExpansion())
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Collectors[0].Request.Path; got != test.want {
				t.Fatalf("%q expanded to %q, want %q", test.given, got, test.want)
			}
		})
	}
}

// An unset variable must not become an empty string. A collector with no path,
// or a credential that silently becomes blank, is a configuration that parses
// and is wrong, and the exporter would serve it.
func TestAnUnsetVariableIsAnError(t *testing.T) {
	os.Unsetenv("DEMO_ABSENT_ONE")
	os.Unsetenv("DEMO_ABSENT_TWO")
	path := envConfig(t, collectorWithPath("${DEMO_ABSENT_ONE}${DEMO_ABSENT_TWO}"))

	_, err := LoadConfig(path, WithEnvExpansion())
	if err == nil {
		t.Fatal("an unset variable must not expand to an empty string")
	}
	// Every missing name at once: finding them one restart at a time is
	// miserable.
	for _, want := range []string{"DEMO_ABSENT_ONE", "DEMO_ABSENT_TWO", "--config.export-env"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

// Set-but-empty is a deliberate choice, not a mistake.
func TestAnEmptyVariableExpandsToNothing(t *testing.T) {
	t.Setenv("DEMO_EMPTY", "")
	cfg, err := LoadConfig(envConfig(t, collectorWithPath("/base${DEMO_EMPTY}")), WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Path; got != "/base" {
		t.Fatalf("path=%q, want the empty value substituted", got)
	}
}

// A value is substituted before the document is parsed, so a line break in one
// does not make a long string: it ends the line and the rest becomes YAML.
func TestAValueWithALineBreakIsRefused(t *testing.T) {
	t.Setenv("DEMO_MULTILINE", "/one\nmetrics: []")
	_, err := LoadConfig(envConfig(t, collectorWithPath("${DEMO_MULTILINE}")), WithEnvExpansion())
	if err == nil {
		t.Fatal("a value containing a line break must be refused")
	}
	for _, want := range []string{"DEMO_MULTILINE", "line break"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

// The error names the file, because an operator running with a config and a
// target file needs to know which one to go and fix.
func TestTheErrorNamesTheDocument(t *testing.T) {
	os.Unsetenv("DEMO_ABSENT_THREE")
	path := envConfig(t, collectorWithPath("${DEMO_ABSENT_THREE}"))
	_, err := LoadConfig(path, WithEnvExpansion())
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error=%v, want it to name %s", err, path)
	}
}

// The scheduled target document carries the addresses and credentials of the
// things being scraped, which is exactly the material an operator keeps out of
// a committed file, so the flag applies to it too.
func TestTargetFilesExpandTheSameWay(t *testing.T) {
	t.Setenv("DEMO_TARGET", "http://api.example:8080")
	path := t.TempDir() + "/targets.yaml"
	body := "targets:\n  - name: one\n    collector: example\n    target: ${DEMO_TARGET}\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	plain, err := LoadTargetFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.Targets[0].Target; got != "${DEMO_TARGET}" {
		t.Fatalf("target=%q, want the reference left alone without the flag", got)
	}

	expanded, err := LoadTargetFile(path, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := expanded.Targets[0].Target; got != "http://api.example:8080" {
		t.Fatalf("target=%q, want the expanded value", got)
	}
}

// A reload must read the file the way startup did. One that quietly stopped
// expanding would replace a working configuration with one full of literal
// references — and the collector would then request a path called
// "${DEMO_PATH}".
func TestAReloadKeepsExpanding(t *testing.T) {
	t.Setenv("DEMO_PATH", "/first")
	path := envConfig(t, collectorWithPath("${DEMO_PATH}"))
	cfg, err := LoadConfig(path, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, path, slog.Default())
	manager.SetPythonPath("python3")
	manager.SetEnvExpansion(true)

	t.Setenv("DEMO_PATH", "/second")
	if err := os.WriteFile(path, []byte(collectorWithPath("${DEMO_PATH}/v2")), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = manager.lastMod.Add(-1)
	manager.reloadConfig()
	if got := manager.Get().Collectors[0].Request.Path; got != "/second/v2" {
		t.Fatalf("path=%q after reload, want the reference expanded again", got)
	}
}

// A manager that was not told to expand must not start doing so on reload.
func TestAReloadDoesNotStartExpanding(t *testing.T) {
	t.Setenv("DEMO_PATH", "/expanded")
	path := envConfig(t, collectorWithPath("/static"))
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, path, slog.Default())
	manager.SetPythonPath("python3")

	if err := os.WriteFile(path, []byte(collectorWithPath("${DEMO_PATH}")), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = manager.lastMod.Add(-1)
	manager.reloadConfig()
	if got := manager.Get().Collectors[0].Request.Path; got != "${DEMO_PATH}" {
		t.Fatalf("path=%q, want the reference untouched", got)
	}
}

// An unset variable is a startup error, not a scrape error, so a reload that
// would introduce one is rejected and the previous configuration stays active.
func TestAReloadWithAMissingVariableIsRejected(t *testing.T) {
	t.Setenv("DEMO_PATH", "/first")
	os.Unsetenv("DEMO_ABSENT_FOUR")
	path := envConfig(t, collectorWithPath("${DEMO_PATH}"))
	cfg, err := LoadConfig(path, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, path, slog.Default())
	manager.SetPythonPath("python3")
	manager.SetEnvExpansion(true)

	if err := os.WriteFile(path, []byte(collectorWithPath("${DEMO_ABSENT_FOUR}")), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = manager.lastMod.Add(-1)
	manager.reloadConfig()
	if got := manager.Get().Collectors[0].Request.Path; got != "/first" {
		t.Fatalf("path=%q, want the previous configuration to stay active", got)
	}
}

// The substitution is textual and happens before parsing, so a reference can
// supply a whole structured value, not only a scalar.
func TestExpansionHappensBeforeParsing(t *testing.T) {
	t.Setenv("DEMO_METHOD", "POST")
	t.Setenv("DEMO_HEADER", "application/json")
	body := "collectors:\n  - name: example\n    request:\n      type: http\n      method: ${DEMO_METHOD}\n" +
		"      headers:\n        Accept: ${DEMO_HEADER}\n" +
		"    transform:\n      type: jq\n    metrics:\n      - name: demo_value\n        expression: .value\n"
	cfg, err := LoadConfig(envConfig(t, body), WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Method; got != "POST" {
		t.Fatalf("method=%q", got)
	}
	if got := cfg.Collectors[0].Request.Headers["Accept"]; got != "application/json" {
		t.Fatalf("Accept=%q", got)
	}
}
