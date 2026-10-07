package config

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// A value YAML reads as none — nothing after a colon or a dash, null or ~ —
// is refused where a file takes a value: a key's, an entry of a list, a
// value of a mapping. The decoder took each without a word: the key as if
// it were left out, the entry as if its dash were not there, so that a rule
// meant under a bare dash was missing with nothing said, and the mapping's
// value as "" or 0, so that value_map: {up: } mapped up to 0. Each is told
// by its line, with the key, the list and the entry's place in it or the
// mapping and its key, how it is written, and what to do; several are all
// told, in the order of the file, after whatever the decoder itself
// refuses. The checks of the configuration, which come after the file is
// read, are not reached.
func TestAKeyAnEntryOrAMappingValueWithNoValueIsRefused(t *testing.T) {
	const key, entry = ", which YAML reads as no value at all; write its value, or take the key out", ", which YAML reads as no value at all, so the entry would be left out without a word; write %s there, or take the entry out"
	for name, tc := range map[string]struct {
		document string
		want     []string
	}{
		"a bare dash among the rules": {"collectors:\n  - name: node\n    metrics:\n      -\n      - name: up\n", []string{"line 4: metrics entry 1 is a dash with nothing after it" + fmt.Sprintf(entry, "a metric rule")}},
		"a dash written null and one ~": {"collectors:\n  - name: node\n    metrics:\n      - name: up\n      - null\n      - ~\n", []string{
			"line 5: metrics entry 2 is written null" + fmt.Sprintf(entry, "a metric rule"), "line 6: metrics entry 3 is written ~" + fmt.Sprintf(entry, "a metric rule"),
		}},
		"a label, a collector and a file": {"collector_files:\n  -\ncollectors:\n  -\n  - name: node\n    metrics:\n      - name: up\n        labels:\n          -\n", []string{
			"line 2: collector_files entry 1 is a dash with nothing after it" + fmt.Sprintf(entry, "a value"),
			"line 4: collectors entry 1 is a dash with nothing after it" + fmt.Sprintf(entry, "a collector"),
			"line 9: labels entry 1 is a dash with nothing after it" + fmt.Sprintf(entry, "a label"),
		}},
		"entries in a flow list": {"collectors:\n  - name: node\n    transform: {include: [up, ~, null]}\n", []string{"line 3: include entry 2 is written ~" + fmt.Sprintf(entry, "a value"), "line 3: include entry 3 is written null" + fmt.Sprintf(entry, "a value")}},
		"keys of text and of a number": {"collectors:\n  - name: node\n    name_escaping:\n    max_concurrent_probes: null\n    request:\n      max_age: ~\n", []string{
			"line 3: name_escaping has nothing after its colon" + key, "line 4: max_concurrent_probes is written null" + key, "line 6: max_age is written ~" + key,
		}},
		"a required key": {"collectors:\n  - name:\n    request:\n      type:\n", []string{"line 2: name has nothing after its colon" + key, "line 4: type has nothing after its colon" + key}},
		"mappings and lists as a whole": {"collectors:\n  - name: node\n    request:\n    transform: ~\n    limits: Null\n    metrics:\n", []string{
			"line 3: request has nothing after its colon" + key, "line 4: transform is written ~" + key, "line 5: limits is written Null" + key, "line 6: metrics has nothing after its colon" + key,
		}},
		"the top level": {"collectors:\ncollector_files: NULL\nweb:\notlp:\n", []string{"line 1: collectors has nothing after its colon" + key, "line 2: collector_files is written NULL" + key, "line 3: web has nothing after its colon" + key, "line 4: otlp has nothing after its colon" + key}},
		"a value of a mapping": {"collectors:\n  - name: node\n    metrics:\n      - name: state\n        value_map: {up: , down: 0}\n    transform:\n      labels:\n        site: ~\n", []string{
			`line 5: value_map key "up" has nothing after its colon` + key, `line 8: labels key "site" is written ~` + key,
		}},
		"a block that enabled switches on": {"otlp:\n  enabled:\n  endpoint: http://collector:4318/v1/metrics\nweb:\n  basic_auth:\n    enabled: true\n    username:\n", []string{
			"line 3: otlp sets endpoint but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them",
			"line 2: enabled has nothing after its colon" + key, "line 7: username has nothing after its colon" + key,
		}},
		"beside what the decoder refuses": {"collectors:\n  - name: node\n    requst: {}\n    limits:\n      max_metrics: 0.5\n      max_help_length:\n    metrics:\n      - name: m\n        value_map: {null: 1}\n", []string{
			`line 3: unknown key "requst" in a collector`, "line 5: max_metrics is 0.5, which is not a whole number", "line 6: max_help_length has nothing after its colon" + key,
			"line 9: value_map has the key null, which YAML reads as no key at all, so the entry would be dropped; to use that text as the key, quote it",
		}},
		// The value an alias names is the value: it is told where the key
		// that takes it is written.
		"through an alias": {"x-none: &none ~\ncollectors:\n  - name: node\n    metrics_prefix: *none\n    metrics: [*none]\n", []string{"line 4: metrics_prefix is written ~" + key, "line 5: metrics entry 1 is written ~" + fmt.Sprintf(entry, "a metric rule")}},
		// A key the mapping writes with no value is the one the decoder
		// reads, over the one a merge key would supply.
		"over a merged key":   {"x-request: &request {type: http, max_age: 5s}\ncollectors:\n  - name: node\n    request:\n      <<: *request\n      max_age:\n", []string{"line 6: max_age has nothing after its colon" + key}},
		"in a merged mapping": {"x-request: &request {type: http, max_age: }\ncollectors:\n  - name: node\n    request:\n      <<: *request\n", []string{"line 1: max_age has nothing after its colon" + key}},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", tc.document))
		if want := strings.Join(tc.want, "; "); err == nil || err.Error() != want {
			t.Errorf("%s: the load says\n%v\nwant\n%s", name, err, want)
		}
	}
	// A collector file's and the target file's are told the same way.
	dir := t.TempDir()
	file := testutil.WriteIn(t, dir, "collectors.yaml", "collectors:\n  - name: node\n    metrics:\n      -\n    request:\n      type:\n")
	want := "collector file " + file + ": line 4: metrics entry 1 is a dash with nothing after it" + fmt.Sprintf(entry, "a metric rule") + "; line 6: type has nothing after its colon" + key
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")); err == nil || err.Error() != want {
		t.Errorf("in a collector file: %v\nwant %s", err, want)
	}
	want = "line 1: interval has nothing after its colon" + key + "; line 3: targets entry 1 is a dash with nothing after it" + fmt.Sprintf(entry, "a static target") + "; line 6: path is written ~" + key + `; line 7: labels key "site" has nothing after its colon` + key
	if _, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval:\ntargets:\n  -\n  - collector: node\n    request:\n      path: ~\n    labels: {site: }\n")); err == nil || err.Error() != want {
		t.Errorf("in the target file: %v\nwant %s", err, want)
	}

	// What is not refused: a value an anchor holds and the mapping that
	// merges it in overrides, which the decoder never reads there; a file's
	// own x- key, whatever it holds; a document after --- that is empty;
	// and text that is empty, which is a value.
	for name, document := range map[string]string{
		"overridden":        "x-request: &request {type: http, max_age: }\ncollectors:\n  - name: node\n    request:\n      <<: *request\n      max_age: 5s\n",
		"the file's own":    "x-own:\nx-list: [~, null]\nx-map: {a: }\ncollectors:\n  - name: node\n",
		"an empty document": "collectors:\n  - name: node\n---\n",
		"empty text":        "collectors:\n  - name: node\n    name_escaping: \"\"\n    metrics_prefix: ''\n    limits: {}\n    metrics: []\n    transform: {labels: {site: \"\"}, include: []}\n",
	} {
		if got := valueProblems([]byte(document), reflect.TypeOf(model.Config{})); got != nil {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// noValueHere stands, in a document being written, where a value that is
// none is then put: text no shipped file holds.
const noValueHere = "NOVALUEHERE"

// noValueSite is a place of a document where a value that is none was put:
// what valueProblems must say of it, after the line, for each way it is
// written.
type noValueSite struct {
	// said takes how the value was written, as the message says it.
	said func(how string) string
	// kind is what the place is: a key, an entry of a list, which reads
	// otherwise when nothing is written, or a value of a mapping.
	kind string
	// of is the type the files' types read the place into, path the place
	// as the schemas have it, and named what a message about a value
	// written there calls it (TestOnlyAValueOfAnotherKindIsRefusedAnew).
	of          reflect.Type
	path, named string
}

// The kinds of place a value is written at.
const (
	siteKey   = "a key"
	siteEntry = "an entry of a list"
	siteValue = "a value of a mapping"
)

// eachNoValueSite puts noValueHere, in turn, at every place of the document
// where a file of type t takes a value, and calls visit with the place while
// it stands there: at every key of every mapping the type has a key for, in
// place of its value or, where the mapping does not write the key, as a key
// more; as an entry of every list, first, in the middle and last, and as the
// one entry of a list the mapping does not write; and as a value of every
// mapping of names to values, and of one the mapping does not write. The
// document is as it was when it returns.
//
// A mapping that merges another in, and anything under an anchor or behind
// an alias, is left as it is: what is written there may be read in several
// places or in none, which is tested apart
// (TestAKeyAnEntryOrAMappingValueWithNoValueIsRefused).
func eachNoValueSite(doc *yaml.Node, t reflect.Type, visit func(noValueSite)) {
	entries := map[string]string{"Collector": "a collector", "MetricRule": "a metric rule", "LabelRule": "a label", "StaticTarget": "a static target"}
	here := func() *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: noValueHere} }
	scalar := func(text string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text} }
	ofKey := func(key string) noValueSite {
		return noValueSite{kind: siteKey, said: func(how string) string {
			return key + " " + how + ", which YAML reads as no value at all; write its value, or take the key out"
		}}
	}
	ofEntry := func(key string, at int, elem reflect.Type) noValueSite {
		what, ok := entries[elem.Name()]
		if !ok {
			what = "a value"
		}
		return noValueSite{kind: siteEntry, said: func(how string) string {
			return fmt.Sprintf("%s entry %d %s, which YAML reads as no value at all, so the entry would be left out without a word; write %s there, or take the entry out", key, at+1, how, what)
		}}
	}
	ofValue := func(key string) noValueSite {
		return noValueSite{kind: siteValue, said: func(how string) string {
			return fmt.Sprintf("%s key %q %s, which YAML reads as no value at all; write its value, or take the key out", key, "zz", how)
		}}
	}
	at := func(site noValueSite, of reflect.Type, path, named string) noValueSite {
		for of.Kind() == reflect.Pointer {
			of = of.Elem()
		}
		site.of, site.path, site.named = of, path, named
		return site
	}
	var walk func(n *yaml.Node, t reflect.Type, key, path string)
	walk = func(n *yaml.Node, t reflect.Type, key, path string) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			return
		}
		switch {
		case t.Kind() == reflect.Struct && n.Kind == yaml.MappingNode:
			written := map[string]int{}
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Tag == "!!merge" {
					return
				}
				written[n.Content[i].Value] = i
			}
			for f := 0; f < t.NumField(); f++ {
				field := t.Field(f)
				name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
				if name == "" || name == "-" {
					continue
				}
				kind := field.Type
				for kind.Kind() == reflect.Pointer {
					kind = kind.Elem()
				}
				keyPath := joinSchemaPath(path, name)
				if i, ok := written[name]; ok {
					value := n.Content[i+1]
					n.Content[i+1] = here()
					visit(at(ofKey(name), kind, keyPath, name))
					n.Content[i+1] = value
					walk(value, field.Type, name, keyPath)
					continue
				}
				content := n.Content
				n.Content = append(content[:len(content):len(content)], scalar(name), here())
				visit(at(ofKey(name), kind, keyPath, name))
				switch kind.Kind() {
				case reflect.Slice:
					n.Content[len(n.Content)-1] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{here()}}
					visit(at(ofEntry(name, 0, kind.Elem()), kind.Elem(), keyPath+"[]", name+" entry 1"))
				case reflect.Map:
					n.Content[len(n.Content)-1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{scalar("zz"), here()}}
					visit(at(ofValue(name), kind.Elem(), keyPath+".*", name+` key "zz"`))
				}
				n.Content = content
			}
		case t.Kind() == reflect.Slice && n.Kind == yaml.SequenceNode:
			content := n.Content
			for _, i := range slices.Compact([]int{0, len(content) / 2, len(content)}) {
				n.Content = slices.Insert(slices.Clone(content), i, here())
				visit(at(ofEntry(key, i, t.Elem()), t.Elem(), path+"[]", fmt.Sprintf("%s entry %d", key, i+1)))
			}
			n.Content = content
			for _, item := range content {
				walk(item, t.Elem(), key, path+"[]")
			}
		case t.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
			content := n.Content
			n.Content = append(content[:len(content):len(content)], scalar("zz"), here())
			visit(at(ofValue(key), t.Elem(), path+".*", key+` key "zz"`))
			n.Content = content
		}
	}
	walk(doc.Content[0], t, "the document", "")
}

