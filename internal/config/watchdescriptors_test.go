package config

import (
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The watch now reloads for the descriptor files of the configuration in
// force (Manager.descriptors). These tests hold what that left as it was: a
// configuration that names no descriptor file is watched for exactly what it
// was, in every build, the configuration the exporter starts with is read
// as Load reads it, and a manager without the watch stamps no descriptor
// file.

// oldConfigChanged is Manager.configChanged as it was before the descriptor
// files of the configuration in force were watched.
func oldConfigChanged(m *Manager) bool {
	st, err := os.Stat(m.path)
	if err != nil {
		return false
	}
	return !st.ModTime().Equal(m.lastMod) || st.Size() != m.lastSize || collectorFilesStamp(m.path, m.watchedFiles) != m.collectorFiles ||
		len(m.retryFiles) > 0 && filesStamp(m.retryFiles) != m.retryStamp
}

// The descriptor files of a configuration are the files its collectors
// name, in their order and each once, and never a certificate of the OTLP
// export or a credential file of the exporter's own authentication; a
// configuration without collectors that name one, and no configuration,
// have none.
func TestTheDescriptorFilesAreTheOnesTheCollectorsName(t *testing.T) {
	dir := t.TempDir()
	r := rand.New(rand.NewPCG(3, 4))
	for round := range 400 {
		c := generatedConfig(r, dir)
		var want []string
		for i := range c.Collectors {
			request := &c.Collectors[i].Request
			for _, path := range append([]string{request.ProtosetFile}, request.ProtoFiles...) {
				if path != "" && !slices.Contains(want, path) {
					want = append(want, path)
				}
			}
		}
		got := namedDescriptorFiles(c)
		if !slices.Equal(got, want) {
			t.Fatalf("round %d: the descriptor files are %v, want %v", round, got, want)
		}
		// They are among the files a refused reload is tried again for,
		// which are those the configuration named before.
		named := oldNamedFiles(c)
		for _, path := range got {
			if !slices.Contains(named, path) {
				t.Fatalf("round %d: %s is a descriptor file and not a file the configuration names (%v)", round, path, named)
			}
		}
		withoutCollectors := *c
		withoutCollectors.Collectors = nil
		if files := namedDescriptorFiles(&withoutCollectors); len(files) != 0 {
			t.Fatalf("round %d: a configuration without collectors has the descriptor files %v", round, files)
		}
	}
	if files := namedDescriptorFiles(nil); len(files) != 0 {
		t.Fatalf("no configuration has the descriptor files %v", files)
	}
}

// Over generated configurations, in force and with the files they name
// rewritten, removed and brought back at random, with a reload refused or
// not, the watch finds the configuration changed exactly when it did before
// the descriptor files in force were watched, which it asks for apart: they
// are found changed exactly when one is no longer as it was stamped. A
// configuration whose collectors name no descriptor file has no file
// watched in force, none found changed, and never has the static target
// file read with it. Without the watch no configuration has a file watched
// in force.
func TestAConfigurationWithoutDescriptorFilesIsWatchedAsItWas(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		testutil.WriteIn(t, dir, fmt.Sprint("present-", i), "x")
	}
	path := testutil.WriteIn(t, dir, "config.yaml", "collectors: []\n")
	targetsPath := testutil.WriteIn(t, dir, "targets.yaml", "targets: []\n")
	times := time.Now().Add(-time.Hour).Truncate(time.Second)
	// touch gives a file a modification time no file had before, and
	// writes it first when it is not there.
	touch := func(file string) {
		t.Helper()
		if _, err := os.Stat(file); err != nil {
			if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		times = times.Add(time.Second)
		if err := os.Chtimes(file, times, times); err != nil {
			t.Fatal(err)
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	without, with, changed := 0, 0, 0
	logger := testutil.QuietLogger(t)
	for round := range 300 {
		for i := range 3 {
			touch(fmt.Sprint(dir, "/present-", i))
		}
		c := generatedConfig(r, dir)
		m := NewManager(c, path, logger)
		m.SetTargets(targetsPath, &model.StaticTargetFile{Targets: []model.StaticTarget{{Name: "t", Collector: "c0", Request: model.TargetRequestConfig{Message: "{}"}}}})
		if len(m.descriptors.files) != 0 || m.descriptors.changed() {
			t.Fatalf("round %d: without the watch the files watched in force are %v", round, m.descriptors.files)
		}
		m.SetWatchInterval(time.Minute)
		if !slices.Equal(m.descriptors.files, namedDescriptorFiles(c)) {
			t.Fatalf("round %d: the files watched in force are %v, want the descriptor files %v", round, m.descriptors.files, namedDescriptorFiles(c))
		}
		if r.IntN(2) == 0 {
			// As loadConfig leaves them when the reload is refused.
			m.retryFiles = namedFiles(c)
			m.retryStamp = filesStamp(m.retryFiles)
		}
		for step := range 4 {
			switch file := fmt.Sprint(dir, "/present-", r.IntN(3)); r.IntN(4) {
			case 0:
				touch(file)
			case 1:
				_ = os.Remove(file)
			case 2:
				touch(path)
			}
			old, now := oldConfigChanged(m), m.configChanged()
			if now != old {
				t.Fatalf("round %d step %d: the configuration is found changed: %v; it was %v", round, step, now, old)
			}
			descriptors := m.descriptors.changed()
			if want := filesStamp(m.descriptors.files) != m.descriptors.stamp; descriptors != want {
				t.Fatalf("round %d step %d: the descriptor files are found changed: %v, and are as stamped: %v", round, step, descriptors, !want)
			}
			if len(namedDescriptorFiles(c)) == 0 {
				without++
				if descriptors || m.targetsFollowDescriptors() {
					t.Fatalf("round %d step %d: a configuration without descriptor files has one found changed: %v, or the target file read for one", round, step, descriptors)
				}
				continue
			}
			with++
			if descriptors && !old {
				changed++
			}
		}
	}
	if without < 100 || with < 100 || changed < 20 {
		t.Fatalf("the rounds held %d steps of configurations without descriptor files, %d with, %d of them changed by a descriptor file alone; too few to say", without, with, changed)
	}
}

// LoadStamped reads the configuration the exporter starts with as Load
// does: the same configuration, or the same error, for a file that loads,
// one that is refused, one that is not YAML and one that is not there. Only
// a file that could be read leaves the descriptor files in the stamp.
func TestLoadStampedReadsWhatLoadReads(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{
		"a configuration that loads in a build with http": testutil.WriteIn(t, dir, "minimal.yaml", testutil.MinimalConfig),
		"a collector without a request type":              testutil.WriteIn(t, dir, "untyped.yaml", "collectors:\n  - name: a\n    metrics:\n      - name: v\n        expression: .v\n"),
		"no collectors":                                   testutil.WriteIn(t, dir, "empty.yaml", "collectors: []\n"),
		"not YAML":                                        testutil.WriteIn(t, dir, "broken.yaml", "collectors: [\n"),
		"no file":                                         dir + "/absent.yaml",
	} {
		want, wantErr := Load(path)
		stamp := TakeStamp(path, true)
		got, err := LoadStamped(path, &stamp)
		if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
			t.Errorf("%s: LoadStamped fails with %v, Load with %v", name, err, wantErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: LoadStamped reads another configuration than Load", name)
		}
		if read := name != "not YAML" && name != "no file"; (stamp.descriptors != nil) != read {
			t.Errorf("%s: the stamp has descriptor files: %v, want %v", name, stamp.descriptors != nil, read)
		}
	}
}

// The files a collector's descriptor files lead to are asked for with the
// collector's request.type taken as the validation names it (fetch.ReadFiles),
// so that a collector not yet validated has the files it will have. A
// validated collector has the ones it had: every collector of the shipped
// configurations that load in this build — the examples, the ones under
// configs and the fixtures' — and of a table of generated ones is answered
// for by its type, looked up as the collector has it, as it was before.
func TestAValidatedCollectorLeadsToTheFilesItDid(t *testing.T) {
	old := func(c *model.Collector) ([]string, string) {
		if rt := fetch.RequestTypes[c.Request.Type]; rt != nil && rt.ReadFiles != nil {
			return rt.ReadFiles(c)
		}
		return nil, ""
	}
	same := func(where string, c *model.Collector) {
		t.Helper()
		paths, read := readFiles(c)
		if wantPaths, wantRead := old(c); !slices.Equal(paths, wantPaths) || read != wantRead {
			t.Errorf("%s: collector %q leads to %v with the mark %q; it led to %v with %q", where, c.Name, paths, read, wantPaths, wantRead)
		}
	}
	collectors := 0
	for _, root := range []string{"../../examples", "../../configs", "../../testdata"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			// A file that is no configuration, or one of a request type this
			// build has not, does not load, and has no validated collector.
			if c, loadErr := Load(path); loadErr == nil {
				for i := range c.Collectors {
					collectors++
					same(path, &c.Collectors[i])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if collectors == 0 && fetch.RequestTypes[fetch.RequestTypeHTTP] != nil {
		t.Fatal("no shipped configuration loaded in a build with http")
	}
	dir := t.TempDir()
	r := rand.New(rand.NewPCG(7, 8))
	generated := 0
	for range 300 {
		c := generatedConfig(r, dir)
		for i := range c.Collectors {
			generated++
			same("generated", &c.Collectors[i])
		}
	}
	if generated < 300 {
		t.Fatalf("the table held %d collectors; too few to say", generated)
	}
}
