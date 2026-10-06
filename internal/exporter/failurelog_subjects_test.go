package exporter

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// logSubject is one thing the failure log tells from every other, as the
// tests name it: of a kind, with the parts of that kind — a probe's
// collector, target and key; a static target's collector and name; the
// collector, address and file of what a trip found; the target and metric
// the static targets endpoint left out — and what of it failed: itself, an
// aspect of it, or a rule of it, told by its name, expression and items.
type logSubject struct {
	kind                      byte
	parts                     [3]string
	aspect                    failureAspect
	rule                      bool
	metric, expression, items string
}

// The aspects a key may end with.
var logAspects = []failureAspect{staleAspect, scheduleAspect, utf8Aspect, listingAspect, skippedAspect, carbonLinesAspect, sampleLinesAspect}

// partsOf is how many parts a subject of each kind has.
var partsOf = map[byte]int{probeSubject: 3, staticSubject: 2, addressSubject: 3, endpointSubject: 2}

// trip is the key of the subject itself, as the exporter makes it.
func (s logSubject) trip() subjectKey {
	switch s.kind {
	case probeSubject:
		return probeFailureKey(s.parts[0], s.parts[1], s.parts[2])
	case staticSubject:
		return staticTargetKey(s.parts[0], s.parts[1])
	case endpointSubject:
		return staticClashKey(s.parts[0], s.parts[1])
	}
	return failureKey(s.parts[0], s.parts[1], s.parts[2])
}

// key is the key of what of the subject failed, as the exporter makes it.
func (s logSubject) key() subjectKey {
	switch {
	case s.rule:
		trip := s.trip()
		return trip.rule(ruleFailureKey(trip.bytes, s.metric, s.expression, s.items))
	case s.aspect == "":
		return s.trip()
	case s.kind == addressSubject:
		return aspectKey(s.parts[0], s.parts[1], s.parts[2], s.aspect)
	}
	return s.trip().aspect(s.aspect)
}

// made says whether the exporter makes a key of this form: a probe's own,
// its stale answers and its rules; a static target's own, its schedule and
// its rules; a file, its rules, and the aspects of a response, a file and a
// directory; and the endpoint's.
func (s logSubject) made() bool {
	file := s.parts[2] != ""
	switch s.kind {
	case probeSubject:
		return s.rule || s.aspect == "" || s.aspect == staleAspect
	case staticSubject:
		return s.rule || s.aspect == "" || s.aspect == scheduleAspect
	case addressSubject:
		switch s.aspect {
		case "":
			return file
		case utf8Aspect, carbonLinesAspect, sampleLinesAspect:
			return !s.rule
		case listingAspect, skippedAspect:
			return !s.rule && !file
		}
		return false
	}
	return !s.rule && s.aspect == ""
}

// keyWas is the key a subject the exporter makes a key of had while a key
// was its parts with a NUL between them: copied from where each was made,
// the collector first, then what stood for the target — a probe's target, a
// NUL and its key; the words in staticTargetWas and a static target's name;
// the address — then the file or what stood in its place, and what was
// appended to that.
func (s logSubject) keyWas() string {
	collector, target, last := s.parts[0], s.parts[1], s.parts[2]
	var trip string
	switch s.kind {
	case probeSubject:
		trip = failureKeyWas(collector, target+"\x00"+last, "")
		if s.aspect == staleAspect {
			return failureKeyWas(collector, target+"\x00"+last, "\x00stale")
		}
	case staticSubject:
		trip = failureKeyWas(collector, staticTargetWas+target, "")
		if s.aspect == scheduleAspect {
			return failureKeyWas(collector, staticTargetWas+target, "schedule")
		}
	case endpointSubject:
		return failureKeyWas("", staticTargetWas+collector, "family "+target)
	default:
		trip = failureKeyWas(collector, target, last)
		switch s.aspect {
		case utf8Aspect:
			if last != "" {
				target += "\x00" + last
			}
			return failureKeyWas(collector, target, "\x00utf8")
		case listingAspect, skippedAspect:
			return failureKeyWas(collector, target, string(s.aspect))
		case carbonLinesAspect, sampleLinesAspect:
			return trip + string(s.aspect)
		}
	}
	if s.rule {
		return ruleFailureKeyWas(trip, s.metric, s.expression, s.items)
	}
	return trip
}

// plain says that the subject is none of those whose key could be another's,
// or be read as another's, while a key was its parts with a NUL between
// them: none of its parts holds a NUL, its collector is named, and its
// target or address does not begin with the words a static target's own
// failures were told by.
func (s logSubject) plain() bool {
	for _, part := range []string{s.parts[0], s.parts[1], s.parts[2], s.metric, s.expression, s.items} {
		if strings.Contains(part, "\x00") {
			return false
		}
	}
	if s.kind == endpointSubject {
		return true
	}
	return s.parts[0] != "" && (s.kind == staticSubject || !strings.HasPrefix(s.parts[1], staticTargetWas))
}

