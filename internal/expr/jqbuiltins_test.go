package expr

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/itchyny/gojq"
)

// A program written for jq uses names gojq leaves to whoever embeds it, and
// two formats gojq does not have; each compiles here and gives what jq gives.
func TestTheNamesOfJQThatGojqLeavesOutAreDefined(t *testing.T) {
	for program, want := range map[string]any{
		`.a | debug`:                             1,
		`.a | debug("at a")`:                     1,
		`.a | debug("at \(.)") | . + 1`:          2,
		`.a | stderr`:                            1,
		`input_line_number`:                      0,
		`input_filename`:                         nil,
		`[have_literal_numbers, have_decnum]`:    []any{true, false},
		`.a | toarray`:                           []any{1},
		`.list | toarray`:                        []any{"x"},
		`null | toarray`:                         []any{nil},
		`.text | @base32`:                        "NBSWY3DP",
		`.text | @base32 | @base32d`:             "hello",
		`"NBSWY3DPEE======" | @base32d`:          "hello!",
		`"NBSWY3DPEE" | @base32d`:                "hello!",
		`.a | @base32`:                           "GE======",
		`@base32 "id=\(.text)&n=\(.a)"`:          "id=NBSWY3DP&n=GE======",
		`@base32d "\(.encoded) world"`:           "hello world",
		`[.list[] | @base32] | first`:            "PA======",
		`{k: (.text | @base32)}.k`:               "NBSWY3DP",
		`def f: @base32; .text | f`:              "NBSWY3DP",
		`.text | @base32 "\(@base32 "\(.)")"`:    "JZBFGV2ZGNCFA===",
		`try ("not base32!" | @base32d) catch .`: "@base32d cannot decode its input: it is not base32",
		`.text | ascii_upcase | @base64`:         "SEVMTE8=",
		`"@base32" | length`:                     7,
	} {
		code, err := CompileJQ(program)
		if err != nil {
			t.Errorf("%s: %v", program, err)
			continue
		}
		input := map[string]any{"a": 1, "list": []any{"x"}, "text": "hello", "encoded": "NBSWY3DP"}
		got, _ := code.Run(input, JQBindings{}.Values(input)...).Next()
		if err, ok := got.(error); ok {
			t.Errorf("%s: %v", program, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", program, got, want)
		}
	}
}

// None of the added names is one gojq has by now: a custom function does not
// replace a builtin, so one that became a builtin would be dead code here.
func TestTheAddedNamesAreNoBuiltinOfGojq(t *testing.T) {
	query, err := gojq.Parse("builtins")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	builtins, _ := code.Run(nil).Next()
	for _, b := range builtins.([]any) {
		if name, _, _ := strings.Cut(b.(string), "/"); slices.Contains(JQAdded, name) && b != "debug/1" {
			t.Errorf("%s is a gojq builtin", b)
		}
	}
	for format := range jqFormats {
		query, err := gojq.Parse(`"a" | ` + format)
		if err != nil {
			t.Fatal(err)
		}
		code, err := gojq.Compile(query)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := code.Run(nil).Next(); !isError(got) {
			t.Errorf("gojq knows %s by now: %#v", format, got)
		}
	}
}

func isError(v any) bool {
	_, ok := v.(error)
	return ok
}
