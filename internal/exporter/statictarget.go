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

// scrapeTarget scrapes one static target with cfg, the configuration that was
// in force together with it (config.Manager's InForce). The caller has just
// read the two. When cfg is no longer in force, the scrape is still made but
// counted nowhere that is shown: without the generation cfg was read at,
// nothing says whether its collector has been in the configuration ever since
// (scrapeTargetSince).
func (s *Server) scrapeTarget(ctx context.Context, cfg *model.Config, target model.StaticTarget) {
	s.scrapeTargetSince(ctx, cfg, s.generationOf(cfg), target)
}

// scrapeTargetSince scrapes one static target with cfg, the configuration
// that was in force together with it, read at generation
// (Server.followedInForce). The scrape may begin long after, when it has
// waited for a slot. It counts in its collector's statistics only when no
// reload has removed the collector meanwhile (statsSince). While cfg is the
// followed configuration, the collector is found by its name where that
// configuration keeps its position. Once a reload has come, it is found by
// going through the collectors of cfg, as it was for every scrape (collectorOf).
func (s *Server) scrapeTargetSince(ctx context.Context, cfg *model.Config, generation uint64, target model.StaticTarget) {
	collector := s.followed.Load().collectorOf(cfg, target.Collector)
	if collector == nil {
		s.logger.Error("static target references unknown collector", "target", target.Name, "collector", target.Collector)
		return
	}
	s.scrapeStaticTarget(ctx, target, cfg, generation, collector)
}

// scrapeStaticTarget collects one target through the same fetch, decode, and
// transform path as /probe, including the collector response cache. It
// publishes the result with the target's health metrics as its latest result
// on the static targets endpoint. With export_via_otlp, it also queues it for
// OTLP under the target's own resource (statictargetsendpoint.go).
func (s *Server) scrapeStaticTarget(ctx context.Context, target model.StaticTarget, cfg *model.Config, generation uint64, c *model.Collector) {
	sc := &staticScrape{s: s, target: target, cfg: cfg, c: c, start: time.Now()}
	// A static target scrape runs on a goroutine of the scrape loop. A panic
	// there would take the whole exporter down, not just one scrape as a
	// probe's panic does (probeflight.go). The panic is logged and the scrape
	// ends as failed, once the scrape has got far enough to be counted.
	defer sc.recoverPanic()
	ctx = sc.begin(ctx, generation)
	sc.failedOnPanic = func() { sc.failed(errScrapePanicked) }

	headers, err := fetch.TargetHeaders(&sc.target)
	if err != nil {
		s.logCollectFailure(ctx, sc.rec.read, sc.log, "credentials", err)
		sc.failed(err)
		return
	}
	if model.UsesCache(c) {
		sc.cacheKey = s.probeCacheKey(cfg, c, target.Target, targetCacheQuery(&sc.target), headers, targetOwnRequest(&sc.target)...)
	}
	if sc.answeredFromCache() {
		return
	}

	job := collectJob{
		collector: c, target: target.Target, overrides: sc.overrides, headers: headers,
		rec: sc.rec, display: sc.address, cacheKey: sc.cacheKey, revalidateKey: sc.cacheKey, log: sc.log,
		scrape: true, budget: time.Duration(target.Interval), budgetSource: budgetFromInterval,
		lastSuccess: lastSuccessKey(c, &sc.target, target.Target, sc.overrides),
	}
	// A target that failure_backoff is backing off from is not contacted,
	// and its scrape waits for no slot. It fails at once, as its fetch would
	// have (backoff.go). It is identified like the identical probe, by the
	// key of its request, whether or not the collector caches.
	backoffKey := sc.cacheKey
	if c.FailureBackoff != nil && backoffKey == "" {
		backoffKey = s.probeCacheKey(cfg, c, target.Target, targetCacheQuery(&sc.target), headers, targetOwnRequest(&sc.target)...)
	}
	if c.FailureBackoff != nil {
		sc.status.backoffKey = backoffKey
	}
	turn := s.backoffs.admit(c, backoffKey, sc.address)
	defer turn.settle(sc.rec.read, backoffUnknown)
	var trip collected
	if turn != nil && turn.refusal != nil {
		job.backingOff = turn.refusal
		trip = s.collect(ctx, job)
		trip.unauthorized = turn.unauthorized
	} else {
		if !sc.acquireSlot(ctx) {
			return
		}
		defer s.trips.release(c.Name)
		if model.CacheTTL(c) > 0 {
			sc.count(func(st *serverStats) { st.cacheMisses++ })
		}
		trip = s.collect(ctx, job)
		s.settleBackoff(turn, sc.rec.read, trip, sc.log.attrs)
	}
	sc.ended(trip)
}

