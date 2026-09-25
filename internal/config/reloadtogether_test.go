package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The configuration and the static target file are only valid together, so a
// change that spans both — removing a collector and the target that uses it —
// must reload as a whole, whichever file is written first and however the
// reload is triggered (reloadtogether in config.go's apply).

const twoCollectors = "collectors:\n" +
	"  - name: a\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: a_value\n        expression: 'value=(\\d+)'\n" +
	"  - name: b\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: b_value\n        expression: 'value=(\\d+)'\n"

const twoTargets = "interval: 1m\ntargets:\n  - name: ta\n    collector: a\n    target: http://a.invalid\n  - name: tb\n    collector: b\n    target: http://b.invalid\n"

// oneCollector and oneTarget are twoCollectors and twoTargets without b.
var (
	oneCollector, _, _ = strings.Cut(twoCollectors, "  - name: b")
	oneTarget, _, _    = strings.Cut(twoTargets, "  - name: tb")
)

type pair struct {
	manager               *Manager
	configPath, targetsAt string
}

func newPair(t *testing.T, config, targets string) *pair {
	t.Helper()
	dir := t.TempDir()
	p := &pair{configPath: testutil.WriteIn(t, dir, "config.yaml", config), targetsAt: testutil.WriteIn(t, dir, "targets.yaml", targets)}
	cfg, err := Load(p.configPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(p.targetsAt)
	if err == nil {
		err = ValidateStaticTargets(file)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.manager = NewManager(cfg, p.configPath, testutil.QuietLogger(t))
	p.manager.SetTargets(p.targetsAt, file)
	return p
}

// write rewrites a file with a modification time later than any read so far,
// as a watch tick would find it.
func (p *pair) write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Duration(len(body)) * time.Millisecond).Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func (p *pair) state() (collectors, targets int) {
	return len(p.manager.Get().Collectors), len(p.manager.StaticTargets())
}

func (p *pair) reloads(file string) (successes, failures uint64) {
	st := p.manager.Reloads.files[file]
	if st == nil {
		return 0, 0
	}
	return st.successes, st.failures
}

func TestAChangeToBothFilesReloadsTogether(t *testing.T) {
	t.Run("both written before one watch tick", func(t *testing.T) {
		p := newPair(t, twoCollectors, twoTargets)
		p.write(t, p.configPath, oneCollector)
		p.write(t, p.targetsAt, oneTarget)
		p.manager.reloadChanged()
		if c, n := p.state(); c != 1 || n != 1 {
			t.Fatalf("collectors=%d targets=%d, want 1 and 1", c, n)
		}
		if _, failures := p.reloads(reloadFileConfig); failures != 0 {
			t.Fatalf("the configuration was refused %d times", failures)
		}
	})

	t.Run("configuration first, targets a tick later", func(t *testing.T) {
		p := newPair(t, twoCollectors, twoTargets)
		p.write(t, p.configPath, oneCollector)
		p.manager.reloadChanged()
		// Alone it is refused: the target file in force still uses b.
		if c, n := p.state(); c != 2 || n != 2 {
			t.Fatalf("after the configuration alone: collectors=%d targets=%d, want both kept", c, n)
		}
		// Nothing changed since: the refused configuration is not read
		// again on every tick.
		p.manager.reloadChanged()
		if _, failures := p.reloads(reloadFileConfig); failures != 1 {
			t.Fatalf("the waiting configuration was read again without a change: %d failures", failures)
		}
		p.write(t, p.targetsAt, oneTarget)
		p.manager.reloadChanged()
		if c, n := p.state(); c != 1 || n != 1 {
			t.Fatalf("after the targets: collectors=%d targets=%d, want 1 and 1", c, n)
		}
		if successes, failures := p.reloads(reloadFileConfig); successes != 1 || failures != 1 {
			t.Fatalf("configuration reloads: %d successes, %d failures", successes, failures)
		}
	})

	t.Run("targets first, configuration a tick later", func(t *testing.T) {
		p := newPair(t, twoCollectors, twoTargets)
		p.write(t, p.targetsAt, oneTarget)
		p.manager.reloadChanged()
		p.write(t, p.configPath, oneCollector)
		p.manager.reloadChanged()
		if c, n := p.state(); c != 1 || n != 1 {
			t.Fatalf("collectors=%d targets=%d, want 1 and 1", c, n)
		}
	})

	t.Run("on demand", func(t *testing.T) {
		p := newPair(t, twoCollectors, twoTargets)
		p.write(t, p.configPath, oneCollector)
		p.write(t, p.targetsAt, oneTarget)
		if err := p.manager.Reload(ReloadTriggerHTTP); err != nil {
			t.Fatalf("a change to both files was refused: %v", err)
		}
		if c, n := p.state(); c != 1 || n != 1 {
			t.Fatalf("collectors=%d targets=%d, want 1 and 1", c, n)
		}
	})

	t.Run("OTLP turned off with the target that needed it", func(t *testing.T) {
		withOTLP := "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n" + oneCollector
		exported := strings.Replace(oneTarget, "    target: http://a.invalid\n", "    target: http://a.invalid\n    export_via_otlp: true\n", 1)
		p := newPair(t, withOTLP, exported)
		p.write(t, p.configPath, oneCollector)
		p.write(t, p.targetsAt, oneTarget)
		if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
			t.Fatal(err)
		}
		if p.manager.Get().OTLP.Enabled || p.manager.StaticTargets()[0].ExportViaOTLP {
			t.Fatal("the change was not applied as a whole")
		}
	})
}

