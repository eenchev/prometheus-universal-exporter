package exporter

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sampling rules of otlp.traces (otlptracesampling.go): their order and
// reasons, the ratio decided by the trace id as OpenTelemetry's consistent
// probability sampling decides it, its threshold as ot=th writes it, the
// tracestate the spans carry, the caller's sampled flag, and the rate cap.

// idWithRandomness is a trace id whose last 56 bits are randomness, and
// whose first nine bytes are high, which the ratio does not go by.
func idWithRandomness(randomness uint64) [16]byte {
	var id [16]byte
	for i := range 9 {
		id[i] = 0xff
	}
	binary.BigEndian.PutUint64(id[8:], randomness)
	id[8] = 0xff
	return id
}

func sampledParent(id [16]byte, sampled bool) traceParent {
	return traceParent{traceID: id, spanID: [8]byte{1}, sampled: sampled, ok: true}
}

// The thresholds are OpenTelemetry's for the same ratios, as its
// specification of probability sampling in tracestate gives them: the
// rejection threshold, (1 - ratio) * 2^56, in hex with its trailing zeros
// left out, and 0, which keeps every trace, for a ratio of 1.
func TestRatioThresholdsAreOpenTelemetrys(t *testing.T) {
	for ratio, want := range map[float64]string{0.25: "c", 0.5: "8", 0.01: "fd70a3d70a3d71", 0.1: "e6666666666666", 1: "0", 0.75: "4"} {
		if got := thresholdText(ratioThreshold(ratio)); got != want {
			t.Errorf("the threshold of %v is th:%s, want th:%s", ratio, got, want)
		}
	}
	for ratio, want := range map[float64]string{0.07: "7%", 0.01: "1%", 1: "100%", 0.125: "12.5%", 0.0001: "0.01%"} {
		if got := ratioText(ratio); got != want {
			t.Errorf("the ratio %v is written %s, want %s", ratio, got, want)
		}
	}
	if got := ratioThreshold(0); got != otlpMaxRandomness {
		t.Errorf("the threshold of 0 is %x, want 2^56, which keeps nothing", got)
	}
}

// The ratio goes by the trace id's last 56 bits alone: the same id is
// decided alike every time, an id at the threshold is kept and one just
// under it is not, a ratio of 1 keeps the id of no randomness, and a ratio
// of 0 is no rule. A caller's explicit randomness, ot=rv, is what goes, in
// place of its trace id's.
func TestTheRatioDecidesByTheTraceIDsRandomness(t *testing.T) {
	cfg := model.OTLPConfig{Traces: model.OTLPTracesConfig{Threshold: model.Duration(time.Hour), SampleRatio: 0.25}}
	decide := func(cfg model.OTLPConfig, id [16]byte, state traceState) samplingReason {
		parent := sampledParent(id, false)
		parent.state = state
		return decideTrace(cfg, time.Millisecond, false, parent).reason
	}
	for randomness, want := range map[uint64]samplingReason{
		0xc0000000000000: keptRatio, 0xc0000000000001: keptRatio, 0xffffffffffffff: keptRatio,
		0xbfffffffffffff: keptNone, 0: keptNone, 0x12345678abcdef: keptNone,
	} {
		for range 3 {
			if got := decide(cfg, idWithRandomness(randomness), nil); got != want {
				t.Errorf("at 0.25 the randomness %014x is decided %q, want %q", randomness, samplingReasonNames[got], samplingReasonNames[want])
			}
		}
	}
	// The first byte of the id's second half is not randomness.
	low := idWithRandomness(0xc0000000000000)
	low[8] = 0
	if got := decide(cfg, low, nil); got != keptRatio {
		t.Errorf("the eighth byte from the end decided the trace: %q", samplingReasonNames[got])
	}
	all := cfg
	all.Traces.SampleRatio = 1
	if got := decide(all, idWithRandomness(0), nil); got != keptRatio {
		t.Errorf("a ratio of 1 does not keep the trace of no randomness: %q", samplingReasonNames[got])
	}
	none := cfg
	none.Traces.SampleRatio = 0
	if d := decideTrace(none, time.Millisecond, false, sampledParent(idWithRandomness(0xffffffffffffff), false)); d.reason != keptNone || d.ratioTried {
		t.Errorf("a ratio of 0 is a rule: %+v", d)
	}
	if got := decide(cfg, idWithRandomness(0), traceState{"ot=th:8;rv:c0000000000000"}); got != keptRatio {
		t.Errorf("the caller's explicit randomness is not what decides: %q", samplingReasonNames[got])
	}
	if got := decide(cfg, idWithRandomness(0xffffffffffffff), traceState{"ot=rv:00000000000001"}); got != keptNone {
		t.Errorf("the trace id's randomness decided beside an explicit one: %q", samplingReasonNames[got])
	}
	// An id the exporter makes is decided too, and is the id its spans
	// are of.
	d := decideTrace(all, time.Millisecond, false, traceParent{})
	if d.reason != keptRatio || d.traceID == [16]byte{} {
		t.Errorf("a trace without a caller's: %+v", d)
	}
	root := spanRoot{name: "probe x"}
	sampledRoot(&root, all, d, traceParent{})
	if spans := buildSpans(root, nil, nil); spans[0].TraceID != fmt.Sprintf("%x", d.traceID) {
		t.Errorf("the spans are of trace %s, the decision of %x", spans[0].TraceID, d.traceID)
	}
}