// staticScrape is the state of one scrape of scrapeStaticTarget.
type staticScrape struct {
	s      *Server
	target model.StaticTarget
	cfg    *model.Config
	c      *model.Collector
	start  time.Time

	identity  otlpResourceIdentity
	address   string
	overrides fetch.RequestOverrides
	rec       statsRecorder
	spans     *tripSpans
	log       collectLog
	// result is what the scrape publishes besides the health metrics: the
	// target's metrics, or a stale result in their place.
	result model.MetricSet
	// fetched is when the data in result came from the target: now for a
	// trip just made, the cache entry's time for a cached or stale result.
	fetched time.Time
	// status is what the status page shows of this scrape
	// (statictargetstatus.go), filled in as the scrape goes.
	status staticScrapeStatus
	// cacheKey is set once the request is known; a failure before it has no
	// cached result to fall back on.
	cacheKey string
	// failedOnPanic ends the scrape as failed when it panics, once it has
	// got far enough to be counted.
	failedOnPanic func()
}

// recoverPanic logs a panic of the scrape, and ends the scrape as failed.
func (sc *staticScrape) recoverPanic() {
	if recovered := recover(); recovered != nil {
		sc.s.logger.Error("static target scrape panicked", "target", sc.target.Name, "collector", sc.c.Name, "panic", panicText(recovered), "stack", string(debug.Stack()))
		if sc.failedOnPanic != nil {
			sc.failedOnPanic()
		}
	}
}

// count counts in the scrape's statistics.
func (sc *staticScrape) count(f func(*serverStats)) { sc.rec.update(f) }

// begin counts the scrape and begins its trace, which the context it
// returns carries.
func (sc *staticScrape) begin(ctx context.Context, generation uint64) context.Context {
	s, c, target := sc.s, sc.c, &sc.target
	sc.identity = targetResource(target, sc.cfg.OTLP)
	sc.address = fetch.DisplayTarget(c, target.Target)
	sc.overrides = fetch.TargetOverrides(target)
	method := fetch.RequestMethodFor(c, sc.overrides)
	requestURL := ""
	if label, err := fetch.RequestLabelFor(target.Target, c, sc.overrides); err == nil {
		requestURL = label
	}
	// The request is identified before anything is counted, so every counter
	// this collection raises lands on the target's own series as well as on the
	// collector's.
	sc.rec = s.recorderFor(s.statsSince(generation, c.Name), c.Name, requestURL, method)
	// Once a reload has removed or changed the collector read at generation
	// (configRead), what the scrape caches and what the failure log
	// remembers of it go nowhere. Neither does the result it publishes. The
	// result is also held to the target (targetStands), and so is what the
	// failure log remembers (logStands).
	sc.rec.read = s.readTargetAt(generation, target.Name)
	sc.count(func(st *serverStats) { st.probes++ })
	// With otlp.traces on, the scrape records its trace, exported at its
	// end when it was slow, or failed under otlp.traces.errors
	// (otlptraces.go).
	ctx, sc.spans = beginTrace(ctx, sc.cfg.OTLP, traceParent{})
	// With otlp.traces.propagate too, its requests carry its trace; a
	// scrape has no caller whose trace it could pass on.
	ctx = withPropagation(ctx, sc.cfg.OTLP, sc.spans.traceLink(), traceParent{}, nil)

	// Repeats of the same failure are logged sparingly (failurelog.go).
	sc.log = collectLog{
		key:    staticTargetKey(c.Name, target.Name),
		failed: "static target scrape failed", continuing: "static target stage failed; continuing", recovery: "static target recovered",
		attrs: []any{"target", target.Name, "collector", c.Name, "address", sc.address},
	}
	sc.status = staticScrapeStatus{started: sc.start, collector: target.Collector, target: target.Target}
	return ctx
}

