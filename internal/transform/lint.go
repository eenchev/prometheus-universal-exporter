package transform

// The metric lint gives advice about the series a collector makes: names
// that break Prometheus's naming conventions, labels whose values look
// unbounded, and one metric name made in two ways by two collectors.
// --dry-run reports what the configuration alone shows (LintConfiguration),
// and the debug report adds what one run's series show (LintRun). Advice
// never fails anything: a dry run's exit status and a probe's answer are
// what they are without it.
//
// Each rule has an id (model.LintRules), shown with its findings, and a
// collector's lint.ignore silences the rules it lists for that collector.

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// LintFinding is one piece of advice: the collector it is about, the rule that
// gave it, the metric and what is wrong with it and what to do instead.
type LintFinding struct {
	Collector string `json:"collector"`
	Rule      string `json:"rule"`
	Metric    string `json:"metric"`
	Message   string `json:"message"`
}

// String is the finding on one line, as a log or a report shows it.
func (f LintFinding) String() string {
	return fmt.Sprintf("[%s] metric %q: %s", f.Rule, f.Metric, f.Message)
}

// MaxLintLabelValues is how many distinct values one label of one metric may
// have in one run before unbounded-label calls it unbounded. A label with a
// value per host, pod or status code stays well under it; one with a value
// per request, user or session does not.
const MaxLintLabelValues = 100

// LintConfiguration is the advice the configuration alone gives, for every collector
// in its order: the name rules on the names its rules make, and the names
// another collector makes in another way. The series a run makes are not
// known here, so unbounded-label gives none.
func LintConfiguration(conf *model.Config) []LintFinding {
	var out []LintFinding
	for i := range conf.Collectors {
		out = append(out, LintCollector(&conf.Collectors[i], conf)...)
	}
	return out
}

// LintCollector is the advice the configuration gives about c: the name rules on
// the names its rules make, and the names a collector of conf other than c,
// or one of c's name, makes in another way. c need not be one of conf's: a
// collector written on the debug page is compared with those in force.
func LintCollector(c *model.Collector, conf *model.Config) []LintFinding {
	var out []LintFinding
	for _, f := range lintStaticFamilies(c) {
		out = append(out, lintNameFindings(c, f.name, f.typ)...)
	}
	if conf != nil {
		out = append(out, lintClashes(c, conf)...)
	}
	return lintSilenced(c, out)
}

