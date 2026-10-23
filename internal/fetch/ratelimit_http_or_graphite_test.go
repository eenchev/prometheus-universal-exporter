//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import "time"

// waited is the waits made so far.
func (c *rateClock) waited() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}
