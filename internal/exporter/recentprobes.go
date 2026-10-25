package exporter

import (
	"container/list"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The status page (statictargetstatus.go) also shows the recent probes: in
// most setups Prometheus does all the probing through /probe, and the
// static targets section alone would show nothing of it. Each probe the
// exporter answers is recorded under its collector, its target as the log
// shows it and its probe parameters, with when it began, how long it took,
// its result, the HTTP status it was answered with, the series it gave and
// its error, shown as the static targets' errors are. A debug probe is not
// recorded: it is counted nowhere and is the debug page's to show. Nor is a
// probe refused before its collector and target were found good (a 400:
// the caller's mistake, and anything at all could be its key), nor one whose
// caller went away before it was answered.
//
// The targets come from whoever may call /probe, so what is kept is bounded:
// the last recentProbesPerKey probes of each key, and no more than
// recentProbeKeys keys, the key probed least recently forgotten to make room
// for a new one. A key's target is cut to recentProbeTargetBytes and its
// parameters to recentProbeParamsBytes, and an error to
// model.MaxFailureBytes, so the page holds at most about
// recentProbeKeys × recentProbesPerKey × model.MaxFailureBytes, 10 MB, of
// errors, and far less in practice: a key failing the same way again keeps
// the one text. A reload forgets the keys of every collector it removes or
// changes, as the rest of the per-collector state (reconcile.go).
//
// These are constants rather than flags: the exporter has no flag for any
// page's size, and the bound is there to protect the exporter, not to be
// tuned.

const (
	// recentProbesPerKey is how many probes of a key are kept.
	recentProbesPerKey = 10
	// recentProbeKeys is how many keys are kept.
	recentProbeKeys = 500
	// recentProbeTargetBytes bounds a key's target, and
	// recentProbeParamBytes each of its parameters' values, which
	// recentProbeParamsBytes bounds all together.
	recentProbeTargetBytes = 512
	recentProbeParamBytes  = 64
	recentProbeParamsBytes = 512
)

// The results a probe is shown with.
const (
	probeSuccess  = "success"
	probeCached   = "cached"
	probeFailed   = "failed"
	probeRefused  = "refused by policy"
	probeRejected = "rejected by limit"
	probeStale    = "stale result served"
	probeBackoff  = "backing off"
)

// recentProbeKey is what the probes are kept by: the collector, the target
// as the log shows it and the probe parameters as the page shows them, each
// cut to its bound. Two probes whose differences the cut or the redaction
// hides share a key.
type recentProbeKey struct {
	collector, target, params string
}

// recentProbe is one probe as the page shows it.
type recentProbe struct {
	at       time.Time
	duration time.Duration
	result   string
	// shared says the probe was answered by a trip another probe started
	// (probeflight.go).
	shared bool
	status int
	series int
	// failure is the error, as the page shows it; empty for a success.
	failure string
}

// recentProbeEntry is what is kept of a key: its probes, oldest first, and
// its place in the order of the keys by their last probe.
type recentProbeEntry struct {
	key    recentProbeKey
	probes []recentProbe
	place  *list.Element
}

// recentProbes is the recent probes of every key kept.
type recentProbes struct {
	mu      sync.Mutex
	entries map[recentProbeKey]*recentProbeEntry
	// order has the keys, the one probed most recently at the front.
	order *list.List
}

func newRecentProbes() *recentProbes {
	return &recentProbes{entries: map[recentProbeKey]*recentProbeEntry{}, order: list.New()}
}

// record keeps p as the latest probe of key, for a probe that read its
// collector as read says. It records nothing once a reload has removed or
// changed that collector: it asks under the lock the reload forgets the
// collector's keys under (forgetLocked), so a probe of the old definition is
// either recorded before they are forgotten, and forgotten with them, or
// not at all.
func (r *recentProbes) record(read configRead, key recentProbeKey, p recentProbe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !read.stands(key.collector) {
		return
	}
	entry := r.entries[key]
	if entry == nil {
		if r.order.Len() >= recentProbeKeys {
			oldest := r.order.Back()
			delete(r.entries, oldest.Value.(*recentProbeEntry).key)
			r.order.Remove(oldest)
		}
		entry = &recentProbeEntry{key: key}
		entry.place = r.order.PushFront(entry)
		r.entries[key] = entry
	} else {
		r.order.MoveToFront(entry.place)
	}
	if n := len(entry.probes); n > 0 && entry.probes[n-1].failure == p.failure {
		// The same error again keeps the one text.
		p.failure = entry.probes[n-1].failure
	}
	if len(entry.probes) == recentProbesPerKey {
		copy(entry.probes, entry.probes[1:])
		entry.probes[len(entry.probes)-1] = p
		return
	}
	entry.probes = append(entry.probes, p)
}

// forgetLocked forgets the keys of the collectors named, under mu.
func (r *recentProbes) forgetLocked(collectors map[string]bool) {
	for element := r.order.Front(); element != nil; {
		next := element.Next()
		if entry := element.Value.(*recentProbeEntry); collectors[entry.key.collector] {
			delete(r.entries, entry.key)
			r.order.Remove(element)
		}
		element = next
	}
}

// snapshot is a copy of every key kept, with its probes.
func (r *recentProbes) snapshot() []recentProbeEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recentProbeEntry, 0, len(r.entries))
	for element := r.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*recentProbeEntry)
		out = append(out, recentProbeEntry{key: entry.key, probes: append([]recentProbe(nil), entry.probes...)})
	}
	return out
}

