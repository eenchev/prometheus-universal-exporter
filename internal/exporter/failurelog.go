package exporter

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// A target that is down fails every probe, and every probe logged its failure:
// one line per scrape interval per Prometheus replica per target, the same
// line again and again, burying everything else in the log. The self-metrics
// already count every failure exactly; the log is for learning that
// something broke, what, and when it recovered.
//
// So a failure is logged in full the first time. While the same thing keeps
// failing the same way — the same collector, target (and file, for a
// directory), stage and error — the repeats are logged at debug level only,
// and at the failure's own level once every failureRepeatInterval, with how
// many times it happened since the last line and since when it has been
// failing. A different stage or error is a new failure, logged in full. The
// first success after a failure is logged at info level, with how long it
// failed and how many times. With --log.level=debug every repeat is still
// visible.

const (
	// failureRepeatInterval is how often a failure that keeps happening is
	// logged again.
	failureRepeatInterval = 5 * time.Minute
	// failureLogMaxEntries bounds the failures remembered; past it, a new one
	// is logged in full every time rather than remembered.
	failureLogMaxEntries = 10000
	// failureLogForget is how long a failure that has stopped being reported,
	// such as that of a target no longer probed, is remembered.
	failureLogForget = time.Hour
)

type failureState struct {
	stage, err           string
	first, logged, seen  time.Time
	failures, suppressed int
}

type failureLog struct {
	mu        sync.Mutex
	entries   map[string]*failureState
	interval  time.Duration
	now       func() time.Time
	lastSweep time.Time
}

func newFailureLog() *failureLog {
	return &failureLog{entries: map[string]*failureState{}, interval: failureRepeatInterval, now: time.Now}
}

// failureKey identifies what failed. file is empty but for a directory's file.
func failureKey(collector, target, file string) string {
	return collector + "\x00" + target + "\x00" + file
}

// failed logs a failure at level, or counts it as a repeat.
func (f *failureLog) failed(logger *slog.Logger, level slog.Level, key, msg, stage string, err error, attrs ...any) {
	errText := ""
	if err != nil {
		errText = err.Error()
		attrs = append(attrs, "error", err)
	}
	now := f.now()
	f.mu.Lock()
	st := f.entries[key]
	if st == nil || st.stage != stage || st.err != errText {
		if st != nil || f.rememberLocked(now) {
			f.entries[key] = &failureState{stage: stage, err: errText, first: now, logged: now, seen: now, failures: 1}
		}
		f.mu.Unlock()
		logger.Log(context.Background(), level, msg, attrs...)
		return
	}
	st.failures++
	st.seen = now
	if now.Sub(st.logged) < f.interval {
		st.suppressed++
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "repeat", true)...)
		return
	}
	repeated, since := st.suppressed+1, st.first
	st.suppressed, st.logged = 0, now
	f.mu.Unlock()
	logger.Log(context.Background(), level, msg, append(attrs, "repeated", repeated, "failing_since", since.UTC().Format(time.RFC3339))...)
}

// rememberLocked makes room for a new entry, reporting whether there is.
// When full, failures not seen for failureLogForget — a target no longer
// probed — are forgotten, at most once a minute.
func (f *failureLog) rememberLocked(now time.Time) bool {
	if len(f.entries) < failureLogMaxEntries {
		return true
	}
	if now.Sub(f.lastSweep) >= time.Minute {
		f.lastSweep = now
		for k, st := range f.entries {
			if now.Sub(st.seen) > failureLogForget {
				delete(f.entries, k)
			}
		}
	}
	return len(f.entries) < failureLogMaxEntries
}

// recovered logs the first success after a failure, and forgets the failure.
func (f *failureLog) recovered(logger *slog.Logger, key, msg string, attrs ...any) {
	f.mu.Lock()
	st := f.entries[key]
	if st == nil {
		f.mu.Unlock()
		return
	}
	delete(f.entries, key)
	f.mu.Unlock()
	logger.Info(msg, append(attrs, "stage", st.stage, "failed_for", f.now().Sub(st.first).Round(time.Second).String(), "failures", st.failures)...)
}

// forget drops what is remembered about key without logging a recovery, for
// a failure whose subject is gone rather than fixed.
func (f *failureLog) forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
}

// forgetCollectors drops what is remembered about collectors a reload removed.
func (f *failureLog) forgetCollectors(names map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.entries {
		collector, _, _ := strings.Cut(key, "\x00")
		if names[collector] {
			delete(f.entries, key)
		}
	}
}
