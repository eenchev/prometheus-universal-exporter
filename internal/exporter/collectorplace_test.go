package exporter

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A collector is found by its name where the fingerprints of its
// configuration are kept (fingerprintGeneration.place, fingerprint.go),
// where the collectors were gone through for every probe, every scrape of a
// static target and every collector the schedule asked about. The tests
// here hold that what is found is what going through them found.

// fingerprintOfAsItWas is followedConfig.fingerprintOf as it was while it
// went through the collectors of cfg for every name.
func fingerprintOfAsItWas(f *followedConfig, cfg *model.Config, name string) string {
	for i := range cfg.Collectors {
		if cfg.Collectors[i].Name != name {
			continue
		}
		if f != nil && f.config == cfg && f.fingerprints != nil && f.fingerprints.config == cfg {
			return f.fingerprints.at(i)
		}
		return collectorFingerprint(&cfg.Collectors[i])
	}
	return ""
}

// memoFingerprintAsItWas is fingerprintMemo.fingerprint as it was while it
// went through the collectors of cfg for every collector.
func memoFingerprintAsItWas(m *fingerprintMemo, cfg *model.Config, c *model.Collector) string {
	index := -1
	if cfg != nil {
		for i := range cfg.Collectors {
			if &cfg.Collectors[i] == c {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return collectorFingerprint(c)
	}
	return m.of(cfg).at(index)
}

// placeTestNames are the names the generated configurations draw from; the
// last two are never a collector's.
var placeTestNames = []string{"a", "b", "c", "d", "e", "f", "g", "h", "never_configured", ""}

// placeTestConfig is a configuration of count collectors named at random,
// each with a definition of its own. With unique every name is used once at
// most, as in a loaded configuration; without, two collectors may share a
// name, which no loaded configuration has (config.Validate) and which shows
// that the first so named is the one found.
func placeTestConfig(random *rand.Rand, count int, unique bool) *model.Config {
	cfg := &model.Config{}
	names := placeTestNames[:len(placeTestNames)-2]
	order := random.Perm(len(names))
	for i := range count {
		name := names[random.IntN(len(names)/2)]
		if unique {
			name = names[order[i%len(names)]]
			if i >= len(names) {
				name += strconv.Itoa(i / len(names))
			}
		}
		c := testutil.Collector(name, "text")
		c.Metrics[0].Name = fmt.Sprintf("definition_%d_%d", i, random.IntN(1000))
		cfg.Collectors = append(cfg.Collectors, c)
	}
	return cfg
}

// A collector found by its name is the one going through the collectors
// found. Over 60 generated configurations (20 under the race detector) of
// no collector to 24, half of them named so that collectors share a name,
// and for every name of theirs, one never configured and the empty one: the place
// kept for the name is the index of the first collector so named, or none;
// the collector a probe or a scrape is given is the one
// model.CollectorByName gives, the very one and not a copy; and the
// fingerprint the schedule is given is the one it was given while every
// collector was gone through for it, with as many definitions encoded —
// asked with the configuration followed, with none followed, with another
// one followed, and with one followed that keeps no fingerprints. The
// fingerprint a probe's cache key is made of is as it was too, for every
// collector, the second of a name among them, for a copy of one, and for a
// collector of another configuration than the one remembered, again with as
// many definitions encoded. With the configuration followed its collectors
// are gone through once in all, and once for every ask otherwise.
func TestACollectorFoundByItsNameIsTheOneGoingThroughThemFound(t *testing.T) {
	var encoded, scanned int
	encodes, scans := func() { encoded++ }, func() { scanned++ }
	fingerprintedHook.Store(&encodes)
	collectorsScannedHook.Store(&scans)
	t.Cleanup(func() {
		fingerprintedHook.Store(nil)
		collectorsScannedHook.Store(nil)
	})
	// counting runs do and says how many definitions it encoded.
	counting := func(do func() string) (string, int) {
		before := encoded
		value := do()
		return value, encoded - before
	}
	configurations := alloctest.UnlessRaced(60, 20)
	shared, absent, later, byPlace := 0, 0, 0, 0
	for seed := range configurations {
		random := rand.New(rand.NewPCG(uint64(seed), 34))
		count := random.IntN(9)
		if seed%20 == 0 {
			count = 24
		}
		cfg := placeTestConfig(random, count, seed%2 == 0)
		other := placeTestConfig(random, 1+random.IntN(8), true)
		// Each side has fingerprints of its own to keep, so that both encode
		// what they would alone.
		memo, memoWas := &fingerprintMemo{}, &fingerprintMemo{}
		asked := append(slices.Clone(placeTestNames), testutil.CollectorNames(cfg)...)
		slices.Sort(asked)
		asked = slices.Compact(asked)
		followed, followedWas := planFollowing(nil, cfg, nil, memo).next, planFollowing(nil, cfg, nil, memoWas).next
		for _, given := range []struct {
			what     string
			now, was *followedConfig
		}{
			{"the configuration followed", followed, followedWas},
			{"no configuration followed", nil, nil},
			{"another configuration followed", planFollowing(nil, other, nil, &fingerprintMemo{}).next, planFollowing(nil, other, nil, &fingerprintMemo{}).next},
			{"a configuration followed that keeps no fingerprints", &followedConfig{config: cfg}, &followedConfig{config: cfg}},
		} {
			// Every name with the configuration followed, which encodes each
			// definition once; four of them otherwise, each ask encoding one.
			names := asked
			if given.now != followed {
				random.Shuffle(len(asked), func(i, j int) { asked[i], asked[j] = asked[j], asked[i] })
				names = asked[:4]
			}
			scanned = 0
			for _, name := range names {
				first := model.CollectorByName(cfg, name)
				if got := given.now.collectorOf(cfg, name); got != first {
					t.Fatalf("configuration %d, %s: the collector named %q is %p, want %p, the first so named", seed, given.what, name, got, first)
				}
				got, made := counting(func() string { return given.now.fingerprintOf(cfg, name) })
				want, madeWas := counting(func() string { return fingerprintOfAsItWas(given.was, cfg, name) })
				if got != want || made != madeWas || (got == "") != (first == nil) {
					t.Fatalf("configuration %d, %s: the fingerprint of the collector named %q is %q after %d definitions encoded, want %q after %d", seed, given.what, name, got, made, want, madeWas)
				}
			}
			// Two asks for every name: the collector and its fingerprint.
			if want := 2 * len(names); given.now == followed && scanned > 1 || given.now != followed && scanned != want {
				t.Fatalf("configuration %d, %s: the collectors were gone through %d times for %d asks; want once at most with the configuration followed and once for each ask otherwise", seed, given.what, scanned, want)
			}
		}
		for _, name := range asked {
			index, found := followed.fingerprints.place(name)
			if want := placeByScan(cfg, name); found != (want >= 0) || found && index != want {
				t.Fatalf("configuration %d: the place of the collector named %q is %d, found %v; want %d", seed, name, index, found, want)
			}
			if !found {
				absent++
			}
		}
		// The fingerprint of a probe's cache key, by the collector itself.
		ask := func(cfg *model.Config, c *model.Collector, what string) {
			t.Helper()
			got, made := counting(func() string { return memo.fingerprint(cfg, c) })
			want, madeWas := counting(func() string { return memoFingerprintAsItWas(memoWas, cfg, c) })
			if got != want || made != madeWas {
				t.Fatalf("configuration %d: the fingerprint of %s is %q after %d definitions encoded, want %q after %d", seed, what, got, made, want, madeWas)
			}
		}
		for i := range cfg.Collectors {
			c := &cfg.Collectors[i]
			if first := model.CollectorByName(cfg, c.Name); first != c {
				shared++
			}
			if i > 0 {
				later++
			}
			// While the fingerprints remembered are the configuration's, its
			// collectors are not gone through for one of them, but once to
			// note where each is, by the first to ask those fingerprints. The
			// former fingerprint, kept above, goes through them itself and
			// tells no one.
			remembered := memo.current.Load()
			noted := 0
			if remembered.places == nil {
				noted = 1
			}
			scanned = 0
			ask(cfg, c, fmt.Sprintf("collector %d, named %q", i, c.Name))
			if first := model.CollectorByName(cfg, c.Name); remembered.config == cfg && first == c {
				byPlace++
				if scanned != noted {
					t.Fatalf("configuration %d: the collectors were gone through %d times for the fingerprint of collector %d, the first named %q; want %d", seed, scanned, i, c.Name, noted)
				}
			}
			if i%4 == 0 {
				duplicate := *c
				ask(cfg, &duplicate, fmt.Sprintf("a copy of collector %d", i))
				// A collector of another configuration, and that configuration's
				// own, whose fingerprints then take the place of those remembered
				// until cfg is asked about again.
				foreign := &other.Collectors[i%len(other.Collectors)]
				ask(cfg, foreign, "a collector of another configuration")
				ask(other, foreign, "a collector of the other configuration, in it")
			}
		}
		ask(nil, &other.Collectors[0], "a collector with no configuration")
		ask(cfg, nil, "no collector")
	}
	// What the configurations must have had to show anything.
	if floor := configurations / 4; shared < floor || absent < 2*configurations || later < 2*configurations || byPlace < 2*configurations {
		t.Errorf("the %d configurations had %d collectors that share an earlier one's name, %d that are not the first of their configuration, %d names without a collector and %d fingerprints asked for while the configuration's were remembered; too few of one of them to show anything", configurations, shared, later, absent, byPlace)
	}
}
