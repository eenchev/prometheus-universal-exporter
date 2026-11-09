package exporter

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A clock placeholder with ~last_success starts from the time of the last
// successful trip of the same collector, target and parameters
// (fetch/clockplaceholders.go). The exporter keeps that time, the clock the
// trip's placeholders were filled from, for each of them in memory: a
// start forgets them all, and each placeholder then starts from its
// fallback. A trip that fails, or carries on past a failed stage, keeps the
// last success it started from, so that the next asks for its time again;
// a debug run reads the time and keeps nothing.

// maxLastSuccesses bounds what is kept: past it, the success kept longest
// ago is forgotten, its next trip starting from its fallback.
const maxLastSuccesses = 10000

// lastSuccesses is the time of the last successful trip of each key
// (lastSuccessKey). Its zero value is ready to use.
type lastSuccesses struct {
	mu sync.Mutex
	at map[string]time.Time
}

// get is the last success of key, zero when none is kept.
func (l *lastSuccesses) get(key string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.at[key]
}

// record keeps now as the last success of key, unless a later one is kept
// already: of two trips of the same key at once, the one that started last
// ends where the next one starts.
func (l *lastSuccesses) record(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if kept, ok := l.at[key]; ok {
		if now.After(kept) {
			l.at[key] = now
		}
		return
	}
	if l.at == nil {
		l.at = map[string]time.Time{}
	}
	if len(l.at) >= maxLastSuccesses {
		var oldest string
		var oldestAt time.Time
		for k, at := range l.at {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(l.at, oldest)
	}
	l.at[key] = now
}

// lastSuccessKey is what a last success is kept under: the collector, the
// static target, "" for a probe, the target, the path a probe or target
// sets, and the parameters, so that two targets, or two tenants of one,
// each start from their own. It is "" for a trip whose placeholders read
// no last success.
func lastSuccessKey(c *model.Collector, static *model.StaticTarget, target string, overrides fetch.RequestOverrides) string {
	since := c.Clock.Since
	name := ""
	if static != nil {
		name = static.Name
		since = since || fetch.TargetClockUse(static).Since
	}
	if !since {
		return ""
	}
	names := make([]string, 0, len(overrides.Params))
	for param := range overrides.Params {
		names = append(names, param)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, part := range []string{c.Name, name, target, overrides.Path} {
		b.WriteString(part)
		b.WriteByte(0)
	}
	for _, param := range names {
		b.WriteString(param)
		b.WriteByte('=')
		b.WriteString(overrides.Params[param])
		b.WriteByte(0)
	}
	return b.String()
}
