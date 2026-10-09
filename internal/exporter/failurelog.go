package exporter

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A target that is down fails every probe, and every probe logged its failure:
// one line per scrape interval per Prometheus replica per target, the same
// line again and again, burying everything else in the log. The self-metrics
// already count every failure exactly; the log is for learning that
// something broke, what, and when it recovered.
//
// So a failure is logged in full the first time. While the same thing keeps
// failing the same way — the same collector, target (and file, for a
// directory), stage and error — the repeats are logged at debug level only,
// and at the failure's own level once every failureRepeatInterval, with how
// many times it happened since the last line and since when it has been
// failing. A different stage or error is a new failure, logged in full. The
// first success after a failure is logged at info level, with how long it
// failed and how many times. With --log.level=debug every repeat is still
// visible.
//
// The same error is the same failure, not the same text: where in the
// response it happened and what it measured are left out of what is compared
// (model.SameFailureText), so an empty cell in row 3 and then in row 7, or a
// label 612 bytes long and then 640, is one failure that keeps happening.
// What is logged is the text in full, as it is each time.

const (
	// failureRepeatInterval is how often a failure that keeps happening is
	// logged again.
	failureRepeatInterval = 5 * time.Minute
	// failureLogMaxEntries bounds the failures remembered; past it, a new one
	// is logged in full every time rather than remembered.
	failureLogMaxEntries = 10000
	// failureLogForget is how long a failure that has stopped being reported,
	// such as that of a target no longer probed, is remembered.
	failureLogForget = time.Hour
)

type failureState struct {
	// key is the key the failure is remembered under, for a scrape that
	// finds a rule's failure without making the rule's key (rememberedRule,
	// ruleFailedFor), with whose the failure is, as the trip that reported
	// it said: what a reload goes by when it forgets the failures of a
	// collector or of a static target (forgetCollectorsLocked,
	// forgetStaticTargetsLocked).
	key subjectKey
	// rule says that the failure is a rule's, and trip is then the key of the
	// trip or file the rule failed on, as the scrape that reported the
	// failure gave it (ruleFailedFor): what the failure is counted for
	// (ruleFailures), for as long as it is remembered.
	trip string
	rule bool
	// err is the text the failure is recognised by (model.SameFailureText).
	stage, err           string
	first, logged, seen  time.Time
	failures, suppressed int
}

type failureLog struct {
	mu      sync.Mutex
	entries map[string]*failureState
	// ruleFailures is, for each trip or file whose rules have failures among
	// the entries, how many: what a scrape asks before it looks for the
	// rules that recovered (remembersRules). It is kept in step where an
	// entry is made and where one is dropped (putLocked, dropLocked), and
	// nowhere else is either done.
	//
	// The trip or file is the key the scrape gave with the rule's failure,
	// which the entry keeps (failureState.trip), and is the key the scrape
	// asks with. It is not read back out of the rule's key: the parts of a
	// key are the operator's and the scraper's to choose, and the key of a
	// directory's file named rule ends as the marker of a rule's key begins
	// (ruleKeyMarker), so the first marker in the key of a rule of that
	// file is not the one its key was made with.
	ruleFailures map[string]int
	// ruleKey is where the key of a rule is written to be looked up
	// (ruleLocked), under the lock, in place of a key made for every
	// lookup, and what a key is made of when a rule's failure is first
	// remembered (failedOf). It is as long as the longest key asked for.
	ruleKey   []byte
	interval  time.Duration
	now       func() time.Time
	lastSweep time.Time
}

func newFailureLog() *failureLog {
	return &failureLog{entries: map[string]*failureState{}, ruleFailures: map[string]int{}, interval: failureRepeatInterval, now: time.Now}
}

