package repository

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// node is the part of a schema that describes the place.
func (p valuePlace) node(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	node := schema
	for _, part := range p {
		var next map[string]any
		switch part {
		case "[]":
			next, _ = node["items"].(map[string]any)
		case "{}":
			next, _ = node["additionalProperties"].(map[string]any)
		default:
			properties, _ := node["properties"].(map[string]any)
			next, _ = properties[part].(map[string]any)
		}
		if next == nil {
			t.Fatalf("the schema has no %s", strings.Join(p, "."))
		}
		node = next
	}
	return node
}

// named is the place as the exporter names it in a message about the value
// written there: the key, the list and the entry's place in it, or the
// mapping and the key.
func (p valuePlace) named() string {
	last, key := p[len(p)-1], ""
	if len(p) > 1 {
		key = p[len(p)-2]
	}
	switch last {
	case "[]":
		return key + " entry 1"
	case "{}":
		return key + ` key "k"`
	}
	return last
}

// builtSchema is the schema this binary generates for one of the three
// files, which is the committed one in a build with every request type and
// lists the types it has in any other.
func builtSchema(t *testing.T, file string) map[string]any {
	t.Helper()
	render := map[string]func() ([]byte, error){configSchemaFile: config.SchemaJSON, collectorFileSchemaFile: config.CollectorFileSchemaJSON, staticTargetsSchemaFile: config.StaticTargetsSchemaJSON}[file]
	raw, err := render()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// notTextAdvice is what the exporter says to do about a boolean or a number
// written where the schema's node takes text alone: to quote it, or, of a
// key held to allowed values, which they are. The node is of the schema
// this binary generates (builtSchema), whose request types are the ones it
// was built with.
func notTextAdvice(node map[string]any, written string) string {
	if allowed := slices.DeleteFunc(toStrings(node["enum"]), func(value string) bool { return value == "" }); len(allowed) > 0 {
		return "write one of " + strings.Join(allowed, ", ")
	}
	return "to use that text there, quote it: " + strconv.Quote(written)
}

// refusedAtLine reports whether the exporter refused the document while
// reading it, for what stands on the line: an error of the decoding names
// the line of each thing it refuses, and the checks of the configuration,
// which come after and name no line, are not reached then. What a block that
// enabled switches on says of a key beside a missing enabled is of the
// block, not of the value written.
func refusedAtLine(err error, line int) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	if _, after, found := strings.Cut(text, ".yaml: "); found && strings.HasPrefix(text, "collector file ") {
		text = after
	}
	if !strings.HasPrefix(text, "line ") {
		return false
	}
	for _, problem := range strings.Split(strings.TrimPrefix(text, "line "), "; line ") {
		if said, found := strings.CutPrefix(problem, strconv.Itoa(line)+": "); found && !strings.Contains(said, "but not enabled") {
			return true
		}
	}
	return false
}

