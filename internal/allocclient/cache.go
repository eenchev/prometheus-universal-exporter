package allocclient

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// The cache directory holds the bundle in force the way Kubernetes holds a
// mounted ConfigMap:
//   - each generation's files are in a directory of their own,
//     ..gen-<generation>;
//   - a symbolic link ..data points to the one in force;
//   - each top-level name of the bundle has a link through ..data, such as
//     config.yaml -> ..data/config.yaml.
//
// A new bundle is written into its own directory, checked there, and put in
// force by replacing ..data, which is a single rename. Whoever opens a file
// through the links finds the new generation from then on, and the old one
// before, never a mix of both. The configuration manager reads
// <cache-dir>/config.yaml and <cache-dir>/static-targets.yaml, and the swap
// and its reload happen under its reload lock, so no reload reads the
// configuration of one generation with the collector files of another.
//
// The other generations' directories are removed once the new one is in
// force, and a refused one at once. The one that ..data names is never
// removed, nor written again:
//   - a swap or a restore cut short can leave ..data at the new generation,
//     and that is put right before the next bundle is written (repair);
//   - when ..data names a generation that is not in force (a cached bundle
//     that did not load at startup) and that generation is received again,
//     it is checked and reloaded where it is.
//
// Files are written 0600 and directories 0700, because a bundle holds
// configuration, which may name credentials.
//
// What is put in force survives a crash. Each file is synced before its
// generation's directory, each directory after the files and directories in
// it, and the cache directory after the generation's directory and the new
// links are made, before ..data is replaced and again after. A restart
// therefore finds either the previous generation or the whole new one.
//
// At startup the cache directory is tidied (Tidy). A bundle is resumed only
// when the client installed it, and anything that a write cut short by a
// crash left, or that was put there by hand, is removed.
//
// The names beginning with ".." belong to the cache directory: a bundle with
// such a top-level name is refused.
const (
	dataLink  = "..data"
	genPrefix = "..gen-"
	tmpLink   = "..tmp-link"
)

// linkedGeneration is the generation ..data names, empty when there is no
// ..data or it names no generation's directory.
func linkedGeneration(cacheDir string) string {
	target, err := os.Readlink(filepath.Join(cacheDir, dataLink))
	if err != nil || !strings.HasPrefix(target, genPrefix) {
		return ""
	}
	return strings.TrimPrefix(target, genPrefix)
}

// CachedGeneration is the generation of the bundle that the client installed
// in the cache directory, or "" when there is none. ..data must name the
// directory ..gen-<generation>, whose files must be that generation and a
// bundle that the client would accept, and each of its top-level names must
// link through ..data. A config.yaml put into the cache directory by hand, or
// a generation whose write was cut short, is not a bundle.
func CachedGeneration(cacheDir string) string {
	generation := linkedGeneration(cacheDir)
	if generation == "" {
		return ""
	}
	files, err := readTree(filepath.Join(cacheDir, genPrefix+generation))
	if err != nil || checkBundle(&allocapi.Bundle{Generation: generation, Files: files}) != nil {
		return ""
	}
	for _, name := range topLevel(files) {
		if target, err := os.Readlink(filepath.Join(cacheDir, name)); err != nil || target != dataLink+"/"+name {
			return ""
		}
	}
	return generation
}

// readTree reads the files under dir, by their slash-separated paths
// relative to it, as writeTree wrote them: anything but a directory or a
// regular file, or more than a bundle may hold, is an error.
func readTree(dir string) (map[string]string, error) {
	// Read through an os.Root, so nothing under dir can lead the walk out
	// of it.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	tree := root.FS()
	files := map[string]string{}
	total := 0
	err = fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s in %s is not a regular file", path, dir)
		}
		b, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		if total += len(b); total > maxBundleBytes {
			return fmt.Errorf("%s holds more than a bundle may", dir)
		}
		files[path] = string(b)
		return nil
	})
	return files, err
}

// Tidy prepares the cache directory at startup, before anything reads it,
// and returns the generation of the bundle that the client installed there
// (CachedGeneration), or "" for none. It removes anything else that the
// client may have left, as after a write cut short by a crash or a kill: the
// other generations' directories and a ..tmp-link. Without an installed
// bundle, it also removes ..data and the links through it, and config.yaml
// and static-targets.yaml unless they link through ..data, so the
// configuration manager finds no files that the client did not put there.
// Other files are left alone. Whatever cannot be removed is logged, and
// removed again after the next bundle is put in force.
func Tidy(cacheDir string, logger *slog.Logger) string {
	generation := CachedGeneration(cacheDir)
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return generation
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(cacheDir, name)
		target, err := os.Readlink(path)
		throughData := err == nil && target == dataLink+"/"+name
		var remove bool
		switch {
		case name == tmpLink:
			remove = true
		case strings.HasPrefix(name, genPrefix):
			remove = name != genPrefix+generation
		case name == dataLink:
			remove = generation == ""
		case name == allocapi.ConfigFile || name == allocapi.StaticTargetsFile:
			// The manager reads these two: one that does not link through
			// the installed bundle's ..data is not the bundle's.
			remove = generation == "" || !throughData
		default:
			remove = generation == "" && throughData
		}
		if !remove {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			logger.Warn("removing what is not the bundle in force from the allocator's cache directory failed", "path", path, "error", err.Error())
			continue
		}
		logger.Info("removed what is not the bundle in force from the allocator's cache directory", "path", path)
	}
	return generation
}

