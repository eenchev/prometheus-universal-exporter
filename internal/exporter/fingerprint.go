package exporter

import (
	"sync"
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every probe of a caching collector keys its cached result, and its
// in-flight trip, by the collector's definition (collectorFingerprint), so a
// reload retires what the old definition produced. Encoding the definition
// and hashing it on every probe cost more than the rest of the key; a
// configuration never changes once loaded — a reload publishes a new one —
// so each collector's fingerprint is worked out once per configuration, on
// first use, and remembered until the next configuration takes its place.
// The configuration a reload is about to put in force has every fingerprint
// worked out before it is in force (prepare), on the path that reloads: what
// uses the configuration first, the following of the reload (reconcile.go) or
// a probe that comes before that is done, then finds them made, and so does
// the schedule of the static targets when it next looks
// (statictargetschedule.go).

// fingerprintedHook, set by tests, is called whenever a collector's
// definition is encoded and hashed (collectorFingerprint), which is what
// following a reload costs (reconcile.go): so a test can count the times,
// and see where they happen.
var fingerprintedHook atomic.Pointer[func()]

// fingerprintMemo remembers the fingerprints of one configuration's
// collectors, and those of the configuration a reload last prepared, which
// is the one that reload put in force: so it never holds more than the two,
// however many reloads there were, and those of a configuration no longer
// in force only while a probe that still read it was the last to ask (of).
type fingerprintMemo struct {
	current  atomic.Pointer[fingerprintGeneration]
	prepared atomic.Pointer[fingerprintGeneration]
}

// fingerprintGeneration is the fingerprints of one configuration, each worked
// out the first time it is asked for. whole says that all of them have been,
// the configuration having been prepared.
type fingerprintGeneration struct {
	config *model.Config
	once   []sync.Once
	values []string
	whole  atomic.Bool
}

func newFingerprintGeneration(cfg *model.Config) *fingerprintGeneration {
	return &fingerprintGeneration{config: cfg, once: make([]sync.Once, len(cfg.Collectors)), values: make([]string, len(cfg.Collectors))}
}

// fingerprint is collectorFingerprint(c), remembered when c is one of cfg's
// collectors. A collector that is not, such as a copy, is fingerprinted
// afresh.
func (m *fingerprintMemo) fingerprint(cfg *model.Config, c *model.Collector) string {
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

// prepare works out every fingerprint of cfg, a configuration that is about
// to be put in force and that nothing reads yet, and keeps them for of to
// find once it is. The probes of the configuration still in force are left
// the fingerprints they use: what is remembered for them is not replaced
// until something asks for those of cfg. A configuration whose fingerprints
// are kept already, prepared or remembered, has those that are left worked
// out, and none again.
func (m *fingerprintMemo) prepare(cfg *model.Config) {
	generation := m.prepared.Load()
	if generation == nil || generation.config != cfg {
		if generation = m.current.Load(); generation == nil || generation.config != cfg {
			generation = newFingerprintGeneration(cfg)
		}
	}
	for i := range cfg.Collectors {
		generation.at(i)
	}
	generation.whole.Store(true)
	m.prepared.Store(generation)
}

// of is the fingerprints of cfg's collectors: those remembered, when they
// are cfg's, and otherwise those prepared for it or, when none were, new
// ones, none worked out yet, remembered in their place. A caller that goes
// through all of a configuration's collectors, as the following of a reload
// does (reconcile.go), asks once and reads each by its index.
func (m *fingerprintMemo) of(cfg *model.Config) *fingerprintGeneration {
	generation := m.current.Load()
	if generation == nil || generation.config != cfg {
		fresh := m.prepared.Load()
		if fresh == nil || fresh.config != cfg {
			fresh = newFingerprintGeneration(cfg)
		}
		// Around a reload, probes still holding the previous configuration
		// and those holding the new one may trade the memo back and forth
		// until the previous ones finish; each still gets a right answer,
		// and those holding the new one the fingerprints prepared for it.
		if m.current.CompareAndSwap(generation, fresh) {
			generation = fresh
		} else if latest := m.current.Load(); latest != nil && latest.config == cfg {
			generation = latest
		} else {
			generation = fresh
		}
	}
	return generation
}

// at is the fingerprint of the configuration's collector at index, worked
// out the first time it is asked for.
func (g *fingerprintGeneration) at(index int) string {
	g.once[index].Do(func() { g.values[index] = collectorFingerprint(&g.config.Collectors[index]) })
	return g.values[index]
}
