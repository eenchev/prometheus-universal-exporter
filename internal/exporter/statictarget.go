package exporter

import (
	"context"
	"fmt"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// scrapeTarget scrapes one static target with the configuration in force.
func (s *Server) scrapeTarget(ctx context.Context, target model.StaticTarget) {
	cfg := s.manager.Get()
	collector := model.CollectorByName(cfg, target.Collector)
	if collector == nil {
		s.logger.Error("static target references unknown collector", "target", target.Name, "collector", target.Collector)
		return
	}
	s.scrapeStaticTarget(ctx, target, cfg, collector)
}

// scrapeStaticTarget collects one target through the same fetch, decode, and
// transform path as /probe, including the collector response cache, and
// publishes the result with the target's health metrics: as its latest result
// on the static targets endpoint, and, with export_via_otlp, queued for OTLP
// under the target's own resource (statictargetsendpoint.go).
func (s *Server) scrapeStaticTarget(ctx context.Context, target model.StaticTarget, cfg *model.Config, c *model.Collector) {
	start := time.Now()
	// A static target scrape runs on a goroutine of the scrape loop, where a panic
	// would take the whole exporter down rather than one scrape, as a probe's
	// does (probeflight.go). It is logged and the scrape ends as failed, once
	// the scrape has got far enough to be counted.
	var failedOnPanic func()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("static target scrape panicked", "target", target.Name, "collector", c.Name, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			if failedOnPanic != nil {
				failedOnPanic()
			}
		}
	}()
	identity := targetResource(&target, cfg.OTLP)
	address := fetch.DisplayTarget(c, target.Target)
	overrides := fetch.TargetOverrides(&target)
	method := fetch.RequestMethodFor(c, overrides)
	requestURL := ""
	if label, err := fetch.RequestLabelFor(target.Target, c, overrides); err == nil {
		requestURL = label
	}
	// The request is identified before anything is counted, so every counter
	// this collection raises lands on the target's own series as well as on the
	// collector's.
	rec := s.recorderFor(s.statsFor(c.Name), c.Name, requestURL, method)
	count := rec.update
	count(func(st *serverStats) { st.probes++ })

	// Repeats of the same failure are logged sparingly (failurelog.go).
	log := collectLog{
		key:    failureKey(c.Name, "static target "+target.Name, ""),
		failed: "static target scrape failed", continuing: "static target stage failed; continuing", recovery: "static target recovered",
		attrs: []any{"target", target.Name, "collector", c.Name, "address", address},
	}
	// result is what the scrape publishes besides the health metrics: the
	// target's metrics, or a stale result in their place.
	var result model.MetricSet
	finish := func(up float64) {
		// The scrape is counted once, even when what follows panics.
		failedOnPanic = nil
		elapsed := time.Since(start)
		count(func(st *serverStats) { st.lastDuration = elapsed.Seconds() })
		// A new slice, so the health metrics are never appended into one the
		// result shares with a set that was cached.
		lastSuccess := s.recordStaticTargetOutcome(target.Name, up == 1, time.Now())
		health := staticTargetHealthMetrics(target, c, up, elapsed.Seconds(), lastSuccess).Metrics
		published := model.MetricSet{Metrics: make([]model.Metric, 0, len(result.Metrics)+len(health))}
		published.Metrics = append(append(published.Metrics, result.Metrics...), health...)
		s.publishStaticTarget(target, identity, published)
	}
	// cacheKey is set once the request is known; a failure before it has no
	// cached result to fall back on.
	var cacheKey string
	// failed ends a scrape that failed, and was logged. With
	// cache.stale_if_error the last good result is exported in place of
	// nothing, marked stale; http_exporter_target_up still says the scrape
	// failed.
	failed := func() {
		if model.StaleIfError(c) > 0 && cacheKey != "" {
			now := time.Now()
			if cached, fetched, ok := s.cache.GetStale(cacheKey, now); ok {
				if answer, err := withFreshness(cached, c, true, fetched, now); err == nil {
					count(func(st *serverStats) {
						st.staleServed++
						st.emitted += uint64(len(cached.Metrics))
					})
					result = withTargetLabels(answer, target.Labels)
				}
			}
		}
		finish(0)
	}
	failedOnPanic = failed

	headers, err := fetch.TargetHeaders(&target)
	if err != nil {
		s.logCollectFailure(log, "credentials", err)
		failed()
		return
	}
	if model.UsesCache(c) {
		cacheKey = s.probeCacheKey(cfg, c, target.Target, targetCacheQuery(&target), headers, targetOwnRequest(&target)...)
	}
	if model.CacheTTL(c) > 0 {
		if cached, fetched, ok := s.cache.Get(cacheKey, time.Now()); ok {
			count(func(st *serverStats) {
				st.cacheHits++
				st.success++
				st.emitted += uint64(len(cached.Metrics))
			})
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			result = withTargetLabels(answer, target.Labels)
			finish(1)
			return
		}
		count(func(st *serverStats) { st.cacheMisses++ })
	}

	// A static target scrape shares the collector's max_concurrent_probes with its
	// probes, and waits for a slot within its budget rather than failing.
	if err := s.trips.acquire(ctx, c.Name, maxConcurrentProbes(c)); err != nil {
		if shuttingDown(ctx) {
			s.abortedByShutdown(target, c)
			return
		}
		count(func(st *serverStats) { st.rejected++ })
		s.logCollectFailure(log, "concurrency", err)
		failed()
		return
	}
	defer s.trips.release(c.Name)
	trip := s.collect(ctx, collectJob{
		collector: c, target: target.Target, overrides: overrides, headers: headers,
		rec: rec, display: address, cacheKey: cacheKey, log: log,
	})
	if trip.aborted {
		failedOnPanic = nil
		s.abortedByShutdown(target, c)
		return
	}
	if trip.failed() {
		failed()
		return
	}
	// Under error_handling log or ignore a failed stage leaves the target up
	// with nothing of the collector's to export, as it answers a probe 200
	// with an empty body.
	count(func(st *serverStats) { st.success++ })
	if !trip.carriedOn {
		result = withTargetLabels(trip.answer, target.Labels)
	}
	finish(1)
}

