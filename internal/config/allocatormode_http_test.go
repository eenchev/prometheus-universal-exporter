//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What the manager does for an exporter in allocator mode
// (allocatormode.go): starting with nothing in force, a static target file
// that may be absent, checking a bundle where it was written, and replacing
// the files in force under the reload lock.

// A manager made to await its configuration holds an empty one, reports no
// reload status, and stays awaiting through a reload that is refused, here
// for files not there yet; the first reload that puts a configuration in
// force ends it. A manager made with a configuration never awaits.
func TestAnAwaitingManagerHoldsAnEmptyConfigurationUntilOneIsPutInForce(t *testing.T) {
	dir := t.TempDir()
	configPath, targetsPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "static-targets.yaml")
	m := NewAwaitingManager(configPath, testutil.QuietLogger(t))
	m.SetTargets(targetsPath, nil)
	m.AllowAbsentTargets()
	if cfg := m.Get(); !m.Awaiting() || cfg == nil || len(cfg.Collectors) != 0 || m.StaticTargets() != nil || len(m.ReloadMetrics()) != 0 {
		t.Fatalf("awaiting=%v config=%v targets=%v metrics=%v", m.Awaiting(), cfg, m.StaticTargets(), m.ReloadMetrics())
	}
	if err := m.Reload(ReloadTriggerSignal); err == nil || !m.Awaiting() {
		t.Fatalf("a reload without files: %v, awaiting=%v", err, m.Awaiting())
	}
	testutil.WriteIn(t, dir, "config.yaml", oneCollector)
	if err := m.Reload("allocator"); err != nil {
		t.Fatal(err)
	}
	if m.Awaiting() || len(m.Get().Collectors) != 1 || len(m.StaticTargets()) != 0 {
		t.Fatalf("after the first reload: awaiting=%v collectors=%d targets=%d", m.Awaiting(), len(m.Get().Collectors), len(m.StaticTargets()))
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if NewManager(cfg, configPath, testutil.QuietLogger(t)).Awaiting() {
		t.Error("a manager made with a configuration awaits")
	}
}

// With AllowAbsentTargets, a static target file that is gone is a file
// without targets: the reload puts none in force where it would otherwise be
// refused, and the file back puts its targets in force again.
func TestAnAbsentTargetFileIsNoTargetsWhenAllowed(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		p := newPair(t, oneCollector, oneTarget)
		if allowed {
			p.manager.AllowAbsentTargets()
		}
		if err := os.Remove(p.targetsAt); err != nil {
			t.Fatal(err)
		}
		err := p.manager.Reload("allocator")
		if !allowed {
			if err == nil || len(p.manager.StaticTargets()) != 1 {
				t.Fatalf("not allowed: %v, %d targets in force", err, len(p.manager.StaticTargets()))
			}
			continue
		}
		if err != nil || p.manager.StaticTargetFile() == nil || len(p.manager.StaticTargets()) != 0 {
			t.Fatalf("allowed: %v, %d targets in force", err, len(p.manager.StaticTargets()))
		}
		p.write(t, p.targetsAt, oneTarget)
		if err := p.manager.Reload("allocator"); err != nil || len(p.manager.StaticTargets()) != 1 {
			t.Fatalf("the file back: %v, %d targets in force", err, len(p.manager.StaticTargets()))
		}
	}
}

