//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// diskCollector is an xpath collector with two rules of one metric name,
// disk_bytes, read from two elements of a disk, and one rule of a name of
// its own, disk_inodes.
func diskCollector(name, mode string) model.Collector {
	c := testutil.Collector(name, "xml")
	c.Transform = model.TransformConfig{Type: "xpath"}
	disk := model.LabelRule{Name: "disk", Expression: "../@name"}
	c.Metrics = []model.MetricRule{
		{Name: "disk_bytes", Type: model.GaugeMetricType, Expression: "//disk/used", ErrorMode: mode, Labels: []model.LabelRule{{Name: "state", Value: "used"}, disk}},
		{Name: "disk_bytes", Type: model.GaugeMetricType, Expression: "//disk/free", ErrorMode: mode, Labels: []model.LabelRule{{Name: "state", Value: "free"}, disk}},
		{Name: "disk_inodes", Type: model.GaugeMetricType, Expression: "//disk/inodes", ErrorMode: mode, Labels: []model.LabelRule{disk}},
	}
	return c
}

// disks are four disks as a target answers them, each element's text by the
// disk: numbers, until a test writes something else in one.
type disks struct{ used, free, inodes [4]string }

func wholeDisks() disks {
	return disks{used: [4]string{"10", "20", "30", "40"}, free: [4]string{"1", "2", "3", "4"}, inodes: [4]string{"5", "6", "7", "8"}}
}

func (d disks) xml() string {
	var b strings.Builder
	b.WriteString("<disks>")
	for i := range d.used {
		fmt.Fprintf(&b, `<disk name="d%d"><used>%s</used><free>%s</free><inodes>%s</inodes></disk>`, i, d.used[i], d.free[i], d.inodes[i])
	}
	b.WriteString("</disks>")
	return b.String()
}

// diskTarget answers what serve was last given, as XML.
type diskTarget struct {
	*httptest.Server
	body atomic.Pointer[string]
}

func newDiskTarget(t *testing.T) *diskTarget {
	t.Helper()
	target := &diskTarget{}
	target.serve(wholeDisks())
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(*target.body.Load()))
	}))
	t.Cleanup(target.Close)
	return target
}

func (d *diskTarget) serve(table disks) {
	body := table.xml()
	d.body.Store(&body)
}

// ruleLogServer is a server of the collectors that logs at debug level into
// the buffer, without the time, and whose failure log's clock stands at one
// moment until later moves it.
func ruleLogServer(t *testing.T, collectors ...model.Collector) (*Server, *bytes.Buffer, *atomic.Int64) {
	t.Helper()
	testutil.CaptureLogs(t)
	server := modeServer(t, collectors...)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: withoutTime}))
	later, began := &atomic.Int64{}, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	server.failures.now = func() time.Time { return began.Add(time.Duration(later.Load())) }
	return server, logs, later
}

// withoutTime leaves a line's time out.
func withoutTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// ruleLines takes the lines of rules that failed or recovered out of the
// log, in order, and empties it.
func ruleLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, record := range logRecords(t, logs) {
		if record["msg"] == "metric extraction failed" || record["msg"] == "metric extraction recovered" {
			lines = append(lines, record)
		}
	}
	logs.Reset()
	return lines
}

// ruleLine is a line of a probe of target as the log holds it: its level
// and message, the probe's collector and target, and then the attributes
// given, as pairs.
func ruleLine(level, msg, collector, target string, pairs ...any) map[string]any {
	line := map[string]any{"level": level, "msg": msg, "collector": collector, "target": target, "url": target}
	for i := 0; i < len(pairs); i += 2 {
		line[pairs[i].(string)] = pairs[i+1]
	}
	return line
}

const (
	usedBlank    = `metric "disk_bytes" value is missing for node %d: XPath "//disk/used" selected a node without a value`
	freeNoNumber = `metric "disk_bytes" node %d: value "n/a" is not a number; map text to numbers with value_map`
)

// A rule is logged, remembered and recovered by itself, whatever the other
// rules of its metric name do. While the rule that reads //disk/used fails
// on every scrape, the rule of the same name that reads //disk/free starts
// to fail: it is logged in full, as a warning, with its own error, and both
// lines name the rule by its expression. Before, the name had one line with
// the first rule's error, which the log recognised and held back as a
// repeat, so the second rule failed without a word. The second rule then
// recovers, which is logged while the first is still remembered: its
// failure in another node is a repeat, five minutes on its line counts
// every repeat since it began, and its own recovery counts every scrape it
// failed.
func TestARuleIsLoggedRememberedAndRecoveredByItself(t *testing.T) {
	target := newDiskTarget(t)
	server, logs, later := ruleLogServer(t, diskCollector("disks", model.ErrorModeLog))
	scrape := func(change func(*disks)) []map[string]any {
		t.Helper()
		table := wholeDisks()
		change(&table)
		target.serve(table)
		if answer := probeOnce(t, server, probePath("disks", target.URL, ""), nil); answer.Code != http.StatusOK {
			t.Fatalf("answered %d: %s", answer.Code, answer.Body)
		}
		return ruleLines(t, logs)
	}
	failed := func(level, expression, said string, more ...any) map[string]any {
		pairs := append([]any{"metric", "disk_bytes", "expression", expression, "error_mode", "log", "failures", float64(1), "error", said}, more...)
		return ruleLine(level, "metric extraction failed", "disks", target.URL, pairs...)
	}
	recovered := func(expression, failedFor string, failures int) map[string]any {
		return ruleLine("INFO", "metric extraction recovered", "disks", target.URL, "metric", "disk_bytes", "expression", expression, "stage", "metric", "failed_for", failedFor, "failures", float64(failures))
	}
	for _, step := range []struct {
		what   string
		change func(*disks)
		later  time.Duration
		want   []map[string]any
	}{
		{"the first rule fails", func(d *disks) { d.used[1] = "" }, 0,
			[]map[string]any{failed("WARN", "//disk/used", fmt.Sprintf(usedBlank, 1))}},
		{"it fails in another node", func(d *disks) { d.used[3] = "" }, 0,
			[]map[string]any{failed("DEBUG", "//disk/used", fmt.Sprintf(usedBlank, 3), "repeat", true)}},
		{"the second rule starts to fail", func(d *disks) { d.used[1], d.free[2] = "", "n/a" }, 0,
			[]map[string]any{failed("DEBUG", "//disk/used", fmt.Sprintf(usedBlank, 1), "repeat", true), failed("WARN", "//disk/free", fmt.Sprintf(freeNoNumber, 2))}},
		{"the second rule fails again", func(d *disks) { d.used[1], d.free[0] = "", "n/a" }, 0,
			[]map[string]any{failed("DEBUG", "//disk/used", fmt.Sprintf(usedBlank, 1), "repeat", true), failed("DEBUG", "//disk/free", fmt.Sprintf(freeNoNumber, 0), "repeat", true)}},
		{"the second rule recovers", func(d *disks) { d.used[2] = "" }, time.Minute,
			[]map[string]any{failed("DEBUG", "//disk/used", fmt.Sprintf(usedBlank, 2), "repeat", true), recovered("//disk/free", "1m0s", 2)}},
		{"five minutes on", func(d *disks) { d.used[2] = "" }, failureRepeatInterval + time.Second,
			[]map[string]any{failed("WARN", "//disk/used", fmt.Sprintf(usedBlank, 2), "repeated", float64(5), "failing_since", "2026-10-04T09:00:00Z")}},
		{"the first rule recovers", func(*disks) {}, failureRepeatInterval + time.Minute,
			[]map[string]any{recovered("//disk/used", "6m0s", 6)}},
		{"nothing fails", func(*disks) {}, failureRepeatInterval + time.Minute, nil},
	} {
		later.Store(int64(step.later))
		if lines := scrape(step.change); !reflect.DeepEqual(lines, step.want) {
			t.Fatalf("%s: the rules' lines are\n%v\nwant\n%v", step.what, lines, step.want)
		}
	}
}