// finish ends the scrape, up or down, and publishes its result.
func (sc *staticScrape) finish(up float64) {
	s, c, target := sc.s, sc.c, sc.target
	// The scrape is counted once, even when what follows panics.
	sc.failedOnPanic = nil
	elapsed := time.Since(sc.start)
	sc.count(func(st *serverStats) { st.lastDuration = elapsed.Seconds() })
	observeStaticScrape(sc.rec.collector, elapsed, sc.spans.traceLink())
	// The time was spent whether or not the result is published below:
	// it is load all the same.
	s.load.addStaticScrape(elapsed)
	// If a reload has removed, changed or brought back the target or
	// its collector since the scrape read them, the scrape is another
	// target's under the name. Its outcome is not that target's, and
	// its result would replace the one that target's own scrape
	// published.
	sc.status.duration = elapsed
	sc.status.ok = up == 1
	lastSuccess, stands := s.recordStaticTargetOutcome(sc.rec.read, target, sc.status, time.Now())
	if !stands {
		s.logger.Debug("static target scrape ended after a reload removed or changed its target or its collector; its result is not published", "target", target.Name, "collector", c.Name, "superseded", true)
		return
	}
	// A new slice, so the health metrics are never appended into one the
	// result shares with a set that was cached.
	health := staticTargetHealthMetrics(target, c, up, elapsed.Seconds(), lastSuccess).Metrics
	published := model.MetricSet{Metrics: make([]model.Metric, 0, len(sc.result.Metrics)+len(health))}
	published.Metrics = append(append(published.Metrics, sc.result.Metrics...), health...)
	// Over OTLP the result is as of when it came from the target, the
	// health metrics after it as of now (scrapeTime).
	at := scraped(sc.result, c, sc.fetched)
	if model.StaleIfError(c) <= 0 || len(sc.result.Metrics) == 0 {
		sc.fetched = time.Time{}
	}
	s.publishStaticResult(sc.rec.read, target, sc.identity, published, sc.fetched, at)
	if sc.spans != nil {
		s.endScrapeTrace(sc.cfg.OTLP, sc.spans, &target, c, sc.address, sc.start, len(sc.result.Metrics), sc.status.cached, sc.status.err)
	}
}

// failed ends a scrape that failed with err, and was logged. With
// cache.stale_if_error the last good result is exported in place of
// nothing, marked stale; http_exporter_target_up still says the scrape
// failed.
func (sc *staticScrape) failed(err error) {
	c := sc.c
	sc.status.err = err
	if model.StaleIfError(c) > 0 && sc.cacheKey != "" {
		now := time.Now()
		if cached, cachedAt, ok := sc.s.cache.GetStale(sc.cacheKey, now); ok {
			if answer, err := withFreshness(cached, c, true, cachedAt, now); err == nil {
				sc.fetched = cachedAt
				sc.status.stale = true
				sc.count(func(st *serverStats) {
					st.staleServed++
					st.emitted += uint64(len(cached.Metrics))
				})
				sc.result = withTargetLabels(answer, sc.target.Labels)
			}
		}
	}
	sc.finish(0)
}

// answeredFromCache ends the scrape with the collector's cached result, and
// says whether it did.
func (sc *staticScrape) answeredFromCache() bool {
	c := sc.c
	if model.CacheTTL(c) <= 0 {
		return false
	}
	mark := time.Now()
	cached, cachedAt, ok := sc.s.cache.Get(sc.cacheKey, mark)
	if !ok {
		sc.spans.step("cache", "miss", mark, time.Now(), "")
		return false
	}
	sc.spans.step("cache", "hit", mark, time.Now(), "")
	sc.count(func(st *serverStats) {
		st.cacheHits++
		st.success++
		st.emitted += uint64(len(cached.Metrics))
	})
	sc.fetched = cachedAt
	sc.status.cached = true
	answer, _ := withFreshness(cached, c, false, cachedAt, time.Now())
	sc.result = withTargetLabels(answer, sc.target.Labels)
	sc.finish(1)
	return true
}

