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

// boundedTripFailure is a stage's error as a trip reports it: the error
// itself, and when its text is longer than a failure's may be
// (model.MaxFailureBytes), a new one of the start of the text and its
// length. The text of a stage's error goes into the answer to the scraper,
// into the log line, into a debug probe's report and, as what the failure is
// recognised by, into the failure log's memory, where it stays for as long as
// the failure repeats; and it is made of what the target sent, what the
// scraper asked for and what a script raised, none of which the limits on a
// response bound once an error has quoted them. So every error a trip
// reports passes here first, once, where the stage's failure is known for
// what it is and before anything is made of its text: what a stage of
// collect failed with (stageFailed, the target policy's refusal, a rule
// under error_mode fail), what a file of a directory failed with
// (collectDirectory), and what a static target's scrape failed with before
// its trip (logCollectFailure). What the error was to errors.Is and
// errors.As is read before: a cut error is no longer the one it was cut
// from.
//
// An error within the bound is returned as it is, and costs the reading of
// its text.
func boundedTripFailure(err error) error { return model.BoundedFailure(err) }

// collectLog is how a caller's failures are logged: under which key of the
// failure log (failurelog.go), with which messages and attributes.
type collectLog struct {
	key                          subjectKey
	failed, continuing, recovery string
	attrs                        []any
}

// logCollectFailure logs a failure of stage at error level, for a caller
// that read its collector as read says (tripFailed). The error is bounded
// here (boundedTripFailure): it is that of a stage before the trip, which
// collect has not seen.
func (s *Server) logCollectFailure(read configRead, l collectLog, stage string, err error, extra ...any) {
	s.logTripFailure(context.Background(), read, l, stage, boundedTripFailure(err), extra...)
}

// logTripFailure is logCollectFailure for a trip, which a debug probe's
// report takes instead of the log (probedebug.go).
func (s *Server) logTripFailure(ctx context.Context, read configRead, l collectLog, stage string, err error, extra ...any) {
	attrs := append(append(append([]any{}, l.attrs...), "stage", stage), extra...)
	s.tripFailed(ctx, read, slog.LevelError, l.key, l.failed, stage, err, attrs...)
}