// A metric name only one rule exports is logged exactly as it was, in a
// collector whose other rules share a name too: the failure, its repeat,
// the line five minutes on and the recovery are these lines to the letter,
// with no attribute more and none in another place.
func TestAMetricOfOneRuleLogsTheLinesItDid(t *testing.T) {
	target := newDiskTarget(t)
	server, logs, later := ruleLogServer(t, diskCollector("disks", model.ErrorModeLog))
	lines := func(blank int) string {
		t.Helper()
		table := wholeDisks()
		if blank >= 0 {
			table.inodes[blank] = ""
		}
		target.serve(table)
		if answer := probeOnce(t, server, probePath("disks", target.URL, ""), nil); answer.Code != http.StatusOK {
			t.Fatalf("answered %d: %s", answer.Code, answer.Body)
		}
		var kept []string
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if strings.Contains(line, "metric extraction") {
				kept = append(kept, line)
			}
		}
		logs.Reset()
		return strings.Join(kept, "\n")
	}
	probe := `"collector":"disks","target":"` + target.URL + `","url":"` + target.URL + `","metric":"disk_inodes",`
	said := func(node int) string {
		return fmt.Sprintf(`"error_mode":"log","failures":1,"error":"metric \"disk_inodes\" value is missing for node %d: XPath \"//disk/inodes\" selected a node without a value"`, node)
	}
	for _, step := range []struct {
		blank int
		later time.Duration
		want  string
	}{
		{1, 0, `{"level":"WARN","msg":"metric extraction failed",` + probe + said(1) + `}`},
		{3, 0, `{"level":"DEBUG","msg":"metric extraction failed",` + probe + said(3) + `,"repeat":true}`},
		{0, failureRepeatInterval + time.Second, `{"level":"WARN","msg":"metric extraction failed",` + probe + said(0) + `,"repeated":2,"failing_since":"2026-10-04T09:00:00Z"}`},
		{-1, failureRepeatInterval + time.Minute, `{"level":"INFO","msg":"metric extraction recovered",` + probe + `"stage":"metric","failed_for":"6m0s","failures":3}`},
		{-1, failureRepeatInterval + time.Minute, ""},
	} {
		later.Store(int64(step.later))
		if got := lines(step.blank); got != step.want {
			t.Fatalf("with the value of disk %d missing, the rule's lines are\n%s\nwant\n%s", step.blank, got, step.want)
		}
	}
}

// logRuleFailuresByName is logRuleFailures as it was while a metric name's
// rules were logged, remembered and recovered as one, the oracle of
// TestRulesOfNamesOfTheirOwnAreLoggedAsTheyWere.
func logRuleFailuresByName(ctx context.Context, s *Server, read configRead, c *model.Collector, failures []transform.RuleFailure, l collectLog, complete bool) {
	failing := map[string]bool{}
	for _, f := range failures {
		if !f.Logged {
			continue
		}
		failing[f.Metric] = true
		attrs := append(append([]any{}, l.attrs...), "metric", f.Metric, "error_mode", model.ErrorModeLog, "failures", f.Failures)
		s.tripFailed(ctx, read, slog.LevelWarn, l.key+"\x00rule\x00"+f.Metric, "metric extraction failed", "metric", f.First, attrs...)
	}
	if !complete {
		return
	}
	for _, rule := range c.Metrics {
		if rule.ErrorMode == model.ErrorModeLog && !failing[rule.Name] {
			attrs := append(append([]any{}, l.attrs...), "metric", rule.Name)
			s.tripRecovered(ctx, read, l.key+"\x00rule\x00"+rule.Name, "metric extraction recovered", attrs...)
		}
	}
}

