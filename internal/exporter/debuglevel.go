package exporter

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Debugging collectors is one feature with two levels, --web.debug:
//
//   - edit serves the debug page, /debug, where a collector — one written
//     on the page, or one of the configuration in force by name — is run on
//     a pasted response. Nothing is fetched, so nothing a target answers is
//     ever shown;
//   - full adds what shows a target's answer: the page fetching a target
//     through a configured collector, or scraping a static target,
//     /probe?...&debug=true and /static-targets?debug=<name>.
//
// Every run is made by one engine (runDebug, probedebug.go) and reported in
// one structure (debugReport), written as text for the two URLs and as JSON
// for the page (debugpage.go). Off, the default, serves none of it.

// DebugLevel is the level of --web.debug.
type DebugLevel uint8

// The levels of --web.debug.
const (
	DebugOff DebugLevel = iota
	DebugEdit
	DebugFull
)

var debugLevelNames = [...]string{DebugOff: "off", DebugEdit: "edit", DebugFull: "full"}

func (l DebugLevel) String() string { return debugLevelNames[l] }

// ParseDebugLevel reads --web.debug: off, edit or full, in any case. Any
// other value is a command-line error, as a misspelt level would otherwise
// leave debugging off, or on, without a word.
func ParseDebugLevel(value string) (DebugLevel, error) {
	for level, name := range debugLevelNames {
		if strings.EqualFold(strings.TrimSpace(value), name) {
			return DebugLevel(level), nil
		}
	}
	return DebugOff, fmt.Errorf("--web.debug %q is not a level; use off, edit or full", value)
}

// SetDebug sets the level of debugging, --web.debug.
func (s *Server) SetDebug(level DebugLevel) {
	s.debug = level
	if level != DebugOff && s.debugSlots == nil {
		s.debugSlots = make(chan struct{}, debugMaxConcurrent)
	}
}

// debugKind is what asked for a run: a debug probe, a debug scrape of a
// static target, or the debug page.
type debugKind uint8

const (
	debugKindProbe debugKind = iota
	debugKindStatic
	debugKindPage
)

var debugKindNames = [...]string{debugKindProbe: "probe", debugKindStatic: "static", debugKindPage: "page"}

func (k debugKind) String() string { return debugKindNames[k] }

// debugResult is how a run ended: it went through whole; it ran, or was
// taken and could not run, and did not; or it was refused before it ran.
type debugResult uint8

const (
	debugSuccess debugResult = iota
	debugFailure
	debugInvalid
)

var debugResultNames = [...]string{debugSuccess: "success", debugFailure: "failure", debugInvalid: "invalid"}

// result is how the run reported ended.
func (r *debugReport) result() debugResult {
	if r.ok {
		return debugSuccess
	}
	return debugFailure
}

// debugRuns counts the runs by kind and result, for
// http_exporter_debug_runs_total.
type debugRuns [len(debugKindNames)][len(debugResultNames)]atomic.Uint64

// countDebugRun counts a run of kind that ended as result.
func (s *Server) countDebugRun(kind debugKind, result debugResult) {
	s.debugRunCounts[kind][result].Add(1)
}

// debugRunMetrics is http_exporter_debug_runs_total, by kind and result,
// from 0, and nothing with debugging off. A kind the level does not serve
// is not there: at edit, the page alone.
func (s *Server) debugRunMetrics() []model.Metric {
	if s.debug == DebugOff {
		return nil
	}
	name := "http_exporter_debug_runs_total"
	var out []model.Metric
	for kind := range debugKindNames {
		if !s.debugServes(debugKind(kind)) {
			continue
		}
		for result := range debugResultNames {
			out = append(out, model.Metric{
				Name: name, Help: exporterMetricHelp[name], Type: model.CounterMetricType,
				Value:  float64(s.debugRunCounts[kind][result].Load()),
				Labels: map[string]string{"kind": debugKindNames[kind], "result": debugResultNames[result]},
			})
		}
	}
	return out
}

// debugServes reports whether the level serves runs of kind.
func (s *Server) debugServes(kind debugKind) bool {
	if kind == debugKindPage {
		return s.debug != DebugOff
	}
	return s.debug == DebugFull
}

// debugReportsRefused is the refusal of a debug probe or debug scrape below
// --web.debug=full: they show what a target answered.
func (s *Server) debugReportsRefused() string {
	refusal := "debug reports of probes and static targets show what a target answered, and need --web.debug=full (the chart's server.debug: full) to allow /probe?debug=true and /static-targets?debug=<name>"
	if s.debug == DebugEdit {
		refusal += "; at --web.debug=edit the debug page runs collectors on pasted responses only"
	}
	return refusal
}

// debugPageRunLine is the second line of a report of the debug page's run,
// after what it would have answered.
const debugPageRunLine = "A debug run puts nothing in force, skips the response cache, shares no trip, records no self-metric but http_exporter_debug_runs_total and exports nothing over OTLP.\n"
