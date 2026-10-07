package exporter

// The keys of the OTLP export (otlpKey): what a resource and a series of it
// are told from every other by, where points wait for an export, among the
// resources of one, and where the start times of cumulative series are kept.
//
// A key was its parts joined, a NUL and an = between them, and the parts
// are a target's and the operator's: a label's value, a resource's service
// name and its attributes' names and values may hold both. The tests here
// keep the old keys as an oracle (oldOTLPResourceKey, oldOTLPSeriesKey) and
// an export made with them (otlpKeyModel), and hold the export to it
// wherever no two subjects shared an old key, and to an export whose keys
// are each subject's own wherever two did.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// oldOTLPResourceKey is a resource's key as it was made: the service name,
// and a NUL, the name, = and the value of each attribute.
func oldOTLPResourceKey(r otlpResourceIdentity) string {
	var b strings.Builder
	b.WriteString(r.ServiceName)
	for _, name := range model.SortedKeys(r.Attributes) {
		b.WriteByte(0)
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(r.Attributes[name])
	}
	return b.String()
}

// oldOTLPSeriesKey is a series' key as it was made: the name, a NUL, the
// type, and a NUL, the name, = and the value of each label.
func oldOTLPSeriesKey(metric model.Metric) string {
	keys := make([]string, 0, len(metric.Labels))
	for key := range metric.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metric.Name)
	b.WriteByte(0)
	b.WriteString(string(metric.Type))
	for _, key := range keys {
		b.WriteByte(0)
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(metric.Labels[key])
	}
	return b.String()
}

// oldOTLPStartKey is the key a series' start time was kept under: its
// resource's key, a NUL, and its own.
func oldOTLPStartKey(resource string, m model.Metric) string {
	return resource + "\x00" + oldOTLPSeriesKey(m)
}

// sameSubject is what a subject is in a test, with nothing joined: every
// part quoted, so that the quotes say where each ends.
func sameSubject(pairs map[string]string, head ...string) string {
	var b strings.Builder
	for _, part := range head {
		b.WriteString(strconv.Quote(part))
		b.WriteByte(' ')
	}
	for _, name := range model.SortedKeys(pairs) {
		b.WriteString(strconv.Quote(name))
		b.WriteByte(':')
		b.WriteString(strconv.Quote(pairs[name]))
		b.WriteByte(' ')
	}
	return b.String()
}

// readOTLPKey reads a key back into the parts it was made of, as only a test
// does: the join, which ends at the first two NULs and has a 0x01 after
// every NUL of its own, and then the parts of the join, head of them and
// then names and values in turn, by the lengths after it. Between two parts
// the join has the one byte it was made with, which is checked and never
// looked for.
func readOTLPKey(key string, head int) (join string, parts []string, err error) {
	written, rest, ended := strings.Cut(key, otlpKeyEnd)
	if !ended {
		return "", nil, fmt.Errorf("no end of the join in %q", key)
	}
	if strings.Count(written, "\x00") != strings.Count(written, otlpKeyNUL) {
		return "", nil, fmt.Errorf("a NUL of the join %q is not written before 0x01", written)
	}
	join = strings.ReplaceAll(written, otlpKeyNUL, "\x00")
	at := 0
	for i := 0; rest != ""; i++ {
		long, read := binary.Uvarint([]byte(rest))
		if read <= 0 || long > uint64(len(join)) {
			return "", nil, fmt.Errorf("no length of a part of the join where %q begins", rest)
		}
		length := int(long)
		rest = rest[read:]
		if i > 0 {
			// Before a name, and between the parts of the head, a NUL;
			// before a value, =.
			between := byte(0)
			if i > head && (i-head)%2 == 1 {
				between = '='
			}
			if at >= len(join) || join[at] != between {
				return "", nil, fmt.Errorf("part %d does not follow %q at byte %d of the join %q", i, between, at, join)
			}
			at++
		}
		if at+length > len(join) {
			return "", nil, fmt.Errorf("part %d of %d bytes at byte %d of the join %q", i, length, at, join)
		}
		parts = append(parts, join[at:at+length])
		at += length
	}
	if at != len(join) || len(parts) < head || (len(parts)-head)%2 != 0 {
		return "", nil, fmt.Errorf("%d parts end at byte %d of the %d of the join", len(parts), at, len(join))
	}
	return join, parts, nil
}

// keyPieces are what the parts of a generated subject are made of: the
// bytes a key was joined with, bytes a length may be, a byte that is no
// UTF-8, a name a part may end or begin with, and what a key has after a
// NUL of its join and where the join ends.
var keyPieces = []string{"\x00", "=", "1", "0", "2", "a", "b", "counter", "\xff", "é", "\x00b=2", "10\x00", "\x01", "\x00\x00"}

// keyPart is a part of a subject: nothing, a piece, or several.
func keyPart(random *rand.Rand) string {
	var b strings.Builder
	for range random.IntN(4) {
		b.WriteString(keyPieces[random.IntN(len(keyPieces))])
	}
	if random.IntN(40) == 0 {
		b.WriteString(strings.Repeat("x\x00", 100))
	}
	return b.String()
}

// keySubject is a generated resource or series: the parts every one has, and
// its attributes or labels.
type keySubject struct {
	head  []string
	pairs map[string]string
}

func (s keySubject) key() string    { return otlpKey("", s.pairs, s.head...) }
func (s keySubject) String() string { return sameSubject(s.pairs, s.head...) }

// old is the subject's key as it was made.
func (s keySubject) old() string {
	if len(s.head) == 1 {
		return oldOTLPResourceKey(otlpResourceIdentity{ServiceName: s.head[0], Attributes: s.pairs})
	}
	return oldOTLPSeriesKey(model.Metric{Name: s.head[0], Type: model.MetricType(s.head[1]), Labels: s.pairs})
}

// joinedAlike is another subject whose parts were joined to the bytes s's
// were, by one of three rules, and false when the rule makes none of s: the
// first two pairs as one, whose value holds what was between them; the
// first pair as the end of the head; or the only pair cut at an = of its
// value instead.
func (s keySubject) joinedAlike(rule int) (keySubject, bool) {
	names := model.SortedKeys(s.pairs)
	other := keySubject{head: slices.Clone(s.head), pairs: map[string]string{}}
	switch {
	case rule == 0 && len(names) >= 2:
		for _, name := range names[2:] {
			other.pairs[name] = s.pairs[name]
		}
		other.pairs[names[0]] = s.pairs[names[0]] + "\x00" + names[1] + "=" + s.pairs[names[1]]
	case rule == 1 && len(names) >= 1:
		for _, name := range names[1:] {
			other.pairs[name] = s.pairs[name]
		}
		other.head[len(other.head)-1] += "\x00" + names[0] + "=" + s.pairs[names[0]]
	case rule == 2 && len(names) == 1 && strings.Contains(s.pairs[names[0]], "="):
		before, after, _ := strings.Cut(s.pairs[names[0]], "=")
		other.pairs[names[0]+"="+before] = after
	default:
		return keySubject{}, false
	}
	return other, true
}

