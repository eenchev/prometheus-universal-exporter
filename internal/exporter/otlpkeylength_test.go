package exporter

// The keys of the OTLP export as they are remembered (otlpKeyOf): a key of
// up to otlpKeyLongest bytes as itself, a longer one by its digest, so that
// what a point waiting and a start time keep of a series is bounded however
// long its labels are, a probe's target among them.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// headOTLPKeys is the export as it was before a key was remembered by its
// digest: every point waiting, and every start time, under its whole key.
// Its resources and their points are in the order of their keys, and a
// name is one writer's, as the export's are (ownOTLPKeys).
func headOTLPKeys() *otlpKeyModel {
	return &otlpKeyModel{
		oneWriter: true,
		resource:  func(r otlpResourceIdentity) string { return r.key() },
		series:    otlpMetricKey,
		start: func(resource string, m model.Metric) string {
			return otlpSeriesKey(string(appendKeyPart(nil, resource)), &m)
		},
		pending: map[string]*otlpModelResource{}, starts: newOTLPStartTimes(),
	}
}

// keyLengthTarget is a probe's target of about n bytes, now and then with
// a NUL, a %00 the query unescaped.
func keyLengthTarget(run *keyRun, n int) string {
	target := "http://203.0.113.7/" + strings.Repeat("p", max(n-19, 0))
	if run.random.IntN(5) == 0 {
		at := run.random.IntN(len(target))
		target = target[:at] + "\x00" + target[at:]
	}
	return target
}

