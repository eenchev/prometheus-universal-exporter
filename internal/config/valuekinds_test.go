package config

import (
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"gopkg.in/yaml.v3"
)

// A value is written as the kind its key takes. The decoder hands a key of
// text the text of whatever is written, so that name: true was the name
// "true" and delimiter: 1 the character 1, and reads the words YAML 1.1 had
// for a boolean — yes, no, on, off, y, n — as one where a key takes a
// boolean, quoted or not, which are text to YAML and to an editor. The
// schemas refuse both, and so does the load: a boolean or a number at a key
// the schemas hold to text alone — a name, a word of a few, a host, the
// delimiter — is told by its line, with the key, the list and the entry's
// place in it, how it is written, how YAML reads it and how to write the
// text if the text is meant, or, at a key that takes one of a few words,
// which quoting would not make it one of, the words; and such a word at a
// key that takes a boolean is told with which of true and false it was read
// as. Several are all told,
// in the order of the file, after whatever the decoder itself refuses, and
// the checks of the configuration, which come after, are not reached.
func TestAValueOfAnotherKindThanItsKeyTakesIsRefused(t *testing.T) {
	notText := func(line int, place, written, kind string) string {
		return fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; to use that text there, quote it: %q", line, place, written, kind, written)
	}
	oneOf := func(line int, place, written, kind string, values ...string) string {
		return fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; write one of %s", line, place, written, kind, strings.Join(values, ", "))
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "get", "post", "put", "patch", "delete", "head"}
	noBoolean := func(line int, place, written string, meant bool) string {
		return fmt.Sprintf("line %d: %s is written %s, which YAML reads as text, not as a boolean; write %t", line, place, written, meant)
	}
	for name, tc := range map[string]struct {
		document string
		want     []string
	}{
		"a rule's name": {"collectors:\n  - name: node\n    metrics:\n      - name: true\n", []string{notText(4, "name", "true", "a boolean")}},
		"names of each kind": {"collectors:\n  - name: True\n    metrics_prefix: FALSE\n    metrics:\n      - name: 1\n        labels:\n          - name: 0x1F\n            value: x\n          - name: false\n            value: x\n", []string{
			notText(2, "name", "True", "a boolean"), notText(3, "metrics_prefix", "FALSE", "a boolean"), notText(5, "name", "1", "a number"), notText(7, "name", "0x1F", "a number"), notText(9, "name", "false", "a boolean"),
		}},
		"words of a few": {"collectors:\n  - name: node\n    name_escaping: false\n    request:\n      type: 1\n      method: 1.5\n    decoder: {type: true}\n    transform: {type: 2}\n    error_handling:\n      on_fetch_error: false\n    metrics:\n      - name: up\n        type: 1e3\n        error_mode: true\n        time_zone: +1\n", []string{
			oneOf(3, "name_escaping", "false", "a boolean", "fail", "underscores", "values"), oneOf(5, "type", "1", "a number", fetch.BuiltRequestTypes()...), oneOf(6, "method", "1.5", "a number", methods...),
			oneOf(7, "type", "true", "a boolean", model.DecoderTypes...), oneOf(8, "type", "2", "a number", model.TransformTypes...),
			oneOf(10, "on_fetch_error", "false", "a boolean", "fail", "log", "ignore"), oneOf(13, "type", "1e3", "a number", "gauge", "counter", "histogram", "summary", "untyped"), oneOf(14, "error_mode", "true", "a boolean", "fail", "log", "ignore"),
			notText(15, "time_zone", "+1", "a number"),
		}},
		"entries of lists": {"collector_files: [a.yaml, 1, true]\ncollectors:\n  - name: node\n    request:\n      redirect_trusted_hosts:\n        - 10\n        - cdn.example.com\n        - .5\n      accept_codes: [5]\n      retry: {codes: [NOT_FOUND, 14]}\n    transform:\n      libraries: [lxml, 3.12]\n", []string{
			notText(1, "collector_files entry 2", "1", "a number"), notText(1, "collector_files entry 3", "true", "a boolean"),
			notText(6, "redirect_trusted_hosts entry 1", "10", "a number"), notText(8, "redirect_trusted_hosts entry 3", ".5", "a number"), notText(9, "accept_codes entry 1", "5", "a number"), notText(10, "codes entry 2", "14", "a number"),
			oneOf(12, "libraries entry 2", "3.12", "a number", model.SortedKeys(transform.PythonLibraries)...),
		}},
		"the delimiter": {"collectors:\n  - name: node\n    response:\n      csv:\n        delimiter: 1\n", []string{notText(5, "delimiter", "1", "a number")}},
		"words that are no boolean": {"collectors:\n  - name: node\n    coalesce: yes\n    request:\n      follow_redirects: On\n      tls: {insecure_skip_verify: n}\n    response:\n      csv: {header: NO, trim_space: \"y\"}\n    metrics:\n      - name: up\n        required: 'off'\n        labels:\n          - name: site\n            truncate: Yes\n", []string{
			noBoolean(3, "coalesce", "yes", true), noBoolean(5, "follow_redirects", "On", true), noBoolean(6, "insecure_skip_verify", "n", false), noBoolean(8, "header", "NO", false), noBoolean(8, "trim_space", "y", true),
			noBoolean(11, "required", "off", false), noBoolean(14, "truncate", "Yes", true),
		}},
		"the switch of a block": {"otlp:\n  enabled: yes\n  endpoint: http://collector:4318/v1/metrics\nweb:\n  basic_auth:\n    enabled: off\n  self_metrics: {verbose: on}\n", []string{
			noBoolean(2, "enabled", "yes", true), noBoolean(6, "enabled", "off", false), noBoolean(7, "verbose", "on", true),
		}},
		"beside what the decoder refuses": {"collectors:\n  - name: true\n    requst: {}\n    coalesce: maybe\n    limits:\n      max_metrics: 0.5\n      max_help_length:\n    request:\n      enable_http2: yes\n", []string{
			`line 3: unknown key "requst" in a collector`, "line 4: expected true or false, not a string", notText(2, "name", "true", "a boolean"), "line 6: max_metrics is 0.5, which is not a whole number",
			"line 7: max_help_length has nothing after its colon, which YAML reads as no value at all; write its value, or take the key out", noBoolean(9, "enable_http2", "yes", true),
		}},
		// The value an alias names is the value, told where it is written,
		// and a key the mapping writes is the one read, over a merged one.
		"through an alias":    {"x-name: &name true\nx-on: &on yes\ncollectors:\n  - name: *name\n    coalesce: *on\n", []string{notText(1, "name", "true", "a boolean"), noBoolean(2, "coalesce", "yes", true)}},
		"over a merged key":   {"x-request: &request {type: http, method: GET}\ncollectors:\n  - name: node\n    request:\n      <<: *request\n      method: 1\n", []string{oneOf(6, "method", "1", "a number", methods...)}},
		"in a merged mapping": {"x-request: &request {type: http, method: 1, follow_redirects: yes}\ncollectors:\n  - name: node\n    request:\n      <<: *request\n", []string{oneOf(1, "method", "1", "a number", methods...), noBoolean(1, "follow_redirects", "yes", true)}},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", tc.document))
		if want := strings.Join(tc.want, "; "); err == nil || err.Error() != want {
			t.Errorf("%s: the load says\n%v\nwant\n%s", name, err, want)
		}
	}
	// A collector file's and the target file's are told the same way.
	dir := t.TempDir()
	file := testutil.WriteIn(t, dir, "collectors.yaml", "collectors:\n  - name: 7\n    coalesce: no\n")
	want := "collector file " + file + ": " + notText(2, "name", "7", "a number") + "; " + noBoolean(3, "coalesce", "no", false)
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")); err == nil || err.Error() != want {
		t.Errorf("in a collector file: %v\nwant %s", err, want)
	}
	want = notText(3, "name", "1", "a number") + "; " + noBoolean(5, "export_via_otlp", "yes", true) + "; " + oneOf(7, "method", "true", "a boolean", methods[:6]...) + "; " + notText(8, "accept_codes entry 1", "5", "a number") + "; " + noBoolean(9, "non_idempotent", "N", false)
	if _, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: 1\n    collector: node\n    export_via_otlp: yes\n    request:\n      method: true\n      accept_codes: [5]\n      retry: {non_idempotent: N}\n")); err == nil || err.Error() != want {
		t.Errorf("in the target file: %v\nwant %s", err, want)
	}

	// What is not refused. Free text takes a boolean and a number as the
	// text they spell, in a key, an entry and a mapping's value, and a key
	// of a mapping is text however it is written; text is text at a key
	// held to it, a date written without quotes and a word of YAML 1.1's
	// among it, and so is what is quoted; a boolean is true or false in
	// any of the three cases YAML takes; a value an anchor holds and the
	// mapping that merges it in overrides is never read there; and a
	// file's own x- key holds what it likes.
	for name, document := range map[string]string{
		"free text":       "collectors:\n  - name: node\n    request:\n      path: 1\n      body: true\n      bearer_token: 12345\n      headers: {X-Shard: 3, X-Debug: true}\n      query: {page: 1}\n      allowed_targets: [10, true]\n    transform:\n      labels: {shard: 3, canary: false}\n      include: [1]\n    metrics:\n      - name: up\n        description: 404\n        expression: 1\n        items: true\n        labels:\n          - name: site\n            value: 1\n          - name: code\n            expression: 2\n            value_map: {1: 1.5, 2: true}\n",
		"keys of mapping": "collectors:\n  - name: node\n    metrics:\n      - name: state\n        value_map: {1: 0, true: 1, 2.5: 2}\n",
		"text":            "collectors:\n  - name: yes\n    metrics_prefix: on\n    metrics:\n      - name: 2026-10-06\n        time_zone: no\n        labels:\n          - name: y\n            value: x\n",
		"quoted":          "collectors:\n  - name: \"true\"\n    metrics_prefix: '1'\n    request: {type: \"1\", redirect_trusted_hosts: ['10', \"true\"]}\n    response: {csv: {delimiter: \"1\"}}\n    metrics:\n      - name: !!str 1\n",
		"booleans":        "collectors:\n  - name: node\n    coalesce: True\n    request: {follow_redirects: FALSE, enable_http2: true}\n    metrics:\n      - name: up\n        required: false\n",
		"overridden":      "x-request: &request {type: http, method: 1, follow_redirects: yes}\ncollectors:\n  - name: node\n    request:\n      <<: *request\n      method: GET\n      follow_redirects: true\n",
		"the file's own":  "x-name: true\nx-list: [yes, 1]\ncollectors:\n  - name: node\n",
	} {
		if got := valueProblems([]byte(document), reflect.TypeOf(model.Config{})); got != nil {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := valueProblems([]byte("interval: 1m\ntargets:\n  - name: yes\n    collector: 1\n    target: true\n    labels: {shard: 3, 1: one}\n    params: {param_id: 7}\n    request: {path: 1, body: false, headers: {X-Shard: 3}}\n"), reflect.TypeOf(model.StaticTargetFile{})); got != nil {
		t.Errorf("the target file's free text: %q", got)
	}
}

// The words refused where a boolean is taken are the ones the decoder reads
// as a boolean there, each as the one it says, and no others: any other
// text, a word in another case among it, the decoder refuses itself, as it
// does a number.
func TestTheWordsRefusedAsBooleansAreTheOnesTheDecoderReadsAsOne(t *testing.T) {
	if len(yamlBooleanWords) != 16 {
		t.Fatalf("%d words", len(yamlBooleanWords))
	}
	for word, meant := range yamlBooleanWords {
		for _, written := range []string{word, `"` + word + `"`} {
			var read struct {
				Switch bool `yaml:"switch"`
			}
			if err := yaml.Unmarshal([]byte("switch: "+written+"\n"), &read); err != nil || read.Switch != meant {
				t.Errorf("the decoder reads %s as %v, %v; want %v", written, read.Switch, err, meant)
			}
			want := []string{fmt.Sprintf("line 3: coalesce is written %s, which YAML reads as text, not as a boolean; write %t", word, meant)}
			if got := valueProblems([]byte("collectors:\n  - name: node\n    coalesce: "+written+"\n"), reflect.TypeOf(model.Config{})); !slices.Equal(got, want) {
				t.Errorf("coalesce: %s: %q, want %q", written, got, want)
			}
		}
	}
	for _, written := range []string{"maybe", "t", "f", "yEs", "oN", "nO", "ja", "enabled", `"true"`, "'False'", "0", "1"} {
		var read struct {
			Switch bool `yaml:"switch"`
		}
		if err := yaml.Unmarshal([]byte("switch: "+written+"\n"), &read); err == nil {
			t.Errorf("the decoder reads %s as the boolean %v", written, read.Switch)
		}
		if got := valueProblems([]byte("collectors:\n  - name: node\n    coalesce: "+written+"\n"), reflect.TypeOf(model.Config{})); got != nil {
			t.Errorf("coalesce: %s, which the decoder refuses itself: %q", written, got)
		}
	}
}

// A value an environment variable supplies is the kind it reads as, as one
// written in the file is: a name taken from a variable set to true is a
// boolean where the reference stands unquoted, and is refused as one
// written so is, and in quotes it is the text.
func TestAValueFromAVariableIsTheKindItReadsAs(t *testing.T) {
	t.Setenv("KINDS_NAME", "true")
	t.Setenv("KINDS_SWITCH", "yes")
	for document, want := range map[string]string{
		"collectors:\n  - name: ${KINDS_NAME}\n":                       `line 2: name is written true, which YAML reads as a boolean, not as text; to use that text there, quote it: "true"`,
		"collectors:\n  - name: node\n    coalesce: ${KINDS_SWITCH}\n": "line 3: coalesce is written yes, which YAML reads as text, not as a boolean; write true",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document), WithEnvExpansion()); err == nil || err.Error() != want {
			t.Errorf("the load says %v\nwant %s\n%s", err, want, document)
		}
	}
	_, err := Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: \"${KINDS_NAME}\"\n    metrics_prefix: '${KINDS_SWITCH}'\n"), WithEnvExpansion())
	if err == nil || strings.Contains(err.Error(), "YAML reads") || !strings.Contains(err.Error(), `collector "true"`) {
		t.Errorf("a quoted reference: the load says %v, want it past the reading of the file, as the collector \"true\"", err)
	}
}

