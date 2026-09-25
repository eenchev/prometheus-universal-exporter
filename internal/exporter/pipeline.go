package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A probe and a static target's scrape make the same trip: fetch the
// target, decode the response, transform it and validate the result, with
// the collector's error_handling deciding what a failed stage does. collect
// is that trip, for both. What differs is around it: how a probe waits for a
// slot, what it answers and where a static target's result goes, which
// the callers keep (server.go, statictarget.go).

// collectJob is what one trip needs.
type collectJob struct {
	collector *model.Collector
	target    string
	overrides fetch.RequestOverrides
	headers   http.Header
	rec       statsRecorder
	// display is the target as logs and self-metrics show it.
	display  string
	cacheKey string
	// budget bounds the trip when the probe has a deadline (scrapetimeout.go);
	// a failure that ran out of it says so, and where it came from.
	budget       time.Duration
	budgetSource string
	// scrape is set for a static target's scrape, whose budget is its
	// interval, so an error it runs out of says so as a scrape's.
	scrape bool
	log    collectLog
}

// collectLog is how a caller's failures are logged: under which key of the
// failure log (failurelog.go), with which messages and attributes.
type collectLog struct {
	key                          string
	failed, continuing, recovery string
	attrs                        []any
}

// logCollectFailure logs a failure of stage at error level.
func (s *Server) logCollectFailure(l collectLog, stage string, err error, extra ...any) {
	s.logTripFailure(context.Background(), l, stage, err, extra...)
}

// logTripFailure is logCollectFailure for a trip, which a debug probe's
// report takes instead of the log (probedebug.go).
func (s *Server) logTripFailure(ctx context.Context, l collectLog, stage string, err error, extra ...any) {
	attrs := append(append(append([]any{}, l.attrs...), "stage", stage), extra...)
	s.tripFailed(ctx, slog.LevelError, l.key, l.failed, stage, err, attrs...)
}

// collected is how a trip ended.
type collected struct {
	// set is the validated result and answer the same with its freshness
	// series, when the trip went through whole.
	set    *model.MetricSet
	answer model.MetricSet
	// carriedOn is set when a stage failed under error_handling log or
	// ignore: the trip counts as a success with nothing to export.
	carriedOn bool
	// stage and err say why the trip failed, when it did; metric names the
	// metric rule with error_mode fail that failed it. The failure is
	// already logged.
	stage  string
	err    error
	metric string
	// aborted is set when the trip failed because it was cancelled (cutShort):
	// the exporter is shutting down (AbortStaticScrapes), or every probe
	// waiting for the trip went away. Nothing about the target is known, and
	// nothing was logged.
	aborted bool
	// refused is set when the collector's allowed_targets or
	// denied_targets refused the target: whatever error_handling says, the
	// trip fails, and a probe is answered 403.
	refused bool
}