// A failure is remembered under the key of its subject: the one thing that
// fails, keeps failing and recovers. The parts of a subject are the
// operator's and the scraper's to choose — a probe's target is any bytes its
// caller sends, a directory's files have any names, and so have the
// directory, a rule's expression and its items — so a key is made in a way
// that no choice of them makes two subjects' keys the same, and nothing is
// ever read back out of one. Whose a failure is, which a reload asks when it
// forgets, is told beside the key and kept with the entry.
//
// The bytes of a key are the digest of its subject (subjectBytes): of the
// kind of its subject, a letter, and its parts, each after its length in
// decimal and a NUL, so a part ends where its length says and not where a
// byte of it says (appendSubjectEncoding); and then what of the subject the
// failure is, when it is not the subject's own: one of the aspects below,
// or, for a rule, the marker of a rule's key and what tells the rule apart
// (appendRuleFailureKey). The kinds are
//
//   - a probe (probeFailureKey): its collector, its target and what else
//     makes it the probe it is;
//   - a static target (staticTargetKey): its collector and its name;
//   - what a trip found at the address it went to (failureKey, aspectKey):
//     its collector, the address, and the directory's file, or none;
//   - a static target's metric left out of the static targets endpoint
//     (staticClashKey): the target's name and the metric's;
//   - a metric name the OTLP export had as two kinds under one resource
//     (otlpNameClashKey): the resource's key and the name.
//
// So a probe whose target reads as a static target's name is a probe, a
// directory's file named schedule, stale or rule is a file, and a target
// with NULs in it is one part, wherever they stand.

// The kinds of a key's subject.
const (
	probeSubject    = 'p'
	staticSubject   = 's'
	addressSubject  = 'a'
	endpointSubject = 'e'
	exportSubject   = 'o'
)

// failureAspect is what of a subject a failure is, when it is not the
// subject's own: it ends a key. No aspect begins as the marker of a rule's
// key does, and none is another's beginning.
type failureAspect string

const (
	// staleAspect is a probe answered with the last good result.
	staleAspect failureAspect = "\x00stale"
	// scheduleAspect is the turns the schedule gave up for a static target.
	scheduleAspect failureAspect = "\x00schedule"
	// utf8Aspect is the bytes of a response or a file that were not UTF-8.
	utf8Aspect failureAspect = "\x00utf8"
	// listingAspect and skippedAspect are a directory with more entries
	// than a scrape lists, and with more matching files than it reads.
	listingAspect failureAspect = "\x00listing"
	skippedAspect failureAspect = "\x00skipped"
	// carbonLinesAspect and sampleLinesAspect are the lines the graphite
	// and the prometheus decoder left out.
	carbonLinesAspect failureAspect = "\x00carbon lines"
	sampleLinesAspect failureAspect = "\x00sample lines"
)

// ruleKeyMarker is what stands, in the key of a rule's failure, between the
// key of the trip or file the rule failed on and what tells the rule apart
// (appendRuleFailureKey). A key is made with it and never taken apart at it.
const ruleKeyMarker = "\x00rule\x00"

// subjectKey is the key of a subject and whose the subject is.
type subjectKey struct {
	// bytes are what the subject's failure is remembered under, and what
	// no other subject has.
	bytes string
	// collector is the collector the subject is of, whose failures a
	// reload that removes or changes the collector forgets; empty for what
	// the static targets endpoint remembers, which is of none.
	collector string
	// static says that the subject is a static target's own — its scrape,
	// the turns the schedule gave up for it, a rule of its scrape — and
	// target is then the target's name: what a reload that removes or
	// changes the static target forgets. What the target's trip found at
	// the address it went to is not, and is a probe's of that address too.
	target string
	static bool
}

// aspect is the key of what of k's subject the aspect names. k is a
// subject's own key.
func (k subjectKey) aspect(of failureAspect) subjectKey {
	k.bytes += string(of)
	return k
}

// rule is the key of a rule of k's trip or file whose bytes are bytes
// (appendRuleFailureKey): a rule's failure is whose its trip is.
func (k subjectKey) rule(bytes string) subjectKey {
	k.bytes = bytes
	return k
}

// probeFailureKey is the key of the probe of target by the collector that
// probe, its key in the cache, tells from the collector's other probes of
// the target: those that differ in their parameters or forwarded headers,
// tenants and paths, fail apart in the log. probe is empty when the
// collector's definition could not be fingerprinted, and its probes of one
// target are then one.
func probeFailureKey(collector, target, probe string) subjectKey {
	return subjectKey{bytes: subjectBytes(probeSubject, "", collector, target, probe), collector: collector}
}

// staticTargetKey is the key of the static target name's own failures,
// scraped with the collector.
func staticTargetKey(collector, name string) subjectKey {
	return subjectKey{bytes: subjectBytes(staticSubject, "", collector, name), collector: collector, target: name, static: true}
}