// A value is written as the kind its key takes, and the schemas and the
// exporter say the same of one that is not, at every place of the three
// files where a single value is written: a key, an entry of a list, a value
// of a mapping. The schemas say what each takes. Free text — a description,
// an expression, a header, a label's value — takes a boolean and a number
// too, read as the text they spell; a key held to text alone, which is one
// with allowed values or a pattern, takes neither, and the exporter, which
// took name: true for the name "true", refuses it at its line, saying how
// YAML reads it and to quote it, or, of a key the schema holds to values,
// which they are; a boolean is true or false, and the words
// YAML 1.1 read as one, which are text to an editor and which the exporter
// read as booleans, are refused by both; and a number or a boolean in
// quotes is text, which no key of numbers or booleans takes.
//
// The places are read from the committed schemas (valuePlaces), so a key a
// file gains is held to it here without a row of its own, and which keys
// are held to text alone is read there too, as the exporter reads it. Each
// place is written alone in its document, which is then refused for other
// reasons where it is not refused for this one: what is compared is whether
// the schema finds a problem at the place and whether the exporter refuses
// what is on its line while reading the file. A number is one the key's
// least and most allow, so that it is the kind that is tried and not the
// value. One place takes text beside a number and reads both as text, an
// entry of accept_status: a boolean there is refused by the schema for its
// kind and by the exporter once the file is read, as no status.
//
// A collector file's collectors are the configuration's, so its places are
// tried in one way each, and under the race detector, where a load costs
// several times more, so are the other two files': every way still at a
// place in five of each file.
func TestSchemaAndExporterAgreeOnTheKindOfValueEveryPlaceTakes(t *testing.T) {
	type file struct {
		name          string
		schema, built map[string]any
		load          func(document string) error
		every         bool
	}
	dir := t.TempDir()
	collectorFiles := testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")
	files := []file{
		{"the configuration", loadSchema(t), builtSchema(t, configSchemaFile), func(document string) error {
			_, err := config.Load(testutil.WriteIn(t, dir, "alone.yaml", document))
			return err
		}, alloctest.UnlessRaced(true, false)},
		{"a collector file", loadSchemaFile(t, collectorFileSchemaFile), builtSchema(t, collectorFileSchemaFile), func(document string) error {
			testutil.WriteIn(t, dir, "collectors.yaml", document)
			_, err := config.Load(collectorFiles)
			return err
		}, false},
		{"the target file", loadSchemaFile(t, staticTargetsSchemaFile), builtSchema(t, staticTargetsSchemaFile), func(document string) error {
			_, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", document))
			return err
		}, alloctest.UnlessRaced(true, false)},
	}
	type way struct {
		written string
		// refused says whether a place of these types refuses it, and
		// notText how YAML reads it where the exporter says so.
		refused bool
		notText string
	}
	textOnly, words, free, booleans, numbers := 0, 0, 0, 0, 0
	for _, file := range files {
		tried := 0
		for at, place := range valuePlaces(file.schema) {
			node := place.node(t, file.schema)
			types := toStrings(node["type"])
			if len(types) == 0 || slices.Contains(types, "object") || slices.Contains(types, "array") {
				continue
			}
			takes := func(kind string) bool { return slices.Contains(types, kind) }
			text, number, boolean := takes("string"), takes("number") || takes("integer"), takes("boolean")
			// A number within what the key holds one to.
			among := float64(77)
			if least, ok := node["minimum"].(float64); ok && among < least {
				among = least
			}
			if most, ok := node["maximum"].(float64); ok && among > most {
				among = most
			}
			ways := []way{
				{"True", !boolean, "a boolean"}, {"false", !boolean, "a boolean"},
				{strconv.FormatFloat(among, 'f', -1, 64), !number, "a number"},
			}
			switch {
			case text && !number && !boolean:
				textOnly++
				ways = append(ways, way{"1.5e3", true, "a number"}, way{"0x1F", true, "a number"})
			case text:
				free++
			case boolean:
				booleans++
				ways = append(ways, way{"Yes", true, ""}, way{"off", true, ""}, way{`"y"`, true, ""}, way{`"true"`, true, ""})
			default:
				numbers++
				ways = append(ways, way{`"77"`, true, ""}, way{`'1'`, true, ""})
			}
			for i, way := range ways {
				if !file.every && i != at%len(ways) {
					continue
				}
				tried++
				document := place.written(" " + way.written)
				line := strings.Count(document, "\n")
				what := fmt.Sprintf("%s, %s written %s", file.name, strings.Join(place, "."), way.written)
				err := file.load(document)
				// An entry of accept_status is a status or a class of
				// them, a number or text, and the exporter reads whatever
				// is written as text and refuses what is neither after the
				// file is read, as the tables of the keys show of true.
				afterReading := len(place) > 1 && place[len(place)-2] == "accept_status" && way.notText == "a boolean"
				if refusedAtLine(err, line) != way.refused && !afterReading {
					t.Errorf("%s: want it refused by the exporter at its line: %v; the exporter says %v\n%s", what, way.refused, err, document)
				}
				problems := schemaProblems(t, file.schema, document)
				if containsPrefixed(problems, place.found()+": ") != way.refused {
					t.Errorf("%s: want it refused by the schema: %v; the schema says %v\n%s", what, way.refused, problems, document)
				}
				// Where text alone is taken, the exporter says how YAML
				// reads what is written and how to write the text, or,
				// where the schema holds the key to values, which they
				// are; where a boolean is, which of the two the word was
				// read as.
				var said string
				switch {
				case text && !number && !boolean && way.refused:
					advice := notTextAdvice(place.node(t, file.built), way.written)
					said = fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; %s", line, place.named(), way.written, way.notText, advice)
					if strings.HasPrefix(advice, "write one of ") {
						words++
					}
				case boolean && !text && way.written != `"true"` && way.notText == "" && way.refused:
					word := strings.Trim(way.written, `"`)
					said = fmt.Sprintf("line %d: %s is written %s, which YAML reads as text, not as a boolean; write %t", line, place.named(), word, word != "off")
				}
				if said != "" && (err == nil || !strings.Contains(err.Error(), said)) {
					t.Errorf("%s: the exporter says %v\nwant %s\n%s", what, err, said, document)
				}
				if said == "" && err != nil && (strings.Contains(err.Error(), "not as text") || strings.Contains(err.Error(), "not as a boolean")) {
					t.Errorf("%s: the exporter says %v, of a value its key takes or refuses otherwise\n%s", what, err, document)
				}
			}
		}
		t.Logf("%s: %d values written", file.name, tried)
	}
	if textOnly < 50 || words < 30 || free < 120 || booleans < 35 || numbers < 20 {
		t.Errorf("of the places tried, %d take text alone, %d times one of a few words, %d text and what spells it, %d a boolean and %d a number", textOnly, words, free, booleans, numbers)
	}
	t.Logf("%d places take text alone, %d text and what spells it, %d a boolean and %d a number or a duration", textOnly, free, booleans, numbers)
}