// Runs of exports of probes with otlp.probe_attributes, whose targets are
// of every length from a few bytes to past otlpKeyLongest and of 8 KiB, so
// that the keys of their series, in the queue and among the start times,
// are remembered as themselves, of every length up to the longest, and by
// their digests, also those of exactly a digest's length — beside static
// targets' resources whose attributes make the start times' keys long
// where the queue's are short — with exports that fail and points that wait
// again, are sent byte for byte as they were sent while every key was
// remembered whole: the points, their order, and every start time.
func TestOTLPExportsAreWhatTheyWereWithKeysRememberedByTheirDigests(t *testing.T) {
	runs := alloctest.UnlessRaced(120, 30)
	const rounds = 6
	collector := &model.Collector{Name: "app"}
	pendingAsItself, pendingDigested, startsAsItself, startsDigested, ofADigestsLength, exports := 0, 0, 0, 0, 0, 0
	for seed := range runs {
		run := newKeyRun(uint64(1000+seed), true, true)
		for i := range run.resources[1:] {
			if run.random.IntN(2) == 0 {
				run.resources[1+i].Attributes["k8s.pod.name"] = strings.Repeat("pod-", 40+run.random.IntN(30))
			}
		}
		server, head := newOTLPKeyServer(t, "svc", nil), headOTLPKeys()
		server.manager.Get().OTLP.ProbeAttributes = true
		probe := server.manager.Get().OTLP
		var targets []string
		for range 3 + run.random.IntN(4) {
			n := run.random.IntN(300)
			if run.random.IntN(6) == 0 {
				n = 8 << 10
			}
			targets = append(targets, keyLengthTarget(run, n))
		}
		queue := func(at int64) {
			for i, target := range targets {
				set := run.scrape(at + int64(10*i))
				server.queueProbeOTLP(set, collector, target, time.Time{})
				head.queue(defaultResourceIdentity(probe), withTargetLabels(set, map[string]string{probeCollectorAttribute: collector.Name, probeTargetAttribute: target}))
			}
			for i, identity := range run.resources[1:] {
				if run.random.IntN(2) == 0 {
					set := run.scrape(at + int64(100*i))
					server.queueOTLPResource(set, identity, scrapeTime{})
					head.queue(identity, set)
				}
			}
			server.otlpMu.Lock()
			for _, batch := range server.otlpPending {
				for key, pending := range batch.metrics {
					if otlpKeyDigested(key) {
						pendingDigested++
						ofADigestsLength += btoi(len(otlpMetricKey(pending.metric)) == sha256.Size)
					} else {
						pendingAsItself++
					}
				}
			}
			server.otlpMu.Unlock()
		}
		for round := range rounds {
			at := int64(1000 * (round + 1))
			queue(at)
			drained, headDrained := server.drainOTLP(), head.drain()
			if len(drained) == 0 {
				continue
			}
			if round < rounds-1 && run.random.IntN(4) == 0 {
				queue(at + 500)
				server.requeueOTLP(drained)
				head.requeue(headDrained)
				continue
			}
			exports++
			if sent, was := server.push(t, drained), head.export(t, headDrained); !bytes.Equal(sent, was) {
				t.Fatalf("run %d, export %d is sent as\n%q\nand with every key remembered whole it was\n%q", seed, round, sent, was)
			}
		}
		server.otlpStarts.mu.Lock()
		for key := range server.otlpStarts.series {
			if otlpKeyDigested(key) {
				startsDigested++
			} else {
				startsAsItself++
			}
		}
		server.otlpStarts.mu.Unlock()
	}
	t.Logf("%d runs, %d exports: points waiting under their keys %d times and under digests %d (%d of a key a digest's length); start times under their keys %d, under digests %d", runs, exports, pendingAsItself, pendingDigested, ofADigestsLength, startsAsItself, startsDigested)
	if exports < runs || pendingAsItself == 0 || pendingDigested == 0 || startsAsItself == 0 || startsDigested == 0 {
		t.Errorf("the runs did not have keys of both kinds where points wait and where start times are kept")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// What is remembered of a series is bounded however long its key is: a
// start time and a point waiting, with probe attributes, for a probe's
// target of 8 KiB, keep about what they keep for a short one, where they
// kept the target's 8 KiB, and the start time another copy of it: at the
// bound of start times, twice otlp.max_pending_points, 1.6 GiB of them for
// an hour. A probe's points share one copy of its target, and none of them
// keeps the request's line, of which the target may be a part.
func TestWhatIsRememberedOfASeriesIsBoundedWhateverTheTarget(t *testing.T) {
	const probes = 400
	held := func(pad string) (start, pending float64) {
		server := newOTLPKeyServer(t, "svc", nil)
		server.manager.Get().OTLP.ProbeAttributes = true
		c := &model.Collector{Name: "app"}
		measure := func(do func(i int)) float64 {
			// Twice, so that what an earlier test left to be finalized is
			// gone before the count starts.
			runtime.GC()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := range probes {
				do(i)
			}
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&after)
			return float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)) / probes
		}
		// Each probe's target is part of a request line of 16 KiB, as
		// the query's target is.
		target := func(i int) string {
			line := "GET /probe?collector=app&target=http://203.0.113.1/" + strconv.Itoa(i) + pad + "&junk=" + strings.Repeat("j", 16<<10) + " HTTP/1.1"
			from := strings.Index(line, "http://")
			return line[from:strings.Index(line, "&junk")]
		}
		pending = measure(func(i int) {
			set := model.MetricSet{Metrics: []model.Metric{{Name: "app_requests_total", Type: model.CounterMetricType, Value: 1}}}
			server.queueProbeOTLP(set, c, target(i), time.Time{})
		})
		drained := server.drainOTLP()
		of := server.otlpStarts.forResource(drained[0].Identity.key())
		start = measure(func(i int) {
			_ = otlpMetrics(model.MetricSet{Metrics: drained[0].Set.Metrics[i : i+1]}, "1", of)
		})
		runtime.KeepAlive(drained)
		return start, pending
	}
	shortStart, shortPending := held("")
	longStart, longPending := held(strings.Repeat("a", 8000))
	t.Logf("per series: a start time %.0f bytes for a short target, %.0f for one of 8 KiB; a point waiting %.0f and %.0f", shortStart, longStart, shortPending, longPending)
	if longStart > shortStart+512 {
		t.Errorf("a start time keeps %.0f bytes for a target of 8 KiB, %.0f for a short one", longStart, shortStart)
	}
	// The point keeps its target, once, and nothing of the line.
	if longPending > shortPending+8000+2048 {
		t.Errorf("a point waiting keeps %.0f bytes for a target of 8 KiB, %.0f for a short one", longPending, shortPending)
	}
	if shortPending > 4096 {
		t.Errorf("a point waiting keeps %.0f bytes for a short target that is part of a request line of 16 KiB", shortPending)
	}
}

