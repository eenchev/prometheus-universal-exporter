package config

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// checkValuesAsItWas is checkValues before a key that is an alias was read
// as the key it names and a number past a float64's range as a number, kept
// as the oracle of the documents that have neither.
func checkValuesAsItWas(document []byte, decoded reflect.Value) (problems []string, readAgain map[string]bool) {
	var doc yaml.Node
	if yaml.Unmarshal(document, &doc) != nil || len(doc.Content) == 0 {
		return nil, nil
	}
	readAgain = map[string]bool{}
	visiting := map[*yaml.Node]bool{}
	// at is the place for a message, kinds what its schema says the place
	// takes, and out the value decoded there, which is not valid where
	// nothing was.
	var walk func(n *yaml.Node, t reflect.Type, out reflect.Value, at writtenAt, kinds *valueKinds)
	walk = func(n *yaml.Node, t reflect.Type, out reflect.Value, at writtenAt, kinds *valueKinds) {
		key := at.key
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
			if out.IsValid() {
				out = out.Elem()
			}
		}
		switch t.Kind() {
		case reflect.Struct:
			if n.Kind != yaml.MappingNode {
				return
			}
			fields := map[string]int{}
			for i := 0; i < t.NumField(); i++ {
				name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
				if name != "" && name != "-" {
					fields[name] = i
				}
			}
			for _, entry := range model.DecodedEntries(n) {
				field, ok := fields[entry.Key.Value]
				switch {
				case !ok:
				case noValue(entry.Value):
					problems = append(problems, fmt.Sprintf("line %d: %s %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, entry.Key.Value, noValueAfterColon(entry.Value)))
				default:
					var value reflect.Value
					if out.IsValid() {
						value = out.Field(field)
					}
					walk(entry.Value, t.Field(field).Type, value, writtenAt{key: entry.Key.Value}, kinds.key(entry.Key.Value))
				}
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for i, item := range n.Content {
					if entry := resolveAlias(item); noValue(entry) {
						how := "is written " + entry.Value
						if entry.Value == "" {
							how = "is a dash with nothing after it"
						}
						problems = append(problems, fmt.Sprintf("line %d: %s entry %d %s, which YAML reads as no value at all, so the entry would be left out without a word; write %s there, or take the entry out", item.Line, key, i+1, how, yamlEntry(t.Elem())))
						continue
					}
					var value reflect.Value
					// The decoder leaves out of the list what it takes for
					// no value, which is refused above: past one, the
					// entries are not where the document has them.
					if out.IsValid() && out.Len() == len(n.Content) {
						value = out.Index(i)
					}
					walk(item, t.Elem(), value, writtenAt{key: key, entry: i + 1}, kinds.entry())
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
				if noValue(entry.Value) {
					problems = append(problems, fmt.Sprintf("line %d: %s key %q %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, key, entry.Key.Value, noValueAfterColon(entry.Value)))
					continue
				}
				// A value of a mapping is not one to write into: no
				// mapping of the files has whole numbers or statuses.
				walk(entry.Value, t.Elem(), reflect.Value{}, writtenAt{key: key, mapKey: entry.Key.Value, inMap: true}, kinds.value())
			}
		case reflect.String:
			if n.Kind == yaml.ScalarNode && kinds != nil && kinds.number && out.CanSet() {
				if text, ok := wholeNumberText(n, kinds.least, kinds.most); ok {
					out.SetString(text)
				}
				return
			}
			if n.Kind != yaml.ScalarNode || kinds == nil || !kinds.textOnly {
				return
			}
			var reads string
			switch n.ShortTag() {
			case "!!bool":
				reads = "a boolean"
			case "!!int", "!!float":
				reads = "a number"
			default:
				return
			}
			// Quoted, a number or a boolean is no value of a key that
			// takes one of a few words, so that key is told the words.
			advice := fmt.Sprintf("to use that text there, quote it: %q", n.Value)
			if len(kinds.allowed) > 0 {
				advice = "write one of " + strings.Join(kinds.allowed, ", ")
			}
			problems = append(problems, fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; %s", n.Line, at, n.Value, reads, advice))
		case reflect.Bool:
			if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
				return
			}
			if meant, word := yamlBooleanWords[n.Value]; word {
				problems = append(problems, fmt.Sprintf("line %d: %s is written %s, which YAML reads as text, not as a boolean; write %t", n.Line, at, n.Value, meant))
			}
		case reflect.Int, reflect.Int64:
			// A duration and a size read themselves, and say what they take.
			if t == durationType || t == byteSizeType || n.Kind != yaml.ScalarNode || n.ShortTag() != "!!float" {
				return
			}
			whole, isWhole := model.ReadWholeNumber(n.Value)
			if !isWhole {
				// A fraction, however small, is refused as it was; what is
				// no number at all, infinity and not-a-number, is the
				// decoder's to refuse.
				if _, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
					readAgain[decoderRefusal(n, t)] = true
					problems = append(problems, fmt.Sprintf("line %d: %s is %s, which is not a whole number", n.Line, key, n.Value))
				}
				return
			}
			readAgain[decoderRefusal(n, t)] = true
			value, held := whole.Int(t.Bits())
			if !held {
				least := int64(-1) << (t.Bits() - 1)
				problems = append(problems, fmt.Sprintf("line %d: %s is %s, which is past the whole numbers it holds, %d to %d; write a whole number in that range", n.Line, key, n.Value, least, -(least+1)))
				return
			}
			if out.CanSet() {
				out.SetInt(value)
			}
		}
	}
	walk(doc.Content[0], decoded.Type(), decoded, writtenAt{key: "the document"}, schemaKinds(decoded.Type()))
	return problems, readAgain
}

// Over the example configurations and generated documents of each kind of
// file — whole numbers written every way at keys of one and at
// request.accept_status, fractions, text, booleans, nulls, keys of a mapping
// of the file's, merges of one mapping and of a list, mappings and values
// that are aliases, in the configuration, a collector file and the static
// target file — that have no key that is an alias and no number past a
// float64's range, checkValues gives the problems, the decoder's messages it
// says instead, and the values it writes, that it gave.
func TestTheWalkOfADocumentWithoutAnAliasKeyIsAsItWas(t *testing.T) {
	var documents [][]byte
	for _, pattern := range []string{"../../examples/*.yaml", "../../examples/*/*.yaml", "../../configs/*.yaml", "testdata/*.yaml"} {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			document, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			documents = append(documents, document)
		}
	}
	if len(documents) < 10 {
		t.Fatalf("only %d example configurations", len(documents))
	}
	random := rand.New(rand.NewPCG(117, 1))
	numbers := []string{"7", "0", "-3", "1e3", "7.0", "9007199254740993.0", "1.5", "-0.5", "1e19", "0x1F7", "5.03e2", "+503", "0503", "'503'", "2xx", "true", "yes", "~", "", "x", ".inf", "1_000.0", "!!float 7", "!!str 7", "'1e400'"}
	pick := func() string { return numbers[random.IntN(len(numbers))] }
	limits := func() string {
		var parts []string
		for _, key := range []string{"max_metrics", "max_labels_per_metric", "max_help_length", "max_cache_entries"} {
			if random.IntN(2) == 0 {
				parts = append(parts, key+": "+pick())
			}
		}
		switch random.IntN(4) {
		case 0:
			parts = append(parts, "<<: *d")
		case 1:
			parts = append(parts, "<<: [*d, *e]")
		case 2:
			parts = append(parts, "<<: {max_metrics: "+pick()+"}")
		}
		random.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
		return "{" + strings.Join(parts, ", ") + "}"
	}
	collector := func(name string) string {
		limit := limits()
		if random.IntN(5) == 0 {
			limit = "*d"
		}
		return "  - name: " + name + "\n    max_concurrent_probes: " + pick() + "\n    request: {type: http, accept_status: [" + pick() + ", *s, " + pick() + "], retry: {attempts: " + pick() + "}}\n" +
			"    decoder: {type: json}\n    transform: {type: jq, labels: {zone: " + pick() + ", " + pick() + ": x}}\n    limits: " + limit + "\n    metrics: [{name: m, expression: .x}]\n"
	}
	for range alloctest.UnlessRaced(300, 30) {
		anchors := "x-s: &s " + pick() + "\nx-d: &d " + strings.ReplaceAll(strings.ReplaceAll(limits(), "*d", "{}"), "*e", "{}") + "\nx-e: &e {max_metrics: " + pick() + ", max_help_length: " + pick() + "}\n"
		documents = append(documents,
			[]byte(anchors+"otlp: {enabled: false, max_pending_points: "+pick()+"}\ncollectors:\n"+collector("a")+collector("b")),
			[]byte(anchors+"interval: 1m\nconcurrency: "+pick()+"\ntargets:\n  - {name: t, collector: a, target: 'http://x', request: {accept_status: ["+pick()+", *s], retry: {attempts: "+pick()+"}}}\n"))
	}
	compared := 0
	for _, document := range documents {
		for _, into := range []reflect.Type{reflect.TypeOf(model.Config{}), reflect.TypeOf(collectorFile{}), reflect.TypeOf(model.StaticTargetFile{})} {
			now, was := reflect.New(into), reflect.New(into)
			// What the decoder makes of the document, which the walk writes
			// into, in each of two copies.
			_ = yaml.Unmarshal(document, now.Interface())
			_ = yaml.Unmarshal(document, was.Interface())
			problems, readAgain := checkValues(document, now.Elem())
			wasProblems, wasReadAgain := checkValuesAsItWas(document, was.Elem())
			if !slices.Equal(problems, wasProblems) || !reflect.DeepEqual(readAgain, wasReadAgain) || !reflect.DeepEqual(now.Interface(), was.Interface()) {
				t.Fatalf("%s into %s:\nproblems %q, were %q\nread again %v, was %v\nwrote %+v, wrote %+v", document, into, problems, wasProblems, readAgain, wasReadAgain, now.Elem(), was.Elem())
			}
			compared++
		}
	}
	t.Logf("%d documents, each into three types", compared/3)
}