// keySubjects are n generated resources and series, and after each one
// that has it another whose parts were joined alike.
func keySubjects(n int) []keySubject {
	random := rand.New(rand.NewPCG(20261006, 35)) //nolint:gosec // subjects for a test
	var out []keySubject
	for len(out) < n {
		s := keySubject{head: []string{keyPart(random)}, pairs: map[string]string{}}
		if random.IntN(2) == 0 {
			s.head = append(s.head, keyPart(random))
		}
		for range random.IntN(4) {
			s.pairs[keyPart(random)] = keyPart(random)
		}
		out = append(out, s)
		if other, has := s.joinedAlike(random.IntN(3)); has {
			out = append(out, other)
		}
	}
	// And parts whose lengths take two bytes and three to write.
	long := keySubject{head: []string{strings.Repeat("y", 20000), "counter"}, pairs: map[string]string{strings.Repeat("\x00", 300): strings.Repeat("=", 130), "a": ""}}
	out = append(out, long)
	for rule := range 2 {
		other, _ := long.joinedAlike(rule)
		out = append(out, other)
	}
	return out
}

// A key is read back to the parts it was made of, whatever they hold — the
// bytes the parts are joined with and a key's own 0x01 and two NULs, bytes
// that are no UTF-8, nothing, more bytes than one byte of a length counts —
// so no two subjects have one key: over generated resources and series,
// thousands of which were joined to the bytes another was and had its key.
// The join a key is read back to is the old key itself.
func TestAnOTLPKeyIsReadBackToItsParts(t *testing.T) {
	subjects := keySubjects(alloctest.UnlessRaced(40000, 4000))
	keys, old := map[string]string{}, map[string]string{}
	shared := 0
	for _, s := range subjects {
		key := s.key()
		join, parts, err := readOTLPKey(key, len(s.head))
		if err != nil {
			t.Fatalf("the key %q of %s is not read back: %v", key, s, err)
		}
		want := slices.Clone(s.head)
		for _, name := range model.SortedKeys(s.pairs) {
			want = append(want, name, s.pairs[name])
		}
		if !slices.Equal(parts, want) {
			t.Fatalf("the key %q of %s is read back as %q", key, s, parts)
		}
		if join != s.old() {
			t.Fatalf("the key %q of %s is of the join %q, and its old key was %q", key, s, join, s.old())
		}
		if other, taken := keys[key]; taken && other != s.String() {
			t.Fatalf("%s and %s have the one key %q", other, s, key)
		}
		keys[key] = s.String()
		if other, taken := old[s.old()]; taken && other != s.String() {
			shared++
		}
		old[s.old()] = s.String()
	}
	if floor := len(subjects) / 8; shared < floor {
		t.Errorf("%d of %d subjects had the old key of another, want at least %d for the test to be of them", shared, len(subjects), floor)
	}
}

// The subjects that had one key, each pair with what went wrong for it, have
// a key each: two series of a resource, two resources, and two series of two
// resources where their start times are kept.
func TestSubjectsOfTheOTLPExportThatHadOneKeyHaveTheirOwn(t *testing.T) {
	series := func(name string, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Type: model.CounterMetricType, Labels: labels}
	}
	for name, pair := range map[string][2]model.Metric{
		"a label's value that holds a NUL and the next label": {series("m", map[string]string{"a": "1\x00b=2"}), series("m", map[string]string{"a": "1", "b": "2"})},
		"three labels as one":                    {series("m", map[string]string{"a": "1\x00b=2\x00c=3"}), series("m", map[string]string{"a": "1", "b": "2", "c": "3"})},
		"a name that holds the type and a label": {series("m\x00counter\x00a=1", nil), series("m", map[string]string{"a": "1\x00counter"})},
		"a label's name that holds an =":         {series("m", map[string]string{"a=b": "c"}), series("m", map[string]string{"a": "b=c"})},
	} {
		if a, b := oldOTLPSeriesKey(pair[0]), oldOTLPSeriesKey(pair[1]); a != b {
			t.Errorf("%s: the two series had the keys %q and %q, and the test is of two that had one", name, a, b)
		}
		if a, b := otlpMetricKey(pair[0]), otlpMetricKey(pair[1]); a == b {
			t.Errorf("%s: the two series have the one key %q", name, a)
		}
	}
	resource := func(service string, attributes map[string]string) otlpResourceIdentity {
		return otlpResourceIdentity{ServiceName: service, Attributes: attributes}
	}
	for name, pair := range map[string][2]otlpResourceIdentity{
		"an attribute's name that holds an =":                       {resource("svc", map[string]string{"a=b": "c"}), resource("svc", map[string]string{"a": "b=c"})},
		"an attribute's value that holds a NUL and the next":        {resource("svc", map[string]string{"a": "1\x00b=2"}), resource("svc", map[string]string{"a": "1", "b": "2"})},
		"a service name that holds a NUL and an attribute":          {resource("svc\x00a=1", nil), resource("svc", map[string]string{"a": "1"})},
		"a service name that holds a NUL and an attribute's name":   {resource("svc\x00a", map[string]string{"b": "2"}), resource("svc", map[string]string{"a\x00b": "2"})},
		"attributes that are no UTF-8, which a !!binary scalar may": {resource("svc", map[string]string{"a": "\xff\x00b=\xfe"}), resource("svc", map[string]string{"a": "\xff", "b": "\xfe"})},
	} {
		if a, b := oldOTLPResourceKey(pair[0]), oldOTLPResourceKey(pair[1]); a != b {
			t.Errorf("%s: the two resources had the keys %q and %q, and the test is of two that had one", name, a, b)
		}
		if a, b := pair[0].key(), pair[1].key(); a == b {
			t.Errorf("%s: the two resources have the one key %q", name, a)
		}
	}
	// Where start times are kept a key is a resource's and a series' at
	// once: the resource's last attribute may hold what the series begins
	// with, and the series' last label what another's resource ends with.
	type started struct {
		resource otlpResourceIdentity
		series   model.Metric
	}
	for name, pair := range map[string][2]started{
		"a resource's attribute that holds the series": {
			{resource("svc", map[string]string{"a": "1"}), series("x", map[string]string{"l": "v\x00x\x00counter"})},
			{resource("svc", map[string]string{"a": "1\x00x\x00counter\x00l=v"}), series("x", nil)},
		},
		"a service name that holds the series": {
			{resource("svc", nil), series("x", map[string]string{"l": "v\x00x\x00counter"})},
			{resource("svc\x00x\x00counter\x00l=v", nil), series("x", nil)},
		},
	} {
		if a, b := oldOTLPStartKey(oldOTLPResourceKey(pair[0].resource), pair[0].series), oldOTLPStartKey(oldOTLPResourceKey(pair[1].resource), pair[1].series); a != b {
			t.Errorf("%s: the two had the keys %q and %q, and the test is of two that had one", name, a, b)
		}
		starts := newOTLPStartTimes()
		starts.forResource(pair[0].resource.key())(pair[0].series, "1")
		starts.forResource(pair[1].resource.key())(pair[1].series, "2")
		if len(starts.series) != 2 {
			t.Errorf("%s: the start times of the two series are kept as %d", name, len(starts.series))
		}
	}
}