// A file refused for itself does not wait for the other: a change to the
// other file does not read it again, and the other is still checked against
// what is in force.
func TestAFileRefusedForItselfDoesNotWait(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	p.write(t, p.configPath, "collectors: [\n")
	p.manager.reloadChanged()
	p.write(t, p.targetsAt, oneTarget)
	p.manager.reloadChanged()
	if successes, failures := p.reloads(reloadFileConfig); successes != 0 || failures != 1 {
		t.Fatalf("configuration reloads: %d successes, %d failures; a broken file must not be retried with the other", successes, failures)
	}
	if c, n := p.state(); c != 2 || n != 1 {
		t.Fatalf("collectors=%d targets=%d, want the configuration kept and the targets reloaded", c, n)
	}
}

// When both files are read and disagree, the one that agrees with the other
// as in force still goes in, and the other is refused, naming why.
func TestWhatAgreesWithWhatIsInForceStillGoesIn(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	// The configuration only gains a collector; the targets name one that
	// exists nowhere.
	three := twoCollectors + "  - name: c\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: c_value\n        expression: 'value=(\\d+)'\n"
	p.write(t, p.configPath, three)
	p.write(t, p.targetsAt, strings.Replace(twoTargets, "collector: b", "collector: missing", 1))
	err := p.manager.Reload(ReloadTriggerHTTP)
	if err == nil || !strings.Contains(err.Error(), "static target file") || strings.Contains(err.Error(), "configuration "+p.configPath) {
		t.Fatalf("err=%v, want only the target file refused", err)
	}
	if c, n := p.state(); c != 3 || n != 2 {
		t.Fatalf("collectors=%d targets=%d, want the new configuration and the old targets", c, n)
	}
	if p.manager.StaticTargets()[1].Collector != "b" {
		t.Fatal("the refused targets were installed")
	}
}

// The file the exporter started with is not a change: the first watch tick
// reloads nothing.
func TestTheFirstWatchTickReloadsNothing(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	p.manager.reloadChanged()
	if successes, failures := p.reloads(reloadFileConfig); successes != 0 || failures != 0 {
		t.Fatalf("an unchanged configuration was reloaded: %d successes, %d failures", successes, failures)
	}
}

// A configuration refused because a collector file it adds is broken is read
// again when that file is fixed, though the configuration file itself has not
// changed since; until then it is not read again on every tick.
func TestFixingAnAddedCollectorFileReloads(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	dir := filepath.Dir(p.configPath)
	extra := filepath.Join(dir, "extra.yaml")
	p.write(t, extra, "collectors:\n  - name: c\n    request: [broken\n")
	p.write(t, p.configPath, twoCollectors+"collector_files: [extra.yaml]\n")
	p.manager.reloadChanged()
	if _, failures := p.reloads(reloadFileConfig); failures != 1 {
		t.Fatalf("the broken collector file was not refused: %d failures", failures)
	}
	p.manager.reloadChanged()
	if _, failures := p.reloads(reloadFileConfig); failures != 1 {
		t.Fatalf("an unchanged refused configuration was read again: %d failures", failures)
	}
	if err := os.WriteFile(extra, []byte(strings.Replace(twoCollectors, "name: a", "name: c", 1)[:strings.Index(twoCollectors, "  - name: b")]), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(3 * time.Hour)
	if err := os.Chtimes(extra, later, later); err != nil {
		t.Fatal(err)
	}
	p.manager.reloadChanged()
	if c, _ := p.state(); c != 3 {
		t.Fatalf("after the fix: %d collectors, want 3", c)
	}
}

// A rejected reload names the file it rejected, so an error such as a YAML
// line number can be found; each file's rejection names its own.
func TestARejectedReloadNamesTheFile(t *testing.T) {
	p := newPair(t, twoCollectors, twoTargets)
	out := testutil.CaptureLogs(t)
	p.manager.logger = slog.Default()
	p.write(t, p.configPath, "collectors: [\n")
	_ = p.manager.Reload(ReloadTriggerSignal)
	p.write(t, p.configPath, twoCollectors)
	p.write(t, p.targetsAt, "targets: [\n")
	_ = p.manager.Reload(ReloadTriggerSignal)
	rejected := map[string]string{}
	for _, record := range testutil.AssertJSONLines(t, out, len(strings.Split(strings.TrimSpace(out.String()), "\n"))) {
		if msg, _ := record["msg"].(string); strings.HasSuffix(msg, "reload rejected") {
			file, _ := record["file"].(string)
			rejected[msg] = file
		}
	}
	if got := rejected["configuration reload rejected"]; got != p.configPath {
		t.Errorf("the configuration's rejection names file %q, want %q", got, p.configPath)
	}
	if got := rejected["static target reload rejected"]; got != p.targetsAt {
		t.Errorf("the static target file's rejection names file %q, want %q", got, p.targetsAt)
	}
}
