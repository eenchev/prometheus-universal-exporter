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

// Which traces otlp.traces exports is decided once, at the end of a probe
// or a scrape, by rules taken in order, the first that keeps the trace
// saying why:
//
//   - slow: it took otlp.traces.threshold or longer;
//   - error: it failed, and otlp.traces.errors is on;
//   - parent: otlp.traces.parent_based is on, and the probe's caller sent a
//     traceparent with its sampled flag set;
//   - ratio: its trace id falls in the otlp.traces.sample_ratio share, as
//     OpenTelemetry's consistent probability sampling decides it.
//
// A trace no rule keeps is dropped. One kept is then held to
// otlp.traces.max_per_second, a token bucket, before it is queued.
//
// The ratio is decided as OpenTelemetry's TraceIdRatioBased sampler and its
// consistent probability sampling decide it, so that the exporter and every
// tracer of the same trace sampling at the same ratio keep the same traces:
// the trace's randomness, the trace id's last 56 bits as W3C trace context
// level 2 makes them random (or the caller's explicit randomness, ot=rv in
// its tracestate), is compared with a rejection threshold of
// (1 - ratio) * 2^56, and the trace is kept when it is not under it. A
// trace kept so carries the threshold, ot=th, in its tracestate, by which a
// backend counts what each kept trace stands for; one kept for another
// reason was not kept by a probability and carries none of its own.

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

// otlpTraceSamplingOn says whether a rule beside the threshold and errors
// can keep a trace: only then does a kept trace say why on its span
// (sampling.reason), so that the export of a configuration without them is
// what it was before them.
func otlpTraceSamplingOn(cfg model.OTLPConfig) bool {
	return cfg.Traces.ParentBased || cfg.Traces.SampleRatio > 0
}

// otlpMaxRandomness is 2^56: a trace's randomness is under it, and the
// rejection threshold of a ratio is (1 - ratio) of it.
const otlpMaxRandomness = uint64(1) << 56

// ratioThreshold is the rejection threshold of ratio: a trace whose
// randomness is under it is not kept. It is 2^56 less ratio's share of
// 2^56 rounded, as OpenTelemetry's sampling packages work it out, so that
// the threshold written in ot=th is theirs for the same ratio: 0.01 is
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

// traceRandomness is a trace's randomness: the caller's explicit one, rv in
// the ot member of its tracestate, where it sent a valid one, and otherwise
// the trace id's last seven bytes, which trace context level 2 makes random,
// as crypto/rand makes the exporter's own ids.
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
	// traceID is the trace's id: the caller's, or one made for the ratio's
	// rule; zero where no rule needed one, and buildSpans makes it.
	traceID [16]byte
	// randomness and threshold are what the ratio's rule compared, set
	// when it was reached.
	randomness, threshold uint64
	ratioTried            bool
}

// decideTrace applies the rules to a probe's or a scrape's trace that took
// elapsed and failed as failed says, whose caller sent parent, if any.
// It is run at the end of every probe while the trace export is on, and
// makes nothing of the trace's spans: they are made only of one kept.
func decideTrace(cfg model.OTLPConfig, elapsed time.Duration, failed bool, parent traceParent) traceDecision {
	d := traceDecision{}
	if parent.ok {
		d.traceID = parent.traceID
	}
	t := cfg.Traces
	switch {
	case elapsed >= otlpTraceThreshold(cfg):
		d.reason = keptSlow
	case failed && t.Errors:
		d.reason = keptError
	case t.ParentBased && parent.ok && parent.sampled:
		d.reason = keptParent
	case t.SampleRatio > 0:
		if !parent.ok {
			d.traceID = newTraceID()
		}
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
// commas as several are read. One that cannot be read — a member without a
// key or with one that is no key, a value that is no value, a key twice, or
// more than 32 members — is no tracestate, as the W3C asks of a header it
// cannot parse: it is not propagated.
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

// traceStateKey says whether key is a tracestate key: a simple key of up to
// 256 of a-z, 0-9, _, -, *, / beginning with a letter, or a tenant's,
// tenant@system, its tenant of up to 241 and its system of up to 14 that
// begins with a letter.
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

// withOT is the state with its ot member's th set to th, or left out when
// th is "", the ot member's other fields kept in their order, and the ot
// member moved to the front, as the W3C asks of a member changed; the
// rightmost members are dropped past 32. An ot member left with no field is
// left out.
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

// spanTraceState is the tracestate the spans of a trace kept for reason
// carry: a trace the ratio kept carries its threshold, ot=th, before the
// caller's members; one its caller's sampled flag kept, the caller's
// tracestate as it came, whose threshold, if any, is the probability its
// caller kept it by and so the exporter's span too; and one kept for being
// slow or failed the caller's without a threshold, since no probability
// kept it, and a backend that counts what a trace stands for by its
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

// traceRate is otlp.traces.max_per_second: a token bucket of perSecond
// tokens a second and as many at most, so that a burst of perSecond traces
// goes through at once and then perSecond a second. A trace exported takes
// a token, and one that finds none is dropped. One kept by parent_based or
// sample_ratio takes a token only while half the bucket, rounded down,
// would be left after it, so that those, which are many when the ratio is
// high, cannot spend what a slow or a failed trace, the reason the export
// is there, would find: a slow or failed one may take the last token. A
// bucket of 1 keeps nothing back. The lock is held for a few sums.
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

// otlpTraceTally counts the traces the rules and the cap decided of since
// the start, by why: kept by reason and handed to the queue, and dropped as
// not sampled, over the rate, or for want of room in the queue, which a
// kept trace may be after. They are counted at the end of every probe while
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