// Keys are in the order the old keys had, which is the order an export's
// resources and each one's points are in: over generated subjects, among
// them many whose old key was the beginning of another's and went on with a
// NUL or ended there, and those that had the old key of another, which
// follow one another.
func TestOTLPKeysAreInTheOrderTheOldKeysHad(t *testing.T) {
	for head := 1; head <= 2; head++ {
		old := map[string]string{}
		for _, s := range keySubjects(alloctest.UnlessRaced(20000, 2000)) {
			if len(s.head) == head {
				old[s.key()] = s.old()
			}
		}
		// The old keys in the order of the keys are the old keys in their
		// own order: of two subjects that had a key each, the one that was
		// exported first still is.
		var distinct []string
		begins := 0
		for _, key := range model.SortedKeys(old) {
			switch last := len(distinct) - 1; {
			case last >= 0 && old[key] == distinct[last]:
				continue
			case last >= 0 && strings.HasPrefix(old[key], distinct[last]+"\x00"):
				begins++
			}
			distinct = append(distinct, old[key])
		}
		want := slices.Clone(distinct)
		slices.Sort(want)
		if !slices.Equal(distinct, slices.Compact(want)) {
			t.Fatalf("the keys of %d parts before the pairs are not in the order of their old keys", head)
		}
		if len(distinct) < len(old)/2 || len(distinct) > len(old)*9/10 || begins < len(old)/50 {
			t.Fatalf("%d old keys of %d subjects, %d of them a key before it and then a NUL: the test is of many that had their own, some that had another's, and some that went on from another's with a NUL", len(distinct), len(old), begins)
		}
	}
	// Two that were joined alike are in the order of their keys, the same
	// in every export.
	a, b := otlpMetricKey(model.Metric{Name: "m", Labels: map[string]string{"a": "1\x00b=2"}}), otlpMetricKey(model.Metric{Name: "m", Labels: map[string]string{"a": "1", "b": "2"}})
	for range 20 {
		if got := model.SortedKeys(map[string]bool{a: true, b: true}); !slices.Equal(got, []string{min(a, b), max(a, b)}) {
			t.Fatalf("two series that were joined alike are in the order %q", got)
		}
	}
}

// keySink keeps what a measured function made, so that it is made.
var keySink string

// A key is made in one allocation, and a series' start time is found with
// no other: fewer than the old keys cost, which grew as they were written
// beside the names in order, and were joined to their resource's. A subject
// of more pairs than a key puts in order in place costs two more, and still
// fewer.
func TestAnOTLPKeyIsMadeInOneAllocation(t *testing.T) {
	labelled := model.Metric{Name: "item_requests_total", Type: model.CounterMetricType, Value: 1, Labels: map[string]string{"id": "item-1024", "region": "eu-west-1", "kind": "disk"}}
	plain := model.Metric{Name: "item_requests_total", Type: model.CounterMetricType, Value: 1}
	nul := model.Metric{Name: "item_requests_total", Type: model.CounterMetricType, Value: 1, Labels: map[string]string{"id": "item\x001024", "region": "eu\x00west\x001"}}
	long := model.Metric{Name: "item_requests_total", Type: model.CounterMetricType, Value: 1, Labels: map[string]string{"id": strings.Repeat("item\x00", 60), "region": strings.Repeat("eu", 9000)}}
	many := model.Metric{Name: "item_requests_total", Type: model.CounterMetricType, Value: 1, Labels: map[string]string{}}
	for i := range 12 {
		many.Labels["label_"+strconv.Itoa(i)] = "value-" + strconv.Itoa(i)
	}
	identity := otlpResourceIdentity{ServiceName: "prometheus-universal-exporter", Attributes: map[string]string{"deployment.environment": "production", "k8s.namespace.name": "monitoring"}}
	starts := newOTLPStartTimes()
	start := starts.forResource(identity.key())
	oldResource := oldOTLPResourceKey(identity)
	for _, c := range []struct {
		name     string
		now, old func()
		most     float64
	}{
		{"a series with labels", func() { keySink = otlpMetricKey(labelled) }, func() { keySink = oldOTLPSeriesKey(labelled) }, 1},
		{"a series without labels", func() { keySink = otlpMetricKey(plain) }, func() { keySink = oldOTLPSeriesKey(plain) }, 1},
		{"a series with NULs in its labels", func() { keySink = otlpMetricKey(nul) }, func() { keySink = oldOTLPSeriesKey(nul) }, 1},
		{"a series with long labels, one with NULs", func() { keySink = otlpMetricKey(long) }, func() { keySink = oldOTLPSeriesKey(long) }, 1},
		{"a series with twelve labels", func() { keySink = otlpMetricKey(many) }, func() { keySink = oldOTLPSeriesKey(many) }, 3},
		{"a resource", func() { keySink = identity.key() }, func() { keySink = oldOTLPResourceKey(identity) }, 1},
		{"the start time of a series with labels", func() { keySink = start(labelled, "1") }, func() { keySink = oldOTLPStartKey(oldResource, labelled) }, 1},
		{"the start time of a series without labels", func() { keySink = start(plain, "1") }, func() { keySink = oldOTLPStartKey(oldResource, plain) }, 1},
	} {
		now := alloctest.AllocsAtMost(200, c.most, c.now)
		old, _ := alloctest.Allocations(200, c.old)
		if now > c.most || now >= old {
			t.Errorf("%s: %v allocations, want at most %v and fewer than the old key's %v", c.name, now, c.most, old)
		}
	}
	// The one allocation is of the key's size, whatever that is: keys of
	// every size over a few hundred bytes, their parts' lengths written
	// with one byte and with two and their NULs with two, never grow past
	// what was allocated for them, which a size one byte short would at
	// the sizes the allocator gives.
	var sized []model.Metric
	for n := range 300 {
		sized = append(sized, model.Metric{Name: strings.Repeat("m", 100+n), Type: model.CounterMetricType, Labels: map[string]string{"a": strings.Repeat("\x00", n)}})
	}
	if got := alloctest.AllocsAtMost(20, float64(len(sized)), func() {
		for i := range sized {
			keySink = otlpMetricKey(sized[i])
		}
	}); got != float64(len(sized)) {
		t.Errorf("%d keys of every size take %v allocations, want one each", len(sized), got)
	}
}

