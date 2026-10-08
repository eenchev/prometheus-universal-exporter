//go:build !select_request_types || request_type_http || request_type_grpc

package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A reload used to check a pair of a static target file and a configuration
// as often as it asked about it, and to look through the configuration it
// read twice for the collectors whose descriptor files the check of a
// target file opens, when that was the one in force. The reload as it was is
// kept here, as an oracle: over generated sequences of reloads it and the
// reload now are made side by side, on two managers of the same files, and
// must leave everything alike.

// oldApply is Manager.apply as it was before each pair was checked once.
func oldApply(m *Manager, trigger string, doConfig, doTargets bool) error {
	var errs []error
	var cfg *model.Config
	var targets *model.StaticTargetFile
	if doConfig {
		c, err := m.loadConfig()
		if err != nil {
			errs = append(errs, m.rejectConfig(trigger, err, false))
		}
		cfg = c
	}
	if doTargets {
		f, err := m.loadTargets()
		if err != nil {
			errs = append(errs, m.rejectTargets(trigger, err, false))
		}
		targets = f
	}
	if cfg == nil && targets == nil {
		return errors.Join(errs...)
	}
	pairConfig, pairTargets := m.InForce()
	if cfg != nil {
		pairConfig = cfg
	}
	if targets != nil {
		pairTargets = targets
		m.targetsRetryChecked = targetsChecked(targets, pairConfig, m.Get())
		m.targetsRetryFiles = namedFiles(m.targetsRetryChecked)
		m.targetsRetryStamp = filesStamp(m.targetsRetryFiles)
		if m.WatchEnabled() {
			m.targetsRetryFiles, m.targetsRetryStamp, _ = retryImported(m.targetsRetryFiles, m.targetsRetryStamp, m.targetsRetryChecked)
		}
	}
	if agree(pairTargets, pairConfig) == nil {
		m.install(trigger, cfg, targets)
		return errors.Join(errs...)
	}
	if cfg != nil && agree(m.StaticTargetFile(), cfg) == nil {
		m.install(trigger, cfg, nil)
		cfg = nil
	}
	if targets != nil && agree(targets, m.Get()) == nil {
		m.install(trigger, nil, targets)
		targets = nil
	}
	if cfg != nil {
		errs = append(errs, m.rejectConfig(trigger, agree(m.StaticTargetFile(), cfg), true))
	}
	if targets != nil {
		errs = append(errs, m.rejectTargets(trigger, agree(targets, m.Get()), true))
	}
	return errors.Join(errs...)
}

// oldReloadChanged is Manager.reloadChanged making oldApply.
func oldReloadChanged(m *Manager) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	configChanged, descriptorsChanged, targetsChanged := m.configChanged(), m.descriptors.changed(), m.targetsChanged()
	doConfig := configChanged || descriptorsChanged || targetsChanged && m.configWaits
	doTargets := targetsChanged || configChanged && m.targetsWaits || descriptorsChanged && m.targetsFollowDescriptors()
	if !doConfig && !doTargets {
		return
	}
	_ = oldApply(m, reloadTriggerWatch, doConfig, doTargets)
}

// reloadFile is a file of a generated reload sequence and what may be
// written to it; removed is a variant that removes it. The first variant of
// each file is what the managers start with.
type reloadFile struct {
	path     string
	variants []string
}

const removed = "\x00removed"

// reloadSnapshot is all a reload leaves that can be told apart between two
// managers of the same files: what it returned and logged, what is in force
// and whether it was replaced, the reload metrics but their timestamps, and
// what the watch reads the files again for.
type reloadSnapshot struct {
	Err, Logs, Config, Targets, Metrics     string
	ConfigReplaced, TargetsReplaced         bool
	RetryFiles, TargetsRetryFiles           []string
	RetryStamp, TargetsRetryStamp           string
	RetryConfigs                            int
	RetryImports, ConfigWaits, TargetsWaits bool
	TargetsRetryNamed, WatchedFiles         []string
	CollectorFiles                          string
	Descriptors, Loading                    stampedFiles
	LastMod, TargetsLastMod                 time.Time
	LastSize, TargetsLastSize               int64
}

