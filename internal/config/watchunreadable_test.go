//go:build !select_request_types || request_type_http

package config

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A reload refused because a file could not be read was never tried again
// when the operator then made it readable: the watch stamped the files by
// their modification time and size, which a chmod does not change, so the
// exporter stayed on the old configuration, reported as refused, until
// something else changed. The stamps now hold each file's mode, and a file
// refused because a file it reads could not be read is read again at every
// tick, for what no stamp holds — an owner, an ACL, a directory's
// permissions — quietly while it is refused for the same reason.

// unreadableWorld is a configuration whose own collector is a and whose
// collector file more.yaml defines b, with a static target file whose one
// target uses a, each file written once with its own time; a manager of
// them with the watch on; and what the manager logs.
type unreadableWorld struct {
	manager                    *Manager
	config, collectors, target string
	logs                       *bytes.Buffer
	// reads counts the reads of each path by readDocument, and refused are
	// the paths it is refused, as a file is whose directory or ACL the
	// user may not read.
	reads   map[string]int
	refused map[string]bool
}

const unreadableTargets = "interval: 1m\ntargets:\n  - name: ta\n    collector: a\n    target: http://a.invalid\n"

func newUnreadableWorld(t *testing.T) *unreadableWorld {
	t.Helper()
	dir := t.TempDir()
	w := &unreadableWorld{
		collectors: testutil.WriteIn(t, dir, "more.yaml", testutil.CollectorsDocument("b")),
		config:     testutil.WriteIn(t, dir, "config.yaml", "collector_files: [more.yaml]\n"+testutil.CollectorsDocument("a")),
		target:     testutil.WriteIn(t, dir, "targets.yaml", unreadableTargets),
		logs:       &bytes.Buffer{},
		reads:      map[string]int{},
		refused:    map[string]bool{},
	}
	at := time.Unix(1_700_000_000, 0)
	for i, path := range []string{w.config, w.collectors, w.target} {
		when := at.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	// A file whose permissions let nobody read it is refused as it is to a
	// user without privileges, so the tests hold whoever runs them, root
	// included, who reads it whatever its mode.
	readFile = func(path string) ([]byte, error) {
		w.reads[path]++
		if st, err := os.Stat(path); err == nil && (st.Mode().Perm()&0o444 == 0 || w.refused[path]) {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		}
		return os.ReadFile(path)
	}
	t.Cleanup(func() { readFile = os.ReadFile })
	stamp := TakeStamp(w.config, true)
	cfg, err := LoadStamped(w.config, &stamp)
	if err != nil {
		t.Fatal(err)
	}
	targetsStamp := TakeStamp(w.target, false)
	file, err := LoadStaticTargets(w.target)
	if err != nil {
		t.Fatal(err)
	}
	w.manager = NewManager(cfg, w.config, slog.New(slog.NewJSONHandler(w.logs, nil)))
	w.manager.SetTargets(w.target, file)
	w.manager.UseStamp(stamp)
	w.manager.UseTargetsStamp(targetsStamp)
	w.manager.SetWatchInterval(time.Minute)
	return w
}

// chmodKeepingTime sets the permissions of path and its modification time
// back to when.
func chmodKeepingTime(t *testing.T, path string, mode os.FileMode, when time.Time) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// tick is one tick of the watch, and what it logged.
func (w *unreadableWorld) tick(t *testing.T) []map[string]any {
	t.Helper()
	w.manager.reloadChanged()
	var lines []map[string]any
	if w.logs.Len() > 0 {
		lines = testutil.AssertJSONLines(t, w.logs, strings.Count(w.logs.String(), "\n"))
	}
	w.logs.Reset()
	return lines
}

func (w *unreadableWorld) failures(file string) uint64 {
	if st := w.manager.Reloads.files[file]; st != nil {
		return st.failures
	}
	return 0
}

func (w *unreadableWorld) targetCollector() string { return w.manager.StaticTargets()[0].Collector }

// For each file the watch reads — the configuration, a collector file, the
// static target file — a new version written with permissions that let
// nobody read it is a reload refused, logged once with the reason and
// counted once, retried_when saying it is read again at every tick; the
// ticks that follow read it again and log and count nothing; and the
// permissions put right, its modification time and size as they were, put
// the new version in force at the next tick.
func TestAFileMadeReadableAgainIsReloadedAlthoughOnlyItsPermissionsChanged(t *testing.T) {
	for _, which := range []string{"configuration", "collector file", "static target file"} {
		t.Run(which, func(t *testing.T) {
			w := newUnreadableWorld(t)
			path, body, file, msg := w.config, "collector_files: [more.yaml]\n"+testutil.CollectorsDocument("a", "c"), reloadFileConfig, "configuration"
			switch which {
			case "collector file":
				path, body = w.collectors, testutil.CollectorsDocument("c")
			case "static target file":
				path, body, file, msg = w.target, strings.Replace(unreadableTargets, "collector: a", "collector: b", 1), ReloadFileStaticTargets, "static target"
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			later := st.ModTime().Add(time.Hour)
			chmodKeepingTime(t, path, 0, later)
			lines := w.tick(t)
			if len(lines) != 1 || lines[0]["msg"] != msg+" reload rejected" || !strings.Contains(lines[0]["error"].(string), "open "+path+": permission denied") ||
				!strings.HasSuffix(lines[0]["retried_when"].(string), " changes, and at every tick while a file it reads cannot be read") || w.failures(file) != 1 {
				t.Fatalf("made unreadable: logged %v, %d reloads refused", lines, w.failures(file))
			}
			for range 3 {
				reads := w.reads[path]
				if lines := w.tick(t); len(lines) != 0 || w.reads[path] == reads || w.failures(file) != 1 {
					t.Fatalf("a tick while it is unreadable: logged %v, read %t, %d reloads refused", lines, w.reads[path] > reads, w.failures(file))
				}
			}
			chmodKeepingTime(t, path, 0o600, later)
			lines = w.tick(t)
			names := testutil.CollectorNames(w.manager.Get())
			if file == ReloadFileStaticTargets {
				if len(lines) != 1 || lines[0]["msg"] != "static targets reloaded" || w.targetCollector() != "b" {
					t.Fatalf("readable again: logged %v, the target uses %s", lines, w.targetCollector())
				}
			} else if want := map[string][]string{"configuration": {"a", "c", "b"}, "collector file": {"a", "c"}}[which]; len(lines) != 1 || lines[0]["msg"] != "configuration reloaded" || !reflect.DeepEqual(names, want) {
				t.Fatalf("readable again: logged %v, collectors %v, want %v", lines, names, want)
			}
			if lines := w.tick(t); len(lines) != 0 {
				t.Fatalf("a tick after the reload logged %v", lines)
			}
		})
	}
}

// A file refused because it could not be read for what no stamp holds — its
// directory's permissions, its owner, an ACL — is read again at every tick,
// and is in force at the tick after it can be read, nothing about it having
// changed; for the configuration a collector file of which it was, for the
// static target file the target file itself.
func TestAFileRefusedForWhatNoStampHoldsIsReadAgainAtEveryTick(t *testing.T) {
	for _, which := range []string{"configuration", "collector file", "static target file"} {
		t.Run(which, func(t *testing.T) {
			w := newUnreadableWorld(t)
			path, file := w.config, reloadFileConfig
			switch which {
			case "collector file":
				path = w.collectors
			case "static target file":
				path, file = w.target, ReloadFileStaticTargets
			}
			w.refused[path] = true
			if err := w.manager.Reload(ReloadTriggerSignal); err == nil || !strings.Contains(err.Error(), "open "+path+": permission denied") {
				t.Fatalf("a reload with %s unreadable: %v", path, err)
			}
			w.logs.Reset()
			for range 3 {
				reads := w.reads[path]
				if lines := w.tick(t); len(lines) != 0 || w.reads[path] == reads || w.failures(file) != 1 {
					t.Fatalf("a tick while it is unreadable: logged %v, read %t, %d reloads refused", lines, w.reads[path] > reads, w.failures(file))
				}
			}
			w.refused[path] = false
			msg := map[string]string{reloadFileConfig: "configuration reloaded", ReloadFileStaticTargets: "static targets reloaded"}[file]
			if lines := w.tick(t); len(lines) != 1 || lines[0]["msg"] != msg || lines[0]["trigger"] != reloadTriggerWatch || len(w.manager.Reloads.Rejected()) != 0 {
				t.Fatalf("readable again: logged %v, rejected %v", lines, w.manager.Reloads.Rejected())
			}
			reads := w.reads[path]
			if lines := w.tick(t); len(lines) != 0 || w.reads[path] != reads {
				t.Fatalf("a tick after the reload: logged %v, read %t", lines, w.reads[path] > reads)
			}
		})
	}
}

// A try at a tick that is refused for another reason than the last refusal
// — a second collector file unreadable too — is a refusal of its own, logged
// and counted once, and the tries after it are quiet again.
func TestATryRefusedForAnotherReasonIsLoggedAndCountedOnce(t *testing.T) {
	w := newUnreadableWorld(t)
	other := testutil.WriteIn(t, filepath.Dir(w.config), "other.yaml", testutil.CollectorsDocument("d"))
	testutil.WriteIn(t, filepath.Dir(w.config), "config.yaml", "collector_files: [more.yaml, other.yaml]\n"+testutil.CollectorsDocument("a"))
	w.refused[w.collectors] = true
	if err := w.manager.Reload(ReloadTriggerSignal); err == nil {
		t.Fatal("a reload with a collector file unreadable was accepted")
	}
	w.logs.Reset()
	w.refused[other] = true
	lines := w.tick(t)
	if len(lines) != 1 || !strings.Contains(lines[0]["error"].(string), "open "+other+": permission denied") || w.failures(reloadFileConfig) != 2 {
		t.Fatalf("a try refused for another file too: logged %v, %d reloads refused", lines, w.failures(reloadFileConfig))
	}
	if lines := w.tick(t); len(lines) != 0 || w.failures(reloadFileConfig) != 2 {
		t.Fatalf("the try after it: logged %v, %d reloads refused", lines, w.failures(reloadFileConfig))
	}
	w.refused[w.collectors], w.refused[other] = false, false
	if lines := w.tick(t); len(lines) != 1 || lines[0]["msg"] != "configuration reloaded" || !reflect.DeepEqual(testutil.CollectorNames(w.manager.Get()), []string{"a", "b", "d"}) {
		t.Fatalf("both readable: logged %v, collectors %v", lines, testutil.CollectorNames(w.manager.Get()))
	}
}

// A file refused for what it says — not YAML — is not read again until it
// changes, whichever of the files it is: the ticks that follow read nothing
// and log nothing, and its line promises no try at every tick.
func TestAFileRefusedForWhatItSaysIsNotReadAgainAtEveryTick(t *testing.T) {
	for _, which := range []string{"configuration", "collector file", "static target file"} {
		t.Run(which, func(t *testing.T) {
			w := newUnreadableWorld(t)
			path, file := w.config, reloadFileConfig
			switch which {
			case "collector file":
				path = w.collectors
			case "static target file":
				path, file = w.target, ReloadFileStaticTargets
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("collectors: [\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			later := st.ModTime().Add(time.Hour)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
			lines := w.tick(t)
			if len(lines) != 1 || !strings.HasSuffix(lines[0]["msg"].(string), "reload rejected") || strings.Contains(lines[0]["retried_when"].(string), "every tick") || w.failures(file) != 1 {
				t.Fatalf("not YAML: logged %v, %d reloads refused", lines, w.failures(file))
			}
			reads := w.reads[path]
			for range 3 {
				if lines := w.tick(t); len(lines) != 0 || w.reads[path] != reads || w.failures(file) != 1 {
					t.Fatalf("a tick after it: logged %v, read %t, %d reloads refused", lines, w.reads[path] > reads, w.failures(file))
				}
			}
		})
	}
}

// A static target file refused only because the configuration in force
// does not have its collector waits for the configuration; when the
// configuration that has it cannot be read, the target file is tried again
// with it at every tick, and both go in force at the tick after the
// configuration can be read, the waiting file's refusal logged once.
func TestATargetFileWaitingForAnUnreadableConfigurationIsTriedWithIt(t *testing.T) {
	w := newUnreadableWorld(t)
	st, err := os.Stat(w.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.target, []byte(strings.Replace(unreadableTargets, "collector: a", "collector: c", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(w.target, st.ModTime().Add(time.Hour), st.ModTime().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if lines := w.tick(t); len(lines) != 1 || lines[0]["msg"] != "static target reload rejected" || !w.manager.targetsWaits {
		t.Fatalf("a target of a collector the configuration has not: logged %v", lines)
	}
	st, err = os.Stat(w.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.config, []byte("collector_files: [more.yaml]\n"+testutil.CollectorsDocument("a", "c")), 0o600); err != nil {
		t.Fatal(err)
	}
	later := st.ModTime().Add(time.Hour)
	w.refused[w.config] = true
	if err := os.Chtimes(w.config, later, later); err != nil {
		t.Fatal(err)
	}
	if lines := w.tick(t); len(lines) != 2 || lines[0]["msg"] != "configuration reload rejected" || lines[1]["msg"] != "static target reload rejected" {
		t.Fatalf("the configuration that has it, unreadable: logged %v", lines)
	}
	for range 2 {
		reads := w.reads[w.target]
		if lines := w.tick(t); len(lines) != 0 || w.reads[w.target] == reads || w.failures(ReloadFileStaticTargets) != 2 || w.failures(reloadFileConfig) != 1 {
			t.Fatalf("a tick while the configuration is unreadable: logged %v, the target file read %t", lines, w.reads[w.target] > reads)
		}
	}
	w.refused[w.config] = false
	if lines := w.tick(t); len(lines) != 2 || lines[0]["msg"] != "configuration reloaded" || lines[1]["msg"] != "static targets reloaded" || w.targetCollector() != "c" {
		t.Fatalf("the configuration readable: logged %v, the target uses %s", lines, w.targetCollector())
	}
}

// The permissions of a file in force changed, and nothing else, are a
// change the watch reloads for, whichever of the files it is: they may have
// made it unreadable, which the reload then says at that tick.
func TestTheWatchReloadsAFileWhosePermissionsAloneChanged(t *testing.T) {
	for _, which := range []string{"configuration", "collector file", "static target file"} {
		t.Run(which, func(t *testing.T) {
			w := newUnreadableWorld(t)
			path, msg := w.config, "configuration reloaded"
			switch which {
			case "collector file":
				path = w.collectors
			case "static target file":
				path, msg = w.target, "static targets reloaded"
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if lines := w.tick(t); len(lines) != 0 {
				t.Fatalf("a tick with nothing changed logged %v", lines)
			}
			chmodKeepingTime(t, path, 0o640, st.ModTime())
			if lines := w.tick(t); len(lines) != 1 || lines[0]["msg"] != msg {
				t.Fatalf("the permissions changed: logged %v", lines)
			}
			if lines := w.tick(t); len(lines) != 0 {
				t.Fatalf("the tick after it logged %v", lines)
			}
		})
	}
}

// So the other way: a configuration refused only because the static target
// file in force uses a collector it removes waits for the target file, and
// is tried again with it at every tick while the target file that no longer
// uses it cannot be read; both go in force at the tick after it can be.
func TestAConfigurationWaitingForAnUnreadableTargetFileIsTriedWithIt(t *testing.T) {
	w := newUnreadableWorld(t)
	testutil.WriteIn(t, filepath.Dir(w.config), "config.yaml", "collector_files: [more.yaml]\n"+testutil.CollectorsDocument("c"))
	if lines := w.tick(t); len(lines) != 1 || lines[0]["msg"] != "configuration reload rejected" || !w.manager.configWaits {
		t.Fatalf("a configuration without the collector of a target: logged %v", lines)
	}
	testutil.WriteIn(t, filepath.Dir(w.target), "targets.yaml", strings.Replace(unreadableTargets, "collector: a", "collector: c", 1))
	w.refused[w.target] = true
	if lines := w.tick(t); len(lines) != 2 || lines[0]["msg"] != "static target reload rejected" || lines[1]["msg"] != "configuration reload rejected" {
		t.Fatalf("the target file that uses another, unreadable: logged %v", lines)
	}
	for range 2 {
		reads := w.reads[w.config]
		if lines := w.tick(t); len(lines) != 0 || w.reads[w.config] == reads || w.failures(reloadFileConfig) != 2 || w.failures(ReloadFileStaticTargets) != 1 {
			t.Fatalf("a tick while the target file is unreadable: logged %v, the configuration read %t", lines, w.reads[w.config] > reads)
		}
	}
	w.refused[w.target] = false
	if lines := w.tick(t); len(lines) != 2 || lines[0]["msg"] != "configuration reloaded" || lines[1]["msg"] != "static targets reloaded" || w.targetCollector() != "c" {
		t.Fatalf("the target file readable: logged %v, the target uses %s", lines, w.targetCollector())
	}
}

// collectorFilesStampBeforeMode is collectorFilesStamp as it was before it
// stamped each file's mode.
func collectorFilesStampBeforeMode(configPath string, entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	files, err := resolveCollectorFiles(configPath, entries)
	if err != nil {
		return "error:" + err.Error()
	}
	var b strings.Builder
	for _, file := range files {
		b.WriteString(file)
		if st, err := os.Stat(file); err == nil {
			b.WriteByte(0)
			b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
			b.WriteByte(0)
			b.WriteString(strconv.FormatInt(st.Size(), 10))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Over generated sequences of what happens to collector files listed by
// name and by a pattern — an edit with a new time, an edit keeping time and
// size, a chmod, a delete, a recreate — the stamp of the collector files
// tells a change exactly when the stamp as it was did, or the permissions
// of a file it lists changed, which it lists while every entry resolves.
func TestTheCollectorFilesStampTellsAChmodBesidesWhatItTold(t *testing.T) {
	steps := alloctest.UnlessRaced(300, 100)
	random := rand.New(rand.NewPCG(66, 8))
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.yaml")
	entries := []string{"named.yaml", "d/*.yaml"}
	paths := []string{filepath.Join(dir, "named.yaml"), filepath.Join(dir, "d", "a.yaml"), filepath.Join(dir, "d", "b.yaml")}
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_000_000, 0)
	write := func(path, content string, when time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths[:2] {
		write(path, "collectors-0", at)
	}
	events := []string{"edit with a new time", "edit keeping time and size", "chmod", "delete", "recreate"}
	seen := map[string]int{}
	for step := range steps {
		path := paths[random.IntN(len(paths))]
		event := events[random.IntN(len(events))]
		before, statErr := os.Stat(path)
		if (statErr == nil) == (event == "recreate") {
			continue
		}
		was, is := collectorFilesStampBeforeMode(conf, entries), collectorFilesStamp(conf, entries)
		switch event {
		case "edit with a new time":
			at = at.Add(time.Second)
			write(path, fmt.Sprintf("collectors-%04d", step), at)
		case "edit keeping time and size":
			write(path, fmt.Sprintf("collectors-%04d", step), before.ModTime())
			chmodKeepingTime(t, path, before.Mode().Perm(), before.ModTime())
		case "chmod":
			chmodKeepingTime(t, path, before.Mode().Perm()^0o044, before.ModTime())
		case "delete":
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		case "recreate":
			write(path, fmt.Sprintf("collectors-%04d", step), at)
		}
		seen[event]++
		toldBefore := collectorFilesStampBeforeMode(conf, entries) != was
		// A file is listed while every entry resolves: one that does not
		// is what the stamp holds in their place.
		chmodded := false
		if _, err := resolveCollectorFiles(conf, entries); err == nil {
			if after, err := os.Stat(path); err == nil && statErr == nil && after.Mode() != before.Mode() {
				chmodded = true
			}
		}
		if tells := collectorFilesStamp(conf, entries) != is; tells != (toldBefore || chmodded) {
			t.Fatalf("step %d, %s of %s: the stamp tells a change %t, as it was %t, the mode changed %t", step, event, path, tells, toldBefore, chmodded)
		}
	}
	for _, event := range events {
		if seen[event] == 0 {
			t.Errorf("no %s among the steps", event)
		}
	}
}
