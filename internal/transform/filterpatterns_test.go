package transform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// passthroughOf is a prometheus collector without rules, which passes on the
// series include and exclude pick.
func passthroughOf(include, exclude []string) *model.Collector {
	return &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus", Include: include, Exclude: exclude}}
}

// passedThrough are the names of the series a pass-through keeps of those
// named.
func passedThrough(t *testing.T, c *model.Collector, names ...string) []string {
	t.Helper()
	in := model.MetricSet{}
	for _, name := range names {
		in.Metrics = append(in.Metrics, model.Metric{Name: name, Type: model.GaugeMetricType, Value: 1})
	}
	set, _, err := applyPrometheusTransform(context.Background(), in, c, c.Transform, nil)
	if err != nil {
		t.Fatal(err)
	}
	kept := []string{}
	if set != nil {
		for _, m := range set.Metrics {
			kept = append(kept, m.Name)
		}
	}
	return kept
}

// An entry of transform.include or transform.exclude is a regular expression
// matched against a metric's name, anywhere in it, and the empty one matches
// every name. exclude: [""] loaded, and dropped every series of the target
// without a word; include: [""] loaded and kept every one, a filter that
// filters nothing. Neither can be meant, so the load refuses the entry,
// naming the collector and the key and saying what to write for either
// meaning: .* matches every name, and the key left out filters nothing.
func TestAnEmptyEntryOfIncludeOrExcludeIsRefused(t *testing.T) {
	for key, c := range map[string]*model.Collector{"include": passthroughOf([]string{""}, nil), "exclude": passthroughOf(nil, []string{""})} {
		want := fmt.Sprintf(`collector "node" transform.%s has an entry that is the empty string; an entry is a regular expression matched against a metric's name as the target gives it, anywhere in it, so the empty one matches every name: write '.*' to match every name, or leave transform.%s out to filter nothing`, key, key)
		if err := CheckTransformSettings(c); err == nil || err.Error() != want {
			t.Errorf("transform.%s: [\"\"]: %v\nwant %s", key, err, want)
		}
	}
	// An entry among others is refused all the same, once for each, and the
	// entries beside it are still checked.
	err := CheckTransformSettings(passthroughOf([]string{"^up$", "", "("}, []string{"", ""}))
	var problems model.Problems
	if !errors.As(err, &problems) || len(problems) != 4 {
		t.Fatalf("three empty entries and one that does not compile: %v", err)
	}
	for i, want := range []string{"transform.include has an entry that is the empty string", `transform.include "("`, "transform.exclude has an entry that is the empty string", "transform.exclude has an entry that is the empty string"} {
		if !strings.Contains(problems[i].Error(), want) {
			t.Errorf("problem %d is %v, want %s", i+1, problems[i], want)
		}
	}
	// What the message says to write does what it says: .* is every name,
	// and no key at all is no filter.
	names := []string{"up", "node_load1", "process_cpu_seconds_total"}
	for name, test := range map[string]struct {
		c    *model.Collector
		want []string
	}{
		"exclude: ['.*']": {passthroughOf(nil, []string{".*"}), []string{}},
		"include: ['.*']": {passthroughOf([]string{".*"}, nil), names},
		"neither":         {passthroughOf(nil, nil), names},
		"include: [load]": {passthroughOf([]string{"load"}, nil), []string{"node_load1"}},
		"exclude: ['^n']": {passthroughOf(nil, []string{"^n"}), []string{"up", "process_cpu_seconds_total"}},
	} {
		if err := CheckTransformSettings(test.c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := passedThrough(t, test.c, names...); !slices.Equal(got, test.want) {
			t.Errorf("%s passes on %v, want %v", name, got, test.want)
		}
	}
}

// An entry of nothing but blanks is a regular expression too, one that
// matches only the names that hold those blanks, which a UTF-8 name may:
// include: ["  "] loaded, and the pass-through then exported nothing, with
// no error. Written as bare blanks the entry is a slip — an empty string
// that picked up spaces, a template that filled in nothing — so the load
// refuses it, with every blank strings.TrimSpace takes off, and says how a
// pattern that means a blank is written: '[ ]' or '\x20', which compile and
// match the names with a space in them and no other.
func TestAnEntryOfIncludeOrExcludeThatIsNothingButBlanksIsRefused(t *testing.T) {
	for _, entry := range []string{" ", "  ", "\t", " \t\r\n", "\u00A0", "\u0085", "\u3000 ", "\u2028"} {
		for key, c := range map[string]*model.Collector{"include": passthroughOf([]string{entry}, nil), "exclude": passthroughOf(nil, []string{entry})} {
			want := fmt.Sprintf(`collector "node" transform.%s entry %q is nothing but blanks; an entry is a regular expression matched against a metric's name as the target gives it, anywhere in it, so this one matches only the names that hold these blanks: write the pattern that was meant, or, for one that does mean a blank, '[ ]' or '\x20' in single quotes, or leave transform.%s out to filter nothing`, key, entry, key)
			if err := CheckTransformSettings(c); err == nil || err.Error() != want {
				t.Errorf("transform.%s: [%q]: %v\nwant %s", key, entry, err, want)
			}
		}
	}
	// A blank among other characters is a pattern like any other, and so
	// is a character that only looks like a blank.
	names := []string{"disk free", "diskfree", "disk  free", "disk\tfree"}
	for entry, want := range map[string][]string{
		"[ ]": {"disk free", "disk  free"}, `\x20`: {"disk free", "disk  free"}, `\x20{2}`: {"disk  free"}, "k f": {"disk free"}, " free": {"disk free", "disk  free"},
		`\s`: {"disk free", "disk  free", "disk\tfree"}, `\t`: {"disk\tfree"}, "\u200B": {}, "\uFEFF": {},
	} {
		c := passthroughOf([]string{entry}, nil)
		if err := CheckTransformSettings(c); err != nil {
			t.Errorf("include: [%q]: %v", entry, err)
			continue
		}
		if got := passedThrough(t, c, names...); !slices.Equal(got, want) {
			t.Errorf("include: [%q] passes on %q, want %q", entry, got, want)
		}
	}
	// The message says single quotes: there YAML leaves \x20 as the four
	// characters the regular expression reads, and '[ ]' as text; in double
	// quotes "\x20" is the blank itself, and is refused as one.
	var written struct {
		Include []string `yaml:"include"`
	}
	if err := yaml.Unmarshal([]byte(`include: ['[ ]', '\x20', "\x20"]`), &written); err != nil || !slices.Equal(written.Include, []string{"[ ]", `\x20`, " "}) {
		t.Fatalf("YAML reads the entries as %q: %v", written.Include, err)
	}
	var problems model.Problems
	if err := CheckTransformSettings(passthroughOf(written.Include, nil)); errors.As(err, &problems) || err == nil || !strings.Contains(err.Error(), `transform.include entry " " is nothing but blanks`) {
		t.Fatalf("of the three, the one in double quotes alone is refused: %v", err)
	}
}

// checkTransformSettingsBeforeEmptyEntries is CheckTransformSettings as it
// was before an entry of include or exclude that is empty or nothing but
// blanks was refused, kept as an oracle: of every other setting, and of
// every other entry, the check must say what this says. Its collectors set
// no name_escaping, under which a name that is not classic is refused as it
// was, and told since what would export it (EscapingAdvice), here too.
func checkTransformSettingsBeforeEmptyEntries(x *model.Collector) error {
	t := x.Transform
	var errs []error
	passthrough := t.Type == "prometheus" && len(x.Metrics) == 0
	for _, setting := range []struct {
		key string
		set bool
	}{{"include", len(t.Include) > 0}, {"exclude", len(t.Exclude) > 0}, {"rename", len(t.Rename) > 0}} {
		if setting.set && !passthrough {
			errs = append(errs, fmt.Errorf("collector %q sets transform.%s, which picks or renames the metrics a prometheus transform passes through, so it applies only to a prometheus transform without metrics rules", x.Name, setting.key))
		}
	}
	for _, setting := range []struct {
		key         string
		expressions []string
	}{{"include", t.Include}, {"exclude", t.Exclude}} {
		for _, expression := range setting.expressions {
			if _, err := expr.CompileRegex(expression); err != nil {
				errs = append(errs, fmt.Errorf("collector %q transform.%s %q: %w", x.Name, setting.key, expression, err))
			}
		}
	}
	for _, from := range model.SortedKeys(t.Rename) {
		to := t.Rename[from]
		if err := checkMetricName(x, to); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.rename %q to %q: %w", x.Name, from, to, err))
		}
	}
	for _, name := range model.SortedKeys(t.Labels) {
		if !model.ValidLabelName(name) {
			errs = append(errs, fmt.Errorf("collector %q transform.labels has invalid label name %q%s", x.Name, name, EscapingAdvice(name)))
		} else if err := model.CheckLabelName(name); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.labels: %w", x.Name, err))
		}
	}
	targets := map[string]string{}
	for _, from := range model.SortedKeys(t.RenameLabels) {
		to := t.RenameLabels[from]
		if !model.ValidLabelName(to) {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels %q to invalid label name %q%s", x.Name, from, to, EscapingAdvice(to)))
			continue
		}
		if err := model.CheckLabelName(to); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels %q: %w", x.Name, from, err))
			continue
		}
		if other, taken := targets[to]; taken {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels renames both %q and %q to %q; a label can be the target of one rename", x.Name, other, from, to))
			continue
		}
		targets[to] = from
	}
	return model.JoinProblems(errs...)
}