// textOnlyKeys are the keys of a file of type t that the load holds to text
// alone, each by the path its schema has it at, [] for an entry of a list
// and .* for a value of a mapping, with the words it is held to, where it is
// held to some.
func textOnlyKeys(t reflect.Type) map[string][]string {
	keys := map[string][]string{}
	var walk func(kinds *valueKinds, path string)
	walk = func(kinds *valueKinds, path string) {
		if kinds == nil {
			return
		}
		if kinds.textOnly {
			keys[path] = kinds.allowed
		}
		for key, sub := range kinds.keys {
			walk(sub, joinSchemaPath(path, key))
		}
		walk(kinds.entries, path+"[]")
		walk(kinds.values, path+".*")
	}
	walk(schemaKinds(t), "")
	return keys
}

// The keys the load holds to text alone are the ones the schemas hold to
// it, read from the schemas and not listed in the code: a key with allowed
// values or a pattern, which the schemas make text alone, and the few they
// say are text themselves. They are named here, so that a key that comes to
// be among them, or to leave them, is a change someone has seen: a number
// or a boolean written there was taken as its text, and is refused.
func TestTheKeysHeldToTextAloneAreTheSchemas(t *testing.T) {
	collector := []string{
		"decoder.type", "error_handling.on_decode_error", "error_handling.on_fetch_error", "error_handling.on_transform_error",
		"metrics[].error_mode", "metrics[].labels[].name", "metrics[].name", "metrics[].time_zone", "metrics[].type", "metrics_prefix", "name", "name_escaping",
		"request.accept_codes[]", "request.descriptors", "request.method", "request.redirect_trusted_hosts[]", "request.retry.codes[]", "request.rpc", "request.type",
		"response.csv.delimiter", "response.graphite.invalid_lines", "response.graphite.value", "transform.libraries[]", "transform.required_libs[]", "transform.type",
	}
	var ofCollectors []string
	for _, key := range collector {
		ofCollectors = append(ofCollectors, "collectors[]."+key)
	}
	for name, tc := range map[string]struct {
		file reflect.Type
		want []string
	}{
		"the configuration":      {reflect.TypeOf(model.Config{}), append([]string{"collector_files[]"}, ofCollectors...)},
		"a collector file":       {reflect.TypeOf(collectorFile{}), ofCollectors},
		"the static target file": {reflect.TypeOf(model.StaticTargetFile{}), []string{"targets[].name", "targets[].request.accept_codes[]", "targets[].request.method", "targets[].request.retry.codes[]"}},
	} {
		got := model.SortedKeys(textOnlyKeys(tc.file))
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s holds to text alone\n%q\nwant\n%q", name, got, tc.want)
		}
	}
	if got := textOnlyKeys(reflect.TypeOf(model.Collector{})); len(got) != 0 {
		t.Errorf("a type that is no file's holds %v to text", got)
	}
}

