package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A probe and a static target's scrape make the same trip: fetch the target,
// decode the response, transform it and validate the result. The collector's
// error_handling decides what a failed stage does. collect is that trip, for
// both. What differs is around it: how a probe waits for a slot, what it
// answers, and where a static target's result goes. The callers keep that
// (server.go, statictarget.go).

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
	// revalidateKey is the key of the cache entry whose response the trip
	// revalidates (revalidatesFor). It is cacheKey for a trip that fills the
	// cache. For a debug probe, which reads the entry but writes nothing, it is
	// the key the same probe would have.
	revalidateKey string
	// budget bounds the trip when the probe has a deadline (scrapetimeout.go);
	// a failure that ran out of it says so, and where it came from.
	budget       time.Duration
	budgetSource string
	// scrape is set for a static target's scrape, whose budget is its
	// interval, so an error it runs out of says so as a scrape's.
	scrape bool
	// staticName is the static target a scrape is of, "" for a probe.
	staticName string
	log        collectLog
	// python is the worker statistics the trip's scripts count in when the
	// trip counts in no statistics itself, rec's, as a debug probe does not
	// (pythonstats.go).
	python *transform.PythonStats
	// pasted is the response a debug page's run decodes instead of fetching
	// one (debugpage.go): no request is made, and the body is held to the
	// collector's response limit as a fetched one is.
	pasted *fetch.HTTPResponse
	// pastedSteps are the answers pasted for the steps of a chain, by step
	// name, which a chained collector's run reads after pasted.
	pastedSteps map[string]*fetch.HTTPResponse
	// requester, when set, is the collector whose request block fetches the
	// target, with its target policy, credentials and accept_status. collector
	// then decodes and transforms the response. A debug page's run of one
	// collector on what another fetched sets it to the configured collector
	// that fetches. For every other trip it is nil, and collector makes the
	// request.
	requester *model.Collector
	// backingOff, when set, is why failure_backoff keeps the trip from the
	// target (backoff.go). The trip then contacts nothing and fails with it in
	// the fetch stage, under on_fetch_error, as the fetch it stands for would
	// have.
	backingOff error
	// lastSuccess is the key the trip's last success is kept under
	// (lastsuccess.go), "" when its clock placeholders read none.
	lastSuccess string
}

// boundedTripFailure is a stage's error as a trip reports it: the error
// itself, or, when its text is longer than a failure's may be
// (model.MaxFailureBytes), a new error with the start of the text and its
// length. The text of a stage's error goes into the answer to the scraper,
// the log line, a debug probe's report and, as what the failure is recognised
// by, the failure log's memory, where it stays as long as the failure repeats.
// It is made of what the target sent, what the scraper asked for and what a
// script raised, and the limits on a response bound none of these once an
// error has quoted them. So every error a trip reports passes here first,
// once, where the stage's failure is known for what it is and before anything
// is made of its text. This covers:
//   - what a stage of collect failed with (stageFailed, the target policy's
//     refusal, a rule under error_mode fail);
//   - what a file of a directory failed with (collectDirectory);
//   - what a static target's scrape failed with before its trip
//     (logCollectFailure).
//
// What the error was to errors.Is and errors.As is read before: a cut error
// is no longer the one it was cut from. An error within the bound is returned
// as it is, and costs only the reading of its text.
func boundedTripFailure(err error) error { return model.BoundedFailure(err) }

// collectLog is how a caller's failures are logged: under which key of the
// failure log (failurelog.go), with which messages and attributes.
type collectLog struct {
	key                          subjectKey
	failed, continuing, recovery string
	attrs                        []any
}

// logCollectFailure logs a failure of stage at error level, with ctx, the
// trip's, for a caller that read its collector as read says (tripFailed). The
// error is bounded here (boundedTripFailure), because it is that of a stage
// before the trip, which collect has not seen.
func (s *Server) logCollectFailure(ctx context.Context, read configRead, l collectLog, stage string, err error, extra ...any) {
	s.logTripFailure(ctx, read, l, stage, boundedTripFailure(err), extra...)
}

// logTripFailure is logCollectFailure for a trip, which a debug probe's
// report takes instead of the log (probedebug.go).
func (s *Server) logTripFailure(ctx context.Context, read configRead, l collectLog, stage string, err error, extra ...any) {
	attrs := append(append(append([]any{}, l.attrs...), "stage", stage), extra...)
	s.tripFailed(ctx, read, slog.LevelError, l.key, l.failed, stage, err, attrs...)
}

