package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter's own log is forwarded over OTLP, with otlp.logs, to the
// collector its metrics go to: every line at or above otlp.logs.level, as
// OTLP log records, besides the JSON lines on stderr, which go on as they
// were. The export is OTLP/HTTP with a JSON body, sent as the metrics are
// (deliverOTLPOf), with the otlp block's headers, TLS, compression, timeout
// and retries, under the exporter-wide resource.
//
// Logging must never wait for the export. A line is taken by the handler
// that writes it to stderr (OTLPLogs.Handler), encoded as a record and put
// in a queue under a lock held for no more than that, and a line that finds
// the queue at otlp.logs.max_pending_records is dropped and counted rather
// than waited for. The queue is sent every otlp.logs.interval, in the
// background.
//
// The export logs what becomes of it, and those lines are never exported:
// they are written with the handler beneath the tee, to stderr alone. A
// collector that is down would otherwise have each failed export queue a
// line about itself for the next, which fails too. They are a repeated
// failure (failurelog.go) besides, so a collector down for hours is a line
// every five minutes on stderr, not one an interval.
//
// Readiness does not depend on the log export: a log that cannot be
// delivered is no reason to stop answering probes.

// OTLPLogs takes the exporter's log lines for export over OTLP. It is made
// before the configuration is read, with the handler the lines go to on
// stderr, and given to the server (Server.SetOTLPLogs), which switches it on
// and off as otlp.logs says and sends what it queued.
type OTLPLogs struct {
	// base is the handler beneath the tee: stderr.
	base slog.Handler
	// taking is what the tee takes, nil while the log export is off, read
	// on every line without a lock.
	taking atomic.Pointer[otlpLogsTaking]

	mu sync.Mutex
	// queue holds the records waiting, the oldest first, each the JSON of
	// one OTLP LogRecord.
	queue [][]byte
	// The counts since the start, the status page's and the
	// self-metrics'.
	sent                int
	dropped             [otlpLogDropReasons]int
	successes, failures int
	// endpoint and history are what the status page shows of the exports
	// to the endpoint in force; a reload that changes it starts them
	// again.
	endpoint string
	history  otlpLogsHistory
}

// otlpLogsTaking is what the tee takes while the log export is on: lines at
// level or above, up to max waiting.
type otlpLogsTaking struct {
	level slog.Level
	max   int
}

// otlpLogsHistory is the status page's record of the exports to one
// endpoint.
type otlpLogsHistory struct {
	lastStarted time.Time
	lastOK      bool
	lastRecords int
	lastSuccess time.Time
	failures    int
	errorText   string
	failedAt    time.Time
}

// otlpLogDropReason is why log records were given up on.
type otlpLogDropReason int

const (
	// otlpLogDropPendingCap: the queue was at otlp.logs.max_pending_records.
	otlpLogDropPendingCap otlpLogDropReason = iota
	// otlpLogDropRefused: the endpoint refused a request with an answer
	// that is not retried, or rejected some records of one it accepted.
	otlpLogDropRefused
	// otlpLogDropShutdown: those of the last export before exiting, which
	// failed.
	otlpLogDropShutdown
	otlpLogDropReasons
)

// NewOTLPLogs makes the log export's tee over base, which the lines are
// written to as before; it takes nothing until the server switches it on.
func NewOTLPLogs(base slog.Handler) *OTLPLogs {
	return &OTLPLogs{base: base}
}

// Handler is the handler the exporter logs with: base, and the export's
// queue while the log export is on.
func (l *OTLPLogs) Handler() slog.Handler {
	return &otlpLogHandler{logs: l, base: l.base}
}

// configure switches the tee as cfg says. A log export switched off drops
// what waits for it, as the metrics' export drops what was queued while it
// is off.
func (l *OTLPLogs) configure(cfg model.OTLPConfig) {
	if !otlpLogsOn(cfg) {
		l.taking.Store(nil)
		l.mu.Lock()
		l.queue = nil
		l.mu.Unlock()
		return
	}
	level, _ := model.LogLevel(cfg.Logs.Level)
	limit := cfg.Logs.MaxPendingRecords
	if limit <= 0 {
		limit = model.DefaultOTLPLogsMaxPendingRecords
	}
	l.taking.Store(&otlpLogsTaking{level: level, max: limit})
	l.mu.Lock()
	// A lower bound than the records waiting drops the oldest of them.
	l.capLocked(limit)
	l.mu.Unlock()
}

// stop takes no more lines: those the last export has drained are the last.
func (l *OTLPLogs) stop() { l.taking.Store(nil) }