// otlpKeyModel is the export as it keeps its points and its start times
// (queueOTLPResource, requeueOTLP, drainOTLP, pushOTLP), with the keys a
// test gives it: the old ones, for the export as it was, or keys that are
// each subject's own. Its resources and their points are in the order of
// the old keys, and, where two had one, of the export's own keys. Its
// points are made as they were before a name was one writer's
// (otlpMetricsAsItWas).
//
// oneWriter is the model in which a name of a resource is exported as the
// kind written last (otlpMetricsOf), which it settles among its series
// before any point is made: it is a model of series whose values their
// types allow, each with the time it was scraped at, where a type is a kind
// and a series is a point. A cumulative series it leaves out is still one
// its start times have seen. leftOut counts the series it has left out.
type otlpKeyModel struct {
	resource  func(otlpResourceIdentity) string
	series    func(model.Metric) string
	start     func(resource string, m model.Metric) string
	pending   map[string]*otlpModelResource
	starts    *otlpStartTimes
	oneWriter bool
	queued    int64
	leftOut   int
}

// otlpModelResource is a resource of the model and the points waiting or
// drained for it, and when each was queued, by its key.
type otlpModelResource struct {
	identity otlpResourceIdentity
	waiting  map[string]model.Metric
	metrics  []model.Metric
	written  map[string]int64
}

// oldOTLPKeys is the export as it was, a key the parts joined.
func oldOTLPKeys() *otlpKeyModel {
	return &otlpKeyModel{resource: oldOTLPResourceKey, series: oldOTLPSeriesKey, start: oldOTLPStartKey, pending: map[string]*otlpModelResource{}, starts: newOTLPStartTimes()}
}

// ownOTLPKeys is the export with keys that are each subject's own, made of
// nothing the export's keys are made with: every part quoted. A name is
// one writer's in it.
func ownOTLPKeys() *otlpKeyModel {
	return &otlpKeyModel{
		oneWriter: true,
		resource:  func(r otlpResourceIdentity) string { return sameSubject(r.Attributes, r.ServiceName) },
		series:    func(m model.Metric) string { return sameSubject(m.Labels, m.Name, string(m.Type)) },
		start: func(resource string, m model.Metric) string {
			return strconv.Quote(resource) + sameSubject(m.Labels, m.Name, string(m.Type))
		},
		pending: map[string]*otlpModelResource{}, starts: newOTLPStartTimes(),
	}
}

func (k *otlpKeyModel) of(identity otlpResourceIdentity) *otlpModelResource {
	key := k.resource(identity)
	if k.pending[key] == nil {
		k.pending[key] = &otlpModelResource{identity: identity, waiting: map[string]model.Metric{}, written: map[string]int64{}}
	}
	return k.pending[key]
}

// queue is queueOTLPResource: a point replaces the one waiting under its
// key.
func (k *otlpKeyModel) queue(identity otlpResourceIdentity, set model.MetricSet) {
	if len(set.Metrics) == 0 {
		return
	}
	resource := k.of(identity)
	for _, m := range set.Metrics {
		k.queued++
		resource.waiting[k.series(m)], resource.written[k.series(m)] = m, k.queued
	}
}

// drain is drainOTLP: the resources and each one's points in order.
func (k *otlpKeyModel) drain() []*otlpModelResource {
	var out []*otlpModelResource
	for _, resource := range k.pending {
		for _, m := range resource.waiting {
			resource.metrics = append(resource.metrics, m)
		}
		slices.SortFunc(resource.metrics, func(a, b model.Metric) int {
			if c := strings.Compare(oldOTLPSeriesKey(a), oldOTLPSeriesKey(b)); c != 0 {
				return c
			}
			return strings.Compare(otlpMetricKey(a), otlpMetricKey(b))
		})
		out = append(out, resource)
	}
	slices.SortFunc(out, func(a, b *otlpModelResource) int {
		if c := strings.Compare(oldOTLPResourceKey(a.identity), oldOTLPResourceKey(b.identity)); c != 0 {
			return c
		}
		return strings.Compare(a.identity.key(), b.identity.key())
	})
	k.pending = map[string]*otlpModelResource{}
	return out
}

// requeue is requeueOTLP: a drained point waits again unless one was queued
// under its key since.
func (k *otlpKeyModel) requeue(drained []*otlpModelResource) {
	for _, was := range drained {
		resource := k.of(was.identity)
		for _, m := range was.metrics {
			if _, newer := resource.waiting[k.series(m)]; !newer {
				resource.waiting[k.series(m)], resource.written[k.series(m)] = m, was.written[k.series(m)]
			}
		}
	}
}

// export is the body pushOTLP sends for what was drained, uncompressed.
func (k *otlpKeyModel) export(t *testing.T, drained []*otlpModelResource) []byte {
	t.Helper()
	payload := otlpPayload{}
	for _, resource := range drained {
		if len(resource.metrics) == 0 {
			continue
		}
		key := k.resource(resource.identity)
		start := func(m model.Metric, at string) string { return k.starts.start(k.start(key, m), cumulativeCount(m), at) }
		metrics := resource.metrics
		if k.oneWriter {
			// Every cumulative series drained is one the start times
			// have seen, in the order drained, those left out too.
			started := map[string]string{}
			for _, m := range resource.metrics {
				if m.Type != model.GaugeMetricType {
					started[k.series(m)] = start(m, strconv.FormatInt(*m.Timestamp*int64(time.Millisecond), 10))
				}
			}
			metrics, start = k.ofTheLastWriter(resource), func(m model.Metric, _ string) string { return started[k.series(m)] }
		}
		payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: otlpResource{Attributes: resource.identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}, Metrics: otlpMetricsAsItWas(model.MetricSet{Metrics: metrics}, "0", start)}}})
	}
	body, err := encodeOTLP(payload, model.OTLPCompressionNone)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// ofTheLastWriter are the series drained for a resource without those of a
