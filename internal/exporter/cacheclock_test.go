//go:build !select_request_types || request_type_http || request_type_graphite

package exporter

import "time"

// The http and graphite tests of revalidation both age the cache, so the
// helper is built with either type, in no file of one alone, and with
// no build that has neither.

// outOfDate makes every cached entry no longer fresh, and, with expired,
// past its stale window too, as the clock would.
func outOfDate(server *Server, expired bool) {
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	past := time.Now().Add(-time.Second)
	for _, entry := range server.cache.entries {
		entry.freshUntil = past
		if expired {
			entry.expires = past
		}
	}
}
