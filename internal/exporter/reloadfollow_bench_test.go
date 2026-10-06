//go:build !select_request_types || request_type_http

package exporter

// Benchmarks of following a reload (reconcile.go): what the per-collector
// state costs to bring in line with a configuration put in force, for 50, 500
// and 2,000 collectors, how much of it is left to do once the configuration
// is in force, when a probe may ask for the state, and how much of that is
// done with the statistics lock held, which every probe takes to find its
// collector's statistics (docs/DEVELOPMENT.md):
//
//	go test -run '^$' -bench 'FollowReload' -benchtime 20x ./internal/exporter/
//
// unchanged is a reload that read the same collectors again, changed one
// that changed every collector, and half one that removed half the
// collectors and added as many; /static is the same with a static target of
// each collector, in a file reloaded with the configuration. The look the
// schedule of those targets takes after the reload is the scrape loop's and
// not part of the following: BenchmarkFollowReloadSchedule measures it.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// followBenchNames are count collector names, from the one numbered first.
func followBenchNames(first, count int) []string {
	names := make([]string, count)
	for i := range names {
		names[i] = "c" + strconv.Itoa(first+i)
	}
	return names
}

// followBenchManager is a manager holding document as its configuration, as
// a reload would have read it, and with static a static target file of one
// target of each collector.
func followBenchManager(tb testing.TB, document string, static bool) *config.Manager {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		tb.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		tb.Fatal(err)
	}
	manager := config.NewManager(cfg, "", slog.New(slog.DiscardHandler))
	if static {
		manager.SetTargets("", followBenchTargets(tb, cfg))
	}
	return manager
}

// followBenchTargets is a static target file of one target of each collector
// of cfg, checked as a reload checks the file it reads.
func followBenchTargets(tb testing.TB, cfg *model.Config) *model.StaticTargetFile {
	tb.Helper()
	file := &model.StaticTargetFile{Interval: model.Duration(60e9)}
	for i := range cfg.Collectors {
		name := cfg.Collectors[i].Name
		file.Targets = append(file.Targets, model.StaticTarget{Name: "t_" + name, Collector: name, Target: "http://127.0.0.1:9/" + name, Labels: map[string]string{"site": name}})
	}
	err := config.ValidateStaticTargets(file)
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		tb.Fatal(err)
	}
	return file
}

// followBenchShapes are the reloads measured: the two configurations a
// benchmark reloads between, as their documents.
var followBenchShapes = []struct {
	name          string
	first, second func(n int) string
}{
	{"unchanged",
		func(n int) string { return testutil.CollectorsDocument(followBenchNames(0, n)...) },
		func(n int) string { return testutil.CollectorsDocument(followBenchNames(0, n)...) }},
	{"changed",
		func(n int) string { return testutil.CollectorsDocument(followBenchNames(0, n)...) },
		func(n int) string {
			return strings.ReplaceAll(testutil.CollectorsDocument(followBenchNames(0, n)...), "_value\n", "_renamed\n")
		}},
	{"half",
		func(n int) string { return testutil.CollectorsDocument(followBenchNames(0, n)...) },
		func(n int) string { return testutil.CollectorsDocument(followBenchNames(n/2, n)...) }},
}

// BenchmarkFollowReload is one following of a reload: the other
// configuration of its shape is prepared, as a reload prepares the one it
// has read (prepareReload), it replaces the configuration in force, and the
// per-collector state is brought in line with it. The time of the operation
// is all of it; following-ns/op is what is left once the configuration is in
// force, which the first to ask for the state then waits for, the reload or
// a probe, and locked-ns/op the part of that under the statistics lock.
func BenchmarkFollowReload(b *testing.B) {
	for _, shape := range followBenchShapes {
		for _, n := range []int{50, 500, 2000} {
			for _, static := range []bool{false, true} {
				name := fmt.Sprintf("%s/n=%d", shape.name, n)
				if static {
					name += "/static"
				}
				b.Run(name, func(b *testing.B) {
					managers := [2]*config.Manager{followBenchManager(b, shape.first(n), static), followBenchManager(b, shape.second(n), static)}
					server := NewServer(managers[0], "python3", slog.New(slog.DiscardHandler))
					// From where the following is worked out to its end is
					// the time the statistics lock is held, nothing else
					// asking for it here.
					var planned time.Time
					var following, locked time.Duration
					hook := func() { planned = time.Now() }
					followPlannedHook.Store(&hook)
					defer followPlannedHook.Store(nil)
					b.ReportAllocs()
					b.ResetTimer()
					for i := range b.N {
						// The configuration is prepared and the manager
						// replaced, as an accepted reload prepares and then
						// replaces what it holds, and the state follows.
						next := managers[(i+1)%2]
						server.prepareReload(next.InForce())
						server.manager = next
						inForce := time.Now()
						server.reconcile()
						following += time.Since(inForce)
						locked += time.Since(planned)
					}
					b.ReportMetric(float64(following.Nanoseconds())/float64(b.N), "following-ns/op")
					b.ReportMetric(float64(locked.Nanoseconds())/float64(b.N), "locked-ns/op")
				})
			}
		}
	}
}

