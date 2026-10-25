package main

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The line the graceful shutdown begins with names the last OTLP export only
// when OTLP export is on, as it is only then that one is sent: an exporter
// without OTLP, or with an otlp block switched off or without an endpoint,
// said it would send an export it never sent.
func TestTheShutdownNamesTheLastOTLPExportOnlyWhenOneIsSent(t *testing.T) {
	for name, c := range map[string]struct {
		otlp model.OTLPConfig
		want bool
	}{
		"no otlp block":       {model.OTLPConfig{}, false},
		"switched off":        {model.OTLPConfig{Enabled: false, Endpoint: "http://collector:4318/v1/metrics"}, false},
		"on without endpoint": {model.OTLPConfig{Enabled: true}, false},
		"on":                  {model.OTLPConfig{Enabled: true, Endpoint: "http://collector:4318/v1/metrics"}, true},
	} {
		message := shutdownMessage(c.otlp)
		if got := strings.Contains(message, "OTLP"); got != c.want {
			t.Errorf("%s: %q", name, message)
		}
		if !strings.HasPrefix(message, "shutting down: finishing the probes in progress") || !strings.HasSuffix(message, "a second signal exits at once") {
			t.Errorf("%s: %q", name, message)
		}
	}
}
