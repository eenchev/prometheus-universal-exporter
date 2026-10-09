package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// A reference is expanded where the document holds it as a value, and the
// value is written back so YAML reads exactly it (expandenv.go): whatever the
// variable holds, it cannot become a comment, an anchor, an alias or more of
// the document.

func loadExpandedTargets(t *testing.T, body string) (string, error) {
	t.Helper()
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", body)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := expandEnvironment(path, raw, staticTargetsEnvFlag)
	return string(out), err
}

func TestValuesAreExpandedAsValues(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "abc #def")
	t.Setenv("DEMO_STAR", "*star")
	t.Setenv("DEMO_AMP", "&anchor [x] {y}")
	t.Setenv("DEMO_QUOTES", `say "hi" \ 'there'`)
	t.Setenv("DEMO_HEADER", "X-Tenant")
	t.Setenv("DEMO_ATTEMPTS", "3")
	t.Setenv("DEMO_URL", "http://api.example:8080/status?a=1&b=2")
	t.Setenv("DEMO_REGION", "eu")
	t.Setenv("DEMO_EMPTY", "")
	body := `# A comment may name ${DEMO_UNSET_IN_A_COMMENT} without it being set.
interval: 1m
targets:
  - name: one
    collector: text
    target: ${DEMO_URL}
    request:
      bearer_token: ${DEMO_TOKEN}
      body: "${DEMO_QUOTES} and $${LITERAL}"
      retry:
        attempts: ${DEMO_ATTEMPTS}
      headers:
        ${DEMO_HEADER}: '${DEMO_AMP}'
        X-Star: Bearer ${DEMO_STAR}
    labels:
      region: ${DEMO_REGION} # a comment after the value
      empty: "${DEMO_EMPTY}"
      folded: "${DEMO_REGION}-
        west"
`
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", body)
	file, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	target := file.Targets[0]
	for name, check := range map[string][2]string{
		"target":       {target.Target, "http://api.example:8080/status?a=1&b=2"},
		"bearer_token": {target.Request.BearerToken, "abc #def"},
		"body":         {target.Request.Body, `say "hi" \ 'there' and ${LITERAL}`},
		"header name":  {target.Request.Headers["X-Tenant"], "&anchor [x] {y}"},
		"leading star": {target.Request.Headers["X-Star"], "Bearer *star"},
		"region":       {target.Labels["region"], "eu"},
		"empty":        {target.Labels["empty"], ""},
		"multi-line":   {target.Labels["folded"], "eu- west"},
	} {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}
	if target.Request.Retry == nil || target.Request.Retry.Attempts == nil || *target.Request.Retry.Attempts != 3 {
		t.Errorf("a number stays a number: retry=%+v", target.Request.Retry)
	}

	// A leading star or ampersand on its own, unquoted in the file.
	for _, value := range []string{"*star", "&anchor", "!tag", "|pipe", ">gt", "@at", "%pct", "- dash", "key: value", "[1, 2]"} {
		t.Setenv("DEMO_ANY", value)
		out, err := loadExpandedTargets(t, "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n    labels:\n      any: ${DEMO_ANY}\n")
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", out)
		file, err := LoadStaticTargets(path)
		if err != nil || file.Targets[0].Labels["any"] != value {
			t.Errorf("%q read back as %v, %v:\n%s", value, file, err, out)
		}
	}
}

// Rewriting a value keeps the lines of the document, so an error after
// expansion names the line of the file.
func TestExpansionKeepsTheLineNumbers(t *testing.T) {
	t.Setenv("DEMO_REGION", "a region\nwith a line break")
	body := "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n    labels:\n      region: \"${DEMO_REGION}\n        and more\"\n    unknown_key: x\n"
	out, err := loadExpandedTargets(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != strings.Count(body, "\n") {
		t.Fatalf("the document has %d lines, was %d:\n%s", strings.Count(out, "\n"), strings.Count(body, "\n"), out)
	}
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", body)
	if _, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion()); err == nil || !strings.Contains(err.Error(), `line 9: unknown key "unknown_key"`) {
		t.Fatalf("err=%v, want the file's own line 9", err)
	}
}

// A reference in a block value (| or >) is expanded in its lines; a value with
// a line break cannot go there, and the error says to quote it instead.
func TestBlockValuesExpandInTheirLines(t *testing.T) {
	t.Setenv("DEMO_SERVICE", "checkout #1")
	body := "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n    request:\n      body: |\n        {\"service\": \"${DEMO_SERVICE}\"}\n        # not a comment in a block\n    labels:\n      region: eu\n"
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", body)
	file, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Targets[0].Request.Body; got != "{\"service\": \"checkout #1\"}\n# not a comment in a block\n" {
		t.Fatalf("body=%q", got)
	}
	if file.Targets[0].Labels["region"] != "eu" {
		t.Fatal("the block swallowed what follows it")
	}
	t.Setenv("DEMO_SERVICE", "two\nlines")
	if _, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion()); err == nil || !strings.Contains(err.Error(), "line 7: an environment variable with a line break cannot go into a block value") {
		t.Fatalf("err=%v", err)
	}
}