// cutShort reports whether ctx was cancelled rather than run out of time: by
// a shutdown, or because every probe that waited for the trip went away.
// Either way nobody waits for its answer, and the target did not fail.
func cutShort(ctx context.Context) bool {
	return ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func (r collected) failed() bool { return r.err != nil }

// collect makes one trip to the target. It counts every stage in the
// collector's self-metrics, logs failures and recovery, and caches a result
// that went through whole.
// stageTargetPolicy is the stage of a trip the collector's allowed_targets or
// denied_targets refused.
const stageTargetPolicy = "target_policy"

func (s *Server) collect(ctx context.Context, j collectJob) collected {
	c, rec := j.collector, j.rec
	trace := probeTraceFrom(ctx)
	start := time.Now()
	defer func() {
		// The last-scrape timestamp and the duration histogram describe a trip
		// to the target, not an answer from the cache. A debug probe's trip
		// is in neither.
		if trace == nil {
			rec.scraped(time.Now())
			s.observeTargetScrape(c.Name, time.Since(start))
		}
	}()
	// stageFailed applies a stage's error policy.
	// extra attributes go to the log only, never into the probe's answer.
	// mark is when the stage in progress started, for a debug probe's
	// report.
	mark := start
	stageFailed := func(stage string, err error, policy string, extra ...any) collected {
		if cutShort(ctx) {
			trace.step(stage, "cancelled", time.Since(mark), err.Error())
			return collected{stage: stage, err: err, aborted: true}
		}
		trip := "probe"
		if j.scrape {
			trip = "scrape"
		}
		err = explainBudget(ctx, trip, j.budget, j.budgetSource, err)
		attrs := append(append(append([]any{}, j.log.attrs...), "stage", stage), extra...)
		switch policy {
		case model.ErrorPolicyLog, model.ErrorPolicyIgnore:
			trace.step(stage, "failed", time.Since(mark), err.Error()+"; carried on, error_handling "+policy)
		default:
			trace.step(stage, "failed", time.Since(mark), err.Error())
		}
		switch policy {
		case model.ErrorPolicyLog:
			s.tripFailed(ctx, slog.LevelWarn, j.log.key, j.log.continuing, stage, err, attrs...)
			return collected{carriedOn: true}
		case model.ErrorPolicyIgnore:
			s.tripDebug(ctx, j.log.continuing, append(attrs, "error", err)...)
			return collected{carriedOn: true}
		}
		s.logTripFailure(ctx, j.log, stage, err, extra...)
		return collected{stage: stage, err: err}
	}

	response, err := fetch.FetchCollector(ctx, j.target, c, j.overrides, j.headers)
	trace.record(func(t *probeTrace) { t.response = response })
	grpcCode, grpc := fetch.GRPCStatusCode(c, err)
	if err != nil {
		rec.update(func(x *serverStats) {
			// No response arrived.
			x.lastStatus = 0
			if errors.Is(err, model.ErrLimitExceeded) {
				x.limitErrors++
			}
			if grpc {
				x.grpcCode, x.grpcCalled = grpcCode, true
			}
		})
		if errors.Is(err, fetch.ErrTargetRefused) {
			rec.update(func(x *serverStats) { x.refused++ })
			trace.step(stageTargetPolicy, "refused", time.Since(mark), err.Error())
			s.logTripFailure(ctx, j.log, stageTargetPolicy, err)
			return collected{stage: stageTargetPolicy, err: err, refused: true}
		}
		var extra []any
		var status *fetch.CallStatusError
		if errors.As(err, &status) {
			extra = []any{"grpc_code", status.CodeName}
		}
		return stageFailed(fetch.FetchErrorStage(c, err), err, c.ErrorHandling.OnFetchError, extra...)
	}
	if response.GRPCCode != nil {
		grpcCode = *response.GRPCCode
	}
	trace.step(fetch.FetchStage(c), "ok", time.Since(mark), fetchedNote(response))
	rec.update(func(x *serverStats) {
		x.lastStatus = response.StatusCode
		x.lastBytes = int64(len(response.Body))
		if grpc {
			x.grpcCode, x.grpcCalled = grpcCode, true
		}
	})
	if !fetch.AcceptedStatus(c, j.overrides, response.StatusCode) {
		// The target's own explanation is usually in the body; the start of
		// it goes to the log, not to the answer, which Prometheus may keep.
		var excerpt []any
		if body := bodyExcerpt(response.Body); body != "" {
			excerpt = []any{"response_body", body}
		}
		mark = time.Now()
		return stageFailed("http_status", fmt.Errorf("received HTTP status %d", response.StatusCode), c.ErrorHandling.OnFetchError, excerpt...)
	}
	var set *model.MetricSet
	if response.Directory != nil {
		// A directory: each file is decoded and transformed on its own, and
		// one that fails is left out rather than failing the trip.
		// A read the shutdown cut short is no result of the target's
		// (statictargetschedule.go): its files are not reported failed.
		if response.Directory.CutShort && cutShort(ctx) {
			return collected{stage: fetch.FetchStage(c), err: context.Cause(ctx), aborted: true}
		}
		mark = time.Now()
		set = s.collectDirectory(ctx, response.Directory, c, rec, j.display)
		trace.step("files", "ok", time.Since(mark), fmt.Sprintf("%d files, %d series", len(response.Directory.Files), len(set.Metrics)))
		trace.record(func(t *probeTrace) { t.transform = set })
	} else {
		mark = time.Now()
		decoded, err := decode.Decode(response, c)
		if err != nil {
			rec.update(func(x *serverStats) { x.parseErrors++ })
			return stageFailed("decode", err, c.ErrorHandling.OnDecodeError)
		}
		trace.step("decode", "ok", time.Since(mark), decoded.Kind)
		trace.record(func(t *probeTrace) { t.decoded = decoded.Kind })
		rec.update(func(x *serverStats) { x.decodeOK++ })
		s.noteGraphite(ctx, decoded, c, rec, j.display, "")
		mark = time.Now()
		scriptCtx, timer := transform.WithScriptTimer(ctx)
		set, err = s.transformRecorded(scriptCtx, decoded, response, c, rec)
		recordScriptDuration(rec, timer)
		if err != nil {
			rec.update(func(x *serverStats) {
				if errors.Is(err, model.ErrMissingValue) {
					x.missing++
				}
				if errors.Is(err, model.ErrScriptFailed) {
					x.scriptErrors++
				}
				x.transformErrors++
			})
			// A metric rule with error_mode fail asked for the scrape to fail
			// when it cannot produce its value. That is a statement about this
			// one metric, more specific than the collector's
			// on_transform_error, so it is honoured even where the collector
			// would have carried on after a failed transform.
			var failure *transform.MetricFailure
			if errors.As(err, &failure) {
				if cutShort(ctx) {
					return collected{stage: "metric", err: err, aborted: true}
				}
				trace.step("transform", "failed", time.Since(mark), "metric "+failure.Metric+": "+err.Error())
				s.logTripFailure(ctx, j.log, "metric", err, "metric", failure.Metric)
				return collected{stage: "metric", err: err, metric: failure.Metric}
			}
			return stageFailed("transform", err, c.ErrorHandling.OnTransformError)
		}
		trace.step("transform", "ok", time.Since(mark), fmt.Sprintf("%d series", len(set.Metrics)))
		trace.record(func(t *probeTrace) { t.transform = set })
		s.sanitizeUTF8(ctx, set, rec, c, j.display)
	}
	mark = time.Now()
	if err := set.Validate(c.Limits); err != nil {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		return stageFailed("validation", err, model.ErrorPolicyFail)
	}
	now := time.Now()
	answer, err := withFreshness(*set, c, false, now, now)
	if err != nil {
		return stageFailed("validation", err, model.ErrorPolicyFail)
	}
	trace.step("validation", "ok", time.Since(mark), "")
	rec.update(func(x *serverStats) { x.emitted += uint64(len(set.Metrics)) })
	s.tripRecovered(ctx, j.log.key, j.log.recovery, j.log.attrs...)
	// A directory read the deadline cut short answers this probe with what
	// it read, but is not kept: the files it did not reach are fine as far
	// as anyone knows, and a cached copy would serve them failed. A debug
	// probe's result is not kept either.
	if trace == nil && (response.Directory == nil || !response.Directory.CutShort) {
		s.cache.Put(j.cacheKey, c.Name, *set, model.CacheTTL(c), model.StaleIfError(c), c.Limits.MaxCacheEntries, now)
	}
	return collected{set: set, answer: answer}
}

// transformRecorded transforms a decoded response, and counts in the
// collector's self-metrics the series its rules carried on without: each in
// http_exporter_rule_failures_total, and those whose value the response did
// not contain in http_exporter_missing_keys_total too.
func (s *Server) transformRecorded(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, rec statsRecorder) (*model.MetricSet, error) {
	ctx, report := transform.WithRuleReport(ctx)
	set, err := transform.Transform(ctx, d, r, c, s.pythonPath)
	failures := report.Failures()
	probeTraceFrom(ctx).record(func(t *probeTrace) { t.failures = append(t.failures, failures...) })
	if len(failures) == 0 {
		return set, err
	}
	rec.update(func(x *serverStats) {
		for _, f := range failures {
			x.missing += f.Missing
		}
	})
	if rec.collector != nil {
		rec.collector.mu.Lock()
		if rec.collector.ruleFailures == nil {
			rec.collector.ruleFailures = map[string]uint64{}
		}
		for _, f := range failures {
			rec.collector.ruleFailures[f.Metric] += f.Failures
		}
		rec.collector.mu.Unlock()
	}
	return set, err
}

// fetchedNote is a response as a debug probe's stages show it.
func fetchedNote(r *fetch.HTTPResponse) string {
	switch {
	case r.Directory != nil:
		return fmt.Sprintf("directory %s, %d files", r.Directory.Path, len(r.Directory.Files))
	case r.GRPCCode != nil:
		return fmt.Sprintf("gRPC status %d, %d bytes", *r.GRPCCode, len(r.Body))
	case r.NoStatus:
		return fmt.Sprintf("%d bytes", len(r.Body))
	}
	return fmt.Sprintf("status %d, %d bytes", r.StatusCode, len(r.Body))
}

// bodyExcerptBytes is how much of an error response's body the log shows.
const bodyExcerptBytes = 256

// bodyExcerpt is the start of a response body for a log line: at most
// bodyExcerptBytes, with invalid UTF-8 and
// control characters replaced and runs of whitespace collapsed, so an HTML
// error page or a binary body reads as one tidy line. It ends in … when cut.
func bodyExcerpt(body []byte) string {
	cut := len(body) > bodyExcerptBytes
	if cut {
		// A character cut in two is replaced below like any invalid UTF-8.
		body = body[:bodyExcerptBytes]
	}
	text := strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(string(body), " "))
	text = strings.Join(strings.Fields(text), " ")
	if cut && text != "" {
		text += "…"
	}
	return text
}
