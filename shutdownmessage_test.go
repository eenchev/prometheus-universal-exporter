package main

import (
	"strings"
	"testing"
	"time"

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
		for _, probes := range []int{0, 1} {
			message := shutdownMessage(c.otlp, probes, 0, 15*time.Second)
			if got := strings.Contains(message, "OTLP"); got != c.want {
				t.Errorf("%s, %d probes: %q", name, probes, message)
			}
			if !strings.HasPrefix(message, "shutting down: ") {
				t.Errorf("%s, %d probes: %q", name, probes, message)
			}
		}
	}
}

// The line says what the shutdown waits for and for how long at most, and
// that a second signal ends the wait; with nothing to wait for and no export
// to send, that it exits.
func TestTheShutdownSaysWhatItWaitsFor(t *testing.T) {
	none := model.OTLPConfig{}
	for _, c := range []struct {
		probes, others int
		want           string
	}{
		{1, 0, "shutting down: waiting for 1 probe in progress, at most 15s; a second signal exits at once"},
		{2, 0, "shutting down: waiting for 2 probes in progress, at most 15s; a second signal exits at once"},
		{2, 1, "shutting down: waiting for 2 probes and 1 other request in progress, at most 15s; a second signal exits at once"},
		{0, 3, "shutting down: waiting for 3 requests in progress, at most 15s; a second signal exits at once"},
		{0, 0, "shutting down: no probes in progress; exiting"},
	} {
		if got := shutdownMessage(none, c.probes, c.others, 15*time.Second); got != c.want {
			t.Errorf("%d probes, %d others: %q, want %q", c.probes, c.others, got, c.want)
		}
	}
}