// For collectors none of whose metric names is exported by two rules that
// differ, what is logged of their rules is what was logged before, line for
// line: over generated rules, under every error mode and with rules alike in
// name, expression and items among them, and generated sequences of scrapes
// on which some of the rules fail, with the same text or another, on which
// the transform is at times not complete, and between which minutes or
// hours pass, the log of a server that logs them as now and the log of one
// that logs them as before read the same. So do the logs of a debug probe's
// report.
func TestRulesOfNamesOfTheirOwnAreLoggedAsTheyWere(t *testing.T) {
	modes := []string{model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail}
	// kinds counts the lines compared, by what tells one kind from another.
	// level=WARN is a line of a debug probe's report.
	kinds := map[string]int{`"level":"WARN"`: 0, `"repeat":true`: 0, `"repeated":`: 0, `"msg":"metric extraction recovered"`: 0, "level=WARN": 0}
	for seed := range uint64(60) {
		random := rand.New(rand.NewPCG(seed, 14))
		c := model.Collector{Name: "generated"}
		for i := range 1 + random.IntN(6) {
			rule := model.MetricRule{Name: fmt.Sprintf("m%d", i), Expression: fmt.Sprintf(".e%d", random.IntN(3)), ErrorMode: modes[random.IntN(len(modes))]}
			if random.IntN(3) == 0 {
				rule.Items = ".items[]"
			}
			if i > 0 && random.IntN(4) == 0 {
				// Another rule of an earlier one's name, expression and
				// items: the same rule, to the transform and to the log.
				twin := c.Metrics[random.IntN(i)]
				rule.Name, rule.Expression, rule.Items = twin.Name, twin.Expression, twin.Items
			}
			c.Metrics = append(c.Metrics, rule)
		}
		// The servers' own collector is not the generated one, whose rules
		// are only read for what tells them apart and how they fail.
		now, before, clock := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		was, wasBefore, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		was.failures.now = now.failures.now
		l := collectLog{key: failureKey(c.Name, "http://target.example", ""), attrs: []any{"collector", c.Name, "target", "http://target.example"}}
		ctx, wasCtx := context.Background(), context.Background()
		var trace, wasTrace *probeTrace
		if seed%5 == 0 {
			ctx, trace = newProbeTrace(ctx)
			wasCtx, wasTrace = newProbeTrace(wasCtx)
		}
		for step := range 40 {
			switch random.IntN(12) {
			case 0:
				clock.Add(int64(failureRepeatInterval + time.Minute))
			case 1:
				clock.Add(int64(failureLogForget + time.Minute))
			}
			var failures []transform.RuleFailure
			complete := random.IntN(8) != 0
			for i, rule := range c.Metrics {
				twin := false
				for _, earlier := range c.Metrics[:i] {
					twin = twin || earlier.Name == rule.Name
				}
				if twin || random.IntN(3) != 0 {
					continue
				}
				if rule.ErrorMode == model.ErrorModeFail {
					// A rule under fail fails the transform, and is in no
					// report.
					complete = false
					continue
				}
				failed := 1 + random.Uint64N(4)
				failures = append(failures, transform.RuleFailure{
					Metric: rule.Name, Expression: rule.Expression, Items: rule.Items, Failures: failed, Missing: random.Uint64N(failed + 1),
					First:  model.Errorf("metric %q value %d is missing for item %d", rule.Name, random.IntN(2), model.Position(random.IntN(9))),
					Logged: rule.ErrorMode == model.ErrorModeLog,
				})
			}
			now.logRuleFailures(ctx, configRead{}, &c, failures, l, complete)
			logRuleFailuresByName(wasCtx, was, configRead{}, &c, failures, l, complete)
			if before.String() != wasBefore.String() {
				t.Fatalf("seed %d, scrape %d, with the rules %+v failing %+v, complete %v: the log is\n%s\nwant as it was\n%s", seed, step, c.Metrics, failures, complete, before, wasBefore)
			}
			for kind := range kinds {
				kinds[kind] += strings.Count(before.String(), kind)
			}
			before.Reset()
			wasBefore.Reset()
		}
		if trace != nil {
			if trace.logs.String() != wasTrace.logs.String() {
				t.Fatalf("seed %d: a debug probe's logs are\n%s\nwant as they were\n%s", seed, trace.logs.String(), wasTrace.logs.String())
			}
			kinds["level=WARN"] += strings.Count(trace.logs.String(), "level=WARN")
		}
	}
	for kind, lines := range kinds {
		if lines < 20 {
			t.Errorf("only %d lines with %s were compared", lines, kind)
		}
	}
}

// A scrape whose rules neither fail nor recover, as nearly every scrape is,
// costs the log no more than it did, for a collector of thirty rules half of
// which share their names: it allocates nothing more, though each rule is
// now looked up by itself.
func TestRulesThatNeitherFailNorRecoverCostTheLogNoMoreThanTheyDid(t *testing.T) {
	c := model.Collector{Name: "generated"}
	for i := range 30 {
		c.Metrics = append(c.Metrics, model.MetricRule{Name: fmt.Sprintf("m%d", min(i, 15)), Items: ".items[]", Expression: fmt.Sprintf(".values.e%d", i), ErrorMode: model.ErrorModeLog})
	}
	server, _, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
	l := collectLog{key: failureKey(c.Name, "http://target.example", ""), attrs: []any{"collector", c.Name, "target", "http://target.example", "url", "http://target.example"}}
	ctx := context.Background()
	was := testing.AllocsPerRun(50, func() { logRuleFailuresByName(ctx, server, configRead{}, &c, nil, l, true) })
	now := testing.AllocsPerRun(50, func() { server.logRuleFailures(ctx, configRead{}, &c, nil, l, true) })
	if now > was {
		t.Errorf("a scrape of thirty rules that do not fail allocates %v times for the log, and allocated %v", now, was)
	}
}

// logRuleFailuresAskingEveryRule is logRuleFailures as it was while a scrape
// asked the failure log about each of its rules under log, whether or not
// any rule of the trip was remembered, and while a rule's key was its name,
// expression and items with a NUL between them: the oracle of
// TestRulesAreLoggedAsTheyWereWhenEveryRuleWasAsked.
func logRuleFailuresAskingEveryRule(ctx context.Context, s *Server, read configRead, c *model.Collector, failures []transform.RuleFailure, l collectLog, complete bool) {
	keyOf := func(key, metric, expression, items string) string {
		return key + "\x00rule\x00" + metric + "\x00" + expression + "\x00" + items
	}
	failing := map[string]bool{}
	var shared sharedRuleNames
	for _, f := range failures {
		if !f.Logged {
			continue
		}
		key := keyOf(l.key, f.Metric, f.Expression, f.Items)
		failing[key] = true
		attrs := append(append([]any{}, l.attrs...), "metric", f.Metric)
		attrs = append(shared.telling(attrs, c, f.Metric, f.Expression, f.Items), "error_mode", model.ErrorModeLog, "failures", f.Failures)
		s.tripFailed(ctx, read, slog.LevelWarn, key, "metric extraction failed", "metric", f.First, attrs...)
	}
	if !complete {
		return
	}
	for _, rule := range c.Metrics {
		if rule.ErrorMode != model.ErrorModeLog {
			continue
		}
		key := keyOf(l.key, rule.Name, rule.Expression, rule.Items)
		if failing[key] || !s.failures.remembers(key) {
			continue
		}
		attrs := append(append([]any{}, l.attrs...), "metric", rule.Name)
		s.tripRecovered(ctx, read, key, "metric extraction recovered", shared.telling(attrs, c, rule.Name, rule.Expression, rule.Items)...)
	}
}

