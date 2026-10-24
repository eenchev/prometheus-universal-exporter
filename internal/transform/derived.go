package transform

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's derived list computes series from the series its own rules
// made, so that every transform can export what only arithmetic over its
// values gives, such as a cache's hit ratio from its hits and misses:
//
//	derived:
//	  - name: memcached_gets_total
//	    hidden: true
//	    sum: [memcached_get_hits_total, memcached_get_misses_total]
//	  - name: memcached_get_hit_ratio
//	    ratio: [memcached_get_hits_total, memcached_gets_total]
//
// Derivation reads the series as the transform made them: after the value
// maps and scale of the rules, the label value maps and truncation, and
// before transform.labels, remove_labels, rename_labels, metrics_prefix and
// name escaping, which then apply to the derived series as to every other
// (Transform). So an operand is written as the rule's name is written,
// without the prefix, and a label of by as the rule's label is named, before
// any rename; and a derived series gets the collector-wide labels without
// carrying them through the arithmetic.
//
// An entry sets one operation:
//
//   - sum of two families or more adds up their series, matched by their
//     label sets; with by, of exactly one family, it adds up that family's
//     series into one for each set of values of the labels by keeps, and
//     into one series when by is [];
//   - difference [a, b] is a - b, and ratio [a, b] is a / b, the series of
//     the two matched by their label sets.
//
// Matching is PromQL's one-to-one matching without on or ignoring: series of
// the operands are matched when their labels are the same, names and
// values, a label of an empty value being no label. A series with no
// partner in every other operand makes no derived series, nor does a label
// set that two series of one operand have, which has no one partner. A
// result that is NaN or infinite — a ratio over 0 among them — makes no
// series either: no number is better than one no query can use, and the
// target answered every value it was asked for, so it is no failure.
//
// Entries are computed in their order, and an entry may read an earlier
// one, hidden or not. A hidden entry is computed for later entries to read
// and is not exported, which is what a hit ratio of hits / (hits + misses)
// needs without exporting the sum.
//
// Exported derived series count against limits.max_metrics as every series
// does, and a scrape that makes more fails as one whose rules make more
// does. A derived series has no timestamp, whatever its operands had, and
// no created timestamp.

// derivedOperation names the operation of an entry, as it is written.
func derivedOperation(d *model.DerivedRule) (op string, operands []string) {
	switch {
	case d.Sum != nil:
		return "sum", d.Sum
	case d.Difference != nil:
		return "difference", d.Difference
	case d.Ratio != nil:
		return "ratio", d.Ratio
	}
	return "", nil
}

// DerivedWhere is how a message names the derived entry at index of a
// collector's derived list: by its name, or by its place, counted from 1,
// when it has none.
func DerivedWhere(x *model.Collector, index int) string {
	if name := x.Derived[index].Name; strings.TrimSpace(name) != "" {
		return fmt.Sprintf("collector %q derived %q", x.Name, name)
	}
	return fmt.Sprintf("collector %q derived entry %d", x.Name, index+1)
}

// DescribeDerived says what a derived entry computes, as a debug probe's
// report shows it: "ratio of a, b", "sum of a by (queue)".
func DescribeDerived(d *model.DerivedRule) string {
	op, operands := derivedOperation(d)
	text := op + " of " + strings.Join(operands, ", ")
	if d.By != nil {
		text += " by (" + strings.Join(*d.By, ", ") + ")"
	}
	return text
}

