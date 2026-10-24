//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// lint.ignore takes the ids of the lint rules and refuses another, naming
// the collector, the id and the ids it takes, so that a misspelt id is not
// a silencing that silences nothing.
func TestLintIgnoreTakesTheRuleIDs(t *testing.T) {
	load := func(block string) (*model.Config, error) {
		t.Helper()
		return Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", testutil.CollectorsDocument("app")+block))
	}
	cfg, err := load("    lint:\n      ignore: [counter-total, unit-suffix, unbounded-label, name-clash]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Lint.Ignore; !slices.Equal(got, model.LintRules) {
		t.Fatalf("ignore loaded as %v", got)
	}
	if _, err := load("    lint: {}\n"); err != nil {
		t.Fatal(err)
	}
	for block, want := range map[string]string{
		"    lint:\n      ignore: [unit_suffix]\n":  `collector "app" lint.ignore has unknown rule "unit_suffix"; want one of counter-total, unit-suffix, unbounded-label, name-clash`,
		"    lint:\n      ignore: [all]\n":          `unknown rule "all"`,
		"    lint:\n      silence: [unit-suffix]\n": "silence",
	} {
		if _, err := load(block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error with %q", block, err, want)
		}
	}
}
