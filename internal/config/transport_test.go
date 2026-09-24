package config

import (
	"os"
	"strings"
	"testing"
)

func TestRedirectPolicyIsNoLongerAccepted(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	document := "collectors:\n  - name: legacy\n    request:\n      redirect_policy: none\n" +
		"    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	// follow_redirects replaced it, and the strict decoder makes the removal
	// loud instead of silently changing how a collector behaves.
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "redirect_policy") {
		t.Fatalf("Load() error=%v, want the removed field to be rejected", err)
	}
}