// CheckDerived refuses a derived list that could not be computed, or whose
// series every scrape would refuse, and gives an entry without a type the
// type gauge. It is checked once the rules are, their types filled in.
//
// An operand is a rule's name or an earlier entry's. The series of a python
// script and of a prometheus pass-through are named by the target and the
// script, at run time, so an operand of such a collector that no rule names
// is taken, and read from the series of that name each scrape makes; one a
// rule names is checked as any other's. Only a counter, a gauge or an
// untyped family can be an operand: a histogram or a summary has no one
// value to add. Such a family that only a scrape shows is read as no series,
// and is reported as a failure of the entry, under error_mode log
// (deriveSeries).
func CheckDerived(x *model.Collector) error {
	if len(x.Derived) == 0 {
		return nil
	}
	ruleTypes := map[string]model.MetricType{}
	for i := range x.Metrics {
		r := &x.Metrics[i]
		if r.Name == "" {
			continue
		}
		t := r.Type
		if r.Histogram != nil {
			t = model.HistogramMetricType
		}
		if _, seen := ruleTypes[r.Name]; !seen || t == model.HistogramMetricType || t == model.SummaryMetricType {
			ruleTypes[r.Name] = t
		}
	}
	runTimeNames := x.Transform.Type == "python" || x.Transform.Type == "prometheus"
	earlier := map[string]int{}
	read := map[string]bool{}
	var errs []error
	for i := range x.Derived {
		d := &x.Derived[i]
		if d.Type == "" {
			d.Type = model.GaugeMetricType
		}
		err := checkDerivedEntry(x, i, ruleTypes, earlier, runTimeNames)
		errs = append(errs, err)
		_, operands := derivedOperation(d)
		for _, name := range operands {
			read[name] = true
		}
		if _, twice := earlier[d.Name]; !twice && d.Name != "" {
			earlier[d.Name] = i
		}
	}
	for i := range x.Derived {
		if d := &x.Derived[i]; d.Hidden && d.Name != "" && !read[d.Name] {
			errs = append(errs, fmt.Errorf("%s is hidden, and no later entry reads it, so it is computed for nothing; read it in a later entry, or take hidden out to export it", DerivedWhere(x, i)))
		}
	}
	return model.JoinProblems(errs...)
}

// checkDerivedEntry is CheckDerived of the entry at index, given the types
// of the rules' names and the entries before it.
func checkDerivedEntry(x *model.Collector, index int, ruleTypes map[string]model.MetricType, earlier map[string]int, runTimeNames bool) error {
	d, where := &x.Derived[index], DerivedWhere(x, index)
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%s has no name", where)
	}
	if err := checkMetricName(x, d.Name); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if limit := x.Limits.MaxMetricNameLength; limit > 0 {
		if exported := ExportedMetricName(x, d.Name); len(exported) > limit {
			return fmt.Errorf("%s is exported as %q, %d bytes, longer than limits.max_metric_name_length %d, so every series of it would fail validation; shorten the name or raise the limit", where, exported, len(exported), limit)
		}
	}
	if _, clash := ruleTypes[d.Name]; clash {
		return fmt.Errorf("%s has the name of a rule of the collector; a derived family is a family of its own, so give it a name no rule has", where)
	}
	for name, t := range ruleTypes {
		var suffixes []string
		switch t {
		case model.HistogramMetricType:
			suffixes = []string{"_bucket", "_sum", "_count"}
		case model.SummaryMetricType:
			suffixes = []string{"_sum", "_count"}
		}
		for _, suffix := range suffixes {
			if d.Name == name+suffix {
				return fmt.Errorf("%s clashes with the %s %q, which is written as series of that name; give it another name", where, t, name)
			}
		}
	}
	if first, twice := earlier[d.Name]; twice {
		return fmt.Errorf("%s is the name of derived entry %d too; give each entry a name of its own", where, first+1)
	}
	switch d.Type {
	case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
	default:
		return fmt.Errorf("%s has type %q; a derived series is one number, so use gauge, counter or untyped", where, d.Type)
	}
	if limit := x.Limits.MaxHelpLength; limit > 0 && len(d.Description) > limit {
		return fmt.Errorf("%s description is %d bytes, longer than limits.max_help_length %d, so every series would fail validation; shorten it or raise the limit", where, len(d.Description), limit)
	}
	var written []string
	for _, op := range []struct {
		key string
		set bool
	}{{"sum", d.Sum != nil}, {"difference", d.Difference != nil}, {"ratio", d.Ratio != nil}} {
		if op.set {
			written = append(written, op.key)
		}
	}
	switch len(written) {
	case 0:
		return fmt.Errorf("%s has no operation; set one of sum, difference and ratio to the names of the families it reads", where)
	case 1:
	default:
		return fmt.Errorf("%s sets %s; an entry computes one of them, so make one entry for each", where, strings.Join(written, " and "))
	}
	op, operands := derivedOperation(d)
	switch {
	case d.By != nil && op != "sum":
		return fmt.Errorf("%s sets by, which only sum takes, to add up the series of one family; take by out", where)
	case d.By != nil && len(operands) != 1:
		return fmt.Errorf("%s sets by, so its sum adds up the series of one family, and it reads %d; list one family under sum, or take by out to add up the series of several, matched by their labels", where, len(operands))
	case d.By == nil && op == "sum" && len(operands) < 2:
		return fmt.Errorf("%s sum reads %d families; list two or more to add up their series, matched by their labels, or set by to add up the series of one, as in by: [queue], or by: [] for one series of them all", where, len(operands))
	case op != "sum" && len(operands) != 2:
		return fmt.Errorf("%s %s reads %d families, and takes two, [a, b]: %s", where, op, len(operands), map[string]string{"difference": "a - b", "ratio": "a / b"}[op])
	}
	if d.By != nil {
		for i, label := range *d.By {
			if !TakesLabelNameBeforeRenames(x, label) {
				return fmt.Errorf("%s by has invalid label name %q%s", where, label, EscapingAdvice(label))
			}
			if slices.Contains((*d.By)[:i], label) {
				return fmt.Errorf("%s by lists the label %q twice; list each once", where, label)
			}
		}
	}
	for _, operand := range operands {
		if strings.TrimSpace(operand) == "" {
			return fmt.Errorf("%s %s has an empty operand; write the name of a rule of the collector, or of an earlier derived entry", where, op)
		}
		if operand == d.Name {
			return fmt.Errorf("%s %s reads itself; an entry reads the collector's rules and the derived entries before it", where, op)
		}
		if _, ok := earlier[operand]; ok {
			continue
		}
		t, isRule := ruleTypes[operand]
		if !isRule {
			if later := slices.IndexFunc(x.Derived[index+1:], func(e model.DerivedRule) bool { return e.Name == operand }); later >= 0 {
				return fmt.Errorf("%s %s reads %q, the derived entry %d, which comes after it; the entries are computed in order, so move %q above it", where, op, operand, index+later+2, operand)
			}
			if runTimeNames {
				continue
			}
			return fmt.Errorf("%s %s reads %q, which is neither a rule of the collector nor an earlier derived entry; write the name of one, as it is written, without metrics_prefix", where, op, operand)
		}
		if t == model.HistogramMetricType || t == model.SummaryMetricType {
			return fmt.Errorf("%s %s reads %q, a %s, which has no one value to compute with; read a counter, a gauge or an untyped family", where, op, operand, t)
		}
	}
	return nil
}

