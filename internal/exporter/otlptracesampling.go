package exporter

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// otlp.traces decides which traces to export once, at the end of a probe or
// a scrape. The rules are tried in order, and the first one that keeps the
// trace gives the reason:
//
//   - slow: it took otlp.traces.threshold or longer;
//   - error: it failed, and otlp.traces.errors is on;
//   - parent: otlp.traces.parent_based is on, and the probe's caller sent a
//     traceparent with its sampled flag set;
//   - ratio: its trace id falls in the otlp.traces.sample_ratio share, as
//     OpenTelemetry's consistent probability sampling decides it.
//
// A trace that no rule keeps is dropped. A kept trace is then held to
// otlp.traces.max_per_second, a token bucket, before it is queued.
//
// The ratio is decided as OpenTelemetry's TraceIdRatioBased sampler and its
// consistent probability sampling decide it. So the exporter and every
// tracer of the same trace that samples at the same ratio keep the same
// traces. The trace's randomness is the last 56 bits of the trace id, which
// W3C trace context level 2 makes random, or the caller's explicit
// randomness (ot=rv in its tracestate). It is compared with a rejection
// threshold of (1 - ratio) * 2^56, and the trace is kept when it is not
// under it. A trace kept this way carries the threshold, ot=th, in its
// tracestate, so a backend can count what each kept trace stands for. A
// trace kept for another reason was not kept by a probability: it carries
// no threshold of its own.

// samplingReason is why a trace is exported, or keptNone that it is not.
type samplingReason int

const (
	keptNone samplingReason = iota
	keptSlow
	keptError
	keptParent
	keptRatio
	samplingReasons
)

// samplingReasonNames are the reasons as sampling.reason, the self-metrics
// and the status page name them.
var samplingReasonNames = [samplingReasons]string{"", "slow", "error", "parent", "ratio"}

// otlpTraceSamplingOn says whether a rule other than the threshold and
// errors can keep a trace. Only then does a kept trace record the reason on
// its span (sampling.reason), so a configuration without those rules
// exports what it did before they existed.
func otlpTraceSamplingOn(cfg model.OTLPConfig) bool {
	return cfg.Traces.ParentBased || cfg.Traces.SampleRatio > 0
}

// otlpMaxRandomness is 2^56: a trace's randomness is under it, and the
// rejection threshold of a ratio is (1 - ratio) of it.
const otlpMaxRandomness = uint64(1) << 56

// ratioThreshold is the rejection threshold of ratio: a trace whose
// randomness is under it is not kept. It is 2^56 minus ratio's share of
// 2^56, rounded, worked out as OpenTelemetry's sampling packages do. So the
// threshold written in ot=th matches theirs for the same ratio: 0.01 is
// fd70a3d70a3d71.
func ratioThreshold(ratio float64) uint64 {
	if ratio <= 0 {
		return otlpMaxRandomness
	}
	if ratio >= 1 {
		return 0
	}
	return otlpMaxRandomness - uint64(math.Round(ratio*float64(otlpMaxRandomness)))
}

// thresholdText is a threshold as ot=th writes it: 14 hex digits with the
// trailing zeros left out, 0 for a threshold of none, which keeps every
// trace.
func thresholdText(threshold uint64) string {
	text := strings.TrimRight(fmt.Sprintf("%014x", threshold), "0")
	if text == "" {
		return "0"
	}
	return text
}

// traceRandomness is a trace's randomness. It is the caller's explicit
// randomness (rv in the ot member of its tracestate) when the caller sent a
// valid one. Otherwise it is the trace id's last seven bytes, which trace
// context level 2 makes random, as crypto/rand makes the exporter's own ids.
func traceRandomness(id [16]byte, state traceState) uint64 {
	if rv, ok := state.otValue("rv"); ok && len(rv) == 14 {
		if n, err := strconv.ParseUint(rv, 16, 64); err == nil {
			return n
		}
	}
	return binary.BigEndian.Uint64(id[8:]) & (otlpMaxRandomness - 1)
}