// failureKey is the key of what a trip of the collector found at target, the
// address it went to: file is the directory's file that failed, and empty
// for the response or the directory itself. A probe and a static target's
// scrape of one address share it.
func failureKey(collector, target, file string) subjectKey {
	return aspectKey(collector, target, file, "")
}

// aspectKey is the key of an aspect of what failureKey names.
func aspectKey(collector, target, file string, of failureAspect) subjectKey {
	return subjectKey{bytes: subjectBytes(addressSubject, of, collector, target, file), collector: collector}
}

// staticClashKey is the key of the static target's metric the static
// targets endpoint left out. It is of no collector and is not the target's
// own: the endpoint settles it (settleStaticClashes), and no reload does.
func staticClashKey(target, metric string) subjectKey {
	return subjectKey{bytes: subjectBytes(endpointSubject, "", target, metric)}
}

// otlpNameClashKey is the key of the metric name that an OTLP export had
// as two kinds under the resource whose key is resource (otlpMetricsOf). It
// is of no collector: the two that wrote the name may be any, and no reload
// forgets it.
func otlpNameClashKey(resource, metric string) subjectKey {
	return subjectKey{bytes: subjectBytes(exportSubject, "", resource, metric)}
}

// subjectBytes are the bytes of the key of a subject of kind with the
// parts, and of the aspect of it when there is one: the digest of the
// subject (appendSubjectEncoding) and then the aspect, made at once, in one
// allocation, which for a usual subject is the only one.
//
// A key is a digest rather than the parts themselves because a probe's
// target is as long as its caller makes it, up to 8 KiB, and a failure is
// remembered for an hour (failureLogForget): with the target in the key, ten
// thousand probes of failing targets that differ held some 80 MiB in keys
// alone. The digest is as long whatever the parts are, so what an entry
// holds of its subject is bounded, and since it is of one length a key
// ends where its digest does: an aspect or the marker of a rule's key after
// it is never read as part of the subject. The log never reads a key back;
// the log lines take the target from the trip that reports the failure, as
// before.
//
// The encoding is written where that of a usual subject fits, without an
// allocation of its own, and that of a longer one, such as a probe of a long
// target, is hashed as it is written (streamSubjectDigest): no copy of a
// long target is made to remember its failure.
func subjectBytes(kind byte, of failureAspect, parts ...string) string {
	var digest [sha256.Size]byte
	var room [512]byte
	if size := subjectEncodingSize(parts); size <= len(room) {
		digest = sha256.Sum256(appendSubjectEncoding(room[:0], kind, parts...))
	} else {
		digest = streamSubjectDigest(kind, parts)
	}
	var b strings.Builder
	b.Grow(len(digest) + len(of))
	b.Write(digest[:])
	b.WriteString(string(of))
	return b.String()
}

// subjectEncodingSize is how long the encoding of a subject with the parts
// is (appendSubjectEncoding).
func subjectEncodingSize(parts []string) int {
	size := 1
	for _, part := range parts {
		size += decimalDigits(len(part)) + 1 + len(part)
	}
	return size
}

// decimalDigits is how many digits n, which is not negative, is written with.
func decimalDigits(n int) int {
	digits := 1
	for ; n >= 10; n /= 10 {
		digits++
	}
	return digits
}

// streamSubjectDigest is the digest of the encoding of a subject of kind
// with the parts (appendSubjectEncoding), hashed a piece at a time through
// a buffer of its own, so that what it allocates is the same however long
// the parts are.
func streamSubjectDigest(kind byte, parts []string) [sha256.Size]byte {
	h := sha256.New()
	buffer := make([]byte, 0, 512)
	buffer = append(buffer, kind)
	for _, part := range parts {
		buffer = strconv.AppendInt(buffer, int64(len(part)), 10)
		buffer = append(buffer, 0)
		for part != "" {
			if len(buffer) == cap(buffer) {
				h.Write(buffer)
				buffer = buffer[:0]
			}
			n := copy(buffer[len(buffer):cap(buffer)], part)
			buffer, part = buffer[:len(buffer)+n], part[n:]
		}
		// A length and its NUL, 21 bytes at most, fit after what is
		// left: past half the buffer, it is hashed first.
		if len(buffer) > cap(buffer)/2 {
			h.Write(buffer)
			buffer = buffer[:0]
		}
	}
	h.Write(buffer)
	var digest [sha256.Size]byte
	h.Sum(digest[:0])
	return digest
}

