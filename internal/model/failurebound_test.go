package model

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A failure over the bound keeps what kind of failure it is, whichever of
// the three kinds the self-metrics count apart it was marked as, and two of
// them when it was marked as two; it keeps nothing else of the error it was
// cut from, neither that error nor what that wrapped, and is no kind the
// error was not. Its text is 2,000 bytes, the start of the error's and its
// length, and it is recognised by 2,000 bytes with the mark for the length.
// No error is no error, and an error that is a nil pointer of its type, which
// panics when it is read, is handed on as it came.
func TestAFailureOverTheBoundKeepsWhatKindOfFailureItIs(t *testing.T) {
	if BoundedFailure(nil) != nil {
		t.Error("no error is bounded to an error")
	}
	// An error that cannot be read is handed on as it came.
	var unread *movingError
	if got := BoundedFailure(unread); got != error(unread) { //nolint:errorlint // the very error
		t.Errorf("an error that is a nil pointer of its type is bounded to %v", got)
	}
	cause := errors.New("the script said so")
	long := fmt.Errorf("%s: %w", strings.Repeat("e", 3*MaxFailureBytes), cause)
	kinds := []error{ErrLimitExceeded, ErrMissingValue, ErrScriptFailed}
	for marked := range 1 << len(kinds) {
		err := long
		for i, kind := range kinds {
			if marked&(1<<i) != 0 {
				err = MarkError(err, kind)
			}
		}
		got := BoundedFailure(err)
		for i, kind := range kinds {
			if want := marked&(1<<i) != 0; errors.Is(got, kind) != want {
				t.Errorf("an error marked %03b, cut, is %v: %t, want %t", marked, kind, errors.Is(got, kind), want)
			}
		}
		mark := fmt.Sprintf("... (%d bytes)", len(long.Error()))
		if text := got.Error(); len(text) != MaxFailureBytes || text != long.Error()[:MaxFailureBytes-len(mark)]+mark {
			t.Errorf("an error marked %03b is cut to %d bytes, %.60q ... %q", marked, len(text), text, text[max(0, len(text)-40):])
		}
		if same := SameFailureText(got); same != long.Error()[:MaxFailureBytes-len("... (# bytes)")]+"... (# bytes)" {
			t.Errorf("an error marked %03b, cut, is recognised by %d bytes, %.60q ... %q", marked, len(same), same, same[max(0, len(same)-40):])
		}
		if errors.Is(got, cause) || errors.Is(got, long) {
			t.Errorf("an error marked %03b, cut, still wraps the long one", marked)
		}
	}
}

// boundedErrors are errors as the stages of a trip make them, each made of
// a part of so many bytes: an error of a text alone, one that wraps another
// with words of its own, one that names a line and a size and shows a value
// (Errorf), one given the text it is recognised by (SameFailureAs), one
// marked with its kind, and one that wraps two.
func boundedErrors(part string) []error {
	inner := errors.New("connection reset: " + part)
	return []error{
		errors.New(part),
		fmt.Errorf("HTTP request failed: %w", inner),
		Errorf("line %d: %s is %d bytes: %s", Position(len(part)), Quoted(part), Size(len(part)), part),
		Errorf("metric %q item %d: %w", "m", Position(3), inner),
		SameFailureAs(errors.New("read tcp 10.0.0.1:53412->10.0.0.2:80: "+part), "read tcp #->10.0.0.2:80: "+part),
		MarkError(fmt.Errorf("python transform: %w", inner), ErrScriptFailed),
		fmt.Errorf("%w; and then %w", inner, errors.New(part)),
		fmt.Errorf("%w (the probe ran out of its 10s budget: scrape_timeout)", Errorf("row %d: %s", Position(7), part)),
	}
}

