package exporter

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The debug scrape of a static target, /static-targets?debug=<name>, finds
// the target by going through the file's targets by index and its
// collector where the configuration followed keeps its place
// (staticDebugTarget), where it went through the targets by value, copying
// each on the heap, and then through the collectors. The tests here hold
// that it finds what it found, and that the finding costs nothing that grows
// with the file.

// staticDebugTargetAsItWas is the lookup serveStaticTargetDebug made while
// it went through the targets by value and then through the collectors: a
// copy of the first target named name, nil when there is none, and the
// collector it references, nil when the configuration has none of that name.
func staticDebugTargetAsItWas(followed *followedConfig, name string) (*model.StaticTarget, *model.Collector) {
	var target *model.StaticTarget
	for _, t := range staticTargetsOf(followed.targets) { //nolint:gocritic // The lookup as it was, copies and all, is the oracle.
		if t.Name == name {
			target = &t
			break
		}
	}
	if target == nil {
		return nil, nil
	}
	return target, model.CollectorByName(followed.config, target.Collector)
}

// staticDebugTestFile is a static target file of count targets drawn at
// random from names and of collectors drawn from placeTestNames, two of
// which no configuration has. With unique every target has a name of its
// own, as in a loaded file; without, two may share a name, which no loaded
// file has (config.ValidateStaticTargets) and which shows that the first so
// named is the one found.
func staticDebugTestFile(random *rand.Rand, count int, unique bool) *model.StaticTargetFile {
	file := &model.StaticTargetFile{}
	for i := range count {
		name := fmt.Sprintf("t%d", random.IntN(count/2+1))
		if unique {
			name = fmt.Sprintf("t%d", i)
		}
		file.Targets = append(file.Targets, model.StaticTarget{
			Name:      name,
			Collector: placeTestNames[random.IntN(len(placeTestNames))],
			Target:    fmt.Sprintf("http://127.0.0.1:%d/%d", 9000+i, random.IntN(1000)),
			Labels:    map[string]string{"i": strconv.Itoa(i)},
		})
	}
	return file
}

