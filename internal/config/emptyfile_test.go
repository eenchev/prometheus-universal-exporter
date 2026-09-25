package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An empty configuration or static target file, or one of comments only, is
// refused saying so and naming the file, rather than with a bare "EOF".
func TestAnEmptyFileSaysSo(t *testing.T) {
	for _, body := range []string{"", "# nothing yet\n", "\n\n"} {
		path := testutil.WriteFile(t, "empty.yaml", body)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "configuration file "+path+" is empty") {
			t.Errorf("config %q: %v", body, err)
		}
		if _, err := LoadStaticTargets(path); err == nil || !strings.Contains(err.Error(), "static target file "+path+" is empty") {
			t.Errorf("static targets %q: %v", body, err)
		}
	}
}