// An error within the bound is the very error it was, whatever made it and
// whatever it wraps: BoundedFailure hands it back, and so its text, what it
// is recognised by, its type and its chain are what they were before any
// error was bounded, for errors of every length up to the bound and of
// every shape a stage makes. One over the bound is cut to 2,000 bytes or
// fewer, valid UTF-8 where the error was, that start as the error did and
// end with its length, and is recognised by 2,000 bytes or fewer that start
// as what the error was recognised by did.
func TestAnErrorWithinTheBoundIsTheErrorItWas(t *testing.T) {
	random := rand.New(rand.NewPCG(36, 1))
	alphabet := []string{"a", "é", "日", "\U0001F600", " ", `"`, "\n", "0"}
	within, over := 0, 0
	for range alloctest.UnlessRaced(1200, 300) {
		var part strings.Builder
		// Lengths gather around the bound, where the two cases meet.
		for size := random.IntN(2 * MaxFailureBytes); part.Len() < size; {
			part.WriteString(alphabet[random.IntN(len(alphabet))])
		}
		for _, err := range boundedErrors(part.String()) {
			text, same := err.Error(), SameFailureText(err)
			got := BoundedFailure(err)
			if len(text) <= MaxFailureBytes && len(same) <= MaxFailureBytes {
				within++
				if got != err { //nolint:errorlint // the very error, not one that wraps it
					t.Fatalf("an error of %d bytes, recognised by %d, is not the error it was: %T %.80q", len(text), len(same), got, got)
				}
				continue
			}
			over++
			cut, recognised := got.Error(), SameFailureText(got)
			if len(cut) > MaxFailureBytes || len(recognised) > MaxFailureBytes || !utf8.ValidString(cut) || !utf8.ValidString(recognised) {
				t.Fatalf("an error of %d bytes is cut to %d, recognised by %d: %.80q", len(text), len(cut), len(recognised), cut)
			}
			if len(text) > MaxFailureBytes {
				mark := fmt.Sprintf("... (%d bytes)", len(text))
				if !strings.HasSuffix(cut, mark) || !strings.HasPrefix(text, strings.TrimSuffix(cut, mark)) {
					t.Fatalf("an error of %d bytes is cut to %.80q ... %q", len(text), cut, cut[max(0, len(cut)-30):])
				}
			} else if cut != text {
				t.Fatalf("an error of %d bytes, within the bound, reads %.80q once its recognised text is cut", len(text), cut)
			}
			if len(same) > MaxFailureBytes {
				const mark = "... (# bytes)"
				if !strings.HasSuffix(recognised, mark) || !strings.HasPrefix(same, strings.TrimSuffix(recognised, mark)) {
					t.Fatalf("an error recognised by %d bytes is recognised, cut, by %.80q ... %q", len(same), recognised, recognised[max(0, len(recognised)-30):])
				}
			} else if recognised != same {
				t.Fatalf("an error recognised by %d bytes, within the bound, is recognised by %.80q once its text is cut", len(same), recognised)
			}
		}
	}
	if floor := alloctest.UnlessRaced(2400, 600); within < floor || over < floor {
		t.Errorf("%d errors were within the bound and %d over it, want %d of each", within, over, floor)
	}
}

// Bounding an error within the bound allocates nothing of its own. An error
// that holds its text, as one of errors.New or of fmt.Errorf does, however
// deep the chain it wraps, is bounded in no allocation at all; one that makes
// its text when it is read (Errorf) costs the reading of its text and of
// what it is recognised by, each once, and not an allocation more. Each is
// the very error it was. The last count is not taken under the race
// detector, where fmt's pool hands back only some of what it is given and
// two readings of one text allocate differently.
func TestBoundingAnErrorWithinTheBoundAllocatesNothingOfItsOwn(t *testing.T) {
	plain := errors.New("dial tcp 10.0.0.7:80: connect: connection refused")
	wrapped := fmt.Errorf("HTTP request failed: %w", fmt.Errorf("Get %q: %w", "http://db.internal/status", plain))
	marked := MarkError(wrapped, ErrLimitExceeded)
	var kept error
	for name, err := range map[string]error{"errors.New": plain, "fmt.Errorf": wrapped, "MarkError": marked} {
		if allocs := alloctest.AllocsAtMost(200, 0, func() { kept = BoundedFailure(err) }); allocs != 0 || kept != err { //nolint:errorlint // the very error
			t.Errorf("bounding an error of %s allocates %v times and gives %v, want none and the error itself", name, allocs, kept)
		}
	}
	moving := Errorf("metric %q item %d: value %s is not a number", "m", Position(7), Quoted("n/a"))
	if kept = BoundedFailure(moving); kept != moving { //nolint:errorlint // the very error
		t.Errorf("an error made by Errorf is bounded to %v", kept)
	}
	if alloctest.RaceDetector {
		return
	}
	var text, same string
	reading, _ := alloctest.Allocations(200, func() { text, same = moving.Error(), SameFailureText(moving) })
	if allocs := alloctest.AllocsAtMost(200, reading, func() { kept = BoundedFailure(moving) }); allocs > reading {
		t.Errorf("bounding an error made by Errorf allocates %v times, and reading its text and what it is recognised by %v (%q, %q)", allocs, reading, text, same)
	}
}