// abortedByShutdown notes a scrape the shutdown cut short
// (AbortStaticScrapes): it publishes nothing and is no failure of the
// target's, whose last result stands.
func (s *Server) abortedByShutdown(target model.StaticTarget, c *model.Collector) {
	s.logger.Debug("static target scrape cut short by the shutdown; its last result stands", "target", target.Name, "collector", c.Name)
}

// staticTargetHealthMetrics reports the outcome of one static target scrape.
// Without it a failing target would simply be absent from the endpoint and the
// OTLP stream, which cannot be told from a target that was never configured.
// lastSuccess is when the target was last scraped successfully, zero if never:
// the endpoint keeps serving a target's last values while its scrapes are
// skipped or failing, and the timestamp is what tells how old they are.
func staticTargetHealthMetrics(target model.StaticTarget, c *model.Collector, up, duration float64, lastSuccess time.Time) model.MetricSet {
	labels := map[string]string{"collector": c.Name, "static_target": target.Name, "target": fetch.DisplayTarget(c, target.Target)}
	for name, value := range target.Labels {
		if _, exists := labels[name]; !exists {
			labels[name] = value
		}
	}
	return model.MetricSet{Metrics: []model.Metric{
		{Name: "http_exporter_target_up", Help: "Whether the last scrape of this static target succeeded.", Type: model.GaugeMetricType, Value: up, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_scrape_duration_seconds", Help: "Duration of the last scrape of this static target in seconds.", Type: model.GaugeMetricType, Value: duration, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_last_success_timestamp_seconds", Help: "Unix time of the last successful scrape of this static target; 0 if none has succeeded.", Type: model.GaugeMetricType, Value: unixSeconds(lastSuccess), Labels: model.CloneLabels(labels)},
	}}
}

// withTargetLabels adds a target's configured labels to every metric it
// produced. A label the collector already extracted is never overwritten, so
// declared metric labels keep precedence over target-wide ones. The cached
// metric set stays unlabelled, which is what lets a static target scrape and an
// equivalent probe share cache entries.
func withTargetLabels(set model.MetricSet, labels map[string]string) model.MetricSet {
	if len(labels) == 0 {
		return set
	}
	out := model.CloneMetricSet(set)
	for i := range out.Metrics {
		if out.Metrics[i].Labels == nil {
			out.Metrics[i].Labels = map[string]string{}
		}
		for name, value := range labels {
			if _, exists := out.Metrics[i].Labels[name]; !exists {
				out.Metrics[i].Labels[name] = value
			}
		}
	}
	return out
}

// targetCacheQuery reproduces the /probe query a caller would have to send to make
// the same request, so a static target scrape and an equivalent probe share cache
// entries and a differing one never does.
func targetCacheQuery(t *model.StaticTarget) url.Values {
	values := url.Values{"target": {t.Target}, "collector": {t.Collector}}
	for name, value := range t.Params {
		values.Set(name, value)
	}
	if t.Request.Method != "" {
		values.Set("method", t.Request.Method)
	}
	if t.Request.PathSet {
		values.Set("path", t.Request.Path)
	}
	if t.Request.BodySet {
		values.Set("body", t.Request.Body)
	}
	if t.Request.Timeout > 0 {
		values.Set("timeout", time.Duration(t.Request.Timeout).String())
	}
	if t.Request.InsecureSkipVerify != nil {
		values.Set("insecure_skip_verify", strconv.FormatBool(*t.Request.InsecureSkipVerify))
	}
	if t.Request.FollowRedirects != nil {
		values.Set("follow_redirects", strconv.FormatBool(*t.Request.FollowRedirects))
	}
	if t.Request.EnableHTTP2 != nil {
		values.Set("enable_http2", strconv.FormatBool(*t.Request.EnableHTTP2))
	}
	if t.Request.Retry != nil {
		values.Set("retry_attempts", strconv.Itoa(t.Request.Retry.Attempts))
		values.Set("retry_backoff", time.Duration(t.Request.Retry.Backoff).String())
	}
	if from := strings.TrimSpace(t.Request.From); from != "" {
		values.Set("from", from)
	}
	if until := strings.TrimSpace(t.Request.Until); until != "" {
		values.Set("until", until)
	}
	return values
}

// targetOwnRequest is what the target's request sets that no probe can, for
// its cache key: a graphite collector's targets.
func targetOwnRequest(t *model.StaticTarget) []string {
	if len(t.Request.Targets) == 0 {
		return nil
	}
	return append([]string{"targets"}, t.Request.Targets...)
}

// targetResource resolves the OTLP resource identity for this target, with the
// exporter-wide service name and attributes as the defaults.
func targetResource(t *model.StaticTarget, cfg model.OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		identity.Attributes[key] = value
	}
	if t.OTLP.ServiceName != "" {
		identity.ServiceName = t.OTLP.ServiceName
	}
	for key, value := range t.OTLP.ResourceAttributes {
		identity.Attributes[key] = value
	}
	return identity
}
