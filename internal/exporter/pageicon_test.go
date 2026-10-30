package exporter

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The pages' icon is the project's logo itself: logo.svg, which go:embed
// reads, is a copy of docs/logo.svg, which go:embed cannot reach from here,
// and the two may not drift apart.
func TestThePagesIconIsTheProjectsLogo(t *testing.T) {
	logo, err := os.ReadFile(filepath.Join("..", "..", "docs", "logo.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(logoSVG, logo) {
		t.Error("internal/exporter/logo.svg differs from docs/logo.svg; copy docs/logo.svg over it")
	}
}
