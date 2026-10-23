//go:build !select_request_types || request_type_dns || request_type_kubernetes || request_type_redis || request_type_tcp || request_type_tlscert

package repository

import (
	"strings"
	"testing"
)

// The helpers of the tests of docs/DNS.md, docs/TLSCERT.md,
// docs/KUBERNETES.md, docs/REDIS.md and docs/TCP.md.

// pageBlocks returns the fenced blocks of a page in the given language.
func pageBlocks(t *testing.T, path, language string) []string {
	t.Helper()
	var blocks []string
	for _, part := range strings.Split(read(t, path), "```"+language+"\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	return blocks
}

// pageBlock is the block of blocks that starts with prefix.
func pageBlock(t *testing.T, blocks []string, prefix string) string {
	t.Helper()
	for _, block := range blocks {
		if strings.HasPrefix(block, prefix) {
			return block
		}
	}
	t.Fatalf("no example starts %q", prefix)
	return ""
}