// snapshot is m's reloadSnapshot after a reload that returned err and
// logged logs, the pair before it having been was.
func snapshot(m *Manager, err error, logs *bytes.Buffer, was inForce) reloadSnapshot {
	s := reloadSnapshot{Logs: logs.String()}
	logs.Reset()
	if err != nil {
		s.Err = err.Error()
	}
	cfg, file := m.InForce()
	s.ConfigReplaced, s.TargetsReplaced = cfg != was.config, file != was.targets
	for i := range cfg.Collectors {
		s.Config += cfg.Collectors[i].Name + " "
	}
	if file != nil {
		for i := range file.Targets {
			t := &file.Targets[i]
			s.Targets += t.Name + "/" + t.Collector + "/" + t.Request.Message + " "
		}
	}
	for _, metric := range m.ReloadMetrics() {
		if !strings.HasSuffix(metric.Name, "_timestamp_seconds") {
			s.Metrics += fmt.Sprintf("%s %v %v\n", metric.Name, metric.Labels, metric.Value)
		}
	}
	s.RetryFiles, s.RetryStamp, s.RetryConfigs, s.RetryImports = m.retryFiles, m.retryStamp, len(m.retryConfigs), m.retryImports
	s.TargetsRetryFiles, s.TargetsRetryStamp, s.TargetsRetryNamed = m.targetsRetryFiles, m.targetsRetryStamp, namedFiles(m.targetsRetryChecked)
	s.ConfigWaits, s.TargetsWaits = m.configWaits, m.targetsWaits
	s.WatchedFiles, s.CollectorFiles = m.watchedFiles, m.collectorFiles
	s.Descriptors, s.Loading = m.descriptors, m.loading
	s.LastMod, s.LastSize, s.TargetsLastMod, s.TargetsLastSize = m.lastMod, m.lastSize, m.targetsLastMod, m.targetsLastSize
	return s
}

// reloadOutcomes counts what the reloads of a sequence did, so a test can
// see that its corpus reaches each outcome.
type reloadOutcomes map[string]int

// compareReloads makes sequences of reloads of the files world returns for
// a fresh directory — the configuration first, the static target file
// second — on a manager making oldApply and one making apply, both started
// from the first variant of each file, with the watch on or off: at each
// step some files are written or removed, and a reload is asked for,
// a tick of the watch made, or the configuration, the target file or both
// read. After each step the two must be alike (reloadSnapshot).
func compareReloads(t *testing.T, sequences int, world func(t *testing.T, dir string) []reloadFile) reloadOutcomes {
	t.Helper()
	outcomes := reloadOutcomes{}
	for seed := range sequences {
		r := rand.New(rand.NewPCG(uint64(seed), 47))
		files := world(t, t.TempDir())
		for _, f := range files {
			writeVariant(t, f.path, f.variants[0])
		}
		watch := r.IntN(2) == 0
		var managers [2]*Manager
		var logs [2]*bytes.Buffer
		for i := range managers {
			m := managerOf(t, files[0].path, files[1].path)
			logs[i] = &bytes.Buffer{}
			m.logger = slog.New(slog.NewTextHandler(logs[i], &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey && len(groups) == 0 {
					return slog.Attr{}
				}
				return a
			}}))
			if watch {
				m.SetWatchInterval(time.Minute)
			}
			managers[i] = m
		}
		for step := range 8 {
			for _, f := range files {
				if r.IntN(2) == 0 {
					writeVariant(t, f.path, f.variants[r.IntN(len(f.variants))])
				}
			}
			kind := r.IntN(5)
			var snaps [2]reloadSnapshot
			for i, m := range managers {
				config, targets := m.InForce()
				was := inForce{config: config, targets: targets}
				var err error
				switch kind {
				case 0:
					if i == 0 {
						m.reloadMu.Lock()
						err = oldApply(m, ReloadTriggerHTTP, true, true)
						m.reloadMu.Unlock()
					} else {
						err = m.Reload(ReloadTriggerHTTP)
					}
				case 1:
					if i == 0 {
						oldReloadChanged(m)
					} else {
						m.reloadChanged()
					}
				default:
					doConfig, doTargets := kind != 3, kind != 2
					m.reloadMu.Lock()
					if i == 0 {
						err = oldApply(m, ReloadTriggerSignal, doConfig, doTargets)
					} else {
						err = m.apply(ReloadTriggerSignal, doConfig, doTargets)
					}
					m.reloadMu.Unlock()
				}
				snaps[i] = snapshot(m, err, logs[i], was)
			}
			if !reflect.DeepEqual(snaps[0], snaps[1]) {
				t.Fatalf("sequence %d (watch %t), step %d, reload %d: the reload left\n%+v\nwhere it left\n%+v", seed, watch, step, kind, snaps[1], snaps[0])
			}
			outcomes.count(snaps[0], watch)
		}
	}
	return outcomes
}

