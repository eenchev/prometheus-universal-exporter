package fetch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A brace before a placeholder is a brace (requesttemplate.go,
// parsePlaceholdersOf): where `{{` opens a placeholder only when param_
// follows it, a `{{` that opens none is passed by one byte, so that the
// second of its braces can be the first of two that do.

// placeholdersSteppingTwo is parsePlaceholdersOf as it was while a `{{`
// that param_ does not follow was passed by both its bytes: in
// `{{{param_a}}` the scan went from the first brace to the third, found no
// `{{` there, and never read the placeholder.
func placeholdersSteppingTwo(where, text string, strict, filters bool, unfiltered string) ([]pathPlaceholder, error) {
	var out []pathPlaceholder
	for offset := 0; ; {
		open := strings.Index(text[offset:], "{{")
		if open < 0 {
			return out, nil
		}
		open += offset
		if !strict {
			after := text[open+2:]
			trimmed := strings.TrimLeft(after, " \t")
			if !strings.HasPrefix(trimmed, PathParamPrefix) {
				offset = open + 2
				continue
			}
			if len(trimmed) != len(after) {
				return nil, fmt.Errorf("%s has a placeholder with a space after {{; write {{%s<name>}} without spaces", where, PathParamPrefix)
			}
		}
		closing := strings.Index(text[open+2:], "}}")
		if closing < 0 {
			return nil, fmt.Errorf("%s has an unclosed placeholder at %q; write {{param_name}} or {{param_name:default}}", where, text[open:])
		}
		closing += open + 2
		inner := text[open+2 : closing]
		filter := ""
		if i := strings.LastIndex(inner, "|"); i >= 0 {
			if !filters {
				return nil, fmt.Errorf("%s placeholder {{%s}} has a filter; %s", where, inner, unfiltered)
			}
			filter = inner[i+1:]
			if !slices.Contains(bodyFilters, filter) {
				return nil, fmt.Errorf("%s placeholder {{%s}} has the unknown filter %q; use one of %s. A default may not contain | unless a filter follows", where, inner, filter, strings.Join(bodyFilters, ", "))
			}
			inner = inner[:i]
		}
		name, def, hasDefault := strings.Cut(inner, ":")
		if !PathParamName.MatchString(name) {
			return nil, fmt.Errorf("%s placeholder {{%s}} is not a path parameter; placeholders are named %s<name>, with letters, digits and underscores, e.g. {{param_tenant}}", where, inner, PathParamPrefix)
		}
		if strings.ContainsAny(def, "{}") {
			return nil, fmt.Errorf("%s placeholder {{%s}} has a default containing a brace; if it is an environment reference, run with --config.expand-env so it is expanded first", where, inner)
		}
		out = append(out, pathPlaceholder{Name: name, Default: def, HasDefault: hasDefault, Filter: filter, start: open, end: closing + 2})
		offset = closing + 2
	}
}

// placeholderPlace is how one kind of place has its text read for
// placeholders: the arguments the parser is given for it.
type placeholderPlace struct {
	name            string
	where           string
	strict, filters bool
	unfiltered      string
}

// placeholderPlaces are the places a placeholder is filled in, each as its
// reader calls the parser: a header value, a query value, a gRPC metadata
// value and a Graphite expression (templateField.parse without filters), a
// body and a gRPC message (with them), a label value (ParseLabelParams),
// and the path, the one place where every `{{` must open a placeholder.
var placeholderPlaces = []placeholderPlace{
	{name: "a header, query or metadata value or a Graphite expression", where: "request.headers.X", unfiltered: "filters apply only in request.body, and a path, header or query value is always encoded one way"},
	{name: "a body or a gRPC message", where: "request.body", filters: true, unfiltered: "filters apply only in request.body, and a path, header or query value is always encoded one way"},
	{name: "a label value", where: "transform.labels.l", unfiltered: labelUnfiltered},
	{name: "the path", where: "request.path", strict: true, unfiltered: "filters apply only in request.body, and a path, header or query value is always encoded one way"},
}

// aBraceBeforeAPlaceholder is the one kind of text the parser reads
// otherwise than it did: an odd number of `{`, three or more, directly
// before param_, blanks allowed between. The scan that went two bytes at a
// time took such a run in pairs from its start and was left with one brace
// before param_, which opens nothing; in an even run its last pair is the
// placeholder's, as it is for the scan that goes one byte at a time.
var aBraceBeforeAPlaceholder = regexp.MustCompile(`(^|[^{])\{(\{\{)+[ \t]*param_`)

// braceTexts are texts made of what the parser looks at: runs of one to
// five opening and closing braces around a placeholder's inside, a word
// that is none, blanks and nothing, alone, after a prefix, before a suffix,
// and two of them in a row.
func braceTexts() []string {
	run := func(brace string) []string {
		var out []string
		for n := 1; n <= 5; n++ {
			out = append(out, strings.Repeat(brace, n))
		}
		return out
	}
	insides := []string{"param_a", " param_a", "\tparam_a ", "param_a:d", "param_a:", "param_a|json", "param_a:d|raw", "param_", "param_a-b", "param_a:{", "x", "not_a_param", " ", ""}
	// The pairs are made of fewer, closed by up to three braces: a
	// placeholder and a word that is none, and, in a run without the race
	// detector, a blank before a placeholder and a filter as well.
	paired := []string{"param_a", "x", " param_a", "param_a|json"}
	paired = paired[:alloctest.UnlessRaced(len(paired), 2)]
	var ones, some []string
	for _, opening := range run("{") {
		for _, closing := range append(run("}"), "") {
			for _, inside := range insides {
				text := opening + inside + closing
				ones = append(ones, text)
				if slices.Contains(paired, inside) && len(closing) <= 3 {
					some = append(some, text)
				}
			}
		}
	}
	texts := slices.Clone(ones)
	for _, text := range ones {
		for _, prefix := range []string{"a", "{", "}}", "{{param_b}}", "a {{ x }} "} {
			texts = append(texts, prefix+text)
		}
		for _, suffix := range []string{"-", "}", "{{param_b:x}}", "{{{param_b}}}", "{{"} {
			texts = append(texts, text+suffix)
		}
	}
	for _, first := range some {
		for _, second := range some {
			texts = append(texts, first+second, first+"/"+second)
		}
	}
	return texts
}

// The parser reads every text as it did but one kind: over texts of braces
// in runs of one to five around placeholders, blanks and other words, in
// each place a placeholder is filled in, the placeholders found and the
// refusal are those of the parser as it was, unless the text has an odd
// run of braces, three or more, directly before param_. Only such texts
// differ, in every place but the path, which is read as it was for all of
// them: there every `{{` opens a placeholder, and a third brace is refused.
func TestOnlyABraceBeforeAPlaceholderIsReadOtherwise(t *testing.T) {
	texts := braceTexts()
	if floor := alloctest.UnlessRaced(17000, 7000); len(texts) < floor {
		t.Fatalf("%d texts, fewer than %d", len(texts), floor)
	}
	braced := make([]bool, len(texts))
	concerned := 0
	for i, text := range texts {
		if braced[i] = aBraceBeforeAPlaceholder.MatchString(text); braced[i] {
			concerned++
		}
	}
	for _, place := range placeholderPlaces {
		differ := 0
		for i, text := range texts {
			now, nowErr := parsePlaceholdersOf(place.where, text, place.strict, place.filters, place.unfiltered)
			was, wasErr := placeholdersSteppingTwo(place.where, text, place.strict, place.filters, place.unfiltered)
			if slices.Equal(now, was) && (nowErr == nil) == (wasErr == nil) && (nowErr == nil || nowErr.Error() == wasErr.Error()) {
				continue
			}
			differ++
			if place.strict || !braced[i] {
				t.Errorf("%s, %q:\n now %+v, %v\n was %+v, %v", place.name, text, now, nowErr, was, wasErr)
			}
		}
		switch {
		case place.strict && differ != 0:
			t.Errorf("%s: %d texts are read otherwise", place.name, differ)
		case !place.strict && (differ == 0 || differ > concerned):
			t.Errorf("%s: %d texts are read otherwise, of %d with a brace before a placeholder", place.name, differ, concerned)
		}
	}
}

// Nothing that is shipped is read otherwise: every line with `{{` in it of
// the examples, the configurations, the chart, the test data and the
// documentation — configurations, schemas, Helm templates with braces of
// their own — is read in each place as the parser that stepped two bytes
// read it, but the lines that show a brace before a placeholder, which the
// documentation of that has.
func TestTheShippedFilesAreReadForPlaceholdersAsTheyWere(t *testing.T) {
	lines, shown := 0, 0
	for _, root := range []string{"../../examples", "../../configs", "../../charts", "../../testdata", "../../docs", "../../README.md"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(content), "\n") {
				if !strings.Contains(line, "{{") {
					continue
				}
				lines++
				braced := aBraceBeforeAPlaceholder.MatchString(line)
				if braced {
					shown++
				}
				for _, place := range placeholderPlaces {
					now, nowErr := parsePlaceholdersOf(place.where, line, place.strict, place.filters, place.unfiltered)
					was, wasErr := placeholdersSteppingTwo(place.where, line, place.strict, place.filters, place.unfiltered)
					same := slices.Equal(now, was) && (nowErr == nil) == (wasErr == nil) && (nowErr == nil || nowErr.Error() == wasErr.Error())
					if !same && (place.strict || !braced) {
						t.Errorf("%s, read as %s, %q:\n now %+v, %v\n was %+v, %v", path, place.name, line, now, nowErr, was, wasErr)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if lines < 100 || shown == 0 {
		t.Errorf("%d lines with braces were read, %d of them showing a brace before a placeholder", lines, shown)
	}
}

// What a brace before a placeholder was read as, and is: in every place but
// the path the text was taken as it is written, with no placeholder in it
// and nothing refused, whatever followed param_ — a header, a query value,
// a body, a message, a metadata value and an expression were sent with the
// placeholder in them, and a label value was exported so, or, holding no
// other placeholder, refused as one that is not filled in. Now the brace is
// text and the placeholder is read, and what is wrong with it is refused as
// it is anywhere else. The path refused the third brace, and does.
func TestABraceBeforeAPlaceholderIsABrace(t *testing.T) {
	type read struct {
		// placeholders are the texts of the placeholders found.
		placeholders []string
		// refused is a part of the refusal, "" for none.
		refused string
	}
	for _, tc := range []struct {
		text      string
		now       read
		nowInBody *read // where a body's reading differs from the others'
	}{
		{text: "{{{param_a}}}", now: read{placeholders: []string{"{{param_a}}"}}},
		{text: `{"a":{{{param_a}}}`, now: read{placeholders: []string{"{{param_a}}"}}},
		{text: "{{{param_a}}}-{{param_b:x}}", now: read{placeholders: []string{"{{param_a}}", "{{param_b:x}}"}}},
		{text: "{{{{{param_a:d}}}}}", now: read{placeholders: []string{"{{param_a:d}}"}}},
		{text: "{{{param_a}},db}", now: read{placeholders: []string{"{{param_a}}"}}},
		{text: "{{{ param_a}}", now: read{refused: "has a placeholder with a space after {{"}},
		{text: "{{{\tparam_a}}", now: read{refused: "has a placeholder with a space after {{"}},
		{text: "{{{param_a", now: read{refused: `has an unclosed placeholder at "{{param_a"`}},
		{text: "{{{param_a-b}}}", now: read{refused: "placeholder {{param_a-b}} is not a path parameter"}},
		{text: `{"a":{{{param_a|json}}}`, now: read{refused: "placeholder {{param_a|json}} has a filter"}, nowInBody: &read{placeholders: []string{"{{param_a|json}}"}}},
	} {
		for _, place := range placeholderPlaces {
			was, wasErr := placeholdersSteppingTwo(place.where, tc.text, place.strict, place.filters, place.unfiltered)
			now, nowErr := parsePlaceholdersOf(place.where, tc.text, place.strict, place.filters, place.unfiltered)
			if place.strict {
				if wasErr == nil || nowErr == nil || nowErr.Error() != wasErr.Error() {
					t.Errorf("%s, %q: %v, and was %v", place.name, tc.text, nowErr, wasErr)
				}
				continue
			}
			// "{{{param_a}}}-{{param_b:x}}" had its second placeholder
			// read, and the one after the brace left as text beside it.
			if wasErr != nil || len(was) != strings.Count(tc.text, "{{param_b") {
				t.Errorf("%s, %q was read as %+v, %v", place.name, tc.text, was, wasErr)
			}
			want := tc.now
			if place.filters && tc.nowInBody != nil {
				want = *tc.nowInBody
			}
			var found []string
			for _, p := range now {
				found = append(found, tc.text[p.start:p.end])
			}
			if !slices.Equal(found, want.placeholders) || (nowErr == nil) != (want.refused == "") || nowErr != nil && !strings.Contains(nowErr.Error(), want.refused) {
				t.Errorf("%s, %q: read as %q, %v\nwant %q, %q", place.name, tc.text, found, nowErr, want.placeholders, want.refused)
			}
		}
	}
}

// Each place writes the value between the braces around it: a header and a
// query value as given, a body under its filter, a label value as given.
// Braces that open nothing stay text, two before two that open a
// placeholder and one before them alike, and `{{ param_x }}` with blanks
// is refused in each place as it was.
func TestAValueIsFilledInBetweenTheBracesAroundIt(t *testing.T) {
	params := map[string]string{"param_x": `a"b`, "param_a": "acme"}
	for _, tc := range []struct {
		field templateField
		want  string
	}{
		{templateField{"request.headers.X-Set", "{{{param_x}}}", "header"}, `{a"b}`},
		{templateField{"request.query.set", "{{{param_x}}}", "query"}, `{a"b}`},
		{templateField{"request.metadata.x-set", "{{{param_x}}}", "header"}, `{a"b}`},
		{templateField{"request.body", `{"a":{{{param_x}}}`, "body"}, `{"a":{a"b}`},
		{templateField{"request.body", `{"a":{{{param_x|json}}}`, "body"}, `{"a":{"a\"b"}`},
		{templateField{"request.body", `{"filter":{{{param_x|raw}}}`, "body"}, `{"filter":{a"b}`},
		{templateField{"request.message", `{"a":{{{param_x|json}}}`, "body"}, `{"a":{"a\"b"}`},
		{templateField{"request.targets[0]", "app.{{{param_a}},db}.cpu", "graphite"}, "app.{acme,db}.cpu"},
		{templateField{"request.headers.X-Set", "{{ {{param_a}}", "header"}, "{{ acme"},
		{templateField{"request.headers.X-Set", "{{{{param_a}}}}", "header"}, "{{acme}}"},
		{templateField{"request.body", "{{{{{param_a}}}}}", "body"}, "{{{acme}}}"},
		{templateField{"request.body", `{{"not": "one"}} {{{x}}} {{{ }}}`, "body"}, `{{"not": "one"}} {{{x}}} {{{ }}}`},
	} {
		if got, err := tc.field.render(params); err != nil || got != tc.want {
			t.Errorf("%s %q: %q, %v\nwant %q", tc.field.where, tc.field.text, got, err, tc.want)
		}
	}
	for _, field := range []templateField{
		{"request.headers.X-Set", "{{ param_x }}", "header"},
		{"request.query.set", "{{ param_x }}", "query"},
		{"request.body", "{{ param_x }}", "body"},
		{"request.body", "{{{ param_x }}}", "body"},
	} {
		if _, err := field.render(params); err == nil || err.Error() != field.where+" has a placeholder with a space after {{; write {{param_<name>}} without spaces" {
			t.Errorf("%s %q: %v", field.where, field.text, err)
		}
	}

	// A label value, of transform.labels and of a rule, alone in the value
	// and beside another placeholder.
	c := labelled(t, labelCollector(
		map[string]string{"set": "{{{param_a}}}-{{param_b:x}}", "one": "{{{param_a}}}", "two": "{{ {{param_a}}", "four": "{{{{param_a}}}}", "none": "{{{x}}} {{ }}"},
		model.LabelRule{Name: "source", Value: "{{{param_a}}}"},
	))
	if fields := TemplatedFields(c); !slices.Equal(fields, []string{"transform.labels.four", "transform.labels.one", "transform.labels.set", "transform.labels.two"}) || !TemplatedRuleLabel(c, 0, 0) {
		t.Errorf("the label values that are filled: %v", fields)
	}
	filled, err := FilledLabels(c, params)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"set": "{acme}-x", "one": "{acme}", "two": "{{ acme", "four": "{{acme}}", "none": "{{{x}}} {{ }}"}; !reflect.DeepEqual(filled.Transform.Labels, want) {
		t.Errorf("transform.labels are filled as %q, want %q", filled.Transform.Labels, want)
	}
	if got := filled.Metrics[0].Labels[0].Value; got != "{acme}" {
		t.Errorf("a rule's label is filled as %q", got)
	}
	// The parameter after the brace is one the collector takes: a probe may
	// give it, and must, as it has no default.
	if unused, err := CheckRequestParams(c, RequestOverrides{Params: map[string]string{"param_a": "acme"}}); err != nil || len(unused) != 0 {
		t.Errorf("a probe that gives the parameter after the brace: unused %v, %v", unused, err)
	}
	if _, err := CheckRequestParams(c, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "needs param_a, which the probe did not supply and which has no default") {
		t.Errorf("a probe that leaves it out: %v", err)
	}
	for _, value := range []string{"{{ param_x }}", "{{{ param_x }}}"} {
		spaced := labelCollector(map[string]string{"set": value})
		if err := readLabels(&spaced); err == nil || !strings.Contains(err.Error(), "transform.labels.set has a placeholder with a space after {{") {
			t.Errorf("a label value %q: %v", value, err)
		}
	}
}