// The rules are tried in order, the first that keeps the trace saying why:
// slow, then failed under errors, then the caller's sampled flag under
// parent_based, then the ratio; none keeps it otherwise.
func TestTheSamplingRulesAreTriedInOrder(t *testing.T) {
	kept := idWithRandomness(0xffffffffffffff)
	dropped := idWithRandomness(0)
	all := model.OTLPTracesConfig{Threshold: model.Duration(time.Second), Errors: true, ParentBased: true, SampleRatio: 0.5}
	for name, test := range map[string]struct {
		traces  model.OTLPTracesConfig
		elapsed time.Duration
		failed  bool
		parent  traceParent
		want    samplingReason
	}{
		"slow, failed, sampled and in the ratio": {all, 2 * time.Second, true, sampledParent(kept, true), keptSlow},
		"slow at the threshold":                  {all, time.Second, false, traceParent{}, keptSlow},
		"failed, sampled and in the ratio":       {all, time.Millisecond, true, sampledParent(kept, true), keptError},
		"failed without errors, sampled":         {model.OTLPTracesConfig{Threshold: all.Threshold, ParentBased: true}, time.Millisecond, true, sampledParent(dropped, true), keptParent},
		"sampled and in the ratio":               {all, time.Millisecond, false, sampledParent(kept, true), keptParent},
		"not sampled, in the ratio":              {all, time.Millisecond, false, sampledParent(kept, false), keptRatio},
		"not sampled, out of the ratio":          {all, time.Millisecond, false, sampledParent(dropped, false), keptNone},
		"sampled without parent_based":           {model.OTLPTracesConfig{Threshold: all.Threshold, Errors: true}, time.Millisecond, false, sampledParent(kept, true), keptNone},
		"no caller's with parent_based":          {model.OTLPTracesConfig{Threshold: all.Threshold, ParentBased: true}, time.Millisecond, false, traceParent{}, keptNone},
		"failed without errors":                  {model.OTLPTracesConfig{Threshold: all.Threshold}, time.Millisecond, true, traceParent{}, keptNone},
	} {
		if got := decideTrace(model.OTLPConfig{Traces: test.traces}, test.elapsed, test.failed, test.parent).reason; got != test.want {
			t.Errorf("%s: kept as %q, want %q", name, samplingReasonNames[got], samplingReasonNames[test.want])
		}
	}
}

