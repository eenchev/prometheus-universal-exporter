package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// scheduledTargetConcurrency bounds how many scheduled targets are scraped at
// once, so a large target file cannot open an unbounded number of connections.
const scheduledTargetConcurrency = 8

// scrapeScheduledTargets runs every configured target once. It is called from
// the OTLP export loop, so the scrape period is the OTLP interval and every
// export carries a freshly collected set. The whole pass is bounded by the
// interval, so a slow target cannot delay the next export indefinitely.
func (s *Server) scrapeScheduledTargets(ctx context.Context, budget time.Duration) {
	targets := s.manager.Targets()
	if len(targets) == 0 {
		return
	}
	cfg := s.manager.Get()
	if budget <= 0 {
		budget = 30 * time.Second
	}
	scrapeCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	slots := make(chan struct{}, scheduledTargetConcurrency)
	var wg sync.WaitGroup
	for i := range targets {
		target := targets[i]
		collector := collectorByName(cfg, target.Collector)
		if collector == nil {
			s.logger.Error("scheduled target references unknown collector", "target", target.Name, "collector", target.Collector)
			continue
		}
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.scrapeScheduledTarget(scrapeCtx, target, collector, cfg.OTLP)
		}()
	}
	wg.Wait()
}

// scrapeScheduledTarget collects one target through the same fetch, decode, and
// transform path as /probe, including the collector response cache, and stages
// the result under the target's own OTLP resource.
func (s *Server) scrapeScheduledTarget(ctx context.Context, target ScheduledTarget, c *Collector, cfg OTLPConfig) {
	start := time.Now()
	identity := target.resource(cfg)
	address := displayTarget(c, target.Target)
	overrides := target.overrides()
	method := requestMethodFor(c, overrides)
	requestURL := ""
	if label, err := requestLabelFor(target.Target, c, overrides); err == nil {
		requestURL = label
	}
	// The request is identified before anything is counted, so every counter
	// this collection raises lands on the target's own series as well as on the
	// collector's.
	rec := s.recorderFor(s.statsFor(c.Name), c.Name, requestURL, method)
	count := rec.update
	count(func(st *serverStats) { st.probes++ })
	// Set once the exporter has actually gone to the target, so a collection
	// answered from the cache does not move the last-scrape timestamp.
	scraped := false

	finish := func(up float64) {
		elapsed := time.Since(start)
		count(func(st *serverStats) { st.lastDuration = elapsed.Seconds() })
		if scraped {
			rec.scraped(time.Now())
			s.observeTargetScrape(c.Name, elapsed)
		}
		s.queueOTLPResource(scheduledHealthMetrics(target, c, up, elapsed.Seconds()), identity)
	}
	fail := func(stage string, err error) {
		s.logger.Error("scheduled target scrape failed", "target", target.Name, "collector", c.Name, "address", address, "stage", stage, "error", err)
		finish(0)
	}

	headers, err := target.headers()
	if err != nil {
		fail("credentials", err)
		return
	}
	cacheTTL := time.Duration(c.Cache)
	var cacheKey string
	if cacheTTL > 0 {
		cacheKey = probeCacheKey(c, target.Target, target.cacheQuery(), headers)
		if cached, ok := s.cache.Get(cacheKey, time.Now()); ok {
			count(func(st *serverStats) {
				st.cacheHits++
				st.success++
				st.emitted += uint64(len(cached.Metrics))
			})
			s.queueOTLPResource(withTargetLabels(cached, target.Labels), identity)
			finish(1)
			return
		}
		count(func(st *serverStats) { st.cacheMisses++ })
	}

	// A scheduled scrape shares the collector's max_concurrent_probes with its
	// probes, and waits for a slot within its budget rather than failing.
	if err := s.trips.acquire(ctx, c.Name, maxConcurrentProbes(c)); err != nil {
		count(func(st *serverStats) { st.rejected++ })
		fail("concurrency", err)
		return
	}
	defer s.trips.release(c.Name)
	response, err := fetchCollector(ctx, target.Target, c, overrides, headers)
	scraped = true
	if err != nil {
		if errors.Is(err, errLimitExceeded) {
			count(func(st *serverStats) { st.limitErrors++ })
		}
		fail(fetchStage(c), err)
		return
	}
	count(func(st *serverStats) {
		st.lastStatus = response.StatusCode
		st.lastBytes = int64(len(response.Body))
	})
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fail("http_status", fmt.Errorf("received HTTP status %d", response.StatusCode))
		return
	}
	var set *MetricSet
	if response.Directory != nil {
		set = s.collectDirectory(ctx, response.Directory, c, rec, address)
	} else {
		decoded, err := decode(response, c)
		if err != nil {
			count(func(st *serverStats) { st.parseErrors++ })
			fail("decode", err)
			return
		}
		count(func(st *serverStats) { st.decodeOK++ })
		scriptCtx, timer := withScriptTimer(ctx)
		set, err = transform(scriptCtx, decoded, response, c, s.pythonPath)
		recordScriptDuration(rec, timer)
		if err != nil {
			count(func(st *serverStats) {
				st.transformErrors++
				if errors.Is(err, errMissingValue) {
					st.missing++
				}
				if errors.Is(err, errScriptFailed) {
					st.scriptErrors++
				}
			})
			fail("transform", err)
			return
		}
	}
	if err := set.Validate(c.Limits); err != nil {
		count(func(st *serverStats) { st.limitErrors++ })
		fail("validation", err)
		return
	}
	s.cache.Put(cacheKey, c.Name, *set, cacheTTL, c.Limits.MaxCacheEntries, time.Now())
	count(func(st *serverStats) {
		st.success++
		st.emitted += uint64(len(set.Metrics))
	})
	s.queueOTLPResource(withTargetLabels(*set, target.Labels), identity)
	finish(1)
}

// scheduledHealthMetrics reports the outcome of one scheduled scrape. Without
// it a failing target is simply absent from the OTLP stream, which cannot be
// distinguished from a target that was never configured.
func scheduledHealthMetrics(target ScheduledTarget, c *Collector, up, duration float64) MetricSet {
	labels := map[string]string{"collector": c.Name, "scheduled_target": target.Name, "target": displayTarget(c, target.Target)}
	for name, value := range target.Labels {
		if _, exists := labels[name]; !exists {
			labels[name] = value
		}
	}
	return MetricSet{Metrics: []Metric{
		{Name: "http_exporter_target_up", Help: "Whether the last scheduled scrape of this target succeeded.", Type: GaugeMetricType, Value: up, Labels: cloneLabels(labels)},
		{Name: "http_exporter_target_scrape_duration_seconds", Help: "Duration of the last scheduled scrape of this target in seconds.", Type: GaugeMetricType, Value: duration, Labels: cloneLabels(labels)},
	}}
}

// withTargetLabels adds a target's configured labels to every metric it
// produced. A label the collector already extracted is never overwritten, so
// declared metric labels keep precedence over target-wide ones. The cached
// metric set stays unlabelled, which is what lets a scheduled scrape and an
// equivalent probe share cache entries.
func withTargetLabels(set MetricSet, labels map[string]string) MetricSet {
	if len(labels) == 0 {
		return set
	}
	out := cloneMetricSet(set)
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

func collectorByName(cfg *Config, name string) *Collector {
	for i := range cfg.Collectors {
		if cfg.Collectors[i].Name == name {
			return &cfg.Collectors[i]
		}
	}
	return nil
}
