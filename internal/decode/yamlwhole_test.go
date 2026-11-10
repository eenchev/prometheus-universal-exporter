package decode

import (
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A whole number beyond 64 bits keeps every digit in YAML as it does in
// JSON: the two documents below are one to the transforms.
func TestAYAMLWholeNumberBeyond64BitsKeepsEveryDigit(t *testing.T) {
	const id = "123456789012345678901234567890"
	asYAML := "id: " + id + "\n" +
		"negative: -" + id + "\n" +
		"signed: +" + id + "\n" +
		"list: [" + id + ", 7, 1.5, 18446744073709551615, 18446744073709551616]\n" +
		"nested: {deep: [{id: " + id + "}]}\n" +
		"anchored: &a " + id + "\n" +
		"aliased: *a\n" +
		"quoted: \"" + id + "\"\n" +
		"tagged: !!float " + id + "\n" +
		"text: !!str " + id + "\n" +
		"fraction: " + id + ".5\n" +
		"exponent: 1e40\n" +
		"small: 42\n" +
		id + ": key\n"
	asJSON := `{"id": ` + id + `, "negative": -` + id + `, "signed": ` + id +
		`, "list": [` + id + `, 7, 1.5, 18446744073709551615, 18446744073709551616]` +
		`, "nested": {"deep": [{"id": ` + id + `}]}, "anchored": ` + id + `, "aliased": ` + id +
		`, "quoted": "` + id + `", "tagged": 1.2345678901234568e+29, "text": "` + id + `"` +
		`, "fraction": ` + id + `.5, "exponent": 1e40, "small": 42, "1.2345678901234568e+29": "key"}`
	decoded := func(body, decoder string) any {
		t.Helper()
		d, err := Decode(&fetch.HTTPResponse{Body: []byte(body)}, &model.Collector{Decoder: model.DecoderConfig{Type: decoder}})
		if err != nil {
			t.Fatal(err)
		}
		return d.Data
	}
	fromYAML, fromJSON := decoded(asYAML, "yaml"), decoded(asJSON, "json")
	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Fatalf("yaml decoded to\n%#v\njson to\n%#v", fromYAML, fromJSON)
	}
	want, _ := new(big.Int).SetString(id, 10)
	if got, ok := fromYAML.(map[string]any)["id"].(*big.Int); !ok || got.Cmp(want) != 0 {
		t.Fatalf("id = %#v, want the whole number %s", fromYAML.(map[string]any)["id"], id)
	}

	// The document that is nothing but the number, and the one too long to
	// be made a number of, which stays the text of its digits as JSON's does.
	if got, ok := decoded(id, "yaml").(*big.Int); !ok || got.Cmp(want) != 0 {
		t.Errorf("a document of the number alone = %#v", decoded(id, "yaml"))
	}
	long := strings.Repeat("9", model.MaxWholeNumberDigits+1)
	if got := decoded("n: "+long, "yaml"); !reflect.DeepEqual(got, decoded(`{"n": `+long+`}`, "json")) || got.(map[string]any)["n"] != long {
		t.Errorf("a number of %d digits is not its text", len(long))
	}

	// A key that is an alias of such a number is its digits, with nothing of
	// the mark it was decoded under.
	aliasKey := decoded("a: &x "+id+"\n*x : v\n", "yaml").(map[string]any)
	if aliasKey[id] != "v" || len(aliasKey) != 2 {
		t.Errorf("a key that is an alias of the number: %#v", aliasKey)
	}

	// Text that only looks like what a number is marked as is text.
	for _, text := range []string{"whole number 123", strings.TrimSpace(yamlWholeMark), "whole number 000000000000000000000000 12"} {
		if got := decoded("v: "+text, "yaml").(map[string]any)["v"]; got != text {
			t.Errorf("the text %q decoded to %#v", text, got)
		}
	}
}