// shippedDocuments are the documents the repository ships that the exporter
// reads as a configuration, a collector file or a target file, each with
// the type it is read into: the examples, the files under configs and the
// fixtures, and every fenced YAML block of the documentation that is one.
func shippedDocuments(t *testing.T) map[string]reflect.Type {
	t.Helper()
	documents := map[string]reflect.Type{}
	add := func(text string) {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(text), &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			return
		}
		keys := map[string]bool{}
		for i := 0; i+1 < len(doc.Content[0].Content); i += 2 {
			keys[doc.Content[0].Content[i].Value] = true
		}
		switch {
		case keys["targets"] && !keys["collectors"]:
			documents[text] = reflect.TypeOf(model.StaticTargetFile{})
		case keys["collectors"] || keys["collector_files"] || keys["otlp"] || keys["web"]:
			documents[text] = reflect.TypeOf(model.Config{})
		}
	}
	for _, root := range []string{"../../examples", "../../configs", "../../testdata", "../../docs", "../../README.md"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			switch filepath.Ext(path) {
			case ".yaml", ".yml":
				add(string(raw))
			case ".md":
				for _, part := range strings.Split(string(raw), "```yaml\n")[1:] {
					block, _, _ := strings.Cut(part, "```")
					add(block)
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return documents
}

// Refusing a value YAML reads as none changes what the load says of nothing
// else. valueProblems, which is all of the load that changed, says of every
// document the repository ships — the examples, the files under configs,
// the fixtures and each configuration and target file among the fenced
// blocks of the documentation, none of which writes such a value — what it
// said, which of all but a few is nothing; and so of some documents that
// have what it refused before, a limit with a fraction and a key that is no
// key. Of each of those documents with one such value put at one place —
// at every key its mappings write and every key they could, in every list,
// first, in the middle and last, and in every mapping of names to values,
// written as nothing, as null and as ~ in turn (eachNoValueSite) — it says
// what it said of that document before, word for word and in order, and
// one thing more: that place, by its line and in the words of its kind.
// The decoder and the checks of the configuration are as they were, and
// the first is not reached for a document valueProblems refuses.
//
// The places are some nine thousand, and each is a document written out
// and read twice, so of each kind of place in each kind of file the first
// and every tenth after it is tried, and under the race detector, where one
// costs many times more, every hundred and twentieth.
func TestOnlyAValueThatIsNoneIsRefusedAnew(t *testing.T) {
	documents := shippedDocuments(t)
	shipped := len(documents)
	for _, document := range []string{
		"collectors:\n  - name: node\n    max_concurrent_probes: 1.9\n    limits:\n      max_metrics: 0.5\n    metrics:\n      - name: state\n        value_map: {null: 0, ~: 1, up: 1}\n        labels:\n          - name: site\n            value: x\n",
		"interval: 1m\nconcurrency: 2.5\ntargets:\n  - collector: node\n    request:\n      retry: {attempts: 1.5}\n    labels: {~: x}\n",
		"web: {self_metrics: {verbose: true}}\notlp: {enabled: false, max_pending_points: 10.5, headers: {null: x}}\ncollector_files: ['a.yaml']\n",
	} {
		documents[document] = reflect.TypeOf(model.Config{})
		if strings.HasPrefix(document, "interval") {
			documents[document] = reflect.TypeOf(model.StaticTargetFile{})
		}
	}
	every := alloctest.UnlessRaced(10, 120)
	at, tried, refusedBefore := 0, 0, 0
	seen, kinds := map[string]int{}, map[string]int{}
	for _, text := range model.SortedKeys(documents) {
		kind := documents[text]
		was := valueProblemsBeforeNoValue([]byte(text), kind)
		if now := valueProblems([]byte(text), kind); !slices.Equal(now, was) {
			t.Errorf("of a document that writes no value as none the load says\n%q\nand said\n%q\n%s", now, was, text)
			continue
		}
		if len(was) > 0 {
			refusedBefore++
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
			t.Fatal(err)
		}
		// Written in block style throughout, where nothing after a colon
		// or a dash is a value that is none.
		var block func(n *yaml.Node)
		block = func(n *yaml.Node) {
			if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
				n.Style &^= yaml.FlowStyle
			}
			for _, child := range n.Content {
				block(child)
			}
		}
		block(&doc)
		eachNoValueSite(&doc, kind, func(site noValueSite) {
			class := site.kind + " of a " + kind.Name()
			if at, seen[class] = at+1, seen[class]+1; seen[class]%every != 1 {
				return
			}
			out, err := yaml.Marshal(&doc)
			before, _, marked := strings.Cut(string(out), noValueHere)
			if err != nil || !marked || strings.Count(string(out), noValueHere) != 1 {
				t.Fatalf("writing the document with a place marked: %v\n%s", err, out)
			}
			line := strings.Count(before, "\n") + 1
			value := []string{"", "null", "~"}[tried%3]
			how := "is written " + value
			switch {
			case value == "" && site.kind == siteEntry:
				how = "is a dash with nothing after it"
			case value == "":
				how = "has nothing after its colon"
			}
			written := []byte(strings.Replace(string(out), noValueHere, value, 1))
			want := "line " + strconv.Itoa(line) + ": " + site.said(how)
			now, was := valueProblems(written, kind), valueProblemsBeforeNoValue(written, kind)
			found := slices.Index(now, want)
			if found < 0 || !slices.Equal(slices.Delete(slices.Clone(now), found, found+1), was) {
				t.Errorf("the load says\n%q\nand said\n%q\nwant what it said and %s\n%s", now, was, want, written)
			}
			tried++
			kinds[class]++
		})
	}
	if shipped < 40 || refusedBefore < 3 || tried < alloctest.UnlessRaced(900, 75) || len(kinds) != 6 {
		t.Fatalf("%d documents are shipped, %d were refused before, and %d places were tried, of these kinds: %v", shipped, refusedBefore, tried, kinds)
	}
	t.Logf("%d documents, %d of them refused before, and %d of %d places tried: %v", len(documents), refusedBefore, tried, at, kinds)
}

// valueProblemsBeforeNoValue is valueProblems as it was before a value YAML
// reads as none was refused, kept as an oracle: of a document that writes no
// such value valueProblems must say what this says, and of one that does,
// that and one problem for each such value the decoder reads.
func valueProblemsBeforeNoValue(document []byte, t reflect.Type) []string {
	var doc yaml.Node
	if yaml.Unmarshal(document, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	var problems []string
	visiting := map[*yaml.Node]bool{}
	var walk func(n *yaml.Node, t reflect.Type, key string)
	walk = func(n *yaml.Node, t reflect.Type, key string) {
		for n != nil && n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		// A node that holds itself is refused by the decoder.
		if n == nil || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Struct:
			if n.Kind != yaml.MappingNode {
				return
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
				if name != "" && name != "-" {
					fields[name] = t.Field(i).Type
				}
			}
			for _, entry := range model.DecodedEntries(n) {
				if field, ok := fields[entry.Key.Value]; ok {
					walk(entry.Value, field, entry.Key.Value)
				}
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for _, item := range n.Content {
					walk(item, t.Elem(), key)
				}
			}
		case reflect.Map:
			if n.Kind != yaml.MappingNode {
				return
			}
			for _, entry := range model.DecodedEntries(n) {
				if entry.Key.Kind == yaml.ScalarNode && entry.Key.ShortTag() == "!!null" {
					written := entry.Key.Value
					if written == "" {
						written = "nothing"
					}
					problems = append(problems, fmt.Sprintf("line %d: %s has the key %s, which YAML reads as no key at all, so the entry would be dropped; to use that text as the key, quote it", entry.Key.Line, key, written))
				}
				walk(entry.Value, t.Elem(), key)
			}
		case reflect.Int, reflect.Int64:
			// A duration and a size read themselves, and say what they take.
			if t == durationType || t == byteSizeType || n.Kind != yaml.ScalarNode || n.ShortTag() != "!!float" {
				return
			}
			if value, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil && value != math.Trunc(value) {
				problems = append(problems, fmt.Sprintf("line %d: %s is %s, which is not a whole number", n.Line, key, n.Value))
			}
		}
	}
	walk(doc.Content[0], t, "the document")
	return problems
}
