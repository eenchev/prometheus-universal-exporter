//go:build !select_request_types || request_type_http

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Of several keys an http collector's request block may not set, the
// validation names the first in the file (model.RequestConfig.Order),
// where it named the common part's keys first, then those of the other
// types' own in the order of the types, then retry's.
func TestTheValidationRefusesTheFirstKeyInTheFile(t *testing.T) {
	for name, test := range map[string]struct{ keys, want string }{
		"another type's before the common part's":  {"send: x\nroot: /r\n", "request.send, which"},
		"the common part's before another type's":  {"root: /r\nsend: x\n", "request.root, which"},
		"two types' in the reverse of their order": {"statement: select 1\nrpc: a.B/C\n", "request.statement, which"},
		"two types' in their order":                {"rpc: a.B/C\nstatement: select 1\n", "request.rpc, which"},
		"retry's before another type's":            {"retry: {codes: [UNAVAILABLE]}\nsend: x\n", "request.retry.codes, which applies only"},
		"another type's before retry's":            {"send: x\nretry: {codes: [UNAVAILABLE]}\n", "request.send, which"},
	} {
		var c model.Collector
		if err := yaml.Unmarshal([]byte("name: web\nrequest:\n  type: http\n"+indent(test.keys)), &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	// A collector built in code has no order: the order of the code holds.
	c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Root: "/r", Foreign: []string{"send"}}}
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "request.root, which") {
		t.Errorf("built in code: %v", err)
	}
}

// The decoder leaves an unknown key that a key the validation refuses
// comes before in the file to be refused once that key is gone, and
// refuses one that comes first, as before.
func TestAnUnknownKeyAfterARefusedOneIsLeftToTheValidation(t *testing.T) {
	var c model.Collector
	if err := yaml.Unmarshal([]byte("name: web\nrequest:\n  type: http\n  send: x\n  pth: /a\n"), &c); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "request.send, which") {
		t.Errorf("error %v", err)
	}
	err := yaml.Unmarshal([]byte("name: web\nrequest:\n  type: http\n  pth: /a\n  send: x\n"), &c)
	if err == nil || !strings.Contains(err.Error(), "line 4: field pth not found in type model.RequestConfig") {
		t.Errorf("error %v", err)
	}
	// A key the type takes before the unknown one changes nothing.
	err = yaml.Unmarshal([]byte("name: web\nrequest:\n  type: http\n  path: /a\n  pth: /a\n"), &c)
	if err == nil || !strings.Contains(err.Error(), "line 5: field pth not found in type model.RequestConfig") {
		t.Errorf("error %v", err)
	}
}

// A static target's request is checked against its collector's type key by
// key in the order the file has them, retry's keys where retry is.
func TestTheCheckOfATargetsRequestRefusesTheFirstKeyInTheFile(t *testing.T) {
	c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	for request, want := range map[string]string{
		"{username: u, message: m, targets: [a]}":       "request.username, which",
		"{targets: [a], message: m, username: u}":       "request.targets, which",
		"{retry: {codes: [UNAVAILABLE]}, targets: [a]}": "request.retry.codes, which applies only",
		"{targets: [a], retry: {codes: [UNAVAILABLE]}}": "request.targets, which",
	} {
		var target model.StaticTarget
		if err := yaml.Unmarshal([]byte("name: t\ncollector: web\nrequest: "+request+"\n"), &target); err != nil {
			t.Fatal(err)
		}
		if err := CheckTargetRequest(&target, &c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", request, err, want)
		}
	}
}

// indent puts each line of a block two spaces in.
func indent(block string) string {
	return "  " + strings.ReplaceAll(strings.TrimSuffix(block, "\n"), "\n", "\n  ") + "\n"
}