// kindsWritten are the ways a single value is written that the differential
// test puts at every place: what YAML reads each as, and the word a key
// that takes a boolean reads it as, where it is one of YAML 1.1's. The first
// six are booleans and numbers, and the last four such words.
var kindsWritten = []struct {
	written, reads string
	word           bool
}{
	{"true", "a boolean", false}, {"False", "a boolean", false}, {"1", "a number", false}, {"-1.5e3", "a number", false}, {"0x1F", "a number", false}, {".inf", "a number", false},
	{"2026-10-06", "", false}, {"text", "", false}, {`"1"`, "", false}, {"'true'", "", false},
	{"yes", "", true}, {"Off", "", true}, {`"no"`, "", true}, {"Y", "", true},
}

// Holding a value to the kind its key takes changes what the load says of
// nothing else. valueProblems, which is all of the load that changed, says
// of every document the repository ships — the examples, the files under
// configs, the fixtures and each configuration and target file among the
// fenced blocks of the documentation — what it said, so none of them writes
// a number or a boolean where text alone is taken, or a word for a boolean:
// each loads or is refused as it was, message for message. Of each of those
// documents with one value more, written at one place in one of the ways
// YAML has for a single value (kindsWritten) — at every key its mappings
// write and every key they could, in every list and in every mapping of
// names to values (eachNoValueSite) — it says what it said of that document
// before, word for word and in order, and one thing more only where the
// value is of another kind than the place takes: a boolean or a number
// where the schemas hold the key to text alone, the keys named in
// TestTheKeysHeldToTextAloneAreTheSchemas, and a word that is no boolean
// where a boolean is taken. Text, quoted or not, a date among it, is told
// of nowhere; a boolean and a number are told of at no key of free text, at
// no key of a mapping's values, and at no key that takes a number, a
// duration, a size, a block or a list, where the decoder says what it said.
//
// The places are some nine thousand, and each is a document written out
// and read twice, so of each kind of place in each kind of file every
// thirtieth is tried, and under the race detector, where one costs many
// times more, every three hundredth, in one of the ways in turn; of the places that
// take text alone or a boolean, three times as many, each in one of the
// ways that are neither too, so that the places tried under the race
// detector still have some of each among them.
func TestOnlyAValueOfAnotherKindIsRefusedAnew(t *testing.T) {
	documents := shippedDocuments(t)
	shipped := len(documents)
	for _, document := range []string{
		"collectors:\n  - name: node\n    max_concurrent_probes: 1.9\n    coalesce: maybe\n    limits:\n      max_metrics: 0.5\n    metrics:\n      - name: state\n        value_map: {null: 0, ~: 1, up: 1}\n        labels:\n          - name: site\n            value: x\n",
		"interval: 1m\nconcurrency: 2.5\ntargets:\n  - collector: node\n    request:\n      retry: {attempts: 1.5}\n    labels: {~: x}\n",
		"web: {self_metrics: {verbose: true}}\notlp: {enabled: false, max_pending_points: 10.5, headers: {null: x}}\ncollector_files: ['a.yaml']\n",
	} {
		documents[document] = reflect.TypeOf(model.Config{})
		if strings.HasPrefix(document, "interval") {
			documents[document] = reflect.TypeOf(model.StaticTargetFile{})
		}
	}
	every := alloctest.UnlessRaced(30, 300)
	at, tried, anew, asBefore, refusedBefore := 0, 0, map[string]int{}, 0, 0
	seen := map[string]int{}
	for _, text := range model.SortedKeys(documents) {
		kind := documents[text]
		was := valueProblemsBeforeKinds([]byte(text), kind)
		if now := valueProblems([]byte(text), kind); !slices.Equal(now, was) {
			t.Errorf("of a shipped document the load says\n%q\nand said\n%q\n%s", now, was, text)
			continue
		}
		if len(was) > 0 {
			refusedBefore++
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
			t.Fatal(err)
		}
		textOnly := textOnlyKeys(kind)
		eachNoValueSite(&doc, kind, func(site noValueSite) {
			class := site.kind + " of a " + kind.Name()
			at, seen[class] = at+1, seen[class]+1
			pick := seen[class]%every == 1
			if _, held := textOnly[site.path]; site.of.Kind() == reflect.Bool || site.of.Kind() == reflect.String && held {
				class = "another kind at " + class
				seen[class]++
				pick = pick || seen[class]%(every/3) == 1
			}
			if !pick {
				return
			}
			out, err := yaml.Marshal(&doc)
			before, _, marked := strings.Cut(string(out), noValueHere)
			if err != nil || !marked || strings.Count(string(out), noValueHere) != 1 {
				t.Fatalf("writing the document with a place marked: %v\n%s", err, out)
			}
			line := strings.Count(before, "\n") + 1
			ways := []int{tried % len(kindsWritten)}
			switch _, held := textOnly[site.path]; {
			case site.of.Kind() == reflect.String && held:
				ways = append(ways, tried%6)
			case site.of.Kind() == reflect.Bool:
				ways = append(ways, len(kindsWritten)-4+tried%4)
			}
			for _, way := range slices.Compact(ways) {
				way := kindsWritten[way]
				tried++
				written := []byte(strings.Replace(string(out), noValueHere, way.written, 1))
				now, was := valueProblems(written, kind), valueProblemsBeforeKinds(written, kind)
				var want string
				allowed, held := textOnly[site.path]
				switch word := strings.Trim(way.written, `"`); {
				case site.of.Kind() == reflect.String && held && way.reads != "" && len(allowed) > 0:
					want = fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; write one of %s", line, site.named, way.written, way.reads, strings.Join(allowed, ", "))
				case site.of.Kind() == reflect.String && held && way.reads != "":
					want = fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; to use that text there, quote it: %q", line, site.named, way.written, way.reads, way.written)
				case site.of.Kind() == reflect.Bool && way.word:
					want = fmt.Sprintf("line %d: %s is written %s, which YAML reads as text, not as a boolean; write %t", line, site.named, word, yamlBooleanWords[word])
				}
				if want == "" {
					asBefore++
					if !slices.Equal(now, was) {
						t.Errorf("%s written %s: the load says\n%q\nand said\n%q\n%s", site.path, way.written, now, was, written)
					}
					continue
				}
				anew[map[bool]string{true: "text alone", false: "a boolean"}[way.reads != ""]]++
				found := slices.Index(now, want)
				if found < 0 || !slices.Equal(slices.Delete(slices.Clone(now), found, found+1), was) {
					t.Errorf("%s written %s: the load says\n%q\nand said\n%q\nwant what it said and %s\n%s", site.path, way.written, now, was, want, written)
				}
			}
		})
	}
	if shipped < 40 || refusedBefore < 3 || tried < alloctest.UnlessRaced(600, 60) || anew["text alone"] < alloctest.UnlessRaced(180, 15) || anew["a boolean"] < alloctest.UnlessRaced(80, 5) || asBefore < alloctest.UnlessRaced(300, 30) {
		t.Fatalf("%d documents are shipped, %d were refused before, and of %d places tried %v were refused anew and %d were as before", shipped, refusedBefore, tried, anew, asBefore)
	}
	t.Logf("%d documents, %d of them refused before; of %d places, %d tried: %v refused anew, %d as before", len(documents), refusedBefore, at, tried, anew, asBefore)
}