// BenchmarkFollowReloadProbeWait is what a probe waits for the per-collector
// state when it asks between a reload putting its configuration in force and
// the reload's own following of it, the window in which a probe follows the
// configuration itself (reconcile.go): the file of 50, 500 and 2,000
// collectors is read again, as SIGHUP reads it, the reload is held where it
// has put the configuration in force and has not yet followed it, and the
// state is asked for as a probe asks. probe-wait-ns/op is how long that
// took; the time of the operation is the whole reload, the file read and
// checked and the configuration prepared, put in force and followed.
func BenchmarkFollowReloadProbeWait(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(testutil.CollectorsDocument(followBenchNames(0, n)...)), 0o600); err != nil {
				b.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				b.Fatal(err)
			}
			manager := config.NewManager(cfg, path, slog.New(slog.DiscardHandler))
			manager.SetPythonPath("python3")
			// Told before the server is: the reload waits here with its
			// configuration in force and not yet followed.
			inForce, followOn := make(chan struct{}), make(chan struct{})
			manager.OnInstall(func() {
				inForce <- struct{}{}
				<-followOn
			})
			server := NewServer(manager, "python3", slog.New(slog.DiscardHandler))
			reloaded := make(chan error, 1)
			var waited time.Duration
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				go func() { reloaded <- manager.Reload(config.ReloadTriggerSignal) }()
				<-inForce
				asked := time.Now()
				followed := server.reconcile()
				waited += time.Since(asked)
				if followed.config != manager.Get() {
					b.Fatal("the probe was given another configuration than the one in force")
				}
				followOn <- struct{}{}
				if err := <-reloaded; err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(waited.Nanoseconds())/float64(b.N), "probe-wait-ns/op")
		})
	}
}

// BenchmarkFollowReloadSchedule is the look the schedule of the static
// targets takes after a reload (StaticScrapeLoop, planFollowed), which the
// benchmarks above leave out: the schedule is the scrape loop's own, and
// plans on that loop's goroutine, some time after the reload has returned.
// Every collector of 50, 500 and 2,000 has one static target, in a file
// reloaded with the configuration, and the schedule has looked at the
// configuration before; the reload, prepared and followed as above, is not
// timed, so the time, the bytes and the allocations of the operation are the
// look's alone. encodes/op is how many collectors' definitions the reload
// and the look after it encoded together (collectorFingerprint), and
// schedule-encodes/op how many of them the look did.
func BenchmarkFollowReloadSchedule(b *testing.B) {
	for _, shape := range followBenchShapes {
		for _, n := range []int{50, 500, 2000} {
			b.Run(fmt.Sprintf("%s/n=%d", shape.name, n), func(b *testing.B) {
				managers := [2]*config.Manager{followBenchManager(b, shape.first(n), true), followBenchManager(b, shape.second(n), true)}
				server := NewServer(managers[0], "python3", slog.New(slog.DiscardHandler))
				schedule := newTargetSchedule()
				now := time.Unix(1_000_000, 0)
				var encoded, bySchedule int64
				hook := func() { encoded++ }
				fingerprintedHook.Store(&hook)
				defer fingerprintedHook.Store(nil)
				// reload puts the other configuration in force as a reload
				// does, and look is the loop's look at what is then followed.
				reload := func(i int) *followedConfig {
					next := managers[(i+1)%2]
					server.prepareReload(next.InForce())
					server.manager = next
					return server.followedInForce()
				}
				look := func(followed *followedConfig) {
					before := encoded
					schedule.planFollowed(followed.config, staticTargetsOf(followed.targets), followed, now)
					bySchedule += encoded - before
				}
				// The start, and a reload each way: from then on every
				// configuration followed was prepared by a reload.
				look(server.followedInForce())
				look(reload(0))
				look(reload(1))
				encoded, bySchedule = 0, 0
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					b.StopTimer()
					followed := reload(i)
					b.StartTimer()
					look(followed)
				}
				b.ReportMetric(float64(encoded)/float64(b.N), "encodes/op")
				b.ReportMetric(float64(bySchedule)/float64(b.N), "schedule-encodes/op")
			})
		}
	}
}
