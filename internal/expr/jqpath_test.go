package expr

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"
)

// runJQ is what gojq itself gives for input: every value, or the first error.
func runJQ(t *testing.T, program *JQProgram, input any) ([]any, error) {
	t.Helper()
	var values []any
	iterator := program.Code.Run(input, input, nil, nil)
	for {
		value, ok := iterator.Next()
		if !ok {
			return values, nil
		}
		if err, isErr := value.(error); isErr {
			return values, err
		}
		values = append(values, value)
	}
}

// A field path looked up directly gives exactly what gojq gives, and where
// gojq would fail, or the expression is not a field path, the lookup declines
// and gojq runs.
func TestJQFieldPathsAgreeWithGojq(t *testing.T) {
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	inputs := []any{
		nil, true, 1, 2.5, "text", huge, json.Number("12"),
		[]any{1, 2}, []any{},
		map[string]any{},
		map[string]any{"a": nil},
		map[string]any{"a": 1, "b": "x"},
		map[string]any{"a": huge, "n": json.Number("1e400")},
		map[string]any{"a": map[string]any{"b": map[string]any{"c": 7}}},
		map[string]any{"a": map[string]any{"b": nil}},
		map[string]any{"a": map[string]any{"b": []any{1}}},
		map[string]any{"a": map[string]any{"b": "text"}},
		map[string]any{"a": []any{map[string]any{"b": 1}}},
		map[string]any{"a": "text"},
		map[string]any{"a key": map[string]any{"b": 2, "b c": 3}, "_x1": false},
	}
	for _, test := range []struct {
		expression string
		path       bool
	}{
		{".", true},
		{".a", true},
		{".a.b", true},
		{".a.b.c", true},
		{".a.b.c.d", true},
		{"._x1", true},
		{".missing", true},
		{`."a key"`, true},
		{`."a key".b`, true},
		{`.a."b"`, true},
		{`."a key"."b c"`, true},
		{".a[0]", false},
		{".a?", false},
		{".a.b?", false},
		{".[]", false},
		{".a[]", false},
		{".a | .b", false},
		{".a // 1", false},
		{`."\(1)"`, false},
		{`.["a"]`, false},
		{".a[1:]", false},
		{"$root.a", false},
		{"1", false},
		{"..", false},
	} {
		program, err := CompileJQProgram(test.expression)
		if err != nil {
			t.Fatalf("%s: %v", test.expression, err)
		}
		if program.isPath != test.path {
			t.Errorf("%s: recognised as a field path: %v, want %v", test.expression, program.isPath, test.path)
		}
		for _, input := range inputs {
			want, wantErr := runJQ(t, program, input)
			got, ok := program.Lookup(input)
			if !ok {
				if test.path && wantErr == nil {
					if _, isMap := input.(map[string]any); isMap || input == nil {
						t.Errorf("%s on %#v: the lookup declined what gojq evaluates to %v", test.expression, input, want)
					}
				}
				continue
			}
			if wantErr != nil {
				t.Errorf("%s on %#v: the lookup gave %#v where gojq fails with %v", test.expression, input, got, wantErr)
				continue
			}
			if len(want) != 1 || !reflect.DeepEqual(got, want[0]) {
				t.Errorf("%s on %#v: the lookup gave %#v, gojq %#v", test.expression, input, got, want)
			}
		}
	}
}
