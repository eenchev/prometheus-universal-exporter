//go:build request_type_s3file

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An s3file collector that sets no decoder decodes each object by its
// name's extension, as a file is, and the warning at load says so.
func TestAnS3FileCollectorWithoutADecoderIsWarnedOfTheExtension(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: objects
    request:
      type: s3file
      root: /exports
      path: status.json
    transform:
      type: jq
    metrics:
      - name: v
        expression: .value
`))
	if err != nil {
		t.Fatal(err)
	}
	want := `collector "objects" sets no decoder.type, so it decodes by the extension of each file`
	if !strings.HasPrefix(strings.Join(cfg.Warnings, "\n"), want) {
		t.Errorf("warnings %q, want one starting %q", cfg.Warnings, want)
	}
}
