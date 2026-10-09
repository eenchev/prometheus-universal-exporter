package exporter

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// subjectBytesWas is subjectBytes as it was while a key held its subject's
// parts themselves rather than their digest: the kind, each part after its
// length and a NUL, and the aspect.
func subjectBytesWas(kind byte, of failureAspect, parts ...string) string {
	var b strings.Builder
	b.WriteByte(kind)
	for _, part := range parts {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(0)
		b.WriteString(part)
	}
	b.WriteString(string(of))
	return b.String()
}

// The digest of a subject is that of its encoding however long the subject
// is: over 3,000 generated subjects of one to three parts, each up to 2,000
// bytes, around the length past which the encoding is hashed as it is
// written (streamSubjectDigest), the key is the digest of the encoding the
// key once was, before the aspect, and is as long whatever the parts are.
// Under the race detector it is 600 subjects.
func TestTheKeyOfASubjectIsTheDigestOfItsEncodingHoweverLong(t *testing.T) {
	random := rand.New(rand.NewPCG(53, 4))
	streamed := 0
	for range alloctest.UnlessRaced(3000, 600) {
		parts := make([]string, 1+random.IntN(3))
		for i := range parts {
			length := random.IntN(600)
			if random.IntN(3) == 0 {
				length = random.IntN(2000)
			}
			parts[i] = strings.Repeat(string(rune('a'+random.IntN(26))), length)
		}
		of := []failureAspect{"", staleAspect, utf8Aspect}[random.IntN(3)]
		encoding := subjectBytesWas(probeSubject, "", parts...)
		want := sha256.Sum256([]byte(encoding))
		key := subjectBytes(probeSubject, of, parts...)
		if key != string(want[:])+string(of) {
			t.Fatalf("the key of the parts of %d bytes is %x, want the digest of their encoding %x and the aspect %q", len(encoding), key, want, of)
		}
		if len(encoding) > 512 {
			streamed++
		}
	}
	if streamed < alloctest.UnlessRaced(3000, 600)/10 {
		t.Fatalf("only %d of the subjects have an encoding hashed as it is written", streamed)
	}
}

// A probe of a caller's target of 8 KiB that fails costs the failure log
// what one of a short target does: making its key and remembering its
// failure allocate under 1.5 KiB, the key is the digest, 32 bytes, and the
// entry holds no byte of the target. While the key held the target, making
// it alone was the 8 KiB of the target, kept for an hour.
func TestRememberingTheFailureOfAProbeOfALongTargetCostsLittle(t *testing.T) {
	f := newFailureLog()
	logger := slog.New(slog.DiscardHandler)
	const probeKey = "801c9767d942bc4f7daa0e21fc42bf212a61e040d8692d2fc6d86384e8c69def"
	pad := strings.Repeat("a", 8000)
	targets := make([]string, 6*(20+1))
	for i := range targets {
		targets[i] = "http://target.example/" + strconv.Itoa(i) + pad
	}
	next := 0
	failure := errors.New("received HTTP status 500")
	remember := func() {
		f.failed(logger, slog.LevelWarn, probeFailureKey("app", targets[next], probeKey), "probe failed", "http_status", failure)
		next++
	}
	if allocated := alloctest.BytesAtMost(20, 1536, remember); allocated > 1536 {
		t.Errorf("remembering the failure of a probe of an 8 KiB target allocates %d bytes, want at most 1536", allocated)
	}
	if len(f.entries) != next {
		t.Fatalf("%d failures are remembered of %d probes", len(f.entries), next)
	}
	for key, st := range f.entries {
		if len(key) != subjectDigestBytes || st.key.bytes != key || strings.Contains(st.key.collector+st.key.target+st.trip+st.stage+st.err, "aaaa") {
			t.Fatalf("an entry is under a key of %d bytes and holds %+v", len(key), st)
		}
	}
}

