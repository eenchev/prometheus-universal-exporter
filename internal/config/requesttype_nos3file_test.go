//go:build select_request_types && !request_type_s3file

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build of select_request_types without the s3file type refuses a
// configuration with an s3file collector when it loads, saying the type
// exists and that the default build includes it, and the tag that adds it.
func TestABuildWithoutS3FileRefusesAnS3FileCollector(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: objects
    request:
      type: s3file
      root: /exports
      path: app.prom
      region: eu-west-1
    transform:
      type: prometheus
`))
	if err == nil {
		t.Fatal("an s3file collector loaded in a build without the type")
	}
	for _, want := range []string{`collector "objects" uses request.type "s3file", which this build of the exporter does not include`, "the default build includes every type", "request_type_s3file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