// recentProbeKeyFor is the key of a probe of collector c of target, shown
// as logTarget, with overrides.
func recentProbeKeyFor(c *model.Collector, logTarget string, overrides fetch.RequestOverrides) recentProbeKey {
	return recentProbeKey{
		collector: c.Name,
		target:    model.CutTo(logTarget, recentProbeTargetBytes, false),
		params:    shownProbeParams(overrides),
	}
}

// shownProbeParams is the parameters of a probe that change its request,
// as the page shows them: name=value, in order of name. They are part of
// the key, since they make other requests: tenants, paths. A value is no
// secret by contract (it may fill a label value, § 42.10c), but one may
// still have been sent as one: the value of a parameter whose name reads as
// a credential is withheld, anything in a value that reads as one masked
// (fetch.RedactText, which withholds a URL's userinfo too),
// and each value cut to recentProbeParamBytes. Forwarded headers are not
// shown: they are the caller's, and may carry credentials.
func shownProbeParams(o fetch.RequestOverrides) string {
	query := probeKeyQuery("", "", o)
	query.Del("target")
	query.Del("collector")
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	shown := make([]string, 0, len(names))
	for _, name := range names {
		value := query.Get(name)
		if fetch.CredentialName(name) {
			value = fetch.Redacted
		} else {
			value = fetch.RedactText(value)
		}
		shown = append(shown, model.CutTo(name, recentProbeParamBytes, false)+"="+model.CutTo(value, recentProbeParamBytes, false))
	}
	return model.CutTo(strings.Join(shown, " "), recentProbeParamsBytes, false)
}

// shownFailureText is the text of a failure as the status page shows it: on
// one line, masked for anything reading as a credential, and cut to
// model.MaxFailureBytes.
func shownFailureText(text string) string {
	text = fetch.RedactText(strings.Join(strings.Fields(text), " "))
	return model.CutTo(text, model.MaxFailureBytes, false)
}

// recentProbe is the probe answered with p, begun at start, as the page
// shows it.
func (p *probeResult) recentProbe(start time.Time, shared bool) recentProbe {
	out := recentProbe{at: start, duration: time.Since(start), shared: shared, status: p.status, failure: p.failure}
	if p.metrics != nil {
		out.status = 200
		out.series = len(p.metrics.Metrics)
	}
	switch {
	case p.outcome != "":
		out.result = p.outcome
	case p.ok:
		out.result = probeSuccess
	default:
		out.result = probeFailed
	}
	if out.result == probeSuccess || out.result == probeCached {
		out.failure = ""
	} else if out.failure == "" {
		out.failure = "the probe failed"
	}
	return out
}

