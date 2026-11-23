//go:build request_type_radius && request_type_whois

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of the radius and whois pages are ones a reader can use as
// written: each page's collector loads, and its static targets load and are
// targets of that collector.
func TestRADIUSAndWhoisPageExamplesLoad(t *testing.T) {
	for _, page := range []string{"docs/RADIUS.md", "docs/WHOIS.md"} {
		var cfgText, targetsText string
		for _, block := range yamlBlocks(read(t, page)) {
			switch {
			case strings.HasPrefix(block, "collectors:\n"):
				cfgText = block
			case strings.HasPrefix(block, "interval:"):
				targetsText = block
			}
		}
		if cfgText == "" || targetsText == "" {
			t.Errorf("%s shows no collector, or no static targets", page)
			continue
		}
		dir := t.TempDir()
		cfg, err := config.Load(testutil.WriteIn(t, dir, "config.yaml", cfgText))
		if err != nil {
			t.Errorf("%s: %v\n%s", page, err, cfgText)
			continue
		}
		targets, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", targetsText))
		if err == nil {
			err = config.ValidateStaticTargets(targets)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(targets, cfg)
		}
		if err != nil {
			t.Errorf("%s: %v\n%s", page, err, targetsText)
		}
	}
}
