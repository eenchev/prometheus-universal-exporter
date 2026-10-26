package model

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// A request block, a collector's or a static target's, keeps the order the
// file has its keys in, and those of each mapping it holds directly, for
// the checks that refuse several of them to name the first.
func TestARequestBlockKeepsTheOrderOfItsKeys(t *testing.T) {
	for document, want := range map[string][]string{
		"request:\n  type: http\n  path: /x\n  retry: {codes: [A], attempts: 2}\n  root: /r\n": {"type", "path", "retry", "retry.codes", "retry.attempts", "root"},
		"request: {root: /r, type: http, path: /x}\n":                                          {"root", "type", "path"},
		// A key a merge key brings in is where its anchor has it.
		"x: &base {path: /x}\nrequest:\n  type: http\n  root: /r\n  <<: *base\n": {"path", "type", "root"},
	} {
		var collector struct {
			Request RequestConfig `yaml:"request"`
		}
		if err := yaml.Unmarshal([]byte(document), &collector); err != nil {
			t.Fatalf("%q: %v", document, err)
		}
		if !slices.Equal(collector.Request.Order, want) {
			t.Errorf("%q: order %v, want %v", document, collector.Request.Order, want)
		}
		var target struct {
			Request TargetRequestConfig `yaml:"request"`
		}
		if err := yaml.Unmarshal([]byte(document), &target); err == nil && !slices.Equal(target.Request.Order, want) {
			t.Errorf("%q: a target's order %v, want %v", document, target.Request.Order, want)
		}
	}
	if got := InFileOrder([]string{"root", "path", "built"}, []string{"type", "path", "root"}); !slices.Equal(got, []string{"path", "root", "built"}) {
		t.Errorf("InFileOrder: %v", got)
	}
}
