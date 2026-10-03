//go:build !select_request_types || request_type_http

package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A file holds one document: one that goes on after --- would have the rest
// ignored without a word, so it is refused naming the line. A leading ---, a
// final --- with nothing after it, and ... ending the document are fine.
func TestAFileHoldsOneDocument(t *testing.T) {
	collector := testutil.CollectorsDocument("demo")
	targets := "interval: 1m\ntargets:\n  - name: one\n    collector: demo\n    target: http://a.invalid\n"
	for name, tc := range map[string]struct {
		body string
		load func(string) error
		want string
	}{
		"configuration":  {collector + "---\notlp:\n  enabled: true\n", func(p string) error { _, err := Load(p); return err }, "line 12: a second document starts here"},
		"static targets": {targets + "---\ninterval: 5m\n", func(p string) error { _, err := LoadStaticTargets(p); return err }, "line 7: a second document starts here"},
		"collector file": {"collector_files: [more.yaml]\n", func(p string) error {
			testutil.WriteIn(t, filepath.Dir(p), "more.yaml", collector+"---\ncollectors: []\n")
			_, err := Load(p)
			return err
		}, "collector file"},
	} {
		t.Run(name, func(t *testing.T) {
			path := testutil.WriteIn(t, t.TempDir(), "file.yaml", tc.body)
			if err := tc.load(path); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "a second document starts here") {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	for name, body := range map[string]string{
		"a leading ---":           "---\n" + collector,
		"a final ---":             collector + "---\n",
		"an end marker":           collector + "...\n",
		"a final --- and comment": collector + "---\n# nothing more\n",
	} {
		path := testutil.WriteIn(t, t.TempDir(), "config.yaml", body)
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
