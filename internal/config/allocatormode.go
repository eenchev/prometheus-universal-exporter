package config

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An exporter in allocator mode (--allocator.url) is given its configuration
// by the target allocator as a bundle of files, which it writes into its
// cache directory and loads there with the manager, as it would load mounted
// files: <cache-dir>/config.yaml with its collector files, and this replica's
// static targets in <cache-dir>/static-targets.yaml, absent when it has none.
// What the manager needs for that is here: starting with nothing in force,
// a static target file that may be absent, checking a bundle before it is
// put in place, and putting it in place where no reload can see half of it.

// NewAwaitingManager returns a manager of the configuration at path that has
// none in force yet: an exporter in allocator mode that starts without a
// bundle in its cache directory. It holds an empty configuration, with no
// collectors and no static targets, so whatever reads the configuration in
// force finds one, and Awaiting is true until a reload puts a configuration
// in force: a static target file put in force alone does not end it.
// No reload status is reported for a file before it has been loaded.
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

// AllowAbsentTargets makes a static target file that does not exist, at the
// path SetTargets gave, a file without targets: a reload that finds it gone
// puts no targets in force, where it would otherwise be refused as a file
// that cannot be read. A bundle without static targets for a replica has no
// such file. Call it before the manager is used.
func (m *Manager) AllowAbsentTargets() { m.absentTargets = true }

// targetsAbsent reports whether there is no file at path, as opposed to one
// that cannot be read.
func targetsAbsent(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// CheckReplacement checks the configuration at configPath and the static
// target file at targetsPath, absent for none, as a reload would check them
// in place of the files in force, read with the same options and held to
// the same Python interpreter, and together: a pair the reload would put in
// force only in part is refused. Nothing is put in force and nothing is
// recorded. An exporter in allocator mode checks a bundle with it where it
// was written, before it replaces the files in force.
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

// Generation returns the generation of the allocator's bundle the
// configuration in force was read from, "" outside allocator mode and before
// the first bundle. It is stored with the configuration, in the same step
// that puts it in force: from the moment a bundle's configuration answers
// probes its generation is the one named, and a bundle refused never is.
func (m *Manager) Generation() string { return m.current.Load().generation }

// SetGeneration says the files at the manager's paths are generation of the
// allocator's bundles, and the configuration in force was read from them:
// an exporter in allocator mode that starts with the bundle in its cache
// directory. Call it before the manager is used.
func (m *Manager) SetGeneration(generation string) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.generation = generation
	pair := *m.current.Load()
	pair.generation = generation
	m.current.Store(&pair)
}

// ReloadReplacing replaces the files in force with replace and reloads them,
// both under the lock every reload takes: a reload triggered meanwhile, by
// the watch, SIGHUP or POST /-/reload, reads either the files as they were
// or as replace left them, never some of each, however long reading them
// takes. The files replace puts in place are generation of the allocator's
// bundles: what the reload puts in force carries it (Generation), and so
// does what any later reload reads from them. When replace fails nothing is
// reloaded and its error is returned. When the reload of the new files is
// refused, restore, when replace returned one, puts the old files back and
// they are reloaded with the generation they had, so the pair in force is
// again the old one, read from the files in place; the refusal is returned.
// Without restore the new files stay in place, and so does their generation.
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