// subjectDigestBytes is how long the digest that begins every key is.
const subjectDigestBytes = sha256.Size

// appendSubjectEncoding appends to dst what the digest of a subject of kind
// with the parts is taken of: the kind, a letter, and then each part after
// its length in decimal and a NUL (appendKeyPart), so a part ends where its
// length says and not where a byte of it says, and no two subjects that
// differ have one encoding.
func appendSubjectEncoding(dst []byte, kind byte, parts ...string) []byte {
	dst = append(dst, kind)
	for _, part := range parts {
		dst = appendKeyPart(dst, part)
	}
	return dst
}

// appendKeyPart appends part to dst as a key holds a part: after its length
// and a NUL.
func appendKeyPart(dst []byte, part string) []byte {
	dst = strconv.AppendInt(dst, int64(len(part)), 10)
	dst = append(dst, 0)
	return append(dst, part...)
}

// putLocked remembers st under key, in place of what was remembered there,
// and counts a rule's failure for the trip st says it is of, in place of
// what the entry it replaces was counted for.
func (f *failureLog) putLocked(key subjectKey, st *failureState) {
	st.key = key
	if was, known := f.entries[key.bytes]; !known || was.rule != st.rule || was.trip != st.trip {
		if known {
			f.uncountLocked(was)
		}
		if st.rule {
			f.ruleFailures[st.trip]++
		}
	}
	f.entries[key.bytes] = st
}

// dropLocked forgets what is remembered under key, if anything is, and
// counts a rule's failure no longer, for its trip. Every entry that goes,
// goes through here, so the counts are those of the entries there are.
func (f *failureLog) dropLocked(key string) {
	st, known := f.entries[key]
	if !known {
		return
	}
	delete(f.entries, key)
	f.uncountLocked(st)
}

// uncountLocked counts st, when it is a rule's failure, no longer for the
// trip it was counted for.
func (f *failureLog) uncountLocked(st *failureState) {
	if !st.rule {
		return
	}
	if f.ruleFailures[st.trip]--; f.ruleFailures[st.trip] <= 0 {
		delete(f.ruleFailures, st.trip)
	}
}

// failed logs a failure at level, or counts it as a repeat.
func (f *failureLog) failed(logger *slog.Logger, level slog.Level, key subjectKey, msg, stage string, err error, attrs ...any) {
	f.failedFor(configRead{}, logger, level, key, msg, stage, err, attrs...)
}

// failedFor is failed for a trip that read its collector as read says. When
// the collector no longer stands (configRead) — a reload removed it, or
// changed its definition, since — the failure is of a collector that is gone:
// it is remembered nowhere, so it is neither taken for the first failure of
// the collector now under the name nor ends a run of that one's, and is
// logged at debug level only, as superseded. So is the failure of a static
// target's scrape whose target a reload removed or changed since
// (logStands).
func (f *failureLog) failedFor(read configRead, logger *slog.Logger, level slog.Level, key subjectKey, msg, stage string, err error, attrs ...any) {
	f.failedOf(read, logger, level, false, key, "", "", "", msg, stage, err, attrs...)
}

// ruleFailedFor is failedFor for the failure of a rule of the trip or file
// whose key is trip, the rule told apart by its metric name, its expression
// and its items (appendRuleFailureKey). It returns the bytes of the key a
// failure of the rule is remembered under when it returns, and nothing when
// none is: a failure that starts while the log is full is not remembered,
// and neither is that of a trip whose collector no longer stands, which
// leaves what is remembered of the rule as it was.
//
// The failure is counted for trip while it is remembered, so the scrape that
// asks with that key whether a rule of its may have recovered
// (remembersRules) is told of it, whatever the two keys hold, and it is
// whose trip is.
//
// The rule's key, whose size is that of its expression, is not made to
// report the failure: its bytes are written where those of the last one
// looked up were (ruleLocked), and a key is made of them only when the
// failure is remembered and was not before. So a rule that fails on every
// scrape has its key made once, and one whose failure a full log does not
// remember has none made, on any of the scrapes the log stays full for.
func (f *failureLog) ruleFailedFor(read configRead, logger *slog.Logger, level slog.Level, trip subjectKey, metric, expression, items, msg, stage string, err error, attrs ...any) string {
	return f.failedOf(read, logger, level, true, trip, metric, expression, items, msg, stage, err, attrs...)
}