// Shown is a text as it is up to its limit, and past it the text's start,
// cut between two characters, and its length; it is recognised by the start
// and the mark, and holds nothing of a text it cut.
func TestShownIsATextByItsStartAndItsLength(t *testing.T) {
	for _, tc := range []struct {
		text       string
		limit      int
		said, same string
	}{
		{"", 8, "", ""},
		{"http://a/b", 10, "http://a/b", "http://a/b"},
		{"http://a/bc", 10, "http://a/b... (11 bytes)", "http://a/b... (# bytes)"},
		{"aaaaaaaaé", 9, "aaaaaaaa... (10 bytes)", "aaaaaaaa... (# bytes)"},
		{"aaaaaaa日本", 9, "aaaaaaa... (13 bytes)", "aaaaaaa... (# bytes)"},
		{"aaaaaa\U0001F600b", 9, "aaaaaa... (11 bytes)", "aaaaaa... (# bytes)"},
		{strings.Repeat("\x80", 20), 9, strings.Repeat("\x80", 9) + "... (20 bytes)", strings.Repeat("\x80", 9) + "... (# bytes)"},
	} {
		shown := Shown(tc.text, tc.limit)
		if shown.String() != tc.said || shown.Same() != tc.same {
			t.Errorf("Shown(%q, %d) is %q, recognised by %q, want %q and %q", tc.text, tc.limit, shown, shown.Same(), tc.said, tc.same)
		}
		if err := Errorf("Get %s: refused", shown); err.Error() != "Get "+tc.said+": refused" || SameFailureText(err) != "Get "+tc.same+": refused" {
			t.Errorf("an error of Shown(%q, %d) reads %q, recognised by %q", tc.text, tc.limit, err, SameFailureText(err))
		}
	}
}

// validateOracle is validate's and labelFailure's wording of a set that
// cannot be exposed as it was before a name was shown by its start: each
// name quoted whole with %q, whatever its length. It words the failures
// whose text names the metric or a label; ok is false for a set it has no
// wording for.
func validateOracle(m *Metric, l Limits, duplicate bool) (text string, ok bool) {
	switch {
	case !ValidMetricName(m.Name) && m.Name != "" && utf8.ValidString(m.Name):
		return fmt.Sprintf("metric name %q is not a classic Prometheus name; set the collector's name_escaping to underscores or values to export it escaped", m.Name), true
	case !ValidMetricName(m.Name):
		return fmt.Sprintf("invalid metric name %q", m.Name), true
	case len(m.Name) > l.MaxMetricNameLength && l.MaxMetricNameLength > 0:
		return fmt.Sprintf("invalid metric name %q: longer than limits.max_metric_name_length %d", m.Name, l.MaxMetricNameLength), true
	case m.Type == "nope":
		return fmt.Sprintf("metric %q has invalid type %q", m.Name, m.Type), true
	case len(m.Labels) > l.MaxLabelsPerMetric && l.MaxLabelsPerMetric > 0:
		return fmt.Sprintf("metric %q has %d labels, more than limits.max_labels_per_metric %d; drop labels it does not need or raise limits.max_labels_per_metric", m.Name, len(m.Labels), l.MaxLabelsPerMetric), true
	}
	for k, v := range m.Labels {
		switch {
		case !ValidLabelName(k) && k != "" && utf8.ValidString(k):
			return fmt.Sprintf("metric %q has label %q, which is not a classic Prometheus label name; set the collector's name_escaping to underscores or values to export it escaped", m.Name, k), true
		case !ValidLabelName(k):
			return fmt.Sprintf("metric %q has invalid label name %q", m.Name, k), true
		case l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength:
			return fmt.Sprintf("metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length", m.Name, k, len(v), l.MaxLabelValueLength), true
		}
	}
	if l.MaxHelpLength > 0 && len(m.Help) > l.MaxHelpLength {
		return fmt.Sprintf("metric %q help is %d bytes, longer than limits.max_help_length %d; shorten it or raise limits.max_help_length", m.Name, len(m.Help), l.MaxHelpLength), true
	}
	if duplicate {
		return fmt.Sprintf("duplicate metric series %q", m.Name), true
	}
	return "", false
}