// otlpLogsOn says whether cfg exports the exporter's log.
func otlpLogsOn(cfg model.OTLPConfig) bool {
	return cfg.Enabled && cfg.Endpoint != "" && cfg.Logs.Enabled && cfg.Logs.Endpoint != ""
}

// wants says whether a line at level is taken.
func (l *OTLPLogs) wants(level slog.Level) bool {
	taking := l.taking.Load()
	return taking != nil && level >= taking.level
}

// add queues one record, or drops and counts it when the queue is full.
func (l *OTLPLogs) add(record []byte) {
	taking := l.taking.Load()
	if taking == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) >= taking.max {
		l.dropped[otlpLogDropPendingCap]++
		return
	}
	l.queue = append(l.queue, record)
}

// drain takes every record waiting.
func (l *OTLPLogs) drain() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	records := l.queue
	l.queue = nil
	return records
}

// requeue puts records an export could not deliver back before those queued
// since, still within the bound, the oldest dropped first past it. Nothing
// is put back while the log export is off.
func (l *OTLPLogs) requeue(records [][]byte) {
	taking := l.taking.Load()
	if taking == nil || len(records) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = append(records[:len(records):len(records)], l.queue...)
	l.capLocked(taking.max)
}

// capLocked drops the oldest records past limit, under mu.
func (l *OTLPLogs) capLocked(limit int) {
	if over := len(l.queue) - limit; over > 0 {
		l.dropped[otlpLogDropPendingCap] += over
		l.queue = append([][]byte(nil), l.queue[over:]...)
	}
}

// drop counts records given up on.
func (l *OTLPLogs) drop(records int, why otlpLogDropReason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropped[why] += records
}

// droppedLocked is every record given up on, under mu.
func (l *OTLPLogs) droppedLocked() int {
	n := 0
	for _, records := range l.dropped {
		n += records
	}
	return n
}

// record records an export of records to endpoint, of which sent got
// through, that failed with err unless err is nil.
func (l *OTLPLogs) record(endpoint string, started time.Time, records, sent int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if endpoint != l.endpoint {
		l.endpoint, l.history = endpoint, otlpLogsHistory{}
	}
	l.sent += sent
	h := &l.history
	h.lastStarted, h.lastOK, h.lastRecords = started, err == nil, records
	if err == nil {
		l.successes++
		h.lastSuccess, h.failures = time.Now(), 0
		return
	}
	l.failures++
	h.failures++
	h.errorText, h.failedAt = shownOTLPError(err), started
}

// otlpLogHandler is the tee: every line goes to base as it did, and those
// the log export takes to its queue too. attrs are the attributes WithAttrs
// gave it, as the record carries them, and prefix the groups WithGroup
// opened, each name followed by a dot.
type otlpLogHandler struct {
	logs   *OTLPLogs
	base   slog.Handler
	attrs  []otlpAnyAttribute
	prefix string
}

func (h *otlpLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level) || h.logs.wants(level)
}

func (h *otlpLogHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	// The tee may take a line under --log.level, which stderr does not
	// write: Enabled said yes for the export.
	if h.base.Enabled(ctx, r.Level) {
		err = h.base.Handle(ctx, r)
	}
	if h.logs.wants(r.Level) {
		h.logs.add(h.encode(r))
	}
	return err
}

func (h *otlpLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	out := *h
	out.base = h.base.WithAttrs(attrs)
	out.attrs = append([]otlpAnyAttribute(nil), h.attrs...)
	for _, a := range attrs {
		out.attrs = appendLogAttribute(out.attrs, h.prefix, a)
	}
	return &out
}

func (h *otlpLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	out := *h
	out.base = h.base.WithGroup(name)
	out.prefix = h.prefix + name + "."
	return &out
}

