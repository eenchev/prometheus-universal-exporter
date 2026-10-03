package repository

import (
	"go/parser"
	"go/token"
	"testing"
)

// A rule's time_zone names a zone of the IANA database, which the exporter
// must know wherever it runs: the image has the zone files, and a binary run
// anywhere else — a scratch image, a host without them — has the zones it
// carries, which the main package links in with time/tzdata. The
// time package reads the host's files first and the binary's own when there
// are none, so the import is the whole of it; a test cannot take the host's
// files away to see the difference, and checks the import instead.
func TestTheBinaryCarriesTheTimeZones(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	carried := false
	for _, spec := range parsed.Imports {
		if spec.Path.Value == `"time/tzdata"` && spec.Name != nil && spec.Name.Name == "_" {
			carried = true
		}
	}
	if !carried {
		t.Error(`main.go does not import _ "time/tzdata", so a rule's time_zone is unknown on a host without zone files`)
	}
}