// apply writes b into the cache directory, checks it and puts it in force.
//   - refusal is why b was refused; the files in force are kept.
//   - err is a failure to write into the cache directory: b is neither in
//     force nor refused, and is fetched and tried again. A reload that
//     refused b without the earlier files being put back is such a failure:
//     ..data may still name b's directory, which is kept, and the next try
//     puts them back first.
//
// A b already in the directory that ..data names, with none in force, is
// checked and reloaded there without being written (inUseUnchanged).
func (c *Client) apply(b *allocapi.Bundle) (refusal, err error) {
	if err := checkBundle(b); err != nil {
		return err, nil
	}
	dir := filepath.Join(c.o.CacheDir, genPrefix+b.Generation)
	if !c.inUseUnchanged(b) {
		if err := c.repair(); err != nil {
			return nil, fmt.Errorf("putting the bundle in force back in the cache directory: %w", err)
		}
		if err := writeTree(dir, b.Files); err != nil {
			c.discard(b.Generation)
			return nil, err
		}
		c.step("wrote " + filepath.Base(dir))
	}
	if err := c.o.Manager.CheckReplacement(filepath.Join(dir, allocapi.ConfigFile), filepath.Join(dir, allocapi.StaticTargetsFile)); err != nil {
		c.discard(b.Generation)
		return err, nil
	}
	var swapErr, restoreErr error
	err = c.o.Manager.ReloadReplacing(ReloadTrigger, b.Generation, func() (func() error, error) {
		restore, err := c.swap(b.Generation, b.Files)
		if swapErr = err; err != nil {
			return nil, err
		}
		return func() error {
			restoreErr = restore()
			return restoreErr
		}, nil
	})
	switch {
	case swapErr != nil:
		c.discard(b.Generation)
		return nil, swapErr
	case restoreErr != nil:
		c.discard(b.Generation)
		return nil, fmt.Errorf("the reload refused generation %s, and putting the files before it back failed: %w", b.Generation, err)
	case err != nil:
		c.discard(b.Generation)
		return err, nil
	}
	// What repair puts back from now on.
	c.mu.Lock()
	if c.inForce != b.Generation {
		c.inForce, c.inForceAt = b.Generation, c.now()
	}
	c.mu.Unlock()
	c.removeOtherGenerations(b.Generation)
	return nil, nil
}

// inUseUnchanged reports whether b is the generation that ..data names, with
// its directory holding b's files, while no generation is in force. That is
// the case for a cached bundle that did not load at startup, which the
// allocator sends again when asked without a generation. Its directory is
// then checked and reloaded where it is, not written again: config.yaml
// reads it all the while, and a refusal leaves it as it was. With a
// generation in force, ..data naming another one is what a swap or a
// restore cut short left, and repair puts the one in force back first.
func (c *Client) inUseUnchanged(b *allocapi.Bundle) bool {
	c.mu.Lock()
	inForce := c.inForce
	c.mu.Unlock()
	if inForce != "" || linkedGeneration(c.o.CacheDir) != b.Generation {
		return false
	}
	files, err := readTree(filepath.Join(c.o.CacheDir, genPrefix+b.Generation))
	return err == nil && maps.Equal(files, b.Files)
}

// discard removes the directory of generation, unless ..data names it.
func (c *Client) discard(generation string) {
	if linkedGeneration(c.o.CacheDir) == generation {
		return
	}
	_ = os.RemoveAll(filepath.Join(c.o.CacheDir, genPrefix+generation))
}

// repair puts the generation in force back in the cache directory when a
// swap or a restore cut short left ..data naming another, so that a bundle is
// always written beside the files in force and its restore puts those back.
// With a generation in force, the links are put back and the files reloaded
// under the reload lock, as for a swap. With none in force, there is nothing
// that a reload could put in force, so ..data and its links are removed.
func (c *Client) repair() error {
	c.mu.Lock()
	want := c.inForce
	c.mu.Unlock()
	if linkedGeneration(c.o.CacheDir) == want {
		return nil
	}
	if want == "" {
		return c.relink("", nil)
	}
	entries, err := os.ReadDir(filepath.Join(c.o.CacheDir, genPrefix+want))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var relinkErr error
	_ = c.o.Manager.ReloadReplacing(ReloadTrigger, want, func() (func() error, error) {
		relinkErr = c.relink(want, names)
		return nil, relinkErr
	})
	return relinkErr
}