// traceDecision is what the rules decided of a trace, and of the ratio's
// rule what it went by, which the debug page tells.
type traceDecision struct {
	reason samplingReason
	// traceID is the trace's id: the caller's, or the one made when the
	// trace began.
	traceID [16]byte
	// randomness and threshold are what the ratio's rule compared, set
	// when it was reached.
	randomness, threshold uint64
	ratioTried            bool
}

// decideTrace applies the rules to the trace of a probe or a scrape. The
// trace took elapsed, failed as failed says, and its caller sent parent, if
// any. It runs at the end of every probe while the trace export is on. It
// builds none of the trace's spans: spans are built only for a kept trace.
func decideTrace(cfg model.OTLPConfig, elapsed time.Duration, failed bool, parent traceParent) traceDecision {
	id := parent.traceID
	if !parent.ok {
		id = newTraceID()
	}
	return decideTraceOf(cfg, elapsed, failed, parent, id)
}

// decideTraceOf is decideTrace of the trace whose id is id, made when it
// began (traceLink): the caller's, or one of its own.
func decideTraceOf(cfg model.OTLPConfig, elapsed time.Duration, failed bool, parent traceParent, id [16]byte) traceDecision {
	d := traceDecision{traceID: id}
	t := cfg.Traces
	switch {
	case elapsed >= otlpTraceThreshold(cfg):
		d.reason = keptSlow
	case failed && t.Errors:
		d.reason = keptError
	case t.ParentBased && parent.ok && parent.sampled:
		d.reason = keptParent
	case t.SampleRatio > 0:
		d.ratioTried = true
		d.randomness, d.threshold = traceRandomness(d.traceID, parent.state), ratioThreshold(t.SampleRatio)
		if d.randomness >= d.threshold {
			d.reason = keptRatio
		}
	}
	return d
}

// ratioText is a ratio as the debug page and the status page write it: a
// percentage, to twelve digits, so that 0.07 is 7% rather than the
// 7.000000000000001% its product is.
func ratioText(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'g', 12, 64) + "%"
}

// why says why the trace that took elapsed and failed as failed says is
// exported, or why not, as the debug page tells it.
func (d traceDecision) why(cfg model.OTLPConfig, elapsed time.Duration, failed bool, parent traceParent) string {
	threshold := otlpTraceThreshold(cfg)
	took := shownScrapeDuration(elapsed)
	t := cfg.Traces
	sampled := func(kept bool) string {
		if kept {
			return fmt.Sprintf("sampled at %s by otlp.traces.sample_ratio (trace id randomness 0x%014x ≥ threshold 0x%014x)", ratioText(t.SampleRatio), d.randomness, d.threshold)
		}
		return fmt.Sprintf("not sampled at %s by otlp.traces.sample_ratio (trace id randomness 0x%014x < threshold 0x%014x)", ratioText(t.SampleRatio), d.randomness, d.threshold)
	}
	switch d.reason {
	case keptSlow:
		return fmt.Sprintf("it took %s, at least otlp.traces.threshold %s", took, threshold)
	case keptError:
		return fmt.Sprintf("it failed, and otlp.traces.errors exports every failed trace; it took %s, under otlp.traces.threshold %s", took, threshold)
	case keptParent:
		return fmt.Sprintf("its caller's traceparent has the sampled flag, and otlp.traces.parent_based exports it; it took %s, under otlp.traces.threshold %s", took, threshold)
	case keptRatio:
		return fmt.Sprintf("it was %s; it took %s, under otlp.traces.threshold %s", sampled(true), took, threshold)
	}
	reasons := []string{fmt.Sprintf("it took %s, under otlp.traces.threshold %s", took, threshold)}
	switch {
	case failed:
		reasons = append(reasons, "it failed, which without otlp.traces.errors exports nothing")
	case t.Errors:
		reasons = append(reasons, "it did not fail")
	}
	if t.ParentBased {
		switch {
		case !parent.ok:
			reasons = append(reasons, "it has no caller's traceparent for otlp.traces.parent_based")
		default:
			reasons = append(reasons, "its caller's traceparent has no sampled flag")
		}
	}
	if d.ratioTried {
		reasons = append(reasons, "it was "+sampled(false))
	}
	return strings.Join(reasons, "; ")
}

