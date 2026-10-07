package repository

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A place of a file that has a schema where a value is written: a key, an
// entry of a list or a value of a mapping, by the chain of keys that leads
// to it, with [] for an entry of a list and {} for a value of a mapping.
type valuePlace []string

// valuePlaces are the places of a schema's file where a value is written,
// every one: each key the schema has, at any depth, each list's entry and
// each mapping's value. They are read from the schema itself, so a key that
// is added to a file is among them without anyone adding it.
func valuePlaces(schema map[string]any) []valuePlace {
	var places []valuePlace
	var walk func(node map[string]any, path valuePlace)
	walk = func(node map[string]any, path valuePlace) {
		properties, _ := node["properties"].(map[string]any)
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			at := append(path[:len(path):len(path)], key)
			places = append(places, at)
			walk(properties[key].(map[string]any), at)
		}
		if items, ok := node["items"].(map[string]any); ok {
			at := append(path[:len(path):len(path)], "[]")
			places = append(places, at)
			walk(items, at)
		}
		if values, ok := node["additionalProperties"].(map[string]any); ok {
			at := append(path[:len(path):len(path)], "{}")
			places = append(places, at)
			walk(values, at)
		}
	}
	walk(schema, nil)
	return places
}

// written is a document that holds the place and nothing beside it, every
// mapping and list on the way there having that one key or entry, with
// value written at the place, after its colon or its dash: nothing, " null"
// or " ~". A mapping's one key is k. The place is on the document's last
// line.
func (p valuePlace) written(value string) string {
	var b strings.Builder
	indent, dash := "", ""
	for i, part := range p {
		end := "\n"
		if i == len(p)-1 {
			end = value + "\n"
		}
		switch part {
		case "[]":
			// The entry begins on the dash's own line.
			if i == len(p)-1 {
				fmt.Fprintf(&b, "%s-%s", indent, end)
				continue
			}
			dash = "- "
			continue
		case "{}":
			part = "k"
		}
		fmt.Fprintf(&b, "%s%s%s:%s", indent, dash, part, end)
		indent, dash = indent+strings.Repeat(" ", 2+len(dash)), ""
	}
	return b.String()
}

// said is what the exporter says of the place written with no value, after
// the line: the key, or the list and the entry's place in it, or the
// mapping and the key, and how the value that is none was written.
func (p valuePlace) said(value string) string {
	last, key := p[len(p)-1], ""
	if len(p) > 1 {
		key = p[len(p)-2]
	}
	how := "is written" + value
	switch {
	case last == "[]" && value == "":
		return key + " entry 1 is a dash with nothing after it, which YAML reads as no value at all"
	case last == "[]":
		return key + " entry 1 " + how + ", which YAML reads as no value at all"
	case value == "":
		how = "has nothing after its colon"
	}
	if last == "{}" {
		return key + ` key "k" ` + how + ", which YAML reads as no value at all"
	}
	return last + " " + how + ", which YAML reads as no value at all"
}

// found is where a validator of the schema finds the place in the document
// written makes: the keys joined by dots, a list's one entry as [0] and a
// mapping's one key as k.
func (p valuePlace) found() string {
	var b strings.Builder
	for _, part := range p {
		switch part {
		case "[]":
			b.WriteString("[0]")
		case "{}":
			b.WriteString(".k")
		default:
			b.WriteString(".")
			b.WriteString(part)
		}
	}
	return b.String()
}