// A scrape that asks the failure log once whether any rule of its trip is
// remembered, and looks for the rules that recovered only then, logs what it
// logged while it asked about every rule: no recovery goes unlogged, and no
// line is another. Over 80 generated collectors whose rules share metric
// names, differ in expression and items and have twins, under every error
// mode, and 60 generated scrapes each of two targets and a directory's file
// in turn, on which some rules fail with one text or another, the transform
// is at times not complete, and minutes or hours pass, the log reads line
// for line as the former logging, kept as an oracle, wrote it: with the
// rule's key made as it is now, with the expression's length in it, which
// tells these rules apart as the former key did.
func TestRulesAreLoggedAsTheyWereWhenEveryRuleWasAsked(t *testing.T) {
	modes := []string{model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail}
	kinds := map[string]int{`"level":"WARN"`: 0, `"repeat":true`: 0, `"repeated":`: 0, `"msg":"metric extraction recovered"`: 0, `"expression":`: 0, `"items":`: 0, `"file":`: 0}
	for seed := range uint64(80) {
		random := rand.New(rand.NewPCG(seed, 16))
		c := model.Collector{Name: "generated"}
		for range 1 + random.IntN(7) {
			rule := model.MetricRule{Name: fmt.Sprintf("m%d", random.IntN(3)), Expression: fmt.Sprintf(".e%d", random.IntN(3)), ErrorMode: modes[random.IntN(len(modes))]}
			if random.IntN(3) == 0 {
				rule.Items = ".items[]"
			}
			c.Metrics = append(c.Metrics, rule)
		}
		now, logs, clock := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		was, wasLogs, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		was.failures.now = now.failures.now
		trips := []collectLog{
			{key: failureKey(c.Name, "http://a.example", ""), attrs: []any{"collector", c.Name, "target", "http://a.example"}},
			{key: failureKey(c.Name, "http://b.example", ""), attrs: []any{"collector", c.Name, "target", "http://b.example"}},
			{key: failureKey(c.Name, "/var/metrics", "a.prom"), attrs: []any{"collector", c.Name, "target", "/var/metrics", "file", "a.prom"}},
		}
		ctx := context.Background()
		for step := range 60 {
			switch random.IntN(15) {
			case 0:
				clock.Add(int64(failureRepeatInterval + time.Minute))
			case 1:
				clock.Add(int64(failureLogForget + time.Minute))
			}
			l := trips[random.IntN(len(trips))]
			var failures []transform.RuleFailure
			complete := random.IntN(8) != 0
			for i, rule := range c.Metrics {
				// Rules alike in name, expression and items are one rule,
				// logged when either is under log.
				twin, logged := false, false
				for j, other := range c.Metrics {
					if other.Name == rule.Name && other.Expression == rule.Expression && other.Items == rule.Items {
						twin = twin || j < i
						logged = logged || other.ErrorMode == model.ErrorModeLog
					}
				}
				if twin || random.IntN(3) != 0 {
					continue
				}
				if rule.ErrorMode == model.ErrorModeFail {
					complete = false
					continue
				}
				failed := 1 + random.Uint64N(4)
				failures = append(failures, transform.RuleFailure{
					Metric: rule.Name, Expression: rule.Expression, Items: rule.Items, Failures: failed, Missing: random.Uint64N(failed + 1),
					First:  model.Errorf("metric %q value %d is missing for item %d", rule.Name, random.IntN(2), model.Position(random.IntN(9))),
					Logged: logged,
				})
			}
			now.logRuleFailures(ctx, configRead{}, &c, failures, l, complete)
			logRuleFailuresAskingEveryRule(ctx, was, configRead{}, &c, failures, l, complete)
			if logs.String() != wasLogs.String() {
				t.Fatalf("seed %d, scrape %d of %q, with the rules %+v failing %+v, complete %v: the log is\n%s\nwant as it was\n%s", seed, step, l.key, c.Metrics, failures, complete, logs, wasLogs)
			}
			for kind := range kinds {
				kinds[kind] += strings.Count(logs.String(), kind)
			}
			logs.Reset()
			wasLogs.Reset()
		}
	}
	for kind, lines := range kinds {
		if lines < 100 {
			t.Errorf("only %d lines with %s were compared", lines, kind)
		}
	}
}

// A scrape of a trip none of whose rules has a failure remembered makes no
// rule's key: for thirty rules under log with expressions of 2 KB it
// allocates nothing for the log, where it allocated a key of more than 2 KB
// for each rule. It is so before any rule of the trip has failed, while a
// rule of another target fails on every scrape, and again once the trip's
// own rule has failed and recovered — which the log says, so the count the
// scrape asks for is not at zero while the failure is remembered.
func TestAScrapeWithNoRuleRememberedMakesNoKey(t *testing.T) {
	c := model.Collector{Name: "generated"}
	for i := range 30 {
		c.Metrics = append(c.Metrics, model.MetricRule{Name: fmt.Sprintf("m%d", min(i, 15)), Items: ".items[]", Expression: fmt.Sprintf(".values.e%d | %s", i, strings.Repeat("x", 2000)), ErrorMode: model.ErrorModeLog})
	}
	server, logs, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
	trip := func(target string) collectLog {
		return collectLog{key: failureKey(c.Name, target, ""), attrs: []any{"collector", c.Name, "target", target, "url", target}}
	}
	l, other := trip("http://target.example"), trip("http://other.example")
	failing := []transform.RuleFailure{{Metric: c.Metrics[7].Name, Expression: c.Metrics[7].Expression, Items: c.Metrics[7].Items, Failures: 1, First: model.Errorf("metric %q value is missing for item %d", "m7", model.Position(3)), Logged: true}}
	ctx := context.Background()
	healthy := func(what string) {
		t.Helper()
		if allocated := testing.AllocsPerRun(50, func() { server.logRuleFailures(ctx, configRead{}, &c, nil, l, true) }); allocated != 0 {
			t.Errorf("%s, a scrape of thirty rules that do not fail allocates %v times for the log, want none", what, allocated)
		}
		if lines := ruleLines(t, logs); len(lines) != 0 {
			t.Errorf("%s, a scrape on which nothing fails logs %v", what, lines)
		}
	}
	healthy("before any rule failed")
	server.logRuleFailures(ctx, configRead{}, &c, failing, other, true)
	if lines := ruleLines(t, logs); len(lines) != 1 {
		t.Fatalf("the failure of the other target's rule logs %v", lines)
	}
	healthy("while a rule of another target is remembered")
	server.logRuleFailures(ctx, configRead{}, &c, failing, l, true)
	server.logRuleFailures(ctx, configRead{}, &c, failing, l, true)
	server.logRuleFailures(ctx, configRead{}, &c, nil, l, true)
	lines := ruleLines(t, logs)
	if len(lines) != 3 || lines[2]["msg"] != "metric extraction recovered" || lines[2]["target"] != "http://target.example" || lines[2]["failures"] != float64(2) {
		t.Fatalf("the rule's failure, its repeat and its recovery log %v", lines)
	}
	healthy("after the trip's rule recovered")
	if !server.failures.remembersRules(other.key) || server.failures.remembersRules(l.key) {
		t.Errorf("the log remembers a rule of the other target: %v, and of the trip: %v; want true and false", server.failures.remembersRules(other.key), server.failures.remembersRules(l.key))
	}
}