// acquireSlot waits for the scrape's slot of the collector's
// max_concurrent_probes, which a static target scrape shares with its
// probes. It waits within its budget rather than failing. A scrape that finds
// no cache entry is a miss once it has its slot and goes to the target. One
// turned away, or cut short by the shutdown while it waited, fetched nothing
// in place of an entry, and is ended here: acquireSlot returns false then.
func (sc *staticScrape) acquireSlot(ctx context.Context) bool {
	s, c := sc.s, sc.c
	mark := time.Now()
	if err := s.trips.acquire(ctx, c.Name, maxConcurrentProbes(c)); err != nil {
		sc.spans.step("slot", "failed", mark, time.Now(), err.Error())
		if shuttingDown(ctx) {
			s.abortedByShutdown(sc.target, c)
			return false
		}
		sc.count(func(st *serverStats) { countRejection(st, err) })
		s.logCollectFailure(ctx, sc.rec.read, sc.log, "concurrency", err)
		sc.failed(err)
		return false
	}
	sc.spans.step("slot", "ok", mark, time.Now(), "")
	return true
}

// ended ends the scrape with what its trip collected.
func (sc *staticScrape) ended(trip collected) {
	if trip.aborted {
		sc.failedOnPanic = nil
		sc.s.abortedByShutdown(sc.target, sc.c)
		return
	}
	if trip.failed() {
		if trip.unauthorized || trip.refused {
			// As for a probe: a refused credential, or a target the
			// collector's target policy refused, gets no stale result.
			sc.cacheKey = ""
		}
		sc.status.skipped = trip.skipped
		sc.failed(trip.err)
		return
	}
	// Under error_handling log or ignore a failed stage leaves the target up
	// with nothing of the collector's to export, as it answers a probe 200
	// with an empty exposition.
	sc.count(func(st *serverStats) { st.success++ })
	if !trip.carriedOn {
		sc.result = withTargetLabels(trip.answer, sc.target.Labels)
		sc.fetched = trip.fetched
	}
	sc.finish(1)
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
// lastSuccess is when the target was last scraped successfully, or zero if
// never. It tells how long a target has given nothing new, whatever the
// endpoint then serves of it. A failed scrape leaves only these health series,
// or, with cache.stale_if_error, the last good result marked stale. A turn
// that publishes nothing (one skipped, or cut short by a shutdown) leaves the
// last result as it was, values and all.
func staticTargetHealthMetrics(target model.StaticTarget, c *model.Collector, up, duration float64, lastSuccess time.Time) model.MetricSet {
	labels := map[string]string{"collector": c.Name, "static_target": target.Name, "target": fetch.DisplayTarget(c, target.Target)}
	// A label written "" is the label left out (withTargetLabels).
	for name, value := range target.Labels {
		if _, exists := labels[name]; !exists && value != "" {
			labels[name] = value
		}
	}
	// A target that names none has no target label. A target of a localfile
	// collector that reads the collector's own path may do this. It was
	// target="", which to Prometheus is no label. The label is taken off
	// after the target's labels are added, so the name stays the health
	// series' own and no label of the target's takes its place.
	if labels["target"] == "" {
		delete(labels, "target")
	}
	return model.MetricSet{Metrics: []model.Metric{
		{Name: "http_exporter_target_up", Help: "Whether the last scrape of this static target succeeded.", Type: model.GaugeMetricType, Value: up, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_scrape_duration_seconds", Help: "Duration of the last scrape of this static target in seconds.", Type: model.GaugeMetricType, Value: duration, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_last_success_timestamp_seconds", Help: "Unix time of the last successful scrape of this static target; 0 if none has succeeded.", Type: model.GaugeMetricType, Value: unixSeconds(lastSuccess), Labels: model.CloneLabels(labels)},
	}}
}

// withTargetLabels adds a target's configured labels to every metric it
// produced. A label the collector already extracted is never overwritten, so
// declared metric labels keep precedence over target-wide ones. Nor is le or
// quantile overwritten: the label a histogram's buckets or a summary's
// quantiles carry on every sample that can hold it. A label written "" is
// left out, as a transform.labels value written "" is: it adds nothing. The
// cached metric set stays unlabelled, which lets a static target scrape and
// an equivalent probe share cache entries.
func withTargetLabels(set model.MetricSet, labels map[string]string) model.MetricSet {
	if len(labels) == 0 {
		return set
	}
	out := model.CloneMetricSet(set)
	for i := range out.Metrics {
		if out.Metrics[i].Labels == nil {
			out.Metrics[i].Labels = map[string]string{}
		}
		own := model.SeriesOwnLabel(out.Metrics[i])
		for name, value := range labels {
			if _, exists := out.Metrics[i].Labels[name]; !exists && name != own && value != "" {
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
	if t.Request.Message != "" {
		values.Set("message", t.Request.Message)
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
	if t.Request.NewConnection != nil {
		values.Set("new_connection", strconv.FormatBool(*t.Request.NewConnection))
	}
	if t.Request.IPFamily != nil {
		values.Set("ip_family", *t.Request.IPFamily)
	}
	if retry := t.Request.Retry; retry != nil {
		if retry.Attempts != nil {
			values.Set("retry_attempts", strconv.Itoa(*retry.Attempts))
		}
		if retry.Backoff != nil {
			values.Set("retry_backoff", time.Duration(*retry.Backoff).String())
		}
		if retry.NonIdempotent != nil {
			// No probe parameter sets it, but it changes the request,
			// so it is part of the key.
			values.Set("retry_non_idempotent", strconv.FormatBool(*retry.NonIdempotent))
		}
		if retry.RetryAfter != nil {
			// Neither does one set it, and it changes how long the
			// trip may wait.
			values.Set("retry_retry_after", strconv.FormatBool(*retry.RetryAfter))
		}
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
// its cache key: a graphite collector's targets; a grpc collector's metadata
// and retry codes; the retried statuses; an sshfile, sql, redis or ldap
// collector's account and credential files; a redis, ldap, tcp, sql, grpc,
// http or graphite collector's tls block; an http or graphite collector's
// login; an snmp collector's community or v3 passwords and its
// request_timeout and retries. Each section names itself and how many values
// follow. So a value that looks like the next section's name cannot move the
// boundary between two sections, and two different targets never share a key.
func targetOwnRequest(t *model.StaticTarget) []string {
	var own []string
	if len(t.Request.Targets) > 0 {
		own = append(append(own, "targets", strconv.Itoa(len(t.Request.Targets))), t.Request.Targets...)
	}
	if len(t.Request.Metadata) > 0 {
		own = append(own, "metadata", strconv.Itoa(len(t.Request.Metadata)))
		for _, key := range model.SortedKeys(t.Request.Metadata) {
			own = append(own, key, t.Request.Metadata[key])
		}
	}
	if retry := t.Request.Retry; retry != nil && retry.Codes != nil {
		own = append(append(own, "retry_codes", strconv.Itoa(len(retry.Codes))), retry.Codes...)
	}
	if retry := t.Request.Retry; retry != nil && retry.Statuses != nil {
		own = append(append(own, "retry_statuses", strconv.Itoa(len(retry.Statuses))), retry.Statuses...)
	}
	if t.Request.AcceptStatus != nil {
		own = append(append(own, "accept_status", strconv.Itoa(len(t.Request.AcceptStatus))), t.Request.AcceptStatus...)
	}
	if t.Request.AcceptCodes != nil {
		own = append(append(own, "accept_codes", strconv.Itoa(len(t.Request.AcceptCodes))), t.Request.AcceptCodes...)
	}
	if r := &t.Request; r.Username != "" || r.BindDN != "" || r.PrivateKeyFile != "" || r.PassphraseFile != "" || r.PasswordFile != "" {
		own = append(own, "account", "5", r.Username, r.BindDN, r.PrivateKeyFile, r.PassphraseFile, r.PasswordFile)
	}
	// An snmp target's own community or v3 passwords are the account it
	// reads as. Its request_timeout and retries only set how long it waits,
	// but a target that gives up sooner may answer what one that waits does
	// not.
	if r := &t.Request; r.CommunityFile != "" || r.AuthPasswordFile != "" || r.PrivPasswordFile != "" {
		own = append(own, "snmp_account", "3", r.CommunityFile, r.AuthPasswordFile, r.PrivPasswordFile)
	}
	if r := &t.Request; r.RequestTimeout != nil || r.Retries != nil {
		timeout, retries := "", ""
		if r.RequestTimeout != nil {
			timeout = time.Duration(*r.RequestTimeout).String()
		}
		if r.Retries != nil {
			retries = strconv.Itoa(*r.Retries)
		}
		own = append(own, "snmp_waits", "2", timeout, retries)
	}
	// A target's own tls block counts, every key of it, tls: {} as much as
	// one with files. The certificate a scrape presents is part of the
	// account it reads as, and the roots it trusts decide which server's
	// answer it takes.
	if r := &t.Request; r.TLS != nil {
		own = append(own, "tls", "1", fmt.Sprintf("%#v", *r.TLS))
	}
	// A target that logs in as it does itself is answered for that login.
	if login := fetch.LoginFingerprint(t.Request.Login); login != "" {
		own = append(own, "login", "1", login)
	}
	return own
}

// targetResource resolves the OTLP resource identity for this target, with the
// exporter-wide service name and attributes as the defaults. An attribute
// written "" is left out, the target's as well as the exporter-wide one
// (defaultResourceIdentity). It sets nothing, so an exporter-wide attribute of
// that name stays as it is, while a non-empty value replaces it. The labels of
// otlp.labels_as_resource are attributes of it too; a label written "" is
// none, as it is on no series. otlp.instance_id makes the target's name its
// service.instance.id. These are part of what tells the resource from another
// (otlpResourceIdentity.key). So targets that differ in them are exported
// apart, and targets alike in every one are exported under one resource. The
// configuration's check refuses either beside an attribute of its name
// (config.checkTargetResourceKeys).
func targetResource(t *model.StaticTarget, cfg model.OTLPConfig) otlpResourceIdentity {
	identity := defaultResourceIdentity(cfg)
	if t.OTLP.ServiceName != "" {
		identity.ServiceName = t.OTLP.ServiceName
	}
	for key, value := range t.OTLP.ResourceAttributes {
		if value != "" {
			identity.Attributes[key] = value
		}
	}
	for _, name := range t.OTLP.LabelsAsResource {
		if value := t.Labels[name]; value != "" {
			identity.Attributes[name] = value
		}
	}
	if t.OTLP.InstanceID {
		identity.Attributes[otlpServiceInstanceID] = t.Name
	}
	return identity
}

// otlpServiceInstanceID is the resource attribute a static target's
// otlp.instance_id sets: OpenTelemetry's name for the one instance of a
// service a resource is, which the target's name is among the targets.
const otlpServiceInstanceID = "service.instance.id"

// withoutResourceLabels is set, a static target's labelled result, as it is
// exported over OTLP. It drops the labels of otlp.labels_as_resource, which
// its resource carries (targetResource), from each series where the label has
// the target's value. A series whose own label of that name has another value
// keeps it, because the resource does not say it. Such a label is one its
// collector gave it, which a target's label never replaces (withTargetLabels),
// or a health series' collector or target. set is not changed: the endpoint
// serves it as it is.
func withoutResourceLabels(set model.MetricSet, t model.StaticTarget) model.MetricSet {
	if len(t.OTLP.LabelsAsResource) == 0 {
		return set
	}
	out := model.MetricSet{Metrics: make([]model.Metric, len(set.Metrics))}
	for i, m := range set.Metrics {
		var labels map[string]string
		for _, name := range t.OTLP.LabelsAsResource {
			if value, has := m.Labels[name]; has && value == t.Labels[name] {
				if labels == nil {
					labels = model.CloneLabels(m.Labels)
				}
				delete(labels, name)
			}
		}
		if labels != nil {
			m.Labels = labels
		}
		out.Metrics[i] = m
	}
	return out
}