// keyPartRead reads a part off the beginning of the bytes of a key: the
// digits of its length, written as a length is, a NUL, and that many bytes.
func keyPartRead(key string) (part, rest string, ok bool) {
	digits, after, found := strings.Cut(key, "\x00")
	length, err := strconv.Atoi(digits)
	if !found || err != nil || length < 0 || length > len(after) || strconv.Itoa(length) != digits {
		return "", "", false
	}
	return after[:length], after[length:], true
}

// logSubjectOf reads the subject back out of the bytes of a key, which the
// exporter never does: that every key reads as the subject it was made of,
// and as no other, is what shows that no two subjects have one key.
func logSubjectOf(key string) (logSubject, bool) {
	var s logSubject
	if key == "" || partsOf[key[0]] == 0 {
		return s, false
	}
	s.kind = key[0]
	rest := key[1:]
	for i := range partsOf[s.kind] {
		var ok bool
		if s.parts[i], rest, ok = keyPartRead(rest); !ok {
			return s, false
		}
	}
	if of, rule := strings.CutPrefix(rest, ruleKeyMarker); rule {
		var named, written bool
		s.rule = true
		s.metric, of, named = keyPartRead(of)
		s.expression, s.items, written = keyPartRead(of)
		return s, named && written
	}
	if rest == "" {
		return s, true
	}
	for _, aspect := range logAspects {
		if rest == string(aspect) {
			s.aspect = aspect
			return s, true
		}
	}
	return s, false
}

// logSubjects makes subjects whose parts are joined of pieces that the keys
// were once read at, or could be read at: NULs, the marker of a rule's key
// and its pieces, the aspects and their names, the words a static target's
// own failures were told by, the letters of the kinds, digits as a length
// is written, and plain names. The kinds, and the aspects of each, are
// drawn as often as each other, and every so often a subject is the last
// one with one part, or what of it failed, drawn again, or the subject that
// had the last one's key (twin).
type logSubjects struct {
	random *rand.Rand
	last   logSubject
}

var logSubjectPieces = []string{
	"", "", "a", "web", "http://a", "/var/metrics", "a.prom", "m", ".a", ".items[]", " #",
	"\x00", "\x00\x00", "0", "1", "3", "12", "0\x00", "3\x00", "12\x00",
	"rule", "\x00rule", "rule\x00", "\x00rule\x00", "\x00rul", "ule\x00",
	"stale", "\x00stale", "schedule", "\x00schedule", "utf8", "\x00utf8", "listing", "\x00listing", "skipped", "\x00skipped",
	"carbon lines", "\x00carbon lines", "sample lines", "\x00sample lines", "family ",
	staticTargetWas, staticTargetWas + "a", "store", "p", "s", "e", "é",
}

func (g *logSubjects) part() string {
	var b strings.Builder
	for range g.random.IntN(4) {
		b.WriteString(logSubjectPieces[g.random.IntN(len(logSubjectPieces))])
	}
	return b.String()
}

// twin is the subject that had the key of s while a key was its parts with a
// NUL between them, for the subjects that have a simple one: what a trip
// found at an address or in a file whose name holds what was appended to the
// key of s, or stood in it.
func (s logSubject) twin() (logSubject, bool) {
	collector, target, last := s.parts[0], s.parts[1], s.parts[2]
	found := func(target, file string, aspect failureAspect) (logSubject, bool) {
		return logSubject{kind: addressSubject, parts: [3]string{collector, target, file}, aspect: aspect}, true
	}
	switch {
	case s.rule:
	case s.kind == probeSubject && s.aspect == "":
		return found(target, last+"\x00", "")
	case s.kind == probeSubject && s.aspect == staleAspect:
		return found(target+"\x00"+last+"\x00", "stale", "")
	case s.kind == staticSubject && s.aspect == scheduleAspect:
		return found(staticTargetWas+target, "schedule", "")
	case s.kind == endpointSubject && s.aspect == "":
		return logSubject{kind: addressSubject, parts: [3]string{"", staticTargetWas + collector, "family " + target}}, true
	case s.kind == addressSubject && s.aspect == utf8Aspect && last != "":
		return found(target+"\x00"+last, "", utf8Aspect)
	case s.kind == addressSubject && (s.aspect == listingAspect || s.aspect == skippedAspect) && last == "":
		return found(target, string(s.aspect), "")
	}
	return s, false
}