// checkBundle refuses a bundle that has no configuration, whose files could
// leave the directory they are written into, whose generation is not its
// files' digest, or that has a top-level name that the cache directory keeps
// for itself.
func checkBundle(b *allocapi.Bundle) error {
	if err := allocapi.CheckFiles(b.Files); err != nil {
		return err
	}
	if got := allocapi.Generation(b.Files); got != b.Generation {
		return fmt.Errorf("the bundle says it is generation %q, but its files are generation %q", b.Generation, got)
	}
	for name := range b.Files {
		if top, _, _ := strings.Cut(name, "/"); strings.HasPrefix(top, "..") {
			return fmt.Errorf("the bundle names a file %q, but names beginning with \"..\" are the cache directory's own", name)
		}
	}
	return nil
}

// writeTree writes files into dir, which it creates, and syncs each
// directory that it made, the deepest first and dir last, so the names in
// them are on disk before the swap makes dir reachable. The cache
// directory's entry for dir is synced by the swap (relink), before ..data
// names dir.
func writeTree(dir string, files map[string]string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dirs := map[string]bool{dir: true}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		for d := filepath.Dir(path); !dirs[d]; d = filepath.Dir(d) {
			dirs[d] = true
		}
		if err := writeFile(path, content); err != nil {
			return err
		}
	}
	// A directory's path is longer than its parent's: the longest first
	// syncs each before the directory holding it.
	order := slices.SortedFunc(maps.Keys(dirs), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	for _, d := range order {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

// syncDir has the entries of the directory at path on disk; a test
// replaces it to see which directories are synced, and when.
var syncDir = func(path string) error {
	f, err := os.Open(path) // #nosec G304 -- a directory under the cache directory
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// writeFile writes content into a new file at path, and has it on disk
// before it returns, so syncing its directory then makes the whole file
// reachable after a crash.
func writeFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path under the cache directory, checked by checkBundle
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// step tells Options.Step of a change to the cache directory.
func (c *Client) step(change string) {
	if c.o.Step != nil {
		c.o.Step(change)
	}
}

// topLevel lists the top-level names of files, in order.
func topLevel(files map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for name := range files {
		top, _, _ := strings.Cut(name, "/")
		if !seen[top] {
			seen[top] = true
			out = append(out, top)
		}
	}
	sort.Strings(out)
	return out
}

// linkedNames lists the cache directory's links through ..data.
func (c *Client) linkedNames() []string {
	entries, err := os.ReadDir(c.o.CacheDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "..") || e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if target, err := os.Readlink(filepath.Join(c.o.CacheDir, e.Name())); err == nil && target == dataLink+"/"+e.Name() {
			out = append(out, e.Name())
		}
	}
	return out
}

// link makes the link at name, in the cache directory, point at target,
// replacing whatever was there in one rename.
func (c *Client) link(name, target string) error {
	tmp := filepath.Join(c.o.CacheDir, tmpLink)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(c.o.CacheDir, name)); err != nil {
		return err
	}
	c.step("linked " + name)
	return nil
}

// relink makes the links through ..data match names. It first makes a link
// for each name that has none, then points ..data at the generation's
// directory (or removes it when generation is empty), and then removes the
// links of other names. Until ..data is replaced, every link finds the old
// generation, or nothing, and from then on the new one. The cache directory
// is synced before ..data is replaced, so the generation's directory and the
// new links are on disk before ..data can name them, and again at the end,
// so that the replacement is too.
func (c *Client) relink(generation string, names []string) error {
	have := c.linkedNames()
	for _, name := range names {
		if !slices.Contains(have, name) {
			if err := c.link(name, dataLink+"/"+name); err != nil {
				return err
			}
		}
	}
	if err := syncDir(c.o.CacheDir); err != nil {
		return err
	}
	if generation == "" {
		if err := os.Remove(filepath.Join(c.o.CacheDir, dataLink)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		c.step("removed " + dataLink)
	} else if err := c.link(dataLink, genPrefix+generation); err != nil {
		return err
	}
	for _, name := range have {
		if !slices.Contains(names, name) {
			if err := os.Remove(filepath.Join(c.o.CacheDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			c.step("unlinked " + name)
		}
	}
	return syncDir(c.o.CacheDir)
}

// swap puts the generation written into its directory in force in the cache
// directory, and returns what puts the one before back.
func (c *Client) swap(generation string, files map[string]string) (restore func() error, err error) {
	before, beforeNames := linkedGeneration(c.o.CacheDir), c.linkedNames()
	restore = func() error { return c.relink(before, beforeNames) }
	if err := c.relink(generation, topLevel(files)); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}

// removeOtherGenerations removes the directories of every generation but
// the one in force, and a link left from a write that was cut short.
func (c *Client) removeOtherGenerations(generation string) {
	entries, err := os.ReadDir(c.o.CacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if (strings.HasPrefix(name, genPrefix) && name != genPrefix+generation) || name == tmpLink {
			if err := os.RemoveAll(filepath.Join(c.o.CacheDir, name)); err != nil {
				c.o.Logger.Warn("removing an earlier bundle from the cache directory failed", "path", filepath.Join(c.o.CacheDir, name), "error", err.Error())
				continue
			}
			c.step("removed " + name)
		}
	}
}