// traceState is a W3C tracestate's list members, the leftmost first, each
// key=value; nil when there was none, or one that could not be read.
type traceState []string

// traceStateMaxMembers is the most list members a tracestate may hold.
const traceStateMaxMembers = 32

// parseTraceState reads the tracestate headers a caller sent, joined by
// commas when there are several. A header that cannot be read is no
// tracestate and is not propagated, as the W3C asks for a header it cannot
// parse. That is a member without a key or with an invalid key, an invalid
// value, a repeated key, or more than 32 members.
func parseTraceState(values []string) traceState {
	if len(values) == 0 {
		return nil
	}
	var members traceState
	seen := map[string]bool{}
	for _, value := range values {
		for member := range strings.SplitSeq(value, ",") {
			member = strings.Trim(member, " \t")
			if member == "" {
				continue
			}
			key, v, found := strings.Cut(member, "=")
			if !found || !traceStateKey(key) || !traceStateValue(v) || seen[key] {
				return nil
			}
			seen[key] = true
			members = append(members, member)
		}
	}
	if len(members) > traceStateMaxMembers {
		return nil
	}
	return members
}

// traceStateKey says whether key is a tracestate key. That is a simple key
// of up to 256 of a-z, 0-9, _, -, *, / that begins with a letter, or a
// tenant key, tenant@system, with a tenant of up to 241 and a system of up
// to 14 that begins with a letter.
func traceStateKey(key string) bool {
	simple := func(s string, most int, digitFirst bool) bool {
		if s == "" || len(s) > most {
			return false
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= '0' && c <= '9':
				if i == 0 && !digitFirst {
					return false
				}
			case i > 0 && (c == '_' || c == '-' || c == '*' || c == '/'):
			default:
				return false
			}
		}
		return true
	}
	tenant, system, multi := strings.Cut(key, "@")
	if !multi {
		return simple(key, 256, false)
	}
	return simple(tenant, 241, true) && simple(system, 14, false)
}