// The debug page's line says which rule keeps the trace, or each reason it
// is not kept, the ratio's with the randomness and the threshold.
func TestTheDecisionSaysWhy(t *testing.T) {
	cfg := model.OTLPConfig{Traces: model.OTLPTracesConfig{Threshold: model.Duration(time.Second), ParentBased: true, SampleRatio: 0.01}}
	parent := sampledParent(idWithRandomness(0xffffffffffffff), false)
	d := decideTrace(cfg, 12*time.Millisecond, false, parent)
	if got, want := d.why(cfg, 12*time.Millisecond, false, parent), "it was sampled at 1% by otlp.traces.sample_ratio (trace id randomness 0xffffffffffffff ≥ threshold 0xfd70a3d70a3d71); it took 12ms, under otlp.traces.threshold 1s"; got != want {
		t.Errorf("the kept trace's line:\n%s\nwant\n%s", got, want)
	}
	parent = sampledParent(idWithRandomness(0x10), false)
	d = decideTrace(cfg, 12*time.Millisecond, false, parent)
	if got, want := d.why(cfg, 12*time.Millisecond, false, parent), "it took 12ms, under otlp.traces.threshold 1s; its caller's traceparent has no sampled flag; it was not sampled at 1% by otlp.traces.sample_ratio (trace id randomness 0x00000000000010 < threshold 0xfd70a3d70a3d71)"; got != want {
		t.Errorf("the dropped trace's line:\n%s\nwant\n%s", got, want)
	}
	parent.sampled = true
	d = decideTrace(cfg, 12*time.Millisecond, false, parent)
	if why := d.why(cfg, 12*time.Millisecond, false, parent); !strings.Contains(why, "its caller's traceparent has the sampled flag, and otlp.traces.parent_based exports it") {
		t.Errorf("the line of a trace its caller sampled: %s", why)
	}
	// With no caller's traceparent the trace id is made anew, at random, so
	// the ratio is switched off here: at 1% it kept one such trace in about
	// a hundred, and the line then began with the ratio's keeping.
	cfg.Traces.Errors, cfg.Traces.SampleRatio = true, 0
	d = decideTrace(cfg, 12*time.Millisecond, false, traceParent{})
	if why := d.why(cfg, 12*time.Millisecond, false, traceParent{}); !strings.Contains(why, "; it did not fail; it has no caller's traceparent for otlp.traces.parent_based") {
		t.Errorf("the line of a trace of no caller's: %s", why)
	}
}

// A traceparent's sampled flag is its flags' lowest bit, of a header that
// can be read alone.
func TestTheTraceparentsSampledFlagIsRead(t *testing.T) {
	const id = "4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7"
	for header, want := range map[string]bool{"00-" + id + "-01": true, "00-" + id + "-00": false, "00-" + id + "-03": true, "00-" + id + "-02": false, "01-" + id + "-01-what": true} {
		if p := parseTraceparent(header); !p.ok || p.sampled != want {
			t.Errorf("%s is read as %+v, want sampled %v", header, p, want)
		}
	}
	if p := parseTraceparent("00-" + id + "-0g"); p.ok || p.sampled {
		t.Errorf("a traceparent whose flags are no hex is read as %+v", p)
	}
}

// The spans of a trace the ratio kept carry its threshold in the ot member
// of their tracestate, before the caller's members, the ot member's other
// fields kept and its threshold replaced; one the caller's flag kept carry
// the caller's tracestate as it came; and one kept for being slow or failed
// the caller's without a threshold. The rightmost members past 32 are
// dropped.
func TestTheSpansTraceStateCarriesTheThreshold(t *testing.T) {
	caller := parseTraceState([]string{"congo=t61rcWkgMzE, ot=rv:abcdefabcdefab;th:8", "rojo=00f067aa0ba902b7"})
	if want := (traceState{"congo=t61rcWkgMzE", "ot=rv:abcdefabcdefab;th:8", "rojo=00f067aa0ba902b7"}); !slices.Equal(caller, want) {
		t.Fatalf("the caller's tracestate is read as %q", caller)
	}
	for reason, want := range map[samplingReason]string{
		keptRatio:  "ot=th:c;rv:abcdefabcdefab,congo=t61rcWkgMzE,rojo=00f067aa0ba902b7",
		keptParent: "congo=t61rcWkgMzE,ot=rv:abcdefabcdefab;th:8,rojo=00f067aa0ba902b7",
		keptSlow:   "ot=rv:abcdefabcdefab,congo=t61rcWkgMzE,rojo=00f067aa0ba902b7",
		keptError:  "ot=rv:abcdefabcdefab,congo=t61rcWkgMzE,rojo=00f067aa0ba902b7",
	} {
		if got := spanTraceState(reason, 0.25, caller); got != want {
			t.Errorf("a trace kept as %s carries %q, want %q", samplingReasonNames[reason], got, want)
		}
	}
	for reason, want := range map[samplingReason]string{keptRatio: "ot=th:fd70a3d70a3d71", keptSlow: "", keptParent: ""} {
		if got := spanTraceState(reason, 0.01, nil); got != want {
			t.Errorf("a trace of no caller's kept as %s carries %q, want %q", samplingReasonNames[reason], got, want)
		}
	}
	if got := spanTraceState(keptSlow, 0.5, traceState{"ot=th:8"}); got != "" {
		t.Errorf("a slow trace whose caller's ot held a threshold alone carries %q", got)
	}
	if got := spanTraceState(keptSlow, 0.5, traceState{"a=1", "ot=rv:abcdefabcdefab"}); got != "a=1,ot=rv:abcdefabcdefab" {
		t.Errorf("a slow trace's tracestate without a threshold was changed: %q", got)
	}
	var full []string
	for i := range 32 {
		full = append(full, fmt.Sprintf("v%d=x", i))
	}
	members := parseTraceState([]string{strings.Join(full, ",")})
	if len(members) != 32 {
		t.Fatalf("32 members are read as %d", len(members))
	}
	got := strings.Split(spanTraceState(keptRatio, 1, members), ",")
	if len(got) != 32 || got[0] != "ot=th:0" || got[31] != "v30=x" {
		t.Errorf("a full tracestate with a threshold added is %d members, from %s to %s", len(got), got[0], got[len(got)-1])
	}
	for _, bad := range []string{"Upper=1", "a=1,a=2", "a", "=1", "a=b=c", "1a=x", "a=x\x01", strings.Join(append(full, "v32=x"), ",")} {
		if s := parseTraceState([]string{bad}); s != nil {
			t.Errorf("the tracestate %.40q is read as %q, want none", bad, s)
		}
	}
	if s := parseTraceState([]string{"tenant1@sys=x, ,", "b=y"}); !slices.Equal(s, traceState{"tenant1@sys=x", "b=y"}) {
		t.Errorf("a tenant's key and empty members are read as %q", s)
	}
}