func (g *logSubjects) next() logSubject {
	if twin, has := g.last.twin(); has && g.random.IntN(4) == 0 {
		g.last = twin
		return twin
	}
	s := g.last
	if g.last.kind == 0 || g.random.IntN(3) == 0 {
		s = logSubject{kind: []byte{probeSubject, staticSubject, addressSubject, endpointSubject}[g.random.IntN(4)]}
		for i := range partsOf[s.kind] {
			s.parts[i] = g.part()
		}
	} else if i := g.random.IntN(4); i < partsOf[s.kind] {
		s.parts[i] = g.part()
	}
	s.aspect, s.rule, s.metric, s.expression, s.items = "", false, "", "", ""
	switch of := g.random.IntN(len(logAspects) + 4); {
	case of < len(logAspects):
		s.aspect = logAspects[of]
	case of < len(logAspects)+3:
		s.rule, s.metric, s.expression, s.items = true, g.part(), g.part(), g.part()
	}
	g.last = s
	return s
}

// No two things the failure log tells apart have one key, whatever their
// parts hold. Over 300,000 generated subjects of every kind, whose
// collectors, targets, addresses, files, static target names, probe keys,
// metric names, expressions and items are joined of NULs, the marker of a
// rule's key and its pieces, the aspects, the words a static target's
// failures were told by and lengths as a key writes them:
//
//   - the bytes of each key read back as the subject the key was made of
//     (logSubjectOf), so two subjects that differ have two keys, and among
//     the generated ones no key is that of two;
//   - each key says whose it is: its collector, none for the endpoint's,
//     and the static target, by name, for a static target's own and for no
//     other;
//   - a key of what a trip found, made with its aspect at once, is the key
//     the aspect gives made after it; a key is made in one allocation, and
//     a probe that has its key makes none for its trip;
//   - of the subjects the exporter makes keys of, two that are plain —
//     without a NUL in any part, and with a target that does not begin as a
//     static target's own failures were told — had one key while a key was
//     its parts with a NUL between them (keyWas) only when they were one
//     subject, as now; and thousands of the others had the key of another
//     subject then.
//
// The pairs named first are the ones reported: each had one key and has two.
// Under the race detector it is 60,000 subjects, and the allocations, which
// the detector changes, are not counted.
func TestNoTwoSubjectsHaveOneKey(t *testing.T) {
	const hex = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	for _, pair := range []struct {
		what string
		a, b logSubject
	}{
		{
			"a file named schedule in a directory named as a static target, and that target's skipped turns",
			logSubject{kind: addressSubject, parts: [3]string{"dir", staticTargetWas + "n", "schedule"}},
			logSubject{kind: staticSubject, parts: [3]string{"dir", "n"}, aspect: scheduleAspect},
		},
		{
			"a probe of a target with NULs and a rule of the probe of another target",
			logSubject{kind: probeSubject, parts: [3]string{"c", "http://t\x00\x00\x00rule\x00m\x009\x00.value #"}},
			logSubject{kind: probeSubject, parts: [3]string{"c", "http://t"}, rule: true, metric: "m", expression: ".value #\x00"},
		},
		{
			"the rules of the probes of two targets, one with NULs",
			logSubject{kind: probeSubject, parts: [3]string{"c", "t"}, rule: true, metric: "m", expression: "ab", items: "\x00\x00rule\x00m\x001\x00c\x00d"},
			logSubject{kind: probeSubject, parts: [3]string{"c", "t\x00\x00\x00rule\x00m\x002\x00ab"}, rule: true, metric: "m", expression: "c", items: "d"},
		},
		{
			"a probe answered stale and a file named stale",
			logSubject{kind: probeSubject, parts: [3]string{"c", "t"}, aspect: staleAspect},
			logSubject{kind: addressSubject, parts: [3]string{"c", "t\x00\x00", "stale"}},
		},
		{
			"a directory listed short and a file whose name is a NUL and listing",
			logSubject{kind: addressSubject, parts: [3]string{"c", "t"}, aspect: listingAspect},
			logSubject{kind: addressSubject, parts: [3]string{"c", "t", "\x00listing"}},
		},
		{
			"the bytes of a file and those of a response at an address with a NUL",
			logSubject{kind: addressSubject, parts: [3]string{"c", "t", "f"}, aspect: utf8Aspect},
			logSubject{kind: addressSubject, parts: [3]string{"c", "t\x00f"}, aspect: utf8Aspect},
		},
		{
			"a probe with a key and a file whose name is the key and a NUL",
			logSubject{kind: probeSubject, parts: [3]string{"c", "t", hex}},
			logSubject{kind: addressSubject, parts: [3]string{"c", "t", hex + "\x00"}},
		},
		{
			"the endpoint's metric of a target and a file of a directory named as the target, read by a collector without a name",
			logSubject{kind: endpointSubject, parts: [3]string{"n", "demo"}},
			logSubject{kind: addressSubject, parts: [3]string{"", staticTargetWas + "n", "family demo"}},
		},
	} {
		if !pair.a.made() || !pair.b.made() || pair.a.keyWas() != pair.b.keyWas() {
			t.Errorf("%s: the two had the keys %q and %q, want one key, of two subjects the exporter makes keys of", pair.what, pair.a.keyWas(), pair.b.keyWas())
		}
		if pair.a.key().bytes == pair.b.key().bytes {
			t.Errorf("%s: the two have the one key %q", pair.what, pair.a.key().bytes)
		}
		if pair.a.plain() && pair.b.plain() {
			t.Errorf("%s: both are taken for plain subjects", pair.what)
		}
	}

	generator := &logSubjects{random: rand.New(rand.NewPCG(29, 1))}
	keys, wasKeys := map[string]logSubject{}, map[string]logSubject{}
	kinds, forms := map[byte]int{}, map[string]int{}
	var plain, shared, nuls int
	for range alloctest.UnlessRaced(300000, 60000) {
		s := generator.next()
		key := s.key()
		if read, ok := logSubjectOf(key.bytes); !ok || read != s {
			t.Fatalf("the key %q of %+v reads as %+v (a key: %v)", key.bytes, s, read, ok)
		}
		if other, seen := keys[key.bytes]; seen && other != s {
			t.Fatalf("%+v and %+v have the one key %q", other, s, key.bytes)
		}
		keys[key.bytes] = s
		whose := subjectKey{bytes: key.bytes, collector: s.parts[0]}
		switch s.kind {
		case staticSubject:
			whose.target, whose.static = s.parts[1], true
		case endpointSubject:
			whose.collector = ""
		}
		if key != whose {
			t.Fatalf("the key of %+v is said to be of the collector %q, and of the static target %q: %v; want %q, and %q: %v", s, key.collector, key.target, key.static, whose.collector, whose.target, whose.static)
		}
		if s.kind == addressSubject && s.aspect != "" {
			if later := failureKey(s.parts[0], s.parts[1], s.parts[2]).aspect(s.aspect); later != key {
				t.Fatalf("the key of %+v made with its aspect is %q, and with the aspect after it %q", s, key.bytes, later.bytes)
			}
		}
		kinds[s.kind]++
		forms[string(s.aspect)+strconv.FormatBool(s.rule)]++
		if strings.Contains(strings.Join(s.parts[:], ""), "\x00") {
			nuls++
		}
		if !s.made() {
			continue
		}
		wasKey := s.keyWas()
		if other, seen := wasKeys[wasKey]; seen && other != s {
			if other.plain() && s.plain() {
				t.Fatalf("the plain subjects %+v and %+v had the one key %q, and have two", other, s, wasKey)
			}
			shared++
		}
		wasKeys[wasKey] = s
		if s.plain() {
			plain++
		}
	}
	for _, kind := range []byte{probeSubject, staticSubject, addressSubject, endpointSubject} {
		if kinds[kind] < len(keys)/8 {
			t.Errorf("only %d of the %d subjects are of the kind %c", kinds[kind], len(keys), kind)
		}
	}
	if len(forms) != len(logAspects)+2 {
		t.Errorf("the subjects have %d forms, want each aspect, a rule and the subject's own: %v", len(forms), forms)
	}
	for form, made := range forms {
		if made < len(keys)/40 {
			t.Errorf("only %d of the %d subjects are of the form %q", made, len(keys), form)
		}
	}
	if floor := alloctest.UnlessRaced(300000, 60000); len(keys) < floor/2 || plain < floor/100 || shared < floor/100 || nuls < floor/4 {
		t.Errorf("%d subjects that differ, %d plain ones the exporter makes keys of, %d that had the key of another and %d with a NUL in a part: the generator shows too little", len(keys), plain, shared, nuls)
	}
	if alloctest.RaceDetector {
		// The race detector changes what is allocated.
		return
	}
	long := strings.Repeat("http://a.example/", 20)
	probe := upstreamProbe{failure: probeFailureKey("web", long, hex)}
	var kept subjectKey
	for _, key := range []struct {
		what string
		most float64
		make func()
	}{
		{"a probe", 1, func() { kept = probeFailureKey("web", long, hex) }},
		{"a static target", 1, func() { kept = staticTargetKey("web", "store") }},
		{"what a trip found", 1, func() { kept = failureKey("web", long, "a.prom") }},
		{"an aspect of what a trip found", 1, func() { kept = aspectKey("web", long, "", utf8Aspect) }},
		{"the endpoint's metric", 1, func() { kept = staticClashKey("store", "demo_value") }},
		{"a trip of a probe that has its key", 0, func() { kept = probe.failureKey() }},
	} {
		if allocs := alloctest.AllocsAtMost(100, key.most, key.make); allocs > key.most {
			t.Errorf("the key of %s is made in %v allocations, want %v", key.what, allocs, key.most)
		}
	}
	if kept != probe.failure {
		t.Errorf("a probe that has its key gives the key %q for its trip, want its own %q", kept.bytes, probe.failure.bytes)
	}
}

