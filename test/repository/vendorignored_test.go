package repository

import (
	"os"
	"strings"
	"testing"
)

// The project builds from the module proxy against go.sum and keeps no vendor
// directory. One made locally with `go mod vendor`, to build offline, is kept
// out of commits and out of the image's build context, where go build would
// otherwise prefer it to the modules go.sum pins.
func TestALocalVendorDirectoryIsNeitherCommittedNorBuiltIntoTheImage(t *testing.T) {
	for file, line := range map[string]string{".gitignore": "/vendor/", ".dockerignore": "vendor/"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains("\n"+string(raw)+"\n", "\n"+line+"\n") {
			t.Errorf("%s must hold the line %q, or a local vendor directory lands in %s", file, line, map[string]string{".gitignore": "a commit", ".dockerignore": "the image build"}[file])
		}
	}
}
