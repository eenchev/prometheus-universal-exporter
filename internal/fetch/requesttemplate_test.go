package fetch

import (
	"strings"
	"testing"
)

// Placeholders in request bodies, header values and query values
// (requesttemplate.go).

func TestTemplatePlaceholdersAreParsed(t *testing.T) {
	for name, tc := range map[string]struct {
		field templateField
		want  string // an error fragment, or "" for none
		count int
	}{
		"json body with braces of its own":  {templateField{"request.body", `{"a":{"b":{{param_x|json}}}}`, "body"}, "", 1},
		"braces that are not a placeholder": {templateField{"request.body", `{{"not": "one"}}`, "body"}, "", 0},
		"a default and a filter":            {templateField{"request.body", `{{param_x:a b|form}}`, "body"}, "", 1},
		"spaces":                            {templateField{"request.body", `{{ param_x }}`, "body"}, "a placeholder with a space after {{", 0},
		"an unknown filter":                 {templateField{"request.body", `{{param_x|yaml}}`, "body"}, `unknown filter "yaml"`, 0},
		"unclosed":                          {templateField{"request.body", `{{param_x`, "body"}, "unclosed placeholder", 0},
		"a bad name":                        {templateField{"request.body", `{{param_x-y}}`, "body"}, "is not a path parameter", 0},
		"a filter in a header":              {templateField{"request.headers.X-Tenant", `{{param_x|json}}`, "header"}, "filters apply only in request.body", 0},
		"a filter in a query":               {templateField{"request.query.q", `{{param_x|raw}}`, "query"}, "filters apply only in request.body", 0},
		"an unexpanded environment default": {templateField{"request.body", `{{param_x:${X}}}`, "body"}, "a default containing a brace", 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.field.parse()
			if tc.want == "" {
				if err != nil || len(got) != tc.count {
					t.Fatalf("got %d placeholders, %v", len(got), err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestTemplateValuesAreEncodedForTheirPlace(t *testing.T) {
	body := func(text string) templateField { return templateField{"request.body", text, "body"} }
	for _, tc := range []struct {
		field  templateField
		params map[string]string
		want   string
	}{
		{body(`{"name": {{param_x|json}}}`), map[string]string{"param_x": `a"b\c` + "\n"}, `{"name": "a\"b\\c\n"}`},
		{body(`{"name": {{param_x|json}}}`), map[string]string{"param_x": "Zoë"}, `{"name": "Zoë"}`},
		{body(`{"limit": {{param_n|number}}}`), map[string]string{"param_n": "-1.5e3"}, `{"limit": -1.5e3}`},
		{body(`a={{param_x|form}}&b=1`), map[string]string{"param_x": "x&y=z é"}, `a=x%26y%3Dz+%C3%A9&b=1`},
		{body(`<t>{{param_x|xml}}</t>`), map[string]string{"param_x": `<a & "b">`}, `<t>&lt;a &amp; &#34;b&#34;&gt;</t>`},
		{body(`raw {{param_x}} and {{param_x|raw}}`), map[string]string{"param_x": `"q"`}, `raw "q" and "q"`},
		{body(`{"region": {{param_r:eu|json}}}`), nil, `{"region": "eu"}`},
		{templateField{"request.headers.X-Tenant", "tenant-{{param_t}}", "header"}, map[string]string{"param_t": "acme"}, "tenant-acme"},
	} {
		got, err := tc.field.render(tc.params)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.field.text, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		field  templateField
		params map[string]string
		want   string
	}{
		{body(`{{param_n|number}}`), map[string]string{"param_n": "12; DROP"}, "must be a number for its |number filter"},
		{body(`{{param_n|number}}`), map[string]string{"param_n": "0x10"}, "must be a number"},
		{templateField{"request.headers.X-Tenant", "{{param_t}}", "header"}, map[string]string{"param_t": "a\r\nX-Admin: 1"}, "contains a control character"},
		{body(`{{param_x}}`), nil, "request.body needs param_x"},
	} {
		if _, err := tc.field.render(tc.params); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s with %v: err=%v, want %q", tc.field.text, tc.params, err, tc.want)
		}
	}
}