// keyCollectorWas is the collector of a key as it was read out of one: what
// stands before its first NUL.
func keyCollectorWas(key string) string {
	collector, _, _ := strings.Cut(key, "\x00")
	return collector
}

// forgetCollectorsReadingKeys is forgetCollectorsLocked as it was while the
// collector of an entry was read out of its key, for a log whose entries
// are under the keys they had then.
func forgetCollectorsReadingKeys(f *failureLog, names map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.entries {
		if names[keyCollectorWas(key)] {
			f.dropLocked(key)
		}
	}
}

// forgetStaticTargetsReadingKeys is forgetStaticTargetsLocked as it was
// while the static target of an entry was read out of its key: what follows
// the words in staticTargetWas in the key's second part, of a key that has a
// collector.
func forgetStaticTargetsReadingKeys(f *failureLog, names map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.entries {
		collector, rest, _ := strings.Cut(key, "\x00")
		target, _, _ := strings.Cut(rest, "\x00")
		if name, static := strings.CutPrefix(target, staticTargetWas); static && collector != "" && names[name] {
			f.dropLocked(key)
		}
	}
}

// reported is the failure of a subject as a scrape reports it to the
// failure log.
func (s logSubject) reported() reportedFailure {
	if s.rule {
		trip := s.trip()
		return ruleFailureOf(trip, ruleFailureKey(trip.bytes, s.metric, s.expression, s.items), s.metric, s.expression, s.items)
	}
	return failureOf(s.key())
}

