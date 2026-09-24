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
		"lone dash":            {"a: ${DEMO_DASH}\n", `{"a":"-"}`},
		"lone dash in a list":  {"a:\n  - ${DEMO_DASH}\n", `{"a":["-"]}`},
		"empty in a sequence":  {"a: [${DEMO_EMPTY}, b]\n", `{"a":["","b"]}`},
		"empty in a mapping":   {"a: {k: ${DEMO_EMPTY}}\n", `{"a":{"k":""}}`},
		"empty key":            {"${DEMO_EMPTY}: 1\n", `{"":1}`},
		"empty block value":    {"a: ${DEMO_EMPTY}\n", `{"a":null}`},
		"key starting a line":  {"${DEMO_KEY}: 1\n", `{"X-Tenant":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := expandEnvironment("test.yaml", []byte(tc.document), staticTargetsEnvFlag)
			if err != nil {
				t.Fatal(err)
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