// name that are of another type than the one of the name queued last.
func (k *otlpKeyModel) ofTheLastWriter(resource *otlpModelResource) []model.Metric {
	last := map[string]model.Metric{}
	for _, m := range resource.metrics {
		if of, seen := last[m.Name]; !seen || resource.written[k.series(m)] > resource.written[k.series(of)] {
			last[m.Name] = m
		}
	}
	kept := make([]model.Metric, 0, len(resource.metrics))
	for _, m := range resource.metrics {
		if m.Type == last[m.Name].Type {
			kept = append(kept, m)
		}
	}
	k.leftOut += len(resource.metrics) - len(kept)
	return kept
}

// otlpKeyServer is a server that exports, uncompressed, to a stand-in
// endpoint that keeps what it is sent. It has no collector: its points are
// queued by the test, as a probe's and a static target's are. While
// unavailable is set the endpoint keeps nothing and answers 503, to be
// asked again in an hour, which no export waits for: its points wait
// again.
type otlpKeyServer struct {
	*Server
	otlp        model.OTLPConfig
	mu          sync.Mutex
	bodies      [][]byte
	unavailable bool
}

func newOTLPKeyServer(t *testing.T, service string, attributes map[string]string) *otlpKeyServer {
	t.Helper()
	k := &otlpKeyServer{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the export: %v", err)
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.unavailable {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		k.bodies = append(k.bodies, body)
	}))
	t.Cleanup(endpoint.Close)
	k.otlp = otlpConfig(endpoint.URL)
	k.otlp.Compression = model.OTLPCompressionNone
	k.otlp.ServiceName, k.otlp.ResourceAttributes = service, attributes
	quiet := slog.New(slog.DiscardHandler)
	k.Server = NewServer(config.NewManager(&model.Config{OTLP: k.otlp}, "", quiet), "python3", quiet)
	return k
}

// sent is the body of the one export made since the last was read.
func (k *otlpKeyServer) sent(t *testing.T) []byte {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.bodies) != 1 {
		t.Fatalf("%d exports reached the endpoint, want 1", len(k.bodies))
	}
	body := k.bodies[0]
	k.bodies = nil
	return body
}

// push sends what was drained, as an export does without the exporter's own
// metrics, and returns the body the endpoint got.
func (k *otlpKeyServer) push(t *testing.T, drained []otlpResourceSet) []byte {
	t.Helper()
	if _, _, err := k.pushOTLP(context.Background(), k.otlp, drained, time.Minute); err != nil {
		t.Fatal(err)
	}
	return k.sent(t)
}

// otlpLines are the points of an export's body, a line each: the resource's
// attributes, the series, its value or count, and for a cumulative point
// the millisecond it started at.
func otlpLines(t *testing.T, body []byte) []string {
	t.Helper()
	var payload otlpPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("%v:\n%s", err, body)
	}
	attributes := func(open, between, shut string, attributes []otlpAttribute) string {
		parts := make([]string, 0, len(attributes))
		for _, a := range attributes {
			parts = append(parts, a.Key+"="+strconv.Quote(a.Value.StringValue))
		}
		return open + strings.Join(parts, between) + shut
	}
	var lines []string
	for _, resource := range payload.ResourceMetrics {
		of := attributes("[", " ", "] ", resource.Resource.Attributes)
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				line := func(labels []otlpAttribute, value, start string) {
					if start != "" {
						value += " since " + strings.TrimSuffix(start, "000000")
					}
					lines = append(lines, of+m.Name+attributes("{", ",", "} ", labels)+value)
				}
				switch {
				case m.Gauge != nil:
					for _, p := range m.Gauge.DataPoints {
						line(p.Attributes, strconv.FormatFloat(float64(*p.AsDouble), 'g', -1, 64), p.StartTimeUnixNano)
					}
				case m.Sum != nil:
					for _, p := range m.Sum.DataPoints {
						line(p.Attributes, strconv.FormatFloat(float64(*p.AsDouble), 'g', -1, 64), p.StartTimeUnixNano)
					}
				case m.Histogram != nil:
					for _, p := range m.Histogram.DataPoints {
						line(p.Attributes, p.Count, p.StartTimeUnixNano)
					}
				case m.Summary != nil:
					for _, p := range m.Summary.DataPoints {
						line(p.Attributes, p.Count, p.StartTimeUnixNano)
					}
				}
			}
		}
	}
	return lines
}

// point is a series' value scraped at the millisecond at.
func point(name string, typ model.MetricType, labels map[string]string, value float64, at int64) model.Metric {
	return model.Metric{Name: name, Type: typ, Labels: labels, Value: value, Timestamp: &at}
}

