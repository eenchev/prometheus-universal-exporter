package exporter

import (
	"testing"
)

// A version set at build time wins over what Go stamped.
func TestASetVersionWins(t *testing.T) {
	previous := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = previous })
	if info := computeBuildVersion(); info.Version != "9.9.9" {
		t.Fatalf("version=%q, want the one set at build time", info.Version)
	}
}
