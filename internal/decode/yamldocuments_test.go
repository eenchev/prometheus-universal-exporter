package decode

import (
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// With response.yaml.documents: list a YAML body is the list of its
// documents, in their order, whether it holds several, one or none; a
// document that holds nothing is none of them, and each is decoded as the
// one document of a body is. Without it a second document is refused,
// naming the setting.
func TestAYAMLBodyOfSeveralDocumentsIsAListOfThem(t *testing.T) {
	decoded := func(body, documents string) (any, error) {
		c := &model.Collector{Decoder: model.DecoderConfig{Type: "yaml"}, Response: model.ResponseConfig{YAML: model.YAMLConfig{Documents: documents}}}
		d, err := Decode(&fetch.HTTPResponse{Body: []byte(body)}, c)
		if err != nil {
			return nil, err
		}
		return d.Data, nil
	}
	id, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	for name, tc := range map[string]struct {
		body string
		want any
	}{
		"several":                  {"kind: Pod\nname: a\n---\nkind: Service\nname: b\n", []any{map[string]any{"kind": "Pod", "name": "a"}, map[string]any{"kind": "Service", "name": "b"}}},
		"one":                      {"kind: Pod\n", []any{map[string]any{"kind": "Pod"}}},
		"none":                     {"", []any{}},
		"only separators":          {"---\n---\n", []any{}},
		"a leading and a trailing": {"---\na: 1\n---\n", []any{map[string]any{"a": 1}}},
		"an empty one between":     {"a: 1\n---\n---\nb: 2\n", []any{map[string]any{"a": 1}, map[string]any{"b": 2}}},
		"a null and scalars":       {"null\n---\n7\n---\ntext\n---\n[1, 2]\n", []any{nil, 7, "text", []any{1, 2}}},
		"an end marker":            {"a: 1\n...\n---\nb: 2\n", []any{map[string]any{"a": 1}, map[string]any{"b": 2}}},
		"each read as one is":      {"at: 2024-06-01\n---\nid: 123456789012345678901234567890\n---\n1: x\n", []any{map[string]any{"at": "2024-06-01"}, map[string]any{"id": id}, map[string]any{"1": "x"}}},
		// The library keeps a document's anchors for those after it.
		"an alias of an earlier document": {"a: &n 123456789012345678901234567890\nb: &t 2024-06-01\n---\nc: *n\nd: *t\n", []any{map[string]any{"a": id, "b": "2024-06-01"}, map[string]any{"c": id, "d": "2024-06-01"}}},
		"an anchor in each":               {"a: &x 1\nb: *x\n---\na: &x 2\nb: *x\n", []any{map[string]any{"a": 1, "b": 1}, map[string]any{"a": 2, "b": 2}}},
	} {
		got, err := decoded(tc.body, "list")
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %#v, %v; want %#v", name, got, err, tc.want)
		}
	}
	for name, tc := range map[string]struct{ body, want string }{
		"a key twice in the second":     {"a: 1\n---\nb: 1\nb: 2\n", `line 4: mapping key "b" already defined at line 3`},
		"a document that does not read": {"a: 1\n---\nb: [1\n", "did not find expected ',' or ']'"},
	} {
		if _, err := decoded(tc.body, "list"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	for _, documents := range []string{"", "one"} {
		if _, err := decoded("a: 1\n---\nb: 2\n", documents); err == nil || !strings.Contains(err.Error(), "YAML decode: the body holds more than one YAML document (separated by ---); set response.yaml.documents: list to read them as a list") {
			t.Errorf("documents %q: %v", documents, err)
		}
		if got, err := decoded("a: 1\n---\n", documents); err != nil || !reflect.DeepEqual(got, map[string]any{"a": 1}) {
			t.Errorf("documents %q, one document: %#v, %v", documents, got, err)
		}
	}
}
