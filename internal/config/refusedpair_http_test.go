//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A reload asks whether a static target file agrees with a configuration up
// to five times (Manager.apply): for the pair it read, for each file it read
// with the other as in force, and again for the error of each file it
// refuses. It asked the same pair again each time, and each check costs 5 to
// 6 µs a target, all of it under the reload lock. Now each pair is checked
// once in a reload, and asked again it gives the verdict it gave.

// pairCollectors is a configuration of http collectors of the names given.
func pairCollectors(names ...string) string {
	var b strings.Builder
	b.WriteString("collectors:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "  - name: %s\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: %s_value\n        expression: 'value=(\\d+)'\n", name, name)
	}
	return b.String()
}

// pairTargets is a static target file of n targets, each of collector a but
// the last, which is of collector last: a check that refuses the file for
// it does so at its last target, having checked every other.
func pairTargets(n int, last string) string {
	var b strings.Builder
	b.WriteString("interval: 1m\ntargets:\n")
	for i := range n {
		collector := "a"
		if i == n-1 {
			collector = last
		}
		fmt.Fprintf(&b, "  - name: t%d\n    collector: %s\n    target: http://t%d.invalid\n", i, collector, i)
	}
	return b.String()
}

// pairReload is a reload of the table below: the pair in force, the files
// read and what they hold (a configuration of no collectors is not read),
// what goes in, and how many times the pair was checked, before and now.
type pairReload struct {
	name                         string
	inForce, read                []string
	inForceLast, readLast        string
	readTargets                  bool
	configInstalled, targetsDone bool
	was, want                    int
}

// pairReloads are the reloads the checks are counted for: one each of the
// table of the brief, and the one in which the target file goes alone.
var pairReloads = []pairReload{
	{name: "both_agree", inForce: []string{"a"}, read: []string{"a", "c"}, inForceLast: "a", readLast: "a", readTargets: true, configInstalled: true, targetsDone: true, was: 1, want: 1},
	{name: "targets_refused", inForce: []string{"a"}, inForceLast: "a", readLast: "b", readTargets: true, was: 3, want: 1},
	{name: "config_refused", inForce: []string{"a", "z"}, read: []string{"a"}, inForceLast: "z", was: 3, want: 1},
	{name: "config_alone", inForce: []string{"a"}, read: []string{"a", "c"}, inForceLast: "a", readLast: "b", readTargets: true, configInstalled: true, was: 4, want: 2},
	{name: "targets_alone", inForce: []string{"a", "z"}, read: []string{"a"}, inForceLast: "z", readLast: "z", readTargets: true, targetsDone: true, was: 4, want: 3},
	{name: "neither_alone", inForce: []string{"a", "z"}, read: []string{"a"}, inForceLast: "z", readLast: "b", readTargets: true, was: 5, want: 3},
}

// setUp writes the pair in force and loads it, then writes the files the
// reload reads, and returns a function that makes a manager of the pair in
// force, as many times as asked, and the reload to make with it.
func (r pairReload) setUp(tb testing.TB, targets int) (manager func() *Manager, reload func(*Manager) error) {
	tb.Helper()
	dir := tb.TempDir()
	configPath, targetsAt := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "targets.yaml")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	write(configPath, pairCollectors(r.inForce...))
	write(targetsAt, pairTargets(targets, r.inForceLast))
	cfg, err := Load(configPath)
	if err != nil {
		tb.Fatal(err)
	}
	file, err := LoadStaticTargets(targetsAt)
	if err == nil {
		err = ValidateStaticTargets(file)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		tb.Fatal(err)
	}
	if r.read != nil {
		write(configPath, pairCollectors(r.read...))
	}
	if r.readTargets {
		write(targetsAt, pairTargets(targets, r.readLast))
	}
	manager = func() *Manager {
		m := NewManager(cfg, configPath, slog.New(slog.DiscardHandler))
		m.SetTargets(targetsAt, file)
		return m
	}
	return manager, func(m *Manager) error { return m.apply(reloadTriggerWatch, r.read != nil, r.readTargets) }
}

