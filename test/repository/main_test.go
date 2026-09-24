// Package repository holds the tests that keep the repository consistent with
// itself: the Helm chart, the workflows, the Dockerfile, the documentation,
// the committed schemas and the examples against the code, and the layering
// of the internal packages. They read files rather than exercise one package,
// so they live apart from the code; the command line's own tests stay with
// package main at the repository root.
package repository

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// TestMain runs the tests from the repository root, so every path they name
// is the one a contributor sees.
func TestMain(m *testing.M) {
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// exporterHelp is the exporter's -h output, from a binary built once for the
// tests that compare its flags with the README and the chart.
var exporterHelp = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "exporter-help")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binary := filepath.Join(dir, "exporter")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the exporter: %w\n%s", err, out)
	}
	var stdout bytes.Buffer
	cmd := exec.Command(binary, "-h")
	cmd.Stdout = &stdout
	// -h exits non-zero, as the flag package has it; the text is what matters.
	_ = cmd.Run()
	return stdout.String(), nil
})

// helpText returns the exporter's -h output, failing the test when the
// exporter cannot be built.
func helpText(t *testing.T) string {
	t.Helper()
	help, err := exporterHelp()
	if err != nil {
		t.Fatal(err)
	}
	if help == "" {
		t.Fatal("the exporter printed no help")
	}
	return help
}
