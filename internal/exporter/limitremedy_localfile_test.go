//go:build !select_request_types || request_type_localfile

package exporter

import (
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A label over limits.max_label_value_length may be one no rule gives, as
// the file label a directory collector sets after its transform: the
// failure said `set truncate: true on the label`, which no rule can for it.
// It says now where truncate applies, a label one of the collector's rules
// gives, and names the limit to raise.
func TestALimitFailureOfALabelNoRuleGivesOffersNoRuleToSet(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a-rather-long-file-name.prom", "up 1\n")
	c := dirCollector("textfiles", root, "*.prom")
	c.Limits.MaxLabelValueLength = 10
	server := fileServer(t, c)
	probeFile(t, server, "collector=textfiles").must(t, http.StatusBadGateway,
		`metric "up" label "file" value is 28 bytes, longer than limits.max_label_value_length 10; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`)
}