// reportedWas is the failure of a subject as a scrape reported it while a
// key was its parts with a NUL between them and the collector was read out
// of it: under the key it had, a rule's as one of the trip its scrape named,
// as it has been since the trip was no longer read out of the rule's key.
func (s logSubject) reportedWas() reportedFailure {
	wasKey := s.keyWas()
	whose := subjectKey{collector: keyCollectorWas(wasKey)}
	if s.rule {
		trip := s
		trip.rule, trip.metric, trip.expression, trip.items = false, "", "", ""
		return reportedFailure{rule: true, keyed: true, trip: whose.rule(trip.keyWas()), key: wasKey}
	}
	return failureOf(whose.rule(wasKey))
}

// A reload forgets exactly what is a removed or changed collector's, and
// exactly what is a removed or changed static target's own, whatever the
// parts of the keys hold. Over rounds of 4,000 generated subjects each,
// whose parts hold NULs, the marker, the aspects and the words a static
// target's failures were told by, all remembered as failing, rules as those
// of their trips:
//
//   - forgetting a collector drops the failures of the subjects of that
//     collector, and those of no other: not the endpoint's, which are of
//     none, nor those of a collector whose name the first one's begins or
//     that begins it;
//   - forgetting a static target drops its own failure, its skipped turns
//     and its rules', under every collector, and nothing else: not what the
//     endpoint remembers of the target, not the probe whose target reads as
//     the target's name, nor the file of a directory that does;
//   - the rule failures counted are, after each, those of the entries left.
//
// And for the subjects that are plain and that the exporter makes keys of,
// remembered on a log of their own under the keys they had, the forgetting
// that read the keys (forgetCollectorsReadingKeys,
// forgetStaticTargetsReadingKeys) left what is left now; given the others
// too, it dropped subjects that were not the collector's or the target's,
// hundreds of them. Under the race detector it is two of the fifteen
// rounds.
func TestForgettingDropsExactlyWhatIsTheCollectorsOrTheStaticTargets(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	failed := errors.New("connection refused")
	var dropped, kept, strayed, alike int
	for round := range uint64(alloctest.UnlessRaced(15, 2)) {
		generator := &logSubjects{random: rand.New(rand.NewPCG(round, 29))}
		subjects := map[string]logSubject{}
		var collectors, statics []string
		for len(subjects) < 4000 {
			s := generator.next()
			if s.kind != endpointSubject && s.parts[0] == "" {
				// A collector has a name; the endpoint's are of none.
				s.parts[0] = "c"
			}
			subjects[s.key().bytes] = s
			if s.kind != endpointSubject {
				collectors = append(collectors, s.parts[0])
			}
			if s.kind == staticSubject {
				statics = append(statics, s.parts[1])
			} else {
				// And a target named as one of the others reads.
				statics = append(statics, strings.TrimPrefix(s.parts[1], staticTargetWas))
			}
		}
		// now remembers every subject; plain the plain ones the exporter
		// makes keys of, and all those it makes keys of, under the keys
		// they had.
		now, plain, all := newFailureLog(), newFailureLog(), newFailureLog()
		wasKeys, plainSubjects := map[string]string{}, map[string]bool{}
		for key, s := range subjects {
			s.reported().failed(now, configRead{}, logger, "http", failed)
			if !s.made() {
				continue
			}
			s.reportedWas().failed(all, configRead{}, logger, "http", failed)
			if s.plain() {
				s.reportedWas().failed(plain, configRead{}, logger, "http", failed)
				wasKeys[key], plainSubjects[key] = s.keyWas(), true
			}
		}
		if len(now.entries) != len(subjects) || len(plain.entries) != len(wasKeys) {
			t.Fatalf("round %d: %d subjects are remembered under %d keys, and the %d plain ones under %d", round, len(subjects), len(now.entries), len(wasKeys), len(plain.entries))
		}
		check := func(what string, gone func(logSubject) bool) {
			t.Helper()
			counted := map[string]int{}
			for key, s := range subjects {
				_, remembered := now.entries[key]
				if remembered == gone(s) {
					t.Fatalf("round %d, after forgetting %s: %+v is remembered: %v", round, what, s, remembered)
				}
				if remembered {
					kept++
					if s.rule {
						counted[s.trip().bytes]++
					}
				} else {
					dropped++
				}
				if plainSubjects[key] {
					if _, was := plain.entries[wasKeys[key]]; was != remembered {
						t.Fatalf("round %d, after forgetting %s: the plain subject %+v is remembered: %v, and was: %v", round, what, s, remembered, was)
					}
					alike++
				}
				if s.made() && !s.plain() && remembered {
					if _, was := all.entries[s.keyWas()]; !was {
						strayed++
					}
				}
			}
			if !reflect.DeepEqual(now.ruleFailures, counted) {
				t.Fatalf("round %d, after forgetting %s: the rule failures counted are %v, and those of the entries left %v", round, what, now.ruleFailures, counted)
			}
		}
		gone := map[string]bool{}
		for step := range 12 {
			if step%2 == 0 {
				name := collectors[generator.random.IntN(len(collectors))]
				names := map[string]bool{name: true}
				now.forgetCollectors(names)
				forgetCollectorsReadingKeys(plain, names)
				forgetCollectorsReadingKeys(all, names)
				for key, s := range subjects {
					if s.kind != endpointSubject && s.parts[0] == name {
						gone[key] = true
					}
				}
				check(fmt.Sprintf("the collector %q", name), func(s logSubject) bool { return gone[s.key().bytes] })
				continue
			}
			name := statics[generator.random.IntN(len(statics))]
			names := map[string]bool{name: true}
			now.mu.Lock()
			now.forgetStaticTargetsLocked(names)
			now.mu.Unlock()
			forgetStaticTargetsReadingKeys(plain, names)
			forgetStaticTargetsReadingKeys(all, names)
			for key, s := range subjects {
				if s.kind == staticSubject && s.parts[1] == name {
					gone[key] = true
				}
			}
			check(fmt.Sprintf("the static target %q", name), func(s logSubject) bool { return gone[s.key().bytes] })
		}
	}
	if rounds := alloctest.UnlessRaced(15, 2); dropped < 2000*rounds || kept < 20000*rounds || alike < 300*rounds || strayed < 100*rounds {
		t.Errorf("%d times a subject was found dropped and %d times kept, %d times a plain one as it was, and %d times one kept that the forgetting that read the keys dropped: the generator shows too little", dropped, kept, alike, strayed)
	}
}

