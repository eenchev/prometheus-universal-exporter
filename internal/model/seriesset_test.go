package model

import (
	"bytes"
	"fmt"
	"hash/maphash"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// keyedSeriesSet is the duplicate check as it was before seriesSet hashed a
// series where it lies: it writes a key for every series, its name and its
// labels in sorted order, and tells series apart by their keys. It is kept
// here, unchanged, as what the tests below hold seriesSet against. A key is
// the series exactly as long as no label name holds a "=" and nothing holds a
// 0xff byte, which is so for every set that reaches the check in Validate:
// label names are classic names by then, and values valid UTF-8.
type keyedSeriesSet struct {
	metrics    []Metric
	hash       func([]byte) uint64
	byHash     map[uint64]int
	others     map[string]bool
	key, other []byte
	names      []string
}

func newKeyedSeriesSet(metrics []Metric) *keyedSeriesSet {
	seed := maphash.MakeSeed()
	return &keyedSeriesSet{
		metrics: metrics,
		hash:    func(key []byte) uint64 { return maphash.Bytes(seed, key) },
		byHash:  make(map[uint64]int, len(metrics)),
	}
}

func appendSeriesKey(m *Metric, key []byte, names []string) ([]byte, []string, bool) {
	names = names[:0]
	empty := false
	for k, v := range m.Labels {
		if v == "" {
			empty = true
			continue
		}
		names = append(names, k)
	}
	slices.Sort(names)
	key = append(key, m.Name...)
	for _, k := range names {
		key = append(key, '\xff')
		key = append(key, k...)
		key = append(key, '=')
		key = append(key, m.Labels[k]...)
	}
	return key, names, empty
}

func (s *keyedSeriesSet) add(i int) (duplicate, empty bool) {
	s.key, s.names, empty = appendSeriesKey(&s.metrics[i], s.key[:0], s.names)
	h := s.hash(s.key)
	first, taken := s.byHash[h]
	if !taken {
		s.byHash[h] = i
		return false, false
	}
	var firstEmpty bool
	s.other, s.names, firstEmpty = appendSeriesKey(&s.metrics[first], s.other[:0], s.names)
	if bytes.Equal(s.key, s.other) {
		return true, empty || firstEmpty
	}
	if earlierEmpty, seen := s.others[string(s.key)]; seen {
		return true, empty || earlierEmpty
	}
	if s.others == nil {
		s.others = map[string]bool{}
	}
	s.others[string(s.key)] = empty
	return false, false
}

// keyedValidateError is the error Validate gave for a duplicate when it
// checked by key: that of the first series the keyed set calls a duplicate,
// or none.
func keyedValidateError(metrics []Metric) string {
	set := newKeyedSeriesSet(metrics)
	for i := range metrics {
		switch duplicate, empty := set.add(i); {
		case duplicate && empty:
			return fmt.Sprintf("duplicate metric series %q: Prometheus reads a label with an empty value as no label, so series that differ only in one are the same series", metrics[i].Name)
		case duplicate:
			return fmt.Sprintf("duplicate metric series %q", metrics[i].Name)
		}
	}
	return ""
}

// weakSeriesHashes are hashes a test gives a seriesSet in place of its own,
// from one that tells no two series apart to ones that tell a few: under each
// of them different series share a hash all the time, which with the real one
// they never do in a test.
var weakSeriesHashes = map[string]func([]byte) uint64{
	"one hash for all": func([]byte) uint64 { return 7 },
	"two hashes":       func(digest []byte) uint64 { return uint64(digest[8] & 1) },
	"by name alone":    func(digest []byte) uint64 { return uint64(digest[0]) },
	"by labels alone":  func(digest []byte) uint64 { return uint64(digest[8] & 7) },
}

// checkSeriesSetAgainstKeys holds seriesSet against the keyed check for one
// set of series: the same verdict for every series, the set gone through to
// its end, with the real hash and with every weak one, and the same error
// from Validate. It returns how many verdicts it compared.
func checkSeriesSetAgainstKeys(t *testing.T, metrics []Metric) int {
	t.Helper()
	type verdict struct{ duplicate, empty bool }
	keyed := newKeyedSeriesSet(metrics)
	want := make([]verdict, len(metrics))
	for i := range metrics {
		want[i].duplicate, want[i].empty = keyed.add(i)
	}
	compared := 0
	check := func(hashName string, hash func([]byte) uint64) {
		set := newSeriesSet(metrics)
		if hash != nil {
			set.hash = hash
		}
		for i := range metrics {
			if duplicate, empty := set.add(i); duplicate != want[i].duplicate || empty != want[i].empty {
				t.Errorf("%s: series %d of %s: duplicate=%v empty=%v, by key %v %v", hashName, i, showSeries(metrics), duplicate, empty, want[i].duplicate, want[i].empty)
			}
			compared++
		}
		// Validate itself, which hashes the labels as it checks them.
		set = newSeriesSet(metrics)
		if hash != nil {
			set.hash = hash
		}
		got := ""
		if err := (&MetricSet{Metrics: metrics}).validate(Limits{}, set); err != nil {
			got = err.Error()
		}
		if wantError := keyedValidateError(metrics); got != wantError && validSeriesNames(metrics) {
			t.Errorf("%s: Validate of %s: %q, by key %q", hashName, showSeries(metrics), got, wantError)
		}
	}
	check("the real hash", nil)
	for name, hash := range weakSeriesHashes {
		check(name, hash)
	}
	return compared
}

// validSeriesNames reports whether Validate gets as far as the duplicate
// check for every series: names it refuses are refused before it.
func validSeriesNames(metrics []Metric) bool {
	for _, m := range metrics {
		if !ValidMetricName(m.Name) {
			return false
		}
		for k := range m.Labels {
			if CheckLabelName(k) != nil {
				return false
			}
		}
	}
	return true
}

func showSeries(metrics []Metric) string {
	var b strings.Builder
	for i, m := range metrics {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s{", m.Name)
		for j, k := range SortedKeys(m.Labels) {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%s=%q", k, m.Labels[k])
		}
		b.WriteString("}")
	}
	return b.String()
}

func gauge(name string, labels ...string) Metric {
	m := Metric{Name: name, Type: GaugeMetricType}
	for i := 0; i+1 < len(labels); i += 2 {
		if m.Labels == nil {
			m.Labels = map[string]string{}
		}
		m.Labels[labels[i]] = labels[i+1]
	}
	return m
}

// The duplicate check by hash and comparison gives the verdicts the check by
// key gave, series by series, and Validate the same error: for the same
// series twice, series that differ in one label's value or in having a label,
// labels with empty values on either or both, labels written in another
// order, text beyond ASCII, a label set of one being a part of another's, a
// name and a value changing places, and none of these.
func TestSeriesSetAgreesWithTheKeyedCheckOnATable(t *testing.T) {
	for name, metrics := range map[string][]Metric{
		"no series":           nil,
		"one series":          {gauge("a")},
		"the same twice":      {gauge("a"), gauge("a")},
		"the same labelled":   {gauge("a", "x", "1"), gauge("a", "x", "1")},
		"another value":       {gauge("a", "x", "1"), gauge("a", "x", "2")},
		"another label":       {gauge("a", "x", "1"), gauge("a", "y", "1")},
		"another name":        {gauge("a", "x", "1"), gauge("b", "x", "1")},
		"a label more":        {gauge("a", "x", "1"), gauge("a", "x", "1", "y", "2")},
		"a label fewer":       {gauge("a", "x", "1", "y", "2"), gauge("a", "x", "1")},
		"an empty label":      {gauge("a"), gauge("a", "x", "")},
		"an empty one first":  {gauge("a", "x", ""), gauge("a")},
		"empty on both":       {gauge("a", "x", ""), gauge("a", "y", "")},
		"the same empty one":  {gauge("a", "x", "", "y", "1"), gauge("a", "x", "", "y", "1")},
		"empty, not the same": {gauge("a", "x", "", "y", "1"), gauge("a", "y", "2")},
		"empty against value": {gauge("a", "x", ""), gauge("a", "x", "1")},
		"another order":       {gauge("a", "x", "1", "y", "2", "z", "3"), gauge("a", "z", "3", "y", "2", "x", "1")},
		"name and value swap": {gauge("a", "x", "y"), gauge("a", "y", "x")},
		"values swapped":      {gauge("a", "x", "1", "y", "2"), gauge("a", "x", "2", "y", "1")},
		"values joined":       {gauge("a", "x", "12", "y", "3"), gauge("a", "x", "1", "y", "23")},
		"a name in a value":   {gauge("a", "x", "1,y=2"), gauge("a", "x", "1", "y", "2")},
		"name against label":  {gauge("ax", "y", "1"), gauge("a", "xy", "1")},
		"beyond ASCII":        {gauge("a", "x", "é"), gauge("a", "x", "é"), gauge("a", "x", "e\u0301")},
		"a third time":        {gauge("a", "x", "1"), gauge("a", "x", "2"), gauge("a", "x", "1"), gauge("a", "x", "2"), gauge("a", "x", "1", "y", "")},
		"a duplicate late": {
			gauge("a", "x", "1"), gauge("a", "x", "2"), gauge("b"), gauge("a", "x", "2"),
			gauge("b", "y", ""), gauge("a", "x", "1"), gauge("a", "x", "3"),
		},
	} {
		t.Run(name, func(t *testing.T) { checkSeriesSetAgainstKeys(t, metrics) })
	}
}

// randomSeries makes a set of series out of a few names, label names and
// values, so that series meet, and in three sets of four some series are an
// earlier series again, as it was, with one value changed, with a label dropped, or with a
// label of an empty value added, and the labels of a series are written into
// its map in an order of their own. One set in eight has series of dozens of
// labels.
func randomSeries(r *rand.Rand) []Metric {
	names := []string{"up", "node_cpu_seconds_total", "a", "a_b"}[:1+r.IntN(4)]
	// How often a series is an earlier one again: in one set of four
	// never, so that sets without a duplicate are common too.
	again := r.IntN(4)
	labelNames := []string{"id", "name", "mode", "x", "y", "region", "le", "quantile"}
	if r.IntN(8) == 0 {
		for i := 0; i < 40; i++ {
			labelNames = append(labelNames, fmt.Sprintf("label_%d", i))
		}
	} else {
		labelNames = labelNames[:3+r.IntN(len(labelNames)-2)]
	}
	values := []string{"", "1", "2", "é", "e\u0301", "日本語", "a=b", "1,x=2", " ", "\U0001F600", strings.Repeat("long ", 30)}[:4+r.IntN(8)]
	labelled := func() Metric {
		m := Metric{Name: names[r.IntN(len(names))], Type: GaugeMetricType}
		picked := r.Perm(len(labelNames))[:r.IntN(len(labelNames)+1)]
		for _, k := range picked {
			if m.Labels == nil {
				m.Labels = map[string]string{}
			}
			m.Labels[labelNames[k]] = values[r.IntN(len(values))]
		}
		return m
	}
	metrics := make([]Metric, 0, 40)
	for n := 1 + r.IntN(40); len(metrics) < n; {
		if len(metrics) == 0 || r.IntN(12) >= again {
			metrics = append(metrics, labelled())
			continue
		}
		earlier := metrics[r.IntN(len(metrics))]
		m := Metric{Name: earlier.Name, Type: GaugeMetricType}
		keys := SortedKeys(earlier.Labels)
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		change := r.IntN(4)
		for i, k := range keys {
			if m.Labels == nil {
				m.Labels = map[string]string{}
			}
			switch {
			case i == 0 && change == 1:
				m.Labels[k] = values[r.IntN(len(values))]
			case i == 0 && change == 2:
			default:
				m.Labels[k] = earlier.Labels[k]
			}
		}
		if change == 3 {
			if m.Labels == nil {
				m.Labels = map[string]string{}
			}
			if k := labelNames[r.IntN(len(labelNames))]; m.Labels[k] == "" {
				m.Labels[k] = ""
			}
		}
		metrics = append(metrics, m)
	}
	return metrics
}

// The same agreement over sets made at random from a fixed seed, so the test
// goes through the same ones every time: 3,000 sets of up to 40 series, each
// checked with the real hash and with every weak one, under which different
// series share a hash throughout.
func TestSeriesSetAgreesWithTheKeyedCheckOnRandomSets(t *testing.T) {
	r := rand.New(rand.NewPCG(20261002, 1))
	sets, duplicates, verdicts := 3000, 0, 0
	for i := 0; i < sets && !t.Failed(); i++ {
		metrics := randomSeries(r)
		if keyedValidateError(metrics) != "" {
			duplicates++
		}
		verdicts += checkSeriesSetAgainstKeys(t, metrics)
	}
	// A generator that made no duplicates, or nothing else, would show
	// nothing.
	if duplicates < sets/4 || duplicates > sets-sets/20 {
		t.Errorf("%d of the %d sets hold a duplicate, which is too few or too many to show much", duplicates, sets)
	}
	t.Logf("%d sets, %d of them with a duplicate, %d verdicts compared", sets, duplicates, verdicts)
}

func FuzzSeriesSetAgreesWithTheKeyedCheck(f *testing.F) {
	for seed := uint64(0); seed < 8; seed++ {
		f.Add(seed, seed*7)
	}
	f.Fuzz(func(t *testing.T, seed1, seed2 uint64) {
		checkSeriesSetAgainstKeys(t, randomSeries(rand.New(rand.NewPCG(seed1, seed2))))
	})
}

// A series is compared with another itself, not through a key written for
// each, so text that would read as two labels in a key is still one value:
// the keyed check took a{x="1\xffy=2"} for a{x="1",y="2"}, which the bytes of
// a value that is not valid UTF-8 could bring about, though Validate is given
// none.
func TestSeriesSetTellsApartWhatAKeyRunsTogether(t *testing.T) {
	metrics := []Metric{gauge("a", "x", "1\xffy=2"), gauge("a", "x", "1", "y", "2"), gauge("a\xffx=1"), gauge("a", "x", "1")}
	set := newSeriesSet(metrics)
	for i := range metrics {
		if duplicate, _ := set.add(i); duplicate {
			t.Errorf("series %d of %s is taken for an earlier one", i, showSeries(metrics))
		}
	}
	keyed := newKeyedSeriesSet(metrics)
	keyed.add(0)
	if duplicate, _ := keyed.add(1); !duplicate {
		t.Error("the keyed check tells the first two apart, so this test shows nothing")
	}
}

// manyLabelSeries is n series of one family, each with the given number of
// labels, whose names and values are long enough that a key written for one
// would be some hundreds of bytes.
func manyLabelSeries(n, labels int) []Metric {
	metrics := make([]Metric, n)
	for i := range metrics {
		metrics[i] = Metric{Name: "component_cpu_seconds_total", Type: GaugeMetricType, Labels: make(map[string]string, labels)}
		for j := 0; j < labels; j++ {
			metrics[i].Labels[fmt.Sprintf("label_number_%d", j)] = fmt.Sprintf("value %d of series %d", j, i)
		}
	}
	return metrics
}

// seriesMapSink keeps the map the tests below make for comparison on the
// heap, as a set's own is.
var seriesMapSink map[uint64]int

// seriesMapAllocations is how many allocations a map made for n series takes,
// which is all a set of n series may cost apart from a few of its own.
func seriesMapAllocations(n int) float64 {
	allocations, _ := alloctest.Allocations(10, func() { seriesMapSink = make(map[uint64]int, n) })
	return allocations
}

// Seeing a series allocates nothing: a set and its 2,000 series of 16 labels
// cost what the set's own map does and 2 allocations more, and none for any
// series. The keyed check wrote every series' key into a buffer and its label
// names into a slice, and grew both as it went, which was 13 allocations more
// than the map's for these series; a check that sorted or copied each series'
// labels would allocate thousands of times. The bound is between the two.
func TestSeeingASeriesAllocatesNothing(t *testing.T) {
	metrics := manyLabelSeries(2000, 16)
	got, _ := alloctest.Allocations(10, func() {
		set := newSeriesSet(metrics)
		for i := range metrics {
			if duplicate, _ := set.add(i); duplicate {
				t.Fatal("a duplicate among series that all differ")
			}
		}
	})
	if ofTheMap := seriesMapAllocations(len(metrics)); got > ofTheMap+6 {
		t.Errorf("a set of %d series allocated %v times, a map for them alone %v times: seeing a series allocates", len(metrics), got, ofTheMap)
	}
}

// Validate of one family allocates what the map of its series does and 2
// allocations more, however many series and labels it has: it goes through
// each series' labels once, hashing them as it checks them, and writes no
// key. With the keyed check it was 13 more than the map's for these series;
// the bound is between the two.
func TestValidateAllocatesNothingForASeries(t *testing.T) {
	limits := Limits{MaxMetrics: 100000, MaxLabelsPerMetric: 30, MaxLabelValueLength: 1000, MaxMetricNameLength: 200}
	for _, n := range []int{500, 4000} {
		set := &MetricSet{Metrics: manyLabelSeries(n, 16)}
		got, _ := alloctest.Allocations(10, func() {
			if err := set.Validate(limits); err != nil {
				t.Fatal(err)
			}
		})
		if ofTheMap := seriesMapAllocations(n); got > ofTheMap+6 {
			t.Errorf("Validate of %d series allocated %v times, a map for them alone %v times: a series allocates", n, got, ofTheMap)
		}
	}
}

// The type of a family is looked up when a series follows one of another
// family or type, and not for a series like the one before it; either way a
// family with two types is refused, wherever its series stand.
func TestValidateRefusesAFamilyOfTwoTypesWhereverItsSeriesStand(t *testing.T) {
	counter := func(name string, labels ...string) Metric {
		m := gauge(name, labels...)
		m.Type = CounterMetricType
		return m
	}
	for name, test := range map[string]struct {
		metrics []Metric
		want    string
	}{
		"one after the other": {[]Metric{gauge("a", "x", "1"), counter("a", "x", "2")}, `metric "a" has inconsistent types`},
		"another between":     {[]Metric{gauge("a", "x", "1"), gauge("b"), counter("a", "x", "2")}, `metric "a" has inconsistent types`},
		"after a run":         {[]Metric{gauge("a", "x", "1"), gauge("a", "x", "2"), gauge("a", "x", "3"), counter("a", "x", "4")}, `metric "a" has inconsistent types`},
		"the same type back":  {[]Metric{gauge("a", "x", "1"), counter("b"), gauge("a", "x", "2"), counter("b", "y", "1")}, ""},
		"two families":        {[]Metric{gauge("a"), counter("b"), counter("b", "x", "1"), gauge("b", "x", "2")}, `metric "b" has inconsistent types`},
	} {
		got := ""
		if err := (&MetricSet{Metrics: test.metrics}).Validate(Limits{}); err != nil {
			got = err.Error()
		}
		if got != test.want {
			t.Errorf("%s: %q, want %q", name, got, test.want)
		}
	}
}
