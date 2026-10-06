package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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

// fileProblem is a mistake in a collector file. Its text names the file, and
// File says which it is, so a rejected reload is logged against the file the
// operator has to edit rather than the configuration that only lists it.
type fileProblem struct {
	file string
	err  error
	// named is set when err's own text already names the file, as the
	// errors of reading one do.
	named bool
}

func (p *fileProblem) Error() string {
	if p.named {
		return p.err.Error()
	}
	return fmt.Sprintf("collector file %s: %v", p.file, p.err)
}

func (p *fileProblem) Unwrap() error { return p.err }

// inCollectorFile names the collector file a collector was defined in on
// each problem validation found with it. file is empty for a collector of
// the configuration itself, whose problems are left as they are.
func inCollectorFile(file string, err error) error {
	if file == "" || err == nil {
		return err
	}
	if problems, several := err.(model.Problems); several { //nolint:errorlint // a Problems itself, as JoinProblems splices it
		out := make(model.Problems, 0, len(problems))
		for _, problem := range problems {
			out = append(out, &fileProblem{file: file, err: problem})
		}
		return out
	}
	return &fileProblem{file: file, err: err}
}

// collectorFileOf is the collector file the named collector was read from,
// and empty for one the configuration defines itself, or one that was not
// read from a file at all.
func collectorFileOf(c *model.Config, name string) string {
	if source := c.CollectorSources[name]; slices.Contains(c.LoadedCollectorFiles, source) {
		return source
	}
	return ""
}

// problemFile is the file a refused configuration's problems are in: the
// collector file, when every one of them is in the same one, and the
// configuration otherwise, whose error then names each file in its text.
func problemFile(err error, configPath string) string {
	problems := model.Problems{err}
	if several, ok := err.(model.Problems); ok { //nolint:errorlint // see inCollectorFile
		problems = several
	}
	file := ""
	for i, problem := range problems {
		var inFile *fileProblem
		if !errors.As(problem, &inFile) || i > 0 && inFile.file != file {
			return configPath
		}
		file = inFile.file
	}
	if file == "" {
		return configPath
	}
	return file
}

// collectorFile is the shape of a collector file.
type collectorFile struct {
	Collectors []model.Collector `yaml:"collectors"`
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
func loadCollectorFile(path string, opts []LoadOption) ([]model.Collector, error) {
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
	// The keys are the file's own and those a merge key (<<) brings in, as
	// the decoder reads them.
	for _, entry := range model.MappingEntries(root) {
		if key := entry.Key.Value; key != collectorFileKey && !isExtensionKey(key) {
			return nil, fmt.Errorf("collector file %s: line %d: %q is not allowed; a collector file may only contain %s, and x- keys of its own for YAML anchors", path, entry.Key.Line, key, collectorFileKey)
		}
	}
	var file collectorFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := withValueProblems(withoutExtensionKeys(dec.Decode(&file)), b, reflect.TypeOf(file)); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("collector file %s: %w", path, yamlError(err))
	}
	if err := oneDocument(dec); err != nil {
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
// A file that cannot be read does not stop the others: the mistakes of every
// file are reported together, so several broken files are fixed in one pass.
func mergeCollectorFiles(c *model.Config, configPath string, opts []LoadOption) error {
	// A collector whose name was taken is reported here and left out, so the
	// check of the rest does not report it a second time.
	var errs []error
	c.CollectorSources = map[string]string{}
	own := c.Collectors[:0:0]
	for i := range c.Collectors {
		x := &c.Collectors[i]
		if first, dup := c.CollectorSources[x.Name]; dup {
			errs = append(errs, duplicateCollectorError(x.Name, first, configPath))
			continue
		}
		c.CollectorSources[x.Name] = configPath
		own = append(own, *x)
	}
	c.Collectors = own
	files, err := resolveCollectorFiles(configPath, c.CollectorFiles)
	if err != nil {
		return model.JoinProblems(append(errs, err)...)
	}
	c.LoadedCollectorFiles = files
	for _, file := range files {
		collectors, err := loadCollectorFile(file, opts)
		if err != nil {
			errs = append(errs, &fileProblem{file: file, err: err, named: true})
			continue
		}
		for i := range collectors {
			x := &collectors[i]
			if first, dup := c.CollectorSources[x.Name]; dup {
				errs = append(errs, duplicateCollectorError(x.Name, first, file))
				continue
			}
			c.CollectorSources[x.Name] = file
			c.Collectors = append(c.Collectors, *x)
		}
	}
	return model.JoinProblems(errs...)
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
			b.WriteByte(0)
			b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
			b.WriteByte(0)
			b.WriteString(strconv.FormatInt(st.Size(), 10))
		}
		b.WriteString("\n")
	}
	return b.String()
}