// problemsOf are the problems an error holds, each as its text.
func problemsOf(err error) []string {
	var problems model.Problems
	switch {
	case err == nil:
		return nil
	case errors.As(err, &problems):
		texts := make([]string, len(problems))
		for i, problem := range problems {
			texts[i] = problem.Error()
		}
		return texts
	}
	return []string{err.Error()}
}

// settingsCheckedAsBefore puts a collector's transform settings through the
// check and through the check as it was, and fails unless the check says
// what it said and, for each entry of include and exclude that is empty or
// nothing but blanks, one thing more, which is about that entry. It returns
// how many such entries the collector has.
func settingsCheckedAsBefore(t *testing.T, x *model.Collector) int {
	t.Helper()
	now, was := problemsOf(CheckTransformSettings(x)), problemsOf(checkTransformSettingsBeforeEmptyEntries(x))
	var added []string
	for _, setting := range []struct {
		key     string
		entries []string
	}{{"include", x.Transform.Include}, {"exclude", x.Transform.Exclude}} {
		for _, entry := range setting.entries {
			switch {
			case entry == "":
				added = append(added, fmt.Sprintf("collector %q transform.%s has an entry that is the empty string;", x.Name, setting.key))
			case strings.TrimSpace(entry) == "":
				added = append(added, fmt.Sprintf("collector %q transform.%s entry %q is nothing but blanks;", x.Name, setting.key, entry))
			}
		}
	}
	// Without the problems of those entries, in their order, the check says
	// what it said, word for word and in the same order.
	rest, found := []string{}, 0
	for _, problem := range now {
		if found < len(added) && strings.HasPrefix(problem, added[found]) {
			found++
			continue
		}
		rest = append(rest, problem)
	}
	if found != len(added) || !slices.Equal(rest, was) {
		t.Errorf("%+v:\n now %q\n was %q\n and %d of the %d entries that are empty or blanks are refused", x.Transform, now, was, found, len(added))
	}
	return len(added)
}