// otlpLogRecord is an OTLP LogRecord as OTLP/JSON writes it.
type otlpLogRecord struct {
	TimeUnixNano         string             `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano string             `json:"observedTimeUnixNano"`
	SeverityNumber       int                `json:"severityNumber"`
	SeverityText         string             `json:"severityText"`
	Body                 otlpAnyValue       `json:"body"`
	Attributes           []otlpAnyAttribute `json:"attributes,omitempty"`
}

// otlpAnyAttribute and otlpAnyValue are an attribute of a log record and
// its value. A string is written even when it is empty, which the metrics'
// otlpValue leaves out: a log's attribute of "" says something.
type otlpAnyAttribute struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    string   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func otlpString(text string) otlpAnyValue { return otlpAnyValue{StringValue: &text} }

// encode is the record of r, with the attributes the handler was given and
// then r's own, as JSON. It cannot fail: every value is made a string, a
// number or a boolean first.
func (h *otlpLogHandler) encode(r slog.Record) []byte {
	record := otlpLogRecord{
		ObservedTimeUnixNano: strconv.FormatInt(time.Now().UnixNano(), 10),
		SeverityNumber:       otlpSeverity(r.Level),
		SeverityText:         r.Level.String(),
		Body:                 otlpString(r.Message),
		Attributes:           h.attrs,
	}
	if !r.Time.IsZero() {
		record.TimeUnixNano = strconv.FormatInt(r.Time.UnixNano(), 10)
	}
	if r.NumAttrs() > 0 {
		record.Attributes = append(make([]otlpAnyAttribute, 0, len(h.attrs)+r.NumAttrs()), h.attrs...)
		r.Attrs(func(a slog.Attr) bool {
			record.Attributes = appendLogAttribute(record.Attributes, h.prefix, a)
			return true
		})
	}
	raw, err := json.Marshal(record)
	if err != nil {
		// Not reached: nothing in a record is of a kind JSON refuses.
		raw, _ = json.Marshal(otlpLogRecord{ObservedTimeUnixNano: record.ObservedTimeUnixNano, SeverityNumber: record.SeverityNumber, SeverityText: record.SeverityText, Body: record.Body})
	}
	return raw
}

// otlpSeverity is the OTLP severity number of a slog level, as the OTLP
// specification maps slog's: DEBUG 5, INFO 9, WARN 13, ERROR 17, and a level
// between them the number as far from theirs, within 1 to 24.
func otlpSeverity(level slog.Level) int {
	return min(max(int(level)+9, 1), 24)
}

// appendLogAttribute appends a, under prefix, to attrs as slog's handlers
// write it: a group's attributes each under the group's name and a dot, a
// group without a name inline, an empty group and an empty attribute not at
// all, and a value that resolves to another (slog.LogValuer) as that one.
func appendLogAttribute(attrs []otlpAnyAttribute, prefix string, a slog.Attr) []otlpAnyAttribute {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return attrs
	}
	if a.Value.Kind() == slog.KindGroup {
		inner := prefix
		if a.Key != "" {
			inner = prefix + a.Key + "."
		}
		for _, member := range a.Value.Group() {
			attrs = appendLogAttribute(attrs, inner, member)
		}
		return attrs
	}
	return append(attrs, otlpAnyAttribute{Key: prefix + a.Key, Value: otlpLogValue(a.Value)})
}

// otlpLogValue is an attribute's value with its type: a string, an integer,
// a double or a boolean. An unsigned integer past int64 is written as text,
// as are a double that is not a number or infinite, which JSON has no
// number for, a duration, as the exporter logs durations, a time in
// RFC 3339, and any other value as stderr's JSON writes it.
func otlpLogValue(v slog.Value) otlpAnyValue {
	switch v.Kind() {
	case slog.KindString:
		return otlpString(v.String())
	case slog.KindInt64:
		return otlpAnyValue{IntValue: strconv.FormatInt(v.Int64(), 10)}
	case slog.KindUint64:
		if u := v.Uint64(); u <= math.MaxInt64 {
			return otlpAnyValue{IntValue: strconv.FormatUint(u, 10)}
		}
		return otlpString(strconv.FormatUint(v.Uint64(), 10))
	case slog.KindFloat64:
		f := v.Float64()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return otlpString(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return otlpAnyValue{DoubleValue: &f}
	case slog.KindBool:
		b := v.Bool()
		return otlpAnyValue{BoolValue: &b}
	case slog.KindDuration:
		return otlpString(v.Duration().String())
	case slog.KindTime:
		return otlpString(v.Time().Format(time.RFC3339Nano))
	}
	switch value := v.Any().(type) {
	case error:
		return otlpString(value.Error())
	case fmt.Stringer:
		return otlpString(value.String())
	}
	if raw, err := json.Marshal(v.Any()); err == nil {
		return otlpString(string(raw))
	}
	return otlpString(fmt.Sprintf("%+v", v.Any()))
}

// otlpLogRequest is one request of a log export: its body, and which of the
// export's records it carries, from up to to.
type otlpLogRequest struct {
	body     []byte
	from, to int
}

// otlpLogRequests makes the requests that send records to the endpoint
// under the exporter-wide resource: each of at most otlp.batch_max_size
// records and otlp.batch_max_bytes bytes of JSON, as the metrics' requests
// are bounded, a record larger alone in a request of its own. The records
// are JSON already, so a request is made by writing them into the envelope
// one after another, and its size is known as it is filled.
func otlpLogRequests(cfg model.OTLPConfig, records [][]byte) ([]otlpLogRequest, error) {
	resource, err := json.Marshal(otlpResource{Attributes: defaultResourceIdentity(cfg).attributes()})
	if err != nil {
		return nil, err
	}
	scope, err := json.Marshal(otlpScope{Name: "prometheus-universal-exporter"})
	if err != nil {
		return nil, err
	}
	head := `{"resourceLogs":[{"resource":` + string(resource) + `,"scopeLogs":[{"scope":` + string(scope) + `,"logRecords":[`
	const tail = `]}]}]}`
	maxRecords, maxBytes := otlpBatchBounds(cfg)
	var requests []otlpLogRequest
	for from := 0; from < len(records); {
		size, to := len(head)+len(tail)+len(records[from]), from+1
		for to < len(records) && to-from < maxRecords && size+1+len(records[to]) <= maxBytes {
			size += 1 + len(records[to])
			to++
		}
		raw := make([]byte, 0, size)
		raw = append(raw, head...)
		for i := from; i < to; i++ {
			if i > from {
				raw = append(raw, ',')
			}
			raw = append(raw, records[i]...)
		}
		raw = append(raw, tail...)
		body, err := compressOTLP(raw, cfg.Compression)
		if err != nil {
			return nil, err
		}
		requests = append(requests, otlpLogRequest{body: body, from: from, to: to})
		from = to
	}
	return requests, nil
}

// otlpLogsKey is the key the log export's failures are remembered under in
// the failure log: there is one log export.
var otlpLogsKey = subjectKey{bytes: subjectBytes(logExportSubject, "")}

// SetOTLPLogs gives the server the tee the exporter logs through, which it
// switches on and off as the configuration in force says, now and at every
// reload, and whose records OTLPLogExportLoop sends.
func (s *Server) SetOTLPLogs(logs *OTLPLogs) {
	s.otlpLogs = logs
	s.otlpLogsLogger = slog.New(logs.base)
	logs.configure(s.manager.Get().OTLP)
	s.manager.OnInstall(func() { logs.configure(s.manager.Get().OTLP) })
}

// OTLPLogExportLoop sends the records the tee queued every
// otlp.logs.interval until ctx ends. It returns without a last export,
// which FlushOTLP makes after the metrics' last.
func (s *Server) OTLPLogExportLoop(ctx context.Context) {
	if s.otlpLogs == nil {
		return
	}
	for {
		interval := time.Duration(s.manager.Get().OTLP.Logs.Interval)
		if interval <= 0 {
			interval = model.DefaultOTLPLogsInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.exportOTLPLogs(ctx, s.manager.Get().OTLP, interval, false)
	}
}

// flushOTLPLogs makes the last log export at shutdown, until deadline and
// for at least otlpLogsLastAttempt: what is left of otlp.timeout after the
// metrics' last export, which the lines it logged are among. The tee takes
// nothing after it.
func (s *Server) flushOTLPLogs(cfg model.OTLPConfig, deadline time.Time) {
	if s.otlpLogs == nil {
		return
	}
	s.otlpLogs.stop()
	s.exportOTLPLogs(context.Background(), cfg, max(time.Until(deadline), otlpLogsLastAttempt), true)
}

// otlpLogsLastAttempt is the least time the last log export at shutdown is
// given, however long the metrics' took.
const otlpLogsLastAttempt = time.Second

// exportOTLPLogs sends every record waiting, retrying within budget, in as
// many requests as otlp.batch_max_size and otlp.batch_max_bytes make of
// them, one after another. As the metrics' export: what could not be
// delivered for a reason worth retrying waits for the next export, before
// the records queued since; what the endpoint refused outright is dropped
// and counted, and the export goes on with the next request; and at
// shutdown, when last says there is no next export, what is left is
// dropped and counted.
func (s *Server) exportOTLPLogs(ctx context.Context, cfg model.OTLPConfig, budget time.Duration, last bool) {
	l := s.otlpLogs
	records := l.drain()
	if !otlpLogsOn(cfg) || len(records) == 0 {
		return
	}
	signal := otlpSignal{logs: true, logger: s.otlpLogsLogger}
	target := cfg
	target.Endpoint = cfg.Logs.Endpoint
	start := time.Now()
	deadline := start.Add(budget)
	requests, err := otlpLogRequests(cfg, records)
	sent, retries := 0, 0
	var rest [][]byte
	// failure is why the export failed: the error that left records
	// undelivered, or else the last refusal.
	var failure error
	var partial otlpPartialSuccess
	for k := 0; err == nil && k < len(requests); k++ {
		request := requests[k]
		if k > 0 && time.Until(deadline) < otlpMinAttempt {
			err = fmt.Errorf("the log export ran out of time after %d of its %d requests", k, len(requests))
			rest = records[request.from:]
			break
		}
		var tries int
		var answer otlpPartialSuccess
		tries, answer, err = s.deliverOTLPOf(ctx, signal, target, request.body, deadline)
		retries += tries
		if err != nil && ctx.Err() != nil {
			// Shutting down: the last export sends these.
			l.requeue(records[request.from:])
			return
		}
		count := request.to - request.from
		if err == nil {
			rejected := int(min(answer.rejected, int64(count)))
			sent += count - rejected
			if rejected > 0 || answer.message != "" {
				partial.rejected += int64(rejected)
				partial.message = answer.message
				l.drop(rejected, otlpLogDropRefused)
			}
			continue
		}
		var refused *otlpRefusedError
		if errors.As(err, &refused) {
			l.drop(count, otlpLogDropRefused)
			failure, err = err, nil
			continue
		}
		rest = records[request.from:]
	}
	if err != nil {
		failure = err
	}
	l.record(target.Endpoint, start, len(records), sent, failure)
	attrs := []any{"endpoint", redactedOTLPEndpoint(target.Endpoint), "records", len(records), "retries", retries}
	switch {
	case failure != nil && len(rest) > 0 && last:
		l.drop(len(rest), otlpLogDropShutdown)
		s.failures.failed(s.otlpLogsLogger, slog.LevelWarn, otlpLogsKey, "the last OTLP log export before exiting failed; its log records are dropped; this line is not exported", "export", failure, append(attrs, "dropped_records", len(rest))...)
	case failure != nil && len(rest) > 0:
		l.requeue(rest)
		s.failures.failed(s.otlpLogsLogger, slog.LevelWarn, otlpLogsKey, "OTLP log export failed; its log records are kept for the next export; this line is not exported", "export", failure, append(attrs, "kept_records", len(rest))...)
	case failure != nil:
		var refused *otlpRefusedError
		if errors.As(failure, &refused) && refused.body != "" {
			attrs = append(attrs, "response_body", refused.body)
		}
		s.failures.failed(s.otlpLogsLogger, slog.LevelWarn, otlpLogsKey, "OTLP endpoint refused a log export; its log records are dropped; this line is not exported", "refused", failure, attrs...)
	case partial.rejected > 0 || partial.message != "":
		why := partial.message
		if why == "" {
			why = "the OTLP endpoint gave no reason"
		}
		s.failures.failed(s.otlpLogsLogger, slog.LevelWarn, otlpLogsKey, "OTLP endpoint accepted a log export but rejected some of its log records; they are dropped; this line is not exported", "partial_success", errors.New(why), append(attrs, "rejected_records", partial.rejected)...)
	default:
		s.failures.recovered(s.otlpLogsLogger, otlpLogsKey, "OTLP log export recovered; this line is not exported", "endpoint", redactedOTLPEndpoint(target.Endpoint))
	}
}

// redactedOTLPEndpoint is an endpoint as the log and the status page show
// it: its userinfo and the values of query parameters named like
// credentials withheld.
func redactedOTLPEndpoint(endpoint string) string {
	return fetch.RedactURLString(endpoint, fetch.MaskCredentialQueryValues)
}

// otlpLogStatusMetrics renders the log export's status while it is on.
func (s *Server) otlpLogStatusMetrics() []model.Metric {
	if s.otlpLogs == nil || !otlpLogsOn(s.manager.Get().OTLP) {
		return nil
	}
	l := s.otlpLogs
	l.mu.Lock()
	defer l.mu.Unlock()
	metric := func(name string, value int, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Help: exporterMetricHelp[name], Type: model.CounterMetricType, Value: float64(value), Labels: labels}
	}
	return []model.Metric{
		metric("http_exporter_otlp_log_exports_total", l.successes, map[string]string{"result": "success"}),
		metric("http_exporter_otlp_log_exports_total", l.failures, map[string]string{"result": "failure"}),
		metric("http_exporter_otlp_log_records_sent_total", l.sent, nil),
		metric("http_exporter_otlp_log_records_dropped_total", l.droppedLocked(), nil),
	}
}
