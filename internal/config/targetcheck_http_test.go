//go:build !select_request_types || request_type_http

package config

import (
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The check of a static target file against a configuration goes through
// the configuration's collectors once, to note where each is by its name
// (collectorsByName), whatever the targets are: for one target and for 400,
// each of its own collector or all of the last one, and for a file it
// refuses at its last target as for one it accepts. It went through them
// four times for every target.
func TestTheCheckOfATargetFileGoesThroughTheCollectorsOnce(t *testing.T) {
	for _, tc := range []struct {
		what                string
		collectors, targets int
		last                bool
		edit                func(f *model.StaticTargetFile)
		refused             string
	}{
		{what: "one target of one collector", collectors: 1, targets: 1},
		{what: "one target of 400 collectors", collectors: 400, targets: 1, last: true},
		{what: "400 targets, each of its own collector", collectors: 400, targets: 400},
		{what: "400 targets of the last collector", collectors: 400, targets: 400, last: true},
		{what: "400 targets, the last of no collector", collectors: 400, targets: 400, edit: func(f *model.StaticTargetFile) { f.Targets[399].Collector = "missing" }, refused: `target "target_00399" references unknown collector "missing"`},
		{what: "400 targets, the last without the param its collector needs", collectors: 400, targets: 400, edit: func(f *model.StaticTargetFile) { f.Targets[399].Params = map[string]string{"param_other": "x"} }, refused: `target "target_00399" uses collector "collector_00399", whose request.path needs param_tenant, a parameter without a default; a static target has no probe to supply it, so set it under the target's params, give the placeholder a default, or set request.path on the target`},
	} {
		file, cfg := targetCheckPair(t, tc.collectors, tc.targets, tc.last)
		if tc.edit != nil {
			tc.edit(file)
		}
		indexed := countCollectorIndexes(t)
		err := ValidateStaticTargetsAgainst(file, cfg)
		if got := indexed.Load(); got != 1 {
			t.Errorf("%s: the collectors were gone through %d times, want once", tc.what, got)
		}
		if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || err.Error() != tc.refused) {
			t.Errorf("%s: the check says %v, want %q", tc.what, err, tc.refused)
		}
	}
}

// Every static target file the repository ships - the documented one under
// configs, the examples' and the chart's test data - is checked against
// every configuration it ships that this build loads, the one written for it
// and all the others, and the check says of each pair what it said while it
// went through the collectors for every target, as the collectors whose
// descriptor files it opens are those it found before.
func TestTheCheckOfTheShippedTargetFilesIsAsItWas(t *testing.T) {
	var files []*model.StaticTargetFile
	var configs []*model.Config
	for _, root := range []string{"configs", "examples", filepath.Join("testdata", "chart")} {
		err := filepath.WalkDir(filepath.Join("..", "..", root), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".yaml" {
				return err
			}
			if strings.HasPrefix(entry.Name(), "static-targets") {
				file, err := LoadStaticTargets(path)
				if err == nil {
					err = ValidateStaticTargets(file)
				}
				if err != nil {
					t.Errorf("%s: %v", path, err)
				}
				files = append(files, file)
				return nil
			}
			// A configuration of a request type the build lacks, and a file
			// that is no configuration, is not loaded and not compared.
			if cfg, err := Load(path); err == nil {
				configs = append(configs, cfg)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 4 || len(configs) < 10 {
		t.Fatalf("%d static target files and %d configurations found, want at least 4 and 10: the comparison must not pass by finding nothing", len(files), len(configs))
	}
	accepted, refused := 0, 0
	for _, file := range files {
		for _, cfg := range configs {
			was, got := oldValidateStaticTargetsAgainst(file, cfg), ValidateStaticTargetsAgainst(file, cfg)
			if (was == nil) != (got == nil) || was != nil && was.Error() != got.Error() {
				t.Errorf("the check says %v, and said %v", got, was)
			}
			if was == nil {
				accepted++
			} else {
				refused++
			}
			for _, other := range configs[:2] {
				if got, was := targetsChecked(file, cfg, other), oldTargetsChecked(file, cfg, other); !reflect.DeepEqual(got, was) {
					t.Errorf("the collectors whose descriptor files the check opens are those of %v, and were those of %v", namedFiles(got), namedFiles(was))
				}
			}
		}
	}
	if accepted < 2 || refused < 20 {
		t.Errorf("%d pairs agree and %d do not, want at least 2 and 20", accepted, refused)
	}
}
