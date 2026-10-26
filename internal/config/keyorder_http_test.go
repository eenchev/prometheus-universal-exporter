//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Of several keys a request block may not set, the error names the first in
// the file, whatever refuses each: the decoder a key of no type, the
// validation a key of the common part the type does not take, a key of
// another type's own, whichever types they are, or a key of retry. It once
// named them in the order of the code: the common part's keys, then the
// other types' in the order of the types, then retry's, and an unknown key
// whatever came before it.

// requestKeyOrders are request blocks of an http collector, each with
// several keys it may not set, and what the error names.
var requestKeyOrders = map[string]struct{ keys, want string }{
	"another type's before the common part's":  {"      send: x\n      root: /r\n", `collector "c" sets request.send, which does not apply to request.type "http"`},
	"the common part's before another type's":  {"      root: /r\n      send: x\n", `collector "c" sets request.root, which does not apply to request.type "http"`},
	"an unknown key before another type's":     {"      pth: x\n      send: y\n", `line 6: unknown key "pth" in request`},
	"another type's before an unknown key":     {"      send: y\n      pth: x\n", `collector "c" sets request.send, which does not apply to request.type "http"`},
	"another type's before unknown keys":       {"      send: y\n      pth: x\n      zz: 1\n", `collector "c" sets request.send, which does not apply to request.type "http"`},
	"two types' in the reverse of their order": {"      statement: select 1\n      rpc: a.B/C\n", `collector "c" sets request.statement, which does not apply to request.type "http"`},
	"two types' in their order":                {"      rpc: a.B/C\n      statement: select 1\n", `collector "c" sets request.rpc, which does not apply to request.type "http"`},
	"retry's before another type's":            {"      retry: {codes: [UNAVAILABLE]}\n      send: x\n", `collector "c" sets request.retry.codes, which applies only to request.type grpc`},
	"another type's before retry's":            {"      send: x\n      retry: {codes: [UNAVAILABLE]}\n", `collector "c" sets request.send, which does not apply to request.type "http"`},
	"retry's before an unknown key":            {"      retry: {codes: [UNAVAILABLE]}\n      pth: x\n", `collector "c" sets request.retry.codes, which applies only to request.type grpc`},
}

func TestARequestBlockIsRefusedForItsFirstKeyInTheFile(t *testing.T) {
	for name, test := range requestKeyOrders {
		_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector(test.keys, "")))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
		// The keys are refused alike in a collector file.
		dir := t.TempDir()
		testutil.WriteIn(t, dir, "more.yaml", strings.Replace(checkedHTTPCollector(test.keys, ""), "name: c", "name: d", 1))
		_, err = Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [more.yaml]\n"+testutil.CollectorsDocument("own")))
		if want := strings.Replace(test.want, `collector "c"`, `collector "d"`, 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s, in a collector file: error %v, want %q", name, err, want)
		}
	}
	// On one line, the first key is the one further left.
	flow := "collectors:\n  - name: c\n    request: {type: http, path: /x, statement: select 1, rpc: a.B/C}\n    transform:\n      type: jq\n    metrics:\n      - name: m\n        expression: .a\n"
	_, err := Load(testutil.WriteFile(t, "config.yaml", flow))
	if want := `collector "c" sets request.statement, which does not apply to request.type "http"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a flow mapping: error %v, want %q", err, want)
	}
}

// A static target's request block is refused for its first key in the file
// too: a key the collector's type does not let a target set, in place of
// the first in the order of the code, and a key of retry where retry is,
// in place of after every other key.
func TestAStaticTargetsRequestIsRefusedForItsFirstKeyInTheFile(t *testing.T) {
	configuration := checkedHTTPCollector("", "")
	for name, test := range map[string]struct{ request, want string }{
		"in the reverse of the code's order": {"username: u, message: m, targets: [a]", `target "t" sets request.username, which does not apply to collector "c", whose request.type is "http"`},
		"in the code's order":                {"targets: [a], message: m, username: u", `target "t" sets request.targets, which does not apply`},
		"retry's before another key":         {"retry: {codes: [UNAVAILABLE]}, targets: [a]", `target "t" sets request.retry.codes, which applies only to request.type grpc`},
		"another key before retry's":         {"targets: [a], retry: {codes: [UNAVAILABLE]}", `target "t" sets request.targets, which does not apply`},
	} {
		err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: c\n    target: http://127.0.0.1:1\n    request: {"+test.request+"}\n")
		checkRefusal(t, name, err, test.want)
	}
}

// Moving the keys of a static target's request is no change of the target
// to a reload: the order is kept only for the check of the block.
func TestAStaticTargetWhoseRequestKeysMovedIsNotChanged(t *testing.T) {
	read := func(request string) *model.StaticTargetFile {
		t.Helper()
		f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: t\n    collector: c\n    request: {"+request+"}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	before, after := read("path: /a, timeout: 5s"), read("timeout: 5s, path: /a")
	if changes := targetChanges(before, after); len(changes.changed) != 0 {
		t.Errorf("changed: %v", changes.changed)
	}
	if changes := targetChanges(before, read("timeout: 6s, path: /a")); len(changes.changed) != 1 {
		t.Errorf("a changed timeout is no change: %+v", changes)
	}
}