// The failure log writes what it wrote while its keys held their subjects'
// parts: over 20,000 failures, recoveries and forgotten collectors of
// probes, static targets and what trips found — short targets, targets past
// the length whose encoding is hashed as it is written, the same target
// probed with another key, aspects of each — minutes and hours apart, the
// log writes line for line what the same log given the keys of then
// (subjectBytesWas) writes, with the same repeats, the same sums of them
// and the same number of failures remembered after every step. Under the
// race detector it is 5,000 of them.
func TestTheFailureLogWritesWhatItDidWhileItsKeysHeldTheParts(t *testing.T) {
	random := rand.New(rand.NewPCG(53, 14))
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	logs := func() (*failureLog, *bytes.Buffer, *slog.Logger) {
		f, out := newFailureLog(), &bytes.Buffer{}
		f.now = func() time.Time { return now }
		return f, out, slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
	}
	is, isOut, isLogger := logs()
	was, wasOut, wasLogger := logs()
	type subject struct {
		is, was subjectKey
		target  string
	}
	long := "http://long.example/" + strings.Repeat("x", 700)
	var subjects []subject
	for _, collector := range []string{"web", "api"} {
		for _, target := range []string{"http://a", "http://b", long, long + "y"} {
			for _, probe := range []string{"", "k1", "k2"} {
				for _, of := range []failureAspect{"", staleAspect} {
					subjects = append(subjects, subject{
						subjectKey{bytes: subjectBytes(probeSubject, of, collector, target, probe), collector: collector},
						subjectKey{bytes: subjectBytesWas(probeSubject, of, collector, target, probe), collector: collector}, target})
				}
			}
			for _, of := range []failureAspect{"", utf8Aspect, sampleLinesAspect} {
				subjects = append(subjects, subject{aspectKey(collector, target, "", of), subjectKey{bytes: subjectBytesWas(addressSubject, of, collector, target, ""), collector: collector}, target})
			}
		}
		subjects = append(subjects, subject{staticTargetKey(collector, "store"), subjectKey{bytes: subjectBytesWas(staticSubject, "", collector, "store"), collector: collector, target: "store", static: true}, "store"})
	}
	failures := []error{errors.New("connection refused"), errors.New("received HTTP status 500"), errors.New("received HTTP status 503")}
	stages := []string{"http", "transform"}
	rounds := alloctest.UnlessRaced(20000, 5000)
	for step := range rounds {
		s := subjects[random.IntN(len(subjects))]
		switch roll := random.IntN(40); {
		case roll < 2:
			is.recovered(isLogger, s.is, "probe recovered", "collector", s.is.collector, "target", s.target)
			was.recovered(wasLogger, s.was, "probe recovered", "collector", s.was.collector, "target", s.target)
		case roll == 2 && random.IntN(10) == 0:
			gone := map[string]bool{s.is.collector: true}
			is.forgetCollectors(gone)
			was.forgetCollectors(gone)
		default:
			err, stage := failures[random.IntN(len(failures))], stages[random.IntN(len(stages))]
			for range 1 + random.IntN(4) {
				is.failed(isLogger, slog.LevelWarn, s.is, "probe failed", stage, err, "collector", s.is.collector, "target", s.target)
				was.failed(wasLogger, slog.LevelWarn, s.was, "probe failed", stage, err, "collector", s.was.collector, "target", s.target)
				now = now.Add(time.Duration(random.IntN(150)) * time.Second)
			}
		}
		if random.IntN(400) == 0 {
			now = now.Add(2 * time.Hour)
		}
		if len(is.entries) != len(was.entries) {
			t.Fatalf("step %d: %d failures are remembered, and were %d", step, len(is.entries), len(was.entries))
		}
	}
	if isOut.String() != wasOut.String() {
		isLines, wasLines := bytes.Split(isOut.Bytes(), []byte("\n")), bytes.Split(wasOut.Bytes(), []byte("\n"))
		for i := range min(len(isLines), len(wasLines)) {
			if !bytes.Equal(isLines[i], wasLines[i]) {
				t.Fatalf("line %d is\n%s\nand was\n%s", i+1, isLines[i], wasLines[i])
			}
		}
		t.Fatalf("the log has %d lines, and had %d", len(isLines), len(wasLines))
	}
	out := isOut.Bytes()
	if lines, repeats, sums, recoveries := bytes.Count(out, []byte("\n")), bytes.Count(out, []byte(`"repeat":true`)), bytes.Count(out, []byte(`"repeated":`)), bytes.Count(out, []byte(`"failed_for":`)); lines < rounds || repeats < rounds/10 || sums < rounds/100 || recoveries < rounds/100 {
		t.Fatalf("%d lines, %d repeats, %d sums of repeats and %d recoveries: the generator shows too little", lines, repeats, sums, recoveries)
	}
}
