package config

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An exporter in allocator mode (--allocator.url) gets its configuration from
// the target allocator as a bundle of files. It writes the bundle into its
// cache directory and loads it there with the manager, as it would load
// mounted files: <cache-dir>/config.yaml with its collector files, and this
// replica's static targets in <cache-dir>/static-targets.yaml (absent when it
// has none). This file holds what the manager needs for that: starting with
// nothing in force, a static target file that may be absent, checking a
// bundle before it is put in place, and putting it in place so that no reload
// can see half of it.

// NewAwaitingManager returns a manager of the configuration at path that has
// no configuration in force yet. It is for an exporter in allocator mode that
// starts without a bundle in its cache directory. The manager holds an empty
// configuration, with no collectors and no static targets, so any reader of
// the configuration in force finds one. Awaiting is true until a reload puts
// a configuration in force; a static target file put in force alone does not
// end it. No reload status is reported for a file before it has been loaded.
func NewAwaitingManager(path string, l *slog.Logger) *Manager {
	m := &Manager{path: path, logger: l, Reloads: newReloadStatus()}
	m.current.Store(&inForce{config: &model.Config{}})
	m.awaiting.Store(true)
	return m
}

// Awaiting reports whether the manager was made with nothing in force
// (NewAwaitingManager) and no reload has put anything in force since. It is
// false for a manager made with NewManager.
func (m *Manager) Awaiting() bool { return m.awaiting.Load() }

// AllowAbsentTargets makes a missing static target file, at the path
// SetTargets gave, count as a file without targets. A reload that finds it
// gone puts no targets in force, instead of being refused for a file that
// cannot be read. A bundle without static targets for a replica has no such
// file. Call it before the manager is used.
func (m *Manager) AllowAbsentTargets() { m.absentTargets = true }

// targetsAbsent reports whether there is no file at path, as opposed to one
// that cannot be read.
func targetsAbsent(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// CheckReplacement checks the configuration at configPath and the static
// target file at targetsPath (absent for none) as a reload would check them
// in place of the files in force. It reads them with the same options, holds
// them to the same Python interpreter, and checks them together: a pair the
// reload would put in force only in part is refused. Nothing is put in force
// and nothing is recorded. An exporter in allocator mode uses it to check a
// bundle where it was written, before it replaces the files in force.
func (m *Manager) CheckReplacement(configPath, targetsPath string) error {
	c, err := Load(configPath, m.loadOptions()...)
	if err != nil {
		return err
	}
	if err := ValidatePythonScripts(m.pythonPath, c); err != nil {
		return err
	}
	if targetsAbsent(targetsPath) {
		return nil
	}
	f, err := LoadStaticTargets(targetsPath, m.staticTargetsLoadOptions()...)
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(f, c)
	}
	if err != nil {
		return &targetsError{path: targetsPath, err: err}
	}
	return nil
}

// targetsError names the static target file a problem was found in, as a
// reload's refusal does.
type targetsError struct {
	path string
	err  error
}

func (e *targetsError) Error() string { return "static target file " + e.path + ": " + e.err.Error() }
func (e *targetsError) Unwrap() error { return e.err }

// Generation returns the generation of the allocator's bundle that the
// configuration in force was read from. It is "" outside allocator mode and
// before the first bundle. It is stored with the configuration, in the same
// step that puts it in force. So once a bundle's configuration answers
// probes, its generation is the one returned, and a refused bundle's never is.
func (m *Manager) Generation() string { return m.current.Load().generation }

// SetGeneration records that the files at the manager's paths belong to the
// given generation of the allocator's bundles, and that the configuration in
// force was read from them. It is for an exporter in allocator mode that
// starts with the bundle in its cache directory. Call it before the manager
// is used.
func (m *Manager) SetGeneration(generation string) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.generation = generation
	pair := *m.current.Load()
	pair.generation = generation
	m.current.Store(&pair)
}

// ReloadReplacing replaces the files in force with replace and reloads them,
// both under the lock every reload takes. A reload triggered meanwhile (by
// the watch, SIGHUP or POST /-/reload) reads either the old files or the
// files replace left, never a mix, however long reading them takes. The files
// replace puts in place are the given generation of the allocator's bundles.
// What the reload puts in force carries that generation (Generation), and so
// does what any later reload reads from them.
// When replace fails, nothing is reloaded and its error is returned. When the
// reload of the new files is refused and replace returned a restore, restore
// puts the old files back and they are reloaded with the generation they had.
// The pair in force is then again the old one, read from the files in place,
// and the refusal is returned. Without restore the new files stay in place,
// and so does their generation.
func (m *Manager) ReloadReplacing(trigger, generation string, replace func() (restore func() error, err error)) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	restore, err := replace()
	if err != nil {
		return err
	}
	previous := m.generation
	m.generation = generation
	doTargets := m.targetPath != ""
	refused := m.apply(trigger, true, doTargets)
	if refused == nil || restore == nil {
		return refused
	}
	m.generation = previous
	if err := restore(); err != nil {
		return errors.Join(refused, err)
	}
	_ = m.apply(trigger, true, doTargets)
	return refused
}