// headOTLPKey is otlpKey as it was before a key was written where its
// caller has room: made in a strings.Builder of its size.
func headOTLPKey(before string, pairs map[string]string, head ...string) string {
	var few [2 + 2*8]string
	parts := append(few[:0], head...)
	for name, value := range pairs {
		parts = append(parts, name, value)
		for at := len(parts) - 2; at > len(head) && parts[at] < parts[at-2]; at -= 2 {
			parts[at], parts[at-2] = parts[at-2], parts[at]
			parts[at+1], parts[at-1] = parts[at-1], parts[at+1]
		}
	}
	size := len(before) + len(otlpKeyNUL)*(len(head)-1+len(pairs)) + len(pairs) + len(otlpKeyEnd)
	nuls := 0
	for _, part := range parts {
		nuls += strings.Count(part, "\x00")
		size += len(part) + 1
		for more := len(part) >> 7; more > 0; more >>= 7 {
			size++
		}
	}
	var b strings.Builder
	b.Grow(size + nuls)
	b.WriteString(before)
	for i, part := range parts {
		switch {
		case i == 0:
		case i >= len(head) && (i-len(head))%2 == 1:
			b.WriteByte('=')
		default:
			b.WriteString(otlpKeyNUL)
		}
		if nuls > 0 {
			for nul := strings.IndexByte(part, 0); nul >= 0; nul = strings.IndexByte(part, 0) {
				b.WriteString(part[:nul])
				b.WriteString(otlpKeyNUL)
				part = part[nul+1:]
			}
		}
		b.WriteString(part)
	}
	b.WriteString(otlpKeyEnd)
	var length [binary.MaxVarintLen64]byte
	for _, part := range parts {
		b.Write(binary.AppendUvarint(length[:0], uint64(len(part))))
	}
	return b.String()
}

// A key is the bytes it was, written where its caller has room: over
// generated resources and series, those made to be joined alike, with a
// part before them as a start time's key has, and series whose keys are of
// every length from under the room to past it, of more pairs than are put
// in order in place, and written after bytes already in the room.
func TestAnOTLPKeyIsWhatItWas(t *testing.T) {
	subjects := keySubjects(alloctest.UnlessRaced(20000, 2000))
	for n := range 400 {
		pairs := map[string]string{"a": strings.Repeat("\x00", n%7) + strings.Repeat("v", n)}
		if n%5 == 0 {
			for i := range 12 {
				pairs["label_"+strconv.Itoa(i)] = strconv.Itoa(n)
			}
		}
		subjects = append(subjects, keySubject{head: []string{"m", "counter"}, pairs: pairs})
	}
	for i, s := range subjects {
		before := ""
		if i%2 == 1 {
			before = string(appendKeyPart(nil, s.String()))
		}
		if got, was := otlpKey(before, s.pairs, s.head...), headOTLPKey(before, s.pairs, s.head...); got != was {
			t.Fatalf("%s after %q has the key\n%q\nwhere it had\n%q", s, before, got, was)
		}
		if got, was := appendOTLPKey([]byte("x"), before, s.pairs, s.head...), "x"+headOTLPKey(before, s.pairs, s.head...); string(got) != was {
			t.Fatalf("%s after %q is appended as\n%q\nwhere it is\n%q", s, before, got, was)
		}
	}
}