// collected is how a trip ended.
type collected struct {
	// set is the validated result and answer the same with its freshness
	// series, when the trip went through whole. fetched is when it did: the
	// time its cache entry has, so the result is exported over OTLP as of
	// the same moment now and when the cache answers with it later.
	set     *model.MetricSet
	answer  model.MetricSet
	fetched time.Time
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
	// unauthorized is set when the target refused the credential the trip
	// sent: HTTP 401 or 403, or gRPC UNAUTHENTICATED or PERMISSION_DENIED.
	// Such a failure is never answered with a stale result
	// (unauthorizedFailure).
	unauthorized bool
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
	// The trip's parameters fill the placeholders of the collector's label
	// values where the transform reads them, for a response and for each
	// file of a directory alike (transform.WithLabelParams). Nearly every
	// collector has none, and its trips carry nothing for it.
	if c.LabelParams != nil {
		ctx = transform.WithLabelParams(ctx, j.overrides.Params)
	}
	start := time.Now()
	defer func() {
		// The last-scrape timestamp and the duration histogram describe a trip
		// to the target, not an answer from the cache. A debug probe's trip
		// is in neither.
		if trace == nil {
			rec.scraped(time.Now())
			s.observeTargetScrape(rec.collector, time.Since(start))
		}
	}()
	// stageFailed applies a stage's error policy to the stage's error,
	// which it bounds first (boundedTripFailure); reported is the same for
	// an error that is bounded already.
	// extra attributes go to the log only, never into the probe's answer.
	// mark is when the stage in progress started, for a debug probe's
	// report.
	mark := start
	reported := func(stage string, err error, policy string, extra ...any) collected {
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
			s.tripFailed(ctx, rec.read, slog.LevelWarn, j.log.key, j.log.continuing, stage, err, attrs...)
			return collected{carriedOn: true}
		case model.ErrorPolicyIgnore:
			s.tripDebug(ctx, j.log.continuing, append(attrs, "error", err)...)
			return collected{carriedOn: true}
		}
		s.logTripFailure(ctx, rec.read, j.log, stage, err, extra...)
		return collected{stage: stage, err: err}
	}
	stageFailed := func(stage string, err error, policy string, extra ...any) collected {
		return reported(stage, boundedTripFailure(err), policy, extra...)
	}

	response, err := fetch.FetchCollector(ctx, j.target, c, j.overrides, j.headers)
	// A debug probe's report shows what the target sent: a copy made here,
	// before the decode converts the body and rewrites its Content-Type.
	trace.record(func(t *probeTrace) { t.response = sentResponse(response) })
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
			err = boundedTripFailure(err)
			rec.update(func(x *serverStats) { x.refused++ })
			trace.step(stageTargetPolicy, "refused", time.Since(mark), err.Error())
			s.logTripFailure(ctx, rec.read, j.log, stageTargetPolicy, err)
			return collected{stage: stageTargetPolicy, err: err, refused: true}
		}
		var extra []any
		var status *fetch.CallStatusError
		if errors.As(err, &status) {
			extra = []any{"grpc_code", status.CodeName}
		}
		failure := stageFailed(fetch.FetchErrorStage(c, err), err, c.ErrorHandling.OnFetchError, extra...)
		failure.unauthorized = status != nil && (status.CodeName == "UNAUTHENTICATED" || status.CodeName == "PERMISSION_DENIED")
		return failure
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
		statusErr := fmt.Errorf("received HTTP status %d", response.StatusCode)
		if response.RedirectWithheld != "" {
			// The status is the answer of a host a redirect led to, which
			// was not sent what the collector identifies itself with
			// (fetch/redirecttrust.go): most often why it answers 401 or 403.
			statusErr = fmt.Errorf("received HTTP status %d %s", response.StatusCode, response.RedirectWithheld)
		}
		failure := stageFailed("http_status", statusErr, c.ErrorHandling.OnFetchError, excerpt...)
		failure.unauthorized = response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden
		return failure
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
		set = s.collectDirectory(ctx, response.Directory, c, rec, j.display, j.target)
		trace.step("files", "ok", time.Since(mark), fmt.Sprintf("%d files, %d series", len(response.Directory.Files), len(set.Metrics)))
		trace.record(func(t *probeTrace) { t.transform = set })
	} else {
		mark = time.Now()
		decoded, err := decode.Decode(response, c)
		trace.converted(c, decoded)
		if errors.Is(err, model.ErrLimitExceeded) {
			// The prometheus decoder stops at the first series past
			// limits.max_metrics, which is the validation's failure,
			// found sooner (transform/serieslimit.go).
			rec.update(func(x *serverStats) { x.limitErrors++ })
			return reported("validation", err, model.ErrorPolicyFail)
		}
		if err != nil {
			// A decode's error is bounded where every decoder's leaves
			// (decode.Decode), and is not read for its length again.
			rec.update(func(x *serverStats) { x.parseErrors++ })
			return reported("decode", err, c.ErrorHandling.OnDecodeError)
		}
		trace.step("decode", "ok", time.Since(mark), decoded.Kind)
		trace.record(func(t *probeTrace) { t.decoded = decoded.Kind })
		rec.update(func(x *serverStats) { x.decodeOK++ })
		s.noteGraphite(ctx, decoded, c, rec, j.display, j.target, "")
		s.notePrometheus(ctx, decoded, c, rec, j.display, j.target, "")
		mark = time.Now()
		scriptCtx, timer := transform.WithScriptTimer(ctx)
		var repaired utf8Repairs
		set, repaired, err = s.transformRecorded(scriptCtx, decoded, response, c, rec, j.log)
		recordScriptDuration(rec, timer)
		if errors.Is(err, model.ErrLimitExceeded) {
			// A transform stops at the first series past
			// limits.max_metrics, which is the validation's failure,
			// found sooner (transform/serieslimit.go).
			rec.update(func(x *serverStats) { x.limitErrors++ })
			return stageFailed("validation", err, model.ErrorPolicyFail)
		}
		if err != nil {
			// A transform that ended because its trip was cancelled did not
			// fail: nobody waited for the answer, and its error is the
			// cancellation, wherever in a rule or a script it was met. It is
			// counted as no failure of the transform, as a trip cancelled in
			// another stage is counted as none of that stage's.
			if !cutShort(ctx) {
				rec.update(func(x *serverStats) {
					if errors.Is(err, model.ErrMissingValue) {
						x.missing++
					}
					if errors.Is(err, model.ErrScriptFailed) {
						x.scriptErrors++
					}
					x.transformErrors++
				})
			}
			// A metric rule with error_mode fail asked for the scrape to fail
			// when it cannot produce its value. That is a statement about this
			// one metric, more specific than the collector's
			// on_transform_error, so it is honoured even where the collector
			// would have carried on after a failed transform.
			var failure *transform.MetricFailure
			if errors.As(err, &failure) {
				err = boundedTripFailure(err)
				if cutShort(ctx) {
					return collected{stage: "metric", err: err, aborted: true}
				}
				trace.step("transform", "failed", time.Since(mark), "metric "+failure.Metric+": "+err.Error())
				s.logTripFailure(ctx, rec.read, j.log, "metric", err, "metric", failure.Metric)
				return collected{stage: "metric", err: err, metric: failure.Metric}
			}
			return stageFailed("transform", err, c.ErrorHandling.OnTransformError)
		}
		trace.step("transform", "ok", time.Since(mark), fmt.Sprintf("%d series", len(set.Metrics)))
		trace.record(func(t *probeTrace) { t.transform = set })
		s.noteUTF8Repairs(ctx, repaired, rec, c, j.display, j.target, "")
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
	s.tripRecovered(ctx, rec.read, j.log.key, j.log.recovery, j.log.attrs...)
	// A directory read the deadline cut short answers this probe with what
	// it read, but is not kept: the files it did not reach are fine as far
	// as anyone knows, and a cached copy would serve them failed. A debug
	// probe's result is not kept either, nor that of a collector a reload
	// removed or changed since the trip read it (PutFor).
	if trace == nil && (response.Directory == nil || !response.Directory.CutShort) {
		s.cache.PutFor(rec.read, j.cacheKey, c.Name, *set, model.CacheTTL(c), model.StaleIfError(c), c.Limits.MaxCacheEntries, now)
	}
	return collected{set: set, answer: answer, fetched: now}
}