// oldLoadTargets is loadTargets as it was before AllowAbsentTargets, the
// oracle of the differential test below.
func oldLoadTargets(m *Manager) (*model.StaticTargetFile, error) {
	m.targetsRetryFiles, m.targetsRetryStamp, m.targetsRetryChecked = nil, "", nil
	f, err := LoadStaticTargets(m.targetPath, m.staticTargetsLoadOptions()...)
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Without AllowAbsentTargets, a manager loads a static target file as it did
// before it, present, absent, invalid or empty.
func TestLoadingATargetFileIsAsItWasWithoutAllowAbsentTargets(t *testing.T) {
	for name, body := range map[string]*string{"valid": ptrTo(oneTarget), "absent": nil, "empty": ptrTo(""), "no targets": ptrTo("interval: 1m\ntargets: []\n"), "not yaml": ptrTo("targets: [")} {
		dir := t.TempDir()
		path := filepath.Join(dir, "targets.yaml")
		if body != nil {
			testutil.WriteIn(t, dir, "targets.yaml", *body)
		}
		m := NewManager(&model.Config{}, filepath.Join(dir, "config.yaml"), testutil.QuietLogger(t))
		m.targetPath = path
		got, gotErr := m.loadTargets()
		want, wantErr := oldLoadTargets(m)
		if (gotErr == nil) != (wantErr == nil) || gotErr != nil && gotErr.Error() != wantErr.Error() || (got == nil) != (want == nil) || got != nil && len(got.Targets) != len(want.Targets) {
			t.Errorf("%s: got %v %v, want %v %v", name, got, gotErr, want, wantErr)
		}
	}
}

// CheckReplacement refuses what the reload would refuse of the files, the
// configuration on its own or the pair together, and lets an absent target
// file through; it puts nothing in force and records nothing.
func TestCheckReplacementChecksThePairWithoutPuttingItInForce(t *testing.T) {
	p := newPair(t, oneCollector, oneTarget)
	staged := t.TempDir()
	write := func(name, body string) string { return testutil.WriteIn(t, staged, name, body) }
	before, file := p.manager.InForce()
	metrics := len(p.manager.ReloadMetrics())
	if err := p.manager.CheckReplacement(write("config.yaml", twoCollectors), write("targets.yaml", twoTargets)); err != nil {
		t.Fatalf("a good pair: %v", err)
	}
	if err := p.manager.CheckReplacement(filepath.Join(staged, "config.yaml"), filepath.Join(staged, "absent.yaml")); err != nil {
		t.Fatalf("without a target file: %v", err)
	}
	if err := p.manager.CheckReplacement(write("bad.yaml", "collectors: []\n"), filepath.Join(staged, "absent.yaml")); err == nil {
		t.Fatal("a configuration without collectors was let through")
	}
	err := p.manager.CheckReplacement(write("one.yaml", oneCollector), filepath.Join(staged, "targets.yaml"))
	if err == nil || !strings.Contains(err.Error(), "static target file "+filepath.Join(staged, "targets.yaml")) || !strings.Contains(err.Error(), `unknown collector "b"`) {
		t.Fatalf("targets naming a collector the configuration lacks: %v", err)
	}
	if err := p.manager.CheckReplacement(filepath.Join(staged, "one.yaml"), write("empty.yaml", "interval: 1m\ntargets: []\n")); err == nil {
		t.Fatal("a target file without targets was let through")
	}
	if after, afterFile := p.manager.InForce(); after != before || afterFile != file || len(p.manager.ReloadMetrics()) != metrics {
		t.Fatal("the check changed what is in force or the reload status")
	}
	if s, f := p.reloads(reloadFileConfig); s != 0 || f != 0 {
		t.Fatalf("the check was counted as a reload: %d %d", s, f)
	}
}

// ReloadReplacing replaces the files under the reload lock, so no reload
// reads them half replaced, and reloads what it put in place. When the
// reload is refused the files are restored and reloaded, the pair in force
// is again the old one, and the refusal is returned; when the replacement
// fails, nothing is reloaded.
func TestReloadReplacingReplacesTheFilesUnderTheReloadLock(t *testing.T) {
	p := newPair(t, oneCollector, oneTarget)
	var lockedDuringReplace bool
	err := p.manager.ReloadReplacing("allocator", func() (func() error, error) {
		lockedDuringReplace = !p.manager.reloadMu.TryLock()
		if !lockedDuringReplace {
			p.manager.reloadMu.Unlock()
		}
		p.write(t, p.configPath, twoCollectors)
		p.write(t, p.targetsAt, twoTargets)
		return nil, nil
	})
	if err != nil || !lockedDuringReplace {
		t.Fatalf("replace: %v, under the lock: %v", err, lockedDuringReplace)
	}
	if len(p.manager.Get().Collectors) != 2 || len(p.manager.StaticTargets()) != 2 {
		t.Fatal("the files put in place were not reloaded")
	}
	var restored bool
	err = p.manager.ReloadReplacing("allocator", func() (func() error, error) {
		p.write(t, p.configPath, oneCollector)
		return func() error {
			restored = true
			p.write(t, p.configPath, twoCollectors)
			return nil
		}, nil
	})
	if err == nil || !restored || !strings.Contains(err.Error(), `unknown collector "b"`) {
		t.Fatalf("a refused replacement: %v, restored: %v", err, restored)
	}
	if cfg, f := p.manager.InForce(); len(cfg.Collectors) != 2 || len(f.Targets) != 2 {
		t.Fatalf("after the restore: %d collectors, %d targets", len(cfg.Collectors), len(f.Targets))
	}
	before, _ := p.manager.InForce()
	failed := errors.New("the disk is full")
	if err := p.manager.ReloadReplacing("allocator", func() (func() error, error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatalf("a failed replacement: %v", err)
	}
	if after, _ := p.manager.InForce(); after != before {
		t.Fatal("a failed replacement reloaded")
	}
	if names := collectorNames(p.manager.Get()); !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("collectors %v", names)
	}
}

func collectorNames(c *model.Config) []string {
	var out []string
	for i := range c.Collectors {
		out = append(out, c.Collectors[i].Name)
	}
	return out
}