// Two jq rules of one metric name that differ only in their items are two
// rules to the log: the items are part of what tells a rule apart. While the
// rule over .fast[] fails on every scrape, the rule over .slow[] starts to
// fail with the same words and is logged in full, as a warning, with its
// items; the first rule then recovers, which is logged with its items while
// the second, still failing, stays a repeat; and the second recovers by
// itself, with its own count. Were the items not in the key, the two would
// be one failure: the second rule's would be a repeat nobody sees, and
// neither would recover while the other fails.
func TestRulesThatDifferOnlyInItemsAreLoggedEachByItself(t *testing.T) {
	c := testutil.Collector("queues", "json")
	c.Transform = model.TransformConfig{Type: "jq"}
	queue := []model.LabelRule{{Name: "queue", Expression: ".name"}}
	c.Metrics = []model.MetricRule{
		{Name: "queue_jobs", Type: model.GaugeMetricType, Items: ".fast[]", Expression: ".jobs", ErrorMode: model.ErrorModeLog, Labels: queue},
		{Name: "queue_jobs", Type: model.GaugeMetricType, Items: ".slow[]", Expression: ".jobs", ErrorMode: model.ErrorModeLog, Labels: queue},
	}
	var body atomic.Pointer[string]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(target.Close)
	server, logs, _ := ruleLogServer(t, c)
	const missing = `metric "queue_jobs" value is missing for item 0`
	failed := func(level, items string, more ...any) map[string]any {
		pairs := append([]any{"metric", "queue_jobs", "expression", ".jobs", "items", items, "error_mode", "log", "failures", float64(1), "error", missing}, more...)
		return ruleLine(level, "metric extraction failed", "queues", target.URL, pairs...)
	}
	recovered := func(items string, failures int) map[string]any {
		return ruleLine("INFO", "metric extraction recovered", "queues", target.URL, "metric", "queue_jobs", "expression", ".jobs", "items", items, "stage", "metric", "failed_for", "0s", "failures", float64(failures))
	}
	for _, step := range []struct {
		what, fast, slow string
		want             []map[string]any
	}{
		{"the rule over .fast[] fails", "null", "1", []map[string]any{failed("WARN", ".fast[]")}},
		{"the rule over .slow[] starts to fail", "null", "null", []map[string]any{failed("DEBUG", ".fast[]", "repeat", true), failed("WARN", ".slow[]")}},
		{"the rule over .fast[] recovers", "1", "null", []map[string]any{failed("DEBUG", ".slow[]", "repeat", true), recovered(".fast[]", 2)}},
		{"the rule over .slow[] fails again", "1", "null", []map[string]any{failed("DEBUG", ".slow[]", "repeat", true)}},
		{"the rule over .slow[] recovers", "1", "1", []map[string]any{recovered(".slow[]", 3)}},
		{"nothing fails", "1", "1", nil},
	} {
		answer := fmt.Sprintf(`{"fast":[{"name":"a","jobs":%s},{"name":"b","jobs":2}],"slow":[{"name":"c","jobs":%s}]}`, step.fast, step.slow)
		body.Store(&answer)
		if answer := probeOnce(t, server, probePath("queues", target.URL, ""), nil); answer.Code != http.StatusOK {
			t.Fatalf("%s: answered %d: %s", step.what, answer.Code, answer.Body)
		}
		if lines := ruleLines(t, logs); !reflect.DeepEqual(lines, step.want) {
			t.Fatalf("%s: the rules' lines are\n%v\nwant\n%v", step.what, lines, step.want)
		}
	}
}

// Two rules alike in name, expression and items, one under ignore and one
// under log, are one rule that is logged, whichever of them comes first:
// through /probe its failure is one warning with the failures of both, its
// repeat is held back, and its recovery is logged, with no expression on the
// lines since the name is shared with no other rule. With the rule under
// ignore first nothing was logged at all, the first twin's mode deciding
// for both; that is older than the rules' being logged each by itself. The
// failures counted are what they were, those of both twins.
func TestTwinRulesUnderIgnoreAndLogAreLoggedInEitherOrder(t *testing.T) {
	for _, modes := range [][2]string{{model.ErrorModeIgnore, model.ErrorModeLog}, {model.ErrorModeLog, model.ErrorModeIgnore}} {
		t.Run(modes[0]+" then "+modes[1], func(t *testing.T) {
			c := testutil.Collector("twins", "json")
			c.Transform = model.TransformConfig{Type: "jq"}
			c.Metrics = []model.MetricRule{
				{Name: "twin", Type: model.GaugeMetricType, Expression: ".t", ErrorMode: modes[0], Labels: []model.LabelRule{{Name: "k", Value: "1"}}},
				{Name: "twin", Type: model.GaugeMetricType, Expression: ".t", ErrorMode: modes[1], Labels: []model.LabelRule{{Name: "k", Value: "2"}}},
			}
			var body atomic.Pointer[string]
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(*body.Load()))
			}))
			t.Cleanup(target.Close)
			server, logs, _ := ruleLogServer(t, c)
			failed := func(level string, more ...any) map[string]any {
				pairs := append([]any{"metric", "twin", "error_mode", "log", "failures", float64(2), "error", `metric "twin" value is missing`}, more...)
				return ruleLine(level, "metric extraction failed", "twins", target.URL, pairs...)
			}
			for _, step := range []struct {
				body string
				want []map[string]any
			}{
				{`{"t":null}`, []map[string]any{failed("WARN")}},
				{`{"t":null}`, []map[string]any{failed("DEBUG", "repeat", true)}},
				{`{"t":1}`, []map[string]any{ruleLine("INFO", "metric extraction recovered", "twins", target.URL, "metric", "twin", "stage", "metric", "failed_for", "0s", "failures", float64(2))}},
				{`{"t":1}`, nil},
			} {
				body.Store(&step.body)
				if answer := probeOnce(t, server, probePath("twins", target.URL, ""), nil); answer.Code != http.StatusOK {
					t.Fatalf("answered %d: %s", answer.Code, answer.Body)
				}
				if lines := ruleLines(t, logs); !reflect.DeepEqual(lines, step.want) {
					t.Fatalf("with the answer %s the rules' lines are\n%v\nwant\n%v", step.body, lines, step.want)
				}
			}
			if got := seriesValue(t, selfMetrics(t, server), `http_exporter_rule_failures_total{collector="twins",metric="twin"}`); got != 4 {
				t.Errorf("%v failures are counted for the twins, want the 2 of each of two scrapes", got)
			}
		})
	}
}

// BenchmarkLogRuleFailures is what the log costs a scrape of thirty rules
// under error_mode log, with short expressions and with expressions of 2 KB:
// when nothing fails and nothing is remembered, as on nearly every scrape,
// and when one rule fails on every scrape. /by_name is the same scrape while
// a metric name's rules were logged as one (logRuleFailuresByName), to
// compare with:
//
//	go test -run '^$' -bench 'LogRuleFailures' ./internal/exporter/
func BenchmarkLogRuleFailures(b *testing.B) {
	for _, length := range []int{10, 2000} {
		c := model.Collector{Name: "generated"}
		for i := range 30 {
			c.Metrics = append(c.Metrics, model.MetricRule{Name: fmt.Sprintf("m%d", i), Items: ".items[]", Expression: fmt.Sprintf(".values.e%d | %s", i, strings.Repeat("x", length)), ErrorMode: model.ErrorModeLog})
		}
		quiet := slog.New(slog.DiscardHandler)
		cfg := &model.Config{Collectors: []model.Collector{testutil.Collector(c.Name, "text")}}
		if err := config.Validate(cfg); err != nil {
			b.Fatal(err)
		}
		server := NewServer(config.NewManager(cfg, "", quiet), "python3", quiet)
		l := collectLog{key: failureKey(c.Name, "http://target.example", ""), attrs: []any{"collector", c.Name, "target", "http://target.example", "url", "http://target.example"}}
		ctx := context.Background()
		failing := []transform.RuleFailure{{Metric: c.Metrics[7].Name, Expression: c.Metrics[7].Expression, Items: c.Metrics[7].Items, Failures: 1, First: model.Errorf("metric %q value is missing for item %d", "m7", model.Position(3)), Logged: true}}
		for _, failures := range [][]transform.RuleFailure{nil, failing} {
			name := fmt.Sprintf("expression=%d/failing=%d", length, len(failures))
			b.Run(name+"/now", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					server.logRuleFailures(ctx, configRead{}, &c, failures, l, true)
				}
			})
			b.Run(name+"/by_name", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					logRuleFailuresByName(ctx, server, configRead{}, &c, failures, l, true)
				}
			})
		}
	}
}

