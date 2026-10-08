package config

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// filesStampBeforeIdentity is the watch's filesStamp as it was before it
// stamped the file's identity: the file the path leads to after its links,
// with its time, size and permissions, or that there is none.
func filesStampBeforeIdentity(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte(0)
		st, err := os.Stat(path)
		if err != nil {
			b.WriteString("missing\n")
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			b.WriteString(resolved)
		}
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(st.Size(), 10))
		b.WriteByte(0)
		b.WriteString(st.Mode().String())
		b.WriteByte('\n')
	}
	return b.String()
}

// Over generated sequences of what happens to a watched file, a plain one
// and one reached through a link swapped as Kubernetes swaps ..data — an
// edit with a new time, an edit keeping time and size, a rename over by a
// file of the same time, size and permissions, a rename over with a new
// time, a link swap to a file of the same time and size, a chmod, a delete,
// a recreate — the watch's stamp tells a change exactly when the stamp as it
// was did, or the file the path leads to is another file (os.SameFile),
// where the system says which file it is; elsewhere exactly when it did.
func TestTheWatchStampTellsAnotherFileBesidesWhatItTold(t *testing.T) {
	steps := alloctest.UnlessRaced(300, 100)
	random := rand.New(rand.NewPCG(43, 11))
	dir := t.TempDir()
	at := time.Unix(1_700_000_000, 0)
	versions := 0
	write := func(path, content string, when time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	plain := filepath.Join(dir, "plain.pb")
	write(plain, "plain-0", at)
	mount := filepath.Join(dir, "mount")
	if err := os.MkdirAll(filepath.Join(mount, "..v0"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(mount, "..v0", "x.pb"), "kube-0", at)
	swapData := func(version string) {
		t.Helper()
		if err := os.Symlink(version, filepath.Join(mount, "..data_tmp")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(mount, "..data_tmp"), filepath.Join(mount, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	swapData("..v0")
	kube := filepath.Join(mount, "x.pb")
	if err := os.Symlink(filepath.Join("..data", "x.pb"), kube); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	_, _, identities := fetch.FileIdentity(first)
	events := []string{"edit with a new time", "edit keeping time and size", "rename over keeping time and size", "rename over with a new time", "link swap keeping time and size", "chmod", "delete", "recreate"}
	seen := map[string]int{}
	identityAlone := 0
	for step := 0; step < steps; {
		path := []string{plain, kube}[random.IntN(2)]
		event := events[random.IntN(len(events))]
		target := plain
		if path == kube {
			version, err := os.Readlink(filepath.Join(mount, "..data"))
			if err != nil {
				t.Fatal(err)
			}
			target = filepath.Join(mount, version, "x.pb")
		}
		before, statErr := os.Stat(path)
		if (statErr == nil) == (event == "recreate") || event == "link swap keeping time and size" && path != kube {
			continue
		}
		was, is := filesStampBeforeIdentity([]string{path}), filesStamp([]string{path})
		versions++
		switch event {
		case "edit with a new time":
			at = at.Add(time.Second)
			write(target, fmt.Sprintf("edited-%04d", versions), at)
		case "edit keeping time and size":
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			for i := range raw {
				raw[i] ^= 1
			}
			f, err := os.OpenFile(target, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
		case "rename over keeping time and size", "rename over with a new time":
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			when := before.ModTime()
			if event == "rename over with a new time" {
				at = at.Add(time.Second)
				when = at
			}
			write(target+".new", string(raw), when)
			if err := os.Chmod(target+".new", before.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(target+".new", target); err != nil {
				t.Fatal(err)
			}
		case "link swap keeping time and size":
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			version := fmt.Sprintf("..v%d", versions)
			if err := os.MkdirAll(filepath.Join(mount, version), 0o750); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(mount, version, "x.pb"), string(raw), before.ModTime())
			if err := os.Chmod(filepath.Join(mount, version, "x.pb"), before.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			swapData(version)
		case "chmod":
			if err := os.Chmod(target, before.Mode().Perm()^0o044); err != nil {
				t.Fatal(err)
			}
		case "delete":
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
		case "recreate":
			at = at.Add(time.Second)
			write(target, "recreated", at)
		}
		step++
		seen[event]++
		after, afterErr := os.Stat(path)
		another := statErr == nil && afterErr == nil && !os.SameFile(before, after)
		wasChanged := filesStampBeforeIdentity([]string{path}) != was
		isChanged := filesStamp([]string{path}) != is
		want := wasChanged || another && identities
		if another && !wasChanged {
			identityAlone++
		}
		if isChanged != want {
			t.Errorf("step %d, %s of %s: the watch tells a change %v, want %v (as it was %v, another file %v)", step, event, path, isChanged, want, wasChanged, another)
		}
	}
	for _, event := range events {
		if seen[event] == 0 {
			t.Errorf("%s never happened", event)
		}
	}
	if identities && identityAlone == 0 {
		t.Error("no file became another that the stamp as it was missed")
	}
}
