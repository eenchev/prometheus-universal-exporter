package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"

	"gopkg.in/yaml.v3"
)

// A reload that succeeds says what it changed: the info line it logs for
// the configuration names the collectors it added, removed and changed, the
// one for the static target file the static targets, and the landing page
// shows the last reload's changes with its time. The self-metrics say that
// a reload went through (reloadstatus.go), not what it did; after an edit to
// a ConfigMap, a bundle from the allocator or a reload asked for by hand,
// the question is whether the collector that was edited is the one now in
// force, and the count alone does not answer it.

// reloadChangesNamed is how many names each list of a summary gives before
// it counts the rest: a reload that replaces a thousand static targets
// writes one line of bounded length, not the thousand names.
const reloadChangesNamed = 10

// ReloadSummary is what the last reload that put something in force
// changed, and when it was put in force.
type ReloadSummary struct {
	At time.Time
	// Changes names the collectors and static targets added, removed and
	// changed, a list at a time, or says "no changes".
	Changes string
}

// LastReload is the summary of the last reload that put something in force
// since the exporter started, and false when none has.
func (m *Manager) LastReload() (ReloadSummary, bool) {
	s := m.lastReload.Load()
	if s == nil {
		return ReloadSummary{}, false
	}
	return *s, true
}

// SetFingerprint makes fingerprint how a reload tells whether a collector
// kept under its name changed: fingerprint(cfg, i) is a digest of the
// definition of cfg's collector at i. The exporter keys a probe's cached
// results by such digests, and works them out for a configuration once,
// before a reload puts it in force (OnPrepare); with them, telling what the
// reload changed encodes no definition again. Without one, a collector's
// YAML encoding is compared. It is called under the lock that serializes
// reloads, before what the reload read is in force.
func (m *Manager) SetFingerprint(fingerprint func(cfg *model.Config, index int) string) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.fingerprint = fingerprint
}

// definitionEncoded, when set, is called whenever a reload encodes a
// collector's definition itself (SetDefinitionEncoded).
var definitionEncoded atomic.Pointer[func()]

// SetDefinitionEncoded asks for encoded to be called whenever a reload
// encodes a collector's definition to tell what it changed, having been
// given no fingerprint (SetFingerprint), and nil stops it. It is for tests:
// an exporter's reload reads the fingerprints the exporter made, and
// encodes nothing here.
func SetDefinitionEncoded(encoded func()) {
	if encoded == nil {
		definitionEncoded.Store(nil)
		return
	}
	definitionEncoded.Store(&encoded)
}

// encodedDefinition is the YAML encoding of cfg's collector at index, which
// holds what is configured and nothing compiled from it: a configuration is
// loaded anew on every reload, so an expression compiled for it is a new
// value even when nothing written changed.
func encodedDefinition(cfg *model.Config, index int) string {
	if hook := definitionEncoded.Load(); hook != nil {
		(*hook)()
	}
	encoded, err := yaml.Marshal(&cfg.Collectors[index])
	if err != nil {
		// Never the case for a loaded collector; one that cannot be
		// encoded is taken to have changed.
		return err.Error()
	}
	return string(encoded)
}

// changeLists are the names of what a reload added, removed and changed.
type changeLists struct{ added, removed, changed []string }

// collectorChanges are the names of the collectors added, removed and
// changed from before to after, each sorted. A collector kept under its
// name is changed when fingerprint tells its definitions apart; one added
// or removed is told by its name alone, and its definition never read.
func collectorChanges(before, after *model.Config, fingerprint func(*model.Config, int) string) changeLists {
	var added, removed, changed []string
	if fingerprint == nil {
		fingerprint = encodedDefinition
	}
	was := map[string]int{}
	if before != nil {
		for i := range before.Collectors {
			was[before.Collectors[i].Name] = i
		}
	}
	kept := map[string]bool{}
	if after != nil {
		for i := range after.Collectors {
			name := after.Collectors[i].Name
			index, ok := was[name]
			switch {
			case !ok:
				added = append(added, name)
			case fingerprint(before, index) != fingerprint(after, i):
				changed = append(changed, name)
			}
			kept[name] = true
		}
	}
	for name := range was {
		if !kept[name] {
			removed = append(removed, name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return changeLists{added, removed, changed}
}

// targetChanges are the names of the static targets added, removed and
// changed from before to after, each sorted. Targets are compared as they
// are, as the schedule of the static targets compares them (reconcile.go in
// the exporter): a target holds only what was written, filled with the
// file's defaults.
func targetChanges(before, after *model.StaticTargetFile) changeLists {
	var added, removed, changed []string
	was := map[string]*model.StaticTarget{}
	if before != nil {
		for i := range before.Targets {
			was[before.Targets[i].Name] = &before.Targets[i]
		}
	}
	kept := map[string]bool{}
	if after != nil {
		for i := range after.Targets {
			target := &after.Targets[i]
			previous, ok := was[target.Name]
			switch {
			case !ok:
				added = append(added, target.Name)
			case !reflect.DeepEqual(*previous, *target):
				changed = append(changed, target.Name)
			}
			kept[target.Name] = true
		}
	}
	for name := range was {
		if !kept[name] {
			removed = append(removed, name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return changeLists{added, removed, changed}
}

// reloadChanges says what a reload changed of what, "collectors" or
// "static targets", given the names it added, removed and changed (lists):
// "collectors added: a, b; collectors changed: c", the lists that are not
// empty in that order, each naming reloadChangesNamed names and counting the
// rest, or "no changes".
func reloadChanges(what string, lists changeLists) string {
	var parts []string
	for _, list := range []struct {
		how   string
		names []string
	}{{"added", lists.added}, {"removed", lists.removed}, {"changed", lists.changed}} {
		if len(list.names) == 0 {
			continue
		}
		shown := list.names[:min(len(list.names), reloadChangesNamed)]
		text := what + " " + list.how + ": " + strings.Join(shown, ", ")
		if rest := len(list.names) - len(shown); rest > 0 {
			text += fmt.Sprintf(" and %d more", rest)
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return noReloadChanges
	}
	return strings.Join(parts, "; ")
}

// noReloadChanges is what a reload that changed nothing says.
const noReloadChanges = "no changes"

// joinReloadChanges is the summary of a reload that put in force a file
// that changed one thing, another that changed another, or both: what each
// changed, or "no changes" when neither changed anything.
func joinReloadChanges(changes ...string) string {
	var parts []string
	for _, c := range changes {
		if c != "" && c != noReloadChanges {
			parts = append(parts, c)
		}
	}
	if len(parts) == 0 {
		return noReloadChanges
	}
	return strings.Join(parts, "; ")
}
