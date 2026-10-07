package decode

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The decoder of a prometheus transform stops at the first series past
// limits.max_metrics that the transform keeps. It counted every series a
// rule matches, the ones the rule then carries on without among them, so a
// scrape of which the rules kept a few failed for the many they dropped.
// Now a family counts only when a rule is sure to make a series of each of
// its series (prometheusCounts), and the others are kept without counting,
// for the transform to count what it makes of them. These tests hold the
// two halves of that apart: which rule is sure to, for a metric of each
// type, and what the parser does with a family that does not count,
// against the parser it was (promoracle_test.go).

// Which series of a prometheus transform count while they are decoded is
// asked of the rules that match their metric's name, by its type: a rule
// that fails the scrape on a series it cannot make counts every series, and
// one that carries on counts them unless its type or its scale cannot
// apply to the metric, or it requires a label. One rule that keeps every
// series is enough, whatever the others do.
func TestWhichSeriesOfAPrometheusTransformCountWhileTheyAreDecoded(t *testing.T) {
	two := 2.0
	required := []model.LabelRule{{Name: "site", Expression: "dc", Required: true}}
	optional := []model.LabelRule{{Name: "site", Expression: "dc"}, {Name: "env", Value: "prod"}}
	every := []model.MetricType{model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType, model.HistogramMetricType, model.SummaryMetricType}
	plain := every[:3]
	for _, tc := range []struct {
		name string
		rule model.MetricRule
		// counted are the types of metric the rule is sure to keep under
		// log and ignore; under fail it keeps, or fails the scrape on,
		// every one.
		counted []model.MetricType
	}{
		{"a rule that sets nothing", model.MetricRule{}, every},
		{"labels none of which is required", model.MetricRule{Labels: optional}, every},
		{"a type no histogram or summary can take", model.MetricRule{Type: model.GaugeMetricType}, plain},
		{"the type of a counter", model.MetricRule{Type: model.CounterMetricType}, plain},
		{"the type of a histogram", model.MetricRule{Type: model.HistogramMetricType}, []model.MetricType{model.HistogramMetricType}},
		{"the type of a summary", model.MetricRule{Type: model.SummaryMetricType}, []model.MetricType{model.SummaryMetricType}},
		{"a scale", model.MetricRule{Scale: &two}, plain},
		{"a scale and the type of a histogram", model.MetricRule{Scale: &two, Type: model.HistogramMetricType}, nil},
		{"a required label", model.MetricRule{Labels: required}, nil},
		{"a required label and a type", model.MetricRule{Labels: required, Type: model.GaugeMetricType}, nil},
	} {
		for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail, ""} {
			rule := tc.rule
			rule.Expression, rule.ErrorMode = "^m", mode
			carriesOn := mode == model.ErrorModeLog || mode == model.ErrorModeIgnore
			c := &model.Collector{Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{rule}}
			counts := prometheusCounts(c)
			if all := !carriesOn || len(tc.counted) == len(every); (counts == nil) != all {
				t.Errorf("%s under %q: every series counts is %v, want %v", tc.name, mode, counts == nil, all)
				continue
			}
			for _, typ := range every {
				want := !carriesOn
				for _, counted := range tc.counted {
					want = want || counted == typ
				}
				if got := prometheusRuleKeeps(&rule, typ); got != want {
					t.Errorf("%s under %q: the rule is sure to keep a %s is %v, want %v", tc.name, mode, typ, got, want)
				}
				if counts != nil && counts("m_seconds", typ) != want {
					t.Errorf("%s under %q: a %s m_seconds counts is %v, want %v", tc.name, mode, typ, counts("m_seconds", typ), want)
				}
			}
			// Beside a rule that keeps every series of the metrics it
			// matches, those count, and no other metric the first matches.
			c.Metrics = append(c.Metrics, model.MetricRule{Name: "m_seconds", ErrorMode: mode})
			if counts = prometheusCounts(c); counts != nil {
				for _, typ := range every {
					if !counts("m_seconds", typ) {
						t.Errorf("%s under %q, beside a rule that keeps every m_seconds: a %s m_seconds does not count", tc.name, mode, typ)
					}
					if got, want := counts("m_other", typ), prometheusRuleKeeps(&rule, typ); got != want {
						t.Errorf("%s under %q, beside a rule of another metric: a %s m_other counts is %v, want %v", tc.name, mode, typ, got, want)
					}
				}
			}
		}
	}
	// Without rules nothing fails on a series, and a rule whose expression
	// does not compile leaves everything to the transform, as it did.
	for name, c := range map[string]*model.Collector{
		"no rules":            {Transform: model.TransformConfig{Type: "prometheus", Include: []string{"^m"}}},
		"a broken expression": {Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{{Expression: "(", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeLog}}},
	} {
		if prometheusCounts(c) != nil {
			t.Errorf("%s: some series do not count", name)
		}
	}
}

// lineStoppedIn is the line a parse stopped in with an error of that line,
// which the error names first.
func lineStoppedIn(err error) (line int, ok bool) {
	if err == nil {
		return 0, false
	}
	const stopped = "text format parsing error in line "
	text, found := strings.CutPrefix(err.Error(), stopped)
	if !found {
		return 0, false
	}
	digits, _, _ := strings.Cut(text, ":")
	line, convErr := strconv.Atoi(digits)
	return line, convErr == nil
}