// A set that cannot be exposed is refused in the words it was, to the
// letter, while the names its error quotes are no longer than 200 bytes, the
// default limits.max_metric_name_length: for generated series with a name or
// a label's name that is no name, or too long, or with too many labels, a
// value or a help too long, or there twice, the error is what quoting each
// name whole gave (validateOracle). A name past 200 bytes is quoted by its
// first 200 and its length, and the error goes on to say what it said after
// the name: which limit the name is over, what is wrong with the label. Such
// an error is recognised without the length, so the same series with a
// longer name that starts alike is one failure to the log.
func TestASetThatCannotBeExposedNamesALongNameByItsStart(t *testing.T) {
	random := rand.New(rand.NewPCG(36, 2))
	name := func(length int, valid bool) string {
		if !valid {
			return "bad name " + strings.Repeat("é", length/2)
		}
		return "m" + strings.Repeat("a", max(length-1, 0))
	}
	as, cut := 0, 0
	for range alloctest.UnlessRaced(3000, 500) {
		l := Limits{MaxMetrics: 10, MaxLabelsPerMetric: 3, MaxLabelValueLength: 50, MaxMetricNameLength: []int{0, 50, 200, 1000}[random.IntN(4)], MaxHelpLength: 60}
		lengths := []int{1, 30, 199, 200, 201, 260, 5000}
		m := Metric{Name: name(lengths[random.IntN(len(lengths))], random.IntN(3) > 0), Type: GaugeMetricType, Value: 1, Labels: map[string]string{}}
		if random.IntN(3) == 0 {
			m.Labels[name(lengths[random.IntN(len(lengths))], random.IntN(2) == 0)] = strings.Repeat("v", random.IntN(100))
		}
		switch random.IntN(6) {
		case 0:
			m.Type = "nope"
		case 1:
			m.Labels["a"], m.Labels["b"], m.Labels["c"], m.Labels["d"] = "1", "2", "3", "4"
		case 2:
			m.Help = strings.Repeat("h", 61)
		}
		duplicate := random.IntN(4) == 0
		set := MetricSet{Metrics: []Metric{m}}
		if duplicate {
			set.Metrics = append(set.Metrics, m)
		}
		want, known := validateOracle(&m, l, duplicate)
		err := set.Validate(l)
		if !known {
			if err != nil {
				t.Fatalf("a set the oracle exposes is refused: %.200v", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("a set the oracle refuses with %.200q is exposed", want)
		}
		// The names the oracle's error quotes, in the order it quotes them.
		long := false
		expected := want
		for _, quoted := range append([]string{m.Name}, labelNames(m.Labels)...) {
			if len(quoted) > maxShownName && strings.Contains(want, fmt.Sprintf("%q", quoted)) {
				long = true
				head := quoted[:HeadOf(quoted, maxShownName)]
				expected = strings.Replace(expected, fmt.Sprintf("%q", quoted), fmt.Sprintf("%q... (%d bytes)", head, len(quoted)), 1)
			}
		}
		if !long {
			as++
			if err.Error() != want {
				t.Fatalf("the set is refused with %q, and was with %q", err, want)
			}
			continue
		}
		cut++
		if err.Error() != expected || len(err.Error()) > 1000 {
			t.Fatalf("the set is refused with %d bytes, %q, want %q", len(err.Error()), err, expected)
		}
		if same := SameFailureText(err); strings.Contains(same, fmt.Sprintf("(%d bytes)", len(m.Name))) && len(m.Name) > maxShownName || !strings.Contains(same, "... (# bytes)") {
			t.Fatalf("the failure is recognised by %q, want the mark in place of a long name's length", same)
		}
	}
	if floor := alloctest.UnlessRaced(400, 60); as < floor || cut < floor {
		t.Errorf("%d sets were refused as they were and %d with a name by its start, want %d of each", as, cut, floor)
	}
	// What the error says after the name, of a name as long as a response.
	huge := MetricSet{Metrics: []Metric{{Name: "m" + strings.Repeat("a", alloctest.UnlessRaced(10<<20, 1<<20)), Type: GaugeMetricType}}}
	err := huge.Validate(Limits{MaxMetricNameLength: 200})
	if want := fmt.Sprintf(`invalid metric name "m%s"... (%d bytes): longer than limits.max_metric_name_length 200`, strings.Repeat("a", 199), len(huge.Metrics[0].Name)); err == nil || err.Error() != want {
		t.Errorf("a name of %d bytes is refused with %.300q, want %q", len(huge.Metrics[0].Name), fmt.Sprint(err), want)
	}
}

// labelNames are the names of labels, of which the tests here give a series
// one that the error may name.
func labelNames(labels map[string]string) []string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		if len(name) > 1 {
			names = append(names, name)
		}
	}
	return names
}