// derivedRule is the rule a derived entry's failure is reported under, as
// a rule's is: by the entry's name, under error_mode log.
func derivedRule(d *model.DerivedRule) model.MetricRule {
	return model.MetricRule{Name: d.Name, ErrorMode: model.ErrorModeLog}
}

// ReportedRules are the rules of c whose failures a scrape may report: its
// metric rules, and a rule for each derived entry (derivedRule), so that an
// entry that failed is counted, logged and seen to recover as a rule is.
func ReportedRules(c *model.Collector) []model.MetricRule {
	if len(c.Derived) == 0 {
		return c.Metrics
	}
	rules := slices.Clip(c.Metrics)
	for i := range c.Derived {
		rules = append(rules, derivedRule(&c.Derived[i]))
	}
	return rules
}

// deriveSeries computes the collector's derived entries from the series of
// set, and adds those of the entries not hidden to it.
func deriveSeries(ctx context.Context, set *model.MetricSet, c *model.Collector) error {
	if len(c.Derived) == 0 {
		return nil
	}
	// Only the families the entries read are gathered, each in the order
	// its series came.
	families := map[string][]model.Metric{}
	for i := range c.Derived {
		_, operands := derivedOperation(&c.Derived[i])
		for _, name := range operands {
			families[name] = nil
		}
	}
	for i := range set.Metrics {
		m := &set.Metrics[i]
		if family, wanted := families[m.Name]; wanted {
			families[m.Name] = append(family, *m)
		}
	}
	var out []model.Metric
	for i := range c.Derived {
		d := &c.Derived[i]
		series, err := deriveEntry(d, families)
		if err != nil {
			// A histogram or a summary only a scrape shows (CheckDerived):
			// the entry carries on without series, as a rule under log
			// does, and is reported so. Only a context that is done stops
			// it, as it stops every rule (handleMetricError).
			rule := derivedRule(d)
			if !handleMetricError(ctx, c, rule, err) {
				return interruptedAt(ctx, d.Name)
			}
		}
		if _, read := families[d.Name]; read {
			families[d.Name] = series
		}
		if d.Hidden || len(series) == 0 {
			continue
		}
		if err := takeSeriesN(ctx, len(series)); err != nil {
			return err
		}
		out = append(out, series...)
	}
	if len(out) > 0 {
		// Clipped, so that a prometheus pass-through's series, the decoded
		// ones themselves (passesThrough), are not written past their end.
		set.Metrics = append(slices.Clip(set.Metrics), out...)
	}
	return nil
}

