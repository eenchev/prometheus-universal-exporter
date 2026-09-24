package exporter

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// scheduledTargetConcurrency bounds how many scheduled targets are scraped at
// once, so a large target file cannot open an unbounded number of connections.
const scheduledTargetConcurrency = 8

// scrapeTarget scrapes one scheduled target with the configuration in force.
func (s *Server) scrapeTarget(ctx context.Context, target model.ScheduledTarget) {
	cfg := s.manager.Get()
	collector := model.CollectorByName(cfg, target.Collector)
	if collector == nil {
		s.logger.Error("scheduled target references unknown collector", "target", target.Name, "collector", target.Collector)
		return
	}
	s.scrapeScheduledTarget(ctx, target, cfg, collector)
}

// scrapeScheduledTarget collects one target through the same fetch, decode, and
// transform path as /probe, including the collector response cache, and stages
// the result under the target's own OTLP resource.
func (s *Server) scrapeScheduledTarget(ctx context.Context, target model.ScheduledTarget, cfg *model.Config, c *model.Collector) {
	start := time.Now()
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
		key:    failureKey(c.Name, "scheduled target "+target.Name, ""),
		failed: "scheduled target scrape failed", continuing: "scheduled target stage failed; continuing", recovery: "scheduled target recovered",
		attrs: []any{"target", target.Name, "collector", c.Name, "address", address},
	}
	finish := func(up float64) {
		elapsed := time.Since(start)
		count(func(st *serverStats) { st.lastDuration = elapsed.Seconds() })
		s.queueOTLPResource(scheduledHealthMetrics(target, c, up, elapsed.Seconds()), identity)
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
					s.queueOTLPResource(withTargetLabels(answer, target.Labels), identity)
				}
			}
		}
		finish(0)
	}

	headers, err := fetch.TargetHeaders(&target)
	if err != nil {
		s.logCollectFailure(log, "credentials", err)
		failed()
		return
	}
	if model.UsesCache(c) {
		cacheKey = s.probeCacheKey(cfg, c, target.Target, targetCacheQuery(&target), headers)
	}
	if model.CacheTTL(c) > 0 {
		if cached, fetched, ok := s.cache.Get(cacheKey, time.Now()); ok {
			count(func(st *serverStats) {
				st.cacheHits++
				st.success++
				st.emitted += uint64(len(cached.Metrics))
			})
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			s.queueOTLPResource(withTargetLabels(answer, target.Labels), identity)
			finish(1)
			return
		}
		count(func(st *serverStats) { st.cacheMisses++ })
	}

	// A scheduled scrape shares the collector's max_concurrent_probes with its
	// probes, and waits for a slot within its budget rather than failing.
	if err := s.trips.acquire(ctx, c.Name, maxConcurrentProbes(c)); err != nil {
		count(func(st *serverStats) { st.rejected++ })
		s.logCollectFailure(log, "concurrency", err)
		failed()
		return
	}
	defer s.trips.release(c.Name)
	result := s.collect(ctx, collectJob{
		collector: c, target: target.Target, overrides: overrides, headers: headers,
		rec: rec, display: address, cacheKey: cacheKey, log: log,
	})
	if result.failed() {
		failed()
		return
	}
	// Under error_handling log or ignore a failed stage leaves the target up
	// with nothing of the collector's to export, as it answers a probe 200
	// with an empty body.
	count(func(st *serverStats) { st.success++ })
	if !result.carriedOn {
		s.queueOTLPResource(withTargetLabels(result.answer, target.Labels), identity)
	}
	finish(1)
}

// scheduledHealthMetrics reports the outcome of one scheduled scrape. Without
// it a failing target is simply absent from the OTLP stream, which cannot be
// distinguished from a target that was never configured.
func scheduledHealthMetrics(target model.ScheduledTarget, c *model.Collector, up, duration float64) model.MetricSet {
	labels := map[string]string{"collector": c.Name, "scheduled_target": target.Name, "target": fetch.DisplayTarget(c, target.Target)}
	for name, value := range target.Labels {
		if _, exists := labels[name]; !exists {
			labels[name] = value
		}
	}
	return model.MetricSet{Metrics: []model.Metric{
		{Name: "http_exporter_target_up", Help: "Whether the last scheduled scrape of this target succeeded.", Type: model.GaugeMetricType, Value: up, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_scrape_duration_seconds", Help: "Duration of the last scheduled scrape of this target in seconds.", Type: model.GaugeMetricType, Value: duration, Labels: model.CloneLabels(labels)},
	}}
}

// withTargetLabels adds a target's configured labels to every metric it
// produced. A label the collector already extracted is never overwritten, so
// declared metric labels keep precedence over target-wide ones. The cached
// metric set stays unlabelled, which is what lets a scheduled scrape and an
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
// the same request, so a scheduled scrape and an equivalent probe share cache
// entries and a differing one never does.
func targetCacheQuery(t *model.ScheduledTarget) url.Values {
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
	return values
}

// targetResource resolves the OTLP resource identity for this target, with the
// exporter-wide service name and attributes as the defaults.
func targetResource(t *model.ScheduledTarget, cfg model.OTLPConfig) otlpResourceIdentity {
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