// The error of a set refused for a name within 200 bytes costs what it did:
// it is the error fmt.Errorf makes, recognised by its text, and it is made
// in as many allocations as fmt.Errorf made the error in before a name was
// shown by its start, for a name and for a label's name beside it. The
// allocations are not counted under the race detector, where fmt's pool
// hands back only some of what it is given and the same call allocates now
// more and now less.
func TestTheErrorOfANameWithinTheBoundCostsWhatItDid(t *testing.T) {
	name, label, limit := strings.Repeat("n", 150), strings.Repeat("l", 60), 1000
	kept := nameErrorf("invalid metric name %q: longer than limits.max_metric_name_length %d", shownName(name), limit)
	if want := fmt.Sprintf("invalid metric name %q: longer than limits.max_metric_name_length 1000", name); kept.Error() != want || SameFailureText(kept) != want || fmt.Sprintf("%T", kept) != fmt.Sprintf("%T", fmt.Errorf("%d", limit)) {
		t.Errorf("the error of a metric's name is %T %q", kept, kept)
	}
	if alloctest.RaceDetector {
		return
	}
	was, _ := alloctest.Allocations(200, func() {
		kept = fmt.Errorf("invalid metric name %q: longer than limits.max_metric_name_length %d", name, limit)
	})
	if now := alloctest.AllocsAtMost(200, was, func() {
		kept = nameErrorf("invalid metric name %q: longer than limits.max_metric_name_length %d", shownName(name), limit)
	}); now > was {
		t.Errorf("the error of a metric's name is made in %v allocations, and was in %v", now, was)
	}
	was, _ = alloctest.Allocations(200, func() {
		kept = fmt.Errorf("metric %q has invalid label name %q", name, label)
	})
	if now := alloctest.AllocsAtMost(200, was, func() {
		kept = nameErrorf("metric %q has invalid label name %q", shownName(name), shownName(label))
	}); now > was {
		t.Errorf("the error of a label's name is made in %v allocations, and was in %v", now, was)
	}
}
