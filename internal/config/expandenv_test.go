package config

import (
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

// An unset variable must not become an empty string. A collector with no path,
// or a credential that silently becomes blank, is a configuration that parses
// and is wrong, and the exporter would serve it.
func TestAnUnsetVariableIsAnError(t *testing.T) {
	os.Unsetenv("DEMO_ABSENT_ONE")
	os.Unsetenv("DEMO_ABSENT_TWO")
	path := envConfig(t, collectorWithPath("${DEMO_ABSENT_ONE}${DEMO_ABSENT_TWO}"))

	_, err := Load(path, WithEnvExpansion())
	if err == nil {
		t.Fatal("an unset variable must not expand to an empty string")
	}
	// Every missing name at once: finding them one restart at a time is
	// miserable.
	for _, want := range []string{"DEMO_ABSENT_ONE", "DEMO_ABSENT_TWO", "--config.expand-env"} {
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
	_, err := Load(path, WithEnvExpansion())
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error=%v, want it to name %s", err, path)
	}
}

// A static target file carries the addresses and credentials of the things
// being scraped, which is exactly the material an operator keeps out of a
// committed file. It is expanded with its own flag,
// --static-targets.expand-env; that the configuration's flag does not reach
// it is pinned by the manager and the command line.
func TestStaticTargetFilesExpandWithTheirOwnFlag(t *testing.T) {
	t.Setenv("DEMO_TARGET", "http://api.example:8080")
	path := t.TempDir() + "/targets.yaml"
	body := "interval: 1m\ntargets:\n  - name: one\n    collector: example\n    target: ${DEMO_TARGET}\n    labels:\n      price: $${NOT_A_REFERENCE}\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	plain, err := LoadStaticTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.Targets[0].Target; got != "${DEMO_TARGET}" {
		t.Fatalf("target=%q, want the reference left alone without the flag", got)
	}

	expanded, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := expanded.Targets[0].Target; got != "http://api.example:8080" {
		t.Fatalf("target=%q, want the expanded value", got)
	}
	if got := expanded.Targets[0].Labels["price"]; got != "${NOT_A_REFERENCE}" {
		t.Fatalf("$$ should escape a dollar: %q", got)
	}

	// An unset variable is an error naming the file's own flag.
	os.Unsetenv("DEMO_ABSENT_TARGET")
	if err := os.WriteFile(path, []byte(strings.Replace(body, "DEMO_TARGET", "DEMO_ABSENT_TARGET", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadStaticTargets(path, WithStaticTargetsEnvExpansion())
	if err == nil || !strings.Contains(err.Error(), `"DEMO_ABSENT_TARGET" not set; --static-targets.expand-env requires`) {
		t.Fatalf("err=%v, want the missing variable and --static-targets.expand-env named", err)
	}
}