// The debug scrape finds the target and the collector the lookup as it was
// found. Over 200 generated pairs of configuration and target file (50
// under the race detector) of no target to 40, half of them with names that
// targets share and configurations whose collectors share theirs, and for
// every name of the file, one it does not have and the empty one: the target
// found is the first of the name in the file in force, itself and not a
// copy, and equal to the copy found before; there is none where there was
// none; the collector is the very one found before, or none where none was
// (the target referencing a collector the configuration lacks) — with the
// configuration followed, which finds it where its place is kept, with one
// followed that keeps no places, which goes through the collectors, and
// with no target file. With the configuration followed, the collectors are
// gone through once in all for each configuration, to note their places,
// and not once for each debug scrape; with one that keeps no places, once
// for each target found, as a scrape goes through them there.
func TestTheDebugScrapeFindsTheTargetAndCollectorTheLookupAsItWasFound(t *testing.T) {
	var scanned int
	scans := func() { scanned++ }
	collectorsScannedHook.Store(&scans)
	t.Cleanup(func() { collectorsScannedHook.Store(nil) })
	pairs := alloctest.UnlessRaced(200, 50)
	found, unknownCollector, shared := 0, 0, 0
	for seed := range pairs {
		random := rand.New(rand.NewPCG(uint64(seed), 50))
		count := random.IntN(12)
		if seed%10 == 0 {
			count = 40
		}
		cfg := placeTestConfig(random, random.IntN(10), seed%4 < 2)
		file := staticDebugTestFile(random, count, seed%2 == 0)
		names := []string{"", "never_a_target"}
		for i := range file.Targets {
			names = append(names, file.Targets[i].Name)
		}
		followed := planFollowing(nil, cfg, file, &fingerprintMemo{}).next
		for _, given := range []struct {
			what     string
			followed *followedConfig
			placed   bool
		}{
			{"the configuration followed", followed, true},
			{"a configuration followed that keeps no places", &followedConfig{config: cfg, targets: file}, false},
			{"no target file", &followedConfig{config: cfg}, false},
		} {
			before, asked := scanned, 0
			for _, name := range names {
				wantTarget, wantCollector := staticDebugTargetAsItWas(given.followed, name)
				target, collector := staticDebugTarget(given.followed, name)
				if (target == nil) != (wantTarget == nil) {
					t.Fatalf("seed %d, %s: target %q is found %v, want %v", seed, given.what, name, target != nil, wantTarget != nil)
				}
				if collector != wantCollector {
					t.Fatalf("seed %d, %s: target %q finds collector %p, want %p", seed, given.what, name, collector, wantCollector)
				}
				if target == nil {
					continue
				}
				asked++
				if !reflect.DeepEqual(*target, *wantTarget) {
					t.Fatalf("seed %d, %s: target %q is %+v, want %+v", seed, given.what, name, *target, *wantTarget)
				}
				first := -1
				for i := range file.Targets {
					if file.Targets[i].Name == name {
						first = i
						break
					}
				}
				if first < 0 || target != &file.Targets[first] {
					t.Fatalf("seed %d, %s: target %q is not the first so named in the file in force", seed, given.what, name)
				}
				for i := first + 1; i < len(file.Targets); i++ {
					if file.Targets[i].Name == name {
						shared++
						break
					}
				}
				if collector == nil {
					unknownCollector++
				} else {
					found++
				}
			}
			if given.placed && scanned-before > 1 {
				t.Fatalf("seed %d: with the configuration followed the collectors were gone through %d times for %d debug scrapes, want once at most", seed, scanned-before, asked)
			}
			if !given.placed && scanned-before != asked {
				t.Fatalf("seed %d, %s: the collectors were gone through %d times for %d targets found, want once for each, as collectorOf goes through them", seed, given.what, scanned-before, asked)
			}
		}
	}
	if found == 0 || unknownCollector == 0 || shared == 0 {
		t.Fatalf("the generated files gave %d targets with a collector, %d with an unknown one and %d names shared with a later target: each must be some", found, unknownCollector, shared)
	}
}

// Finding the target and the collector of a debug scrape allocates nothing,
// with the target last of 2,000 and of 10,000 (1,000 and 2,000 under the
// race detector, where nothing is measured) of a configuration followed. It
// allocated once for every target before the one found, a copy of 416 bytes
// of each, which the lookup as it was, measured beside it, still does.
func TestFindingTheTargetOfADebugScrapeAllocatesNothingThatGrowsWithTheFile(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("a", "text"), testutil.Collector("b", "text")}}
	for _, count := range []int{alloctest.UnlessRaced(2000, 1000), alloctest.UnlessRaced(10000, 2000)} {
		file := &model.StaticTargetFile{}
		for i := range count {
			file.Targets = append(file.Targets, model.StaticTarget{Name: fmt.Sprintf("t%d", i), Collector: cfg.Collectors[i%2].Name, Target: "http://127.0.0.1:9/"})
		}
		followed := planFollowing(nil, cfg, file, &fingerprintMemo{}).next
		last := file.Targets[count-1].Name
		find := func() {
			if target, c := staticDebugTarget(followed, last); target == nil || c == nil {
				t.Fatalf("the last of %d targets or its collector is not found", count)
			}
		}
		find()
		if alloctest.RaceDetector {
			// The race detector changes what is allocated.
			continue
		}
		if allocs := alloctest.AllocsAtMost(10, 0, find); allocs > 0 {
			t.Errorf("finding the last of %d static targets for a debug scrape makes %v allocations, want none", count, allocs)
		}
		if allocs, _ := alloctest.Allocations(10, func() { staticDebugTargetAsItWas(followed, last) }); allocs < float64(count) {
			t.Errorf("the lookup as it was makes %v allocations for %d targets, want one for each: the bound above shows nothing", allocs, count)
		}
	}
}
