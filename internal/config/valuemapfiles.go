package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"

	"gopkg.in/yaml.v3"
)

// A rule's value_map, and a label's, may be long, and the same for many
// rules: the states of a device by their numbers, the names of customers by
// their IDs. value_map_file names a file that holds such a map, a YAML or
// JSON mapping, read when the configuration loads and joined with the
// value_map written beside it, whose entries win. From there on the rule is
// as if the whole map were written in it: the checks of a value_map are
// those of the map read, the series a rule gives are known by it, and a
// file that changes is another definition of its collector, since the map
// is part of it. The watch looks at the files (namedFiles), so an edit
// reloads the configuration.
//
// A configuration loaded without the files it names, as the target
// allocator loads one, keeps the key and reads nothing: the replicas, which
// mount the file, read it when they load the bundle.

const (
	// maxValueMapFileBytes and maxValueMapFileEntries bound a value map
	// file: a table for people to read, not a database.
	maxValueMapFileBytes   = 1 << 20
	maxValueMapFileEntries = 10000
)

// valueMapFiles are the files the value_map_file keys of a collector's
// rules and of their labels name, each once, in order.
func valueMapFiles(x *model.Collector) []string {
	var files []string
	add := func(path string) {
		if path != "" && !slices.Contains(files, path) {
			files = append(files, path)
		}
	}
	for i := range x.Metrics {
		add(x.Metrics[i].ValueMapFile)
		for j := range x.Metrics[i].Labels {
			add(x.Metrics[i].Labels[j].ValueMapFile)
		}
	}
	return files
}

// readValueMapFiles reads the value_map_file of each rule of x and of each
// of its labels into the value_map beside it, an entry written there
// keeping its value. A file read once is read once for all the rules that
// name it.
func readValueMapFiles(x *model.Collector) error {
	read := map[string]map[string]string{}
	file := func(where, path string) (map[string]string, error) {
		if err := checkValueMapFileKey(path); err != nil {
			return nil, fmt.Errorf("%s %w", where, err)
		}
		if x.DescriptorFilesUnread {
			return nil, nil
		}
		if entries, done := read[path]; done {
			return entries, nil
		}
		entries, err := readValueMapFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s value_map_file %s: %w", where, path, err)
		}
		read[path] = entries
		return entries, nil
	}
	for i := range x.Metrics {
		r := &x.Metrics[i]
		where := fmt.Sprintf("collector %q metrics rule %d", x.Name, i+1)
		if r.ValueMapFile != "" {
			entries, err := file(where, r.ValueMapFile)
			if err != nil {
				return err
			}
			for key, text := range entries {
				if _, written := r.ValueMap[key]; written {
					continue
				}
				n, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return fmt.Errorf("%s value_map_file %s maps %q to a value that is no number; a rule's value_map maps text to the series' value", where, r.ValueMapFile, key)
				}
				if r.ValueMap == nil {
					r.ValueMap = map[string]float64{}
				}
				r.ValueMap[key] = n
			}
		}
		for j := range r.Labels {
			label := &r.Labels[j]
			if label.ValueMapFile == "" {
				continue
			}
			entries, err := file(fmt.Sprintf("%s label %q", where, label.Name), label.ValueMapFile)
			if err != nil {
				return err
			}
			for key, text := range entries {
				if _, written := label.ValueMap[key]; written {
					continue
				}
				if label.ValueMap == nil {
					label.ValueMap = map[string]string{}
				}
				label.ValueMap[key] = text
			}
		}
	}
	return nil
}

// checkValueMapFileKey refuses a value_map_file that is not the absolute
// path of a file, written alone: the file is read when the configuration
// loads, with no probe to fill a placeholder and no directory a relative
// path would start from on every replica alike.
func checkValueMapFileKey(path string) error {
	switch {
	case strings.TrimSpace(path) != path:
		return fmt.Errorf("value_map_file %q has blanks around it; write the path alone", path)
	case strings.Contains(path, "{{"):
		return fmt.Errorf("value_map_file %q has a placeholder; the file is read when the configuration loads, with no probe to fill one", path)
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("value_map_file %q holds a NUL character, which no path does", path)
	case strings.Contains(path, "://"):
		return fmt.Errorf("value_map_file %q is not a path; the map is read from a file the exporter can open", path)
	case !filepath.IsAbs(path):
		return fmt.Errorf("value_map_file %q is a relative path; write the absolute path of the file, such as /etc/exporter/maps/states.yaml", path)
	}
	return nil
}

// readValueMapFile reads the map of the file at path: a YAML or JSON
// mapping of text, numbers and booleans to the same, each as it is written.
// What is wrong with a file is said without what it holds, but for a key:
// a path may name any file the exporter can read.
func readValueMapFile(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- the configuration names the file, as it names its other files
	if err != nil {
		return nil, errors.Unwrap(err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxValueMapFileBytes+1))
	if err != nil {
		return nil, errors.New("cannot be read")
	}
	if len(raw) > maxValueMapFileBytes {
		return nil, fmt.Errorf("is larger than %d bytes; a value map is a table to read, and a larger one belongs in the source", maxValueMapFileBytes)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("is not YAML or JSON; write a mapping, such as {\"1\": running, \"2\": stopped}")
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("does not hold a mapping; write one, a key and its value a line, such as \"1\": running")
	}
	root := doc.Content[0]
	if len(root.Content)/2 > maxValueMapFileEntries {
		return nil, fmt.Errorf("has more than %d entries", maxValueMapFileEntries)
	}
	entries := make(map[string]string, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: an entry is not a key and its value, each text, a number or a boolean", key.Line)
		}
		if _, twice := entries[key.Value]; twice {
			return nil, fmt.Errorf("line %d: the key %q is written twice", key.Line, key.Value)
		}
		if value.Tag == "!!null" {
			entries[key.Value] = ""
			continue
		}
		entries[key.Value] = value.Value
	}
	return entries, nil
}
