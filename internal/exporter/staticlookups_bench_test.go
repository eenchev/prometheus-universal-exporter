//go:build !select_request_types || request_type_http

package exporter

// Benchmarks of what the exporter looks up by a name for a static target,
// for 100, 2,000 and 10,000 static targets of as many collectors
// (docs/DEVELOPMENT.md):
//
//	go test -run '^$' -bench 'StaticTargetLookup|SelfMetricsSeedStatic' ./internal/exporter/
//
// BenchmarkStaticTargetLookup is what a scrape of the last static target
// asks when it ends, whether a target of its name is in force (in-force),
// that scrape whole, answered from the cache (scrape), and what a read of
// the static targets endpoint that names that one target asks (named).
// BenchmarkSelfMetricsSeedStatic is what a read of the verbose self-metrics
// does for the static targets: finding the request of each (keys), and that
// with the tracker told of them (seed). A configuration of 10,000 collectors
// takes seconds to load, which are not timed and are waited for all the
// same when that size is among those asked for: -bench
// 'StaticTargetLookup/n=2000$' asks for one size.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// staticLookupSizes are how many static targets, each of a collector of its
// own, the benchmarks of the lookups by name have.
var staticLookupSizes = []int{100, 2000, 10000}

// staticLookupServer is a server of n caching collectors with verbose
// self-metrics and one static target of each, all at address, as a reload
// would have read and checked them, with what it follows.
func staticLookupServer(tb testing.TB, n int, address string) (*Server, *followedConfig) {
	tb.Helper()
	manager := followBenchManager(tb, "web:\n  self_metrics:\n    verbose: true\n"+cachedDocument(followBenchNames(0, n)...), false)
	cfg := manager.Get()
	file := &model.StaticTargetFile{Interval: model.Duration(time.Hour)}
	for i := range cfg.Collectors {
		name := cfg.Collectors[i].Name
		file.Targets = append(file.Targets, model.StaticTarget{Name: "t_" + name, Collector: name, Target: address})
	}
	err := config.ValidateStaticTargets(file)
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		tb.Fatal(err)
	}
	manager.SetTargets("", file)
	server := NewServer(manager, "python3", slog.New(slog.DiscardHandler))
	return server, server.followedInForce()
}

// BenchmarkStaticTargetLookup is the lookups by a static target's name: see
// the top of the file.
func BenchmarkStaticTargetLookup(b *testing.B) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	for _, n := range staticLookupSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			server, followed := staticLookupServer(b, n, target.URL)
			targets := staticTargetsOf(followed.targets)
			last := targets[len(targets)-1]
			b.Run("in-force", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if !server.staticTargetInForce(last.Name) {
						b.Fatal("the last static target is not in force")
					}
				}
			})
			b.Run("scrape", func(b *testing.B) {
				// The first scrape goes to the target, and the ones timed
				// are answered from the cache: what is left of a scrape is
				// then what the exporter does around the trip.
				server.scrapeTargetSince(context.Background(), followed.config, followed.generation, last)
				if len(publishedOf(server, last.Name)) == 0 {
					b.Fatal("the scrape of the last static target published nothing")
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					server.scrapeTargetSince(context.Background(), followed.config, followed.generation, last)
				}
			})
			b.Run("named", func(b *testing.B) {
				query := url.Values{staticTargetsParam: {last.Name}}
				b.ReportAllocs()
				for range b.N {
					if names, err := server.requestedStaticTargets(query); err != nil || !names[last.Name] {
						b.Fatalf("the read that names the last static target is given %v, %v", names, err)
					}
				}
			})
		})
	}
}

// BenchmarkSelfMetricsSeedStatic is what a read of the verbose self-metrics
// does for the static targets: see the top of the file. More than
// VerboseRequestSeriesLimit of them, 1,000, are not all tracked, and telling
// the tracker of those left over takes far longer than finding them
// (requestTracker.setStatic): keys is the part that finds each target's
// collector by its name.
func BenchmarkSelfMetricsSeedStatic(b *testing.B) {
	for _, n := range staticLookupSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			server, followed := staticLookupServer(b, n, "http://127.0.0.1:9/status")
			b.Run("keys", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if keys := server.staticRequestKeys(); len(keys) != len(staticTargetsOf(followed.targets)) {
						b.Fatalf("the static targets have %d requests, want one each", len(keys))
					}
				}
			})
			b.Run("seed", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					server.seedStaticRequests()
				}
			})
		})
	}
}