// wantLines fails unless the export, of the old keys or of the export's
// own, holds just the lines.
func wantLines(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Two series of a resource that differ in a label's value alone, one's
// holding a NUL and what the other's next label is, are both exported. They
// had one key in the queue, where the later replaced the other, which was
// never exported.
func TestSeriesWhoseLabelsWereJoinedAlikeAreBothExported(t *testing.T) {
	server, old := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys()
	identity := defaultResourceIdentity(server.otlp)
	set := model.MetricSet{Metrics: []model.Metric{
		point("m_total", model.CounterMetricType, map[string]string{"a": "1\x00b=2"}, 5, 1000),
		point("m_total", model.CounterMetricType, map[string]string{"a": "1", "b": "2"}, 100, 1001),
	}}
	server.queueOTLPResource(set, identity, scrapeTime{})
	old.queue(identity, set)
	wantLines(t, "the export", otlpLines(t, server.push(t, server.drainOTLP())),
		`[service.name="svc"] m_total{a="1",b="2"} 100 since 1001`,
		`[service.name="svc"] m_total{a="1\x00b=2"} 5 since 1000`)
	wantLines(t, "the export with the old keys", otlpLines(t, old.export(t, old.drain())),
		`[service.name="svc"] m_total{a="1",b="2"} 100 since 1001`)
}

// A point that an export could not deliver waits again when a point of
// another series was queued meanwhile, though the two series' labels were
// joined alike: the point was taken to have a newer one, and was dropped.
func TestAPointWaitsAgainBesideASeriesItWasJoinedLike(t *testing.T) {
	server, old := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys()
	identity := defaultResourceIdentity(server.otlp)
	first := model.MetricSet{Metrics: []model.Metric{point("m", model.GaugeMetricType, map[string]string{"a": "1\x00b=2"}, 1, 1000)}}
	second := model.MetricSet{Metrics: []model.Metric{point("m", model.GaugeMetricType, map[string]string{"a": "1", "b": "2"}, 2, 2000)}}
	server.queueOTLPResource(first, identity, scrapeTime{})
	old.queue(identity, first)
	drained, oldDrained := server.drainOTLP(), old.drain()
	server.queueOTLPResource(second, identity, scrapeTime{})
	old.queue(identity, second)
	server.requeueOTLP(drained)
	old.requeue(oldDrained)
	wantLines(t, "the export", otlpLines(t, server.push(t, server.drainOTLP())),
		`[service.name="svc"] m{a="1",b="2"} 2`,
		`[service.name="svc"] m{a="1\x00b=2"} 1`)
	wantLines(t, "the export with the old keys", otlpLines(t, old.export(t, old.drain())),
		`[service.name="svc"] m{a="1",b="2"} 2`)
}

// Two resources that differ in where an attribute's name ends are exported
// as two, each with its own points. They had one key: the points of both
// were exported under the attributes of the one queued first, and a series
// both had was exported once.
func TestResourcesWhoseAttributesWereJoinedAlikeAreExportedApart(t *testing.T) {
	server, old := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys()
	one := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a=b": "c"}}}, server.otlp)
	two := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a": "b=c"}}}, server.otlp)
	sets := func(target string, value float64) model.MetricSet {
		return model.MetricSet{Metrics: []model.Metric{
			point("up", model.GaugeMetricType, nil, value, 1000),
			point("v", model.GaugeMetricType, map[string]string{"static_target": target}, value, 1000),
		}}
	}
	server.queueOTLPResource(sets("one", 1), one, scrapeTime{})
	server.queueOTLPResource(sets("two", 2), two, scrapeTime{})
	old.queue(one, sets("one", 1))
	old.queue(two, sets("two", 2))
	wantLines(t, "the export", otlpLines(t, server.push(t, server.drainOTLP())),
		`[service.name="svc" a="b=c"] up{} 2`,
		`[service.name="svc" a="b=c"] v{static_target="two"} 2`,
		`[service.name="svc" a=b="c"] up{} 1`,
		`[service.name="svc" a=b="c"] v{static_target="one"} 1`)
	wantLines(t, "the export with the old keys", otlpLines(t, old.export(t, old.drain())),
		`[service.name="svc" a=b="c"] up{} 2`,
		`[service.name="svc" a=b="c"] v{static_target="one"} 1`,
		`[service.name="svc" a=b="c"] v{static_target="two"} 2`)
}

// The exporter's own metrics are exported under its own resource beside a
// static target's whose attributes were joined to the same bytes. The two
// had one key, and the exporter's metrics were added to the target's
// resource: exported with its attributes, as the target's.
func TestTheExportersOwnMetricsAreNotExportedUnderATargetsResource(t *testing.T) {
	server := newOTLPKeyServer(t, "svc", map[string]string{"a": "1\x00b=2"})
	target := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a": "1", "b": "2"}}}, server.otlp)
	if own := defaultResourceIdentity(server.otlp); oldOTLPResourceKey(own) != oldOTLPResourceKey(target) {
		t.Fatalf("the two resources had the keys %q and %q, and the test is of two that had one", oldOTLPResourceKey(own), oldOTLPResourceKey(target))
	}
	server.queueOTLPResource(model.MetricSet{Metrics: []model.Metric{point("v", model.GaugeMetricType, nil, 1, 1000)}}, target, scrapeTime{})
	server.exportOTLP(context.Background(), time.Minute)
	targets, own, exports := 0, 0, 0
	for _, line := range otlpLines(t, server.sent(t)) {
		switch {
		case line == `[service.name="svc" a="1" b="2"] v{} 1`:
			targets++
		case strings.HasPrefix(line, `[service.name="svc" a="1\x00b=2"] `):
			own++
			if strings.Contains(line, "] http_exporter_otlp_exports_total{") {
				exports++
			}
		default:
			t.Errorf("the export holds %s", line)
		}
	}
	if targets != 1 || exports != 2 {
		t.Errorf("the export holds %d points of the target's and %d of the exporter's own, %d of them the exports it counts; want 1 of the target's and the exporter's own under its resource", targets, own, exports)
	}
}

// A series of one resource and a series of another keep their own start
// times though the first resource's key and the first series' were joined
// to the bytes the other two were: an attribute's value holds what the
// other's series begins with. The two had one start time between them: each
// took the other's count for its own last, so the lower was reset at every
// export, the higher took the start the lower was given, and a reset of the
// higher went unseen.
func TestSeriesOfTwoResourcesKeepTheirOwnStartTimes(t *testing.T) {
	server, old := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys()
	one := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a": "1"}}}, server.otlp)
	two := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a": "1\x00x_total\x00counter\x00l=v"}}}, server.otlp)
	export := func(at int64, high, low float64) (now, was []string) {
		first := model.MetricSet{Metrics: []model.Metric{point("x_total", model.CounterMetricType, map[string]string{"l": "v\x00x_total\x00counter"}, high, at)}}
		second := model.MetricSet{Metrics: []model.Metric{point("x_total", model.CounterMetricType, nil, low, at+500)}}
		server.queueOTLPResource(first, one, scrapeTime{})
		server.queueOTLPResource(second, two, scrapeTime{})
		old.queue(one, first)
		old.queue(two, second)
		return otlpLines(t, server.push(t, server.drainOTLP())), otlpLines(t, old.export(t, old.drain()))
	}
	const (
		high = `[service.name="svc" a="1"] x_total{l="v\x00x_total\x00counter"} `
		low  = `[service.name="svc" a="1\x00x_total\x00counter\x00l=v"] x_total{} `
	)
	now, was := export(1000, 100, 5)
	wantLines(t, "the first export", now, high+"100 since 1000", low+"5 since 1500")
	wantLines(t, "the first export with the old keys", was, high+"100 since 1000", low+"5 since 1500")
	// Both grew.
	now, was = export(2000, 101, 6)
	wantLines(t, "the second export", now, high+"101 since 1000", low+"6 since 1500")
	wantLines(t, "the second export with the old keys", was, high+"101 since 1500", low+"6 since 2500")
	// The higher was reset, to a count above the lower's.
	now, was = export(3000, 50, 7)
	wantLines(t, "the third export", now, high+"50 since 3000", low+"7 since 1500")
	wantLines(t, "the third export with the old keys", was, high+"50 since 2500", low+"7 since 3500")
}

// keyRunPieces are what the names and values of a generated run are drawn
// from. plain is what no two subjects are joined alike of; the others hold
// the bytes the parts were joined with, and what makes one subject's join
// another's.
var keyRunPieces = struct {
	names, labels                         []string
	values, plainValues                   []string
	services, plainServices               []string
	attributes, plainAttributes           []string
	attributeValues, plainAttributeValues []string
}{
	// Classic names, as every name of a series queued is: one that
	// name_escaping underscores made of http.server.duration, and one that
	// values did.
	names:                []string{"m", "m_total", "http_server_duration", "U__http_2e_server_2e_duration", "a:b"},
	labels:               []string{"a", "b", "c", "static_target"},
	plainValues:          []string{"1", "2", "10", "x", "b=2", "é", "a b"},
	values:               []string{"1", "2", "1\x00b=2", "2\x00c=3", "1\x00b=2\x00c=3", "\x00", "=", "1\x00", "x"},
	plainServices:        []string{"svc", "svc2"},
	services:             []string{"svc", "svc\x00a=1", "svc\x00a=1\x00b=2"},
	plainAttributes:      []string{"a", "b", "k8s.pod.name"},
	attributes:           []string{"a", "b", "a=b", "a=1\x00b"},
	plainAttributeValues: []string{"1", "2", "b=c"},
	attributeValues:      []string{"1", "2", "c", "b=c", "1\x00b=2", "1\x00m\x00counter"},
}

// keyRun is one generated run of exports: its resources, and the series its
// rounds are drawn from.
type keyRun struct {
	random    *rand.Rand
	resources []otlpResourceIdentity
	series    []model.Metric
	counts    []float64
	nul       bool
}

func pick(random *rand.Rand, from []string) string { return from[random.IntN(len(from))] }

// newKeyRun is the seed's run. A plain run is of names and values that hold
// nothing two subjects are joined alike of. In a run of one kind every
// name has one type, as the names of one writer have; in the others a
// name may be a counter of one series and a gauge of another, which two
// writers of a resource make it, and an export has it as one kind
// (otlpMetricsOf).
func newKeyRun(seed uint64, plain, oneKind bool) *keyRun {
	p := keyRunPieces
	values, services, attributes, attributeValues := p.values, p.services, p.attributes, p.attributeValues
	if plain {
		values, services, attributes, attributeValues = p.plainValues, p.plainServices, p.plainAttributes, p.plainAttributeValues
	}
	r := &keyRun{random: rand.New(rand.NewPCG(seed, 35))} //nolint:gosec // exports for a test
	pairs := func(names, values []string) map[string]string {
		out := map[string]string{}
		for range r.random.IntN(4) {
			out[pick(r.random, names)] = pick(r.random, values)
		}
		return out
	}
	// The exporter's own resource, and static targets' made over it.
	otlp := model.OTLPConfig{ServiceName: pick(r.random, services), ResourceAttributes: pairs(attributes, attributeValues)}
	r.resources = append(r.resources, defaultResourceIdentity(otlp))
	for range 1 + r.random.IntN(3) {
		target := &model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: pairs(attributes, attributeValues)}}
		if r.random.IntN(2) == 0 {
			target.OTLP.ServiceName = pick(r.random, services)
		}
		r.resources = append(r.resources, targetResource(target, otlp))
	}
	types := []model.MetricType{model.CounterMetricType, model.CounterMetricType, model.GaugeMetricType, model.HistogramMetricType, model.SummaryMetricType}
	series := func(m model.Metric) {
		r.series = append(r.series, m)
		r.counts = append(r.counts, float64(r.random.IntN(50)))
	}
	// The type of each name of a run of one kind: m_total is the counter
	// the series made below are.
	kinds := map[string]model.MetricType{"m_total": model.CounterMetricType}
	for range 6 + r.random.IntN(8) {
		m := model.Metric{Name: pick(r.random, p.names), Type: types[r.random.IntN(len(types))], Help: "A generated series.", Labels: pairs(p.labels, values)}
		if kind, named := kinds[m.Name]; oneKind && named {
			m.Type = kind
		}
		kinds[m.Name] = m.Type
		if len(m.Labels) == 0 && r.random.IntN(2) == 0 {
			m.Labels = nil
		}
		series(m)
	}
	if plain {
		return r.withNUL()
	}
	// Few subjects drawn so are joined alike, so some are made to be: a
	// series of another's first two labels as one; a resource of another's
	// attributes joined otherwise; and a resource and a series whose keys
	// were joined to the bytes those of another resource and series were.
	if m := r.series[r.random.IntN(len(r.series))]; r.random.IntN(2) == 0 {
		if other, has := (keySubject{head: []string{m.Name}, pairs: m.Labels}).joinedAlike(0); has {
			m.Labels = other.pairs
			series(m)
		}
	}
	if of := r.resources[r.random.IntN(len(r.resources))]; r.random.IntN(2) == 0 {
		other, has := keySubject{head: []string{of.ServiceName}, pairs: of.Attributes}.joinedAlike(r.random.IntN(3))
		if has && !slices.Contains(slices.Collect(maps.Values(other.pairs)), "") {
			r.resources = append(r.resources, otlpResourceIdentity{ServiceName: other.head[0], Attributes: other.pairs})
		}
	}
	if r.random.IntN(3) == 0 {
		target := &model.StaticTarget{}
		if names := model.SortedKeys(r.resources[0].Attributes); len(names) > 0 {
			last := names[len(names)-1]
			target.OTLP.ResourceAttributes = map[string]string{last: r.resources[0].Attributes[last] + "\x00m_total\x00counter\x00l=v"}
		} else {
			target.OTLP.ServiceName = r.resources[0].ServiceName + "\x00m_total\x00counter\x00l=v"
		}
		r.resources = append(r.resources, targetResource(target, otlp))
		series(model.Metric{Name: "m_total", Type: model.CounterMetricType, Labels: map[string]string{"l": "v\x00m_total\x00counter"}})
		series(model.Metric{Name: "m_total", Type: model.CounterMetricType})
	}
	return r.withNUL()
}