// failedOf is failedFor, and ruleFailedFor when rule says the failure is
// that of the rule of the trip whose key is key that metric, expression and
// items tell apart, for which it returns what ruleFailedFor does.
func (f *failureLog) failedOf(read configRead, logger *slog.Logger, level slog.Level, rule bool, key subjectKey, metric, expression, items, msg, stage string, err error, attrs ...any) string {
	errText := ""
	if err != nil {
		// What is remembered is no longer than a failure's text may be,
		// whoever reports the failure: a trip's error is bounded already
		// (boundedTripFailure), and its recognised text is then the one
		// this makes, so the cut costs such an error nothing.
		errText = model.CutTo(model.SameFailureText(err), model.MaxFailureBytes, true)
		attrs = append(attrs, "error", err)
	}
	now := f.now()
	f.mu.Lock()
	var st *failureState
	trip := ""
	if rule {
		// A rule's failure is looked for by the bytes of its key, and is
		// from here on the failure under the key it was first remembered
		// with, or under none yet.
		trip = key.bytes
		st, key.bytes = f.ruleLocked(trip, metric, expression, items), ""
		if st != nil {
			key.bytes = st.key.bytes
		}
	}
	if !read.logStands(key.collector) {
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "superseded", true)...)
		return key.bytes
	}
	f.sweepLocked(now)
	if !rule {
		st = f.entries[key.bytes]
	}
	// A failure not reported for failureLogForget is forgotten: the same
	// failure again, days later, is a new one, not a repeat failing since.
	// So is a rule's that the sweep, which forgets by this same test, has
	// just dropped: it was found before the sweep, and is not looked for
	// again.
	if st != nil && now.Sub(st.seen) > failureLogForget {
		f.dropLocked(key.bytes)
		st = nil
	}
	if st == nil || st.stage != stage || st.err != errText {
		if st != nil || f.rememberLocked() {
			if rule && key.bytes == "" {
				// The one place a rule's key is made: of the bytes it
				// was looked for by, when its failure is first
				// remembered.
				key.bytes = string(f.ruleKey)
			}
			f.putLocked(key, &failureState{rule: rule, trip: trip, stage: stage, err: errText, first: now, logged: now, seen: now, failures: 1})
		} else {
			// The log is full: the failure is under no key.
			key.bytes = ""
		}
		f.mu.Unlock()
		logger.Log(context.Background(), level, msg, attrs...)
		return key.bytes
	}
	st.failures++
	st.seen = now
	if now.Sub(st.logged) < f.interval {
		st.suppressed++
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "repeat", true)...)
		return key.bytes
	}
	repeated, since := st.suppressed+1, st.first
	st.suppressed, st.logged = 0, now
	f.mu.Unlock()
	logger.Log(context.Background(), level, msg, append(attrs, "repeated", repeated, "failing_since", since.UTC().Format(time.RFC3339))...)
	return key.bytes
}

// rememberLocked reports whether there is room for a new entry.
func (f *failureLog) rememberLocked() bool {
	return len(f.entries) < failureLogMaxEntries
}

// sweepLocked forgets the failures not reported for failureLogForget — a
// target no longer probed — at most once a minute, whether or not the log is
// full.
func (f *failureLog) sweepLocked(now time.Time) {
	if now.Sub(f.lastSweep) < time.Minute {
		return
	}
	f.lastSweep = now
	for k, st := range f.entries {
		if now.Sub(st.seen) > failureLogForget {
			f.dropLocked(k)
		}
	}
}

// recovered logs the first success after a failure, and forgets the failure.
// One forgotten already, not reported for failureLogForget, is not logged
// as recovering.
func (f *failureLog) recovered(logger *slog.Logger, key subjectKey, msg string, attrs ...any) {
	f.recoveredFor(configRead{}, logger, key, msg, attrs...)
}

// recoveredFor is recovered for a trip that read its collector as read says:
// the success of a collector that no longer stands (configRead) is no
// recovery of the collector now under the name, whose failure stays
// remembered, and neither is the success of a static target's scrape whose
// target no longer stands (logStands).
func (f *failureLog) recoveredFor(read configRead, logger *slog.Logger, key subjectKey, msg string, attrs ...any) {
	f.mu.Lock()
	st := f.entries[key.bytes]
	// Asked only when there is a failure to forget: a trip that recovers
	// from nothing, as nearly every one does, pays nothing for it.
	if st == nil || !read.logStands(st.key.collector) {
		f.mu.Unlock()
		return
	}
	f.dropLocked(key.bytes)
	stale := f.now().Sub(st.seen) > failureLogForget
	f.mu.Unlock()
	if stale {
		return
	}
	logger.Info(msg, append(attrs, "stage", st.stage, "failed_for", f.now().Sub(st.first).Round(time.Second).String(), "failures", st.failures)...)
}