// transformRecorded transforms a decoded response, and counts in the
// collector's self-metrics the series its rules carried on without: each in
// http_exporter_rule_failures_total, and those whose value the response did
// not contain in http_exporter_missing_keys_total too.
//
// It also returns what the transform repaired for invalid UTF-8, for
// noteUTF8Repairs.
//
// The failures of rules under error_mode log are logged here rather than by
// the transform, through the failure log under l's key and with its
// attributes (logRuleFailures).
func (s *Server) transformRecorded(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, rec statsRecorder, l collectLog) (*model.MetricSet, utf8Repairs, error) {
	ctx, report := transform.WithRuleReport(ctx)
	set, err := transform.Transform(transform.LeaveRuleLoggingToCaller(ctx), d, r, c, s.pythonPath)
	var repaired utf8Repairs
	repaired.count, repaired.first = report.UTF8Repairs()
	failures := report.Failures()
	probeTraceFrom(ctx).record(func(t *probeTrace) { t.failures = append(t.failures, failures...) })
	s.logRuleFailures(ctx, rec.read, c, failures, l, err == nil)
	if len(failures) == 0 {
		return set, repaired, err
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
	return set, repaired, err
}

// logRuleFailures logs the rules under error_mode log that carried on without
// some series, at warning level since the scrape was answered. A rule failing
// on every scrape of a target is one failing thing (failurelog.go), told
// apart by collector, target and rule, so it is logged once and then only
// as a repeat or when it changes, and its recovery is logged when a complete
// transform produces its series again; one that failed as a whole may not
// have reached the rule. The rules of fail need nothing here: they fail the
// scrape, whose failure names the metric.
//
// Each rule is logged, remembered and recovered by itself, and not with the
// collector's other rules of its metric name (appendRuleFailureKey): one that
// starts to fail beside one that has been failing is a new failure, and one
// that works again recovers while the other is still remembered. Where
// several rules export the name, the lines say which rule it is
// (sharedRuleNames). Rules alike in name, expression and items are one rule,
// logged when either is under log (transform.RuleFailure).
func (s *Server) logRuleFailures(ctx context.Context, read configRead, c *model.Collector, failures []transform.RuleFailure, l collectLog, complete bool) {
	// failing are the rules that failed on this scrape, by the keys their
	// failures are remembered under, and unremembered counts those whose
	// failure is not remembered, which have no key: one that started while
	// the log was full, and one the log does not take, a debug probe's or
	// that of a collector that no longer stands, when no earlier scrape's
	// is remembered.
	failing, unremembered := map[string]bool{}, 0
	var shared sharedRuleNames
	for _, f := range failures {
		if !f.Logged {
			continue
		}
		attrs := append(append([]any{}, l.attrs...), "metric", f.Metric)
		attrs = append(shared.telling(attrs, c, f.Metric, f.Expression, f.Items), "error_mode", model.ErrorModeLog, "failures", f.Failures)
		// A rule's failure is reported by what tells the rule apart, and
		// its key is made by the log, when the log first remembers it.
		if key := s.ruleFailed(ctx, read, slog.LevelWarn, l.key, f.Metric, f.Expression, f.Items, "metric extraction failed", "metric", f.First, attrs...); key != "" {
			failing[key] = true
		} else {
			unremembered++
		}
	}
	// A scrape of a trip none of whose rules has a failure remembered, as
	// nearly every one is, has no rule to recover, and neither has one whose
	// remembered failures are all of rules that failed again: it asks the
	// log once. No scrape makes a key to ask, whose cost would grow with the
	// rules' expressions.
	if !complete || !s.failures.remembersRules(l.key, failing) {
		return
	}
	for _, rule := range c.Metrics {
		if rule.ErrorMode != model.ErrorModeLog {
			continue
		}
		// Nearly every rule recovers from nothing, and no line is made for
		// it then, nor is it asked which names the rules share.
		key, remembered := s.failures.rememberedRule(l.key, rule.Name, rule.Expression, rule.Items)
		if !remembered || failing[key] {
			continue
		}
		// A failure remembered now that was not when this scrape reported
		// it is one another scrape of the trip reported since, at the same
		// time as this one: the rule failed here too, and has not
		// recovered.
		if unremembered > 0 && failedAmong(failures, rule.Name, rule.Expression, rule.Items) {
			continue
		}
		attrs := append(append([]any{}, l.attrs...), "metric", rule.Name)
		s.tripRecovered(ctx, read, l.key.rule(key), "metric extraction recovered", shared.telling(attrs, c, rule.Name, rule.Expression, rule.Items)...)
	}
}

// failedAmong reports whether the rule that metric, expression and items
// tell apart is one of those failures reports as logged: told apart as the
// failure log's keys tell rules apart (appendRuleFailureKey), without a key.
func failedAmong(failures []transform.RuleFailure, metric, expression, items string) bool {
	for i := range failures {
		if f := &failures[i]; f.Logged && f.Metric == metric && f.Expression == expression && f.Items == items {
			return true
		}
	}
	return false
}

// appendRuleFailureKey appends to dst the bytes of the failure log's key for
// a rule on the trip or file whose key's bytes are key (subjectKey). The
// rule is told apart as the transform tells it apart
// (transform.RuleFailure): by its metric name, its expression and its
// items.
//
// The name and the expression are each written after their length, as the
// parts of every key are (appendKeyPart), so where one ends is not read from
// what it holds: a jq or css expression may have a NUL in a comment or a
// string, and with only a NUL between them an expression and items that
// hold one would read as those of another rule, whose failures and
// recoveries would then be this rule's. The items are the rest of the key.
//
// The bytes are made here and nowhere else, where the failure log keeps
// those of the last rule it looked for (ruleLocked), and the key a failure
// is remembered under is a string of them (failedOf): so it is the key the
// failure is looked up by.
func appendRuleFailureKey(dst []byte, key, metric, expression, items string) []byte {
	dst = append(dst, key...)
	dst = append(dst, ruleKeyMarker...)
	dst = appendKeyPart(dst, metric)
	dst = appendKeyPart(dst, expression)
	return append(dst, items...)
}

// sharedRuleNames are the metric names several rules of a collector export,
// whose failures are told apart in the log and in a debug probe's report by
// naming the rule's expression, and its items when it has any, beside the
// metric. They are worked out when first asked for, which a scrape whose
// rules neither fail nor recover never does.
//
// A name is shared by what the collector's rules are, and not by which of
// them failed on a scrape, so a rule's lines read the same on every scrape.
// Rules alike in name, expression and items are one rule, and share nothing.
type sharedRuleNames struct {
	names map[string]bool
}

// has reports whether several rules of c export name.
func (n *sharedRuleNames) has(c *model.Collector, name string) bool {
	if n.names == nil {
		n.names = map[string]bool{}
		first := make(map[string]*model.MetricRule, len(c.Metrics))
		for i := range c.Metrics {
			rule := &c.Metrics[i]
			switch known, seen := first[rule.Name]; {
			case !seen:
				first[rule.Name] = rule
			case known.Expression != rule.Expression || known.Items != rule.Items:
				n.names[rule.Name] = true
			}
		}
	}
	return n.names[name]
}

// telling returns a log line's attributes with what tells the rule from the
// others of its name added, when c has others: attrs as they are for a name
// only one rule exports.
func (n *sharedRuleNames) telling(attrs []any, c *model.Collector, name, expression, items string) []any {
	if !n.has(c, name) {
		return attrs
	}
	attrs = append(attrs, "expression", expression)
	if items != "" {
		attrs = append(attrs, "items", items)
	}
	return attrs
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

// bodyRedactBytes is how much of the body is read for the excerpt, so a
// credential the excerpt's end runs through is masked whole.
const bodyRedactBytes = 4 * bodyExcerptBytes

// bodyExcerpt is the start of a response body for a log line: at most
// bodyExcerptBytes, with invalid UTF-8 and
// control characters replaced, runs of whitespace collapsed, and what reads
// as a credential masked (fetch.RedactText), so an HTML error page or a
// binary body reads as one tidy line, and an error page that echoes a token
// does not put it in the log. It ends in … when cut.
func bodyExcerpt(body []byte) string {
	// Credentials are masked in more than is shown, so one the cut runs
	// through is found whole.
	if len(body) > bodyRedactBytes {
		body = body[:bodyRedactBytes]
	}
	text := strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(string(body), " "))
	text = fetch.RedactText(strings.Join(strings.Fields(text), " "))
	if len(text) > bodyExcerptBytes {
		// A character cut in two at the limit is dropped.
		text = strings.ToValidUTF8(text[:bodyExcerptBytes], "") + "…"
	}
	return text
}
