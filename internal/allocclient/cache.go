package allocclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// The cache directory holds the bundle in force the way Kubernetes holds a
// mounted ConfigMap: each generation's files in a directory of their own,
// ..gen-<generation>; a symbolic link ..data to the one in force; and for
// each top-level name of the bundle a link to it through ..data, such as
// config.yaml -> ..data/config.yaml. A new bundle is written into its own
// directory, checked there, and put in force by replacing ..data, one
// rename: whoever opens a file through the links then finds the new
// generation, and before it the old, never part of each. The configuration
// manager reads <cache-dir>/config.yaml and <cache-dir>/static-targets.yaml,
// and the swap and its reload happen under its reload lock, so no reload
// reads the configuration of one generation with the collector files of
// another. The other generations' directories are removed once the new one
// is in force, and a refused one at once. Files are written 0600 and
// directories 0700: a bundle holds configuration, which may name
// credentials.
//
// The names beginning with ".." are the cache directory's own: a bundle
// with a top-level name like that is refused.
const (
	dataLink  = "..data"
	genPrefix = "..gen-"
	tmpLink   = "..tmp-link"
)

// CachedGeneration is the generation of the bundle in the cache directory,
// empty when there is none.
func CachedGeneration(cacheDir string) string {
	target, err := os.Readlink(filepath.Join(cacheDir, dataLink))
	if err != nil || !strings.HasPrefix(target, genPrefix) {
		return ""
	}
	return strings.TrimPrefix(target, genPrefix)
}

// apply writes b into the cache directory, checks it and puts it in force.
// refusal is why b was refused, with the files in force kept; err is a
// failure to write into the cache directory.
func (c *Client) apply(b *allocapi.Bundle) (refusal, err error) {
	if err := checkBundle(b); err != nil {
		return err, nil
	}
	dir := filepath.Join(c.o.CacheDir, genPrefix+b.Generation)
	if err := writeTree(dir, b.Files); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	c.step("wrote " + filepath.Base(dir))
	if err := c.o.Manager.CheckReplacement(filepath.Join(dir, allocapi.ConfigFile), filepath.Join(dir, allocapi.StaticTargetsFile)); err != nil {
		_ = os.RemoveAll(dir)
		return err, nil
	}
	var swapErr error
	err = c.o.Manager.ReloadReplacing(ReloadTrigger, func() (func() error, error) {
		restore, err := c.swap(b.Generation, b.Files)
		swapErr = err
		return restore, err
	})
	if swapErr != nil {
		_ = os.RemoveAll(dir)
		return nil, swapErr
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return err, nil
	}
	c.removeOtherGenerations(b.Generation)
	return nil, nil
}

// checkBundle refuses a bundle whose files could leave the directory they
// are written into, that has no configuration, whose generation is not its
// files' digest, or that has a top-level name the cache directory keeps for
// itself.
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

// writeTree writes files into dir, which it makes anew.
func writeTree(dir string, files map[string]string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := writeFile(path, content); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes content into a new file at path, and has it on disk
// before it returns: a restart after the swap finds the whole file.
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

// relink makes the links through ..data those of names: one is made for
// each name that has none, then ..data is pointed at the generation's
// directory, or removed when generation is empty, and then the links of
// other names are removed. Until ..data is replaced every link finds the
// old generation, or nothing, and from then on the new one.
func (c *Client) relink(generation string, names []string) error {
	have := c.linkedNames()
	for _, name := range names {
		if !slices.Contains(have, name) {
			if err := c.link(name, dataLink+"/"+name); err != nil {
				return err
			}
		}
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
	return nil
}

// swap puts the generation written into its directory in force in the cache
// directory, and returns what puts the one before back.
func (c *Client) swap(generation string, files map[string]string) (restore func() error, err error) {
	before, beforeNames := CachedGeneration(c.o.CacheDir), c.linkedNames()
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