// printedBefore is what a failed script printed before it failed with err
// (transform.PrintedBeforeFailure), as the attributes its failure is logged
// with, and none for a failure with nothing printed before it.
func printedBefore(err error) []any {
	if printed := transform.PrintedBeforeFailure(err); printed != "" {
		return []any{"printed", printed}
	}
	return nil
}

// collected is how a trip ended.
type collected struct {
	// set is the validated result, and answer is the same with its freshness
	// series, when the trip went through whole. fetched is when it did. It is
	// the time its cache entry has, so the result is exported over OTLP as of
	// the same moment now and when the cache answers with it later.
	set     *model.MetricSet
	answer  model.MetricSet
	fetched time.Time
	// carriedOn is set when a stage failed under error_handling log or
	// ignore. The trip has nothing to export, and counts as a success unless
	// the collector has report_failures, which makes it a failure (probeTrip).
	// stage names that stage and carriedErr holds its error, which the log has
	// had. err is nil, so nothing that reads a failure in err takes the trip
	// for one.
	carriedOn  bool
	carriedErr error
	// stage and err say why the trip failed, when it did; metric names the
	// metric rule with error_mode fail that failed it. The failure is
	// already logged.
	stage  string
	err    error
	metric string
	// step is the step of an http collector's chain that failed, "" for
	// none (chainreport.go): report_failures labels its failed stage with
	// it.
	step string
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
	// fetchFailed is set when the fetch failed, or answered with a status
	// the collector does not accept, whatever error_handling made of it.
	// failure_backoff counts it as the target's failure (backoff.go). A trip
	// the target policy refused is not one.
	fetchFailed bool
	// outOfBudget is set when the fetch failed because the trip ran out of
	// its budget, which budget was: failure_backoff counts such a failure
	// only when the budget was at least its min_budget (backoff.go).
	outOfBudget bool
	budget      time.Duration
	// skipped is set when failure_backoff kept the trip from the target
	// (collectJob.backingOff).
	skipped bool
}

