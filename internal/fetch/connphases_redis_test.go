//go:build request_type_redis

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
)

// A redis connection's phases are those of the first command sent on it,
// whose span begins with them, and no other's: over TLS, the connection
// and the handshake; with a Sentinel, the Sentinel's connection the first
// SENTINEL command's and the primary's its INFO's.
func TestARedisConnectionsPhasesAreItsFirstCommands(t *testing.T) {
	secure := redistest.NewTLS(t)
	secure.SetInfo(redisInfo...)
	sections := redisCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = secure.CAFile }, func(c *model.Collector) {
		editRedis(c, func(r *RedisConfig) { r.InfoSections = []string{"server", "memory"} })
	})
	requests := spanTracedRequests(t, sections, secure.Addr)
	if len(requests) != 2 || phaseNames(t, requests[0]) != "connection.connect,connection.tls" || len(requests[1].Phases) != 0 {
		t.Errorf("over TLS: %+v", requests)
	}
	primary := redistest.New(t)
	primary.SetInfo(redisInfo...)
	sentinel := redisSentinelOf(t, primary.Addr)
	requests = spanTracedRequests(t, redisCollector(t, withSentinel(RedisSentinel{Master: "mymaster"})), sentinel.Addr)
	var got []string
	for _, r := range requests {
		got = append(got, r.Method+":"+phaseNames(t, r))
	}
	if len(got) != 3 || got[0] != "SENTINEL:connection.connect" || got[1] != "SENTINEL:" || got[2] != "INFO:connection.connect" {
		t.Errorf("with a Sentinel: %q", got)
	}
}
