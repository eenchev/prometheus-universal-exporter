package decode

import (
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// decodeKind decodes body with the decoder named, as a collector that sets
// it does.
func decodeKind(t *testing.T, kind, body string) (*Decoded, error) {
	t.Helper()
	return Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: http.Header{}}, &model.Collector{Name: kind, Decoder: model.DecoderConfig{Type: kind}})
}

// An NDJSON body is the list of the values of its lines, in their order,
// each made as the json decoder makes a document: a whole number an int, or
// a *big.Int past int64, any other a float64. Blank lines, and a carriage
// return before a line feed, are passed over, and a body of no value is the
// empty list.
func TestNDJSONIsTheListOfTheValuesOfItsLines(t *testing.T) {
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	for body, want := range map[string]any{
		"{\"a\":1}\n{\"a\":2.5}\n": []any{map[string]any{"a": 1}, map[string]any{"a": 2.5}},
		"\n  {\"id\":123456789012345678901234567890}\r\n\r\n\t[1, \"x\", null, true]  \n": []any{map[string]any{"id": huge}, []any{1, "x", nil, true}},
		"\"text\"\n42": []any{"text", 42},
		"":             []any{},
		"\n \r\n\t\n":  []any{},
	} {
		d, err := decodeKind(t, "ndjson", body)
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if d.Kind != "ndjson" || !reflect.DeepEqual(d.Data, want) {
			t.Errorf("%q decodes as %s %#v, want %#v", body, d.Kind, d.Data, want)
		}
	}
}

// A line that is no JSON value, one that goes on after its value, and one
// that ends inside it, as a pretty-printed value does, fail the decode
// naming the line, as the body's line and column; the failure is the same
// failure at whichever line it is.
func TestNDJSONRefusesALineThatIsNotOneValue(t *testing.T) {
	for body, want := range map[string]string{
		"{\"a\":1}\n\n{\"a\": nope}\n":     "NDJSON decode: invalid character 'o' in literal null (expecting 'u'), at line 3, column 8",
		"{\"a\":1}\n{\"a\":2} {\"a\":3}\n": "NDJSON decode: line 2, column 9: trailing data after the JSON value; NDJSON holds one value on each line",
		"{\n  \"a\": 1\n}\n":               "NDJSON decode: line 1: the line ends inside its JSON value; NDJSON holds one whole value on each line",
		"[1]\n\"a\nb\"\n":                  "NDJSON decode: line 2: the line ends inside its JSON value; NDJSON holds one whole value on each line",
	} {
		_, err := decodeKind(t, "ndjson", body)
		if err == nil || err.Error() != want {
			t.Errorf("%q: err=%v, want %s", body, err, want)
		}
	}
	_, first := decodeKind(t, "ndjson", "1 2\n")
	_, later := decodeKind(t, "ndjson", "1\n2\n3 4\n")
	if first == nil || later == nil || model.SameFailureText(first) != model.SameFailureText(later) {
		t.Errorf("the same mistake on another line is another failure: %q, %q", model.SameFailureText(first), model.SameFailureText(later))
	}
}

// The list is the outermost level of the document, so a line's value nests
// one level less than MaxDepth at most, and the document is within MaxDepth
// as every decoder's is.
func TestNDJSONNestsWithinMaxDepth(t *testing.T) {
	deep := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	if _, err := decodeKind(t, "ndjson", "1\n"+deep(MaxDepth-1)+"\n"); err != nil {
		t.Errorf("a line nested %d deep: %v", MaxDepth-1, err)
	}
	_, err := decodeKind(t, "ndjson", "1\n"+deep(MaxDepth)+"\n")
	if err == nil || !strings.Contains(err.Error(), "arrays and objects nested more than 10000 deep, at line 2, column 10000") {
		t.Errorf("a line nested %d deep: %v", MaxDepth, err)
	}
}
