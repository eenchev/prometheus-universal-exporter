//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// spelling is a key that takes one of a few words, put through the schema
// and the exporter in the spelling the schema takes and in others: the
// document, the place of the key in it, at, what setting writes there with
// the word for its %s, the spellings taken and those refused.
type spelling struct {
	key, document, at, setting string
	taken, refused             []string
}

// spellings are the keys of an http collector that take one of a few
// words, and the defaults' error policy: each in lower case, as the schema
// has it, request.method in upper case or lower case.
func spellings() []spelling {
	collector, request, metric := "    # collector\n", "      # request\n", "        # metric\n"
	graphite := strings.Replace(jqCollector, collector, "    decoder: {type: graphite}\n"+collector, 1)
	policy := func(key string) spelling {
		return spelling{key: "collectors[].error_handling." + key, document: jqCollector, at: collector, setting: "    error_handling:\n      " + key + ": %s\n", taken: []string{"fail", "log", "ignore"}, refused: []string{"LOG", "Fail", "IGNORE", "' log'", "'log '"}}
	}
	return []spelling{
		{key: "collectors[].request.type", document: jqCollector, at: "      type: http\n", setting: "      type: %s\n", taken: []string{"http"}, refused: []string{"HTTP", "Http", "' http'", "'http '"}},
		{key: "collectors[].transform.type", document: jqCollector, at: "      type: jq\n", setting: "      type: %s\n", taken: []string{"jq"}, refused: []string{"JQ", "Jq", "' jq'"}},
		{key: "collectors[].decoder.type", document: jqCollector, at: collector, setting: "    decoder: {type: %s}\n", taken: []string{"json", "auto"}, refused: []string{"JSON", "Auto", "' json'", "'auto '"}},
		policy("on_fetch_error"), policy("on_decode_error"), policy("on_transform_error"),
		{key: "defaults.error_handling.on_fetch_error", document: "defaults:\n  error_handling: {on_fetch_error: %s}\n" + jqCollector, at: "%s", setting: "%s", taken: []string{"log"}, refused: []string{"LOG", "' log'"}},
		{key: "collectors[].metrics[].error_mode", document: jqCollector, at: metric, setting: "        error_mode: %s\n", taken: []string{"fail", "log", "ignore"}, refused: []string{"FAIL", "Log", "' ignore '"}},
		{key: "collectors[].request.pagination.type", document: paginatedCollector, at: "        type: page\n", setting: "        type: %s\n", taken: []string{"page"}, refused: []string{"PAGE", "Page", "' page'"}},
		{key: "collectors[].response.graphite.value", document: graphite, at: collector, setting: "    response:\n      graphite: {value: %s}\n", taken: []string{"max", "last"}, refused: []string{"MAX", "Last", "' max'"}},
		{key: "collectors[].response.graphite.invalid_lines", document: graphite, at: collector, setting: "    response:\n      graphite: {invalid_lines: %s}\n", taken: []string{"skip", "fail"}, refused: []string{"SKIP", "Fail", "'skip '"}},
		{key: "collectors[].request.method", document: jqCollector, at: request, setting: "      method: %s\n", taken: []string{"GET", "get", "POST", "post", "HEAD", "head"}, refused: []string{"Get", "pOST", "' GET'", "'get '"}},
	}
}

// A key that takes one of a few words takes them in the spelling the
// schema has, and no other, to the exporter as to the schema: request.type,
// transform.type, decoder.type, the three error policies, the defaults'
// too, a rule's error_mode, request.pagination.type and response.graphite's
// value and invalid_lines in lower case and without blanks around them, and
// request.method in upper case or lower case. The exporter took each in any
// case and with blanks around it, which the schema refused: error_handling
// on_fetch_error: LOG was taken by the exporter alone, as were type: HTTP,
// method: Get and decoder.type: ' json'.
func TestSchemaAndExporterAgreeOnTheSpellingOfAKeysWords(t *testing.T) {
	schema := loadSchema(t)
	for _, key := range spellings() {
		if strings.Count(key.document, key.at) != 1 {
			t.Fatalf("%s: the document has %q %d times", key.key, key.at, strings.Count(key.document, key.at))
		}
		for _, words := range []struct {
			words    []string
			accepted bool
		}{{key.taken, true}, {key.refused, false}} {
			for _, word := range words.words {
				document := strings.Replace(key.document, key.at, strings.Replace(key.setting, "%s", word, 1), 1)
				agree(t, schema, key.key+": "+word, document, words.accepted)
			}
		}
	}
}

// A static target's request.method takes what a collector's does: GET and
// get, which the schema refused in lower case though the exporter took it,
// and not Get or a method with blanks around it, which the exporter took
// and the schema refused.
func TestSchemaAndExporterAgreeOnTheSpellingOfAStaticTargetsMethod(t *testing.T) {
	targetSchema := loadSchemaFile(t, staticTargetsSchemaFile)
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", targetsConfiguration))
	if err != nil {
		t.Fatal(err)
	}
	for method, accepted := range map[string]bool{"GET": true, "get": true, "POST": true, "post": true, "Get": false, "pOST": false, "' GET'": false, "'get '": false} {
		document := "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n    request: {path: /status/x, method: " + method + "}\n"
		problems := schemaProblems(t, targetSchema, document)
		file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
		if err == nil {
			err = config.ValidateStaticTargets(file)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(file, cfg)
		}
		if (len(problems) == 0) != accepted || (err == nil) != accepted {
			t.Errorf("method: %s: want accepted %v by both; the schema says %v, the exporter %v", method, accepted, problems, err)
		}
	}
}