// otlp.traces.max_per_second is a bucket of as many tokens, which fills at
// as many a second: a burst of as many goes through, then as many a second,
// whatever kept them, slow traces too. A trace kept by parent_based or the
// ratio takes a token only while half the bucket would be left after it, so
// that a slow or failed one finds the rest; a bucket of 1 keeps nothing
// back.
func TestTheRateCapIsATokenBucket(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var r traceRate
	perSecond := 4
	var took []bool
	for range 5 {
		took = append(took, r.take(perSecond, keptSlow, start))
	}
	if want := []bool{true, true, true, true, false}; !slices.Equal(took, want) {
		t.Errorf("a burst of slow traces takes %v, want %v", took, want)
	}
	if r.take(perSecond, keptSlow, start.Add(200*time.Millisecond)) {
		t.Error("a slow trace was taken before a token was back")
	}
	if !r.take(perSecond, keptSlow, start.Add(250*time.Millisecond)) || r.take(perSecond, keptError, start.Add(250*time.Millisecond)) {
		t.Error("a quarter of a second did not give back one token of four a second")
	}
	if !r.take(perSecond, keptError, start.Add(time.Hour)) {
		t.Error("an hour later the bucket is empty")
	}
	// A full bucket again: two of the ratio's and the parent's, which
	// leave two, then none of theirs, while slow and failed ones take
	// the last two.
	r = traceRate{}
	took = nil
	for _, reason := range []samplingReason{keptRatio, keptParent, keptRatio, keptSlow, keptRatio, keptError, keptSlow} {
		took = append(took, r.take(perSecond, reason, start))
	}
	if want := []bool{true, true, false, true, false, true, false}; !slices.Equal(took, want) {
		t.Errorf("ratio, parent, ratio, slow, ratio, error, slow take %v, want %v", took, want)
	}
	// A bucket of one keeps nothing back.
	r, perSecond = traceRate{}, 1
	if !r.take(perSecond, keptRatio, start) || r.take(perSecond, keptSlow, start) {
		t.Error("a bucket of 1 kept its token back from the ratio's trace")
	}
	// No cap takes everything; a reload to a lower cap keeps the tokens
	// within it, and a time gone back gives nothing.
	r, perSecond = traceRate{}, 0
	for range 100 {
		if !r.take(perSecond, keptRatio, start) {
			t.Fatal("no cap dropped a trace")
		}
	}
	r.take(10, keptSlow, start)
	perSecond = 2
	took = nil
	for _, at := range []time.Time{start, start, start.Add(-time.Second)} {
		took = append(took, r.take(perSecond, keptSlow, at))
	}
	if want := []bool{true, true, false}; !slices.Equal(took, want) {
		t.Errorf("a lower cap of 2 takes %v, want %v", took, want)
	}
}
