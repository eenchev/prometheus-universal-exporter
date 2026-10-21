//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The playground's collector is read as the loader reads an entry of the
// collectors list: each mistake in the words the loader has for it, at its
// line, whether the YAML decoder, the schema's kinds or the validation finds
// it.
func TestParseCollectorReportsWhatTheLoaderReports(t *testing.T) {
	collector := func(extra string) string {
		return "name: x\nrequest:\n  type: http\ntransform:\n  type: jq\n" + extra + "metrics:\n  - name: up\n    type: gauge\n    expression: .up\n"
	}
	line := regexp.MustCompile(`line (\d+)`)
	for name, doc := range map[string]string{
		"unknown key":       collector("requst: {}\n"),
		"a number for text": collector("decoder:\n  type: 1\n"),
		"a fraction":        collector("max_concurrent_probes: 1.5\n"),
		"a bad expression":  strings.Replace(collector(""), ".up", ".[", 1),
		"a bad name":        strings.Replace(collector(""), "name: x", "name: x-y", 1),
	} {
		_, err := ParseCollector([]byte(doc), nil)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		// The same collector under collectors:, one line further down.
		lines := strings.Split(strings.TrimSuffix(doc, "\n"), "\n")
		for i := range lines {
			lines[i] = "    " + lines[i]
		}
		lines[0] = "  - " + strings.TrimPrefix(lines[0], "    ")
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("collectors:\n"+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, loadErr := Load(path)
		if loadErr == nil {
			t.Fatalf("%s: the loader accepted it", name)
		}
		want := line.ReplaceAllStringFunc(err.Error(), func(at string) string {
			n, _ := strconv.Atoi(at[len("line "):])
			return "line " + strconv.Itoa(n+1)
		})
		if !strings.Contains(loadErr.Error(), want) {
			t.Errorf("%s: the playground says\n  %s\nthe loader\n  %s", name, err, loadErr)
		}
	}
}

// A ${NAME} reference is left as it is written, and refuse sees the
// collector before it is checked, and its refusal is what is returned.
func TestParseCollectorExpandsNothingAndRefusesFirst(t *testing.T) {
	t.Setenv("PARSE_COLLECTOR_SECRET", "s3cr3t")
	doc := "name: x\nrequest:\n  type: http\ntransform:\n  type: jq\nmetrics:\n  - name: up\n    description: ${PARSE_COLLECTOR_SECRET}\n    type: gauge\n    expression: .up\n"
	cfg, err := ParseCollector([]byte(doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Metrics[0].Description; got != "${PARSE_COLLECTOR_SECRET}" {
		t.Errorf("the description is %q", got)
	}
	refused := errors.New("refused")
	broken := strings.Replace(doc, ".up", ".[", 1)
	if _, err := ParseCollector([]byte(broken), func(x *model.Collector) error {
		if x.Name != "x" {
			t.Errorf("refuse was given %q", x.Name)
		}
		return refused
	}); !errors.Is(err, refused) {
		t.Errorf("ParseCollector returned %v, not the refusal", err)
	}
}