// noteProbe records p as the latest probe of key, for a probe that read its
// collector as read says.
func (s *Server) noteProbe(read configRead, key recentProbeKey, p recentProbe) {
	s.recent.record(read, key, p)
}

// probeStatusRow is one key's row, and its object in the JSON: the key, its
// latest probe, and its probes, oldest first.
type probeStatusRow struct {
	Collector string `json:"collector"`
	Target    string `json:"target"`
	Params    string `json:"params,omitempty"`
	Result    string `json:"result"`
	Failing   bool   `json:"failing"`
	Shared    bool   `json:"shared"`
	LastProbe string `json:"last_probe"`
	// LastProbeAge and Duration are of the latest probe.
	LastProbeAge float64 `json:"last_probe_age_seconds"`
	Duration     float64 `json:"duration_seconds"`
	Status       int     `json:"status"`
	Series       int     `json:"series"`
	// LastError is the error of the latest failed probe kept, which
	// LastErrorAt began.
	LastError   string             `json:"last_error,omitempty"`
	LastErrorAt string             `json:"last_error_at,omitempty"`
	Probes      []probeStatusProbe `json:"probes"`
	// Age and DurationText are the latest probe's as the page writes them.
	Age          string `json:"-"`
	DurationText string `json:"-"`
}

// probeStatusProbe is one probe of a key, in the JSON and as a mark of the
// row's strip.
type probeStatusProbe struct {
	At       string  `json:"at"`
	Duration float64 `json:"duration_seconds"`
	Result   string  `json:"result"`
	Shared   bool    `json:"shared"`
	Status   int     `json:"status"`
	Series   int     `json:"series"`
	Error    string  `json:"error,omitempty"`
	// Mark and Good are how the strip shows it.
	Mark string `json:"-"`
	Good bool   `json:"-"`
}

// probeFailing reports whether a probe with result failed.
func probeFailing(result string) bool {
	return result != probeSuccess && result != probeCached
}

// probeMark is the strip's mark of a probe with result.
func probeMark(result string) string {
	switch result {
	case probeSuccess, probeCached:
		return "✓"
	case probeStale:
		return "~"
	case probeBackoff:
		return "…"
	default:
		return "✗"
	}
}

// probeRowsAt is the rows of the recent probes as of now: the failing keys
// first, each part in order of collector, target and parameters.
func (s *Server) probeRowsAt(now time.Time) []probeStatusRow {
	entries := s.recent.snapshot()
	rows := make([]probeStatusRow, 0, len(entries))
	for _, entry := range entries {
		if len(entry.probes) == 0 {
			continue
		}
		latest := entry.probes[len(entry.probes)-1]
		age := max(now.Sub(latest.at), 0)
		row := probeStatusRow{
			Collector: entry.key.collector, Target: entry.key.target, Params: entry.key.params,
			Result: latest.result, Failing: probeFailing(latest.result), Shared: latest.shared,
			LastProbe:    latest.at.UTC().Format(time.RFC3339),
			LastProbeAge: age.Seconds(), Duration: latest.duration.Seconds(),
			Status: latest.status, Series: latest.series,
			Age: shortDuration(age), DurationText: shownScrapeDuration(latest.duration),
			Probes: make([]probeStatusProbe, 0, len(entry.probes)),
		}
		for _, p := range entry.probes {
			row.Probes = append(row.Probes, probeStatusProbe{
				At: p.at.UTC().Format(time.RFC3339), Duration: p.duration.Seconds(),
				Result: p.result, Shared: p.shared, Status: p.status, Series: p.series, Error: p.failure,
				Mark: probeMark(p.result), Good: !probeFailing(p.result),
			})
			if p.failure != "" {
				row.LastError = p.failure
				row.LastErrorAt = p.at.UTC().Format(time.RFC3339)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if a.Failing != b.Failing {
			return a.Failing
		}
		if a.Collector != b.Collector {
			return a.Collector < b.Collector
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Params < b.Params
	})
	return rows
}