// A reload checks each pair of a static target file and a configuration it
// asks about once: once when the files it read agree, once for a file read
// alone and refused for the other in force, twice when both were read and
// one goes alone with the other in force — the pair read, and the file that
// went with the other in force, the pair read being also the refused file
// with the one that went — and three times when neither goes, or the target
// file goes alone: the pair read, and each file with the other in force. It
// checked them one to five times. What goes in force is what the case says.
func TestAReloadChecksEachPairOfFilesOnce(t *testing.T) {
	for _, r := range pairReloads {
		t.Run(r.name, func(t *testing.T) {
			manager, reload := r.setUp(t, 3)
			m := manager()
			inForce, targetsInForce := m.InForce()
			checks := countTargetChecks(t)
			err := reload(m)
			if got := checks.Load(); got != int64(r.want) {
				t.Errorf("the reload checked the target file against a configuration %d times, want %d (it was %d)", got, r.want, r.was)
			}
			cfg, file := m.InForce()
			refused := r.read != nil && !r.configInstalled || r.readTargets && !r.targetsDone
			if (cfg != inForce) != r.configInstalled || (file != targetsInForce) != r.targetsDone || (err != nil) != refused {
				t.Errorf("configuration installed %t, target file installed %t, error %v; want %t and %t", cfg != inForce, file != targetsInForce, err, r.configInstalled, r.targetsDone)
			}
		})
	}
}

// BenchmarkRefusedPairReload is each reload of pairReloads at 10,000
// targets, the configuration and the target file read from disk as a
// reload reads them; checks/op is how many times the target file was
// checked against a configuration. Only the reload is timed.
func BenchmarkRefusedPairReload(b *testing.B) {
	for _, r := range pairReloads {
		b.Run(r.name, func(b *testing.B) {
			manager, reload := r.setUp(b, 10_000)
			checks := countTargetChecks(b)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				m := manager()
				b.StartTimer()
				_ = reload(m)
			}
			b.ReportMetric(float64(checks.Load())/float64(b.N), "checks/op")
		})
	}
}

// Over generated sequences of reloads of http collectors and their targets —
// a configuration accepted, refused for itself or refused for the target
// file, a target file likewise, one read or both, with the watch on and off
// — a reload that checks each pair once leaves what the reload that checked
// a pair each time it asked left (oldApply): the same files in force, the
// same errors and log lines in the same order, the same reload metrics and
// the same files watched, stamped alike.
func TestAReloadThatChecksEachPairOnceDoesWhatItDid(t *testing.T) {
	otlp := "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n"
	exported := strings.Replace(pairTargets(2, "b"), "    target: http://t1.invalid\n", "    target: http://t1.invalid\n    export_via_otlp: true\n", 1)
	sequences := alloctest.UnlessRaced(100, 20)
	outcomes := compareReloads(t, sequences, func(_ *testing.T, dir string) []reloadFile {
		return []reloadFile{
			{path: filepath.Join(dir, "config.yaml"), variants: []string{
				pairCollectors("a", "b"), pairCollectors("a"), pairCollectors("a", "c"), pairCollectors("b"), pairCollectors("a", "b", "c"),
				otlp + pairCollectors("a", "b"), "collectors: [\n",
			}},
			{path: filepath.Join(dir, "targets.yaml"), variants: []string{
				pairTargets(2, "b"), pairTargets(2, "a"), pairTargets(2, "c"), pairTargets(1, "b"), pairTargets(2, "missing"),
				exported, "targets: [\n", "interval: soon\ntargets: []\n",
			}},
		}
	})
	least := sequences / 10
	wantOutcomes(t, outcomes, least, "file installed", "refused for itself, watch true", "refused for itself, watch false", "refused for the other, watch true",
		"refused for the other, watch false", "one went alone, the other refused for it", "both refused for each other")
}
