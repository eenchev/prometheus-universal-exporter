package decode

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlKeysAsTextNow is the outcome v, err of decoding document as the YAML
// decoder did before a mapping two of whose keys are one text, and a null
// key, were refused, as it is now: a document that decoded is refused for
// them as yamlKeysAsText refuses it.
func yamlKeysAsTextNow(document []byte, v any, err error) (any, error) {
	if err != nil {
		return v, err
	}
	root, rootErr := yamlDocumentOf(document)
	if rootErr != nil || root == nil {
		return v, err
	}
	timestampsAsText(root, map[*yaml.Node]bool{})
	if refused := yamlKeysAsText(root); refused != nil {
		return nil, refused
	}
	return v, nil
}

// Keys of one mapping written otherwise that are one text in the decoded
// body fail the decode in the words of a key written twice, naming the text
// and both lines, whichever order Go's map would have given them in: 1 and
// 1.0, 16 and 0x10, which the library decodes into one value and kept the
// later of unseen, true and True, a text and the number it reads as, and a
// key and an alias of it. A null key fails it, as ~, null or nothing; and
// keys merged with << that are the text of the mapping's own fail it too.
// Keys that are different texts decode as they did.
func TestYAMLKeysThatAreOneTextAreRefused(t *testing.T) {
	for document, want := range map[string]string{
		"1: a\n1.0: b\n":                         "line 2: mapping key \"1\" already defined at line 1",
		"1.0: a\n1: b\n":                         "line 2: mapping key \"1\" already defined at line 1",
		"16: a\n0x10: b\n":                       "line 2: mapping key \"16\" already defined at line 1",
		"m:\n  true: a\n  True: b\n":             "line 3: mapping key \"true\" already defined at line 2",
		"'1': a\n1: b\n":                         "line 2: mapping key \"1\" already defined at line 1",
		"&k x: 1\n*k : 2\n":                      "line 2: mapping key \"x\" already defined at line 1",
		"a: 1\n~: 2\n":                           "line 2: mapping key \"~\" is null, which is no text; quote it to read it as text",
		"null: 2\n":                              "line 1: mapping key \"null\" is null, which is no text; quote it to read it as text",
		"? \n: 2\n":                              "line 1: mapping key \"\" is null, which is no text; quote it to read it as text",
		"b: &m {1.0: x}\nc:\n  <<: *m\n  1: y\n": "mapping key \"1\" is in a mapping twice, written otherwise each time, once merged into it with <<",
	} {
		for range 20 {
			_, err := decodeKind(t, "yaml", document)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%q: err=%v, want one that says %s", document, err, want)
			}
		}
	}
	d, err := decodeKind(t, "yaml", "1: a\n1.5: b\n'~': c\ntrue: d\n2024-06-01: e\nb: &m {2: x}\nc:\n  <<: *m\n  1: y\n")
	if err != nil {
		t.Fatal(err)
	}
	got := d.Data.(map[string]any)
	if len(got) != 7 || got["1"] != "a" || got["1.5"] != "b" || got["~"] != "c" || got["true"] != "d" || got["2024-06-01"] != "e" || len(got["c"].(map[string]any)) != 2 {
		t.Errorf("decoded as %#v", got)
	}
}