// withNUL is r, having noted whether a value of its resources or of its
// series holds a NUL.
func (r *keyRun) withNUL() *keyRun {
	for _, resource := range r.resources {
		for _, value := range resource.Attributes {
			r.nul = r.nul || strings.Contains(value, "\x00")
		}
	}
	for _, m := range r.series {
		for _, value := range m.Labels {
			r.nul = r.nul || strings.Contains(value, "\x00")
		}
	}
	return r
}

// scrape is some of the run's series as a scrape at the millisecond at
// finds them: each grown since it was last seen, or now and then reset.
func (r *keyRun) scrape(at int64) model.MetricSet {
	var set model.MetricSet
	for range 1 + r.random.IntN(6) {
		i := r.random.IntN(len(r.series))
		if r.random.IntN(4) == 0 {
			r.counts[i] = float64(r.random.IntN(int(r.counts[i]) + 1))
		} else {
			r.counts[i] += float64(r.random.IntN(9))
		}
		m, count := r.series[i], r.counts[i]
		when := at + int64(len(set.Metrics))
		m.Timestamp = &when
		switch m.Type {
		case model.HistogramMetricType:
			m.Histogram = &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: uint64(count / 2)}, {UpperBound: math.Inf(1), CumulativeCount: uint64(count)}}, Sum: count * 3, Count: uint64(count)}
		case model.SummaryMetricType:
			m.Summary = &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: count * 2, Count: uint64(count)}
		default:
			m.Value = count
		}
		set.Metrics = append(set.Metrics, m)
	}
	return set
}