// Holding the values to their kinds costs a load nothing where there is
// nothing to refuse: the walk of the example configuration, the largest
// file shipped, and of the example target file allocates no more than the
// walk as it was, to within a hundredth. What a place takes is read from a
// tree made of the schema once, not looked up by a path joined at every key,
// with which a reload allocated a few per cent more.
func TestTheWalkOfAWellWrittenFileAllocatesNoMoreThanItDid(t *testing.T) {
	for file, kind := range map[string]reflect.Type{
		"../../configs/config.example.yaml":         reflect.TypeOf(model.Config{}),
		"../../configs/static-targets.example.yaml": reflect.TypeOf(model.StaticTargetFile{}),
	} {
		document, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := valueProblems(document, kind); got != nil {
			t.Fatalf("%s: %q", file, got)
		}
		runs := alloctest.UnlessRaced(5, 2)
		was, _ := alloctest.Allocations(runs, func() { valueProblemsBeforeKinds(document, kind) })
		// A hundredth more is what the race detector's own allocations
		// move a count by, one or two here; a path at every key was a
		// tenth more and over.
		most := was + was/100
		now := alloctest.AllocsAtMost(runs, most, func() { valueProblems(document, kind) })
		if now > most || was < 100 {
			t.Errorf("%s: the walk allocates %v times, and %v as it was", file, now, was)
		}
		t.Logf("%s: %v allocations, %v as it was", file, now, was)
	}
}

// valueProblemsBeforeKinds is valueProblems as it was before a value of
// another kind than its key takes was refused, kept as an oracle: of a
// document whose values are of the kinds their keys take valueProblems must
// say what this says, and of any other, that and one problem for each value
// that is not.
func valueProblemsBeforeKinds(document []byte, t reflect.Type) []string {
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
				field, ok := fields[entry.Key.Value]
				switch {
				case !ok:
				case noValue(entry.Value):
					problems = append(problems, fmt.Sprintf("line %d: %s %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, entry.Key.Value, noValueAfterColon(entry.Value)))
				default:
					walk(entry.Value, field, entry.Key.Value)
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
				if noValue(entry.Value) {
					problems = append(problems, fmt.Sprintf("line %d: %s key %q %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, key, entry.Key.Value, noValueAfterColon(entry.Value)))
					continue
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