// Refusing an entry of include or exclude that is empty or nothing but
// blanks changes what the check says of nothing else. The transform
// settings of every shipped configuration — the examples, the ones under
// configs and the fixtures' — and of a generated table of settings get what
// they got before: the same problems, word for word and in the same order,
// and none of the shipped ones has such an entry. In the table the two
// lists hold each of some entries, alone and among others — patterns that
// compile and that do not, the empty one, the blanks strings.TrimSpace
// takes off and two characters that only look like blanks — under a
// pass-through, a prometheus transform with rules and a jq transform, beside
// renames and labels that are in order and that are not; a collector with
// such an entry gets one problem more for each, and what it got before.
// Under the race detector each pair of lists is checked under one of the
// nine pairings of a transform with the rest of its settings and not under
// all: every list of include still meets every list of exclude, and every
// list each of the nine.
func TestOnlyAnEmptyOrBlankEntryOfIncludeOrExcludeIsRefusedAnew(t *testing.T) {
	files, collectors := 0, 0
	for _, root := range []string{"../../examples", "../../configs", "../../testdata", "../../charts"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			written := writtenCollectors(t, path)
			if len(written) > 0 {
				files++
			}
			for _, x := range written {
				collectors++
				if settingsCheckedAsBefore(t, &x) != 0 {
					t.Errorf("%s: collector %q has an entry of include or exclude that is empty or nothing but blanks", path, x.Name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 10 || collectors < 25 {
		t.Fatalf("%d files with %d collectors were found", files, collectors)
	}

	entries := []string{"", " ", "  ", "\t", "\n", " \t\r\n", "\u00A0", "\u0085", "\u3000", "\u2028 ", "\u200B", "\uFEFF", " \u200B ", "^up$", "node_.*", ".*", "(", "[ ]", `\x20`, "a b", " up", "up "}
	var lists [][]string
	lists = append(lists, nil, []string{})
	for _, entry := range entries {
		lists = append(lists, []string{entry}, []string{"^up$", entry}, []string{entry, "("}, []string{entry, entry})
	}
	tried, refused := 0, 0
	for s, shape := range []struct {
		transform string
		rules     []model.MetricRule
	}{{"prometheus", nil}, {"prometheus", []model.MetricRule{{Expression: "^up$"}}}, {"jq", []model.MetricRule{{Name: "v", Expression: ".v"}}}} {
		for i, include := range lists {
			for j, exclude := range lists {
				for r, rest := range []model.TransformConfig{
					{},
					{Rename: map[string]string{"up": "up2", "": "x", "a": "bad-name"}, Labels: map[string]string{"site": "", "bad-name": "x", "__name__": "y"}},
					{RemoveLabels: []string{"", "a"}, RenameLabels: map[string]string{"a": "b", "c": "b", "": "bad-name"}},
				} {
					if raceDetector && (i+j)%9 != 3*s+r {
						continue
					}
					rest.Type, rest.Include, rest.Exclude = shape.transform, include, exclude
					tried++
					if settingsCheckedAsBefore(t, &model.Collector{Name: "demo", Transform: rest, Metrics: shape.rules}) > 0 {
						refused++
					}
				}
			}
		}
	}
	if tried < alloctest.UnlessRaced(50000, 5500) || refused < alloctest.UnlessRaced(10000, 1100) {
		t.Fatalf("%d settings were tried and %d refused for an entry that is empty or blanks", tried, refused)
	}
	t.Logf("%d files with %d collectors, and %d generated settings, %d of them with an entry that is empty or blanks", files, collectors, tried, refused)
}

// writtenCollectors are the collectors a YAML file writes, under collectors
// at its top or anywhere below, as a chart's values hold them: read as they
// are written, without being validated, and none for a file that is no YAML
// or holds none.
func writtenCollectors(t *testing.T, path string) []model.Collector {
	t.Helper()
	var found []model.Collector
	for _, document := range yamlDocuments(t, path) {
		var walk func(node *yaml.Node)
		walk = func(node *yaml.Node) {
			if node.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(node.Content); i += 2 {
					if node.Content[i].Value == "collectors" && node.Content[i+1].Kind == yaml.SequenceNode {
						var collectors []model.Collector
						if node.Content[i+1].Decode(&collectors) == nil {
							found = append(found, collectors...)
						}
					}
				}
			}
			for _, child := range node.Content {
				walk(child)
			}
		}
		walk(document)
	}
	for i := range found {
		found[i].Transform.Type = strings.ToLower(strings.TrimSpace(found[i].Transform.Type))
	}
	return found
}

// yamlDocuments are the documents of a YAML file, none for a file that is
// not YAML, as a chart's templates are not.
func yamlDocuments(t *testing.T, path string) []*yaml.Node {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var documents []*yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			return documents
		}
		documents = append(documents, &document)
	}
}

// Every blank strings.TrimSpace takes off makes an entry that is refused,
// alone and beside another blank, and no other character does. Under the
// race detector every blank and every character of ASCII is tried still,
// and one in five of the others that a run without it tries.
func TestEveryBlankMakesAnEntryThatIsRefused(t *testing.T) {
	blanks := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF || !unicode.IsSpace(r) && r > 0x3000 && r%97 != 0 {
			continue
		}
		if raceDetector && !unicode.IsSpace(r) && r > unicode.MaxASCII && r%5 != 0 {
			continue
		}
		for _, entry := range []string{string(r), string(r) + " ", "a" + string(r)} {
			// A character a regular expression reads as more than itself
			// may not compile; only whether the entry is refused as empty
			// or as blanks is asked.
			err := CheckTransformSettings(passthroughOf([]string{entry}, nil))
			refused := err != nil && strings.Contains(err.Error(), "is nothing but blanks")
			if want := strings.TrimSpace(entry) == ""; refused != want {
				t.Errorf("include: [%q] (U+%04X): refused as blanks: %v, want %v: %v", entry, r, refused, want, err)
			}
		}
		if unicode.IsSpace(r) {
			blanks++
		}
	}
	if blanks != 25 {
		t.Fatalf("%d blanks were tried", blanks)
	}
}
