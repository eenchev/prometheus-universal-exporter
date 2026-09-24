package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A probe and a scheduled target's scrape make the same trip: fetch the
// target, decode the response, transform it and validate the result, with
// the collector's error_handling deciding what a failed stage does. collect
// is that trip, for both. What differs is around it: how a probe waits for a
// slot, what it answers and where a scheduled target's result goes, which
// the callers keep (server.go, scheduled.go).

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
	// budget bounds the trip when Prometheus said how long it will wait; a
	// failure that ran out of it says so.
	budget time.Duration
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
	attrs := append(append(append([]any{}, l.attrs...), "stage", stage), extra...)
	s.failures.failed(s.logger, slog.LevelError, l.key, l.failed, stage, err, attrs...)
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
}

func (r collected) failed() bool { return r.err != nil }

// collect makes one trip to the target. It counts every stage in the
// collector's self-metrics, logs failures and recovery, and caches a result
// that went through whole.
func (s *Server) collect(ctx context.Context, j collectJob) collected {
	c, rec := j.collector, j.rec
	start := time.Now()
	defer func() {
		// The last-scrape timestamp and the duration histogram describe a trip
		// to the target, not an answer from the cache.
		rec.scraped(time.Now())
		s.observeTargetScrape(c.Name, time.Since(start))
	}()
	// stageFailed applies a stage's error policy.
	stageFailed := func(stage string, err error, policy string) collected {
		err = explainBudget(ctx, j.budget, err)
		attrs := append(append([]any{}, j.log.attrs...), "stage", stage)
		switch policy {
		case model.ErrorPolicyLog:
			s.failures.failed(s.logger, slog.LevelWarn, j.log.key, j.log.continuing, stage, err, attrs...)
			return collected{carriedOn: true}
		case model.ErrorPolicyIgnore:
			s.logger.Debug(j.log.continuing, append(attrs, "error", err)...)
			return collected{carriedOn: true}
		}
		s.logCollectFailure(j.log, stage, err)
		return collected{stage: stage, err: err}
	}

	response, err := fetch.FetchCollector(ctx, j.target, c, j.overrides, j.headers)
	if err != nil {
		if errors.Is(err, model.ErrLimitExceeded) {
			rec.update(func(x *serverStats) { x.limitErrors++ })
		}
		return stageFailed(fetch.FetchStage(c), err, c.ErrorHandling.OnFetchError)
	}
	rec.update(func(x *serverStats) {
		x.lastStatus = response.StatusCode
		x.lastBytes = int64(len(response.Body))
	})
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return stageFailed("http_status", fmt.Errorf("received HTTP status %d", response.StatusCode), c.ErrorHandling.OnFetchError)
	}
	var set *model.MetricSet
	if response.Directory != nil {
		// A directory: each file is decoded and transformed on its own, and
		// one that fails is left out rather than failing the trip.
		set = s.collectDirectory(ctx, response.Directory, c, rec, j.display)
	} else {
		decoded, err := decode.Decode(response, c)
		if err != nil {
			rec.update(func(x *serverStats) { x.parseErrors++ })
			return stageFailed("decode", err, c.ErrorHandling.OnDecodeError)
		}
		rec.update(func(x *serverStats) { x.decodeOK++ })
		scriptCtx, timer := transform.WithScriptTimer(ctx)
		set, err = transform.Transform(scriptCtx, decoded, response, c, s.pythonPath)
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
				s.logCollectFailure(j.log, "metric", err, "metric", failure.Metric)
				return collected{stage: "metric", err: err, metric: failure.Metric}
			}
			return stageFailed("transform", err, c.ErrorHandling.OnTransformError)
		}
		s.sanitizeUTF8(set, rec, c, j.display)
	}
	if err := set.Validate(c.Limits); err != nil {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		return stageFailed("validation", err, model.ErrorPolicyFail)
	}
	now := time.Now()
	answer, err := withFreshness(*set, c, false, now, now)
	if err != nil {
		return stageFailed("validation", err, model.ErrorPolicyFail)
	}
	rec.update(func(x *serverStats) { x.emitted += uint64(len(set.Metrics)) })
	s.failures.recovered(s.logger, j.log.key, j.log.recovery, j.log.attrs...)
	s.cache.Put(j.cacheKey, c.Name, *set, model.CacheTTL(c), model.StaleIfError(c), c.Limits.MaxCacheEntries, now)
	return collected{set: set, answer: answer}
}