// remembersRules reports whether a failure of any rule is remembered for the
// trip or file key is of, other than those under the keys in failing: the
// rules that failed on the scrape that asks, which looks for the rules that
// recovered only when one may have. Nearly no scrape has a rule's failure
// remembered, and of those that have, nearly all have only the failures of
// the rules that failed again, as a rule does on every scrape of a target
// that lacks its value. What is asked is what was counted: the failures
// reported as those of rules of key (ruleFailedFor), key being compared
// whole and nothing read out of it or out of a rule's key, and a failure in
// failing is taken from the count only when it is one of them. failing
// holds the keys the failures were remembered under when they were reported:
// one that was not remembered has no key, and is not among those counted.
func (f *failureLog) remembersRules(key subjectKey, failing map[string]bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	remembered := f.ruleFailures[key.bytes]
	if remembered == 0 {
		return false
	}
	for failed := range failing {
		if st := f.entries[failed]; st != nil && st.rule && st.trip == key.bytes {
			remembered--
		}
	}
	return remembered > 0
}

// rememberedRule is the bytes under which a failure of the rule of the trip or
// file key is remembered, the rule told apart by its metric name, its
// expression and its items (appendRuleFailureKey), and whether one is. The
// rule's key, whose size is that of its expression, is not made to ask: its
// bytes are written where those of the last one asked for were, and the key
// a failure is remembered under is the one made when it was first
// remembered (failedOf). So a rule that does not fail has no key made.
func (f *failureLog) rememberedRule(key subjectKey, metric, expression, items string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.ruleLocked(key.bytes, metric, expression, items)
	if st == nil {
		return "", false
	}
	return st.key.bytes, true
}

// ruleLocked is the failure remembered of the rule of the trip or file whose
// key's bytes are trip, nil when none is, found under the lock by the bytes
// of the rule's key, which it leaves in f.ruleKey.
func (f *failureLog) ruleLocked(trip, metric, expression, items string) *failureState {
	f.ruleKey = appendRuleFailureKey(f.ruleKey[:0], trip, metric, expression, items)
	// A map is read by the bytes of a key without a string made of them.
	return f.entries[string(f.ruleKey)]
}

// forget drops what is remembered about key without logging a recovery, for
// a failure whose subject is gone rather than fixed.
func (f *failureLog) forget(key subjectKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropLocked(key.bytes)
}

// forgetCollectors drops what is remembered about collectors a reload removed
// or changed.
func (f *failureLog) forgetCollectors(names map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgetCollectorsLocked(names)
}

// forgetCollectorsLocked is forgetCollectors under the log's lock. An entry
// is a collector's when the trip that reported its failure said so
// (subjectKey): no key is read to tell.
func (f *failureLog) forgetCollectorsLocked(names map[string]bool) {
	for key, st := range f.entries {
		if names[st.key.collector] {
			f.dropLocked(key)
		}
	}
}

// forgetStaticTargetsLocked drops, under the log's lock, what is remembered
// as the named static targets' own, which a reload removed or changed: under
// any collector, since the reload may have given the target another. An
// entry is a static target's own when the scrape or the schedule that
// reported its failure said so (staticTargetKey), whatever the entry's key
// holds: what a probe remembers of a target that reads as a static target's
// name is the probe's, and stays. What a scrape's trip remembers under the
// address it went to — bytes that were not UTF-8, sample lines left out,
// the files of a directory — is told apart by that address, as a probe's
// is, and stays; so does what the static targets endpoint remembers of a
// target's metric it left out, under no collector, which is the endpoint's
// to settle (settleStaticClashes).
func (f *failureLog) forgetStaticTargetsLocked(names map[string]bool) {
	if len(names) == 0 {
		return
	}
	for key, st := range f.entries {
		if st.key.static && names[st.key.target] {
			f.dropLocked(key)
		}
	}
}
