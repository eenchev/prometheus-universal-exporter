package exporter

import (
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe's parameters fill the {{param_<name>}} placeholders of a
// collector's fixed label values (fetch/labelparams.go). The tests of it
// are with each request type's own, in files built with that type; what
// they share is here.

// labelServer serves a configuration written as a user writes one, loaded
// as the exporter loads it: the placeholders of its label values are read
// then, and nowhere else.
func labelServer(t *testing.T, document string) *Server {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	quiet := testutil.QuietLogger(t)
	return NewServer(config.NewManager(cfg, "", quiet), "python3", quiet)
}

// seriesLines are the series of an exposition, without its comments and
// its # EOF, in order of their text.
func seriesLines(exposition string) []string {
	var series []string
	for _, line := range strings.Split(exposition, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			series = append(series, line)
		}
	}
	sort.Strings(series)
	return series
}
