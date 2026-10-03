package exporter

import (
	"net/http"
	"testing"
	"time"
)

// A trip identical probes share belongs to none of them (probeflight.go), and
// neither do its statistics: what the trip counted is the request's as soon as
// the trip answers a probe, whichever probe started it and whichever left
// before the answer (requeststats.go).

// A trip gathered apart from its probes has no probe's duration: joining a
// request, it adds its counts and leaves the duration of the request's last
// probe alone, where another probe's replaces it.
func TestAbsorbingATripKeepsTheDurationOfTheLastProbe(t *testing.T) {
	request := statsValues{probes: 1, lastDuration: 0.25}
	request.absorb(statsValues{cacheMisses: 1, decodeOK: 1, lastStatus: http.StatusOK, lastScrape: time.Unix(1, 0)})
	if request.lastDuration != 0.25 || request.cacheMisses != 1 || request.decodeOK != 1 || request.lastStatus != http.StatusOK {
		t.Fatalf("after a trip joined the request: %+v", request)
	}
	request.absorb(statsValues{probes: 1, lastDuration: 0.5})
	if request.lastDuration != 0.5 || request.probes != 2 || request.lastStatus != http.StatusOK {
		t.Fatalf("after another probe joined the request: %+v", request)
	}
}