// count notes what a reload did: each file installed, refused for itself or
// refused for the other (it waits for it), with the watch on or off; one
// file going alone while the other is refused for it, and both refused for
// each other; and the watch reading files again for a refused target file.
func (o reloadOutcomes) count(s reloadSnapshot, watch bool) {
	installed, refusedForOther := 0, 0
	for _, line := range strings.Split(s.Logs, "\n") {
		var waits bool
		switch {
		case strings.Contains(line, `msg="configuration reloaded"`), strings.Contains(line, `msg="static targets reloaded"`):
			installed++
			o["file installed"]++
			continue
		case strings.Contains(line, `msg="configuration reload rejected"`):
			waits = s.ConfigWaits
		case strings.Contains(line, `msg="static target reload rejected"`):
			waits = s.TargetsWaits
		default:
			continue
		}
		what := fmt.Sprintf("refused for itself, watch %t", watch)
		if waits {
			what = fmt.Sprintf("refused for the other, watch %t", watch)
			refusedForOther++
		}
		o[what]++
	}
	switch {
	case installed == 1 && refusedForOther == 1:
		o["one went alone, the other refused for it"]++
	case refusedForOther == 2:
		o["both refused for each other"]++
	}
	if len(s.TargetsRetryFiles) > 0 {
		o["a refused target file's check opened files"]++
	}
}

// writeVariant writes body to path, later than any write before it, or
// removes the file for removed.
func writeVariant(t *testing.T, path, body string) {
	t.Helper()
	if body == removed {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return
	}
	(&pair{}).write(t, path, body)
}

// managerOf is a manager of the configuration and the static target file
// at the paths given, loaded as at startup.
func managerOf(t *testing.T, configPath, targetsAt string) *Manager {
	t.Helper()
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(targetsAt)
	if err == nil {
		err = ValidateStaticTargets(file)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, configPath, nil)
	m.SetTargets(targetsAt, file)
	return m
}

// countTargetChecks counts, until the test or the benchmark ends, the
// checks of a static target file against a configuration
// (targetsValidatedHook).
func countTargetChecks(tb testing.TB) *atomic.Int64 {
	tb.Helper()
	var count atomic.Int64
	hook := func() { count.Add(1) }
	targetsValidatedHook.Store(&hook)
	tb.Cleanup(func() { targetsValidatedHook.Store(nil) })
	return &count
}

// wantOutcomes fails unless the corpus reached each of want at least least
// times.
func wantOutcomes(t *testing.T, got reloadOutcomes, least int, want ...string) {
	t.Helper()
	for _, what := range slices.Sorted(slices.Values(want)) {
		if got[what] < least {
			t.Errorf("%d reloads: %s, want at least %d (all: %v)", got[what], what, least, got)
		}
	}
}
