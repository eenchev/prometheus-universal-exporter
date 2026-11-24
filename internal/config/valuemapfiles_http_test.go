//go:build !select_request_types || request_type_http

package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// valueMapConfig is a configuration of one collector whose rule and whose
// label each read a value map from a file, beside one entry of their own.
func valueMapConfig(states, sites string) string {
	return `collectors:
  - name: devices
    request: {type: http}
    transform: {type: jq}
    metrics:
      - name: device_state
        items: .[]
        expression: .state
        value_map: {up: 5}
        value_map_file: ` + states + `
        labels:
          - name: site
            expression: .site
            value_map: {fra1: Frankfurt am Main}
            value_map_file: ` + sites + `
`
}

// A rule's value_map_file, and a label's, is read when the configuration
// loads and joined with the value_map beside it, whose entries win: the
// rule is as if the whole map were written in it. The files are among the
// ones the watch looks at, and a load without the files it names, as the
// target allocator's is, reads none and keeps the keys.
func TestAValueMapIsReadFromItsFile(t *testing.T) {
	dir := t.TempDir()
	states := testutil.WriteIn(t, dir, "states.yaml", "up: 1\ndown: 0\n\"3\": 0.5\n\"*\": -1\n")
	sites := testutil.WriteIn(t, dir, "sites.json", `{"fra1": "Frankfurt", "ams2": "Amsterdam", "7": 7, "gone": null, "on": true}`)
	path := testutil.WriteIn(t, dir, "config.yaml", valueMapConfig(states, sites))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rule := cfg.Collectors[0].Metrics[0]
	if len(rule.ValueMap) != 4 || rule.ValueMap["up"] != 5 || rule.ValueMap["down"] != 0 || rule.ValueMap["3"] != 0.5 || rule.ValueMap["*"] != -1 {
		t.Errorf("the rule's map: %v", rule.ValueMap)
	}
	label := rule.Labels[0].ValueMap
	if len(label) != 5 || label["fra1"] != "Frankfurt am Main" || label["ams2"] != "Amsterdam" || label["7"] != "7" || label["gone"] != "" || label["on"] != "true" {
		t.Errorf("the label's map: %v", label)
	}
	if named := namedFiles(cfg); !slices.Contains(named, states) || !slices.Contains(named, sites) {
		t.Errorf("the watch does not look at the files: %v", named)
	}
	// The allocator's load opens neither file.
	missing := filepath.Join(dir, "on-the-replicas.yaml")
	unread, err := Load(testutil.WriteIn(t, dir, "unread.yaml", valueMapConfig(missing, missing)), WithoutNamedFiles())
	if err != nil {
		t.Fatal(err)
	}
	if rule := unread.Collectors[0].Metrics[0]; len(rule.ValueMap) != 1 || rule.ValueMapFile != missing || len(rule.Labels[0].ValueMap) != 1 {
		t.Errorf("a load without the files read one: %+v", rule)
	}
}

// What is wrong with a value_map_file is said when the configuration
// loads, naming the rule and the file and nothing the file holds but a
// key: a path that is not one, a file that is missing, that is no mapping,
// that maps a rule's value to what is no number, or that is a reference to
// a secret store.
func TestAValueMapFileIsChecked(t *testing.T) {
	dir := t.TempDir()
	good := testutil.WriteIn(t, dir, "good.yaml", "up: 1\n")
	for want, files := range map[string][2]string{
		`collector "devices" metrics rule 1 value_map_file "maps/states.yaml" is a relative path`:             {"maps/states.yaml", good},
		`value_map_file has a {{param_...}} placeholder, which is not filled in there`:                        {"/maps/{{param_site}}.yaml", good},
		`metrics rule 1 label "site" value_map_file ` + filepath.Join(dir, "none.yaml") + `: no such file`:    {good, filepath.Join(dir, "none.yaml")},
		`value_map_file ` + testutil.WriteIn(t, dir, "list.yaml", "- s3cret-line\n") + `: does not hold a ma`: {good, filepath.Join(dir, "list.yaml")},
		`maps "down" to a value that is no number; a rule's value_map maps text to the series' value`:         {testutil.WriteIn(t, dir, "words.yaml", "down: s3cret-word\n"), good},
		`line 2: the key "a" is written twice`:                                                                {good, testutil.WriteIn(t, dir, "twice.yaml", "a: 1\na: 2\n")},
		`line 1: an entry is not a key and its value`:                                                         {good, testutil.WriteIn(t, dir, "nested.yaml", "a: {b: s3cret-nested}\n")},
		`a reference to a secret store, and the key names a file that is opened as one`:                       {good, "secret://vault/maps#sites"},
		`value_map_file ` + dir + `: is not a regular file`:                                                   {dir, good},
	} {
		_, err := Load(testutil.WriteIn(t, dir, "config.yaml", valueMapConfig(files[0], files[1])))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}