// cutShort reports whether ctx was cancelled rather than run out of time: by
// a shutdown, or because every probe that waited for the trip went away.
// Either way nobody waits for its answer, and the target did not fail.
func cutShort(ctx context.Context) bool {
	return ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func (r collected) failed() bool { return r.err != nil }

// ranOutOfBudget reports whether the fetch that failed with err did so
// because the trip's deadline (ctx's) came. That is so when ctx says so, or
// when err is a deadline's error and the deadline has passed. The latter
// happens when a request type set the deadline on a socket, whose timeout can
// come before the context's timer fires. A timeout of the request's own, such
// as its timeout parameter, is not the trip's budget running out.
func ranOutOfBudget(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && errors.Is(err, context.DeadlineExceeded) && !time.Now().Before(deadline)
}

// stageTargetPolicy is the stage of a trip the collector's allowed_targets or
// denied_targets refused.
const stageTargetPolicy = "target_policy"

// collect makes one trip to the target. It counts every stage in the
// collector's self-metrics, logs failures and recovery, and caches a result
// that went through whole.
func (s *Server) collect(ctx context.Context, j collectJob) collected {
	t := &collectTrip{s: s, j: j, c: j.collector, rec: j.rec, trace: probeTraceFrom(ctx)}
	// Each stage is recorded once, for a debug probe's report and for the
	// trip's trace, which are kept only for a debug run and while
	// otlp.traces is on (otlptraces.go).
	t.stages = tripStagesFrom(ctx)
	// The trip's parameters fill the placeholders of the collector's label
	// values where the transform reads them, for a response and for each
	// file of a directory alike (transform.WithLabelParams). Nearly every
	// collector has none, and its trips carry nothing for it.
	if t.c.LabelParams != nil || t.c.ExpressionParams != nil {
		ctx = transform.WithLabelParams(ctx, j.overrides.Params)
	}
	t.ctx = ctx
	start := time.Now()
	t.mark = start
	defer func() {
		// The last-scrape timestamp and the duration histogram describe a trip
		// to the target, not an answer from the cache. A debug probe's trip
		// is in neither, nor is one backoff kept from the target.
		if t.trace == nil && j.backingOff == nil {
			t.rec.scraped(time.Now())
			s.observeTargetScrape(t.rec.collector, time.Since(start), traceLinkFrom(t.ctx))
		}
	}()

	t.requester = t.c
	if j.requester != nil {
		t.requester = j.requester
	}
	if j.backingOff != nil {
		// Nothing is sent, so nothing of the last response's is replaced
		// in the self-metrics; the failure is the fetch's as the fetch's
		// own would be, logged, carried on past or answered stale alike.
		t.rec.update(func(x *serverStats) { x.backoffSkipped++ })
		failure := t.stageFailed(fetch.FetchStage(t.requester), j.backingOff, t.c.ErrorHandling.OnFetchError)
		failure.skipped = true
		return failure
	}
	response, failure := t.fetch()
	if failure != nil {
		return *failure
	}
	// The response as the target sent it, or as the cache kept it, is kept
	// again with the result, before the decode converts the body or
	// rewrites its headers.
	var kept *fetch.CachedResponse
	if t.revalidates && t.trace == nil {
		kept = fetch.KeepResponse(response)
	}
	accepted := fetch.AcceptedStatus(t.requester, j.overrides, response.StatusCode)
	if response.Chain != nil {
		// Each step of a chain judged its own status: the trip's answer
		// is accepted unless a step's was not.
		accepted = response.Chain.Failed == ""
	}
	if accepted && response.Conditional && !response.NotModified {
		// A new response in place of the cached one; one whose status
		// fails the trip revalidated nothing.
		t.rec.update(func(x *serverStats) { x.revalidatedModified++ })
	}
	s.notePages(t.ctx, response, accepted, t.requester, t.rec, j.display, j.target)
	if !accepted {
		return t.statusFailed(response)
	}
	var set *model.MetricSet
	if response.Directory != nil {
		// A directory: each file is decoded and transformed on its own, and
		// one that fails is left out rather than failing the trip.
		// A read the shutdown cut short is no result of the target's
		// (statictargetschedule.go): its files are not reported failed.
		if response.Directory.CutShort && cutShort(t.ctx) {
			return collected{stage: fetch.FetchStage(t.c), err: context.Cause(t.ctx), aborted: true}
		}
		t.mark = time.Now()
		set = s.collectDirectory(t.ctx, response.Directory, t.c, t.rec, s.pythonStats(&t.j), j.display, j.target)
		t.stages.step("files", "ok", t.mark, fmt.Sprintf("%d files, %d series", len(response.Directory.Files), len(set.Metrics)))
		t.trace.record(func(pt *probeTrace) { pt.transform = set })
	} else if set, failure = t.decodeAndTransform(response); failure != nil {
		return *failure
	}
	return t.validated(set, response, kept)
}

// collectTrip is the state of one trip of collect, shared by its stages.
type collectTrip struct {
	s *Server
	j collectJob
	// ctx is the trip's context, with what the transform reads added to it.
	ctx context.Context
	// c is the collector that decodes and transforms; requester the one
	// whose request is made, which is c but for a debug page's run on what
	// another collector fetched.
	c, requester *model.Collector
	rec          statsRecorder
	trace        *probeTrace
	stages       tripStages
	// mark is when the stage in progress started, for a debug probe's
	// report.
	mark time.Time
	// revalidates says the trip revalidates a cached response
	// (revalidatesFor).
	revalidates bool
	// chain is how the requests of an http collector's chain went, nil
	// for a trip that made none (chainreport.go).
	chain *fetch.ChainReport
	// clock is the reading of the clock the trip's clock placeholders are
	// filled from.
	clock fetch.ScrapeClock
}

// reported applies a stage's error policy to the stage's error, which is
// bounded already. extra attributes go to the log only, never into the
// probe's answer.
func (t *collectTrip) reported(stage string, err error, policy string, extra ...any) collected {
	if cutShort(t.ctx) {
		t.stages.step(stage, "cancelled", t.mark, err.Error())
		return collected{stage: stage, err: err, aborted: true}
	}
	j := &t.j
	trip := "probe"
	if j.scrape {
		trip = "scrape"
	}
	err = explainBudget(t.ctx, trip, j.budget, j.budgetSource, err)
	attrs := append(append(append([]any{}, j.log.attrs...), "stage", stage), extra...)
	// The failure is logged before its stage ends, so that its line is
	// the stage's in the trace (otlptraceslink.go).
	switch policy {
	case model.ErrorPolicyLog:
		t.s.tripFailed(t.ctx, t.rec.read, slog.LevelWarn, j.log.key, j.log.continuing, stage, err, attrs...)
		t.stages.step(stage, "failed", t.mark, err.Error()+"; carried on, error_handling "+policy)
		return collected{carriedOn: true, stage: stage, carriedErr: err}
	case model.ErrorPolicyIgnore:
		t.s.tripDebug(t.ctx, j.log.continuing, append(attrs, "error", err)...)
		t.stages.step(stage, "failed", t.mark, err.Error()+"; carried on, error_handling "+policy)
		return collected{carriedOn: true, stage: stage, carriedErr: err}
	}
	t.s.logTripFailure(t.ctx, t.rec.read, j.log, stage, err, extra...)
	t.stages.step(stage, "failed", t.mark, err.Error())
	return collected{stage: stage, err: err}
}

// stageFailed is reported for an error that it bounds first
// (boundedTripFailure).
func (t *collectTrip) stageFailed(stage string, err error, policy string, extra ...any) collected {
	return t.reported(stage, boundedTripFailure(err), policy, extra...)
}

// fetch makes the trip's request, or reads the response pasted on the debug
// page, and records what came back. It returns the response, or the trip's
// result when there is none.
func (t *collectTrip) fetch() (*fetch.HTTPResponse, *collected) {
	j, requester := &t.j, t.requester
	var response *fetch.HTTPResponse
	var err error
	t.revalidates = revalidatesFor(*j)
	// The trip's clock placeholders are filled from one reading of the
	// clock, and those that read ~last_success from the last success kept.
	// A trip that succeeds is kept as the last success, unless it is a debug
	// run, which keeps nothing.
	if j.lastSuccess != "" || requester.Clock.Any || t.c.TransformClock.Any {
		t.clock = fetch.NewScrapeClock(t.s.lastSuccesses.get(j.lastSuccess))
	}
	// The transform reads the same clock, as $scrape_time and
	// $last_success, or scrape_time and last_success in a script.
	if t.c.TransformClock.Any {
		t.ctx = transform.WithScrapeClock(t.ctx, t.clock.Now, t.clock.Since)
	}
	if j.pasted != nil {
		response, err = pastedResponse(t.c, j.pasted)
		if err == nil {
			// A chain reads the answers pasted for its steps, after the
			// first, and shows the URLs it would have asked.
			if chained, ok, chainErr := fetch.PastedChain(t.ctx, j.target, requester, j.overrides, response, j.pastedSteps); ok {
				response, err = chained, chainErr
			}
		}
	} else {
		fetchCtx := t.ctx
		if t.revalidates {
			fetchCtx = fetch.WithRevalidation(t.ctx, t.s.cache.Revalidation(j.revalidateKey, time.Now()))
		}
		if !t.clock.Now.IsZero() {
			fetchCtx = fetch.WithScrapeClock(fetchCtx, t.clock)
		}
		response, err = t.s.fetchCollector(fetchCtx, j.target, requester, j.overrides, j.headers)
	}
	if t.chain = chainReportOf(response, err); t.chain != nil {
		t.s.noteChain(t.ctx, t.chain, requester, t.rec, j.display, j.target, j.staticName)
		t.trace.record(func(pt *probeTrace) { pt.chain = t.chain })
	}
	// A debug probe's report shows what the target sent: a copy made here,
	// before the decode converts the body and rewrites its Content-Type.
	t.trace.record(func(pt *probeTrace) { pt.response = sentResponse(response) })
	if response != nil {
		t.stages.spans.fetched(len(response.Body))
	}
	grpcCode, grpc := fetch.GRPCStatusCode(requester, err)
	if err != nil {
		failure := t.fetchFailed(err, grpcCode, grpc)
		return nil, &failure
	}
	if response.GRPCCode != nil {
		grpcCode = *response.GRPCCode
	}
	t.stages.step(fetch.FetchStage(requester), "ok", t.mark, fetchedNote(response, j.pasted != nil))
	t.rec.update(func(x *serverStats) {
		// A 304 is described as the target sent it, with no body; the
		// response the trip goes on with is the cached one.
		x.lastStatus = response.StatusCode
		x.lastBytes = int64(len(response.Body))
		if response.NotModified {
			x.lastStatus, x.lastBytes = http.StatusNotModified, 0
		}
		if grpc {
			x.grpcCode, x.grpcCalled = grpcCode, true
		}
		if response.NotModified {
			x.revalidatedNotModified++
		}
	})
	return response, nil
}

// fetchFailed is the result of a trip whose request failed with err.
func (t *collectTrip) fetchFailed(err error, grpcCode int, grpc bool) collected {
	t.rec.update(func(x *serverStats) {
		// No response arrived.
		x.lastStatus = 0
		if errors.Is(err, model.ErrLimitExceeded) {
			x.limitExceeded(err)
		}
		if grpc {
			x.grpcCode, x.grpcCalled = grpcCode, true
		}
	})
	if errors.Is(err, fetch.ErrTargetRefused) {
		err = boundedTripFailure(err)
		t.rec.update(func(x *serverStats) { x.refused++ })
		t.s.logTripFailure(t.ctx, t.rec.read, t.j.log, stageTargetPolicy, err, chainFailureAttrs(t.chain)...)
		t.stages.step(stageTargetPolicy, "refused", t.mark, err.Error())
		return collected{stage: stageTargetPolicy, err: err, refused: true, step: chainFailedStep(t.chain)}
	}
	extra := chainFailureAttrs(t.chain)
	var status *fetch.CallStatusError
	if errors.As(err, &status) {
		extra = append(extra, "grpc_code", status.CodeName)
	}
	failure := t.stageFailed(fetch.FetchErrorStage(t.requester, err), err, t.c.ErrorHandling.OnFetchError, extra...)
	failure.unauthorized = status != nil && (status.CodeName == "UNAUTHENTICATED" || status.CodeName == "PERMISSION_DENIED")
	failure.fetchFailed = true
	failure.outOfBudget, failure.budget = ranOutOfBudget(t.ctx, err), t.j.budget
	failure.step = chainFailedStep(t.chain)
	return failure
}

// statusFailed is the result of a trip whose response has a status the
// collector does not accept.
func (t *collectTrip) statusFailed(response *fetch.HTTPResponse) collected {
	// The target's own explanation is usually in the body; the start of
	// it goes to the log, not to the answer, which Prometheus may keep.
	var excerpt []any
	if body := bodyExcerpt(response.Body); body != "" {
		excerpt = []any{"response_body", body}
	}
	t.mark = time.Now()
	statusErr := fmt.Errorf("received HTTP status %d", response.StatusCode)
	if response.StatusCode == http.StatusNotModified && !response.Conditional {
		// Only a request the exporter made conditional on a cached
		// response is answered by that response.
		statusErr = errors.New("received HTTP status 304 Not Modified to a request that was not conditional on a cached response, so there is no response to reuse")
	}
	if response.RedirectWithheld != "" {
		// The status is the answer of a host a redirect led to, which
		// was not sent what the collector identifies itself with
		// (fetch/redirecttrust.go): most often why it answers 401 or 403.
		statusErr = fmt.Errorf("received HTTP status %d %s", response.StatusCode, response.RedirectWithheld)
	}
	if response.NotRetried != "" {
		// Attempts were left, but the target's Retry-After asked for
		// more time than the probe had (request.retry.retry_after).
		statusErr = fmt.Errorf("%w, not retried: %s", statusErr, response.NotRetried)
	}
	if chain := response.Chain; chain != nil && chain.Failed != "" {
		// The step, or the fan-out item, that was answered with it.
		statusErr = fmt.Errorf("%s: %w", chain.Failed, statusErr)
	}
	if pages := response.Pages; pages != nil && len(pages.Pages) > 1 {
		// A page after the first: the pages before it were accepted.
		statusErr = fmt.Errorf("%w on page %d of request.pagination, %s", statusErr, len(pages.Pages), fetch.RedactURLString(pages.Pages[len(pages.Pages)-1].URL, fetch.MaskCredentialQueryValues))
	}
	failure := t.stageFailed("http_status", statusErr, t.c.ErrorHandling.OnFetchError, append(excerpt, chainFailureAttrs(response.Chain)...)...)
	failure.unauthorized = response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden
	failure.fetchFailed = true
	if response.Chain != nil {
		failure.step = response.Chain.FailedStep
	}
	return failure
}

// decodeAndTransform decodes the response and transforms what it holds into
// the trip's series, or returns the trip's result when a stage fails.
func (t *collectTrip) decodeAndTransform(response *fetch.HTTPResponse) (*model.MetricSet, *collected) {
	s, c, rec, j := t.s, t.c, t.rec, &t.j
	t.mark = time.Now()
	decoded, err := decode.Decode(response, c)
	t.trace.converted(c, decoded)
	if errors.Is(err, model.ErrLimitExceeded) {
		// The prometheus decoder stops at the first series past
		// limits.max_metrics, which is the validation's failure,
		// found sooner (transform/serieslimit.go).
		rec.update(func(x *serverStats) { x.limitErrors++ })
		failure := t.reported("validation", err, model.ErrorPolicyFail)
		return nil, &failure
	}
	if err != nil {
		// A decode's error is bounded where every decoder's leaves
		// (decode.Decode), and is not read for its length again.
		rec.update(func(x *serverStats) { x.parseErrors++ })
		failure := t.reported("decode", err, c.ErrorHandling.OnDecodeError)
		return nil, &failure
	}
	t.stages.step("decode", "ok", t.mark, decoded.Kind)
	t.trace.record(func(pt *probeTrace) { pt.decoded = decoded.Kind })
	rec.update(func(x *serverStats) { x.decodeOK++ })
	s.noteGraphite(t.ctx, decoded, c, rec, j.display, j.target, "")
	s.notePrometheus(t.ctx, decoded, c, rec, j.display, j.target, "")
	t.mark = time.Now()
	scriptCtx, timer := transform.WithScriptTimer(t.ctx, s.pythonStats(j))
	set, repaired, err := s.transformRecorded(scriptCtx, decoded, response, c, rec, j.log)
	recordScriptDuration(rec, timer)
	t.stages.scripts(timer, t.mark)
	if errors.Is(err, model.ErrLimitExceeded) {
		// A transform stops at the first series past limits.max_metrics,
		// which is the validation's failure found sooner
		// (transform/serieslimit.go). The same holds for a jq transform
		// stopped for its memory (expressionmemory.go).
		rec.update(func(x *serverStats) { x.limitExceeded(err) })
		failure := t.stageFailed("validation", err, model.ErrorPolicyFail)
		return nil, &failure
	}
	if err != nil {
		failure := t.transformFailed(err)
		return nil, &failure
	}
	t.stages.step("transform", "ok", t.mark, fmt.Sprintf("%d series", len(set.Metrics)))
	t.trace.record(func(pt *probeTrace) { pt.transform = set })
	s.noteUTF8Repairs(t.ctx, repaired, rec, c, j.display, j.target, "")
	return set, nil
}

// transformFailed is the result of a trip whose transform failed with err.
func (t *collectTrip) transformFailed(err error) collected {
	// A transform that ended because its trip was cancelled did not
	// fail: nobody waited for the answer, and its error is the
	// cancellation, wherever in a rule or a script it was met. It is not
	// counted as a failure of the transform, just as a trip cancelled in
	// another stage is not counted as a failure of that stage.
	if !cutShort(t.ctx) {
		t.rec.update(func(x *serverStats) {
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
		if cutShort(t.ctx) {
			return collected{stage: "metric", err: err, aborted: true}
		}
		t.s.logTripFailure(t.ctx, t.rec.read, t.j.log, "metric", err, "metric", failure.Metric)
		t.stages.step("transform", "failed", t.mark, "metric "+failure.Metric+": "+err.Error())
		return collected{stage: "metric", err: err, metric: failure.Metric}
	}
	return t.stageFailed("transform", err, t.c.ErrorHandling.OnTransformError, printedBefore(err)...)
}

// validated checks the trip's series, keeps them in the cache, and is the
// trip's result.
func (t *collectTrip) validated(set *model.MetricSet, response *fetch.HTTPResponse, kept *fetch.CachedResponse) collected {
	s, c, rec, j := t.s, t.c, t.rec, &t.j
	t.mark = time.Now()
	if err := set.ValidateNames(c.Limits, transform.UTF8Names(c)); err != nil {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		return t.stageFailed("validation", err, model.ErrorPolicyFail)
	}
	now := time.Now()
	answer, err := withFreshness(*set, c, false, now, now)
	if err == nil {
		err = checkProbeNames(c, *set)
	}
	if err != nil {
		return t.stageFailed("validation", err, model.ErrorPolicyFail)
	}
	t.stages.step("validation", "ok", t.mark, "")
	rec.update(func(x *serverStats) { x.emitted += uint64(len(set.Metrics)) })
	s.tripRecovered(t.ctx, rec.read, j.log.key, j.log.recovery, j.log.attrs...)
	// A directory read that the deadline cut short answers this probe with
	// what it read, but is not kept. The files it did not reach are fine as
	// far as anyone knows, and a cached copy would serve them as failed. A
	// debug probe's result is not kept either, nor that of a collector that a
	// reload removed or changed since the trip read it (PutFor).
	if t.trace == nil && (response.Directory == nil || !response.Directory.CutShort) {
		s.cache.PutFor(rec.read, j.cacheKey, c.Name, *set, kept, model.CacheTTL(c), model.StaleIfError(c), c.Limits.MaxCacheEntries, now)
	}
	if j.lastSuccess != "" && t.trace == nil && j.pasted == nil {
		s.lastSuccesses.record(j.lastSuccess, t.clock.Now)
	}
	return collected{set: set, answer: answer, fetched: now}
}

// revalidatesFor reports whether j's trip revalidates a cached response, and
// keeps the response it gets for the next trip to revalidate. That is so when:
//   - the collector caches and revalidates (cache.revalidate);
//   - its request is one a validator speaks for (fetch.Revalidable);
//   - the collector that makes the request is the one that decodes it, which
//     is not so for a debug page's run on what another collector fetched, nor
//     for a pasted response.
func revalidatesFor(j collectJob) bool {
	c := j.collector
	return j.revalidateKey != "" && j.pasted == nil && j.requester == nil &&
		model.UsesCache(c) && model.Revalidates(c) && fetch.Revalidable(c, j.overrides)
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
	probeTraceFrom(ctx).record(func(t *probeTrace) {
		t.failures = append(t.failures, failures...)
		for entry, operands := range report.DerivedAbsent() {
			if t.derivedAbsent == nil {
				t.derivedAbsent = map[string][]string{}
			}
			for _, operand := range operands {
				if !slices.Contains(t.derivedAbsent[entry], operand) {
					t.derivedAbsent[entry] = append(t.derivedAbsent[entry], operand)
				}
			}
		}
	})
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
// on every scrape of a target is one failing thing (failurelog.go), told apart
// by collector, target and rule. So it is logged once, and then only as a
// repeat or when it changes. Its recovery is logged when a complete transform
// produces its series again; a transform that failed as a whole may not have
// reached the rule. The rules of fail need nothing here: they fail the
// scrape, whose failure names the metric.
//
// Each rule is logged, remembered and recovered by itself, not with the
// collector's other rules of its metric name (appendRuleFailureKey). So a rule
// that starts to fail beside one that has been failing is a new failure, and
// one that works again recovers while the other is still remembered. Where
// several rules export the name, the lines say which rule it is
// (sharedRuleNames). Rules alike in name, expression and items are one rule,
// logged when either is under log (transform.RuleFailure).
func (s *Server) logRuleFailures(ctx context.Context, read configRead, c *model.Collector, failures []transform.RuleFailure, l collectLog, complete bool) {
	// failing holds the rules that failed on this scrape, by the keys their
	// failures are remembered under. unremembered counts those whose failure
	// is not remembered, which have no key: one that started while the log
	// was full, and one the log does not take (a debug probe's, or that of a
	// collector that no longer stands) when no earlier scrape's is
	// remembered.
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
	// Nearly every scrape is of a trip with no rule failure remembered. Such
	// a scrape has no rule to recover, and neither has one whose remembered
	// failures are all of rules that failed again. It asks the log once. No
	// scrape makes a key to ask, because that cost would grow with the rules'
	// expressions.
	if !complete || !s.failures.remembersRules(l.key, failing) {
		return
	}
	for _, rule := range transform.ReportedRules(c) {
		if rule.ErrorMode != model.ErrorModeLog {
			continue
		}
		// Nearly every rule recovers from nothing, and no line is made for
		// it then, nor is it asked which names the rules share.
		key, remembered := s.failures.rememberedRule(l.key, rule.Name, rule.Expression, rule.Items)
		if !remembered || failing[key] {
			continue
		}
		// If a failure is remembered now that was not when this scrape
		// reported it, another scrape of the trip reported it since, at the same
		// time as this one. The rule failed here too, and has not recovered.
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
// a rule on the trip or file whose key's bytes are key (subjectKey). The rule
// is told apart as the transform tells it apart (transform.RuleFailure): by
// its metric name, its expression and its items.
//
// The name and the expression are each written after their length, as the
// parts of every key are (appendKeyPart), so where one ends is not read from
// what it holds. A jq or css expression may have a NUL in a comment or a
// string. With only a NUL between them, an expression and items that hold one
// would read as those of another rule, whose failures and recoveries would
// then be this rule's. The items are the rest of the key.
//
// The bytes are made here and nowhere else. The failure log keeps those of
// the last rule it looked for (ruleLocked), and the key a failure is
// remembered under is a string of them (failedOf). So it is the key the
// failure is looked up by.
func appendRuleFailureKey(dst []byte, key, metric, expression, items string) []byte {
	dst = append(dst, key...)
	dst = append(dst, ruleKeyMarker...)
	dst = appendKeyPart(dst, metric)
	dst = appendKeyPart(dst, expression)
	return append(dst, items...)
}

// sharedRuleNames are the metric names that several rules of a collector
// export. Their failures are told apart in the log and in a debug probe's
// report by naming the rule's expression, and its items when it has any,
// beside the metric. They are worked out when first asked for, which never
// happens for a scrape whose rules neither fail nor recover.
//
// Whether a name is shared depends on what the collector's rules are, not on
// which of them failed on a scrape, so a rule's lines read the same on every
// scrape. Rules alike in name, expression and items are one rule, and share
// nothing.
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

// fetchedNote is a response as a debug probe's stages show it; pasted is a
// response pasted on the debug page, which was not fetched.
func fetchedNote(r *fetch.HTTPResponse, pasted bool) string {
	switch {
	case pasted:
		return fmt.Sprintf("pasted, status %d, %d bytes; nothing was sent", r.StatusCode, len(r.Body))
	case r.Directory != nil:
		return fmt.Sprintf("directory %s, %d files", r.Directory.Path, len(r.Directory.Files))
	case r.GRPCCode != nil:
		return fmt.Sprintf("gRPC status %d, %d bytes", *r.GRPCCode, len(r.Body))
	case r.NoStatus:
		return fmt.Sprintf("%d bytes", len(r.Body))
	}
	if r.NotModified {
		return fmt.Sprintf("304 Not Modified; the cached response was reused: status %d, %d bytes", r.StatusCode, len(r.Body))
	}
	if p := r.Pages; p != nil && (len(p.Pages) > 1 || p.MoreLeft) {
		note := fmt.Sprintf("status %d, %d bytes, %d pages", r.StatusCode, len(r.Body), len(p.Pages))
		if p.MoreLeft {
			note += ", stopped at request.pagination.max_pages with another page to come"
		}
		return note
	}
	return fmt.Sprintf("status %d, %d bytes", r.StatusCode, len(r.Body))
}

// bodyExcerptBytes is how much of an error response's body the log shows.
const bodyExcerptBytes = 256

// bodyRedactBytes is how much of the body is read for the excerpt, so a
// credential the excerpt's end runs through is masked whole.
const bodyRedactBytes = 4 * bodyExcerptBytes

// bodyExcerpt is the start of a response body for a log line: at most
// bodyExcerptBytes, with invalid UTF-8 and control characters replaced, runs
// of whitespace collapsed, and what reads as a credential masked
// (fetch.RedactText). So an HTML error page or a binary body reads as one
// tidy line, and an error page that echoes a token does not put it in the
// log. It ends in … when cut.
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

// chainFailedStep is the step of the chain that failed, "" for none.
func chainFailedStep(report *fetch.ChainReport) string {
	if report == nil {
		return ""
	}
	return report.FailedStep
}
