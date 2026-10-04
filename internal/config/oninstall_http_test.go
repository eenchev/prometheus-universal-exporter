//go:build !select_request_types || request_type_http

package config

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A reload tells those who asked (OnInstall) when it has put something in
// force, and only then, so that what is kept for a configuration elsewhere
// follows the reload as it is made.

// Each reload that puts a configuration, a static target file or both in
// force tells once, when they are in force: a reload of both files, a watch
// tick that finds only the static target file changed, which leaves the
// configuration in force as it is, and one that finds only the configuration
// changed. A reload that is refused tells nothing, and neither does a watch
// tick that finds nothing changed.
func TestAReloadTellsWhenItHasPutSomethingInForce(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	type seen struct {
		config  *model.Config
		targets *model.StaticTargetFile
	}
	var told []seen
	p.manager.OnInstall(func() {
		cfg, targets := p.manager.InForce()
		told = append(told, seen{cfg, targets})
	})
	startCfg, startTargets := p.manager.InForce()

	if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
		t.Fatal(err)
	}
	if len(told) != 1 || told[0].config == startCfg || told[0].targets == startTargets {
		t.Fatalf("a reload of both files told %d times, the configuration in force then another %v, the targets another %v; want once, with both in force", len(told), len(told) > 0 && told[0].config != startCfg, len(told) > 0 && told[0].targets != startTargets)
	}

	p.manager.reloadChanged()
	if len(told) != 1 {
		t.Fatalf("a watch tick that found nothing changed told: %d times in all, want 1", len(told))
	}

	p.write(t, p.targetsAt, twoTargets+"  - name: tc\n    collector: a\n    target: http://c.invalid\n")
	p.manager.reloadChanged()
	if len(told) != 2 || told[1].config != told[0].config || len(told[1].targets.Targets) != 3 {
		t.Fatalf("a reload of the static target file alone told %d times in all, want twice, the second with the same configuration and the three targets in force", len(told))
	}

	p.write(t, p.configPath, twoCollectors+"  - name: c\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: c_value\n        expression: 'value=(\\d+)'\n")
	p.manager.reloadChanged()
	if len(told) != 3 || told[2].targets != told[1].targets || len(told[2].config.Collectors) != 3 {
		t.Fatalf("a reload of the configuration alone told %d times in all, want three times, the third with the same targets and the three collectors in force", len(told))
	}

	p.write(t, p.configPath, "collectors: [\n")
	if err := p.manager.Reload(ReloadTriggerSignal); err == nil {
		t.Fatal("a configuration that is no YAML was reloaded")
	}
	// The static target file, read with it, was put in force alone.
	if len(told) != 4 || told[3].config != told[2].config {
		t.Fatalf("a reload whose configuration was refused told %d times in all, want four, the last for the static target file alone, with the configuration as it was", len(told))
	}
	p.write(t, p.targetsAt, "targets: [\n")
	if err := p.manager.Reload(ReloadTriggerSignal); err == nil {
		t.Fatal("two files that are no YAML were reloaded")
	}
	if len(told) != 4 {
		t.Fatalf("a reload that was refused whole told: %d times in all, want 4", len(told))
	}
}