// LintRun is the advice of a debug run of c: LintCollector's, the name rules on the
// metrics the run made, which include those of a python script or of a
// prometheus transform's pass-through, and unbounded-label on their labels.
// A finding of a name both give is shown once.
func LintRun(c *model.Collector, conf *model.Config, set *model.MetricSet) []LintFinding {
	out := LintCollector(c, conf)
	if set == nil {
		return out
	}
	seen := map[LintFinding]bool{}
	for _, f := range out {
		seen[f] = true
	}
	var found []LintFinding
	for _, name := range lintFamilyOrder(set) {
		found = append(found, lintNameFindings(c, name, lintTypeOf(set, name))...)
	}
	found = append(found, lintUnboundedLabels(c, set)...)
	for _, f := range lintSilenced(c, found) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func lintSilenced(c *model.Collector, findings []LintFinding) []LintFinding {
	if len(c.Lint.Ignore) == 0 {
		return findings
	}
	return slices.DeleteFunc(findings, func(f LintFinding) bool { return slices.Contains(c.Lint.Ignore, f.Rule) })
}

// lintFamily is a metric name a collector's rules make, as statically known:
// its type, "" where the series keep their own; its help, "" where none is
// written; and its label names, nil where the series keep labels of their
// own that cannot be known (a prometheus transform).
type lintFamily struct {
	name   string
	typ    model.MetricType
	help   string
	labels map[string]bool
}

// lintStaticFamilies are the metric names c's rules make, as exported, in the
// order of their first rule. A python transform's rules make no series and a
// prometheus rule without a name keeps the names of the series it passes
// on, so neither has a name known before a run.
func lintStaticFamilies(c *model.Collector) []*lintFamily {
	if c.Transform.Type == "python" {
		return nil
	}
	var out []*lintFamily
	byName := map[string]*lintFamily{}
	prometheus := c.Transform.Type == "prometheus"
	for i := range c.Metrics {
		rule := &c.Metrics[i]
		if rule.Name == "" {
			continue
		}
		name := ExportedMetricName(c, rule.Name)
		f := byName[name]
		if f == nil {
			f = &lintFamily{name: name}
			if !prometheus {
				f.labels = lintCollectorLabels(c)
			}
			byName[name] = f
			out = append(out, f)
		}
		if f.typ == "" {
			f.typ = rule.Type
		}
		if f.help == "" {
			f.help = rule.Description
		}
		if f.labels != nil {
			for _, label := range rule.Labels {
				f.labels[label.Name] = true
			}
		}
	}
	for _, f := range out {
		if f.labels != nil {
			f.labels = lintExportedLabels(c, f.labels)
		}
	}
	return out
}

// lintCollectorLabels are the label names transform.labels gives every series.
func lintCollectorLabels(c *model.Collector) map[string]bool {
	out := map[string]bool{}
	for name := range c.Transform.Labels {
		out[name] = true
	}
	return out
}

// lintExportedLabels are names as transform.remove_labels and
// transform.rename_labels leave them.
func lintExportedLabels(c *model.Collector, names map[string]bool) map[string]bool {
	out := maps.Clone(names)
	for _, name := range c.Transform.RemoveLabels {
		delete(out, name)
	}
	for from, to := range c.Transform.RenameLabels {
		if names[from] {
			delete(out, from)
			out[to] = true
		}
	}
	return out
}

// lintNameFindings are counter-total's and unit-suffix's advice on the metric
// name of the type typ, "" where it is not known.
func lintNameFindings(c *model.Collector, name string, typ model.MetricType) []LintFinding {
	var out []LintFinding
	add := func(rule, format string, args ...any) {
		out = append(out, LintFinding{Collector: c.Name, Rule: rule, Metric: name, Message: fmt.Sprintf(format, args...)})
	}
	switch {
	case typ == model.CounterMetricType && !strings.HasSuffix(name, "_total"):
		add(model.LintCounterTotal, "a counter whose name does not end in _total is written as %s in the Prometheus text format and as %s_total in OpenMetrics, so its name depends on the format a scraper asks for; name it %s_total", name, name, name)
	case typ == model.GaugeMetricType && strings.HasSuffix(name, "_total"):
		add(model.LintCounterTotal, "a gauge whose name ends in _total reads as a counter, and rate() and increase() over it give nonsense where it goes down; drop the suffix, or make it a counter if it only goes up")
	}
	if message := lintUnitAdvice(name, typ); message != "" {
		add(model.LintUnitSuffix, "%s", message)
	}
	return out
}

// nonBaseUnits are the units unit-suffix asks to be replaced by a base unit,
// by the word a name ends in (before a counter's _total): the base unit's
// word and the scale that converts a value to it.
var nonBaseUnits = map[string]struct {
	base, scale string
}{
	"ms":           {"seconds", "0.001"},
	"millis":       {"seconds", "0.001"},
	"milliseconds": {"seconds", "0.001"},
	"us":           {"seconds", "0.000001"},
	"microseconds": {"seconds", "0.000001"},
	"ns":           {"seconds", "0.000000001"},
	"nanoseconds":  {"seconds", "0.000000001"},
	"minutes":      {"seconds", "60"},
	"hours":        {"seconds", "3600"},
	"days":         {"seconds", "86400"},
	"kb":           {"bytes", "1000"},
	"kilobytes":    {"bytes", "1000"},
	"kib":          {"bytes", "1024"},
	"mb":           {"bytes", "1000000"},
	"megabytes":    {"bytes", "1000000"},
	"mib":          {"bytes", "1048576"},
	"gb":           {"bytes", "1000000000"},
	"gigabytes":    {"bytes", "1000000000"},
	"gib":          {"bytes", "1073741824"},
	"percent":      {"ratio", "0.01"},
	"percentage":   {"ratio", "0.01"},
}

// ambiguousUnits are the units of nonBaseUnits that are as often other words.
var ambiguousUnits = []string{"us", "ns"}

// durationWords are the words that make a name read as a duration or a point
// in time, which is held to have a seconds word. "timeout" and "timeouts"
// are not among them, since they more often count timeouts than measure
// one, nor "times", which counts.
var durationWords = []string{"duration", "latency", "elapsed", "uptime", "age", "time", "timestamp"}

// sizeWords are the words that make a name read as a size in bytes, which is
// held to have a bytes word. "size" is not among them, since queue_size,
// pool_size and batch_size count items, not bytes.
var sizeWords = []string{"memory"}

// lintUnitAdvice is unit-suffix's advice on name, "" when it has none: a unit
// that is not a base unit at its end, or else a word that reads as a
// duration or a size without the base unit's word.
func lintUnitAdvice(name string, typ model.MetricType) string {
	words := lintWords(name)
	if len(words) == 0 {
		return ""
	}
	counted := typ == model.CounterMetricType && words[len(words)-1] == "total" && len(words) > 1
	last := words[len(words)-1]
	if counted {
		last = words[len(words)-2]
	}
	has := func(units ...string) bool {
		return slices.ContainsFunc(words, func(w string) bool { return slices.Contains(units, w) })
	}
	// us and ns are a region or a namespace as often as a unit, and are
	// read as one only in a name that reads as a duration.
	if unit, ok := nonBaseUnits[last]; ok && (!slices.Contains(ambiguousUnits, last) || has(durationWords...)) {
		return fmt.Sprintf("the unit %s is not a base unit; Prometheus names use seconds, bytes and ratios from 0 to 1: convert the value with scale: %s and name it %s", last, unit.scale, lintWithUnit(name, last, unit.base, counted))
	}
	switch {
	case has(durationWords...) && !has("seconds", "ratio"):
		if has("timestamp") {
			return "the name reads as a point in time but has no unit; a point in time is a Unix time in seconds, named " + lintWithUnit(name, "", "seconds", counted)
		}
		return fmt.Sprintf("the name reads as a duration or a point in time but has no unit; a duration is in seconds, named %s, and a point in time a Unix time in seconds, named %s", lintWithUnit(name, "", "seconds", counted), lintWithUnit(name, "", "timestamp_seconds", counted))
	case has(sizeWords...) && !has("bytes", "ratio"):
		return "the name reads as a size but has no unit; a size is in bytes, named " + lintWithUnit(name, "", "bytes", counted)
	}
	return ""
}

// lintWithUnit is name with its unit word old replaced by unit, or unit added
// where old is "", before a counter's _total.
func lintWithUnit(name, old, unit string, counted bool) string {
	stem := name
	if counted {
		stem = strings.TrimSuffix(stem, "_total")
	}
	if old != "" {
		stem = stem[:len(stem)-len(old)] + unit
	} else {
		stem += "_" + unit
	}
	if counted {
		stem += "_total"
	}
	return stem
}

// lintWords are the words of a metric name, lower-cased: the parts between
// underscores, and between any other characters a UTF-8 name may have.
func lintWords(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// The shapes of a label value that make unbounded-label call the label
// unbounded: a value that identifies one thing among ever more, or a time.
var (
	uuidValue    = regexp.MustCompile(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hexValue     = regexp.MustCompile(`^(?i)(0x)?[0-9a-f]{16,}$`)
	numericValue = regexp.MustCompile(`^[0-9]{10,}$`)
	timeValue    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9]{2}:[0-9]{2}(:[0-9]{2}([.,][0-9]+)?)?(Z|z|[+-][0-9]{2}:?[0-9]{2})?)?$`)
)

// lintValueShape says what a label value looks like when it looks unbounded,
// "" otherwise.
func lintValueShape(value string) string {
	switch {
	case uuidValue.MatchString(value):
		return "a UUID"
	case numericValue.MatchString(value):
		return "a number of 10 or more digits, an id or a Unix time"
	case hexValue.MatchString(value):
		return "a hexadecimal string of 16 or more digits, an id or a hash"
	case timeValue.MatchString(value):
		return "a date or a time"
	}
	return ""
}

// lintMaxShownValue bounds the label value a finding quotes.
const lintMaxShownValue = 64

// lintUnboundedLabels is unbounded-label's advice on the series of a run: each
// label of a metric with more than MaxLintLabelValues distinct values among
// them, or else with a value shaped as an id or a time.
func lintUnboundedLabels(c *model.Collector, set *model.MetricSet) []LintFinding {
	type key struct{ metric, label string }
	values := map[key]map[string]bool{}
	shaped := map[key]string{}
	var order []key
	for _, m := range set.Metrics {
		for _, label := range model.SortedKeys(m.Labels) {
			k := key{m.Name, label}
			if values[k] == nil {
				values[k] = map[string]bool{}
				order = append(order, k)
			}
			value := m.Labels[label]
			values[k][value] = true
			if _, ok := shaped[k]; !ok {
				if shape := lintValueShape(value); shape != "" {
					shaped[k] = fmt.Sprintf("%q is %s", lintShownValue(value), shape)
				}
			}
		}
	}
	var out []LintFinding
	for _, k := range order {
		var message string
		switch n := len(values[k]); {
		case n > MaxLintLabelValues:
			message = fmt.Sprintf("the label %s has %d distinct values in this run, more than %d", k.label, n, MaxLintLabelValues)
		case shaped[k] != "":
			message = fmt.Sprintf("the label %s has a value shaped as an id or a time: %s", k.label, shaped[k])
		default:
			continue
		}
		message += "; each value is a series of its own, which Prometheus keeps for as long as it keeps the metric, so a label whose values keep changing grows its series without bound: drop the label or map its values to a few (value_map)"
		out = append(out, LintFinding{Collector: c.Name, Rule: model.LintUnboundedLabel, Metric: k.metric, Message: message})
	}
	return out
}

func lintShownValue(value string) string {
	if len(value) <= lintMaxShownValue {
		return value
	}
	cut := lintMaxShownValue
	for cut > 0 && !lintUTF8Start(value[cut]) {
		cut--
	}
	return value[:cut] + "..."
}

func lintUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// lintClashes is name-clash's advice on c: each metric name c's rules make that
// a rule of another collector of conf makes with another type, another help
// text or other label names. What a side does not know is not compared: a
// type a prometheus rule leaves to the series, a help a rule does not write,
// the labels of a prometheus transform's series, and the names a python
// script or a prometheus rule without a name make.
func lintClashes(c *model.Collector, conf *model.Config) []LintFinding {
	mine := lintStaticFamilies(c)
	if len(mine) == 0 {
		return nil
	}
	var out []LintFinding
	for i := range conf.Collectors {
		other := &conf.Collectors[i]
		if other.Name == c.Name {
			continue
		}
		theirs := map[string]*lintFamily{}
		for _, f := range lintStaticFamilies(other) {
			theirs[f.name] = f
		}
		for _, f := range mine {
			g := theirs[f.name]
			if g == nil {
				continue
			}
			var differences []string
			if f.typ != "" && g.typ != "" && f.typ != g.typ {
				differences = append(differences, fmt.Sprintf("as a %s, where this collector makes a %s", g.typ, f.typ))
			}
			if f.help != "" && g.help != "" && f.help != g.help {
				differences = append(differences, fmt.Sprintf("with the help %q, where this collector's is %q", g.help, f.help))
			}
			if f.labels != nil && g.labels != nil && !maps.Equal(f.labels, g.labels) {
				differences = append(differences, fmt.Sprintf("with the labels [%s], where this collector's are [%s]", strings.Join(model.SortedKeys(g.labels), " "), strings.Join(model.SortedKeys(f.labels), " ")))
			}
			if len(differences) == 0 {
				continue
			}
			out = append(out, LintFinding{Collector: c.Name, Rule: model.LintNameClash, Metric: f.name, Message: fmt.Sprintf("collector %q makes it too, %s; Prometheus keeps one type and one help for a name, and a query over both collectors' series mixes them, so give the two the same type, help and labels, or different names", other.Name, strings.Join(differences, ", "))})
		}
	}
	return out
}

// lintFamilyOrder are the metric names of set in the order each first appears.
func lintFamilyOrder(set *model.MetricSet) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range set.Metrics {
		if !seen[m.Name] {
			seen[m.Name] = true
			out = append(out, m.Name)
		}
	}
	return out
}

func lintTypeOf(set *model.MetricSet, name string) model.MetricType {
	for _, m := range set.Metrics {
		if m.Name == name {
			return m.Type
		}
	}
	return ""
}
