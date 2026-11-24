//go:build request_type_sip

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of the sip page are ones a reader can use as written: its
// collector loads, and its static targets load and are targets of that
// collector.
func TestTheSIPPageExamplesLoad(t *testing.T) {
	var cfgText, targetsText string
	for _, block := range yamlBlocks(read(t, "docs/SIP.md")) {
		switch {
		case strings.HasPrefix(block, "collectors:\n"):
			cfgText = block
		case strings.HasPrefix(block, "interval:"):
			targetsText = block
		}
	}
	if cfgText == "" || targetsText == "" {
		t.Fatal("docs/SIP.md shows no collector, or no static targets")
	}
	dir := t.TempDir()
	cfg, err := config.Load(testutil.WriteIn(t, dir, "config.yaml", cfgText))
	if err != nil {
		t.Fatalf("%v\n%s", err, cfgText)
	}
	targets, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", targetsText))
	if err == nil {
		err = config.ValidateStaticTargets(targets)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(targets, cfg)
	}
	if err != nil {
		t.Errorf("%v\n%s", err, targetsText)
	}
}