// ruleFailuresByName is what the rule report held while it had one entry
// for each metric name, made of the report of each rule as RuleReport.add
// then made it: the oracle of TestRuleFailuresAreCountedByMetricNameAsBefore.
func ruleFailuresByName(failures []transform.RuleFailure) []transform.RuleFailure {
	var merged []transform.RuleFailure
next:
	for _, f := range failures {
		for i := range merged {
			if merged[i].Metric == f.Metric {
				merged[i].Failures += f.Failures
				merged[i].Missing += f.Missing
				if f.Logged && !merged[i].Logged {
					merged[i].Logged, merged[i].First = true, f.First
				}
				continue next
			}
		}
		merged = append(merged, transform.RuleFailure{Metric: f.Metric, Failures: f.Failures, Missing: f.Missing, First: f.First, Logged: f.Logged})
	}
	return merged
}

// The self-metrics count a rule's failures by its metric name, as they did:
// for generated csv collectors whose rules share names, under log and under
// ignore, and generated tables with cells that are empty or no number,
// http_exporter_rule_failures_total of each name and
// http_exporter_missing_keys_total are, after every scrape, what the cells
// of the rules' columns come to, and what the report came to while it had
// one entry for each name.
func TestRuleFailuresAreCountedByMetricNameAsBefore(t *testing.T) {
	columns := []string{"c0", "c1", "c2", "c3"}
	for seed := range uint64(12) {
		random := rand.New(rand.NewPCG(seed, 15))
		c := testutil.Collector("generated", "csv")
		c.Transform = model.TransformConfig{Type: "csv"}
		c.Limits.MaxResponseBytes = 1 << 16
		c.Metrics = nil
		for i := range 2 + random.IntN(5) {
			mode := model.ErrorModeLog
			if random.IntN(3) == 0 {
				mode = model.ErrorModeIgnore
			}
			c.Metrics = append(c.Metrics, model.MetricRule{
				Name: fmt.Sprintf("m%d", random.IntN(3)), Type: model.GaugeMetricType, Expression: columns[random.IntN(len(columns))], ErrorMode: mode,
				Labels: []model.LabelRule{{Name: "rule", Value: strconv.Itoa(i)}, {Name: "host", Expression: "host"}},
			})
		}
		var body atomic.Pointer[string]
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/csv")
			_, _ = w.Write([]byte(*body.Load()))
		}))
		t.Cleanup(target.Close)
		server, _, _ := ruleLogServer(t, c)
		validated := server.manager.Get().Collectors[0]

		// byCells counts what the cells come to, and byReport what the
		// report of each name came to.
		byCells, byReport := map[string]uint64{}, map[string]uint64{}
		var missingCells, missingReported uint64
		for scrape := range 6 {
			var table strings.Builder
			fmt.Fprintf(&table, "host,%s\n", strings.Join(columns, ","))
			for row := range 1 + random.IntN(5) {
				fmt.Fprintf(&table, "h%d", row)
				cells := map[string]string{}
				for _, column := range columns {
					cells[column] = []string{"1", "2.5", "", "n/a"}[random.IntN(4)]
					fmt.Fprintf(&table, ",%s", cells[column])
				}
				table.WriteString("\n")
				for _, rule := range c.Metrics {
					switch cells[rule.Expression] {
					case "":
						missingCells++
						byCells[rule.Name]++
					case "n/a":
						byCells[rule.Name]++
					}
				}
			}
			text := table.String()
			body.Store(&text)

			response := &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(text), Headers: http.Header{"Content-Type": {"text/csv"}}}
			decoded, err := decode.Decode(response, &validated)
			if err != nil {
				t.Fatal(err)
			}
			ctx, report := transform.WithRuleReport(transform.LeaveRuleLoggingToCaller(context.Background()))
			if _, err := transform.Transform(ctx, decoded, response, &validated, "python3"); err != nil {
				t.Fatal(err)
			}
			for _, f := range ruleFailuresByName(report.Failures()) {
				byReport[f.Metric] += f.Failures
				missingReported += f.Missing
			}

			if answer := probeOnce(t, server, probePath("generated", target.URL, ""), nil); answer.Code != http.StatusOK {
				t.Fatalf("seed %d: answered %d: %s", seed, answer.Code, answer.Body)
			}
			exposition := selfMetrics(t, server)
			for _, rule := range c.Metrics {
				counted := uint64(seriesValue(t, exposition, `http_exporter_rule_failures_total{collector="generated",metric="`+rule.Name+`"}`))
				if counted != byCells[rule.Name] || counted != byReport[rule.Name] {
					t.Fatalf("seed %d, scrape %d of\n%s: %d failures are counted for %s, the cells come to %d and the report by name to %d; the rules: %+v", seed, scrape, text, counted, rule.Name, byCells[rule.Name], byReport[rule.Name], c.Metrics)
				}
			}
			if missing := uint64(seriesValue(t, exposition, `http_exporter_missing_keys_total{collector="generated"}`)); missing != missingCells || missing != missingReported {
				t.Fatalf("seed %d, scrape %d: %d missing values are counted, the cells come to %d and the report by name to %d", seed, scrape, missing, missingCells, missingReported)
			}
		}
	}
}

