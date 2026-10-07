//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// targetCheckPair is a valid configuration of collectors http collectors and
// a static target file of targets targets: each of the collector at its own
// place, or with last all of the last one, the collector that going through
// them finds latest. Every third collector, counted from the last, has a
// placeholder in its path, which its targets' params fill, so that the check
// binds parameters as it does for such a collector.
func targetCheckPair(tb testing.TB, collectors, targets int, last bool) (*model.StaticTargetFile, *model.Config) {
	tb.Helper()
	cfg := &model.Config{Collectors: make([]model.Collector, collectors)}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: make([]model.StaticTarget, targets)}
	for i := range collectors {
		cfg.Collectors[i] = testutil.Collector(fmt.Sprintf("collector_%05d", i), "text")
		if (collectors-1-i)%3 == 0 {
			cfg.Collectors[i].Request.Path = "/tenants/{{param_tenant}}/status"
		}
	}
	for i := range targets {
		of := i
		if last {
			of = collectors - 1
		}
		target := model.StaticTarget{Name: fmt.Sprintf("target_%05d", i), Collector: cfg.Collectors[of].Name, Target: fmt.Sprintf("http://target-%d.invalid", i)}
		if cfg.Collectors[of].Request.Path != "" {
			target.Params = map[string]string{"param_tenant": "acme"}
		}
		file.Targets[i] = target
	}
	if err := Validate(cfg); err != nil {
		tb.Fatal(err)
	}
	if err := ValidateStaticTargets(file); err != nil {
		tb.Fatal(err)
	}
	return file, cfg
}

// targetCheckSizes are how many collectors, and as many targets, the
// benchmarks of the check are run with.
var targetCheckSizes = []int{100, 2000, 10000}

// BenchmarkValidateStaticTargetsAgainst measures the check of a static
// target file against a configuration, which a start makes once and a reload
// up to five times. own is n targets and n collectors, each target of its
// own collector, the most that a check which went through the collectors
// for every target had to go through; last is every target of the last
// collector, the farthest one, whose path has a placeholder; and one is a
// single target, of the last collector, which is what the check costs for
// the configuration alone. indexes/op is how many times the check noted
// where the collectors of a configuration are by their names
// (collectorsIndexedHook): once, whatever the targets.
func BenchmarkValidateStaticTargetsAgainst(b *testing.B) {
	for _, n := range targetCheckSizes {
		for _, shape := range []struct {
			name    string
			targets int
			last    bool
		}{{"own", n, false}, {"last", n, true}, {"one", 1, true}} {
			b.Run(fmt.Sprintf("%s/n=%d", shape.name, n), func(b *testing.B) {
				file, cfg := targetCheckPair(b, n, shape.targets, shape.last)
				indexed := countCollectorIndexes(b)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := ValidateStaticTargetsAgainst(file, cfg); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(indexed.Load())/float64(b.N), "indexes/op")
			})
		}
	}
}

// BenchmarkTargetsChecked measures finding the collectors whose descriptor
// files the check of a static target file opens (targetsChecked), which a
// reload that reads the target file does for the configuration read and the
// one in force: n targets and n collectors, each target of its own
// collector, with none of the targets setting a request.message, which is
// every target file without a grpc collector, with every one setting it,
// and with one alone, the last, setting it.
func BenchmarkTargetsChecked(b *testing.B) {
	for _, n := range targetCheckSizes {
		for _, shape := range []struct {
			name string
			sets func(i int) bool
			want int
		}{
			{"no_message", func(int) bool { return false }, 0},
			{"every_message", func(int) bool { return true }, n},
			{"one_message", func(i int) bool { return i == n-1 }, 1},
		} {
			b.Run(fmt.Sprintf("%s/n=%d", shape.name, n), func(b *testing.B) {
				file, cfg := targetCheckPair(b, n, n, false)
				for i := range file.Targets {
					if shape.sets(i) {
						file.Targets[i].Request.Message = "{}"
					}
				}
				indexed := countCollectorIndexes(b)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if checked := targetsChecked(file, cfg); len(checked.Collectors) != shape.want {
						b.Fatalf("%d collectors found, want %d", len(checked.Collectors), shape.want)
					}
				}
				b.ReportMetric(float64(indexed.Load())/float64(b.N), "indexes/op")
			})
		}
	}
}
