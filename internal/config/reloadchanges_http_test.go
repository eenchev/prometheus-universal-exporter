//go:build !select_request_types || request_type_http

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a reload changed (reloadchanges.go): on the info line each file put
// in force logs, and in the manager's last summary.

// changesManager is a manager over a configuration of the named collectors
// and a static target file of targets, read as the exporter reads them at
// startup, logging into the buffer it returns.
func changesManager(t *testing.T, collectors []string, targets string) (m *Manager, config, targetFile string, logs *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	config = filepath.Join(dir, "config.yaml")
	targetFile = filepath.Join(dir, "targets.yaml")
	rewriteFile(t, config, testutil.CollectorsDocument(collectors...))
	rewriteFile(t, targetFile, targets)
	cfg, err := Load(config)
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(targetFile)
	if err == nil {
		err = ValidateStaticTargets(file)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	logs = &bytes.Buffer{}
	m = NewManager(cfg, config, slog.New(slog.NewJSONHandler(logs, nil)))
	m.SetTargets(targetFile, file)
	return m, config, targetFile, logs
}

func rewriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// targetsDocument is a static target file of name=target pairs, each
// scraped through collector a.
func targetsDocument(pairs ...string) string {
	var b strings.Builder
	b.WriteString("interval: 1m\ntargets:\n")
	for _, pair := range pairs {
		name, target, _ := strings.Cut(pair, "=")
		fmt.Fprintf(&b, "  - name: %s\n    collector: a\n    target: %s\n", name, target)
	}
	return b.String()
}

// reloadChangesLogged reads the changes of every line that says a file was
// put in force in logs, at info level, as "message: changes", and empties
// it.
func reloadChangesLogged(t *testing.T, logs *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if msg := entry["msg"]; msg == "configuration reloaded" || msg == "static targets reloaded" {
			if entry["level"] != "INFO" {
				t.Errorf("logged at %v: %s", entry["level"], line)
			}
			out = append(out, fmt.Sprintf("%v: %v", msg, entry["changes"]))
		}
	}
	logs.Reset()
	return out
}

// A reload that puts something in force names on its line for the
// configuration the collectors added, removed and changed, and on its line
// for the static target file the static targets, each sorted, or says "no
// changes"; the manager keeps both with the time as the last reload. A
// refused reload changes neither.
func TestAReloadSaysWhatItChanged(t *testing.T) {
	m, config, targets, logs := changesManager(t, []string{"a", "b", "c"}, targetsDocument("t1=http://one.example", "t2=http://two.example"))
	if _, ok := m.LastReload(); ok {
		t.Fatal("a last reload before any")
	}
	logs.Reset()
	if err := m.Reload("test"); err != nil {
		t.Fatal(err)
	}
	if got, want := reloadChangesLogged(t, logs), []string{"configuration reloaded: no changes", "static targets reloaded: no changes"}; !slices.Equal(got, want) {
		t.Fatalf("an unchanged reload logged %q, want %q", got, want)
	}
	if last, ok := m.LastReload(); !ok || last.Changes != "no changes" || last.At.IsZero() {
		t.Fatalf("last reload: %+v %v", last, ok)
	}

	changedC := strings.Replace(testutil.CollectorYAML("c"), "/status", "/other", 1)
	rewriteFile(t, config, "collectors:\n"+testutil.CollectorYAML("a")+changedC+testutil.CollectorYAML("e")+testutil.CollectorYAML("d"))
	rewriteFile(t, targets, targetsDocument("t3=http://three.example", "t1=http://moved.example"))
	if err := m.Reload("test"); err != nil {
		t.Fatal(err)
	}
	collectors := "collectors added: d, e; collectors removed: b; collectors changed: c"
	targetChanges := "static targets added: t3; static targets removed: t2; static targets changed: t1"
	if got, want := reloadChangesLogged(t, logs), []string{"configuration reloaded: " + collectors, "static targets reloaded: " + targetChanges}; !slices.Equal(got, want) {
		t.Fatalf("logged %q, want %q", got, want)
	}
	want := collectors + "; " + targetChanges
	if last, _ := m.LastReload(); last.Changes != want {
		t.Fatalf("last reload %q, want %q", last.Changes, want)
	}

	// A refused reload puts nothing in force and keeps the last summary.
	rewriteFile(t, config, testutil.CollectorsDocument("dup", "dup"))
	rewriteFile(t, targets, "interval: 1m\ntargets: [")
	if err := m.Reload("test"); err == nil {
		t.Fatal("a broken configuration reloaded")
	}
	if got := reloadChangesLogged(t, logs); len(got) != 0 {
		t.Fatalf("a refused reload logged %q", got)
	}
	if last, _ := m.LastReload(); last.Changes != want {
		t.Fatalf("a refused reload made the last reload %q", last.Changes)
	}
}

// Each list names reloadChangesNamed names and counts the rest, so a reload
// that replaces many collectors writes one line of bounded length.
func TestAReloadsChangesAreBounded(t *testing.T) {
	m, config, _, logs := changesManager(t, []string{"a"}, targetsDocument("t1=http://one.example"))
	names := []string{"a"}
	for i := 1; i <= 13; i++ {
		names = append(names, fmt.Sprintf("x%02d", i))
	}
	rewriteFile(t, config, testutil.CollectorsDocument(names...))
	logs.Reset()
	if err := m.Reload("test"); err != nil {
		t.Fatal(err)
	}
	want := "collectors added: x01, x02, x03, x04, x05, x06, x07, x08, x09, x10 and 3 more"
	if got := reloadChangesLogged(t, logs); !slices.Equal(got, []string{"configuration reloaded: " + want, "static targets reloaded: no changes"}) {
		t.Fatalf("logged %q, want the collectors as %q", got, want)
	}
	if last, _ := m.LastReload(); last.Changes != want {
		t.Fatalf("last reload %q, want %q", last.Changes, want)
	}
}
