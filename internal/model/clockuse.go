package model

// ClockUse is what a collector's request holds of clock placeholders
// (Collector.Clock).
type ClockUse struct {
	// Any is set when the request holds a clock placeholder: each trip
	// fills them all from one reading of the clock.
	Any bool
	// Since is set when one of them reads the time of the last successful
	// trip, {{datetime~last_success(-1h)}}: the exporter keeps that time for
	// each target of the collector.
	Since bool
}
