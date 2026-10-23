package expr

import (
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/itchyny/gojq"
)

// valueVector is one input of testdata/values/helpers.json, the vectors the
// Python helpers are tested against too (internal/transform), and what the
// helper must make of it.
type valueVector struct {
	Input     any      `json:"input"`
	Output    *float64 `json:"output"`
	Error     *string  `json:"error"`
	TypeError bool     `json:"type_error"`
}

func loadValueVectors(t *testing.T) map[string][]valueVector {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/values/helpers.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	vectors := map[string][]valueVector{}
	for _, helper := range ValueHelpers {
		var list []valueVector
		if err := json.Unmarshal(doc[helper], &list); err != nil || len(list) == 0 {
			t.Fatalf("%s: no vectors (%v)", helper, err)
		}
		vectors[helper] = list
	}
	return vectors
}

// Each helper, called as a jq function on the input, gives every vector's
// number to the bit, or fails with exactly its error, or, for input that is
// not text, says what it reads. The jq programs are compiled as every rule's
// are, by CompileJQ.
func TestValueHelpersInJQMatchTheSharedVectors(t *testing.T) {
	for helper, vectors := range loadValueVectors(t) {
		code, err := CompileJQ(helper)
		if err != nil {
			t.Fatalf("%s does not compile: %v", helper, err)
		}
		for _, v := range vectors {
			got, ok := code.Run(v.Input, nil, nil, nil).Next()
			if !ok {
				t.Errorf("%s(%#v) gave no value", helper, v.Input)
				continue
			}
			gotErr, failed := got.(error)
			switch {
			case v.Output != nil:
				if failed {
					t.Errorf("%s(%#v) failed: %v; want %v", helper, v.Input, gotErr, *v.Output)
					continue
				}
				var f float64
				switch x := got.(type) {
				case float64:
					f = x
				case int:
					f = float64(x)
				default:
					t.Errorf("%s(%#v) = %#v, not a number", helper, v.Input, got)
					continue
				}
				if math.Float64bits(f) != math.Float64bits(*v.Output) {
					t.Errorf("%s(%#v) = %v, want %v", helper, v.Input, f, *v.Output)
				}
			case v.Error != nil:
				if !failed || gotErr.Error() != *v.Error {
					t.Errorf("%s(%#v) = %v\n want the error %s", helper, v.Input, got, *v.Error)
				}
			case v.TypeError:
				if !failed || !strings.HasPrefix(gotErr.Error(), helper+" reads text") {
					t.Errorf("%s(%#v) = %v, want an error saying what it reads", helper, v.Input, got)
				}
			default:
				t.Errorf("%s: vector %#v says nothing of what to expect", helper, v.Input)
			}
		}
	}
}

// A helper's name that is a gojq builtin's would hide the builtin from every
// expression, or be hidden by it; none is, in the gojq this is built with.
func TestValueHelpersShadowNoBuiltin(t *testing.T) {
	query, err := gojq.Parse("builtins")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	builtins, _ := code.Run(nil).Next()
	list, ok := builtins.([]any)
	if !ok || len(list) < 100 {
		t.Fatalf("builtins = %#v", builtins)
	}
	for _, b := range list {
		name, _, _ := strings.Cut(b.(string), "/")
		if slices.Contains(ValueHelpers, name) {
			t.Errorf("%s is a gojq builtin", name)
		}
	}
	// And each is known to every program CompileJQ makes, in the middle of
	// an expression as well as alone.
	code, err = CompileJQ(`[(.a | parse_bytes), (.b | parse_duration), (.c | parse_time), (.d | parse_percent)]`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := code.Run(map[string]any{"a": "1.00M", "b": "1h2m", "c": "1970-01-01T00:01:00Z", "d": "55.12%"}, nil, nil, nil).Next()
	if want := []any{1048576.0, 3720.0, 60.0, 0.5512}; !slices.Equal(got.([]any), want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// An error of a helper quotes its input as every error of the exporter
// quotes a value of the response: cut to its first 64 bytes, at a character
// boundary, with its length.
func TestValueHelperErrorsCutALongInput(t *testing.T) {
	long := strings.Repeat("é", 100)
	_, err := ParseBytes(long)
	want := `parse_bytes cannot read "` + strings.Repeat("é", 32) + `"... (200 bytes) as a size: `
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %v, want it to start %s", err, want)
	}
}