// An unquoted value folded over lines cannot be rewritten in place, and is
// refused saying how to write it; a document that is not YAML is left to
// the decoder, which says so in its own terms.
func TestWhatCannotBeExpandedInPlaceIsRefused(t *testing.T) {
	t.Setenv("DEMO_REGION", "eu")
	if _, err := loadExpandedTargets(t, "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n    labels:\n      region: ${DEMO_REGION}\n        west\n"); err == nil || !strings.Contains(err.Error(), "line 7: an environment reference is in an unquoted value that spans lines") {
		t.Fatalf("err=%v", err)
	}
	if _, err := loadExpandedTargets(t, "labels: !!str ${DEMO_REGION}\n"); err == nil || !strings.Contains(err.Error(), "explicit tag") {
		t.Fatalf("err=%v", err)
	}
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", "targets: [\n  ${DEMO_REGION}\n")
	if _, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion()); err == nil || strings.Contains(err.Error(), "environment") {
		t.Fatalf("err=%v, want the YAML error", err)
	}
}

// Where the rewrite was fooled: an anchor before the value, a bare reference
// in a flow collection, and values that do not read back unquoted in every
// place — a lone dash, an empty value in a flow collection or as a key.
func TestExpansionInEveryPlaceAValueStands(t *testing.T) {
	t.Setenv("DEMO_PORT", "8080")
	t.Setenv("DEMO_NAME", "eu west")
	t.Setenv("DEMO_DASH", "-")
	t.Setenv("DEMO_EMPTY", "")
	t.Setenv("DEMO_KEY", "X-Tenant")
	for name, tc := range map[string]struct{ document, want string }{
		"anchored plain":       {"a: &tok ${DEMO_NAME}\nb: *tok\n", `{"a":"eu west","b":"eu west"}`},
		"anchored quoted":      {"a: &tok \"${DEMO_NAME}\"\nb: *tok\n", `{"a":"eu west","b":"eu west"}`},
		"flow sequence":        {"a: [${DEMO_PORT}, ${DEMO_NAME}, b]\n", `{"a":[8080,"eu west","b"]}`},
		"flow mapping":         {"a: {k: ${DEMO_NAME}, n: ${DEMO_PORT}}\n", `{"a":{"k":"eu west","n":8080}}`},
		"flow key":             {"a: {${DEMO_KEY}: v}\n", `{"a":{"X-Tenant":"v"}}`},
		"flow with quotes too": {"a: [\"${DEMO_NAME}\", x]\nb: ${DEMO_PORT}\n", `{"a":["eu west","x"],"b":8080}`},
		"flow, part of value":  {"a: {Authorization: Bearer ${DEMO_KEY}, n: p${DEMO_PORT}}\n", `{"a":{"Authorization":"Bearer X-Tenant","n":"p8080"}}`},
		"flow, after a quote":  {"a: {X-Name: \"x\", X-Token: ${DEMO_NAME}}\n", `{"a":{"X-Name":"x","X-Token":"eu west"}}`},
		"flow after a comment": {"a: 1 # {\nb: [x, ${DEMO_PORT}] # ${DEMO_UNSET}\n", `{"a":1,"b":["x",8080]}`},
		"flow over lines":      {"a: [\n  ${DEMO_PORT},\n  ${DEMO_NAME}\n]\n", `{"a":[8080,"eu west"]}`},
		"flow, a dollar kept":  {"a: [$${DEMO_PORT}, ${DEMO_PORT}]\n", ""},
		"lone dash":            {"a: ${DEMO_DASH}\n", `{"a":"-"}`},
		"lone dash in a list":  {"a:\n  - ${DEMO_DASH}\n", `{"a":["-"]}`},
		"empty in a sequence":  {"a: [${DEMO_EMPTY}, b]\n", `{"a":["","b"]}`},
		"empty in a mapping":   {"a: {k: ${DEMO_EMPTY}}\n", `{"a":{"k":""}}`},
		"empty key":            {"${DEMO_EMPTY}: 1\n", `{"":1}`},
		"empty block value":    {"a: ${DEMO_EMPTY}\n", `{"a":""}`},
		"empty in a list":      {"a:\n  - ${DEMO_EMPTY}\n", `{"a":[""]}`},
		"key starting a line":  {"${DEMO_KEY}: 1\n", `{"X-Tenant":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := expandEnvironment("test.yaml", []byte(tc.document), staticTargetsEnvFlag)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				// Not YAML with the reference expanded either: $$ leaves a
				// brace in a flow collection, so the file is handed on as
				// it is, for the decoder's own error.
				if string(out) != tc.document {
					t.Fatalf("the document was changed:\n%s", out)
				}
				return
			}
			var value any
			if err := yaml.Unmarshal(out, &value); err != nil {
				t.Fatalf("%v:\n%s", err, out)
			}
			got, _ := json.Marshal(value)
			if string(got) != tc.want {
				t.Fatalf("read back %s, want %s, from:\n%s", got, tc.want, out)
			}
			if strings.Count(string(out), "\n") != strings.Count(tc.document, "\n") {
				t.Fatalf("the lines changed:\n%s", out)
			}
		})
	}
}

// A bare reference in a flow collection makes the document one YAML cannot
// read as written. Reading it all the same must not change any other value:
// a reference alone on a line of a block value, after a comma in an unquoted
// description or in a JSON body expands to the variable's value, without the
// quotes that once helped the flow collection parse.
func TestABareFlowReferenceChangesNoOtherValue(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "tok")
	t.Setenv("DEMO_TENANT", "acme")
	rest := "body: |\n  tenant: ${DEMO_TENANT}\n  ids: [${DEMO_TENANT}, other]\n  {\"tenant\": ${DEMO_TENANT}}\ndescription: queue depth, ${DEMO_TENANT}\nlist:\n  - ${DEMO_TENANT}\n"
	want := map[string]any{
		"body":        "tenant: acme\nids: [acme, other]\n{\"tenant\": acme}\n",
		"description": "queue depth, acme",
		"list":        []any{"acme"},
	}
	for _, headers := range []string{
		`headers: {X-Token: "${DEMO_TOKEN}"}`,
		`headers: {X-Token: ${DEMO_TOKEN}}`,
		`headers: {Authorization: Bearer ${DEMO_TOKEN}}`,
	} {
		out, err := expandEnvironment("test.yaml", []byte(headers+"\n"+rest), configEnvFlag)
		if err != nil {
			t.Fatalf("%s: %v", headers, err)
		}
		var got map[string]any
		if err := yaml.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: %v:\n%s", headers, err, out)
		}
		for key, value := range want {
			if a, b := mustJSON(t, got[key]), mustJSON(t, value); a != b {
				t.Errorf("%s: %s = %s, want %s", headers, key, a, b)
			}
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// The braces of a reference are masked while a document with a bare flow
// reference is read. Text that already looks like a masked reference is not
// one: it stays as written, and the references beside it still expand.
func TestTextThatLooksLikeAMaskedReferenceIsKept(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "tok")
	document := "a: [${DEMO_TOKEN}, $<DEMO_TOKEN>, $(DEMO_TOKEN)]\nb: say $<DEMO_TOKEN> and ${DEMO_TOKEN}\n"
	out, err := expandEnvironment("test.yaml", []byte(document), configEnvFlag)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if text := mustJSON(t, got); text != `{"a":["tok","$<DEMO_TOKEN>","$(DEMO_TOKEN)"],"b":"say $<DEMO_TOKEN> and tok"}` {
		t.Fatalf("read back %s from:\n%s", text, out)
	}
}

// A block value whose header says how far it is indented, as in |2, may start
// with a line indented further than the rest. Every line of it is expanded,
// in a mapping, in a list, and as the value of a key written after a dash;
// the keys that follow it are still keys.
func TestABlockValueWithAnIndentationIndicatorIsExpandedInFull(t *testing.T) {
	t.Setenv("DEMO_TENANT", "acme")
	for name, tc := range map[string]struct{ document, want string }{
		"in a mapping":       {"a:\n  body: |2\n        first ${DEMO_TENANT}\n    second ${DEMO_TENANT}\n  next: ${DEMO_TENANT}\n", `{"a":{"body":"    first acme\nsecond acme\n","next":"acme"}}`},
		"folded and chomped": {"a:\n  body: >-2\n        first ${DEMO_TENANT}\n    second ${DEMO_TENANT}\nnext: ${DEMO_TENANT}\n", `{"a":{"body":"    first acme\nsecond acme"},"next":"acme"}`},
		"in a list":          {"a:\n  - |1\n      first ${DEMO_TENANT}\n   second ${DEMO_TENANT}\n  - ${DEMO_TENANT}\n", `{"a":["   first acme\nsecond acme\n","acme"]}`},
		"after a dash":       {"- body: |2\n       first ${DEMO_TENANT}\n     second ${DEMO_TENANT}\n  next: ${DEMO_TENANT}\n", `[{"body":"   first acme\n second acme\n","next":"acme"}]`},
		"at the top":         {"--- |1\n   first ${DEMO_TENANT}\n second ${DEMO_TENANT}\n", `"  first acme\nsecond acme\n"`},
		"no indicator":       {"a:\n  body: |-\n    first ${DEMO_TENANT}\n      second ${DEMO_TENANT}\n  next: ${DEMO_TENANT}\n", `{"a":{"body":"first acme\n  second acme","next":"acme"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := expandEnvironment("test.yaml", []byte(tc.document), configEnvFlag)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := yaml.Unmarshal(out, &value); err != nil {
				t.Fatalf("%v:\n%s", err, out)
			}
			if got := mustJSON(t, value); got != tc.want {
				t.Fatalf("read back %s, want %s, from:\n%s", got, tc.want, out)
			}
		})
	}
}
