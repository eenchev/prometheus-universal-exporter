//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A reload lets those who asked (OnPrepare) prepare what it is about to put
// in force, before it is in force, so that what is costly to work out for a
// configuration is not left to the first that uses it.

// prepareSeen is what a reload gave those who asked to prepare, and what
// was in force while they did, as the reloading goroutine and as another
// read it.
type prepareSeen struct {
	config, inForceConfig, readConfig    *model.Config
	targets, inForceTargets, readTargets *model.StaticTargetFile
}

// Each reload that puts a configuration, a static target file or both in
// force has what it is about to put in force prepared, once, before it is in
// force: it gives the configuration and the static target file as they will
// be in force, the one it leaves as it is among them, while Get and InForce
// still answer with what was in force, to the reloading goroutine and to
// another, which is not kept waiting; when the reload returns what was
// prepared is in force. Those who asked to be told (OnInstall) are told
// after, of what was prepared: the order is prepared, in force, told. So it
// is for a reload of both files, for a watch tick that finds only the static
// target file changed, or only the configuration, for a reload whose
// configuration is refused and whose static target file goes in force alone,
// and for one whose static target file is refused and whose configuration
// goes in force alone. A reload that is refused whole prepares nothing, and
// neither does a watch tick that finds nothing changed.
func TestAReloadPreparesWhatItIsAboutToPutInForce(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	var prepared []prepareSeen
	var events []string
	p.manager.OnPrepare(func(cfg *model.Config, targets *model.StaticTargetFile) {
		seen := prepareSeen{config: cfg, targets: targets}
		seen.inForceConfig, seen.inForceTargets = p.manager.InForce()
		if p.manager.Get() != seen.inForceConfig {
			t.Error("Get and InForce answer with two configurations while a reload prepares")
		}
		// Another goroutine's read is answered while this one prepares: were
		// it kept waiting until the configuration is in force, the reload
		// would wait here for ever.
		read := make(chan struct{})
		go func() {
			seen.readConfig, seen.readTargets = p.manager.InForce()
			close(read)
		}()
		<-read
		prepared = append(prepared, seen)
		events = append(events, "prepared")
	})
	p.manager.OnInstall(func() {
		cfg, targets := p.manager.InForce()
		if len(prepared) == 0 || cfg != prepared[len(prepared)-1].config || targets != prepared[len(prepared)-1].targets {
			t.Error("what is in force when the reload tells is not what it had prepared last")
		}
		events = append(events, "told")
	})
	// check holds the reload just made to have prepared once, what is now in
	// force, while what was in force before it still was.
	check := func(what string, wasConfig *model.Config, wasTargets *model.StaticTargetFile, want int) prepareSeen {
		t.Helper()
		if len(prepared) != want {
			t.Fatalf("%s prepared %d times in all, want %d", what, len(prepared), want)
		}
		last := prepared[want-1]
		if cfg, targets := p.manager.InForce(); cfg != last.config || targets != last.targets {
			t.Fatalf("%s left in force another configuration (%v) or static target file (%v) than it prepared", what, cfg != last.config, targets != last.targets)
		}
		if last.inForceConfig != wasConfig || last.inForceTargets != wasTargets || last.readConfig != wasConfig || last.readTargets != wasTargets {
			t.Fatalf("%s: while it prepared, the configuration in force was the one before it %v, to another goroutine %v, and the static target file %v, to another goroutine %v; want all four", what, last.inForceConfig == wasConfig, last.readConfig == wasConfig, last.inForceTargets == wasTargets, last.readTargets == wasTargets)
		}
		return last
	}
	startConfig, startTargets := p.manager.InForce()

	if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
		t.Fatal(err)
	}
	both := check("a reload of both files", startConfig, startTargets, 1)
	if both.config == startConfig || both.targets == startTargets {
		t.Fatalf("a reload of both files prepared the configuration (%v) or the static target file (%v) that was in force, want those it read", both.config == startConfig, both.targets == startTargets)
	}

	p.manager.reloadChanged()
	if len(prepared) != 1 {
		t.Fatalf("a watch tick that found nothing changed prepared: %d times in all, want 1", len(prepared))
	}

	p.write(t, p.targetsAt, twoTargets+"  - name: tc\n    collector: a\n    target: http://c.invalid\n")
	p.manager.reloadChanged()
	targetsAlone := check("a reload of the static target file alone", both.config, both.targets, 2)
	if targetsAlone.config != both.config || targetsAlone.targets == both.targets || len(targetsAlone.targets.Targets) != 3 {
		t.Fatalf("a reload of the static target file alone prepared another configuration %v, the file in force %v, of %d targets; want the configuration in force with the file it read, of 3", targetsAlone.config != both.config, targetsAlone.targets == both.targets, len(targetsAlone.targets.Targets))
	}

	third := "  - name: c\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: c_value\n        expression: 'value=(\\d+)'\n"
	p.write(t, p.configPath, twoCollectors+third)
	p.manager.reloadChanged()
	configAlone := check("a reload of the configuration alone", targetsAlone.config, targetsAlone.targets, 3)
	if configAlone.targets != targetsAlone.targets || configAlone.config == targetsAlone.config || len(configAlone.config.Collectors) != 3 {
		t.Fatalf("a reload of the configuration alone prepared another static target file %v, the configuration in force %v, of %d collectors; want the file in force with the configuration it read, of 3", configAlone.targets != targetsAlone.targets, configAlone.config == targetsAlone.config, len(configAlone.config.Collectors))
	}

	p.write(t, p.configPath, "collectors: [\n")
	if err := p.manager.Reload(ReloadTriggerSignal); err == nil {
		t.Fatal("a configuration that is no YAML was reloaded")
	}
	// The static target file, read with it, is put in force alone.
	refusedConfig := check("a reload whose configuration was refused", configAlone.config, configAlone.targets, 4)
	if refusedConfig.config != configAlone.config || refusedConfig.targets == configAlone.targets {
		t.Fatalf("a reload whose configuration was refused prepared another configuration %v, the static target file in force %v; want the configuration in force with the file it read", refusedConfig.config != configAlone.config, refusedConfig.targets == configAlone.targets)
	}

	// A static target of a collector the configuration does not have is
	// refused, and the configuration read with it goes in force alone.
	p.write(t, p.configPath, twoCollectors+third)
	p.write(t, p.targetsAt, twoTargets+"  - name: td\n    collector: d\n    target: http://d.invalid\n")
	if err := p.manager.Reload(ReloadTriggerSignal); err == nil || !strings.Contains(err.Error(), "td") {
		t.Fatalf("a static target of a collector that is not there was reloaded: %v", err)
	}
	refusedTargets := check("a reload whose static target file was refused", refusedConfig.config, refusedConfig.targets, 5)
	if refusedTargets.targets != refusedConfig.targets || refusedTargets.config == refusedConfig.config {
		t.Fatalf("a reload whose static target file was refused prepared another static target file %v, the configuration in force %v; want the file in force with the configuration it read", refusedTargets.targets != refusedConfig.targets, refusedTargets.config == refusedConfig.config)
	}

	p.write(t, p.configPath, "collectors: [\n")
	p.write(t, p.targetsAt, "targets: [\n")
	if err := p.manager.Reload(ReloadTriggerSignal); err == nil {
		t.Fatal("two files that are no YAML were reloaded")
	}
	if cfg, targets := p.manager.InForce(); len(prepared) != 5 || cfg != refusedTargets.config || targets != refusedTargets.targets {
		t.Fatalf("a reload that was refused whole prepared, or put something in force: %d times in all, want 5, and what was in force", len(prepared))
	}
	if want := strings.Fields(strings.Repeat("prepared told ", 5)); !slices.Equal(events, want) {
		t.Fatalf("the reloads prepared and told in the order %v, want %v: each prepared, then told", events, want)
	}
}

// Several may ask to prepare: each is given what the reload is about to put
// in force, in the order they asked, all of them before it is in force and
// before any is told.
func TestThoseWhoAskedPrepareInTheOrderTheyAsked(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	start := p.manager.Get()
	var events []string
	for _, name := range []string{"first", "second"} {
		p.manager.OnPrepare(func(cfg *model.Config, _ *model.StaticTargetFile) {
			if cfg == start || p.manager.Get() != start {
				t.Errorf("the %s to prepare was given the configuration in force, or found the one it prepares in force", name)
			}
			events = append(events, name+" prepared")
		})
		p.manager.OnInstall(func() {
			if p.manager.Get() == start {
				t.Errorf("the %s was told with the former configuration in force", name)
			}
			events = append(events, name+" told")
		})
	}
	if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
		t.Fatal(err)
	}
	if want := []string{"first prepared", "second prepared", "first told", "second told"}; !slices.Equal(events, want) {
		t.Fatalf("the reload prepared and told in the order %v, want %v", events, want)
	}
}
