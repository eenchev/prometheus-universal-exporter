package expr

import (
	"encoding/base32"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/itchyny/gojq"
)

// JQAdded are the names jq defines and gojq leaves to whoever embeds it, or
// does not have at all, so that a program written for jq compiles here too.
// The formats @base32 and @base32d are added beside them (jqFormats).
var JQAdded = []string{"debug", "stderr", "input_line_number", "input_filename", "toarray", "have_literal_numbers", "have_decnum"}

// jqAddedFunctions registers JQAdded with a jq program, and the functions
// the added formats are rewritten into. debug and stderr hand their input on
// and, in a program every scrape runs, write nothing: an exporter has no
// terminal to write a message to, and what a target answered is not for its
// log. A debug probe's programs are compiled with functions that tell its
// report (CompileJQTold). A program has one input, which
// is of no file, so its line number is 0 and its file's name null, as jq's
// are before anything was read. A whole number keeps every digit
// (model.Normalize), which is what have_literal_numbers asks after, and
// there are no decimal numbers.
var jqAddedFunctions = []gojq.CompilerOption{
	gojq.WithFunction("input_line_number", 0, 0, func(any, []any) any { return 0 }),
	gojq.WithFunction("input_filename", 0, 0, func(any, []any) any { return nil }),
	gojq.WithFunction("have_literal_numbers", 0, 0, func(any, []any) any { return true }),
	gojq.WithFunction("have_decnum", 0, 0, func(any, []any) any { return false }),
	gojq.WithFunction("toarray", 0, 0, func(v any, _ []any) any {
		if _, ok := v.([]any); ok {
			return v
		}
		return []any{v}
	}),
	gojq.WithFunction(jqFormats["@base32"], 0, 0, func(v any, _ []any) any {
		s, _ := v.(string)
		return base32.StdEncoding.EncodeToString([]byte(s))
	}),
	gojq.WithFunction(jqFormats["@base32d"], 0, 0, func(v any, _ []any) any {
		s, _ := v.(string)
		// jq decodes text whose padding was left off, as its @base64d does.
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(s, "="))
		if err != nil {
			return errors.New("@base32d cannot decode its input: it is not base32")
		}
		return strings.ToValidUTF8(string(decoded), "�")
	}),
}

// jqQuiet are debug and stderr as every scrape's programs have them.
var jqQuiet = jqTelling(func(string, any) {})

// jqTelling are debug and stderr as functions that hand their input on and
// tell it to told, with the function's name.
func jqTelling(told func(function string, value any)) []gojq.CompilerOption {
	return []gojq.CompilerOption{
		gojq.WithFunction("debug", 0, 0, func(v any, _ []any) any { told("debug", v); return v }),
		gojq.WithFunction("stderr", 0, 0, func(v any, _ []any) any { told("stderr", v); return v }),
	}
}

// CompileJQTold compiles expression as CompileJQ does, but for its debug
// and stderr, which tell told each value they are given: debug its input,
// debug(message) the message, stderr its input. The program is not kept: it
// is for a debug probe, whose report shows what was told.
func CompileJQTold(expression string, told func(function string, value any)) (*gojq.Code, error) {
	query, err := gojq.Parse(expression)
	if err != nil {
		return nil, err
	}
	addJQFormats(expression, query)
	return gojq.Compile(query, slices.Concat(jqSharedOptions, jqTelling(told))...)
}

// jqFormats are the formats of jq that gojq does not know, each with the
// function that does its work. gojq compiles a format it does not know into
// a call that fails when it runs, and a custom function cannot stand in for
// that call, so addJQFormats rewrites the format where it is written.
var jqFormats = map[string]string{"@base32": "_tobase32", "@base32d": "_tobase32d"}

// addJQFormats rewrites each format of jqFormats in a parsed program into
// its function: @base32 into (tostring | _tobase32), and @base32 "a\(.b)"
// into the string with each interpolated part piped through the same, as
// gojq compiles the formats it knows (compileFormat, compileString). Text
// between the parts is left as written, as every format leaves it.
func addJQFormats(expression string, query *gojq.Query) {
	if !strings.Contains(expression, "@base32") {
		return
	}
	rewriteJQFormats(reflect.ValueOf(query))
}

var jqTermType = reflect.TypeOf((*gojq.Term)(nil))

// rewriteJQFormats walks every node of a parsed program below v, the inner
// terms before the term that holds them.
func rewriteJQFormats(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		rewriteJQFormats(v.Elem())
		if v.Type() == jqTermType && v.CanInterface() {
			rewriteJQFormat(v.Interface().(*gojq.Term))
		}
	case reflect.Struct:
		for i := range v.NumField() {
			rewriteJQFormats(v.Field(i))
		}
	case reflect.Slice:
		for i := range v.Len() {
			rewriteJQFormats(v.Index(i))
		}
	}
}

func rewriteJQFormat(term *gojq.Term) {
	name, ok := jqFormats[term.Format]
	if term.Type != gojq.TermTypeFormat || !ok {
		return
	}
	format := func() *gojq.Query {
		call := func(name string) *gojq.Query {
			return &gojq.Query{Term: &gojq.Term{Type: gojq.TermTypeFunc, Func: &gojq.Func{Name: name}}}
		}
		return &gojq.Query{Left: call("tostring"), Op: gojq.OpPipe, Right: call(name)}
	}
	term.Format = ""
	if term.Str == nil {
		term.Type, term.Query = gojq.TermTypeQuery, format()
		return
	}
	term.Type = gojq.TermTypeString
	for i, part := range term.Str.Queries {
		if part.Term != nil && part.Term.Str != nil {
			continue
		}
		piped := &gojq.Query{Left: part, Op: gojq.OpPipe, Right: format()}
		term.Str.Queries[i] = &gojq.Query{Term: &gojq.Term{Type: gojq.TermTypeQuery, Query: piped}}
	}
}
