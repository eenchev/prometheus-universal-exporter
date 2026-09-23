package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// A configuration can keep its collectors in files of their own, listed under
// collector_files: one team's collectors per file, or one file per ConfigMap
// key. Each entry is a path or a glob pattern, resolved against the directory
// of the configuration file. A collector file holds a collectors list and
// nothing else — the web, OTLP and other exporter-wide settings belong to the
// configuration alone, so they cannot be set, or overridden, from a file of
// collectors.
//
// The collectors of every file are appended to the configuration's own, in the
// order collector_files lists them and, within a pattern, in file name order,
// and then validated together exactly as if they had been written in one file.
// A collector name must be unique across all of them: the same name twice is a
// startup error, and a rejected reload, naming both places it was defined.

// collectorFile is the shape of a collector file.
type collectorFile struct {
	Collectors []Collector `yaml:"collectors"`
}

// collectorFileKey is the one key a collector file may have.
const collectorFileKey = "collectors"

// isGlobPattern reports whether a collector_files entry is a pattern rather
// than the name of one file.
func isGlobPattern(entry string) bool {
	return strings.ContainsAny(entry, "*?[")
}

// resolveCollectorFiles expands collector_files into the files to read, in
// order, each once. A plain path must exist; a pattern may match nothing, so a
// directory of collector files may be empty. The configuration file itself is
// never read as a collector file, so a pattern such as *.yaml next to it does
// not pull it in.
func resolveCollectorFiles(configPath string, entries []string) ([]string, error) {
	base := filepath.Dir(configPath)
	self, _ := filepath.Abs(configPath)
	seen := map[string]bool{}
	var files []string
	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			return nil, errors.New("collector_files has an empty entry")
		}
		path := entry
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		var matches []string
		if isGlobPattern(entry) {
			found, err := filepath.Glob(path)
			if err != nil {
				return nil, fmt.Errorf("collector_files pattern %q: %w", entry, err)
			}
			sort.Strings(found)
			matches = found
		} else {
			st, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("collector file %s: %w", path, err)
			}
			if st.IsDir() {
				return nil, fmt.Errorf("collector file %s is a directory; use a pattern such as %s", path, filepath.Join(entry, "*.yaml"))
			}
			matches = []string{path}
		}
		for _, match := range matches {
			if st, err := os.Stat(match); err == nil && st.IsDir() {
				continue
			}
			abs, err := filepath.Abs(match)
			if err != nil {
				abs = match
			}
			if abs == self || seen[abs] {
				continue
			}
			seen[abs] = true
			files = append(files, match)
		}
	}
	return files, nil
}

// loadCollectorFile reads one collector file, which must have a non-empty
// collectors list and no other key.
func loadCollectorFile(path string, opts []LoadOption) ([]Collector, error) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, fmt.Errorf("collector file %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("collector file %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("collector file %s is empty; it must define collectors", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("collector file %s must be a mapping with a collectors list", path)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if key := root.Content[i].Value; key != collectorFileKey {
			return nil, fmt.Errorf("collector file %s: line %d: %q is not allowed; a collector file may only contain %s", path, root.Content[i].Line, key, collectorFileKey)
		}
	}
	var file collectorFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("collector file %s: %w", path, err)
	}
	if len(file.Collectors) == 0 {
		return nil, fmt.Errorf("collector file %s defines no collectors", path)
	}
	return file.Collectors, nil
}

// mergeCollectorFiles appends the collectors of every collector file to the
// configuration's own and records where each collector came from. A name
// defined twice, in one file or across files, is an error naming both places.
func mergeCollectorFiles(c *Config, configPath string, opts []LoadOption) error {
	c.CollectorSources = map[string]string{}
	for _, x := range c.Collectors {
		if first, dup := c.CollectorSources[x.Name]; dup {
			return duplicateCollectorError(x.Name, first, configPath)
		}
		c.CollectorSources[x.Name] = configPath
	}
	files, err := resolveCollectorFiles(configPath, c.CollectorFiles)
	if err != nil {
		return err
	}
	c.LoadedCollectorFiles = files
	for _, file := range files {
		collectors, err := loadCollectorFile(file, opts)
		if err != nil {
			return err
		}
		for _, x := range collectors {
			if first, dup := c.CollectorSources[x.Name]; dup {
				return duplicateCollectorError(x.Name, first, file)
			}
			c.CollectorSources[x.Name] = file
		}
		c.Collectors = append(c.Collectors, collectors...)
	}
	return nil
}

func duplicateCollectorError(name, first, second string) error {
	if first == second {
		return fmt.Errorf("duplicate collector %q: defined twice in %s", name, first)
	}
	return fmt.Errorf("duplicate collector %q: defined in %s and in %s", name, first, second)
}

// collectorFilesStamp describes the collector files a configuration reads —
// which files a pattern matches now, and each one's modification time and
// size — so the watch can tell when one was edited, added or removed. An entry
// that cannot be resolved is part of the stamp too, so its file appearing is
// also a change.
func collectorFilesStamp(configPath string, entries []string) string {
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
			b.WriteString("\x00" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + "\x00" + strconv.FormatInt(st.Size(), 10))
		}
		b.WriteString("\n")
	}
	return b.String()
}
