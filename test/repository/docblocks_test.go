//go:build !select_request_types || request_type_graphite || request_type_grpc

package repository

import (
	"strings"
	"testing"
)

// The helpers of the tests that load and run the examples of a page.

// docBlocks returns the fenced blocks of a page in the given language.
func docBlocks(t *testing.T, path, language string) []string {
	t.Helper()
	var blocks []string
	for _, part := range strings.Split(read(t, path), "```"+language+"\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	return blocks
}

func docBlock(t *testing.T, blocks []string, prefix string) string {
	t.Helper()
	for _, block := range blocks {
		if strings.HasPrefix(block, prefix) {
			return block
		}
	}
	t.Fatalf("no example starts %q", prefix)
	return ""
}

// indent prefixes every line of block.
func indent(block, prefix string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(block, "\n") {
		if line != "" {
			b.WriteString(prefix)
			b.WriteString(line)
		}
	}
	return b.String()
}