// Runs of exports — several resources, the exporter's own and static
// targets', each queued scrapes of counters, gauges, histograms and
// summaries whose counts rise and fall, some exports failing so that their
// points wait again — are sent as they were with the old keys, byte for
// byte: the resources, the points and their order, and every start time,
// wherever no two resources and no two series in a run had one old key,
// with NULs and = in their values or without, and no name was of two kinds
// in an export of the run. Every run, those where two had one key too, is
// sent as it is with keys that are each subject's own; and in many of
// those the old keys sent something else.
//
// In a quarter of the runs a name has series of several types, and nearly
// each of those has an export with a name of two kinds under one resource:
// it is sent as the model sends it that leaves out the series of the kinds
// written earlier, which is not what was sent while every series was a
// point.
func TestOTLPExportsAreWhatTheyWereWhereNoTwoSubjectsHadOneKey(t *testing.T) {
	runs := alloctest.UnlessRaced(400, 60)
	const rounds = 6
	apart, apartWithNUL, together, wrong, twoKinds, otherwise, exports := 0, 0, 0, 0, 0, 0, 0
	for seed := range runs {
		run := newKeyRun(uint64(seed), seed%3 == 0, seed%4 != 3)
		server, old, own := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys(), ownOTLPKeys()
		// What the run's subjects are, and what they were by the old keys.
		resources, oldResources := map[string]bool{}, map[string]bool{}
		series, oldSeries, oldStarts := map[string]bool{}, map[string]bool{}, map[string]bool{}
		queue := func(at int64) {
			for i, identity := range run.resources {
				if run.random.IntN(3) == 0 {
					continue
				}
				set := run.scrape(at + int64(100*i))
				server.queueOTLPResource(set, identity, scrapeTime{})
				old.queue(identity, set)
				own.queue(identity, set)
				resources[own.resource(identity)], oldResources[oldOTLPResourceKey(identity)] = true, true
				for _, m := range set.Metrics {
					series[own.start(own.resource(identity), m)] = true
					oldSeries[strconv.Quote(oldOTLPResourceKey(identity))+strconv.Quote(oldOTLPSeriesKey(m))] = true
					oldStarts[oldOTLPStartKey(oldOTLPResourceKey(identity), m)] = true
				}
			}
		}
		differs := false
		for round := range rounds {
			at := int64(1000 * (round + 1))
			queue(at)
			drained, oldDrained, ownDrained := server.drainOTLP(), old.drain(), own.drain()
			if len(drained) == 0 {
				continue
			}
			if round < rounds-1 && run.random.IntN(4) == 0 {
				// The export fails, a scrape is queued meanwhile, and
				// its points wait again.
				queue(at + 500)
				server.requeueOTLP(drained)
				old.requeue(oldDrained)
				own.requeue(ownDrained)
				continue
			}
			exports++
			sent, was, want := server.push(t, drained), old.export(t, oldDrained), own.export(t, ownDrained)
			if !bytes.Equal(sent, want) {
				t.Fatalf("run %d, export %d is sent as\n%q\nand with keys that are each subject's own it is\n%q", seed, round, sent, want)
			}
			differs = differs || !bytes.Equal(sent, was)
			if differs && own.leftOut == 0 && len(oldResources) == len(resources) && len(oldSeries) == len(series) && len(oldStarts) == len(series) {
				t.Fatalf("run %d, export %d is sent as\n%q\nand with the old keys, of which no two subjects had one, and every series a point, it was\n%q", seed, round, sent, was)
			}
		}
		switch {
		case len(oldResources) < len(resources) || len(oldSeries) < len(series) || len(oldStarts) < len(series):
			together++
			if differs {
				wrong++
			}
		case own.leftOut > 0:
			twoKinds++
			if differs {
				otherwise++
			}
		case run.nul:
			apartWithNUL++
			fallthrough
		default:
			apart++
		}
	}
	// What the runs are of, scaled with how many there are.
	if apart < runs/3 || apartWithNUL < runs/20 || together < runs/5 || wrong < runs/10 || twoKinds < runs/10 || otherwise < runs/10 || exports < 3*runs {
		t.Errorf("of %d runs and %d exports, in %d no two subjects had one old key (%d of them with a NUL in a value), in %d two had, and for %d of those the old keys sent something else; in %d a name was of two kinds in an export, and %d of those were sent otherwise while every series was a point", runs, exports, apart, apartWithNUL, together, wrong, twoKinds, otherwise)
	}
	t.Logf("%d runs, %d exports: %d runs without two subjects of one old key (%d with a NUL in a value), %d with, %d of them sent otherwise by the old keys; %d runs with a name of two kinds in an export, %d of them sent otherwise", runs, exports, apart, apartWithNUL, together, wrong, twoKinds, otherwise)
}