// deriveEntry computes one entry's series from the families gathered.
func deriveEntry(d *model.DerivedRule, families map[string][]model.Metric) ([]model.Metric, error) {
	op, operands := derivedOperation(d)
	for _, name := range operands {
		for i := range families[name] {
			if t := families[name][i].Type; t == model.HistogramMetricType || t == model.SummaryMetricType {
				return nil, fmt.Errorf("derived %s reads %q, a %s, which has no one value to compute with", op, name, t)
			}
		}
	}
	derived := func(labels map[string]string, value float64) (model.Metric, bool) {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return model.Metric{}, false
		}
		return model.Metric{Name: d.Name, Help: d.Description, Type: d.Type, Value: value, Labels: labels}, true
	}
	var out []model.Metric
	if d.By != nil {
		by := *d.By
		index := map[string]int{}
		var sums []float64
		var labels []map[string]string
		for _, m := range families[operands[0]] {
			kept := map[string]string{}
			for _, name := range by {
				if value := m.Labels[name]; value != "" {
					kept[name] = value
				}
			}
			key := labelSetKey(kept)
			at, seen := index[key]
			if !seen {
				at = len(sums)
				index[key] = at
				sums = append(sums, 0)
				labels = append(labels, kept)
			}
			sums[at] += m.Value
		}
		for i, sum := range sums {
			if m, ok := derived(labels[i], sum); ok {
				out = append(out, m)
			}
		}
		return out, nil
	}
	// Every operand but the first is looked up by label set; a set two of
	// its series have has no one partner.
	others := make([]map[string]int, len(operands)-1)
	for i, name := range operands[1:] {
		others[i] = map[string]int{}
		for j, m := range families[name] {
			key := labelSetKey(m.Labels)
			if _, twice := others[i][key]; twice {
				others[i][key] = -1
				continue
			}
			others[i][key] = j
		}
	}
	firsts := map[string]int{}
	for _, m := range families[operands[0]] {
		firsts[labelSetKey(m.Labels)]++
	}
	for _, m := range families[operands[0]] {
		key := labelSetKey(m.Labels)
		if firsts[key] > 1 {
			continue
		}
		value, matched := m.Value, true
		for i, name := range operands[1:] {
			j, ok := others[i][key]
			if !ok || j < 0 {
				matched = false
				break
			}
			partner := families[name][j].Value
			switch op {
			case "sum":
				value += partner
			case "difference":
				value -= partner
			case "ratio":
				value /= partner
			}
		}
		if !matched {
			continue
		}
		if s, ok := derived(nonEmptyLabels(m.Labels), value); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// labelSetKey is a key of a label set that two sets share when they have
// the same labels of values that are not empty, which is when they are one
// series to Prometheus.
func labelSetKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name, value := range labels {
		if value != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		// Each part after its length, so that no name or value can end
		// where another part would begin.
		fmt.Fprintf(&b, "%d:%s%d:%s", len(name), name, len(labels[name]), labels[name])
	}
	return b.String()
}

// nonEmptyLabels is a copy of labels without those of empty values, so a
// derived series never shares a label map with the series it was made of.
func nonEmptyLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for name, value := range labels {
		if value != "" {
			out[name] = value
		}
	}
	return out
}
