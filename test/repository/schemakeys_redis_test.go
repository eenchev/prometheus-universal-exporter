//go:build request_type_redis

package repository

import "testing"

// The keys of a redis collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnRedisKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, redisSchemaKeys())
}
