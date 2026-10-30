package exporter

import (
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The status page's OTLP section says whether exports get through: the
// endpoint, its last export and its last success, the failures in a row
// and the readiness, as /ready answers it, the last failure's error, and what waits,
// what was dropped and what was retried. It reads the counters the
// self-metrics read (otlpStatus) and the points pending, and keeps of its
// own only what nothing else did: the last export's details and the last
// failure's error, one record of the endpoint in force (otlpHistory). A
// reload that points OTLP elsewhere starts that record again, as it starts
// the failures that make the exporter unready, so the page never blames the
// new endpoint for the old one's failures; the counters since the start, as
// the self-metrics have them, go on.

// statusPartialSuccess is the result of an export the endpoint accepted
// without some of its data points.
const statusPartialSuccess = "partial success"

// otlpProtocol is how the export is sent: OTLP/HTTP with a JSON body, the
// only protocol the exporter speaks.
const otlpProtocol = "http/json"

// otlpStatusSection is the page's OTLP section, and its object in the JSON:
// {"enabled": false} alone while OTLP export is off.
type otlpStatusSection struct {
	Enabled bool `json:"enabled"`
	*OTLPStatusDetails
}

// OTLPStatusDetails is the OTLP section while OTLP export is on. It is
// exported for encoding/json, which cannot set an embedded pointer to an
// unexported type when the page is read back.
type OTLPStatusDetails struct {
	// Endpoint is otlp.endpoint with its credentials withheld: the URL's
	// userinfo, and the values of query parameters named like credentials.
	// otlp.headers are never shown.
	Endpoint string           `json:"endpoint"`
	Protocol string           `json:"protocol"`
	Interval string           `json:"interval"`
	Exported otlpExportedKind `json:"exported"`
	// Allocator says the exports are this replica's.
	Allocator bool `json:"allocator_mode"`
	// Failing says the last export to the endpoint failed.
	Failing     bool            `json:"failing"`
	LastExport  *otlpLastExport `json:"last_export,omitempty"`
	LastSuccess string          `json:"last_success,omitempty"`
	Failures    int             `json:"consecutive_failures"`
	// UnreadyAfter is otlp.unready_after_failures, and Readiness, with it
	// set, what /ready answers: ready, or unready for UnreadyReasons, the
	// reasons /ready gives, and since UnreadySince when the failed exports
	// are what made it so.
	UnreadyAfter   int      `json:"unready_after_failures,omitempty"`
	Readiness      string   `json:"readiness,omitempty"`
	UnreadyReasons []string `json:"unready_reasons,omitempty"`
	UnreadySince   string   `json:"unready_since,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
	LastErrorAt    string   `json:"last_error_at,omitempty"`
	// Pending are the data points waiting for the next export, MaxPending
	// otlp.max_pending_points.
	Pending    int `json:"pending_points"`
	MaxPending int `json:"max_pending_points"`
	// Dropped is http_exporter_otlp_points_dropped_total, DroppedBy the
	// same by why.
	Dropped   int             `json:"points_dropped"`
	DroppedBy otlpDroppedKind `json:"points_dropped_by_reason"`
	Retries   int             `json:"retries"`
	Succeeded uint64          `json:"exports_succeeded"`
	FailedAll uint64          `json:"exports_failed"`
	// LastSuccessAge is LastSuccess as the page writes it.
	LastSuccessAge string `json:"-"`
	// Logs is the export of the exporter's own log (otlplogs.go).
	Logs otlpLogsSection `json:"logs"`
}

// otlpLogsSection is the log export's part of the OTLP section:
// {"enabled": false} alone while otlp.logs is off.
type otlpLogsSection struct {
	Enabled bool `json:"enabled"`
	*OTLPLogsStatusDetails
}

// OTLPLogsStatusDetails is the log export's part while it is on, exported
// for encoding/json as OTLPStatusDetails is. Readiness is not among it: the
// log export does not make the exporter unready.
type OTLPLogsStatusDetails struct {
	// Endpoint is otlp.logs.endpoint with its credentials withheld.
	Endpoint string `json:"endpoint"`
	Level    string `json:"level"`
	Interval string `json:"interval"`
	// LastExport is when the last export began, LastResult how it went
	// and LastRecords the records it carried.
	LastExport  string `json:"last_export,omitempty"`
	LastResult  string `json:"last_result,omitempty"`
	LastRecords int    `json:"last_records,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
	Failing     bool   `json:"failing"`
	Failures    int    `json:"consecutive_failures"`
	LastError   string `json:"last_error,omitempty"`
	LastErrorAt string `json:"last_error_at,omitempty"`
	// Pending are the records waiting, MaxPending
	// otlp.logs.max_pending_records.
	Pending    int `json:"pending_records"`
	MaxPending int `json:"max_pending_records"`
	Sent       int `json:"records_sent"`
	// Dropped is http_exporter_otlp_log_records_dropped_total, DroppedBy
	// the same by why.
	Dropped   int                `json:"records_dropped"`
	DroppedBy otlpLogDroppedKind `json:"records_dropped_by_reason"`
	Succeeded int                `json:"exports_succeeded"`
	FailedAll int                `json:"exports_failed"`
}

// otlpLogDroppedKind is the log records given up on, by why
// (otlpLogDropReason).
type otlpLogDroppedKind struct {
	PendingCap int `json:"over_max_pending_records"`
	Refused    int `json:"refused"`
	Shutdown   int `json:"last_export_at_shutdown"`
}

// otlpLogsSectionFor is the log export's part of the OTLP section with cfg
// in force.
func (s *Server) otlpLogsSectionFor(cfg model.OTLPConfig) otlpLogsSection {
	if s.otlpLogs == nil || !otlpLogsOn(cfg) {
		return otlpLogsSection{}
	}
	interval := time.Duration(cfg.Logs.Interval)
	if interval <= 0 {
		interval = model.DefaultOTLPLogsInterval
	}
	maxPending := cfg.Logs.MaxPendingRecords
	if maxPending <= 0 {
		maxPending = model.DefaultOTLPLogsMaxPendingRecords
	}
	d := &OTLPLogsStatusDetails{Endpoint: redactedOTLPEndpoint(cfg.Logs.Endpoint), Level: cfg.Logs.Level, Interval: interval.String(), MaxPending: maxPending}
	l := s.otlpLogs
	l.mu.Lock()
	defer l.mu.Unlock()
	d.Pending, d.Sent, d.Succeeded, d.FailedAll = len(l.queue), l.sent, l.successes, l.failures
	d.Dropped = l.droppedLocked()
	d.DroppedBy = otlpLogDroppedKind{PendingCap: l.dropped[otlpLogDropPendingCap], Refused: l.dropped[otlpLogDropRefused], Shutdown: l.dropped[otlpLogDropShutdown]}
	// The record of another endpoint is not this one's.
	if l.endpoint != cfg.Logs.Endpoint {
		return otlpLogsSection{Enabled: true, OTLPLogsStatusDetails: d}
	}
	h := l.history
	if !h.lastStarted.IsZero() {
		d.LastExport, d.LastResult, d.LastRecords = h.lastStarted.UTC().Format(time.RFC3339), statusFailed, h.lastRecords
		if h.lastOK {
			d.LastResult = statusSuccess
		}
	}
	if !h.lastSuccess.IsZero() {
		d.LastSuccess = h.lastSuccess.UTC().Format(time.RFC3339)
	}
	d.Failures, d.Failing, d.LastError = h.failures, h.failures > 0, h.errorText
	if !h.failedAt.IsZero() {
		d.LastErrorAt = h.failedAt.UTC().Format(time.RFC3339)
	}
	return otlpLogsSection{Enabled: true, OTLPLogsStatusDetails: d}
}

// otlpExportedKind is what the exports carry.
type otlpExportedKind struct {
	Probes        bool `json:"probes"`
	StaticTargets int  `json:"static_targets"`
	SelfMetrics   bool `json:"self_metrics"`
}

// otlpDroppedKind is the data points given up on, by why (otlpDropReason).
type otlpDroppedKind struct {
	Refused    int `json:"refused"`
	Rejected   int `json:"rejected"`
	PendingCap int `json:"over_max_pending_points"`
	Shutdown   int `json:"last_export_at_shutdown"`
}

// otlpLastExport is the last export's line.
type otlpLastExport struct {
	Started    string  `json:"started"`
	Age        float64 `json:"age_seconds"`
	Duration   float64 `json:"duration_seconds"`
	Result     string  `json:"result"`
	Requests   int     `json:"requests"`
	DataPoints int     `json:"data_points"`
	Rejected   int64   `json:"rejected_points"`
	Retries    int     `json:"retries"`
	// AgeText and DurationText are Age and Duration as the page writes
	// them.
	AgeText      string `json:"-"`
	DurationText string `json:"-"`
}

// shownOTLPError is err as the status page shows it: its URLs' credentials
// withheld, what reads as a credential masked, and cut, as a static
// target's error is (shownStatusError).
func shownOTLPError(err error) string {
	return shownFailureText(boundedTripFailure(fetch.RedactURLErrors(err)).Error())
}

// otlpStatusSectionFor is the OTLP section with cfg in force, as of now.
// exportedTargets is how many of the static targets in force set
// export_via_otlp.
func (s *Server) otlpStatusSectionFor(cfg model.OTLPConfig, exportedTargets int, now time.Time) otlpStatusSection {
	if !cfg.Enabled || cfg.Endpoint == "" {
		return otlpStatusSection{}
	}
	interval := time.Duration(cfg.Interval)
	if interval <= 0 {
		interval = 30 * time.Second
	}
	maxPending := cfg.MaxPendingPoints
	if maxPending <= 0 {
		maxPending = model.DefaultOTLPMaxPendingPoints
	}
	d := &OTLPStatusDetails{
		Endpoint:     fetch.RedactURLString(cfg.Endpoint, fetch.MaskCredentialQueryValues),
		Protocol:     otlpProtocol,
		Interval:     interval.String(),
		Exported:     otlpExportedKind{Probes: true, StaticTargets: exportedTargets, SelfMetrics: true},
		Allocator:    s.allocatorStatus != nil,
		MaxPending:   maxPending,
		UnreadyAfter: max(cfg.UnreadyAfterFailures, 0),
		Logs:         s.otlpLogsSectionFor(cfg),
	}
	s.otlpMu.Lock()
	d.Pending = s.otlpPoints
	s.otlpMu.Unlock()

	o := s.otlp
	o.mu.Lock()
	d.Retries, d.Succeeded, d.FailedAll = o.retries, o.successes, o.failures
	d.Dropped = o.droppedLocked()
	d.DroppedBy = otlpDroppedKind{Refused: o.dropped[otlpDropRefused], Rejected: o.dropped[otlpDropRejected], PendingCap: o.dropped[otlpDropPendingCap], Shutdown: o.dropped[otlpDropShutdown]}
	// The record of another endpoint, before the first export since a
	// reload changed it, is not this one's.
	var h otlpHistory
	var unreadySince time.Time
	if o.endpoint == cfg.Endpoint {
		h, d.Failures = o.history, o.consecutiveFailures
		// Read under mu: the next failure appends to the times.
		unreadySince = h.unreadySince(d.UnreadyAfter)
		h.failureEnds = nil
	}
	o.mu.Unlock()

	d.Failing = d.Failures > 0
	if !h.last.started.IsZero() {
		last := h.last
		e := &otlpLastExport{
			Started:  last.started.UTC().Format(time.RFC3339),
			Age:      max(now.Sub(last.started), 0).Seconds(),
			Duration: last.duration.Seconds(),
			Result:   statusFailed, Requests: last.requests, DataPoints: last.points, Rejected: last.rejected, Retries: last.retries,
			AgeText:      shortDuration(max(now.Sub(last.started), 0)),
			DurationText: shownScrapeDuration(last.duration),
		}
		switch {
		case h.ok && last.rejected > 0:
			e.Result = statusPartialSuccess
		case h.ok:
			e.Result = statusSuccess
		}
		d.LastExport = e
	}
	if !h.lastSuccess.IsZero() {
		d.LastSuccess = h.lastSuccess.UTC().Format(time.RFC3339)
		d.LastSuccessAge = shortDuration(max(now.Sub(h.lastSuccess), 0))
	}
	d.LastError = h.errorText
	if !h.failedAt.IsZero() {
		d.LastErrorAt = h.failedAt.UTC().Format(time.RFC3339)
	}
	if d.UnreadyAfter > 0 {
		// What /ready answers, from the same reasons, not from the failed
		// exports alone: the exporter is also unready while it shuts down
		// and, in allocator mode, before its first bundle. Read after
		// o.mu is released, which notReadyReasons takes.
		d.Readiness = "ready"
		if reasons := s.notReadyReasons(); len(reasons) > 0 {
			d.Readiness, d.UnreadyReasons = "unready", reasons
			if reasons[0] != shuttingDownReason && d.Failures >= d.UnreadyAfter && !unreadySince.IsZero() {
				d.UnreadySince = unreadySince.UTC().Format(time.RFC3339)
			}
		}
	}
	return otlpStatusSection{Enabled: true, OTLPStatusDetails: d}
}
