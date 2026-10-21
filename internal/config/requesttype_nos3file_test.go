//go:build !request_type_s3file

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the opt-in s3file type, the default build among them,
// refuses a configuration with an s3file collector when it loads, saying
// the type exists and is opt-in, and how to build with it: the variable of
// the Makefile and the Dockerfile, and the tag.
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
	for _, want := range []string{`collector "objects" uses request.type "s3file", which this build of the exporter does not include`, "s3file is an opt-in type, which the default build and the published image leave out", "OPT_IN_REQUEST_TYPES=s3file", "request_type_s3file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
