package allocclient

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// A reload that refuses a bundle, whose restore then fails to put the files
// before it back, leaves ..data naming the refused generation: its
// directory is kept, so config.yaml can still be read, and the bundle is a
// failure to write, neither refused nor seen. The next try puts the files in
// force back first, and the bundle is then refused as any other.
func TestARefusedReloadWhoseRestoreFailsKeepsTheDirectoryInUse(t *testing.T) {
	a := newFakeAllocator(t)
	proceed := make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	c := newTestClient(t, a, t.TempDir(), func(o *Options) {
		tell := o.Event
		o.Event = func(e Event) {
			tell(e)
			if e.Kind == EventError {
				<-proceed
			}
		}
	})
	c.manager.refuse = func(config string) bool { return config == "refused\n" }
	runClient(t, c)
	t.Cleanup(release)
	good := a.serveFiles(map[string]string{"config.yaml": "good\n", "keep.yaml": "k\n"})
	c.waitFor(t, EventInstalled)
	c.manager.mu.Lock()
	c.manager.failRestore = true
	c.manager.mu.Unlock()
	bad := a.serveFiles(map[string]string{"config.yaml": "refused\n"})
	if e := c.waitFor(t, EventError); e.Generation != bad {
		t.Fatalf("error %+v", e)
	}
	// The client waits in the event: the cache directory is as the failed
	// restore left it.
	if got := c.read("config.yaml"); got != "refused\n" {
		t.Errorf("with ..data left at the refused generation config.yaml reads %q", got)
	}
	if got := generations(t, c.dir); !slices.Contains(got, bad) {
		t.Errorf("the directory ..data names was removed: %v", got)
	}
	if st := c.Status(); st.SyncErrors[StageWrite] != 1 || st.Refused != 0 || st.Generation != good {
		t.Errorf("status %+v", st)
	}
	release()
	if e := c.waitFor(t, EventRefused); e.Generation != bad {
		t.Fatalf("refused %+v", e)
	}
	if got := c.read("config.yaml") + c.read("keep.yaml"); got != "good\nk\n" {
		t.Errorf("after the next try the files read %q", got)
	}
	if got := generations(t, c.dir); len(got) != 1 || got[0] != good || CachedGeneration(c.dir) != good {
		t.Errorf("generation directories %v, in force %q", got, CachedGeneration(c.dir))
	}
	if inForce, _ := c.manager.state(); inForce != "good\n" {
		t.Errorf("in force %q", inForce)
	}
}

// install puts files in force in dir as the client does, and returns their
// generation.
func install(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	c := newTestClient(t, newFakeAllocator(t), dir, nil)
	b := &allocapi.Bundle{Files: files}
	b.Seal()
	if refusal, err := c.apply(b); refusal != nil || err != nil {
		t.Fatalf("installing %v: %v %v", files, refusal, err)
	}
	return b.Generation
}

// symlink makes a link at name in dir to target.
func symlink(t *testing.T, dir, target, name string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

// writeIn writes content at name in dir.
func writeIn(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The cache directory holds a bundle only when the client installed it:
// ..data naming a generation's directory, whose files are that generation,
// and every top-level name linking through ..data.
func TestCachedGenerationIsOnlyABundleTheClientInstalled(t *testing.T) {
	files := map[string]string{"config.yaml": "c\n", "collectors/a.yaml": "a\n"}
	dir := t.TempDir()
	generation := install(t, dir, files)
	if got := CachedGeneration(dir); got != generation {
		t.Fatalf("installed bundle: %q, want %q", got, generation)
	}
	for name, spoil := range map[string]func(dir string){
		"a plain config.yaml": func(dir string) {
			_ = os.Remove(filepath.Join(dir, "config.yaml"))
			writeIn(t, dir, "config.yaml", "c\n")
		},
		"a changed file":    func(dir string) { writeIn(t, dir, genPrefix+generation+"/collectors/a.yaml", "b\n") },
		"a name not linked": func(dir string) { _ = os.Remove(filepath.Join(dir, "collectors")) },
		"config.yaml elsewhere": func(dir string) {
			_ = os.Remove(filepath.Join(dir, "config.yaml"))
			symlink(t, dir, genPrefix+generation+"/config.yaml", "config.yaml")
		},
		"..data gone": func(dir string) { _ = os.Remove(filepath.Join(dir, dataLink)) },
		"another generation": func(dir string) {
			_ = os.Remove(filepath.Join(dir, dataLink))
			symlink(t, dir, genPrefix+"other", dataLink)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			install(t, dir, files)
			spoil(dir)
			if got := CachedGeneration(dir); got != "" {
				t.Errorf("CachedGeneration %q, want none", got)
			}
		})
	}
	plain := t.TempDir()
	writeIn(t, plain, "config.yaml", "c\n")
	if got := CachedGeneration(plain); got != "" {
		t.Errorf("a cache directory with a config.yaml put there by hand: %q", got)
	}
}

// Tidy keeps the bundle the client installed and removes what a write cut
// short left: other generations' directories, a ..tmp-link, and a static
// target file the bundle does not link. Other files stay.
func TestTidyKeepsTheInstalledBundleAndRemovesWhatACrashLeft(t *testing.T) {
	dir := t.TempDir()
	generation := install(t, dir, map[string]string{"config.yaml": "c\n"})
	writeIn(t, dir, genPrefix+"cut-short/config.yaml", "half")
	symlink(t, dir, genPrefix+"cut-short", tmpLink)
	writeIn(t, dir, "static-targets.yaml", "targets\n")
	writeIn(t, dir, "notes.txt", "mine\n")
	if got := Tidy(dir, slog.New(slog.NewTextHandler(io.Discard, nil))); got != generation {
		t.Fatalf("Tidy returned %q, want %q", got, generation)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{dataLink, genPrefix + generation, "config.yaml", "notes.txt"}) {
		t.Errorf("after Tidy the cache directory holds %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.yaml")); err != nil || string(b) != "c\n" {
		t.Errorf("config.yaml reads %q, %v", b, err)
	}
}

// Without a bundle the client installed, Tidy removes the files the
// manager would read, ..data and the links through it, and every
// generation's directory, so the exporter starts empty rather than with
// files it did not put there. Other files stay.
func TestTidyWithoutAnInstalledBundleRemovesWhatTheManagerWouldRead(t *testing.T) {
	dir := t.TempDir()
	generation := install(t, dir, map[string]string{"config.yaml": "c\n", "a.yaml": "a\n"})
	writeIn(t, dir, genPrefix+generation+"/a.yaml", "changed\n")
	writeIn(t, dir, "notes.txt", "mine\n")
	plain := t.TempDir()
	writeIn(t, plain, "config.yaml", "c\n")
	writeIn(t, plain, "static-targets.yaml", "t\n")
	writeIn(t, plain, "notes.txt", "mine\n")
	for name, dir := range map[string]string{"a changed generation": dir, "files put there by hand": plain} {
		if got := Tidy(dir, slog.New(slog.NewTextHandler(io.Discard, nil))); got != "" {
			t.Errorf("%s: Tidy returned %q", name, got)
		}
		if got := entries(t, dir); !slices.Equal(got, []string{"notes.txt"}) {
			t.Errorf("%s: after Tidy the cache directory holds %v", name, got)
		}
	}
}

// entries lists the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range list {
		out = append(out, e.Name())
	}
	return out
}