// A debug probe's report shows each rule that carried on by itself, under
// its metric name, with its own count and first error; the rules of a name
// several rules export with their expression, and their items when they
// have any, and the rule of a name of its own as it was shown before. The
// lines of the report's log name the rules the same way.
func TestADebugProbeReportsEachRuleOfAMetricName(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newDiskTarget(t)
	table := wholeDisks()
	table.used[1], table.used[3], table.free[2], table.inodes[0] = "", "", "n/a", ""
	target.serve(table)
	queues := testutil.Collector("queues", "json")
	queues.Transform = model.TransformConfig{Type: "jq"}
	queue := []model.LabelRule{{Name: "queue", Expression: ".name"}}
	queues.Metrics = []model.MetricRule{
		{Name: "queue_jobs", Type: model.GaugeMetricType, Items: ".fast[]", Expression: ".jobs", ErrorMode: model.ErrorModeLog, Labels: queue},
		{Name: "queue_jobs", Type: model.GaugeMetricType, Items: ".slow[]", Expression: ".jobs", ErrorMode: model.ErrorModeIgnore, Labels: queue},
	}
	jobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fast":[{"name":"a","jobs":null},{"name":"b","jobs":2}],"slow":[{"name":"c","jobs":"many"},{"name":"d","jobs":null}]}`))
	}))
	t.Cleanup(jobs.Close)
	server := modeServer(t, diskCollector("disks", model.ErrorModeLog), queues)
	server.SetProbeDebug(true)

	report := debugProbeGet(t, server, "collector=disks&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, report,
		"  Rules that carried on without some series\n"+
			`    disk_bytes (expression "//disk/used"): 2 failed, 2 of them missing values; first: `+fmt.Sprintf(usedBlank, 1)+"\n"+
			`    disk_bytes (expression "//disk/free"): 1 failed; first: `+fmt.Sprintf(freeNoNumber, 2)+"\n"+
			`    disk_inodes: 1 failed, 1 of them missing values; first: metric "disk_inodes" value is missing for node 0: XPath "//disk/inodes" selected a node without a value`+"\n",
		`level=WARN msg="metric extraction failed" collector=disks target=`+target.URL+` metric=disk_bytes expression=//disk/used error_mode=log failures=2 error=`,
		`level=WARN msg="metric extraction failed" collector=disks target=`+target.URL+` metric=disk_bytes expression=//disk/free error_mode=log failures=1 error=`,
		`level=WARN msg="metric extraction failed" collector=disks target=`+target.URL+` metric=disk_inodes error_mode=log failures=1 error=`,
	)

	report = debugProbeGet(t, server, "collector=queues&debug=true&target="+url.QueryEscape(jobs.URL)).Body.String()
	assertContains(t, report,
		"  Rules that carried on without some series\n"+
			`    queue_jobs (expression ".jobs", items ".fast[]"): 1 failed, 1 of them missing values; first: metric "queue_jobs" value is missing for item 0`+"\n"+
			`    queue_jobs (expression ".jobs", items ".slow[]"): 2 failed, 1 of them missing values; first: metric "queue_jobs" item 0: value "many" is not a number; map text to numbers with value_map`+"\n",
		`metric=queue_jobs expression=.jobs items=.fast[] error_mode=log failures=1 error=`,
	)
	if strings.Contains(report, "items=.slow[]") {
		t.Errorf("the rule under ignore is logged in the report:\n%s", report)
	}
}

// A prometheus rule without a name is shown in a debug probe's report as a
// rule without a name, where the line began with nothing: with its
// expression when the collector has several such rules, which share the
// empty name, and by itself when it has one. Every other line is as it was:
// a named rule under its name, and a named rule that matches by its name,
// having no expression, beside one of its name that has, with the empty
// expression that tells it apart, which is what its log line carries too.
func TestADebugProbeShowsARuleWithoutANameAsThat(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("app_other 2\n"))
	}))
	t.Cleanup(target.Close)
	const carriedOn = "  Rules that carried on without some series\n"
	for name, tc := range map[string]struct {
		rules []model.MetricRule
		want  []string
		log   string
	}{
		"several rules without a name": {
			rules: []model.MetricRule{
				{Expression: "^app_jobs", ErrorMode: model.ErrorModeLog},
				{Expression: "^app_queue", ErrorMode: model.ErrorModeIgnore},
				{Name: "workers", Expression: "^app_workers$", ErrorMode: model.ErrorModeLog},
			},
			want: []string{
				`    rule without a name (expression "^app_jobs"): 1 failed, 1 of them missing values; first: expression "^app_jobs" matched no metric in the response`,
				`    rule without a name (expression "^app_queue"): 1 failed, 1 of them missing values; first: expression "^app_queue" matched no metric in the response`,
				`    workers: 1 failed, 1 of them missing values; first: metric "workers" expression "^app_workers$" matched no metric in the response`,
			},
			log: `"metric":"","expression":"^app_jobs","error_mode":"log"`,
		},
		"one rule without a name": {
			rules: []model.MetricRule{
				{Expression: "^app_jobs", ErrorMode: model.ErrorModeLog},
				{Name: "workers", Expression: "^app_workers$", ErrorMode: model.ErrorModeLog},
			},
			want: []string{
				`    rule without a name: 1 failed, 1 of them missing values; first: expression "^app_jobs" matched no metric in the response`,
				`    workers: 1 failed, 1 of them missing values; first: metric "workers" expression "^app_workers$" matched no metric in the response`,
			},
			log: `"metric":"","error_mode":"log"`,
		},
		"a named rule that matches by its name": {
			rules: []model.MetricRule{
				{Name: "app_workers", ErrorMode: model.ErrorModeLog},
				{Name: "app_workers", Expression: "^app_idle$", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "state", Value: "idle"}}},
			},
			want: []string{
				`    app_workers (expression ""): 1 failed, 1 of them missing values; first: metric "app_workers" is not in the response`,
				`    app_workers (expression "^app_idle$"): 1 failed, 1 of them missing values; first: metric "app_workers" expression "^app_idle$" matched no metric in the response`,
			},
			log: `"metric":"app_workers","expression":"","error_mode":"log"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := testutil.Collector("pr", "prometheus")
			c.Transform = model.TransformConfig{Type: "prometheus"}
			c.Metrics = tc.rules
			server, logs, _ := ruleLogServer(t, c)
			if answer := probeOnce(t, server, probePath("pr", target.URL, ""), nil); answer.Code != http.StatusOK {
				t.Fatalf("answered %d: %s", answer.Code, answer.Body)
			}
			assertContains(t, logs.String(), tc.log)
			server.SetProbeDebug(true)
			report := debugProbeGet(t, server, "collector=pr&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
			assertContains(t, report, carriedOn+strings.Join(tc.want, "\n")+"\n\nLogs\n")
		})
	}
}

// Under error_mode ignore two rules of one metric name that fail are
// counted, under the name, and logged nowhere, at any level, on any scrape:
// neither as failing nor, once they work again, as recovered. A debug
// probe's report shows each of them, and has no line of them in its log.
func TestRulesOfOneNameUnderIgnoreAreCountedAndNotLogged(t *testing.T) {
	target := newDiskTarget(t)
	server, logs, _ := ruleLogServer(t, diskCollector("disks", model.ErrorModeIgnore))
	for scrape, change := range []func(*disks){
		func(d *disks) { d.used[1], d.free[2] = "", "n/a" },
		func(d *disks) { d.used[1], d.used[2], d.free[0] = "", "", "" },
		func(*disks) {},
	} {
		table := wholeDisks()
		change(&table)
		target.serve(table)
		if answer := probeOnce(t, server, probePath("disks", target.URL, ""), nil); answer.Code != http.StatusOK {
			t.Fatalf("scrape %d: answered %d: %s", scrape, answer.Code, answer.Body)
		}
	}
	if lines := ruleLines(t, logs); len(lines) != 0 {
		t.Errorf("rules under ignore are logged: %v", lines)
	}
	// Reported they are, each by itself, to whoever asks with a debug probe.
	table := wholeDisks()
	table.used[1], table.free[2] = "", "n/a"
	target.serve(table)
	server.SetProbeDebug(true)
	report := debugProbeGet(t, server, "collector=disks&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, report,
		"  Rules that carried on without some series\n"+
			`    disk_bytes (expression "//disk/used"): 1 failed, 1 of them missing values; first: `+fmt.Sprintf(usedBlank, 1)+"\n"+
			`    disk_bytes (expression "//disk/free"): 1 failed; first: `+fmt.Sprintf(freeNoNumber, 2)+"\n",
		"Logs\n  none\n",
	)
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_rule_failures_total{collector="disks",metric="disk_bytes"}`:  5,
		`http_exporter_rule_failures_total{collector="disks",metric="disk_inodes"}`: 0,
		`http_exporter_missing_keys_total{collector="disks"}`:                       4,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

