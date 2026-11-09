//go:build !select_request_types || request_type_graphite || request_type_grpc

package repository

import "strings"

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