// The failure log writes, for subjects that had keys of their own, what it
// wrote while a key was its parts with a NUL between them and a reload read
// each key to tell whose it was. Over a generated sequence of 40,000 steps on
// a hundred plain subjects of every kind the exporter makes keys of — the
// probes of two collectors, with keys and without, their stale answers and
// rules; static targets, their skipped turns and rules; the bytes and the
// skipped lines of responses and of a directory's files, the files and their
// rules, a directory listed short; the endpoint's — on which a subject
// fails, with one text or another and in one stage or another, as a trip
// that stands reports it or one of a collector a reload retired; recovers;
// is forgotten; minutes and hours pass; and a reload forgets a collector or
// a static target: the log reads line for line as that of the same steps on
// a log that holds each subject under the key it had (reportedWas) and
// forgets by reading the keys, and holds the same failures, each with its
// stage, text, times and counts, and counts the same failures of rules.
//
// With five subjects among them whose keys were another's or read as
// another's — a probe of a target named as a static target, a file named
// schedule in a directory so named, a file whose name is a NUL and listing,
// a probe of a target with NULs that had the key of a rule — the same steps
// read otherwise on the log that holds the keys they had: it is those
// subjects alone that the log treats otherwise. Under the race detector it
// is 6,000 steps.
func TestTheFailureLogWritesWhatItDidForSubjectsThatHadKeysOfTheirOwn(t *testing.T) {
	const hex = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	var plain []logSubject
	rules := [][3]string{{"m", ".a", ""}, {"m", ".b", ".items[]"}, {"", "^app_jobs", ""}}
	withRules := func(s logSubject) {
		plain = append(plain, s)
		for _, rule := range rules {
			of := s
			of.rule, of.metric, of.expression, of.items = true, rule[0], rule[1], rule[2]
			plain = append(plain, of)
		}
	}
	for _, collector := range []string{"web", "files"} {
		for _, probe := range [][2]string{{"http://a", hex}, {"http://a", ""}, {"http://b", hex}} {
			s := logSubject{kind: probeSubject, parts: [3]string{collector, probe[0], probe[1]}}
			withRules(s)
			s.aspect = staleAspect
			plain = append(plain, s)
		}
		for _, name := range []string{"store", "vault"} {
			s := logSubject{kind: staticSubject, parts: [3]string{collector, name}}
			withRules(s)
			s.aspect = scheduleAspect
			plain = append(plain, s)
		}
		for _, at := range [][2]string{{"http://a", ""}, {"/var/metrics", ""}, {"/var/metrics", "a.prom"}, {"/var/metrics", "rule"}} {
			s := logSubject{kind: addressSubject, parts: [3]string{collector, at[0], at[1]}}
			if at[1] != "" {
				withRules(s)
			}
			for _, aspect := range []failureAspect{utf8Aspect, carbonLinesAspect, sampleLinesAspect, listingAspect, skippedAspect} {
				s.aspect = aspect
				if s.made() {
					plain = append(plain, s)
				}
			}
		}
	}
	for _, name := range []string{"store", "vault"} {
		plain = append(plain, logSubject{kind: endpointSubject, parts: [3]string{name, "demo_value"}})
	}
	others := []logSubject{
		{kind: probeSubject, parts: [3]string{"web", staticTargetWas + "store", hex}},
		{kind: addressSubject, parts: [3]string{"files", staticTargetWas + "vault", "schedule"}},
		{kind: addressSubject, parts: [3]string{"files", "/var/metrics", "\x00listing"}},
		{kind: probeSubject, parts: [3]string{"web", "http://a\x00\x00\x00rule\x00m\x002\x00.a"}},
		{kind: addressSubject, parts: [3]string{"web", "http://a\x00\x00", "stale"}},
	}
	seen := map[string]bool{}
	for _, s := range plain {
		if !s.made() || !s.plain() || seen[s.keyWas()] {
			t.Fatalf("%+v is no plain subject the exporter makes a key of, or had the key %q of another here", s, s.keyWas())
		}
		seen[s.keyWas()] = true
	}
	for _, s := range others {
		if !s.made() || s.plain() {
			t.Fatalf("%+v is a plain subject, or one the exporter makes no key of", s)
		}
	}
	failures := []error{nil, errors.New("connection refused"), errors.New("value is missing"), errors.New("not a number")}
	stages := []string{"http", "metric", "schedule"}
	var followed atomic.Pointer[followedConfig]
	followed.Store(&followedConfig{generation: 2, defined: map[string]uint64{"web": 1}})
	retired := configRead{followed: &followed, generation: 1}

	// run makes the steps on the two logs and returns the step at which
	// they first read otherwise, or -1.
	run := func(seed uint64, subjects []logSubject, steps int) (differs int, lines, kinds map[string]int) {
		random := rand.New(rand.NewPCG(seed, 29))
		clock := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
		logs := func() (*failureLog, *bytes.Buffer, *slog.Logger) {
			f, out := newFailureLog(), &bytes.Buffer{}
			f.now = func() time.Time { return clock }
			return f, out, slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: withoutLineTime}))
		}
		now, nowOut, nowLogger := logs()
		was, wasOut, wasLogger := logs()
		lines, kinds = map[string]int{}, map[string]int{}
		for step := range steps {
			s := subjects[random.IntN(len(subjects))]
			attrs := []any{"subject", fmt.Sprintf("%c %q %q", s.kind, s.parts, []string{string(s.aspect), s.metric, s.expression, s.items})}
			switch op := random.IntN(40); {
			case op < 6:
				now.recovered(nowLogger, s.key(), "recovered", attrs...)
				was.recovered(wasLogger, s.reportedWas().subject(), "recovered", attrs...)
				kinds["recovered"]++
			case op == 6:
				now.forget(s.key())
				was.forget(s.reportedWas().subject())
			case op == 7:
				clock = clock.Add(failureRepeatInterval + time.Minute)
			case op == 8 && random.IntN(4) == 0:
				clock = clock.Add(failureLogForget + time.Minute)
			case op == 9 && random.IntN(3) == 0:
				names := map[string]bool{[]string{"web", "files"}[random.IntN(2)]: true}
				now.forgetCollectors(names)
				forgetCollectorsReadingKeys(was, names)
				kinds["a collector forgotten"]++
			case op == 10:
				names := map[string]bool{[]string{"store", "vault"}[random.IntN(2)]: true}
				now.mu.Lock()
				now.forgetStaticTargetsLocked(names)
				now.mu.Unlock()
				forgetStaticTargetsReadingKeys(was, names)
				kinds["a static target forgotten"]++
			case op == 11:
				// The success of a trip of a collector a reload retired
				// is no recovery.
				now.recoveredFor(retired, nowLogger, s.key(), "recovered", attrs...)
				was.recoveredFor(retired, wasLogger, s.reportedWas().subject(), "recovered", attrs...)
			default:
				read := configRead{}
				if op == 12 {
					read = retired
				}
				stage, err := stages[random.IntN(len(stages))], failures[random.IntN(len(failures))]
				nowFailed, wasFailed := s.reported(), s.reportedWas()
				for range 1 + random.IntN(3) {
					nowLogged, wasLogged := nowLogger.With(attrs...), wasLogger.With(attrs...)
					nowFailed.failed(now, read, nowLogged, stage, err)
					wasFailed.failed(was, read, wasLogged, stage, err)
					clock = clock.Add(time.Duration(random.IntN(100)) * time.Second)
				}
			}
			if nowOut.String() != wasOut.String() {
				return step, lines, kinds
			}
			for _, kind := range []string{`"level":"WARN"`, `"repeat":true`, `"repeated":`, `"msg":"recovered"`, `"superseded":true`} {
				lines[kind] += strings.Count(nowOut.String(), kind)
			}
			nowOut.Reset()
			wasOut.Reset()
			if step%50 != 0 {
				continue
			}
			// The two hold the same failures, each as the other has it.
			held := map[string]failureState{}
			for _, s := range subjects {
				if st := now.entries[s.key().bytes]; st != nil {
					want := *st
					wasFailed := s.reportedWas()
					want.key = wasFailed.subject()
					if st.rule {
						want.trip = wasFailed.trip.bytes
					}
					held[want.key.bytes] = want
				}
			}
			if len(held) != len(now.entries) || len(held) != len(was.entries) {
				return step, lines, kinds
			}
			for key, want := range held {
				if st := was.entries[key]; st == nil || *st != want {
					return step, lines, kinds
				}
			}
			counted := map[string]int{}
			for _, st := range was.entries {
				if st.rule {
					counted[st.trip]++
				}
			}
			if !reflect.DeepEqual(was.ruleFailures, counted) || len(now.ruleFailures) != len(counted) {
				return step, lines, kinds
			}
			kinds["entries compared"] += len(held)
		}
		return -1, lines, kinds
	}
	steps := alloctest.UnlessRaced(40000, 6000)
	differs, lines, kinds := run(1, plain, steps)
	if differs >= 0 {
		t.Fatalf("at step %d the log of the plain subjects reads otherwise than the log that holds the keys they had, or holds other failures", differs)
	}
	for kind, floor := range map[string]int{`"level":"WARN"`: steps / 10, `"repeat":true`: steps / 10, `"repeated":`: steps / 400, `"msg":"recovered"`: steps / 100, `"superseded":true`: steps / 100} {
		if lines[kind] < floor {
			t.Errorf("only %d lines with %s were compared in %d steps", lines[kind], kind, steps)
		}
	}
	for kind, floor := range map[string]int{"a collector forgotten": steps / 400, "a static target forgotten": steps / 100, "entries compared": steps / 10} {
		if kinds[kind] < floor {
			t.Errorf("only %d times was %s in %d steps", kinds[kind], kind, steps)
		}
	}
	if differs, _, _ := run(1, append(append([]logSubject{}, plain...), others...), steps); differs < 0 {
		t.Errorf("with the subjects whose keys were another's among them the two logs read alike over %d steps: the log that holds the keys they had does not treat them as the log did", steps)
	}
}

// withoutLineTime leaves a line's time out.
func withoutLineTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