// linesBefore is body up to its line of that number, the first being 1.
func linesBefore(body []byte, line int) []byte {
	at := 0
	for ; line > 1; line-- {
		end := bytes.IndexByte(body[at:], '\n')
		if end < 0 {
			return body
		}
		at += end + 1
	}
	return body[:at]
}

// The parser stops at the first series past the limit among those that
// count, and keeps the others whatever the limit: for thousands of random
// expositions and a corruption of each, read with a random choice of which
// names are kept and which of them count, it returns what the parser it
// was returns when that one is told to stop for the counted families alone
// and, where it does not stop, what it returns without a limit: the same
// series in the same order, or the same error. A family that does not
// count is still kept, so what is wrong with a line of it is still said,
// which the parser it was, not keeping the family, passed over: where the
// body has such a line before the place it stops for the limit, that
// line's error is what is wanted. So nothing but the count moved: a
// family that does not count is parsed, checked and kept as any other.
// Many of the readings are of bodies the plain limit refused and this one
// takes, and many of bodies both refuse. Under the race detector the
// expositions are a twelfth as many, drawn as the first twelfth is.
func TestThePromParserStopsOnlyForTheSeriesThatCount(t *testing.T) {
	g := &promGenerator{random: rand.New(rand.NewPCG(2026, 1006))}
	expositions := alloctest.UnlessRaced(6000, 500)
	spared, stopped, taken := 0, 0, 0
	compare := func(body []byte) {
		t.Helper()
		// Names are kept, and counted, by a letter they have, and a
		// histogram or a summary may count where another metric does not.
		kept, counted, shaped := g.pick("", "a", "e", "s"), g.pick("", "a", "h", "t", "_"), g.random.IntN(3)
		openMetrics, limit := g.random.IntN(2) == 0, 1+g.random.IntN(4)
		keeps := func(name string) bool { return strings.Contains(name, kept) }
		counts := func(name string, typ model.MetricType) bool {
			isShaped := typ == model.HistogramMetricType || typ == model.SummaryMetricType
			return strings.Contains(name, counted) && (shaped == 0 || (shaped == 1) == isShaped)
		}
		where := fmt.Sprintf("%q (OpenMetrics %v, limit %d, kept by %q, counted by %q and %d)", clip(body), openMetrics, limit, kept, counted, shaped)
		// The types the parser gave the families it asked about, for the
		// parser it was to be asked about the same families.
		types := map[string]model.MetricType{}
		got, err := parseExposition(bytes.Clone(body), promOptions{openMetrics: openMetrics, keep: keeps, limit: limit, counts: func(name string, typ model.MetricType) bool {
			types[name] = typ
			return counts(name, typ)
		}})
		empty := mayHoldAnEmptyLabel(body)
		// The parser it was, stopping for the counted families alone.
		countedAlone := promOptions{openMetrics: openMetrics, limit: limit, keep: func(name string) bool {
			typ, asked := types[name]
			return keeps(name) && asked && counts(name, typ)
		}}
		old, oldErr := oracleParse(bytes.Clone(body), promOptions{openMetrics: openMetrics, keep: keeps}, empty)
		// Up to the line that is wrong, if one is: what stops the parse
		// first is the limit there or that line.
		upTo := body
		if line, ok := lineStoppedIn(oldErr); ok {
			upTo = linesBefore(body, line)
		}
		if _, countedErr := oracleParse(bytes.Clone(upTo), countedAlone, empty); errors.Is(countedErr, model.ErrLimitExceeded) {
			stopped++
			if err == nil || err.Error() != countedErr.Error() || !errors.Is(err, model.ErrLimitExceeded) {
				t.Errorf("%s: err=%v, want the limit's error %v", where, err, countedErr)
			}
			return
		}
		switch {
		case (err == nil) != (oldErr == nil) || err != nil && !isCutOf(err.Error(), oldErr.Error()):
			t.Errorf("%s: err=%v, and without a limit it was %v", where, err, oldErr)
		case err == nil:
			if diff := sameMetrics(old, got); diff != "" {
				t.Errorf("%s: %s", where, diff)
			}
			taken++
			if _, plainErr := oracleParse(bytes.Clone(body), promOptions{openMetrics: openMetrics, keep: keeps, limit: limit}, empty); errors.Is(plainErr, model.ErrLimitExceeded) {
				spared++
			}
		}
	}
	for i := 0; i < expositions && !t.Failed(); i++ {
		body := g.exposition()
		compare(body)
		compare(g.corrupt(body))
	}
	if spared < expositions/20 || stopped < expositions/20 || taken < expositions/2 {
		t.Fatalf("of %d expositions and their corruptions, %d are taken, %d of them past the plain limit, and %d refused for the limit: the generator should write many of each", expositions, taken, spared, stopped)
	}
	t.Logf("%d expositions and a corruption of each: %d taken, %d of them refused by a limit that counts every kept series, %d refused for the series that count", expositions, taken, spared, stopped)
}
