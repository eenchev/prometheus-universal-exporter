//go:build !select_request_types || request_type_http

package allocator

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The input files most tests start from: a configuration with a collector
// of its own and one in a collector file, a static target file and a probe
// targets file.
const (
	testConfig = `collector_files: [teams/*.yaml]
` + `collectors:
` + `  - name: demo
    request:
      type: http
      path: /status
    transform:
      type: regex
    metrics:
      - name: demo_value
        expression: 'value=(\d+)'
`
	testCollectorFile = `collectors:
  - name: other
    request:
      type: http
    transform:
      type: regex
    metrics:
      - name: other_value
        expression: 'value=(\d+)'
`
	testProbes = `probe_jobs:
  - name: websites
    collector: demo
    params:
      timeout: 10s
    labels:
      team: web
      empty: ""
    targets:
      - https://shop.example
      - https://docs.example
`
)

// staticTargets is a static target file of n targets of demo, every third
// with an interval of its own, 1m, where the file's is 30s.
func staticTargets(n int) string {
	var b strings.Builder
	b.WriteString("interval: 30s\nconcurrency: 4\ntargets:\n")
	for i := range n {
		fmt.Fprintf(&b, "  - name: t%03d\n    collector: demo\n    target: http://t%03d.example\n", i, i)
		if i%3 == 0 {
			b.WriteString("    interval: 1m\n")
		}
	}
	return b.String()
}

// writeInputs writes the input files into a fresh directory, each that is
// not "" given, and returns their paths.
func writeInputs(t *testing.T, configText, staticText, probesText string) Paths {
	t.Helper()
	dir := t.TempDir()
	p := Paths{Config: testutil.WriteIn(t, dir, "main.yaml", configText)}
	testutil.WriteIn(t, dir, "teams/a.yaml", testCollectorFile)
	if staticText != "" {
		p.StaticTargets = testutil.WriteIn(t, dir, "targets.yaml", staticText)
	}
	if probesText != "" {
		p.ProbeTargets = testutil.WriteIn(t, dir, "probes.yaml", probesText)
	}
	return p
}

func mustLoad(t *testing.T, p Paths) *Inputs {
	t.Helper()
	in, err := LoadInputs(p)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// settledAllocator is an allocator of in whose member list has formed of
// members, on a clock of the test's own.
func settledAllocator(t *testing.T, in *Inputs, members ...string) (*Allocator, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	a := New(in, Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	beat(t, a, members...)
	keepBeating(t, a, clock, 20*time.Second, members...)
	if got := effective(a); !slices.Equal(got, members) {
		t.Fatalf("got the member list %v, want %v", got, members)
	}
	return a, clock
}

// share is what the bundle of member holds of the static targets: their
// names, in order, after the replica's own loader read them from the bundle
// written into a directory, as the exporter in allocator mode does.
func share(t *testing.T, a *Allocator, member string) []string {
	t.Helper()
	b := a.Bundle(member)
	if b == nil {
		t.Fatalf("no bundle for %s", member)
	}
	_, targets := loadBundle(t, b)
	if targets == nil {
		return nil
	}
	names := make([]string, 0, len(targets.Targets))
	for i := range targets.Targets {
		names = append(names, targets.Targets[i].Name)
	}
	return names
}

// loadBundle writes b into a directory and loads it there with the
// exporter's loader, failing the test when the replica would refuse it.
// targets is nil for a bundle without static targets.
func loadBundle(t *testing.T, b *allocapi.Bundle) (*model.Config, *model.StaticTargetFile) {
	t.Helper()
	if err := allocapi.CheckFiles(b.Files); err != nil {
		t.Fatal(err)
	}
	if got := allocapi.Generation(b.Files); got != b.Generation {
		t.Fatalf("the bundle's generation %s is not its files' %s", b.Generation, got)
	}
	dir := t.TempDir()
	for name, body := range b.Files {
		testutil.WriteIn(t, dir, filepath.FromSlash(name), body)
	}
	conf, err := config.Load(filepath.Join(dir, allocapi.ConfigFile))
	if err != nil {
		t.Fatalf("the replica would refuse the bundle's configuration: %v", err)
	}
	path := filepath.Join(dir, allocapi.StaticTargetsFile)
	if _, err := os.Stat(path); err != nil {
		return conf, nil
	}
	targets, err := config.LoadStaticTargets(path)
	if err == nil {
		err = config.ValidateStaticTargets(targets)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(targets, conf)
	}
	if err != nil {
		t.Fatalf("the replica would refuse the bundle's static targets: %v\n%s", err, b.Files[allocapi.StaticTargetsFile])
	}
	return conf, targets
}

// pending reports whether a timer due at at is pending, as a long poll's
// wait is once the request waits.
func (c *fakeClock) pending(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if !t.stopped && !t.done && t.at.Equal(at) {
			return true
		}
	}
	return false
}