// traceStateValue says whether v is a tracestate value: up to 256 printable
// ASCII characters but comma and equals, not ending in a space.
func traceStateValue(v string) bool {
	if v == "" || len(v) > 256 || v[len(v)-1] == ' ' {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

// otValue is the value of sub-key key of the state's ot member, which
// OpenTelemetry writes as key:value pairs separated by semicolons.
func (s traceState) otValue(key string) (string, bool) {
	for _, member := range s {
		if v, ok := strings.CutPrefix(member, "ot="); ok {
			for field := range strings.SplitSeq(v, ";") {
				if value, ok := strings.CutPrefix(field, key+":"); ok {
					return value, true
				}
			}
		}
	}
	return "", false
}

// withOT returns the state with its ot member's th set to th, or removed
// when th is "". The ot member's other fields keep their order, and the ot
// member moves to the front, as the W3C asks for a changed member. Members
// past 32 are dropped from the right. An ot member left with no field is
// removed.
func (s traceState) withOT(th string) traceState {
	var fields []string
	if th != "" {
		fields = append(fields, "th:"+th)
	}
	rest := make(traceState, 0, len(s)+1)
	changed := th != ""
	for _, member := range s {
		v, ok := strings.CutPrefix(member, "ot=")
		if !ok {
			rest = append(rest, member)
			continue
		}
		for field := range strings.SplitSeq(v, ";") {
			if strings.HasPrefix(field, "th:") {
				changed = true
				continue
			}
			if field != "" {
				fields = append(fields, field)
			}
		}
	}
	if !changed {
		return s
	}
	out := rest
	if len(fields) > 0 {
		out = append(traceState{"ot=" + strings.Join(fields, ";")}, rest...)
	}
	if len(out) > traceStateMaxMembers {
		out = out[:traceStateMaxMembers]
	}
	return out
}

func (s traceState) String() string { return strings.Join(s, ",") }

// spanTraceState is the tracestate that the spans of a trace kept for
// reason carry. A trace the ratio kept carries its threshold, ot=th, before
// the caller's members. A trace its caller's sampled flag kept carries the
// caller's tracestate as it came; its threshold, if any, is the probability
// the caller kept it by, and so the exporter's span too. A trace kept for
// being slow or failed carries the caller's without a threshold, since no
// probability kept it: a backend that counts what a trace stands for by its
// threshold would count it too many times.
func spanTraceState(reason samplingReason, ratio float64, caller traceState) string {
	switch reason {
	case keptRatio:
		return caller.withOT(thresholdText(ratioThreshold(ratio))).String()
	case keptParent:
		return caller.String()
	}
	return caller.withOT("").String()
}

// traceRate is otlp.traces.max_per_second: a token bucket that gains
// perSecond tokens a second and holds at most perSecond. A burst of
// perSecond traces goes through at once, then perSecond a second. An
// exported trace takes a token; one that finds none is dropped. A trace kept
// by parent_based or sample_ratio takes a token only while half the bucket,
// rounded down, would be left after it. Those traces are many when the ratio
// is high, and must not spend what a slow or failed trace (the reason the
// export exists) would find. A slow or failed trace may take the last token.
// A bucket of 1 keeps nothing back. The lock is held for a few sums.
type traceRate struct {
	mu        sync.Mutex
	perSecond int
	tokens    float64
	last      time.Time
}

// configureLocked sets the rate, 0 for none. A bucket starts full; one
// whose rate changes, as by a reload, keeps its tokens, within the new
// bound.
func (r *traceRate) configureLocked(perSecond int) {
	if perSecond == r.perSecond {
		return
	}
	if r.perSecond == 0 {
		r.tokens, r.last = float64(perSecond), time.Time{}
	}
	r.perSecond = perSecond
	r.tokens = min(r.tokens, float64(perSecond))
}

// take says whether a trace kept for reason may be exported at now under a
// cap of perSecond, the configuration's in force, and takes its token when
// it may.
func (r *traceRate) take(perSecond int, reason samplingReason, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configureLocked(perSecond)
	if r.perSecond <= 0 {
		return true
	}
	burst := float64(r.perSecond)
	if !r.last.IsZero() && now.After(r.last) {
		r.tokens = min(burst, r.tokens+now.Sub(r.last).Seconds()*burst)
	}
	if r.last.IsZero() || now.After(r.last) {
		r.last = now
	}
	least := 1.0
	if reason == keptParent || reason == keptRatio {
		least += float64(r.perSecond / 2)
	}
	if r.tokens < least {
		return false
	}
	r.tokens--
	return true
}

// otlpTraceTally counts, by reason, the traces the rules and the cap decided
// on since the start: kept by reason and handed to the queue, or dropped as
// not sampled, over the rate, or for lack of room in the queue (which can
// happen to a kept trace). They are counted at the end of every probe while
// the export is on, and so without a lock.
type otlpTraceTally struct {
	kept    [samplingReasons]atomic.Int64
	dropped [traceDropReasons]atomic.Int64
}

// traceDropReason is why a whole trace was dropped.
type traceDropReason int

const (
	traceDroppedNotSampled traceDropReason = iota
	traceDroppedRate
	traceDroppedQueue
	traceDropReasons
)

// traceDropReasonNames are the reasons as the self-metrics' reason label
// and the status page name them.
var traceDropReasonNames = [traceDropReasons]string{"not_sampled", "rate", "queue"}