// An XPath engine failure of the second rule of a metric name is logged
// though the first rule of the name has been failing since earlier scrapes.
// The first rule finds a cell without a value on every scrape, logged once
// and then held back as a repeat; then an element gets the attribute the
// second rule's predicate stumbles over, and the engine's failure is
// logged in full, as a warning, with the expression it is of. Before, the
// name's one line was the first rule's, a repeat, and nothing said the
// second rule's series were gone. Counted under the name are both rules'
// failures, and when the engine reads the answer again the second rule's
// recovery is logged, the first still failing.
func TestAnEngineFailureOfTheSecondRuleOfANameIsLogged(t *testing.T) {
	const (
		value  = "//a/v"
		marked = "count(//a[contains(@x, 5)])"
		engine = `metric "cells" XPath "count(//a[contains(@x, 5)])" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`
		blank  = `metric "cells" value is missing for node 0: XPath "//a/v" selected a node without a value`
	)
	c := testutil.Collector("cells", "xml")
	c.Transform = model.TransformConfig{Type: "xpath"}
	c.Metrics = []model.MetricRule{
		{Name: "cells", Type: model.GaugeMetricType, Expression: value, ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "kind", Value: "value"}}},
		{Name: "cells", Type: model.GaugeMetricType, Expression: marked, ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "kind", Value: "marked"}}},
	}
	var body atomic.Pointer[string]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(target.Close)
	server, logs, _ := ruleLogServer(t, c)
	failed := func(level, expression, said string, more ...any) map[string]any {
		pairs := append([]any{"metric", "cells", "expression", expression, "error_mode", "log", "failures", float64(1), "error", said}, more...)
		return ruleLine(level, "metric extraction failed", "cells", target.URL, pairs...)
	}
	const reads, stumbles = `<r><a><v></v></a><a><v>3</v></a></r>`, `<r><a x="5"><v></v></a><a><v>3</v></a></r>`
	for _, step := range []struct {
		what, body string
		series     []string
		want       []map[string]any
	}{
		{"the first rule fails", reads, []string{`cells{kind="value"} 3`, `cells{kind="marked"} 0`},
			[]map[string]any{failed("WARN", value, blank)}},
		{"it fails again", reads, []string{`cells{kind="value"} 3`, `cells{kind="marked"} 0`},
			[]map[string]any{failed("DEBUG", value, blank, "repeat", true)}},
		{"the engine fails on the second rule", stumbles, []string{`cells{kind="value"} 3`},
			[]map[string]any{failed("DEBUG", value, blank, "repeat", true), failed("WARN", marked, engine)}},
		{"the engine fails again", stumbles, []string{`cells{kind="value"} 3`},
			[]map[string]any{failed("DEBUG", value, blank, "repeat", true), failed("DEBUG", marked, engine, "repeat", true)}},
		{"the engine reads the answer again", reads, []string{`cells{kind="value"} 3`, `cells{kind="marked"} 0`},
			[]map[string]any{failed("DEBUG", value, blank, "repeat", true),
				ruleLine("INFO", "metric extraction recovered", "cells", target.URL, "metric", "cells", "expression", marked, "stage", "metric", "failed_for", "0s", "failures", float64(2))}},
	} {
		body.Store(&step.body)
		answer := probeOnce(t, server, probePath("cells", target.URL, ""), nil)
		if answer.Code != http.StatusOK {
			t.Fatalf("%s: answered %d: %s", step.what, answer.Code, answer.Body)
		}
		for _, series := range step.series {
			if !strings.Contains(answer.Body.String(), "\n"+series+"\n") {
				t.Errorf("%s: no series %s in the answer:\n%s", step.what, series, answer.Body)
			}
		}
		if strings.Contains(answer.Body.String(), `kind="marked"`) != (len(step.series) == 2) {
			t.Errorf("%s: the answer is\n%s\nwant the series %v", step.what, answer.Body, step.series)
		}
		if lines := ruleLines(t, logs); !reflect.DeepEqual(lines, step.want) {
			t.Fatalf("%s: the rules' lines are\n%v\nwant\n%v", step.what, lines, step.want)
		}
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_rule_failures_total{collector="cells",metric="cells"}`); got != 7 {
		t.Errorf("%v failures are counted for the name, want the 5 of the first rule and the 2 of the second", got)
	}
}

// A static target's scrape logs its rules as a probe does: each rule of a
// metric name by itself, named by its expression, with the target's name
// and address, and recovering by itself.
func TestAStaticTargetLogsEachRuleOfAMetricName(t *testing.T) {
	target := newDiskTarget(t)
	testutil.CaptureLogs(t)
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "store", Collector: "disks", Target: target.URL}}}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{diskCollector("disks", model.ErrorModeLog)}}, file)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: withoutTime}))
	identify := regexp.MustCompile(`"level":"([A-Z]+)","msg":"(metric extraction [a-z]+)".*"target":"store".*"metric":"disk_bytes","expression":"([^"]+)"`)
	for _, step := range []struct {
		change func(*disks)
		want   []string
	}{
		{func(d *disks) { d.used[1] = "" }, []string{"WARN metric extraction failed //disk/used"}},
		{func(d *disks) { d.used[1], d.free[2] = "", "n/a" }, []string{"DEBUG metric extraction failed //disk/used", "WARN metric extraction failed //disk/free"}},
		{func(d *disks) { d.free[2] = "n/a" }, []string{"DEBUG metric extraction failed //disk/free", "INFO metric extraction recovered //disk/used"}},
	} {
		table := wholeDisks()
		step.change(&table)
		target.serve(table)
		server.scrapeStaticTargets(context.Background(), 10*time.Second)
		var got []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if found := identify.FindStringSubmatch(line); found != nil {
				got = append(got, strings.Join(found[1:], " "))
			}
		}
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("the scrape's lines of the rules are %q, want %q; the log:\n%s", got, step.want, logs)
		}
		logs.Reset()
	}
}
