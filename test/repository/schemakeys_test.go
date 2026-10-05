package repository

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// strictProblems checks what the validator of schema_test.go lets through
// and a validator of JSON Schema does not: maxLength, the length of a string
// in characters, so a character of several bytes is one; and a key left
// empty, which is null and none of the types a key of the schema takes.
func strictProblems(schema map[string]any, value any, path string) []string {
	var problems []string
	switch x := value.(type) {
	case nil:
		if types, typed := schema["type"]; typed {
			problems = append(problems, fmt.Sprintf("%s: null is not %v", path, types))
		}
	case string:
		if most, ok := schema["maxLength"].(float64); ok && float64(utf8.RuneCountInString(x)) > most {
			problems = append(problems, fmt.Sprintf("%s: %q is longer than %v", path, x, most))
		}
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		for key, item := range x {
			if sub, ok := properties[key].(map[string]any); ok {
				problems = append(problems, strictProblems(sub, item, path+"."+key)...)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range x {
				problems = append(problems, strictProblems(items, item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	return problems
}

// schemaProblems are the problems a schema finds in a document.
func schemaProblems(t *testing.T, schema map[string]any, document string) []string {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, document)
	}
	value := normalizeYAML(doc)
	return append(validateAgainstSchema(schema, value), strictProblems(schema, value, "")...)
}

// The validator takes maximum as it takes minimum: a number above it is
// refused, one at it or below it taken, and text is not a number's to
// bound. A duration's schema is made of both: the number 0 and no other.
func TestTheSchemaValidatorChecksMaximum(t *testing.T) {
	schema := map[string]any{"type": []any{"string", "integer"}, "minimum": float64(0), "maximum": float64(0), "pattern": "^[0-9]+s$"}
	for _, taken := range []any{float64(0), "30s", "0s"} {
		if errs := validateAgainstSchema(schema, taken); len(errs) != 0 {
			t.Errorf("%v is refused: %v", taken, errs)
		}
	}
	for _, refused := range []any{float64(1), float64(30), float64(-1), 1.5, "30", true} {
		if errs := validateAgainstSchema(schema, refused); len(errs) == 0 {
			t.Errorf("%v is taken", refused)
		}
	}
	if errs := validateAgainstSchema(map[string]any{"maximum": float64(10)}, float64(11)); len(errs) != 1 || !strings.Contains(errs[0], "11 is above 10") {
		t.Errorf("11 under a maximum of 10: %v", errs)
	}
	if errs := validateAgainstSchema(map[string]any{"maximum": float64(10)}, float64(10)); len(errs) != 0 {
		t.Errorf("10 under a maximum of 10: %v", errs)
	}
}

// A key of one of the files that have a schema, for the tables that hold the
// schemas and the exporter to one verdict on a key left out, written "",
// written well and written badly (checkSchemaKeys). The tables are the list
// of the keys a schema holds to allowed values, a pattern or a length, each
// type's in the file with that type's build constraint, and a test of the
// build with every type fails when a schema has such a key that no table
// has (TestEveryConstrainedKeyOfTheSchemasIsInATable).

// keyFile is the file a key is written in.
type keyFile int

const (
	// inConfiguration is the configuration, and a collector file too when
	// the document holds collectors alone.
	inConfiguration keyFile = iota
	// inTargetFile is the static target file, loaded beside the row's
	// configuration.
	inTargetFile
)

// was is what the schemas said of a case before "" came to be the key left
// out to them (schemaAsItWas): what they say now, or the other verdict.
type was int

const (
	asNow was = iota
	taken
	refused
)

// said is the verdict was stands for, beside the one of now.
func (w was) said(now bool) bool {
	switch w {
	case taken:
		return true
	case refused:
		return false
	}
	return now
}

type schemaKey struct {
	// key is the key as the schema's path names it, with [] for an entry
	// of a list and {} for a key of a mapping; of tells apart the rows of
	// one key.
	key, of string
	file    keyFile
	// document is the file with the place of the key, at, which setting
	// takes with the key's value for its %s. A row of the target file
	// names the configuration its collectors are in.
	document, at, setting string
	configuration         string
	// valid and invalid are a value both take and one both refuse, where
	// the key has such a value: free text has no invalid one.
	valid, invalid string
	// absent and empty say whether both take the file without the key,
	// and with the key written "". A key taken both ways loads as the
	// same thing both ways: "" is the key left out.
	absent, empty                   bool
	absentWas, emptyWas, invalidWas was
	// duration marks a duration key, which is put through the ways a
	// duration is written too; zero is what the exporter refuses a zero
	// with in a key that needs more, and empty where zero is taken.
	duration bool
	zero     string
}

// name is the key with what tells its row apart.
func (k schemaKey) name() string {
	if k.of == "" {
		return k.key
	}
	return k.key + ", " + k.of
}

// written is the row's file with the key written as value, and without the
// key for a value of nothing.
func (k schemaKey) written(t *testing.T, value string) string {
	t.Helper()
	if strings.Count(k.document, k.at) != 1 {
		t.Fatalf("%s: the document has %q %d times", k.name(), k.at, strings.Count(k.document, k.at))
	}
	if value == "" {
		return strings.Replace(k.document, k.at, "", 1)
	}
	return strings.Replace(k.document, k.at, fmt.Sprintf(k.setting, value), 1)
}

// keySchemas are the committed schemas, and each as it was.
type keySchemas struct {
	configuration, collectorFile, targetFile          map[string]any
	configurationWas, collectorFileWas, targetFileWas map[string]any
}

func loadKeySchemas(t *testing.T) keySchemas {
	t.Helper()
	return keySchemas{
		configuration: loadSchema(t), collectorFile: loadSchemaFile(t, collectorFileSchemaFile), targetFile: loadSchemaFile(t, staticTargetsSchemaFile),
		configurationWas: schemaAsItWas(t, configSchemaFile), collectorFileWas: schemaAsItWas(t, collectorFileSchemaFile), targetFileWas: schemaAsItWas(t, staticTargetsSchemaFile),
	}
}

// schemaNode is the part of a schema that describes the key at path, written
// as a row's key is.
func schemaNode(t *testing.T, schema map[string]any, path string) map[string]any {
	t.Helper()
	node := schema
	for _, part := range strings.Split(path, ".") {
		properties, _ := node["properties"].(map[string]any)
		next, ok := properties[strings.TrimRight(part, "[]{}")].(map[string]any)
		switch {
		case !ok:
			t.Fatalf("the schema has no %s", path)
		case strings.HasSuffix(part, "[]"):
			next, ok = next["items"].(map[string]any)
		case strings.HasSuffix(part, "{}"):
			next, ok = next["propertyNames"].(map[string]any)
		}
		if !ok {
			t.Fatalf("the schema has no %s", path)
		}
		node = next
	}
	return node
}

// schemaAsItWas is a committed schema with what "" being the key left out,
// the number 0 being a duration, the rules of value_map keys and the rule
// that a label's expression is not blanks alone added to it taken out
// again: the schema before, kept as an oracle, so that the tables show what
// each change changed and that the schemas say of every other case what
// they said. The values of an optional key lose their "", its pattern its
// empty alternative and a duration its number; the rules made of a key
// being written are the rules of a key being there; and the rules that were
// not there are gone.
func schemaAsItWas(t *testing.T, file string) map[string]any {
	t.Helper()
	schema := loadSchemaFile(t, file)
	var walk func(node any)
	walk = func(node any) {
		switch x := node.(type) {
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			if enum, ok := x["enum"].([]any); ok {
				x["enum"] = slices.DeleteFunc(slices.Clone(enum), func(value any) bool { return value == "" })
			}
			if pattern, ok := x["pattern"].(string); ok {
				x["pattern"] = strings.TrimPrefix(pattern, "^$|")
			}
			if x["minimum"] == float64(0) && x["maximum"] == float64(0) {
				x["type"] = "string"
				delete(x, "minimum")
				delete(x, "maximum")
				if description, _ := x["description"].(string); strings.HasPrefix(description, durationDescriptionWas) {
					x["description"] = durationDescriptionWas
				}
			}
			for _, sub := range x {
				walk(sub)
			}
		}
	}
	walk(schema)
	if file == staticTargetsSchemaFile {
		delete(schemaNode(t, schema, "targets[].collector"), "minLength")
		delete(schemaNode(t, schema, "targets[].labels{}"), "minLength")
		return schema
	}
	collector := schemaNode(t, schema, "collectors[]")
	delete(collector, "if")
	delete(collector, "then")
	label := schemaNode(t, schema, "collectors[].metrics[].labels[]")
	delete(label, "if")
	delete(label, "then")
	label["oneOf"] = []any{map[string]any{"required": []any{"value"}}, map[string]any{"required": []any{"expression"}}}
	for _, key := range []string{"value", "expression"} {
		schemaNode(t, schema, "collectors[].metrics[].labels[]."+key)["minLength"] = float64(1)
	}
	delete(schemaNode(t, schema, "collectors[].metrics[].labels[].expression"), "not")
	for _, key := range []string{"collectors[].metrics[].value_map", "collectors[].metrics[].labels[].value_map"} {
		delete(schemaNode(t, schema, key), "propertyNames")
	}
	for _, key := range []string{"collectors[].request.allowed_targets[]", "collectors[].request.denied_targets[]"} {
		delete(schemaNode(t, schema, key), "minLength")
	}
	for _, condition := range schemaNode(t, schema, "collectors[].request")["allOf"].([]any) {
		condition := condition.(map[string]any)
		condition["then"] = map[string]any{"required": condition["then"].(map[string]any)["required"]}
	}
	return schema
}

// durationDescriptionWas is how a duration was described before it said
// that zero may be written 0.
const durationDescriptionWas = "A duration such as 500ms, 30s or 5m."

// checkedFile is one of the files a row's document is: check gives what the
// schema finds wrong with a document, what the schema as it was found, what
// the exporter loaded and the error of loading it.
type checkedFile struct {
	name  string
	check func(t *testing.T, document string) (now, before []string, loaded any, err error)
}

// files are the files a row's document is checked as.
func (k schemaKey) files(t *testing.T, schemas keySchemas) []checkedFile {
	t.Helper()
	if k.file == inConfiguration {
		files := []checkedFile{{"the configuration", func(t *testing.T, document string) ([]string, []string, any, error) {
			t.Helper()
			cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
			var loaded any
			if err == nil {
				loaded = []any{cfg.Collectors, cfg.OTLP, cfg.Web}
			}
			return schemaProblems(t, schemas.configuration, document), schemaProblems(t, schemas.configurationWas, document), loaded, err
		}}}
		if strings.HasPrefix(k.document, "collectors:\n") && strings.HasPrefix(k.key, "collectors[]") {
			// A collector file is loaded as the configuration listing it is.
			files = append(files, checkedFile{"a collector file", func(t *testing.T, document string) ([]string, []string, any, error) {
				t.Helper()
				dir := t.TempDir()
				testutil.WriteIn(t, dir, "collectors.yaml", document)
				cfg, err := config.Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n"))
				var loaded any
				if err == nil {
					loaded = cfg.Collectors
				}
				return schemaProblems(t, schemas.collectorFile, document), schemaProblems(t, schemas.collectorFileWas, document), loaded, err
			}})
		}
		return files
	}
	if k.file != inTargetFile {
		t.Fatalf("%s: the row is of no file that has a schema", k.name())
	}
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", k.configuration))
	if err != nil {
		t.Fatalf("%s: its configuration: %v", k.name(), err)
	}
	return []checkedFile{{"the target file", func(t *testing.T, document string) ([]string, []string, any, error) {
		t.Helper()
		file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
		if err == nil {
			err = config.ValidateStaticTargets(file)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(file, cfg)
		}
		return schemaProblems(t, schemas.targetFile, document), schemaProblems(t, schemas.targetFileWas, document), file, err
	}}}
}

// durationForms are the ways a duration is written that are not text to
// YAML, and the text beside them: whether the schemas and the exporter take
// each in a key that takes zero, and whether the schemas took it when a
// duration was text alone.
var durationForms = []struct {
	written    string
	zero       bool
	taken, was bool
}{
	{`0`, true, true, false}, {`"0"`, true, true, true}, {`0s`, true, true, true}, {`"0s"`, true, true, true},
	{`-0`, true, true, false}, {`+0`, true, true, false},
	{`"30s"`, false, true, true}, {`30s`, false, true, true},
	{`30`, false, false, false}, {`1.5`, false, false, false}, {`-5`, false, false, false}, {`true`, false, false, false},
}

// zerosOfAnotherSpelling are numbers YAML reads as 0 that are not written
// 0: the schemas, which are handed the number, take each for the zero it
// is, and the exporter, which reads how it is written, refuses each as no
// duration. With a duration too long to be held, this is what the schemas
// and the exporter differ on in a duration, and it is documented with the
// schemas (docs/CONFIGURATION.md, Editor support).
var zerosOfAnotherSpelling = []string{`0.0`, `00`, `0x0`, `0e0`, `-0.0`, `0_0`}

// checkSchemaKeys puts each row's file through the committed schema and the
// exporter four ways — without the key, with the key written "", with a
// value both take and with one both refuse — and fails unless both give the
// verdict the row says; a key taken both left out and written "" must load
// as the same thing both ways. The schema as it was (schemaAsItWas) must
// say what the row says it said: the same, but for the cases a change is
// about. A duration key goes through the ways a duration is written too.
// Each row is logged as a line of the table the documentation of the
// change gives: the key, what the exporter does with "", and what the
// schema did and does.
func checkSchemaKeys(t *testing.T, keys []schemaKey) {
	t.Helper()
	schemas := loadKeySchemas(t)
	verdict := func(accepted bool) string {
		if accepted {
			return "takes it"
		}
		return "refuses it"
	}
	for _, key := range keys {
		for i, file := range key.files(t, schemas) {
			name := file.name + "'s " + key.name()
			expect := func(what, written string, accepted, before bool) (any, error) {
				t.Helper()
				document := key.written(t, written)
				now, old, loaded, err := file.check(t, document)
				if (len(now) == 0) != accepted || (err == nil) != accepted {
					t.Errorf("%s, %s: want accepted %v by both; the schema says %v, the exporter %v\n%s", name, what, accepted, now, err, document)
				}
				if (len(old) == 0) != before {
					t.Errorf("%s, %s: the schema as it was should have accepted it: %v; it says %v\n%s", name, what, before, old, document)
				}
				return loaded, err
			}
			without, _ := expect("left out", "", key.absent, key.absentWas.said(key.absent))
			with, err := expect(`written ""`, `""`, key.empty, key.emptyWas.said(key.empty))
			if key.absent && key.empty && !reflect.DeepEqual(with, without) {
				t.Errorf("%s: written \"\" it loads as %+v, and left out as %+v", name, with, without)
			}
			if key.valid != "" {
				expect("as "+key.valid, key.valid, true, true)
			}
			if key.invalid != "" {
				expect("as "+key.invalid, key.invalid, false, key.invalidWas.said(false))
			}
			if i == 0 {
				exporter := `takes it as the key left out`
				if err != nil {
					text := err.Error()
					exporter = "refuses it: " + text[strings.LastIndex(text, ".yaml: ")+1:]
				}
				t.Logf("| %s | %s | %s | %s |", key.name(), exporter, verdict(key.emptyWas.said(key.empty)), verdict(key.empty))
			}
			if !key.duration {
				continue
			}
			for _, form := range durationForms {
				document := key.written(t, form.written)
				now, old, _, err := file.check(t, document)
				switch {
				case form.zero && key.zero != "":
					// A zero where more is needed is the exporter's to
					// refuse: how long a duration is, a schema cannot tell.
					if len(now) != 0 || err == nil || !strings.Contains(err.Error(), key.zero) {
						t.Errorf("%s: %s: want it past the schema and refused by the exporter with %q; the schema says %v, the exporter %v", name, form.written, key.zero, now, err)
					}
				case (len(now) == 0) != form.taken || (err == nil) != form.taken:
					t.Errorf("%s: %s: want accepted %v by both; the schema says %v, the exporter %v", name, form.written, form.taken, now, err)
				}
				if (len(old) == 0) != form.was {
					t.Errorf("%s: %s: the schema as it was should have accepted it: %v; it says %v", name, form.written, form.was, old)
				}
			}
			for _, written := range zerosOfAnotherSpelling {
				now, old, _, err := file.check(t, key.written(t, written))
				if len(now) != 0 || len(old) == 0 || err == nil || !strings.Contains(err.Error(), "is not a duration") {
					t.Errorf("%s: %s: want it past the schema, refused by the schema as it was, and refused by the exporter as no duration; the schema says %v, said %v, the exporter %v", name, written, now, old, err)
				}
			}
		}
	}
}

// Every shipped configuration and target file, the examples among them,
// matches its schema as it matched the schema as it was: nothing shipped is
// flagged by a rule the schemas gained with "" being the key left out — a
// rule's name, a grpc collector's descriptors, a value_map's keys.
func TestTheShippedFilesMatchTheSchemasAsTheyDid(t *testing.T) {
	schemas := loadKeySchemas(t)
	examples := shippedExamples(t)
	files := 0
	for _, kind := range []struct {
		files       []string
		now, before map[string]any
	}{
		{append([]string{"configs/config.example.yaml", "configs/config.otlp.example.yaml"}, examples.configs...), schemas.configuration, schemas.configurationWas},
		{append([]string{"configs/static-targets.example.yaml"}, examples.staticTargets...), schemas.targetFile, schemas.targetFileWas},
	} {
		for _, file := range kind.files {
			files++
			document := readYAMLDocument(t, file)
			if now, before := validateAgainstSchema(kind.now, document), validateAgainstSchema(kind.before, document); len(now) != 0 || len(before) != 0 {
				t.Errorf("%s: the schema says %v, and as it was %v", file, now, before)
			}
		}
	}
	if files < 10 {
		t.Fatalf("only %d files were found", files)
	}
}