// A value YAML reads as none — nothing after the colon or the dash, null or
// ~ — is refused wherever one of the three files that have a schema takes a
// value: at every key, at every entry of a list and at every value of a
// mapping, by the schema, to which null is none of the types a key takes,
// and by the exporter, which names the key, the entry or the mapping's key
// and the line, and says what to do. The exporter loaded each: a key as if
// it had been left out, a static target's request.path and request.body as
// one written "", an entry of a list as if its dash were not there, and a
// mapping's value as "" or as 0. The places are read from the committed
// schemas (valuePlaces), so one that a file gains is held to it here
// without a row of its own: some hundred and eighty in the configuration and
// fifty in the target file, each list and each mapping among them. A top-level
// x- key, which is the file's own, takes any value, none too, to both.
//
// A collector file's collectors are the configuration's, so its places are
// tried in one spelling each, and under the race detector, where a load
// costs several times more, so are the configuration's and the target
// file's: each of the three spellings still at a third of the places of
// each kind, and every place in one.
func TestNoKeyEntryOrMappingValueOfTheSchemasTakesNoValue(t *testing.T) {
	spellings := []string{"", " null", " ~"}
	type file struct {
		name   string
		schema map[string]any
		load   func(document string) error
		every  bool
		least  int
	}
	dir := t.TempDir()
	collectorFiles := testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")
	files := []file{
		{"the configuration", loadSchema(t), func(document string) error {
			_, err := config.Load(testutil.WriteIn(t, dir, "alone.yaml", document))
			return err
		}, alloctest.UnlessRaced(true, false), 180},
		{"a collector file", loadSchemaFile(t, collectorFileSchemaFile), func(document string) error {
			testutil.WriteIn(t, dir, "collectors.yaml", document)
			_, err := config.Load(collectorFiles)
			return err
		}, false, 145},
		{"the target file", loadSchemaFile(t, staticTargetsSchemaFile), func(document string) error {
			_, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", document))
			return err
		}, alloctest.UnlessRaced(true, false), 50},
	}
	for _, file := range files {
		places := valuePlaces(file.schema)
		keys, entries, values := 0, 0, 0
		for at, place := range places {
			switch place[len(place)-1] {
			case "[]":
				entries++
			case "{}":
				values++
			default:
				keys++
			}
			for i, value := range spellings {
				if !file.every && i != at%len(spellings) {
					continue
				}
				document := place.written(value)
				want := fmt.Sprintf("line %d: %s", strings.Count(document, "\n"), place.said(value))
				if err := file.load(document); err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("%s, %s written with%s: the exporter says %v\nwant %s\n%s", file.name, strings.Join(place, "."), map[bool]string{true: " nothing", false: value}[value == ""], err, want, document)
				}
				found := place.found() + ": <nil> is not "
				problems := schemaProblems(t, file.schema, document)
				if !containsPrefixed(problems, found) {
					t.Errorf("%s, %s written with%s: the schema says %v\nwant a problem that begins %q\n%s", file.name, strings.Join(place, "."), map[bool]string{true: " nothing", false: value}[value == ""], problems, found, document)
				}
			}
			// Written with a value, whatever else is wrong with a
			// document of one key, the place is not told of as empty.
			document := place.written(" [x]")
			if err := file.load(document); err != nil && strings.Contains(err.Error(), "which YAML reads as no value at all") {
				t.Errorf("%s, %s written with a value: the exporter says %v\n%s", file.name, strings.Join(place, "."), err, document)
			}
			if problems := schemaProblems(t, file.schema, document); containsPrefixed(problems, place.found()+": <nil> is not ") {
				t.Errorf("%s, %s written with a value: the schema says %v\n%s", file.name, strings.Join(place, "."), problems, document)
			}
		}
		if len(places) < file.least || entries < 4 || values < 2 {
			t.Errorf("%s: only %d places were found, %d of them entries of lists and %d values of mappings", file.name, len(places), entries, values)
		}
		t.Logf("%s: %d keys, %d lists and %d mappings", file.name, keys, entries, values)
		// The file's own keys take what they like.
		for _, value := range spellings {
			document := "x-own:" + value + "\n"
			if err := file.load(document); err != nil && strings.Contains(err.Error(), "which YAML reads as no value at all") {
				t.Errorf("%s, an x- key written with%s: the exporter says %v", file.name, value, err)
			}
			if problems := schemaProblems(t, file.schema, document); containsPrefixed(problems, ".x-own") {
				t.Errorf("%s, an x- key written with%s: the schema says %v", file.name, value, problems)
			}
		}
	}
}

// containsPrefixed reports whether one of the problems begins with prefix.
func containsPrefixed(problems []string, prefix string) bool {
	for _, problem := range problems {
		if strings.HasPrefix(problem, prefix) {
			return true
		}
	}
	return false
}
