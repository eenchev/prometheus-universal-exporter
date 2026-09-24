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

// fingerprintMemo remembers the fingerprints of one configuration's
// collectors.
type fingerprintMemo struct {
	current atomic.Pointer[fingerprintGeneration]
}

// fingerprintGeneration is the fingerprints of one configuration, each worked
// out the first time it is asked for.
type fingerprintGeneration struct {
	config *model.Config
	once   []sync.Once
	values []string
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
	generation := m.current.Load()
	if generation == nil || generation.config != cfg {
		fresh := newFingerprintGeneration(cfg)
		// Around a reload, probes still holding the previous configuration
		// and those holding the new one may trade the memo back and forth
		// until the previous ones finish; each still gets a right answer.
		if m.current.CompareAndSwap(generation, fresh) {
			generation = fresh
		} else if latest := m.current.Load(); latest != nil && latest.config == cfg {
			generation = latest
		} else {
			generation = fresh
		}
	}
	generation.once[index].Do(func() { generation.values[index] = collectorFingerprint(c) })
	return generation.values[index]
}
