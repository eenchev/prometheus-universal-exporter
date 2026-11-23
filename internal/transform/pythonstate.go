package transform

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A script that counts what is new since it last ran, lines of a log, the
// tickets after the last it saw, has to remember where it was, and a
// script's names are gone when it ends: a worker runs the scripts of every
// target of its collector in turn, and is stopped and started as the
// exporter sees fit. So a collector with transform.state: true gives each
// of its scripts a dict named state, which the exporter keeps from one run
// to the next: what the script left in it is what its next run for the
// same target finds there.
//
// The state is the exporter's, in its memory, by collector, script, target
// and the probe's parameters: a pre-script and a transform have each their
// own, and so has each target, and each set of parameters it is probed with. It is JSON, of at most MaxPythonStateBytes written out,
// which a script that leaves more, or what JSON cannot carry, fails for;
// what a failed script left is not kept, and neither is what the script of
// a debug probe left, which reads the state and changes nothing. A scrape
// answered from the response cache runs no script, and so changes none.
//
// It is not kept across a restart, and in allocator mode each replica has
// its own: a script has to make do with an empty one, as on its first run.
// The exporter keeps the states of at most pythonStatesKept targets, the
// ones used last, and at most pythonStateBytesKept bytes of them, so
// targets that are gone do not stay and the states of many do not add up.

// MaxPythonStateBytes is the most a script's state may be, as JSON.
const MaxPythonStateBytes = 64 << 10

// pythonStatesKept is how many states the exporter keeps, and
// pythonStateBytesKept how many bytes of them, as JSON, all together.
const (
	pythonStatesKept     = 4096
	pythonStateBytesKept = 64 << 20
)

// pythonStateKey says whose a state is: the collector's script, a
// pre-script or a transform, for a target and the parameters of its probe,
// as the last success a script reads is that of the three.
type pythonStateKey struct{ collector, what, target, params string }

// pythonStateParams is a probe's parameters as a key holds them: each name
// and value, in the names' order.
func pythonStateParams(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(params)) {
		b.WriteString(name)
		b.WriteByte(0)
		b.WriteString(params[name])
		b.WriteByte(0)
	}
	return b.String()
}

// pythonStateStore keeps the states of the scripts that have one, each as
// the JSON the worker wrote, and when it was last read or written, by the
// count of those.
type pythonStateStore struct {
	mu    sync.Mutex
	kept  map[pythonStateKey]*pythonStateEntry
	clock uint64
	// bytes is the length of the states kept, together.
	bytes int
	// dropped is how many states of each collector went to keep the
	// states within their bounds.
	dropped map[string]uint64
}

type pythonStateEntry struct {
	json string
	used uint64
}

// pythonStates are the states of the exporter's scripts.
var pythonStates = &pythonStateStore{kept: map[pythonStateKey]*pythonStateEntry{}}

// get is the state kept under key, as JSON: an empty object when none is.
func (s *pythonStateStore) get(key pythonStateKey) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.kept[key]
	if entry == nil {
		return "{}"
	}
	s.clock++
	entry.used = s.clock
	return entry.json
}

// put keeps state, JSON, under key; an empty object is kept as none. When
// more than pythonStatesKept are kept, or more than pythonStateBytesKept
// bytes of them, those used longest ago go until neither is.
func (s *pythonStateStore) put(key pythonStateKey, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if was := s.kept[key]; was != nil {
		s.bytes -= len(was.json)
		delete(s.kept, key)
	}
	if state == "{}" {
		return
	}
	s.clock++
	s.kept[key] = &pythonStateEntry{json: state, used: s.clock}
	s.bytes += len(state)
	for len(s.kept) > pythonStatesKept || s.bytes > pythonStateBytesKept {
		var oldest pythonStateKey
		least := s.clock
		for k, entry := range s.kept {
			if entry.used < least {
				oldest, least = k, entry.used
			}
		}
		if least == s.clock {
			// Only the state just kept is left.
			return
		}
		s.bytes -= len(s.kept[oldest].json)
		delete(s.kept, oldest)
		if s.dropped == nil {
			s.dropped = map[string]uint64{}
		}
		s.dropped[oldest.collector]++
	}
}

// PythonStateCount is what the exporter keeps of the states of a
// collector's scripts (transform.state): how many, of each script, target
// and parameters, their bytes as JSON together, and how many it has dropped
// to stay within the bounds of all the states, each of which left a script
// to start from an empty state.
type PythonStateCount struct {
	States, Bytes int
	Dropped       uint64
}

// PythonStateCounts are the counts of the collectors that have states
// kept, or had one dropped, by name.
func PythonStateCounts() map[string]PythonStateCount {
	s := pythonStates
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]PythonStateCount, len(s.dropped))
	for key, entry := range s.kept {
		count := counts[key.collector]
		count.States++
		count.Bytes += len(entry.json)
		counts[key.collector] = count
	}
	for collector, dropped := range s.dropped {
		count := counts[collector]
		count.Dropped = dropped
		counts[collector] = count
	}
	return counts
}

// checkPythonState says what is wrong with the state a script left, as the
// worker wrote it: it is a JSON object within MaxPythonStateBytes, which
// the worker has seen to, and is checked here as anything read from a
// worker is.
func checkPythonState(c *model.Collector, what, state string) error {
	if len(state) > MaxPythonStateBytes {
		return fmt.Errorf("python %s of collector %q left a state of %d bytes as JSON, and one may be %d; keep less in state", what, c.Name, len(state), MaxPythonStateBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(state), &object); err != nil || object == nil {
		return fmt.Errorf("python %s of collector %q left a state that is no JSON object", what, c.Name)
	}
	return nil
}
