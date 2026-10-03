package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The response cache is keyed by the collector's definition, so changing the
// prefix on reload never serves metrics cached under the old names.
func TestChangingTheMetricsPrefixChangesTheCacheKey(t *testing.T) {
	a := testutil.Collector("prefixed", "text")
	b := a
	b.MetricsPrefix = "acme"
	if collectorFingerprint(&a) == collectorFingerprint(&b) {
		t.Fatal("the prefix is not part of the cache key")
	}
}
